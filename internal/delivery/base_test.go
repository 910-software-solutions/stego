package delivery

import (
	"os"
	"path/filepath"
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
