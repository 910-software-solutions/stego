package delivery

import (
	"os"
	"path/filepath"
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
			"replicas: [{name: orders, count: 2}]\nimages: [{name: orders, newName: registry.example/orders, digest: " + digest + "}]\n"
		if err := os.WriteFile(filepath.Join(overlay, "kustomization.yaml"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		built := plainBuild(t, overlay)
		for _, want := range []string{"namespace: orders-test", "replicas: 2", "image: registry.example/orders@" + digest} {
			if !strings.Contains(built, want) {
				t.Errorf("the overlay build does not hold %q:\n%s", want, built)
			}
		}
		// The RoleBinding of the base names its ServiceAccount with no namespace, and the overlay sets it.
		if !strings.Contains(built, "kind: ServiceAccount\n  name: orders\n  namespace: orders-test") {
			t.Errorf("the overlay does not set the namespace of the RoleBinding subject:\n%s", built)
		}
	})
}
