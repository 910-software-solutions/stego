package delivery

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

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
		base, _, err := WriteBase("orders", requirements, healthCheckProbes)
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
// a required and an optional variable that are not secret, and the port of the probes.
func secretsFixture() []ComponentNeeds {
	canary := secretsCanary
	port := "8080"
	return []ComponentNeeds{
		{Component: "store", Settings: map[string]string{"connection_string": canary},
			Needs: []Need{{Kind: "secret", Name: "{service}-database", Keys: []string{"url"}}},
			Variables: []Variable{
				{Name: "DATABASE_URL", Type: "string", Description: "The connection string.", Secret: true, SecretSource: &SecretSource{Name: "{service}-database", Key: "url"}},
				{Name: "DATABASE_URL_FILE", Type: "path", Description: "The file of the connection string.", Example: &canary},
				{Name: "PGPASSWORD", Type: "string", Description: "Read by the driver.", Secret: true},
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
	}
}

// secretsBase writes the base of the fixture of secretsFixture for the service orders.
func secretsBase(t *testing.T) Base {
	t.Helper()
	files, err := WriteRequirements("orders", secretsFixture())
	if err != nil {
		t.Fatal(err)
	}
	base, _, err := WriteBase("orders", files.Requirements, healthCheckProbes)
	if err != nil {
		t.Fatal(err)
	}
	return base
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
		other, _, err := WriteBase("orders", written.Requirements, healthCheckProbes)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(other.Files[DeploymentFile]), "secretKeyRef") {
			t.Errorf("the base names a Secret for a source that no need gives:\n%s", other.Files[DeploymentFile])
		}
	})

	t.Run("ST-DLV-UC07-R03-T06 No file of the base or of the Component holds a value, and LoadBase refuses an entry with one", func(t *testing.T) {
		base := secretsBase(t)
		if len(base.Files) != 7 {
			t.Fatalf("the base has %d files, want the four of the base and the three of the Component", len(base.Files))
		}
		for file, data := range base.Files {
			if strings.Contains(string(data), secretsCanary) {
				t.Errorf("%s holds the canary text of a setting, a default or an example:\n%s", file, data)
			}
		}
		for name, entry := range envByName(t, containerOf(t, string(base.Files[DeploymentFile]))) {
			if _, set := entry["value"]; set {
				t.Errorf("the base sets the value of %s", name)
			}
		}
		for name, change := range map[string][2]string{
			"a value":                     {"            - name: PORT\n              valueFrom:\n", "            - name: PORT\n              value: " + secretsCanary + "\n              valueFrom:\n"},
			"a value in place of a ref":   {"            - name: PORT\n              valueFrom:\n                configMapKeyRef:\n                  key: PORT\n                  name: orders-config\n                  optional: true\n", "            - name: PORT\n              value: " + secretsCanary + "\n"},
			"a field reference":           {"                configMapKeyRef:\n                  key: PORT\n                  name: orders-config\n                  optional: true\n", "                fieldRef:\n                  fieldPath: metadata.name\n"},
			"an optional secret":          {"                  key: url\n                  name: orders-database\n", "                  key: url\n                  name: orders-database\n                  optional: true\n"},
			"optional false":              {"                  key: STEGO_ISSUER\n                  name: orders-config\n", "                  key: STEGO_ISSUER\n                  name: orders-config\n                  optional: false\n"},
			"two refs in one entry":       {"                secretKeyRef:\n", "                configMapKeyRef: {name: orders-config, key: X}\n                secretKeyRef:\n"},
			"one variable twice":          {"            - name: STEGO_ISSUER\n", "            - name: PORT\n"},
			"a ConfigMap that is no name": {"                  key: STEGO_ISSUER\n                  name: orders-config\n", "                  key: STEGO_ISSUER\n                  name: Orders_Config\n"},
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
			"STEGO_CURSOR_KEY_FILE": "/var/run/secrets/stego/orders-cursor/key"} {
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
		for _, secret := range []string{"orders-database", "orders-cursor"} {
			mount := mounts[secret]
			if mount["mountPath"] != "/var/run/secrets/stego/"+secret || mount["readOnly"] != true || mount["subPath"] != nil {
				t.Errorf("the secret %s is not mounted read-only at its folder with no subPath: %v", secret, mount)
			}
			source, _ := volumes[secret]["secret"].(map[string]any)
			if source["secretName"] != secret || source["defaultMode"] != SecretFileMode {
				t.Errorf("the volume of %s is not the secret with the mode 0400: %v", secret, volumes[secret])
			}
		}
		if mounts["tmp"] == nil || volumes["tmp"] == nil || len(mounts) != 3 || len(volumes) != 3 {
			t.Errorf("the build with the Component has the mounts %v and the volumes %v, want tmp and the two secrets", mounts, volumes)
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
			"a volume of another kind":             replace(SecretFilesMounts, "          secret:\n            defaultMode: 256\n            secretName: orders-cursor\n", "          emptyDir: {}\n"),
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

// mustYAML encodes a value as YAML for a comparison or a message.
func mustYAML(t *testing.T, value any) []byte {
	t.Helper()
	data, err := yaml.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
