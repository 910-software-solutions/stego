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
	"text/template"

	"gopkg.in/yaml.v3"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/filesys"

	"github.com/jsell-rh/stego/internal/appbuild"
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

	t.Run("ST-DLV-UC07-R03-T03 The written Kubernetes base always sets the restricted security context and a read-only root file system", func(t *testing.T) {
		resources := buildBase(t, base)
		for _, resource := range resources {
			if resource["kind"] != "Deployment" {
				continue
			}
			pod := field(resource, "spec", "template", "spec")
			if field(pod, "securityContext", "runAsNonRoot") != true || field(pod, "securityContext", "seccompProfile", "type") != "RuntimeDefault" {
				t.Errorf("the pod security context is %v, want runAsNonRoot and the RuntimeDefault seccomp profile", field(pod, "securityContext"))
			}
			volumes, _ := field(pod, "volumes").([]any)
			if len(volumes) != 1 || field(volumes[0], "name") != "tmp" || field(volumes[0], "emptyDir", "sizeLimit") == nil {
				t.Errorf("the pod volumes are %v, want one bounded emptyDir tmp", volumes)
			}
		}
		for _, container := range containersOf(t, resources) {
			context := field(container, "securityContext")
			drop, _ := field(context, "capabilities", "drop").([]any)
			if field(context, "allowPrivilegeEscalation") != false || field(context, "readOnlyRootFilesystem") != true || len(drop) != 1 || drop[0] != "ALL" {
				t.Errorf("container %v: the security context is %v, want no privilege escalation, a read-only root and every capability dropped", container["name"], context)
			}
			mounts, _ := field(container, "volumeMounts").([]any)
			if len(mounts) != 1 || field(mounts[0], "mountPath") != "/tmp" {
				t.Errorf("container %v: the mounts are %v, want tmp at /tmp", container["name"], mounts)
			}
		}
	})

	t.Run("ST-DLV-UC07-R02-T02 The written Kubernetes base names an image that no registry serves, and the plan says each overlay must set the digest", func(t *testing.T) {
		for _, container := range containersOf(t, buildBase(t, base)) {
			if image := container["image"]; image != "stego.invalid/orders:set-by-overlay" {
				t.Errorf("container %v: the image is %v, want the placeholder stego.invalid/orders:set-by-overlay", container["name"], image)
			}
		}
		if !strings.Contains(printed, "every overlay must set the image by digest") {
			t.Errorf("the plan does not say that each overlay must set the image digest:\n%s", printed)
		}
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
			// The migration Job carries the annotations of an Argo CD hook (ST-DLV-UC07-R06), which name no Argo CD
			// object: no file holds an apiVersion of Argo CD.
			for _, word := range []string{"kind: ApplicationSet", "kind: Application\n", "apiVersion: argoproj.io", "destination:", "project:"} {
				if bytes.Contains(data, []byte(word)) {
					t.Errorf("the generated file %s holds %q", name, word)
				}
			}
		}
	})

	t.Run("ST-DLV-UC03-R04-T03 Apply writes the offline Containerfile: a pinned Go build with the network off, then a scratch image that runs as a user that is not root", func(t *testing.T) {
		// The compiler keeps the path that Delivering writes as a file of the project, and the record tracks it.
		if compiler.ContainerFile != delivery.ContainerFile {
			t.Fatalf("the compiler keeps %s as the Containerfile; Delivering writes %s", compiler.ContainerFile, delivery.ContainerFile)
		}
		if !strings.Contains(printed, "generate: "+delivery.ContainerFile) {
			t.Fatalf("the plan does not list %s:\n%s", delivery.ContainerFile, printed)
		}
		record, err := os.ReadFile(filepath.Join(dir, ".stego", "state.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		var state struct {
			LastApplied struct {
				ProjectFiles map[string]any `yaml:"project_files"`
			} `yaml:"last_applied"`
		}
		if err := yaml.Unmarshal(record, &state); err != nil {
			t.Fatal(err)
		}
		if tracked := state.LastApplied.ProjectFiles; tracked[delivery.ContainerFile] == nil {
			t.Fatalf("the record does not track %s under project_files: %v", delivery.ContainerFile, slices.Sorted(maps.Keys(tracked)))
		}
		data, err := os.ReadFile(filepath.Join(dir, delivery.ContainerFile))
		if err != nil {
			t.Fatal(err)
		}
		// The test reads the file with its own small parser: comments are dropped and a trailing backslash joins
		// the next line.
		var stages [][]string
		var froms []string
		joined := ""
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if joined == "" && (line == "" || strings.HasPrefix(line, "#")) {
				continue
			}
			if rest, ok := strings.CutSuffix(line, "\\"); ok {
				joined += strings.TrimSpace(rest) + " "
				continue
			}
			joined += line
			if keyword, args, _ := strings.Cut(joined, " "); keyword == "FROM" {
				froms = append(froms, args)
				stages = append(stages, nil)
			} else if len(stages) == 0 {
				t.Fatalf("the instruction %q comes before FROM", joined)
			} else {
				stages[len(stages)-1] = append(stages[len(stages)-1], joined)
			}
			joined = ""
		}
		if len(froms) != 2 {
			t.Fatalf("the Containerfile has the stages %v, not a build stage and a final stage", froms)
		}
		goImage := "docker.io/library/golang:" + strings.TrimPrefix(appbuild.GoVersion, "go") + "-"
		image, alias, _ := strings.Cut(froms[0], " AS ")
		name, digest, pinned := strings.Cut(image, "@sha256:")
		if !pinned || len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" || !strings.HasPrefix(name, goImage) || alias != "build" {
			t.Errorf("the build stage is FROM %q, not the Go image of %s named by its digest", froms[0], appbuild.GoVersion)
		}
		if froms[1] != "scratch" {
			t.Errorf("the final stage is FROM %q, not scratch", froms[1])
		}
		var runs []string
		for _, instruction := range stages[0] {
			keyword, args, _ := strings.Cut(instruction, " ")
			switch keyword {
			case "RUN":
				runs = append(runs, args)
			case "COPY", "WORKDIR":
			default:
				t.Errorf("the build stage holds %q; only WORKDIR, COPY and one RUN may be in it, and no ADD that fetches", instruction)
			}
		}
		if !slices.Contains(stages[0], "COPY go.mod go.sum ./") || !slices.Contains(stages[0], "COPY out/ ./out/") {
			t.Errorf("the build stage does not copy the module files and the generated output: %v", stages[0])
		}
		if len(runs) != 1 {
			t.Fatalf("the build stage runs %v, not the one build", runs)
		}
		run := runs[0]
		if !strings.HasPrefix(run, "env -i ") {
			t.Errorf("the build does not drop the settings of the image and the host: %s", run)
		}
		settings := appbuild.BuildEnvironment()
		settings["GOMODCACHE"] = delivery.PrefetchedModules
		for key, value := range settings {
			if !strings.Contains(run, "'"+key+"="+value+"'") {
				t.Errorf("the build does not set %s=%s, the setting of the application build", key, value)
			}
		}
		if settings["GOPROXY"] != "off" || settings["GOFLAGS"] != "" {
			t.Errorf("the application build settings are not offline: %v", settings)
		}
		build := "go " + strings.Join(appbuild.BuildFlags(delivery.ImageExecutable, "out"), " ")
		for _, part := range []string{"test \"$(go env GOVERSION)\" = " + appbuild.GoVersion + " ", "go mod verify && " + build + "'"} {
			if !strings.Contains(run, part) {
				t.Errorf("the build does not run %q: %s", part, run)
			}
		}
		for _, fetch := range []string{"go mod download", "curl", "wget", "http://", "https://"} {
			if strings.Contains(run, fetch) {
				t.Errorf("the build fetches with %q: %s", fetch, run)
			}
		}
		var user, entrypoint string
		for _, instruction := range stages[1] {
			keyword, args, _ := strings.Cut(instruction, " ")
			switch keyword {
			case "COPY":
				if !strings.HasPrefix(args, "--from=build ") {
					t.Errorf("the final stage copies %q from the build context", args)
				}
			case "USER":
				user = args
			case "ENTRYPOINT":
				entrypoint = args
			default:
				t.Errorf("the final stage holds %q; a scratch image that writes no file runs nothing", instruction)
			}
		}
		// The base runs the pod as non-root with a read-only root file system and an emptyDir at /tmp: a numeric
		// user that is not root passes runAsNonRoot, and the image needs no other writable folder.
		uid, gid, _ := strings.Cut(user, ":")
		if n, err := strconv.Atoi(uid); err != nil || n == 0 {
			t.Errorf("the image runs as the user %q, not a numeric user that is not root", user)
		}
		if n, err := strconv.Atoi(gid); err != nil || n == 0 {
			t.Errorf("the image runs as the group of %q, not a numeric group that is not root", user)
		}
		if entrypoint != `["`+delivery.ImageExecutable+`"]` {
			t.Errorf("the image starts %s, not the executable by its absolute path in exec form", entrypoint)
		}
		containers := containersOf(t, buildBase(t, base))
		for _, container := range containers {
			if field(container, "securityContext", "readOnlyRootFilesystem") != true {
				t.Errorf("the container %v has no read-only root file system", container["name"])
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

// deliveryStanza is the delivery stanza of ST-DLV-TD01: the hosted CI provider, the Argo CD project orders, the
// service's repository with the path of its base, and the location of the platform's environment files.
const deliveryStanza = `delivery:
  argocd:
    project: orders
    source: {repo: https://git.example/orders.git, path: deploy/base}
    environments: {repo: https://git.example/platform.git, revision: main, path: environments/orders/*.yaml}
`

// deliveryProject makes the project of ST-DLV-TD01 with the declaration and the delivery stanza.
func deliveryProject(t *testing.T, stanza string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "orders")
	writeRequirementsDeclaration(t, dir, requirementsDeclaration+stanza)
	git(t, dir, "init", "-q")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "the service orders")
	return dir
}

func TestDeliveryObjectApply(t *testing.T) {
	registry := requirementsRegistry(t)
	env := requirementsEnv(registry)
	dir := deliveryProject(t, deliveryStanza)
	printed := mustRunInProject(t, dir, env, "apply")
	readSet := func(t *testing.T) (map[string]any, delivery.Application) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, delivery.ApplicationSetFile))
		if err != nil {
			t.Fatalf("apply writes no %s: %v", delivery.ApplicationSetFile, err)
		}
		application, err := delivery.LoadApplicationSet(data)
		if err != nil {
			t.Fatal(err)
		}
		var object map[string]any
		if err := yaml.Unmarshal(data, &object); err != nil {
			t.Fatal(err)
		}
		return object, application
	}

	t.Run("ST-DLV-UC07-R04-T01 Applying the plan writes an ApplicationSet whose environment values come from the platform's environment files", func(t *testing.T) {
		object, application := readSet(t)
		if application.Service != "orders" || application.Project != "orders" {
			t.Errorf("the ApplicationSet names the service %q and the project %q", application.Service, application.Project)
		}
		// The overlay pulls the base pinned to a commit; STEGO states the line in the plan and in the file header.
		line := "https://git.example/orders.git//deploy/base?ref=<the full 40-character commit of the release>"
		data, _ := os.ReadFile(filepath.Join(dir, delivery.ApplicationSetFile))
		if !strings.Contains(printed, line) || !strings.Contains(string(data), line) {
			t.Errorf("the plan and the file header do not both state the resource line %s:\n%s", line, printed)
		}
		if !strings.Contains(printed, "locked AppProject") {
			t.Errorf("the plan does not say that the platform must lock the AppProject:\n%s", printed)
		}
		generators, _ := field(object, "spec", "generators").([]any)
		files, _ := field(generators[0], "git", "files").([]any)
		if field(generators[0], "git", "repoURL") != "https://git.example/platform.git" || len(files) != 1 || field(files[0], "path") != "environments/orders/*.yaml" {
			t.Errorf("the files generator is %v, want the platform's environment files of the stanza", generators)
		}
		spec := field(object, "spec", "template", "spec")
		if field(spec, "project") != "orders" {
			t.Errorf("the template project is %v", field(spec, "project"))
		}
		for name, want := range map[string]string{"server": "{{.destination.cluster}}", "namespace": "{{.destination.namespace}}"} {
			if got := field(spec, "destination", name); got != want {
				t.Errorf("the destination %s is %v, want the parameter %s", name, got, want)
			}
		}
		source := field(spec, "source")
		if field(spec, "sources") != nil || field(source, "path") != "{{.destination.overlay_path}}" || field(source, "repoURL") != "https://git.example/platform.git" {
			t.Errorf("the template has the source %v and the sources %v, want one source: the overlay of the environment file", source, field(spec, "sources"))
		}

		// The contract fake of the Argo CD files generator: it reads the environment file of ST-DLV-TD02 as the
		// parameters and renders the template with Go templates and missingkey=error, as Argo CD does with
		// goTemplate. Each destination value comes from the environment file.
		data, err := os.ReadFile(filepath.Join(environmentFixtureDir, "dev.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		var parameters map[string]any
		if err := yaml.Unmarshal(data, &parameters); err != nil {
			t.Fatal(err)
		}
		render := func(text any) string {
			t.Helper()
			parsed, err := template.New("field").Option("missingkey=error").Parse(text.(string))
			if err != nil {
				t.Fatal(err)
			}
			var out strings.Builder
			if err := parsed.Execute(&out, parameters); err != nil {
				t.Fatalf("the generator cannot render %v from the environment file: %v", text, err)
			}
			return out.String()
		}
		for value, want := range map[any]any{
			field(spec, "destination", "server"):    field(parameters, "destination", "cluster"),
			field(spec, "destination", "namespace"): field(parameters, "destination", "namespace"),
			field(source, "path"):                   field(parameters, "destination", "overlay_path"),
		} {
			if got := render(value); got != want {
				t.Errorf("the template field %v renders %q, want %v from the environment file", value, got, want)
			}
		}
		if got := render(field(object, "spec", "template", "metadata", "name")); got != "orders-dev" {
			t.Errorf("the Application of dev is named %q, want orders-dev", got)
		}
	})

	t.Run("ST-DLV-UC07-FL-T Applying the plan writes the Kubernetes base and then the ApplicationSet from the delivery stanza", func(t *testing.T) {
		for _, file := range []string{delivery.KustomizationFile, delivery.DeploymentFile, delivery.ApplicationSetFile} {
			if !strings.Contains(printed, "generate: "+file) {
				t.Errorf("the plan does not list %s:\n%s", file, printed)
			}
		}
		buildBase(t, filepath.Join(dir, delivery.BaseFolder))
		readSet(t)
	})

	t.Run("ST-DLV-PO04-T The run that writes the Kubernetes base also writes the delivery object, or says in the plan that it writes none", func(t *testing.T) {
		state, err := compiler.LoadState(filepath.Join(dir, ".stego", "state.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range []string{delivery.KustomizationFile, delivery.ApplicationSetFile} {
			if state.LastApplied.ProjectFiles[file] == "" {
				t.Errorf("the generation record does not list %s", file)
			}
		}
		if strings.Contains(printed, delivery.NoDeliveryStanza) {
			t.Errorf("the plan says no delivery object is written, but the stanza is there:\n%s", printed)
		}
		without := deliveryProject(t, "")
		second := mustRunInProject(t, without, env, "apply")
		if !strings.Contains(second, "generate: "+delivery.KustomizationFile) || !strings.Contains(second, delivery.NoDeliveryStanza) {
			t.Errorf("the run with no stanza does not write the base and say that it writes no delivery object:\n%s", second)
		}
		if _, err := os.Stat(filepath.Join(without, delivery.ApplicationSetFile)); err == nil {
			t.Error("the run with no stanza writes an ApplicationSet")
		}
	})

	t.Run("ST-DLV-UC07-R04-T02 A delivery stanza that names a destination, a namespace or an overlay is refused, naming the path, and nothing is written", func(t *testing.T) {
		for field, line := range map[string]string{
			"delivery.argocd.destination": "    destination: {cluster: prod}\n",
			"delivery.argocd.namespace":   "    namespace: orders-prod\n",
			"delivery.argocd.overlay":     "    overlay: overlays/prod\n",
		} {
			stanza := strings.Replace(deliveryStanza, "    project: orders\n", "    project: orders\n"+line, 1)
			project := deliveryProject(t, stanza)
			before := folderDigests(t, project)
			_, stderr, err := runInProject(t, project, env, "apply")
			if err == nil {
				t.Errorf("apply accepts a delivery stanza with %s", field)
				continue
			}
			if !strings.Contains(stderr, field) {
				t.Errorf("the refusal does not name %s:\n%s", field, stderr)
			}
			if after := folderDigests(t, project); !maps.Equal(before, after) {
				t.Errorf("the refused apply with %s changed the project folder", field)
			}
		}
	})
}

func TestDeliveryStanzaChecks(t *testing.T) {
	registry := requirementsRegistry(t)
	env := requirementsEnv(registry)
	t.Run("ST-DLV-UC07-R04-T03 The project default and an unknown CI provider are refused by stego validate and stego apply, naming the path", func(t *testing.T) {
		for field, stanza := range map[string]string{
			"delivery.argocd.project":     strings.Replace(deliveryStanza, "project: orders", "project: default", 1),
			"delivery.ci":                 "delivery:\n  ci: jenkins\n",
			"delivery.argocd.source.repo": strings.Replace(deliveryStanza, "https://git.example/orders.git", "ssh://git@-oproxy/orders.git", 1),
		} {
			project := deliveryProject(t, stanza)
			before := folderDigests(t, project)
			for _, command := range []string{"validate", "apply"} {
				stdout, stderr, err := runInProject(t, project, env, command)
				if err == nil {
					t.Errorf("stego %s accepts %s:\n%s", command, field, stdout)
					continue
				}
				if !strings.Contains(stdout+stderr, field) {
					t.Errorf("stego %s does not name %s:\n%s\n%s", command, field, stdout, stderr)
				}
			}
			if after := folderDigests(t, project); !maps.Equal(before, after) {
				t.Errorf("a refused run with %s changed the project folder", field)
			}
		}
	})
}

func TestContainerfileLocalModules(t *testing.T) {
	t.Run("ST-DLV-UC03-R04-T05 Apply copies each local module folder that go.mod replaces a module with into the build stage, and names a replacement out of the project", func(t *testing.T) {
		dir, env, _ := appliedBase(t)
		module, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err != nil {
			t.Fatal(err)
		}
		// Two folders of the project, one of them twice and one in the output folder, and one folder out of it.
		replaces := "\nreplace example.com/lib => ./lib\n\nreplace example.com/tools/v2 => ./third_party/tools\n\n" +
			"replace example.com/again => ./lib/\n\nreplace example.com/inside => ./out/vendored\n\nreplace example.com/far => ../far\n"
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), append(module, replaces...), 0o644); err != nil {
			t.Fatal(err)
		}
		printed := mustRunInProject(t, dir, env, "apply")
		data, err := os.ReadFile(filepath.Join(dir, delivery.ContainerFile))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := delivery.LoadContainerfile(data); err != nil {
			t.Fatalf("the Containerfile with the local module folders does not read back: %v", err)
		}
		want := "COPY go.mod go.sum ./\nCOPY out/ ./out/\nCOPY lib/ ./lib/\nCOPY third_party/tools/ ./third_party/tools/\nRUN "
		if !strings.Contains(string(data), want) {
			t.Errorf("the build stage does not copy the output folder and then each local module folder once, in path order:\n%s", data)
		}
		for _, folder := range []string{"out/vendored", "far"} {
			if strings.Contains(string(data), "COPY "+folder) || strings.Contains(string(data), "COPY ../") {
				t.Errorf("the build stage copies %s:\n%s", folder, data)
			}
		}
		if !strings.Contains(printed, "the Containerfile does not copy the local module ../far that go.mod replaces a module with: it is out of the project folder") {
			t.Errorf("the plan does not name the local module out of the project folder:\n%s", printed)
		}
	})
}

func TestDeployFolderLink(t *testing.T) {
	t.Run("ST-DLV-UC01-R05-T05 Apply refuses a deploy folder that is a symbolic link and writes nothing through it", func(t *testing.T) {
		registry := requirementsRegistry(t)
		env := requirementsEnv(registry)
		for _, link := range []string{"deploy", delivery.BaseFolder} {
			dir := requirementsProject(t)
			outside := t.TempDir()
			if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, link)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(dir, link)); err != nil {
				t.Fatal(err)
			}
			_, stderr, err := runInProject(t, dir, env, "apply")
			if err == nil {
				t.Errorf("%s: apply accepted a folder that is a symbolic link", link)
			} else if !strings.Contains(stderr, "symbolic link") {
				t.Errorf("%s: the refusal does not name the symbolic link:\n%s", link, stderr)
			}
			if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
				t.Errorf("%s: apply wrote through the link: %v, %v", link, entries, err)
			}
			if _, err := os.Stat(filepath.Join(dir, ".stego", "state.yaml")); !os.IsNotExist(err) {
				t.Errorf("%s: the refused apply wrote the record: %v", link, err)
			}
		}
	})
}
