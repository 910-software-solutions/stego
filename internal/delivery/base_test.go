package delivery

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

// testRelease is the digest of the release of the tests, which names the migration Job.
const testRelease = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// testJob is the name of the migration Job of the service orders at testRelease.
const testJob = "orders-migrate-0123456789"

// healthCheckProbes are the probes that health-check serves.
var healthCheckProbes = ProbeSource{Liveness: "/livez", Readiness: "/readyz", PortVariable: "PORT"}

// plainBuild builds the folder on the disk as plain kustomize does, with plugins off, and gives the built stream.
func plainBuild(t *testing.T, folder string) string {
	t.Helper()
	options := krusty.MakeDefaultOptions()
	options.PluginConfig = types.DisabledPluginConfig()
	built, err := krusty.MakeKustomizer(options).Run(filesys.MakeFsOnDisk(), folder)
	if err != nil {
		t.Fatalf("kustomize does not build %s: %v", folder, err)
	}
	data, err := built.AsYaml()
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestKubernetesBase(t *testing.T) {
	t.Run("ST-DLV-INV09-T The Kubernetes base builds with plain kustomize and no plugin", func(t *testing.T) {
		// The platform requirements of the service of ST-DLV-TD01, which the tests of ST-DLV-UC01 keep as the
		// fixture of ST-DLV-TD02.
		requirements, err := os.ReadFile(filepath.Join("testdata", "environment", "platform-requirements.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		base, _, err := WriteBase("orders", testRelease, requirements, healthCheckProbes, nil)
		if err != nil {
			t.Fatal(err)
		}
		project := t.TempDir()
		for name, data := range base.Files {
			if err := os.MkdirAll(filepath.Join(project, filepath.Dir(name)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(project, name), data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		plainBuild(t, filepath.Join(project, BaseFolder))

		var kustomization map[string]any
		if err := yaml.Unmarshal(base.Files[KustomizationFile], &kustomization); err != nil {
			t.Fatal(err)
		}
		for key := range kustomization {
			if key != "apiVersion" && key != "kind" && key != "resources" {
				t.Errorf("the kustomization file has the field %s, such as a generator, a transformer or a Helm chart", key)
			}
		}
		for _, item := range kustomization["resources"].([]any) {
			name := item.(string)
			if strings.ContainsAny(name, "/:") || name == ".." {
				t.Errorf("the kustomization file lists %s, which is not a file in the base folder", name)
			}
			if _, listed := base.Files[BaseFolder+"/"+name]; !listed {
				t.Errorf("the kustomization file lists %s, which the base does not hold", name)
			}
		}

		// The overlay of one environment sets the namespace, the replicas and the image digest on the base.
		overlay := filepath.Join(project, "overlay")
		if err := os.MkdirAll(overlay, 0o755); err != nil {
			t.Fatal(err)
		}
		digest := "sha256:" + strings.Repeat("ab", 32)
		text := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nnamespace: orders-test\nresources: [../deploy/base]\n" +
			"replicas: [{name: orders, count: 2}]\nimages: [{name: stego.invalid/orders, newName: registry.example/orders, digest: " + digest + "}]\n"
		if err := os.WriteFile(filepath.Join(overlay, "kustomization.yaml"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		built := plainBuild(t, overlay)
		for _, want := range []string{"namespace: orders-test", "replicas: 2", "image: registry.example/orders@" + digest} {
			if !strings.Contains(built, want) {
				t.Errorf("the overlay build does not hold %q:\n%s", want, built)
			}
		}
		// LoadBase, which apply runs on each base it writes, refuses a kustomization that is not plain.
		if _, err := LoadBase(base.Files); err != nil {
			t.Fatalf("LoadBase refuses the base that WriteBase writes: %v", err)
		}
		listed := func(extra string) string {
			return strings.Replace(string(base.Files[KustomizationFile]), "  - deployment.yaml\n", "  - deployment.yaml\n  - "+extra+"\n", 1)
		}
		appended := func(text string) func(map[string][]byte) {
			return func(f map[string][]byte) { f[KustomizationFile] = append(f[KustomizationFile], text...) }
		}
		for name, change := range map[string]func(files map[string][]byte){
			"a Helm chart":  appended("helmCharts: [{name: x}]\n"),
			"a generator":   appended("configMapGenerator: [{name: x}]\n"),
			"a transformer": appended("transformers: [t.yaml]\n"),
			"a component":   appended("components: [../c]\n"),
			"a patch":       appended("patches: [{path: p.yaml}]\n"),
			"a remote base": func(f map[string][]byte) {
				f[KustomizationFile] = []byte(listed("https://github.com/example/base?ref=main"))
			},
			"a path out": func(f map[string][]byte) { f[KustomizationFile] = []byte(listed("../other")) },
			"itself":     func(f map[string][]byte) { f[KustomizationFile] = []byte(listed("kustomization.yaml")) },
			"two documents": func(f map[string][]byte) {
				f[ServiceAccountFile] = append(f[ServiceAccountFile], "---\napiVersion: v1\nkind: Secret\nmetadata: {name: x}\n"...)
			},
			"another kind": func(f map[string][]byte) {
				f[ServiceAccountFile] = []byte("apiVersion: v1\nkind: Secret\nmetadata: {name: orders}\n")
			},
			"an anchor": func(f map[string][]byte) {
				f[ServiceAccountFile] = []byte("apiVersion: v1\nkind: ServiceAccount\nmetadata: &m {name: orders}\nx: *m\n")
			},
		} {
			files := map[string][]byte{}
			for file, data := range base.Files {
				files[file] = slices.Clone(data)
			}
			change(files)
			if _, err := LoadBase(files); err == nil {
				t.Errorf("LoadBase accepts a base with %s", name)
			}
		}
		// The RoleBinding of the base names its ServiceAccount with no namespace, and the overlay sets it.
		if !strings.Contains(built, "kind: ServiceAccount\n  name: orders\n  namespace: orders-test") {
			t.Errorf("the overlay does not set the namespace of the RoleBinding subject:\n%s", built)
		}
	})
}

func TestClusterPermissionScope(t *testing.T) {
	t.Run("ST-DLV-UC07-R03-T04 A cluster permission that can take over the namespace is refused, and get on secrets names its secrets", func(t *testing.T) {
		need := func(group, resource string, verbs []string, names []string) []ComponentNeeds {
			return []ComponentNeeds{{Component: "red-team", Needs: []Need{{Kind: "cluster permission", Name: "grant", APIGroup: group, Resource: resource, Verbs: verbs, ResourceNames: names}}}}
		}
		for name, needs := range map[string][]ComponentNeeds{
			"list on secrets":            need("", "secrets", []string{"list"}, nil),
			"watch on secrets":           need("", "secrets", []string{"watch"}, nil),
			"get on secrets, no names":   need("", "secrets", []string{"get"}, nil),
			"service account tokens":     need("", "serviceaccounts/token", []string{"create"}, nil),
			"exec into pods":             need("", "pods/exec", []string{"create"}, nil),
			"attach to pods":             need("", "pods/attach", []string{"create"}, nil),
			"port forward to pods":       need("", "pods/portforward", []string{"create"}, nil),
			"proxy to pods":              need("", "pods/proxy", []string{"get"}, nil),
			"create pods":                need("", "pods", []string{"create"}, nil),
			"RBAC roles":                 need("rbac.authorization.k8s.io", "roles", []string{"get"}, nil),
			"RBAC role bindings":         need("rbac.authorization.k8s.io", "rolebindings", []string{"create"}, nil),
			"RBAC cluster roles":         need("rbac.authorization.k8s.io", "clusterroles", []string{"list"}, nil),
			"RBAC cluster role bindings": need("rbac.authorization.k8s.io", "clusterrolebindings", []string{"update"}, nil),
		} {
			if _, err := DeriveRequirements("orders", needs); err == nil {
				t.Errorf("a need for %s is accepted", name)
			} else if !strings.Contains(err.Error(), "red-team") || !strings.Contains(err.Error(), "grant") {
				t.Errorf("the refusal of %s does not name the component and the need: %v", name, err)
			}
		}
		requirements, err := DeriveRequirements("orders", need("", "secrets", []string{"get"}, []string{"{service}-database"}))
		if err != nil {
			t.Fatalf("get on named secrets is refused: %v", err)
		}
		if got := requirements.Requirements[0].ResourceNames; !slices.Equal(got, []string{"orders-database"}) {
			t.Fatalf("the resource names are %v, want [orders-database]", got)
		}
		secret := []ComponentNeeds{{Component: "c", Needs: []Need{{Kind: "secret", Name: "s", Keys: []string{"k"}, ResourceNames: []string{"x"}}}}}
		if _, err := DeriveRequirements("orders", secret); err == nil {
			t.Error("a secret need with resource_names is accepted")
		}
	})
}

// secretsCanary is the text that the fixture of the environment tests gives as a secret setting, a default and an
// example: no file of the base or of the Component may hold it.
const secretsCanary = "CANARY-SECRET-VALUE"

// secretsFixture is a service with each kind of variable that the base wires: a secret variable with a secret
// source and its *_FILE form, a secret variable with no source, a path variable that names the file of a secret,
// a required and an optional variable that are not secret, the port of the probes, the credential of the owner role
// that only the migrate mode reads, in a secret need of its own, and the key file of SpiceDB, which the migrate mode
// also reads.
func secretsFixture() []ComponentNeeds {
	canary := secretsCanary
	port := "8080"
	return []ComponentNeeds{
		{Component: "store", Settings: map[string]string{"connection_string": canary},
			Needs: []Need{{Kind: "secret", Name: "{service}-database", Keys: []string{"url"}},
				{Kind: "secret", Name: "{service}-database-owner", Keys: []string{"url"}}},
			Variables: []Variable{
				{Name: "DATABASE_URL", Type: "string", Description: "The connection string.", Secret: true, SecretSource: &SecretSource{Name: "{service}-database", Key: "url"}},
				{Name: "DATABASE_URL_FILE", Type: "path", Description: "The file of the connection string.", Example: &canary},
				{Name: "PGPASSWORD", Type: "string", Description: "Read by the driver.", Secret: true},
				{Name: "STEGO_MIGRATION_URL", Type: "string", Description: "The connection string of the migration job.", Secret: true, ReadByMode: "migrate",
					SecretSource: &SecretSource{Name: "{service}-database-owner", Key: "url"}},
				{Name: "STEGO_MIGRATION_URL_FILE", Type: "path", Description: "The file of it.", ReadByMode: "migrate"},
				{Name: "STEGO_STORE_TIMEOUT", Type: "string", Default: &canary, Description: "The time of a call."},
			}},
		{Component: "cursor", Needs: []Need{{Kind: "secret", Name: "{service}-cursor", Keys: []string{"key"}}},
			Variables: []Variable{{Name: "STEGO_CURSOR_KEY_FILE", Type: "path", Required: true, Description: "The file of the key.",
				SecretSource: &SecretSource{Name: "{service}-cursor", Key: "key"}}}},
		{Component: "health", Needs: []Need{{Kind: "ingress", Name: "http", Port: 8080}},
			Variables: []Variable{
				{Name: "PORT", Type: "integer", Default: &port, Description: "The port."},
				{Name: "STEGO_ISSUER", Type: "url", Required: true, Description: "The issuer.", Example: &canary},
			}},
		{Component: "probe", Variables: []Variable{{Name: ConsoleSinkVariable, Type: "string", Description: "The console format."}}},
		{Component: "spice", Needs: []Need{{Kind: "secret", Name: "{service}-spicedb", Keys: []string{"preshared-key"}}},
			Variables: []Variable{{Name: "STEGO_SPICEDB_KEY_FILE", Type: "path", Required: true, Description: "The file of the preshared key.",
				SecretSource: &SecretSource{Name: "{service}-spicedb", Key: "preshared-key"}}}},
	}
}

// secretsBase writes the base of the fixture of secretsFixture for the service orders.
func secretsBase(t *testing.T) Base {
	t.Helper()
	files, err := WriteRequirements("orders", secretsFixture())
	if err != nil {
		t.Fatal(err)
	}
	base, _, err := WriteBase("orders", testRelease, files.Requirements, healthCheckProbes, nil)
	if err != nil {
		t.Fatal(err)
	}
	return base
}

// jobContainerOf gives the one container of the migration Job of a built kustomize stream or of a base file, with
// the volumes of its pod under the key "pod volumes".
func jobContainerOf(t *testing.T, stream string) map[string]any {
	t.Helper()
	decoder := yaml.NewDecoder(strings.NewReader(stream))
	for {
		var resource map[string]any
		if err := decoder.Decode(&resource); err != nil {
			t.Fatalf("no Job in the stream: %v\n%s", err, stream)
		}
		if resource["kind"] != "Job" {
			continue
		}
		pod := resource["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
		container := pod["containers"].([]any)[0].(map[string]any)
		container["pod volumes"] = pod["volumes"]
		return container
	}
}

// containerOf gives the one container of the Deployment of a built kustomize stream or of a base file.
func containerOf(t *testing.T, stream string) map[string]any {
	t.Helper()
	decoder := yaml.NewDecoder(strings.NewReader(stream))
	for {
		var resource map[string]any
		if err := decoder.Decode(&resource); err != nil {
			t.Fatalf("no Deployment in the stream: %v\n%s", err, stream)
		}
		if resource["kind"] != "Deployment" {
			continue
		}
		pod := resource["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
		container := pod["containers"].([]any)[0].(map[string]any)
		container["pod volumes"] = pod["volumes"]
		return container
	}
}

// envByName gives the environment entries of a container by name, and fails on a name given twice.
func envByName(t *testing.T, container map[string]any) map[string]map[string]any {
	t.Helper()
	entries := map[string]map[string]any{}
	list, _ := container["env"].([]any)
	for _, item := range list {
		entry := item.(map[string]any)
		name := entry["name"].(string)
		if _, twice := entries[name]; twice {
			t.Errorf("the variable %s has two environment entries", name)
		}
		entries[name] = entry
	}
	return entries
}

func TestBaseEnvironment(t *testing.T) {
	t.Run("ST-DLV-UC07-R03-T05 The base writes one environment entry, names only, for each variable the service reads", func(t *testing.T) {
		base := secretsBase(t)
		container := containerOf(t, string(base.Files[DeploymentFile]))
		var got []any
		if err := yaml.Unmarshal(mustYAML(t, container["env"]), &got); err != nil {
			t.Fatal(err)
		}
		configMap := func(key string, optional bool) map[string]any {
			reference := map[string]any{"name": "orders-config", "key": key}
			if optional {
				reference["optional"] = true
			}
			return map[string]any{"name": key, "valueFrom": map[string]any{"configMapKeyRef": reference}}
		}
		want := []any{
			map[string]any{"name": "DATABASE_URL", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "orders-database", "key": "url"}}},
			configMap("PORT", true),
			configMap("STEGO_CURSOR_KEY_FILE", false),
			configMap("STEGO_ISSUER", false),
			configMap("STEGO_SPICEDB_KEY_FILE", false),
			configMap("STEGO_STORE_TIMEOUT", true),
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("the environment entries are\n%s\nwant\n%s", mustYAML(t, got), mustYAML(t, want))
		}
		// The Secret and the key of each secretKeyRef are those of a secret need of the platform requirements.
		files, err := WriteRequirements("orders", secretsFixture())
		if err != nil {
			t.Fatal(err)
		}
		requirements, err := LoadRequirements(files.Requirements)
		if err != nil {
			t.Fatal(err)
		}
		if requirement := requirementOfKind(requirements, Secret, "orders-database"); !slices.Contains(requirement.Keys, "url") {
			t.Errorf("the platform requirements state no secret orders-database with the key url: %+v", requirements.Requirements)
		}
		// A service name that makes a Secret name longer than a Kubernetes name is refused with a clear message.
		long := strings.Repeat("o", 60)
		if _, err := WriteRequirements(long, secretsFixture()); err == nil || !strings.Contains(err.Error(), "use a shorter service name") {
			t.Errorf("a Secret name of more than 63 characters is not refused with a clear message: %v", err)
		}
		// A secret source that names no key of a secret need of the service is not kept, so the base wires no
		// entry from a Secret or a key that no need gives.
		bad := secretsFixture()
		bad[0].Variables[0].SecretSource = &SecretSource{Name: "{service}-database", Key: "password"}
		written, err := WriteRequirements("orders", bad)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(written.Requirements), "password") {
			t.Errorf("the platform requirements keep a secret source with a key that the need does not state:\n%s", written.Requirements)
		}
		other, _, err := WriteBase("orders", testRelease, written.Requirements, healthCheckProbes, nil)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(other.Files[DeploymentFile]), "secretKeyRef") {
			t.Errorf("the base names a Secret for a source that no need gives:\n%s", other.Files[DeploymentFile])
		}
	})

	t.Run("ST-DLV-UC07-R03-T06 No file of the base or of the Component holds a value, and LoadBase refuses an entry with one", func(t *testing.T) {
		base := secretsBase(t)
		if len(base.Files) != 13 {
			t.Fatalf("the base has %d files, want the five of the base with the Job the five of the Component secret-files and the three of the Component fs-group", len(base.Files))
		}
		for file, data := range base.Files {
			if strings.Contains(string(data), secretsCanary) {
				t.Errorf("%s holds the canary text of a setting, a default or an example:\n%s", file, data)
			}
			// The credential of the migration job stays out of the serving pod (ST-DLV-Q19, least privilege).
			serving := file == DeploymentFile || file == SecretFilesRemoval || file == SecretFilesMounts
			if serving && (strings.Contains(string(data), "database-owner") || strings.Contains(string(data), "STEGO_MIGRATION_URL")) {
				t.Errorf("%s names the credential of the migration job:\n%s", file, data)
			}
		}
		for name, entry := range envByName(t, containerOf(t, string(base.Files[DeploymentFile]))) {
			if _, set := entry["value"]; set {
				t.Errorf("the base sets the value of %s", name)
			}
		}
		for name, change := range map[string][2]string{
			"a value":                               {"            - name: PORT\n              valueFrom:\n", "            - name: PORT\n              value: " + secretsCanary + "\n              valueFrom:\n"},
			"a value in place of a ref":             {"            - name: PORT\n              valueFrom:\n                configMapKeyRef:\n                  key: PORT\n                  name: orders-config\n                  optional: true\n", "            - name: PORT\n              value: " + secretsCanary + "\n"},
			"a field reference":                     {"                configMapKeyRef:\n                  key: PORT\n                  name: orders-config\n                  optional: true\n", "                fieldRef:\n                  fieldPath: metadata.name\n"},
			"an optional secret":                    {"                  key: url\n                  name: orders-database\n", "                  key: url\n                  name: orders-database\n                  optional: true\n"},
			"optional false":                        {"                  key: STEGO_ISSUER\n                  name: orders-config\n", "                  key: STEGO_ISSUER\n                  name: orders-config\n                  optional: false\n"},
			"two refs in one entry":                 {"                secretKeyRef:\n", "                configMapKeyRef: {name: orders-config, key: X}\n                secretKeyRef:\n"},
			"one variable twice":                    {"            - name: STEGO_ISSUER\n", "            - name: PORT\n"},
			"an envFrom":                            {"          image: stego.invalid/orders:set-by-overlay\n", "          image: stego.invalid/orders:set-by-overlay\n          envFrom:\n            - configMapRef: {name: orders-config}\n"},
			"an init container with an environment": {"      containers:\n", "      initContainers:\n        - name: init\n          image: x\n          env:\n            - {name: PGHOST, value: x}\n      containers:\n"},
			"an ephemeral container with envFrom":   {"      containers:\n", "      ephemeralContainers:\n        - name: debug\n          envFrom:\n            - secretRef: {name: orders-database}\n      containers:\n"},
			"a ConfigMap that is no name":           {"                  key: STEGO_ISSUER\n                  name: orders-config\n", "                  key: STEGO_ISSUER\n                  name: Orders_Config\n"},
			"a hostPath volume":                     {"      volumes:\n", "      volumes:\n        - name: h\n          hostPath:\n            path: /\n"},
		} {
			files := maps.Clone(base.Files)
			text := string(files[DeploymentFile])
			if !strings.Contains(text, change[0]) {
				t.Fatalf("%s: the Deployment does not hold %q:\n%s", name, change[0], text)
			}
			files[DeploymentFile] = []byte(strings.Replace(text, change[0], change[1], 1))
			if _, err := LoadBase(files); err == nil {
				t.Errorf("LoadBase accepts a base with %s", name)
			}
		}
	})

	t.Run("ST-DLV-UC07-R03-T07 A kustomize build of the base, and of the base with the Component secret-files, gives exactly one form of each secret", func(t *testing.T) {
		base := secretsBase(t)
		project := t.TempDir()
		for name, data := range base.Files {
			if err := os.MkdirAll(filepath.Join(project, filepath.Dir(name)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(project, name), data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		overlay := func(name, extra string) string {
			folder := filepath.Join(project, name)
			if err := os.MkdirAll(folder, 0o755); err != nil {
				t.Fatal(err)
			}
			text := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nnamespace: orders-test\nresources: [../deploy/base]\n" + extra
			if err := os.WriteFile(filepath.Join(folder, "kustomization.yaml"), []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
			return plainBuild(t, folder)
		}
		plain := containerOf(t, overlay("plain", ""))
		files := containerOf(t, overlay("files", "components: [../deploy/components/secret-files]\n"))

		env := envByName(t, plain)
		if _, ok := env["DATABASE_URL"]; !ok {
			t.Error("the base build has no DATABASE_URL")
		}
		if _, ok := env["DATABASE_URL_FILE"]; ok {
			t.Error("the base build has DATABASE_URL_FILE next to DATABASE_URL")
		}
		env = envByName(t, files)
		if _, ok := env["DATABASE_URL"]; ok {
			t.Error("the build with the Component keeps DATABASE_URL next to DATABASE_URL_FILE")
		}
		for name, want := range map[string]string{"DATABASE_URL_FILE": "/var/run/secrets/stego/orders-database/url",
			"STEGO_CURSOR_KEY_FILE": "/var/run/secrets/stego/orders-cursor/key", "STEGO_SPICEDB_KEY_FILE": "/var/run/secrets/stego/orders-spicedb/preshared-key"} {
			entry := env[name]
			if entry["value"] != want || entry["valueFrom"] != nil || len(entry) != 2 {
				t.Errorf("the build with the Component gives %s as %v, want only the value %s", name, entry, want)
			}
		}
		// The other variables stay as the base wires them.
		for _, name := range []string{"PORT", "STEGO_ISSUER", "STEGO_STORE_TIMEOUT"} {
			if !reflect.DeepEqual(env[name], envByName(t, plain)[name]) {
				t.Errorf("the Component changes %s: %v", name, env[name])
			}
		}
		if len(env) != len(envByName(t, plain)) {
			t.Errorf("the build with the Component has %d entries, the base build %d", len(env), len(envByName(t, plain)))
		}
		mounts := map[string]map[string]any{}
		for _, item := range files["volumeMounts"].([]any) {
			mount := item.(map[string]any)
			mounts[mount["name"].(string)] = mount
		}
		volumes := map[string]map[string]any{}
		for _, item := range files["pod volumes"].([]any) {
			volume := item.(map[string]any)
			volumes[volume["name"].(string)] = volume
		}
		for _, secret := range []string{"orders-database", "orders-cursor", "orders-spicedb"} {
			mount := mounts[secret]
			if mount["mountPath"] != "/var/run/secrets/stego/"+secret || mount["readOnly"] != true || mount["subPath"] != nil {
				t.Errorf("the secret %s is not mounted read-only at its folder with no subPath: %v", secret, mount)
			}
			source, _ := volumes[secret]["secret"].(map[string]any)
			if source["secretName"] != secret || source["defaultMode"] != SecretFileMode {
				t.Errorf("the volume of %s is not the secret with the mode 0400: %v", secret, volumes[secret])
			}
			// The volume projects only the key that the switched variable reads.
			key := map[string]string{"orders-database": "url", "orders-cursor": "key", "orders-spicedb": "preshared-key"}[secret]
			if items, _ := source["items"].([]any); len(items) != 1 || !reflect.DeepEqual(items[0], map[string]any{"key": key, "path": key}) {
				t.Errorf("the volume of %s projects %v, want only the key %s", secret, source["items"], key)
			}
		}
		if mounts["tmp"] == nil || volumes["tmp"] == nil || len(mounts) != 4 || len(volumes) != 4 {
			t.Errorf("the build with the Component has the mounts %v and the volumes %v, want tmp and the three secrets", mounts, volumes)
		}
	})

	t.Run("ST-DLV-UC07-R03-T08 LoadBase accepts the Component secret-files only with its exact kinds and patches", func(t *testing.T) {
		base := secretsBase(t)
		if _, err := LoadBase(base.Files); err != nil {
			t.Fatalf("LoadBase refuses the base with its Component: %v", err)
		}
		replace := func(file, old, replacement string) func(map[string][]byte) error {
			return func(files map[string][]byte) error {
				if !strings.Contains(string(files[file]), old) {
					return fmt.Errorf("%s does not hold %q", file, old)
				}
				files[file] = []byte(strings.Replace(string(files[file]), old, replacement, 1))
				return nil
			}
		}
		for name, change := range map[string]func(map[string][]byte) error{
			"a Kustomization in place of a Component": replace(SecretFilesKustomization, "kind: Component", "kind: Kustomization"),
			"a third patch":                        replace(SecretFilesKustomization, "  - path: mount-files.yaml\n", "  - path: mount-files.yaml\n  - path: other.yaml\n"),
			"the patches in the other order":       replace(SecretFilesKustomization, "  - path: remove-env.yaml\n  - path: mount-files.yaml\n", "  - path: mount-files.yaml\n  - path: remove-env.yaml\n"),
			"a resource in the Component":          replace(SecretFilesKustomization, "patches:", "resources: [x.yaml]\npatches:"),
			"a patch of a Service":                 replace(SecretFilesRemoval, "kind: Deployment", "kind: Service"),
			"a patch of another Deployment":        replace(SecretFilesMounts, "  name: orders\n", "  name: other\n"),
			"a removal that sets a value":          replace(SecretFilesRemoval, "            - $patch: delete\n", "            - $patch: delete\n              value: x\n"),
			"a mount that is not read-only":        replace(SecretFilesMounts, "              readOnly: true\n", "              readOnly: false\n"),
			"a subPath":                            replace(SecretFilesMounts, "              readOnly: true\n", "              readOnly: true\n              subPath: url\n"),
			"the mode 0644":                        replace(SecretFilesMounts, "defaultMode: 256", "defaultMode: 420"),
			"a volume of another kind":             replace(SecretFilesMounts, "          secret:\n            defaultMode: 256\n            items:\n              - key: key\n                path: key\n            secretName: orders-cursor\n", "          emptyDir: {}\n"),
			"a volume with every key":              replace(SecretFilesMounts, "            items:\n              - key: key\n                path: key\n", ""),
			"a key at another path":                replace(SecretFilesMounts, "              - key: key\n                path: key\n", "              - key: key\n                path: other\n"),
			"a key that no variable reads":         replace(SecretFilesMounts, "              - key: key\n                path: key\n", "              - key: key\n                path: key\n              - key: migration-url\n                path: migration-url\n"),
			"a file out of the secret folder":      replace(SecretFilesMounts, "value: /var/run/secrets/stego/orders-cursor/key", "value: /etc/passwd"),
			"a variable that is not removed first": replace(SecretFilesMounts, "            - name: STEGO_CURSOR_KEY_FILE\n", "            - name: STEGO_ISSUER\n"),
			"a Component with a file left out": func(files map[string][]byte) error {
				delete(files, SecretFilesRemoval)
				return nil
			},
			"another file in the Component": func(files map[string][]byte) error {
				files[SecretFilesFolder+"/other.yaml"] = []byte("kind: ConfigMap\n")
				return nil
			},
		} {
			files := maps.Clone(base.Files)
			if err := change(files); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if _, err := LoadBase(files); err == nil {
				t.Errorf("LoadBase accepts a Component with %s", name)
			}
		}
	})
}

// writeProject writes the files of a base to a new project folder and gives the folder.
func writeProject(t *testing.T, files map[string][]byte) string {
	t.Helper()
	project := t.TempDir()
	for name, data := range files {
		if err := os.MkdirAll(filepath.Join(project, filepath.Dir(name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(project, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return project
}

// buildOverlay writes an overlay of the project with the extra lines and builds it with plain kustomize.
func buildOverlay(t *testing.T, project, name, extra string) string {
	t.Helper()
	folder := filepath.Join(project, name)
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	text := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nnamespace: orders-test\nresources: [../deploy/base]\n" + extra
	if err := os.WriteFile(filepath.Join(folder, "kustomization.yaml"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return plainBuild(t, folder)
}

// resourcesOf gives each resource of a built stream by its kind.
func resourcesOf(t *testing.T, stream string) map[string][]map[string]any {
	t.Helper()
	byKind := map[string][]map[string]any{}
	decoder := yaml.NewDecoder(strings.NewReader(stream))
	for {
		var resource map[string]any
		if err := decoder.Decode(&resource); err != nil {
			return byKind
		}
		kind, _ := resource["kind"].(string)
		byKind[kind] = append(byKind[kind], resource)
	}
}

func TestMigrationJob(t *testing.T) {
	t.Run("ST-DLV-UC07-R06-T01 The base writes the migration Job of the release, which runs migrate with the restricted pod security and the owner credential alone", func(t *testing.T) {
		base := secretsBase(t)
		if _, err := LoadBase(base.Files); err != nil {
			t.Fatalf("LoadBase refuses the base with its Job: %v", err)
		}
		var kustomization struct{ Resources []string }
		if err := yaml.Unmarshal(base.Files[KustomizationFile], &kustomization); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(kustomization.Resources, "migration-job.yaml") {
			t.Fatalf("the kustomization does not list the Job: %v", kustomization.Resources)
		}
		var job map[string]any
		if err := yaml.Unmarshal(base.Files[MigrationJobFile], &job); err != nil {
			t.Fatal(err)
		}
		metadata := job["metadata"].(map[string]any)
		if job["apiVersion"] != "batch/v1" || job["kind"] != "Job" || metadata["name"] != testJob {
			t.Errorf("the Job is %v %v %v, want the batch/v1 Job %s", job["apiVersion"], job["kind"], metadata["name"], testJob)
		}
		wantAnnotations := map[string]any{"argocd.argoproj.io/hook": "PreSync", "argocd.argoproj.io/hook-delete-policy": "BeforeHookCreation,HookSucceeded",
			"argocd.argoproj.io/sync-wave": "0"}
		if !reflect.DeepEqual(metadata["annotations"], wantAnnotations) {
			t.Errorf("the annotations of the Job are %v, want %v", metadata["annotations"], wantAnnotations)
		}
		// One stable selector finds the Job of any release for kubectl delete.
		wantLabels := map[string]any{"app.kubernetes.io/name": "orders", "app.kubernetes.io/part-of": "orders", "app.kubernetes.io/component": "migration"}
		if !reflect.DeepEqual(metadata["labels"], wantLabels) {
			t.Errorf("the labels of the Job are %v, want %v", metadata["labels"], wantLabels)
		}
		spec := job["spec"].(map[string]any)
		for key, want := range map[string]int{"backoffLimit": MigrationBackoffLimit, "activeDeadlineSeconds": MigrationDeadline, "ttlSecondsAfterFinished": MigrationTimeToLive} {
			if spec[key] != want {
				t.Errorf("the Job sets %s to %v, want %d", key, spec[key], want)
			}
		}
		template := spec["template"].(map[string]any)
		labels := template["metadata"].(map[string]any)["labels"].(map[string]any)
		if _, named := labels["app.kubernetes.io/name"]; named || labels["app.kubernetes.io/part-of"] != "orders" {
			t.Errorf("the pods of the Job have the labels %v: the selector of the Deployment would take them, or they name no service", labels)
		}
		pod := template["spec"].(map[string]any)
		wantPodSecurity := map[string]any{"runAsNonRoot": true, "seccompProfile": map[string]any{"type": "RuntimeDefault"}}
		if pod["restartPolicy"] != "Never" || pod["automountServiceAccountToken"] != false || !reflect.DeepEqual(pod["securityContext"], wantPodSecurity) {
			t.Errorf("the pod of the Job is %v, want restartPolicy Never, no token, and the pod security %v with no fsGroup", pod, wantPodSecurity)
		}
		if _, set := pod["serviceAccountName"]; set {
			t.Error("the Job names the service account of the base, which Argo CD syncs only after the PreSync hook")
		}
		container := jobContainerOf(t, string(base.Files[MigrationJobFile]))
		deployment := containerOf(t, string(base.Files[DeploymentFile]))
		if !reflect.DeepEqual(container["args"], []any{"migrate"}) || container["image"] != deployment["image"] ||
			!reflect.DeepEqual(container["securityContext"], deployment["securityContext"]) || !reflect.DeepEqual(container["resources"], deployment["resources"]) {
			t.Errorf("the container of the Job is %v, want the image, the security and the resources of the Deployment with the argument migrate", container)
		}
		env := envByName(t, container)
		owner := map[string]any{"secretKeyRef": map[string]any{"name": "orders-database-owner", "key": "url"}}
		if !reflect.DeepEqual(env["STEGO_MIGRATION_URL"]["valueFrom"], owner) {
			t.Errorf("the Job takes STEGO_MIGRATION_URL as %v, want the key url of orders-database-owner", env["STEGO_MIGRATION_URL"])
		}
		for _, name := range []string{"PORT", "STEGO_ISSUER", "STEGO_SPICEDB_KEY_FILE", "STEGO_STORE_TIMEOUT"} {
			if !reflect.DeepEqual(env[name], envByName(t, deployment)[name]) {
				t.Errorf("the Job takes %s as %v, want the ConfigMap entry of the Deployment", name, env[name])
			}
		}
		// The Job holds no credential of a serving role, and no path of a secret file that the migrate mode does not
		// read.
		for _, name := range []string{"DATABASE_URL", "DATABASE_URL_FILE", "PGPASSWORD", "STEGO_CURSOR_KEY_FILE", "STEGO_MIGRATION_URL_FILE", ConsoleSinkVariable} {
			if _, wired := env[name]; wired {
				t.Errorf("the Job wires %s", name)
			}
		}
		if len(env) != 5 {
			t.Errorf("the Job has %d environment entries, want 5: %v", len(env), env)
		}
		// A service with no variable that the migrate mode alone reads has no Job.
		plain := secretsFixture()
		plain[0].Variables = slices.DeleteFunc(plain[0].Variables, func(v Variable) bool { return v.ReadByMode != "" })
		written, err := WriteRequirements("orders", plain)
		if err != nil {
			t.Fatal(err)
		}
		other, _, err := WriteBase("orders", "", written.Requirements, healthCheckProbes, nil)
		if err != nil {
			t.Fatalf("a base with no migration job needs a release digest: %v", err)
		}
		if _, held := other.Files[MigrationJobFile]; held {
			t.Error("a service with no migration job gets a Job")
		}
	})

	t.Run("ST-DLV-UC07-R06-T02 A kustomize build of the base and of the base with the Component gives the Job its image digest and one form of the owner credential", func(t *testing.T) {
		base := secretsBase(t)
		project := writeProject(t, base.Files)
		digest := "sha256:" + strings.Repeat("cd", 32)
		images := "images: [{name: stego.invalid/orders, newName: registry.example/orders, digest: " + digest + "}]\n"
		plain := resourcesOf(t, buildOverlay(t, project, "plain", images))
		files := resourcesOf(t, buildOverlay(t, project, "files", images+"components: [../deploy/components/secret-files]\n"))
		for name, built := range map[string]map[string][]map[string]any{"plain": plain, "files": files} {
			if len(built["Job"]) != 1 || len(built["Deployment"]) != 1 {
				t.Fatalf("the %s build has the Jobs %v and the Deployments %v, want one each", name, built["Job"], built["Deployment"])
			}
			stream := string(mustYAML(t, built["Job"][0]))
			if !strings.Contains(stream, "image: registry.example/orders@"+digest) || !strings.Contains(stream, "namespace: orders-test") {
				t.Errorf("the %s build does not give the Job the image digest and the namespace:\n%s", name, stream)
			}
			// The selector of the Deployment and of the Service never takes a pod of the Job.
			selector := built["Deployment"][0]["spec"].(map[string]any)["selector"].(map[string]any)["matchLabels"].(map[string]any)
			labels := built["Job"][0]["spec"].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)["labels"].(map[string]any)
			matched := true
			for key, value := range selector {
				if labels[key] != value {
					matched = false
				}
			}
			if matched {
				t.Errorf("the %s build: the selector %v of the Deployment takes the pods of the Job with the labels %v", name, selector, labels)
			}
		}
		env := envByName(t, jobContainerOf(t, string(mustYAML(t, plain["Job"][0]))))
		if _, ok := env["STEGO_MIGRATION_URL"]; !ok {
			t.Error("the base build of the Job has no STEGO_MIGRATION_URL")
		}
		container := jobContainerOf(t, string(mustYAML(t, files["Job"][0])))
		env = envByName(t, container)
		if _, ok := env["STEGO_MIGRATION_URL"]; ok {
			t.Error("the build with the Component keeps STEGO_MIGRATION_URL next to its file")
		}
		for name, want := range map[string]string{"STEGO_MIGRATION_URL_FILE": "/var/run/secrets/stego/orders-database-owner/url",
			"STEGO_SPICEDB_KEY_FILE": "/var/run/secrets/stego/orders-spicedb/preshared-key"} {
			if entry := env[name]; entry["value"] != want || len(entry) != 2 {
				t.Errorf("the build with the Component gives the Job %s as %v, want only the value %s", name, entry, want)
			}
		}
		volumes := map[string]any{}
		for _, item := range container["pod volumes"].([]any) {
			volume := item.(map[string]any)
			volumes[volume["name"].(string)] = volume["secret"]
		}
		want := map[string]any{"tmp": nil,
			"orders-database-owner": map[string]any{"secretName": "orders-database-owner", "defaultMode": SecretFileMode, "items": []any{map[string]any{"key": "url", "path": "url"}}},
			"orders-spicedb":        map[string]any{"secretName": "orders-spicedb", "defaultMode": SecretFileMode, "items": []any{map[string]any{"key": "preshared-key", "path": "preshared-key"}}}}
		if !reflect.DeepEqual(volumes, want) {
			t.Errorf("the Job with the Component has the volumes %v, want %v", volumes, want)
		}
		// The Deployment with the Component never gets the owner credential.
		if text := string(mustYAML(t, files["Deployment"][0])); strings.Contains(text, "database-owner") || strings.Contains(text, "STEGO_MIGRATION_URL") {
			t.Errorf("the Deployment with the Component names the owner credential:\n%s", text)
		}
	})

	t.Run("ST-DLV-UC07-R06-T03 A Job name of more than 63 characters and a release that is no digest are refused with a clear message", func(t *testing.T) {
		longest := strings.Repeat("s", 63-len("-migrate-")-10)
		if name, err := MigrationJobName(longest, testRelease); err != nil || len(name) != 63 {
			t.Errorf("a service name of %d characters gives %q, %v; want a name of 63 characters", len(longest), name, err)
		}
		_, err := MigrationJobName(longest+"s", testRelease)
		if err == nil || !strings.Contains(err.Error(), "63") || !strings.Contains(err.Error(), "use a service name of at most 44 characters") {
			t.Errorf("a Job name of 64 characters is not refused with a clear message: %v", err)
		}
		for _, release := range []string{"", "0123", "0123456789ABCDEF", "not-a-digest-at-all"} {
			if _, err := MigrationJobName("orders", release); err == nil || !strings.Contains(err.Error(), "hex digits") {
				t.Errorf("the release %q is not refused with a clear message: %v", release, err)
			}
		}
		files, err := WriteRequirements(longest+"s", secretsFixture())
		if err == nil {
			_, _, err = WriteBase(longest+"s", testRelease, files.Requirements, healthCheckProbes, nil)
		}
		if err == nil {
			t.Error("WriteBase writes a Job whose name is longer than a Kubernetes label value")
		}
		other, _, err := WriteBase("orders", strings.Replace(testRelease, "0", "f", 1), mustRequirements(t), healthCheckProbes, nil)
		if err != nil || other.Files[MigrationJobFile] == nil || strings.Contains(string(other.Files[MigrationJobFile]), testJob) {
			t.Errorf("another release does not give the Job another name: %v", err)
		}
	})

	t.Run("ST-DLV-UC07-R06-T04 LoadBase accepts the migration Job only with its exact shape", func(t *testing.T) {
		base := secretsBase(t)
		for name, change := range map[string][2]string{
			"an fsGroup":                  {"        runAsNonRoot: true\n", "        fsGroup: 1000\n        runAsNonRoot: true\n"},
			"a service account":           {"      automountServiceAccountToken: false\n", "      automountServiceAccountToken: false\n      serviceAccountName: orders\n"},
			"a service account token":     {"      automountServiceAccountToken: false\n", "      automountServiceAccountToken: true\n"},
			"a privilege escalation":      {"            allowPrivilegeEscalation: false\n", "            allowPrivilegeEscalation: true\n"},
			"a writable root file system": {"            readOnlyRootFilesystem: true\n", "            readOnlyRootFilesystem: false\n"},
			"a capability kept":           {"                - ALL\n", "                - NET_RAW\n"},
			"root":                        {"        runAsNonRoot: true\n", "        runAsNonRoot: false\n"},
			"no seccomp profile":          {"          type: RuntimeDefault\n", "          type: Unconfined\n"},
			"another argument":            {"            - migrate\n", "            - serve\n"},
			"a restart in place":          {"      restartPolicy: Never\n", "      restartPolicy: OnFailure\n"},
			"no time to live":             {"  ttlSecondsAfterFinished: 600\n", ""},
			"no limits":                   {"            limits:\n", "            nolimits:\n"},
			"no cpu limit":                {"            limits:\n              cpu: \"1\"\n", "            limits:\n"},
			"no memory request":           {"              memory: 128Mi\n", ""},
			"no sync wave":                {"    argocd.argoproj.io/sync-wave: \"0\"\n", ""},
			"no name label on the Job":    {"    app.kubernetes.io/name: orders\n", ""},
			"no time limit":               {"  activeDeadlineSeconds: 7200\n", ""},
			"no retry limit":              {"  backoffLimit: 2\n", ""},
			"another hook":                {"argocd.argoproj.io/hook: PreSync", "argocd.argoproj.io/hook: Sync"},
			"the label of the Deployment": {"        app.kubernetes.io/part-of: orders\n", "        app.kubernetes.io/name: orders\n        app.kubernetes.io/part-of: orders\n"},
			"a volume of a secret":        {"        - emptyDir:\n            sizeLimit: 64Mi\n          name: tmp\n", "        - emptyDir:\n            sizeLimit: 64Mi\n          name: tmp\n        - name: db\n          emptyDir: {}\n"},
			"an envFrom":                  {"          image: stego.invalid/orders:set-by-overlay\n", "          envFrom:\n            - secretRef: {name: orders-database}\n          image: stego.invalid/orders:set-by-overlay\n"},
			"an init container":           {"      containers:\n", "      initContainers:\n        - name: init\n          image: x\n      containers:\n"},
			"a value":                     {"                  name: orders-database-owner\n", "                  name: orders-database-owner\n              value: x\n"},
			"another ConfigMap":           {"                  key: STEGO_ISSUER\n                  name: orders-config\n", "                  key: STEGO_ISSUER\n                  name: other-config\n"},
			"a name of another service":   {"  name: orders-migrate-0123456789\n", "  name: other-migrate-0123456789\n"},
		} {
			files := maps.Clone(base.Files)
			text := string(files[MigrationJobFile])
			if !strings.Contains(text, change[0]) {
				t.Fatalf("%s: the Job does not hold %q:\n%s", name, change[0], text)
			}
			files[MigrationJobFile] = []byte(strings.Replace(text, change[0], change[1], 1))
			if _, err := LoadBase(files); err == nil {
				t.Errorf("LoadBase accepts a Job with %s", name)
			}
		}
		// A Component that patches a Job of a base with no Job is refused.
		files := maps.Clone(base.Files)
		delete(files, MigrationJobFile)
		files[KustomizationFile] = []byte(strings.Replace(string(files[KustomizationFile]), "  - migration-job.yaml\n", "", 1))
		if _, err := LoadBase(files); err == nil {
			t.Error("LoadBase accepts a Component that patches a Job of a base with no Job")
		}
	})
}

func TestMigrationJobRequirements(t *testing.T) {
	t.Run("ST-DLV-UC07-R06-T05 The platform requirements and the reference name the owner Secret and mark the variables that only the migration Job reads", func(t *testing.T) {
		files, err := WriteRequirements("orders", secretsFixture())
		if err != nil {
			t.Fatal(err)
		}
		requirements, err := LoadRequirements(files.Requirements)
		if err != nil {
			t.Fatal(err)
		}
		if requirement := requirementOfKind(requirements, Secret, "orders-database-owner"); !slices.Equal(requirement.Keys, []string{"url"}) {
			t.Errorf("the platform requirements state no secret orders-database-owner with the key url: %+v", requirements.Requirements)
		}
		for _, variable := range requirements.Environment {
			if (variable.ReadByMode == MigrateMode) != strings.HasPrefix(variable.Name, "STEGO_MIGRATION_URL") {
				t.Errorf("the platform requirements mark %s as read by the mode %q", variable.Name, variable.ReadByMode)
			}
		}
		reference := string(files.Reference)
		for _, row := range []string{
			"| `STEGO_MIGRATION_URL` | `orders-database-owner` | `url` | the migration Job only (mode migrate) |",
			"| `DATABASE_URL` | `orders-database` | `url` | the Deployment |",
			"| `STEGO_SPICEDB_KEY_FILE` | `orders-spicedb` | `preshared-key` | the Deployment and the migration Job |",
			"| `STEGO_CURSOR_KEY_FILE` | `orders-cursor` | `key` | the Deployment |",
		} {
			if !strings.Contains(reference, row) {
				t.Errorf("the reference does not hold the row %s:\n%s", row, reference)
			}
		}
	})
}

func TestMigrationKeyFiles(t *testing.T) {
	t.Run("ST-DLV-UC07-R06-T07 The key files that the Job gets are the secret file settings that the migrate mode of the runtime reads, apart from its own", func(t *testing.T) {
		// The runtime module does not import internal, and this module does not import the runtime: the test reads
		// the source of the service runtime. It takes every function of the package that ReadMigrationSettings
		// calls, at any depth, by its name, and each constant of the package that those functions give a call as an
		// argument, such as os.Getenv or connectionString, whose value is a *_FILE variable. A constant in a case of
		// a switch, such as the table of settingComponent, is not a read.
		folder := filepath.Join("..", "..", "registry", "runtime", "service")
		set := token.NewFileSet()
		functions := map[string]*ast.FuncDecl{}
		constants := map[string]string{}
		entries, err := os.ReadDir(folder)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			file, err := parser.ParseFile(set, filepath.Join(folder, entry.Name()), nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, declaration := range file.Decls {
				switch d := declaration.(type) {
				case *ast.FuncDecl:
					if d.Recv == nil {
						functions[d.Name.Name] = d
					}
				case *ast.GenDecl:
					for _, spec := range d.Specs {
						value, ok := spec.(*ast.ValueSpec)
						if !ok || d.Tok != token.CONST {
							continue
						}
						for i, name := range value.Names {
							if i < len(value.Values) {
								if literal, ok := value.Values[i].(*ast.BasicLit); ok && literal.Kind == token.STRING {
									constants[name.Name], _ = strconv.Unquote(literal.Value)
								}
							}
						}
					}
				}
			}
		}
		if functions["ReadMigrationSettings"] == nil {
			t.Fatal("the service runtime has no function ReadMigrationSettings")
		}
		read := map[string]bool{}
		seen := map[string]bool{}
		var walk func(name string)
		walk = func(name string) {
			if seen[name] || functions[name] == nil {
				return
			}
			seen[name] = true
			ast.Inspect(functions[name].Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				if callee, ok := call.Fun.(*ast.Ident); ok {
					walk(callee.Name)
				}
				for _, argument := range call.Args {
					if identifier, ok := argument.(*ast.Ident); ok {
						if value, isConstant := constants[identifier.Name]; isConstant && strings.HasSuffix(value, "_FILE") {
							read[value] = true
						}
					}
				}
				return true
			})
		}
		walk("ReadMigrationSettings")
		// The variables that only the migrate mode reads are the Job's own; the registry states them, and the secret
		// source of each key file.
		own := map[string]bool{}
		secretFile := map[string]bool{}
		components, err := filepath.Glob(filepath.Join("..", "..", "registry", "components", "*", "component.yaml"))
		if err != nil || len(components) == 0 {
			t.Fatalf("no component of the registry: %v", err)
		}
		for _, file := range components {
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var component struct {
				Environment []struct {
					Name         string        `yaml:"name"`
					ReadByMode   string        `yaml:"read_by_mode"`
					SecretSource *SecretSource `yaml:"secret_source"`
				} `yaml:"environment"`
			}
			if err := yaml.Unmarshal(data, &component); err != nil {
				t.Fatalf("%s: %v", file, err)
			}
			for _, variable := range component.Environment {
				if variable.ReadByMode == MigrateMode {
					own[variable.Name] = true
				}
				// A file setting with no secret source, such as STEGO_SPICEDB_CA_FILE, is not a key file: the Job
				// takes its path from the ConfigMap as the Deployment does (ST-SUP-RP14-LC14).
				if variable.SecretSource != nil {
					secretFile[variable.Name] = true
				}
				// The Secret and the key of each key file of the Job are those that the registry states for it, which
				// LoadBase holds the mount patch of the Job to.
				if want, keyFile := MigrationKeyFiles[variable.Name]; keyFile && (variable.SecretSource == nil || *variable.SecretSource != want) {
					t.Errorf("%s states the secret source %+v of %s; MigrationKeyFiles states %+v", file, variable.SecretSource, variable.Name, want)
				}
			}
		}
		var want []string
		for name := range read {
			if !own[name] && secretFile[name] {
				want = append(want, name)
			}
		}
		slices.Sort(want)
		if len(own) == 0 || !read["STEGO_MIGRATION_DATABASE_URL_FILE"] {
			t.Fatalf("the test does not find the own variables of the migrate mode: read %v, own %v", read, own)
		}
		if got := slices.Sorted(maps.Keys(MigrationKeyFiles)); !slices.Equal(got, want) {
			t.Errorf("MigrationKeyFiles is %v, but ReadMigrationSettings of the runtime reads the key files %v besides its own", got, want)
		}
	})
}

// mustRequirements gives the platform requirements of the fixture of secretsFixture for the service orders.
func mustRequirements(t *testing.T) []byte {
	t.Helper()
	files, err := WriteRequirements("orders", secretsFixture())
	if err != nil {
		t.Fatal(err)
	}
	return files.Requirements
}

func TestOwnerSecret(t *testing.T) {
	t.Run("ST-DLV-INV13-T LoadBase refuses a Deployment or a patch of it that references the owner Secret, and a Job that references a serving Secret", func(t *testing.T) {
		base := secretsBase(t)
		replace := func(file, old, replacement string) func(map[string][]byte) error {
			return func(files map[string][]byte) error {
				if !strings.Contains(string(files[file]), old) {
					return fmt.Errorf("%s does not hold %q:\n%s", file, old, files[file])
				}
				files[file] = []byte(strings.Replace(string(files[file]), old, replacement, 1))
				return nil
			}
		}
		// jobMount gives the key file of SpiceDB in the Job patch another Secret and key.
		jobMount := func(secret, key string) func(map[string][]byte) error {
			return func(files map[string][]byte) error {
				text := strings.ReplaceAll(string(files[SecretFilesJobMounts]), "orders-spicedb", secret)
				files[SecretFilesJobMounts] = []byte(strings.ReplaceAll(text, "preshared-key", key))
				return nil
			}
		}
		for name, change := range map[string]func(map[string][]byte) error{
			"a Deployment that takes a variable from the owner Secret": replace(DeploymentFile,
				"                  key: url\n                  name: orders-database\n", "                  key: url\n                  name: orders-database-owner\n"),
			"a Deployment that takes a variable from a renamed owner Secret": replace(DeploymentFile,
				"                  key: url\n                  name: orders-database\n", "                  key: url\n                  name: prod-orders-database-owner\n"),
			"a Deployment patch that mounts the owner Secret": func(files map[string][]byte) error {
				files[SecretFilesMounts] = []byte(strings.ReplaceAll(string(files[SecretFilesMounts]), "orders-database", "orders-database-owner"))
				return nil
			},
			"a Job that takes a variable from the Secret of the runtime role": replace(MigrationJobFile,
				"                  name: orders-database-owner\n", "                  name: orders-database\n"),
			// The probes of the security review of #373 (M1, M2, L2).
			"a Deployment secret volume of the owner Secret": replace(DeploymentFile, "      volumes:\n",
				"      volumes:\n        - name: own\n          secret:\n            secretName: orders-database-owner\n"),
			"a Deployment projected volume of the owner Secret": replace(DeploymentFile, "      volumes:\n",
				"      volumes:\n        - name: own\n          projected:\n            sources:\n              - secret: {name: orders-database-owner}\n"),
			"a Job that takes another key of the owner Secret": replace(MigrationJobFile,
				"                  key: url\n                  name: orders-database-owner\n", "                  key: password\n                  name: orders-database-owner\n"),
			"a Job patch that mounts the cursor key":          jobMount("orders-cursor", "key"),
			"a Job patch that mounts a foreign Secret":        jobMount("payments-database", "url"),
			"a Job patch that mounts the Secret of the relay": jobMount("orders-database-relay", "url"),
			"a Job patch that mounts the Secret of the runtime role": func(files map[string][]byte) error {
				text := strings.ReplaceAll(string(files[SecretFilesJobMounts]), "orders-database-owner", "orders-database")
				files[SecretFilesJobMounts] = []byte(text)
				return nil
			},
		} {
			files := maps.Clone(base.Files)
			if err := change(files); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			_, err := LoadBase(files)
			if err == nil {
				t.Errorf("LoadBase accepts %s", name)
			} else if !strings.Contains(err.Error(), "ST-DLV-INV13") {
				t.Errorf("LoadBase refuses %s, but not for the owner Secret: %v", name, err)
			}
		}
	})
}

func TestRefusedVariables(t *testing.T) {
	t.Run("ST-DLV-UC07-R03-T09 A variable that the start guard refuses gets no entry in the base, is never required, and the environment check refuses a value for it", func(t *testing.T) {
		components := secretsFixture()
		components[2].Variables = append(components[2].Variables,
			Variable{Name: "PGHOST", Type: "string", Description: "Read by the driver.", RefusedAtStart: true},
			Variable{Name: "GRPC_GO_LOG_SEVERITY_LEVEL", Type: "string", Description: "Read by gRPC."})
		files, err := WriteRequirements("orders", components)
		if err != nil {
			t.Fatal(err)
		}
		requirements, err := LoadRequirements(files.Requirements)
		if err != nil {
			t.Fatal(err)
		}
		for _, variable := range requirements.Environment {
			if variable.RefusedAtStart != (variable.Name == "PGHOST") {
				t.Errorf("the platform requirements mark %s refused at start: %v", variable.Name, variable.RefusedAtStart)
			}
		}
		base, _, err := WriteBase("orders", testRelease, files.Requirements, healthCheckProbes, nil)
		if err != nil {
			t.Fatal(err)
		}
		env := envByName(t, containerOf(t, string(base.Files[DeploymentFile])))
		if _, wired := env["PGHOST"]; wired {
			t.Error("the base wires PGHOST, which the start guard refuses")
		}
		if _, wired := env["GRPC_GO_LOG_SEVERITY_LEVEL"]; !wired {
			t.Error("the base does not wire GRPC_GO_LOG_SEVERITY_LEVEL, which the start guard allows")
		}
		// A refused variable that a component states as required is refused, so it never reaches the required list.
		required := secretsFixture()
		required[2].Variables = append(required[2].Variables, Variable{Name: "PGHOST", Type: "string", Required: true, Description: "Read by the driver.", RefusedAtStart: true})
		if _, err := WriteRequirements("orders", required); err == nil {
			t.Error("a required variable that the start guard refuses is accepted")
		}
		// The environment check names a value for the refused variable, and passes without it.
		file := func(extra string) []byte {
			return []byte("version: 1\nservice: orders\nenvironment: test\nimage: registry.example/orders@sha256:" + strings.Repeat("ab", 32) + "\n" +
				"destination: {cluster: \"https://api.example.com:6443\", namespace: orders-test, overlay_path: overlays/test}\n" +
				"secrets:\n  orders-database: {url: x}\n  orders-database-owner: {url: x}\n  orders-cursor: {key: x}\n  orders-spicedb: {preshared-key: x}\n" +
				"configuration:\n  STEGO_CURSOR_KEY_FILE: /k\n  STEGO_ISSUER: https://issuer.example\n  STEGO_SPICEDB_KEY_FILE: /s\n" + extra +
				"ingresses:\n  http: {port: 8080}\n")
		}
		fit, err := CheckEnvironment(requirements, file(""))
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range fit.Rows {
			if strings.Contains(row.Requirement, "PGHOST") {
				t.Errorf("the check names PGHOST with no value given: %+v", row)
			}
		}
		fit, err = CheckEnvironment(requirements, file("  PGHOST: db.example\n"))
		if err != nil {
			t.Fatal(err)
		}
		named := false
		for _, row := range fit.Rows {
			if row.Requirement == "configuration PGHOST" && row.Status == WrongType && strings.Contains(row.Detail, "refuses") && !strings.Contains(row.Detail, "db.example") {
				named = true
			}
		}
		if !named {
			t.Errorf("the check does not refuse a value for PGHOST, without the value: %+v", fit.Rows)
		}
		// The start guard refuses each value that is not "", so a value of white space only is refused too; an empty
		// or null value is no value (#380 security review L2).
		for value, refused := range map[string]bool{`" "`: true, `"\t"`: true, `""`: false, "null": false} {
			fit, err = CheckEnvironment(requirements, file("  PGHOST: "+value+"\n"))
			if err != nil {
				t.Fatal(err)
			}
			named = false
			for _, row := range fit.Rows {
				if row.Requirement == "configuration PGHOST" && row.Status == WrongType && strings.Contains(row.Detail, "the service refuses the environment variable PGHOST at start") {
					named = true
				}
			}
			if named != refused {
				t.Errorf("the value %s of PGHOST: the check refuses it %v, want %v: %+v", value, named, refused, fit.Rows)
			}
		}
	})
}

// mustYAML encodes a value as YAML for a comparison or a message.
func mustYAML(t *testing.T, value any) []byte {
	t.Helper()
	data, err := yaml.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// podOf gives the pod spec of a built workload.
func podOf(t *testing.T, workload map[string]any) map[string]any {
	t.Helper()
	spec, _ := workload["spec"].(map[string]any)
	template, _ := spec["template"].(map[string]any)
	pod, _ := template["spec"].(map[string]any)
	if pod == nil {
		t.Fatalf("the workload has no pod spec: %v", workload)
	}
	return pod
}

func TestFSGroupComponent(t *testing.T) {
	t.Run("ST-DLV-UC07-R03-T11 A kustomize build of the base with the Components secret-files and fs-group gives files of the mode 0400 to the fsGroup 65532", func(t *testing.T) {
		base := secretsBase(t)
		for _, file := range []string{FSGroupKustomization, FSGroupDeployment, FSGroupJob} {
			if base.Files[file] == nil {
				t.Fatalf("the base with the Component secret-files has no %s", file)
			}
		}
		if text := string(base.Files[FSGroupKustomization]); !strings.Contains(text, "Never list it on\n# OpenShift") {
			t.Errorf("the kustomization of the Component fs-group does not say never to list it on OpenShift:\n%s", text)
		}
		project := writeProject(t, base.Files)
		plain := resourcesOf(t, buildOverlay(t, project, "plain", "components: [../deploy/components/secret-files]\n"))
		both := resourcesOf(t, buildOverlay(t, project, "both", "components: [../deploy/components/secret-files, ../deploy/components/fs-group]\n"))
		for _, kind := range []string{"Deployment", "Job"} {
			if len(plain[kind]) != 1 || len(both[kind]) != 1 {
				t.Fatalf("the builds have the %s resources %v and %v, want one each", kind, plain[kind], both[kind])
			}
			// The base and the Component secret-files alone set no fsGroup, for OpenShift.
			if security, _ := podOf(t, plain[kind][0])["securityContext"].(map[string]any); security["fsGroup"] != nil {
				t.Errorf("the %s without the Component fs-group sets the fsGroup %v", kind, security["fsGroup"])
			}
			pod := podOf(t, both[kind][0])
			security, _ := pod["securityContext"].(map[string]any)
			want := map[string]any{"fsGroup": FSGroup, "runAsNonRoot": true, "seccompProfile": map[string]any{"type": "RuntimeDefault"}}
			if !reflect.DeepEqual(security, want) {
				t.Errorf("the %s with the Component fs-group has the pod security %v, want %v", kind, security, want)
			}
			// Each secret volume keeps the mode 0400; the fsGroup gives the files to the group of the process.
			secrets := 0
			for _, item := range pod["volumes"].([]any) {
				volume := item.(map[string]any)
				source, _ := volume["secret"].(map[string]any)
				if source == nil {
					continue
				}
				secrets++
				if source["defaultMode"] != SecretFileMode {
					t.Errorf("the %s volume %v does not have the mode 0400 with the Component fs-group", kind, volume)
				}
			}
			if secrets == 0 {
				t.Errorf("the %s build with the two Components mounts no secret", kind)
			}
		}
		// The Component changes nothing but the fsGroup.
		for _, kind := range []string{"Deployment", "Job"} {
			pod := podOf(t, both[kind][0])
			delete(pod["securityContext"].(map[string]any), "fsGroup")
			if !reflect.DeepEqual(both[kind][0], plain[kind][0]) {
				t.Errorf("the Component fs-group changes more than the fsGroup of the %s", kind)
			}
		}
	})

	t.Run("ST-DLV-UC07-R03-T12 LoadBase accepts the Component fs-group only with its exact shape", func(t *testing.T) {
		base := secretsBase(t)
		if _, err := LoadBase(base.Files); err != nil {
			t.Fatalf("LoadBase refuses the base with its Components: %v", err)
		}
		replace := func(file, old, replacement string) func(map[string][]byte) error {
			return func(files map[string][]byte) error {
				if !strings.Contains(string(files[file]), old) {
					return fmt.Errorf("%s does not hold %q", file, old)
				}
				files[file] = []byte(strings.Replace(string(files[file]), old, replacement, 1))
				return nil
			}
		}
		remove := func(file string) func(map[string][]byte) error {
			return func(files map[string][]byte) error {
				delete(files, file)
				return nil
			}
		}
		const (
			notDeployment = FSGroupDeployment + " is not a patch of the apps/v1 Deployment orders that sets the pod fsGroup 65532"
			notJob        = FSGroupJob + " is not a patch of the batch/v1 Job " + testJob + " that sets the pod fsGroup 65532"
			notComponent  = FSGroupKustomization + " is not a Component that lists deployment-fs-group.yaml, job-fs-group.yaml, in this order"
			notExactly    = "the Component " + FSGroupFolder + " does not hold exactly its kustomization and the patches deployment-fs-group.yaml, job-fs-group.yaml"
			moreThan      = ": the patch does more than set the fsGroup of the pod: "
		)
		// Each probe names the reason of its refusal, so that a probe that LoadBase refuses for another reason, such as
		// a YAML syntax error of the probe, fails the test.
		for name, probe := range map[string]struct {
			change func(map[string][]byte) error
			reason []string
		}{
			"another fsGroup":                         {replace(FSGroupDeployment, "fsGroup: 65532", "fsGroup: 0"), []string{notDeployment}},
			"another fsGroup of the Job":              {replace(FSGroupJob, "fsGroup: 65532", "fsGroup: 1000"), []string{notJob}},
			"a runAsUser":                             {replace(FSGroupDeployment, "fsGroup: 65532", "fsGroup: 65532\n        runAsUser: 0"), []string{FSGroupDeployment + moreThan + "yaml: unmarshal errors:", "field runAsUser not found"}},
			"a privileged container":                  {replace(FSGroupJob, "      securityContext:", "      containers: [{name: orders, securityContext: {privileged: true}}]\n      securityContext:"), []string{FSGroupJob + moreThan + "yaml: unmarshal errors:", "field containers not found"}},
			"a supplemental group":                    {replace(FSGroupDeployment, "fsGroup: 65532", "fsGroup: 65532\n        supplementalGroups: [0]"), []string{FSGroupDeployment + moreThan + "yaml: unmarshal errors:", "field supplementalGroups not found"}},
			"a patch of another Deployment":           {replace(FSGroupDeployment, "  name: orders\n", "  name: other\n"), []string{notDeployment}},
			"a patch of a StatefulSet":                {replace(FSGroupDeployment, "kind: Deployment", "kind: StatefulSet"), []string{notDeployment}},
			"a patch of another Job":                  {replace(FSGroupJob, "name: "+testJob, "name: other"), []string{notJob}},
			"a Kustomization in place of a Component": {replace(FSGroupKustomization, "kind: Component", "kind: Kustomization"), []string{notComponent}},
			"the patches in the other order":          {replace(FSGroupKustomization, "  - path: deployment-fs-group.yaml\n  - path: job-fs-group.yaml\n", "  - path: job-fs-group.yaml\n  - path: deployment-fs-group.yaml\n"), []string{notComponent}},
			"a resource in the Component":             {replace(FSGroupKustomization, "patches:", "resources: [x.yaml]\npatches:"), []string{FSGroupKustomization + ": yaml: unmarshal errors:", "field resources not found"}},
			// The probes of the #375 security review: each is refused by the strict decoder, and stays refused.
			"a hostPath volume":          {replace(FSGroupDeployment, "      securityContext:", "      volumes: [{name: host, hostPath: {path: /}}]\n      securityContext:"), []string{FSGroupDeployment + moreThan + "yaml: unmarshal errors:", "field volumes not found"}},
			"a hostNetwork":              {replace(FSGroupJob, "      securityContext:", "      hostNetwork: true\n      securityContext:"), []string{FSGroupJob + moreThan + "yaml: unmarshal errors:", "field hostNetwork not found"}},
			"a namespace":                {replace(FSGroupDeployment, "  name: orders\n", "  name: orders\n  namespace: kube-system\n"), []string{FSGroupDeployment + moreThan + "yaml: unmarshal errors:", "field namespace not found"}},
			"a patch target":             {replace(FSGroupKustomization, "  - path: deployment-fs-group.yaml\n", "  - path: deployment-fs-group.yaml\n    target: {kind: Deployment}\n"), []string{FSGroupKustomization + ": yaml: unmarshal errors:", "field target not found"}},
			"an inline patch":            {replace(FSGroupKustomization, "  - path: job-fs-group.yaml\n", "  - path: job-fs-group.yaml\n  - patch: '{\"spec\": {}}'\n"), []string{FSGroupKustomization + ": yaml: unmarshal errors:", "field patch not found"}},
			"a patch with options":       {replace(FSGroupKustomization, "  - path: deployment-fs-group.yaml\n", "  - path: deployment-fs-group.yaml\n    options: {allowNameChange: true}\n"), []string{FSGroupKustomization + ": yaml: unmarshal errors:", "field options not found"}},
			"no patch of the Job":        {remove(FSGroupJob), []string{notExactly}},
			"no patch of the Deployment": {remove(FSGroupDeployment), []string{notExactly}},
			"no kustomization":           {remove(FSGroupKustomization), []string{notExactly}},
			"the Component fs-group without the Component secret-files": {func(files map[string][]byte) error {
				for _, file := range []string{SecretFilesKustomization, SecretFilesRemoval, SecretFilesMounts, SecretFilesJobRemoval, SecretFilesJobMounts} {
					delete(files, file)
				}
				return nil
			}, []string{"the base holds the Component " + FSGroupFolder + " but not the Component " + SecretFilesFolder + "; the base writes the two together"}},
			"another file in the Component": {func(files map[string][]byte) error {
				files[FSGroupFolder+"/other.yaml"] = []byte("kind: ConfigMap\n")
				return nil
			}, []string{FSGroupFolder + "/other.yaml is not a file of the Kubernetes base"}},
		} {
			files := maps.Clone(base.Files)
			if err := probe.change(files); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if _, err := LoadBase(files); err == nil {
				t.Errorf("LoadBase accepts a Component fs-group with %s", name)
			} else {
				for _, reason := range probe.reason {
					if !strings.Contains(err.Error(), reason) {
						t.Errorf("LoadBase refuses a Component fs-group with %s, but not for the reason %q: %v", name, reason, err)
					}
				}
			}
		}
		// A base with no migration Job gets the patch of the Deployment alone, and LoadBase refuses a patch of a Job.
		fixture := secretsFixture()
		store := &fixture[0]
		store.Variables = slices.DeleteFunc(store.Variables, func(variable Variable) bool { return variable.ReadByMode != "" })
		store.Needs = store.Needs[:1]
		requirements, err := WriteRequirements("orders", fixture)
		if err != nil {
			t.Fatal(err)
		}
		noJob, _, err := WriteBase("orders", testRelease, requirements.Requirements, healthCheckProbes, nil)
		if err != nil {
			t.Fatal(err)
		}
		if noJob.Files[MigrationJobFile] != nil || noJob.Files[FSGroupJob] != nil || noJob.Files[FSGroupDeployment] == nil {
			t.Fatalf("a base with no migration Job holds the files %v", slices.Sorted(maps.Keys(noJob.Files)))
		}
		files := maps.Clone(noJob.Files)
		files[FSGroupJob] = base.Files[FSGroupJob]
		if _, err := LoadBase(files); err == nil {
			t.Error("LoadBase accepts a patch of a Job in a base with no migration Job")
		}
	})
}

func TestLongServiceName(t *testing.T) {
	t.Run("ST-DLV-UC07-R03-T13 A service name of 57 to 63 characters gets its ConfigMap reference, and a secret name of more than 63 characters is refused with a clear message", func(t *testing.T) {
		port := "8080"
		service := strings.Repeat("s", 63)
		configOnly := []ComponentNeeds{{Component: "health", Needs: []Need{{Kind: "ingress", Name: "http", Port: 8080}},
			Variables: []Variable{{Name: "PORT", Type: "integer", Default: &port, Description: "The port."},
				{Name: "STEGO_ISSUER", Type: "url", Required: true, Description: "The issuer."}}}}
		files, err := WriteRequirements(service, configOnly)
		if err != nil {
			t.Fatal(err)
		}
		base, _, err := WriteBase(service, testRelease, files.Requirements, healthCheckProbes, nil)
		if err != nil {
			t.Fatalf("a service name of 63 characters is refused: %v", err)
		}
		if !strings.Contains(string(base.Files[DeploymentFile]), "name: "+ConfigMapName(service)+"\n") {
			t.Errorf("the base does not take STEGO_ISSUER from the ConfigMap %s", ConfigMapName(service))
		}
		_, err = WriteRequirements(service, secretsFixture())
		if err == nil || !strings.Contains(err.Error(), service+"-database") || !strings.Contains(err.Error(), "more than the 63") || !strings.Contains(err.Error(), "use a shorter service name") {
			t.Errorf("a secret name of more than 63 characters is not refused with a clear message: %v", err)
		}
		// A reference of more than 253 characters, or with a capital letter, is refused.
		for _, name := range []string{strings.Repeat("a", 254), "Orders-config", "orders_config"} {
			changed := maps.Clone(base.Files)
			changed[DeploymentFile] = []byte(strings.Replace(string(base.Files[DeploymentFile]), "name: "+ConfigMapName(service)+"\n", "name: "+name+"\n", 1))
			if _, err := LoadBase(changed); err == nil || !strings.Contains(err.Error(), "DNS subdomain") {
				t.Errorf("LoadBase does not refuse the ConfigMap reference %q with a clear message: %v", name, err)
			}
		}
	})
}
