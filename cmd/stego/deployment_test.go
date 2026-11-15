package main

import (
	"bytes"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/filesys"

	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/delivery"
)

// These tests prove that apply writes the Kubernetes base of a service (ST-DLV-UC07), on the service of
// ST-DLV-TD01. The declaration has no delivery stanza yet (ST-DLV-Q12), so the tests of the delivery object wait.

// appliedBase applies the plan to a new project of ST-DLV-TD01 and gives the project folder, the environment of
// its runs and the plan that apply printed.
func appliedBase(t *testing.T) (string, []string, string) {
	t.Helper()
	registry := requirementsRegistry(t)
	dir := requirementsProject(t)
	env := requirementsEnv(registry)
	printed := mustRunInProject(t, dir, env, "apply")
	return dir, env, printed
}

// buildBase builds the folder with the kustomize library on the disk, with plugins off and the load restriction to
// the folder, as plain kustomize builds it, and gives each built resource.
func buildBase(t *testing.T, folder string) []map[string]any {
	t.Helper()
	options := krusty.MakeDefaultOptions()
	options.PluginConfig = types.DisabledPluginConfig()
	options.LoadRestrictions = types.LoadRestrictionsRootOnly
	built, err := krusty.MakeKustomizer(options).Run(filesys.MakeFsOnDisk(), folder)
	if err != nil {
		t.Fatalf("kustomize does not build %s: %v", folder, err)
	}
	data, err := built.AsYaml()
	if err != nil {
		t.Fatal(err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var resources []map[string]any
	for {
		var resource map[string]any
		if err := decoder.Decode(&resource); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		resources = append(resources, resource)
	}
	if len(resources) == 0 {
		t.Fatalf("the build of %s gives no resource", folder)
	}
	return resources
}

// field gives the value at the path of keys in the resource, or nil.
func field(value any, keys ...string) any {
	for _, key := range keys {
		object, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		value = object[key]
	}
	return value
}

// containersOf gives each container of each Deployment of the resources.
func containersOf(t *testing.T, resources []map[string]any) []map[string]any {
	t.Helper()
	var containers []map[string]any
	for _, resource := range resources {
		if resource["kind"] != "Deployment" {
			continue
		}
		list, _ := field(resource, "spec", "template", "spec", "containers").([]any)
		for _, item := range list {
			containers = append(containers, item.(map[string]any))
		}
	}
	if len(containers) == 0 {
		t.Fatalf("the base holds no Deployment container: %v", resources)
	}
	return containers
}

func TestKubernetesBaseApply(t *testing.T) {
	dir, env, printed := appliedBase(t)
	base := filepath.Join(dir, delivery.BaseFolder)

	t.Run("ST-DLV-UC07-R01-T01 The written Kubernetes base builds with plain kustomize, with no plugin or remote base", func(t *testing.T) {
		// The compiler keeps, as files of the project folder, exactly the paths that Delivering writes for the base.
		if !slices.Equal(compiler.BaseFiles, delivery.BaseFiles) {
			t.Fatalf("the compiler keeps %v as the files of the base; Delivering writes %v", compiler.BaseFiles, delivery.BaseFiles)
		}
		if !strings.Contains(printed, "generate: "+delivery.KustomizationFile) {
			t.Fatalf("the plan does not list %s:\n%s", delivery.KustomizationFile, printed)
		}
		buildBase(t, base)
		data, err := os.ReadFile(filepath.Join(dir, delivery.KustomizationFile))
		if err != nil {
			t.Fatal(err)
		}
		var kustomization map[string]any
		if err := yaml.Unmarshal(data, &kustomization); err != nil {
			t.Fatal(err)
		}
		for key := range kustomization {
			if !slices.Contains([]string{"apiVersion", "kind", "resources"}, key) {
				t.Errorf("the kustomization file has the field %s; a plain base lists its resources only", key)
			}
		}
		resources, _ := kustomization["resources"].([]any)
		if len(resources) == 0 {
			t.Fatal("the kustomization file lists no resource")
		}
		for _, item := range resources {
			name, _ := item.(string)
			if name == "" || strings.Contains(name, "/") || strings.Contains(name, ":") || strings.Contains(name, "..") {
				t.Errorf("the kustomization file lists %q, which is not a file of the base folder", item)
				continue
			}
			if _, err := os.Stat(filepath.Join(base, name)); err != nil {
				t.Errorf("the kustomization file lists %s, which is not in the base folder: %v", name, err)
			}
		}
	})

	t.Run("ST-DLV-UC07-R02-T01 The written Kubernetes base sets no replicas, PodDisruptionBudget, namespace, image digest, host name, egress address or secret value", func(t *testing.T) {
		for _, resource := range buildBase(t, base) {
			kind, _ := resource["kind"].(string)
			name := fmt.Sprintf("%s %v", kind, field(resource, "metadata", "name"))
			if field(resource, "spec", "replicas") != nil {
				t.Errorf("%s sets replicas", name)
			}
			if field(resource, "metadata", "namespace") != nil {
				t.Errorf("%s sets a metadata namespace", name)
			}
			switch kind {
			case "PodDisruptionBudget":
				t.Errorf("the base holds the PodDisruptionBudget %s", name)
			case "Secret":
				if resource["data"] != nil || resource["stringData"] != nil {
					t.Errorf("%s holds data", name)
				}
			case "Ingress", "Route":
				text, _ := yaml.Marshal(resource["spec"])
				if strings.Contains(string(text), "host") {
					t.Errorf("%s names a host", name)
				}
			case "NetworkPolicy":
				text, _ := yaml.Marshal(resource["spec"])
				if strings.Contains(string(text), "ipBlock") || strings.Contains(string(text), "cidr") {
					t.Errorf("%s names an address or address range", name)
				}
			}
			for _, container := range func() []any {
				list, _ := field(resource, "spec", "template", "spec", "containers").([]any)
				init, _ := field(resource, "spec", "template", "spec", "initContainers").([]any)
				return append(list, init...)
			}() {
				image, _ := field(container, "image").(string)
				if strings.Contains(image, "@") {
					t.Errorf("%s names the image %s by digest", name, image)
				}
				text, _ := yaml.Marshal(field(container, "env"))
				if strings.Contains(string(text), "value:") {
					t.Errorf("%s gives an environment value: %s", name, text)
				}
			}
		}
	})

	t.Run("ST-DLV-UC07-R03-T01 The written Kubernetes base carries the probes that health-check derives", func(t *testing.T) {
		requirements, _ := readRequirements(t, dir)
		port := 0
		for _, variable := range requirements.Environment {
			if variable.Name == "PORT" && variable.Default != nil {
				port, _ = strconv.Atoi(*variable.Default)
			}
		}
		if port == 0 {
			t.Fatal("the platform requirements state no default for PORT, which health-check declares")
		}
		for _, container := range containersOf(t, buildBase(t, base)) {
			for probe, path := range map[string]string{"livenessProbe": "/livez", "readinessProbe": "/readyz"} {
				if got := field(container, probe, "httpGet", "path"); got != path {
					t.Errorf("container %v: %s path is %v, want %s", container["name"], probe, got, path)
				}
				if got := field(container, probe, "httpGet", "port"); got != port {
					t.Errorf("container %v: %s port is %v, want %d", container["name"], probe, got, port)
				}
			}
		}
	})

	t.Run("ST-DLV-UC07-R03-T02 The written Kubernetes base carries default resources that an overlay can change", func(t *testing.T) {
		defaults := map[string]map[string]string{
			"requests": {"cpu": "100m", "memory": "128Mi", "ephemeral-storage": "32Mi"},
			"limits":   {"cpu": "1", "memory": "512Mi", "ephemeral-storage": "128Mi"},
		}
		expect := func(t *testing.T, resources []map[string]any, memoryLimit string) {
			t.Helper()
			for _, container := range containersOf(t, resources) {
				for group, values := range defaults {
					for resource, value := range values {
						if group == "limits" && resource == "memory" {
							value = memoryLimit
						}
						if got := field(container, "resources", group, resource); got != value {
							t.Errorf("container %v: %s %s is %v, want %s", container["name"], group, resource, got, value)
						}
					}
				}
			}
		}
		expect(t, buildBase(t, base), "512Mi")
		overlay := filepath.Join(dir, "deploy", "overlay-test")
		if err := os.MkdirAll(overlay, 0o755); err != nil {
			t.Fatal(err)
		}
		write := func(name, text string) {
			if err := os.WriteFile(filepath.Join(overlay, name), []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		write("kustomization.yaml", "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: [../base]\npatches: [{path: memory.yaml}]\n")
		write("memory.yaml", "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: orders}\nspec:\n  template:\n    spec:\n      containers:\n        - name: orders\n          resources: {limits: {memory: 1Gi}}\n")
		expect(t, buildBase(t, overlay), "1Gi")
	})

	t.Run("ST-DLV-UC07-R05-T01 With no delivery stanza, applying the plan writes no ApplicationSet, says so, and invents no destination", func(t *testing.T) {
		if !strings.Contains(printed, "no delivery object (Argo CD ApplicationSet) is written for the base of orders: "+delivery.NoDeliveryStanza) {
			t.Fatalf("the plan does not say that no delivery object is written:\n%s", printed)
		}
		for name := range folderDigests(t, dir) {
			if strings.HasPrefix(name, "deploy/overlay-test/") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			for _, word := range []string{"kind: ApplicationSet", "kind: Application\n", "argoproj.io", "destination:", "project:"} {
				if bytes.Contains(data, []byte(word)) {
					t.Errorf("the generated file %s holds %q", name, word)
				}
			}
		}
	})

	t.Run("ST-GEN-UC03-R03-T03 Apply and the drift check name a hand edit of the Kubernetes base, and apply writes nothing", func(t *testing.T) {
		edited := filepath.Join(dir, delivery.DeploymentFile)
		data, err := os.ReadFile(edited)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(edited, append(data, []byte("# replicas: 3\n")...), 0o644); err != nil {
			t.Fatal(err)
		}
		before := folderDigests(t, dir)
		_, stderr, err := runInProject(t, dir, env, "apply")
		if err == nil {
			t.Fatal("apply accepted a hand edit of the Kubernetes base")
		}
		if !strings.Contains(stderr, "hand-edited") || !strings.Contains(stderr, delivery.DeploymentFile) {
			t.Fatalf("the refusal does not name %s as edited by hand:\n%s", delivery.DeploymentFile, stderr)
		}
		if after := folderDigests(t, dir); !maps.Equal(before, after) {
			t.Fatalf("the refused apply changed the project folder")
		}
		stdout, stderr, err := runInProject(t, dir, env, "drift")
		if err == nil {
			t.Fatalf("drift passed after a hand edit of the Kubernetes base:\n%s", stdout)
		}
		if !strings.Contains(stdout+stderr, "modified: "+delivery.DeploymentFile) {
			t.Fatalf("drift does not name %s:\n%s\n%s", delivery.DeploymentFile, stdout, stderr)
		}
	})
}
