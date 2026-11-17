package main

import (
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"

	"github.com/jsell-rh/stego/internal/registry"
)

// treeOf reads every file and folder under a folder, as projectFiles does, with the apply lock and with the
// target of each symbolic link.
func treeOf(t *testing.T, folder string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(folder, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(folder, name)
		switch {
		case entry.Type()&os.ModeSymlink != 0:
			target, err := os.Readlink(name)
			tree[relative+"@"] = target
			return err
		case entry.IsDir():
			tree[relative+"/"] = ""
			return nil
		}
		data, err := os.ReadFile(name)
		tree[relative] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

// declaredArchetypes lists the archetypes that the fixture registry's own folders hold.
func declaredArchetypes(t *testing.T) []string {
	t.Helper()
	registryDir, _ := declaringRegistry(t)
	entries, err := os.ReadDir(filepath.Join(registryDir, "archetypes"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	return names
}

func readConfig(t *testing.T, project string) registry.RegistryConfig {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(project, ".stego", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var config registry.RegistryConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	return config
}

func TestStartAServiceDeclaration(t *testing.T) {
	t.Parallel()
	t.Run("ST-DECL-UC01-R02-T01 A scaffold refuses to run when a declaration is present, and changes no existing file", func(t *testing.T) {
		forms := map[string]func(t *testing.T, project, outside string){
			"a file": func(t *testing.T, project, _ string) { putFile(t, project, "service.yaml", "kind: service\n") },
			"a folder": func(t *testing.T, project, _ string) {
				if err := os.Mkdir(filepath.Join(project, "service.yaml"), 0755); err != nil {
					t.Fatal(err)
				}
			},
			"a symbolic link to a folder outside the project": func(t *testing.T, project, outside string) {
				if err := os.Symlink(outside, filepath.Join(project, "service.yaml")); err != nil {
					t.Fatal(err)
				}
			},
			"a dangling symbolic link": func(t *testing.T, project, outside string) {
				if err := os.Symlink(filepath.Join(outside, "missing"), filepath.Join(project, "service.yaml")); err != nil {
					t.Fatal(err)
				}
			},
		}
		for name, make := range forms {
			project, outside := serviceFolder(t), t.TempDir()
			putFile(t, project, ".stego/config.yaml", declaringConfig(t))
			make(t, project, outside)
			before, beforeOutside := treeOf(t, project), treeOf(t, outside)
			run := stegoIn(t, project, nil, "init", "--archetype", "rest-crud")
			if run.code == 0 || !strings.Contains(run.stderr, "service.yaml") {
				t.Errorf("%s: want a refusal that names service.yaml, got exit %d: %s", name, run.code, run.stderr)
			}
			if !reflect.DeepEqual(before, treeOf(t, project)) || !reflect.DeepEqual(beforeOutside, treeOf(t, outside)) {
				t.Errorf("%s: the refused scaffold made, changed or removed a file", name)
			}
		}
		project := declaringProject(t)
		putFile(t, project, "README.md", "# todo\n")
		putFile(t, project, "fills/policy/fill.go", "package policy\n")
		before := treeOf(t, project)
		if run := stegoIn(t, project, nil, "init", "--archetype", "rest-crud"); run.code != 0 {
			t.Fatalf("stego init beside existing files: %s", run.stderr)
		}
		after := treeOf(t, project)
		if _, ok := after["service.yaml"]; !ok {
			t.Error("stego init wrote no declaration")
		}
		for name, data := range before {
			if after[name] != data {
				t.Errorf("stego init changed or removed %s", name)
			}
		}
	})

	t.Run("ST-DECL-UC01-R02-T02 A scaffold that fails part way removes each file and folder it made, so a rerun passes", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("a folder with no write permission does not stop the root user")
		}
		project := declaringProject(t)
		putFile(t, project, "README.md", "# todo\n")
		// The example content of rpc-service holds proto/service.proto; a folder proto with no write permission
		// makes that write fail after the earlier writes of the run.
		locked := filepath.Join(project, "proto")
		if err := os.Mkdir(locked, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(locked, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
		// The apply lock stays, as after each refused scaffold that took it.
		withoutLock := func(tree map[string]string) map[string]string {
			delete(tree, ".stego/apply.lock")
			return tree
		}
		before := withoutLock(treeOf(t, project))
		run := stegoIn(t, project, nil, "init", "--archetype", "rpc-service")
		if run.code == 0 || !strings.Contains(run.stderr, "permission denied") {
			t.Fatalf("want a refusal of the write into proto, got exit %d: %s%s", run.code, run.stdout, run.stderr)
		}
		if after := withoutLock(treeOf(t, project)); !reflect.DeepEqual(before, after) {
			t.Fatalf("the failed scaffold left files: before %v, after %v", before, after)
		}
		if err := os.Chmod(locked, 0o755); err != nil {
			t.Fatal(err)
		}
		if run := stegoIn(t, project, nil, "init", "--archetype", "rpc-service"); run.code != 0 {
			t.Fatalf("the rerun of stego init: exit %d: %s%s", run.code, run.stdout, run.stderr)
		}
		for _, name := range []string{"service.yaml", "proto/service.proto", "README.md"} {
			if _, err := os.Stat(filepath.Join(project, filepath.FromSlash(name))); err != nil {
				t.Errorf("%s is not there after the rerun: %v", name, err)
			}
		}
	})

	t.Run("ST-DECL-UC01-R03-T01 An unknown archetype is refused, with the archetypes the registry holds", func(t *testing.T) {
		project := declaringProject(t)
		before := treeOf(t, project)
		run := stegoIn(t, project, nil, "init", "--archetype", "no-such-archetype")
		if run.code == 0 {
			t.Fatal("stego init accepted an unknown archetype")
		}
		for _, name := range append([]string{"no-such-archetype"}, declaredArchetypes(t)...) {
			if !strings.Contains(run.stderr, name) {
				t.Errorf("the refusal does not name %s: %s", name, run.stderr)
			}
		}
		if !reflect.DeepEqual(before, treeOf(t, project)) {
			t.Error("the refused scaffold wrote a file or folder")
		}
	})

	t.Run("ST-DECL-UC01-R04-T02 The rest-crud scaffold holds no example content", func(t *testing.T) {
		project := declaringProject(t)
		if run := stegoIn(t, project, nil, "init", "--archetype", "rest-crud"); run.code != 0 {
			t.Fatalf("stego init: %s", run.stderr)
		}
		data, err := os.ReadFile(filepath.Join(project, "service.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		var service map[string]any
		if err := yaml.Unmarshal(data, &service); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"entities", "collections", "slots"} {
			if value, ok := service[key]; ok && reflect.ValueOf(value).Len() != 0 {
				t.Errorf("the scaffold holds %s: %v", key, value)
			}
		}
		var paths []string
		for path := range treeOf(t, project) {
			paths = append(paths, path)
		}
		want := []string{"./", ".stego/", ".stego/.gitignore", ".stego/apply.lock", ".stego/config.yaml", "fills/", "service.yaml"}
		if strings.Join(sortStrings(paths), " ") != strings.Join(want, " ") {
			t.Errorf("the project holds %v, want %v", sortStrings(paths), want)
		}
	})

	t.Run("ST-DECL-UC01-R06-T02 Init refuses while another process holds the apply lock, and writes nothing", func(t *testing.T) {
		project := declaringProject(t)
		putFile(t, project, ".stego/apply.lock", "")
		before := treeOf(t, project)
		// The helper is a process apart from stego that holds the lock, as a running apply does.
		helper := exec.Command("flock", "--exclusive", "--no-fork", filepath.Join(project, ".stego", "apply.lock"), "sleep", "60")
		if err := helper.Start(); err != nil {
			t.Fatal(err)
		}
		waitForLock(t, filepath.Join(project, ".stego", "apply.lock"))
		run := stegoIn(t, project, nil, "init", "--archetype", "rest-crud")
		_ = helper.Process.Kill()
		_ = helper.Wait()
		if run.code == 0 || !strings.Contains(run.stderr, "cannot lock project; another STEGO process may be active") {
			t.Fatalf("want the held-lock error of generation, got exit %d: %s", run.code, run.stderr)
		}
		if !reflect.DeepEqual(before, treeOf(t, project)) {
			t.Fatal("stego init wrote while the lock was held")
		}
		if run := stegoIn(t, project, nil, "init", "--archetype", "rest-crud"); run.code != 0 {
			t.Fatalf("stego init after the lock is released: %s", run.stderr)
		}
	})

	t.Run("ST-DECL-UC01-R06-T03 Init keeps an existing configuration byte for byte", func(t *testing.T) {
		registryDir, commit := declaringRegistry(t)
		config := "# The team's registry.\n\nregistry:   \n  - ref: " + commit + "   \n    url: " + registryDir + "\n\n# end\n"
		project := serviceFolder(t)
		putFile(t, project, ".stego/config.yaml", config)
		if run := stegoIn(t, project, nil, "init", "--archetype", "rest-crud"); run.code != 0 {
			t.Fatalf("stego init: %s", run.stderr)
		}
		data, err := os.ReadFile(filepath.Join(project, ".stego", "config.yaml"))
		if err != nil || string(data) != config {
			t.Fatalf("the configuration changed: %q, %v", data, err)
		}
	})

	t.Run("ST-DECL-UC01-R06-T04 A symbolic link at each path init would write is refused, naming the path", func(t *testing.T) {
		for _, path := range []string{"service.yaml", ".stego/config.yaml", ".stego", ".stego/apply.lock", ".stego/.gitignore", "fills"} {
			project, outside := serviceFolder(t), t.TempDir()
			if path != ".stego" {
				putFile(t, project, ".stego/config.yaml", declaringConfig(t))
			}
			if err := os.RemoveAll(filepath.Join(project, path)); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(project, path)); err != nil {
				t.Fatal(err)
			}
			before, beforeOutside := treeOf(t, project), treeOf(t, outside)
			run := stegoIn(t, project, nil, "init", "--archetype", "rest-crud")
			if run.code == 0 || !strings.Contains(run.stderr, path) {
				t.Errorf("%s: want a refusal that names the path, got exit %d: %s", path, run.code, run.stderr)
			}
			if !reflect.DeepEqual(before, treeOf(t, project)) || !reflect.DeepEqual(beforeOutside, treeOf(t, outside)) {
				t.Errorf("%s: the refused scaffold changed a file", path)
			}
		}
		// The paths of the example content of rpc-service: a folder and a file each.
		for _, path := range []string{"proto", "proto/service.proto", "fills/get-greeting", "authz/schema.zed"} {
			project, outside := declaringProject(t), t.TempDir()
			if err := os.MkdirAll(filepath.Dir(filepath.Join(project, path)), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(project, path)); err != nil {
				t.Fatal(err)
			}
			before, beforeOutside := treeOf(t, project), treeOf(t, outside)
			run := stegoIn(t, project, nil, "init", "--archetype", "rpc-service")
			if run.code == 0 || !strings.Contains(run.stderr, path) {
				t.Errorf("%s: want a refusal that names the path, got exit %d: %s", path, run.code, run.stderr)
			}
			if !reflect.DeepEqual(before, treeOf(t, project)) || !reflect.DeepEqual(beforeOutside, treeOf(t, outside)) {
				t.Errorf("%s: the refused scaffold changed a file", path)
			}
		}
	})

	t.Run("ST-DECL-UC01-R07-T01 Init keeps the apply lock and the saved plan out of the commit, and an existing ignore file as written", func(t *testing.T) {
		project := serviceFolder(t)
		putFile(t, project, ".stego/config.yaml", declaringConfig(t))
		run := stegoIn(t, project, nil, "init", "--archetype", "rest-crud")
		if run.code != 0 || !strings.Contains(run.stdout, ".stego/.gitignore") {
			t.Fatalf("stego init: exit %d, want 0 and .stego/.gitignore among the created files:\n%s%s", run.code, run.stdout, run.stderr)
		}
		if plan := stegoIn(t, project, nil, "plan"); plan.code != 0 {
			t.Fatalf("stego plan: exit %d:\n%s%s", plan.code, plan.stdout, plan.stderr)
		}
		for _, name := range []string{".stego/apply.lock", ".stego/plan.yaml"} {
			if _, err := os.Stat(filepath.Join(project, filepath.FromSlash(name))); err != nil {
				t.Fatalf("%s is not there after init and plan: %v", name, err)
			}
		}
		// The build takes the files of the commit (git ls-tree), so a file that git add leaves out is no input of it.
		git(t, project, "init", "-q")
		git(t, project, "add", "-A")
		committed := strings.Split(git(t, project, "ls-files"), "\n")
		for _, name := range []string{".stego/apply.lock", ".stego/plan.yaml"} {
			if slices.Contains(committed, name) {
				t.Errorf("git add -A takes the local state file %s: %v", name, committed)
			}
		}
		for _, name := range []string{".stego/.gitignore", ".stego/config.yaml", "service.yaml"} {
			if !slices.Contains(committed, name) {
				t.Errorf("git add -A leaves out %s: %v", name, committed)
			}
		}
		// An ignore file that the author already has is kept as written.
		other := serviceFolder(t)
		putFile(t, other, ".stego/config.yaml", declaringConfig(t))
		putFile(t, other, ".stego/.gitignore", "# the team's own\n*.tmp\n")
		if run := stegoIn(t, other, nil, "init", "--archetype", "rest-crud"); run.code != 0 || strings.Contains(run.stdout, ".stego/.gitignore") {
			t.Fatalf("stego init with an ignore file: exit %d, want 0 and no new ignore file:\n%s%s", run.code, run.stdout, run.stderr)
		}
		if data, err := os.ReadFile(filepath.Join(other, ".stego", ".gitignore")); err != nil || string(data) != "# the team's own\n*.tmp\n" {
			t.Errorf("the existing ignore file changed: %q, %v", data, err)
		}
	})

	t.Run("ST-DECL-UC01-R06-T05 With no configuration, init writes one that names the default local registry folder", func(t *testing.T) {
		registryDir, _ := declaringRegistry(t)
		home := t.TempDir()
		configHome := filepath.Join(home, ".config")
		defaultFolder := filepath.Join(configHome, "stego", "registry")
		if err := os.MkdirAll(filepath.Dir(defaultFolder), 0755); err != nil {
			t.Fatal(err)
		}
		if output, err := exec.Command("cp", "-R", registryDir, defaultFolder).CombinedOutput(); err != nil {
			t.Fatalf("copy the registry: %v\n%s", err, output)
		}
		environment := []string{"HOME=" + home, "XDG_CONFIG_HOME=" + configHome}
		project := serviceFolder(t)
		if run := stegoIn(t, project, environment, "init", "--archetype", "rest-crud"); run.code != 0 {
			t.Fatalf("stego init: %s", run.stderr)
		}
		config := readConfig(t, project)
		if len(config.Registry) != 1 || config.Registry[0].URL != defaultFolder || config.Registry[0].Ref != "local" {
			t.Fatalf("the configuration names %+v, want the local source %s", config.Registry, defaultFolder)
		}
		if run := stegoIn(t, project, environment, "validate"); run.code != 0 {
			t.Fatalf("stego validate does not resolve the registry: %s%s", run.stdout, run.stderr)
		}
	})

	t.Run("ST-DECL-UC01-R06-T06 With no configuration and STEGO_REGISTRY set, init writes a configuration that names the override", func(t *testing.T) {
		registryDir, _ := declaringRegistry(t)
		project := serviceFolder(t)
		if run := stegoIn(t, project, []string{"STEGO_REGISTRY=" + registryDir}, "init", "--archetype", "rest-crud"); run.code != 0 {
			t.Fatalf("stego init: %s", run.stderr)
		}
		config := readConfig(t, project)
		if len(config.Registry) != 1 || config.Registry[0].URL != registryDir || config.Registry[0].Ref != "local" {
			t.Fatalf("the configuration names %+v, want the local source %s", config.Registry, registryDir)
		}
	})
}

// waitForLock waits until another process holds the lock on the file.
func waitForLock(t *testing.T, name string) {
	t.Helper()
	for range 500 {
		file, err := os.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		}
		file.Close()
		if err != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the helper process never held the lock")
}

func sortStrings(values []string) []string {
	sorted := append([]string(nil), values...)
	sort.Strings(sorted)
	return sorted
}

// registryWith copies the shipped registry into a new folder with one file written over it, and gives the folder.
func registryWith(t *testing.T, name, data string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "registry")
	if err := shippedRegistryInto(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// keysOf gives the keys of a map in order.
func keysOf(values map[string]any) []string {
	return slices.Sorted(maps.Keys(values))
}

// exampleArchetypes are the archetypes of the fixture registry whose scaffold holds example content, with the files
// of that content beside the declaration.
var exampleArchetypes = map[string][]string{
	"rpc-service":     {"authz/schema.zed", "fills/get-greeting/fill.go", "fills/get-greeting/fill.yaml", "fills/get-greeting/fill_test.go", "proto/service.proto"},
	"browser-service": {"ui/index.html"},
}

// validationFault is one line of the faults that stego validate prints: the rule and the path.
var validationFault = regexp.MustCompile(`(?m)^\s+\[([a-z-]+)\] ([^:\s]+):`)

// faultPaths gives the paths of the faults of one rule that stego validate printed, in order.
func faultPaths(output, rule string) []string {
	var paths []string
	for _, match := range validationFault.FindAllStringSubmatch(output, -1) {
		if match[1] == rule {
			paths = append(paths, match[2])
		}
	}
	return paths
}

// owedSettings gives the settings that stego init printed for an empty scaffold.
func owedSettings(output string) []string {
	_, list, found := strings.Cut(output, "Give these settings in service.yaml before stego validate accepts it:\n")
	if !found {
		return nil
	}
	var settings []string
	for _, line := range strings.Split(list, "\n") {
		if !strings.HasPrefix(line, "  ") {
			break
		}
		settings = append(settings, strings.TrimSpace(line))
	}
	return settings
}

// namedProject makes a project folder with the configuration of ST-DECL-TD01, named as a service may be named: the
// service takes the name of its folder, and a name such as 001 gives no SQL role or service identity.
func namedProject(t *testing.T) string {
	t.Helper()
	project := filepath.Join(t.TempDir(), "greeter")
	putFile(t, project, ".stego/config.yaml", declaringConfig(t))
	return project
}

// moduleOf is the module path that a test project of the archetype takes, through STEGO_MODULE.
func moduleOf(archetype string) []string {
	return []string{"STEGO_MODULE=example.com/" + archetype, "GOWORK=off"}
}

// initialized makes a project folder with namedProject and runs stego init in it.
func initialized(t *testing.T, archetype string, args ...string) (string, stegoRun) {
	t.Helper()
	project := namedProject(t)
	run := stegoIn(t, project, moduleOf(archetype), append([]string{"init", "--archetype", archetype}, args...)...)
	if run.code != 0 {
		t.Fatalf("stego init --archetype %s %v: exit %d: %s", archetype, args, run.code, run.stderr)
	}
	return project, run
}

func TestArchetypeScaffolds(t *testing.T) {
	t.Parallel()
	t.Run("ST-DECL-UC01-R01-T01 The scaffold of each archetype passes checking with no change", func(t *testing.T) {
		for _, archetype := range declaredArchetypes(t) {
			project, _ := initialized(t, archetype)
			before := treeOf(t, project)
			if signExampleFill(t, project, archetype, moduleOf(archetype), "validate") {
				// The signature changes only the manifest of the example fill.
				before = treeOf(t, project)
			}
			if run := stegoIn(t, project, moduleOf(archetype), "validate"); run.code != 0 {
				t.Errorf("%s: stego validate refused the scaffold: %s%s", archetype, run.stdout, run.stderr)
			}
			if after := treeOf(t, project); !reflect.DeepEqual(before, after) {
				t.Errorf("%s: stego validate changed the scaffold", archetype)
			}
		}
	})

	t.Run("ST-DECL-UC01-R01-T02 The scaffold of each archetype applies and builds as written", func(t *testing.T) {
		for _, archetype := range declaredArchetypes(t) {
			project, _ := initialized(t, archetype)
			environment := moduleOf(archetype)
			signExampleFill(t, project, archetype, environment, "plan", "apply", "validate")
			for _, command := range [][]string{{"validate"}, {"apply"}, {"deps"}, {"apply"}, {"drift"}} {
				if run := stegoIn(t, project, environment, command...); run.code != 0 {
					t.Fatalf("%s: stego %v: exit %d: %s%s", archetype, command, run.code, run.stdout, run.stderr)
				}
			}
			build := exec.Command("go", "build", "-mod=readonly", "-o", filepath.Join(t.TempDir(), "service"), "./out")
			build.Dir = project
			build.Env = append(os.Environ(), "GOWORK=off")
			if output, err := build.CombinedOutput(); err != nil {
				t.Errorf("%s: go build ./out: %v\n%s", archetype, err, output)
			}
		}
	})

	t.Run("ST-DECL-UC01-R06-T07 An example file outside the declaration and the example folders, or in a hidden folder, is refused, naming the archetype and the path", func(t *testing.T) {
		for _, target := range []string{
			".github/workflows/evil.yml", ".stego/state.yaml", "fills/.hidden/fill.go", "proto/.env",
			"out/main.go", "go.mod", "go.sum", "Containerfile", "deploy/base/deployment.yaml", "fills", "README.md",
		} {
			registry := registryWith(t, "archetypes/rpc-service/example/"+target+".tmpl", "x\n")
			project := namedProject(t)
			before := treeOf(t, project)
			run := stegoIn(t, project, append(moduleOf("rpc-service"), "STEGO_REGISTRY="+registry), "init", "--archetype", "rpc-service")
			if run.code == 0 || !strings.Contains(run.stderr, "archetype rpc-service") || !strings.Contains(run.stderr, target) {
				t.Errorf("%s: want a refusal that names the archetype and the path, got exit %d: %s", target, run.code, run.stderr)
			}
			if !reflect.DeepEqual(before, treeOf(t, project)) {
				t.Errorf("%s: the refused scaffold wrote a file", target)
			}
		}
		registry := registryWith(t, "archetypes/rpc-service/example/ui/app.js.tmpl", "x\n")
		project := namedProject(t)
		if run := stegoIn(t, project, append(moduleOf("rpc-service"), "STEGO_REGISTRY="+registry), "init", "--archetype", "rpc-service"); run.code != 0 {
			t.Errorf("an example file under ui/ is refused: %s", run.stderr)
		} else if _, err := os.Stat(filepath.Join(project, "ui", "app.js")); err != nil {
			t.Error(err)
		}
	})

	t.Run("ST-SUP-UC01-R07-T03 A component that marks a setting author supplied and also secret, bypass, optional or with a default is refused, naming the component and the setting", func(t *testing.T) {
		const component = "components/grpc-application/component.yaml"
		data, err := os.ReadFile(filepath.Join("..", "..", "registry", filepath.FromSlash(component)))
		if err != nil {
			t.Fatal(err)
		}
		mark := "    author_supplied: true\n"
		if !strings.Contains(string(data), mark) {
			t.Fatal("grpc-application marks no setting author supplied")
		}
		changes := map[string]string{}
		for conflict, extra := range map[string]string{"secret": "    secret: true\n", "bypass": "    bypass: true\n", "optional": "    optional: true\n", "default": "    default: []\n"} {
			changes[conflict] = strings.Replace(string(data), mark, mark+extra, 1)
		}
		reference := "      reference:\n        type: string\n        optional: true\n"
		if !strings.Contains(string(data), reference) {
			t.Fatal("the proto_files items have no optional reference")
		}
		changes["proto_files.reference"] = strings.Replace(string(data), reference, reference+"        author_supplied: true\n", 1)
		for want, changed := range changes {
			registry := registryWith(t, component, changed)
			run := stegoIn(t, namedProject(t), append(moduleOf("rpc-service"), "STEGO_REGISTRY="+registry), "init", "--archetype", "rpc-service")
			if run.code == 0 || !strings.Contains(run.stderr, "grpc-application") || !strings.Contains(run.stderr, "proto_files") || !strings.Contains(run.stderr, want) {
				t.Errorf("%s: want a refusal that names the component, the setting and %s, got exit %d: %s", want, want, run.code, run.stderr)
			}
		}
	})

	t.Run("ST-DECL-UC01-R01-T03 Init refuses a project folder whose name is not a service name, and writes nothing", func(t *testing.T) {
		for _, name := range []string{"Greeter", "greeter_service", "-greeter", "001", "<img src=x onerror=alert(1)>"} {
			project := filepath.Join(t.TempDir(), name)
			putFile(t, project, ".stego/config.yaml", declaringConfig(t))
			before := treeOf(t, project)
			run := stegoIn(t, project, moduleOf("browser-service"), "init", "--archetype", "browser-service")
			if run.code == 0 || !strings.Contains(run.stderr, "is not a service name") {
				t.Errorf("%q: want a refusal that the folder name is not a service name, got exit %d: %s", name, run.code, run.stderr)
			}
			if !reflect.DeepEqual(before, treeOf(t, project)) {
				t.Errorf("%q: the refused scaffold wrote a file", name)
			}
		}
	})

	t.Run("ST-DECL-UC01-R04-T01 Removing any one part of a scaffold's example content makes checking refuse it", func(t *testing.T) {
		// The parts of a proto file: each service and message, and each method of a service.
		protoParts := []*regexp.Regexp{regexp.MustCompile(`(?ms)^(?:service|message) \w+ \{.*?^\}\n`), regexp.MustCompile(`(?m)^  rpc [^\n]*\n`)}
		for archetype, files := range exampleArchetypes {
			scaffold, _ := initialized(t, archetype)
			type cut struct{ name, file, text string }
			var cuts []cut
			for _, file := range files {
				cuts = append(cuts, cut{"the file " + file, file, ""})
				if filepath.Ext(file) != ".proto" {
					continue
				}
				data, err := os.ReadFile(filepath.Join(scaffold, file))
				if err != nil {
					t.Fatal(err)
				}
				for _, parts := range protoParts {
					for _, at := range parts.FindAllStringIndex(string(data), -1) {
						part := string(data[at[0]:at[1]])
						cuts = append(cuts, cut{"the part " + strings.SplitN(strings.TrimSpace(part), "\n", 2)[0] + " of " + file, file, string(data[:at[0]]) + string(data[at[1]:])})
					}
				}
			}
			if archetype == "rpc-service" && len(cuts) != len(files)+4 {
				t.Fatalf("rpc-service: want each file, and the service, the two messages and the method of the proto file, got %d parts", len(cuts))
			}
			// The parts of the declaration text: each key, and each setting of a component.
			text, err := os.ReadFile(filepath.Join(scaffold, "service.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			var declared map[string]any
			if err := yaml.Unmarshal(text, &declared); err != nil {
				t.Fatal(err)
			}
			without := func(remove func(map[string]any)) string {
				var copied map[string]any
				if err := yaml.Unmarshal(text, &copied); err != nil {
					t.Fatal(err)
				}
				remove(copied)
				data, err := yaml.Marshal(copied)
				if err != nil {
					t.Fatal(err)
				}
				return string(data)
			}
			for _, key := range keysOf(declared) {
				switch key {
				case "kind", "name", "archetype", "language":
					continue
				case "overrides":
					for _, component := range keysOf(declared[key].(map[string]any)) {
						for _, setting := range keysOf(declared[key].(map[string]any)[component].(map[string]any)) {
							cuts = append(cuts, cut{"the setting " + component + "." + setting, "service.yaml", without(func(d map[string]any) {
								delete(d["overrides"].(map[string]any)[component].(map[string]any), setting)
							})})
						}
					}
					continue
				}
				cuts = append(cuts, cut{"the key " + key, "service.yaml", without(func(d map[string]any) { delete(d, key) })})
			}
			for _, part := range cuts {
				project := filepath.Join(t.TempDir(), filepath.Base(scaffold))
				if err := os.CopyFS(project, os.DirFS(scaffold)); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(project, part.file)
				var err error
				if part.text == "" {
					err = os.Remove(target)
				} else {
					err = os.WriteFile(target, []byte(part.text), 0644)
				}
				if err != nil {
					t.Fatal(err)
				}
				// A fill with no Go code yet passes stego validate, because apply writes the contract it is written
				// against; apply then checks the fill rules again with go.mod and refuses it.
				if run := stegoIn(t, project, moduleOf(archetype), "validate"); run.code != 0 {
					continue
				}
				if run := stegoIn(t, project, moduleOf(archetype), "apply"); run.code == 0 {
					t.Errorf("%s: stego validate and stego apply accepted the scaffold without %s", archetype, part.name)
				}
			}
		}
	})

	t.Run("ST-DECL-UC01-R05-T01 An empty scaffold leaves the example content out", func(t *testing.T) {
		project, _ := initialized(t, "rpc-service", "--empty")
		var paths []string
		for path := range treeOf(t, project) {
			paths = append(paths, path)
		}
		want := []string{"./", ".stego/", ".stego/.gitignore", ".stego/apply.lock", ".stego/config.yaml", "fills/", "service.yaml"}
		if strings.Join(sortStrings(paths), " ") != strings.Join(want, " ") {
			t.Errorf("the empty scaffold holds %v, want %v", sortStrings(paths), want)
		}
		data, err := os.ReadFile(filepath.Join(project, "service.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		var service map[string]any
		if err := yaml.Unmarshal(data, &service); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"entities", "collections", "queries", "commands", "overrides"} {
			if value, ok := service[key]; ok && reflect.ValueOf(value).Len() != 0 {
				t.Errorf("the empty scaffold holds %s: %v", key, value)
			}
		}
	})

	t.Run("ST-DECL-UC01-R05-T02 An empty scaffold names, as it is written, each setting that checking then asks for", func(t *testing.T) {
		for archetype := range exampleArchetypes {
			project, run := initialized(t, archetype, "--empty")
			owed := owedSettings(run.stdout)
			if len(owed) == 0 {
				t.Errorf("%s: stego init named no setting: %s", archetype, run.stdout)
			}
			check := stegoIn(t, project, moduleOf(archetype), "validate")
			if check.code == 0 {
				t.Fatalf("%s: stego validate accepted the empty scaffold", archetype)
			}
			if asked := faultPaths(check.stdout+check.stderr, "author-supplied"); !reflect.DeepEqual(asked, owed) {
				t.Errorf("%s: stego init named %v, and stego validate asked for %v", archetype, owed, asked)
			}
			if all := validationFault.FindAllString(check.stdout+check.stderr, -1); len(all) != len(owed) {
				t.Errorf("%s: stego validate gave faults other than the settings stego init named: %s", archetype, check.stdout)
			}
		}
	})

	t.Run("ST-DECL-UC01-FL-T An author scaffolds an rpc-service declaration that checking accepts as written", func(t *testing.T) {
		project := namedProject(t)
		config, err := os.ReadFile(filepath.Join(project, ".stego", "config.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		run := stegoIn(t, project, nil, "init", "--archetype", "rpc-service")
		if run.code != 0 {
			t.Fatalf("stego init: exit %d: %s", run.code, run.stderr)
		}
		name := filepath.Base(project)
		if !strings.Contains(run.stdout, `"`+name+`"`) {
			t.Errorf("stego init did not print the project name %s: %s", name, run.stdout)
		}
		for _, file := range append([]string{"service.yaml", "fills/"}, exampleArchetypes["rpc-service"]...) {
			if !strings.Contains(run.stdout, "  "+file+"\n") {
				t.Errorf("stego init did not print %s: %s", file, run.stdout)
			}
			if _, err := os.Stat(filepath.Join(project, file)); err != nil {
				t.Error(err)
			}
		}
		if after, err := os.ReadFile(filepath.Join(project, ".stego", "config.yaml")); err != nil || string(after) != string(config) {
			t.Errorf("stego init changed the configuration: %v", err)
		}
		declaration, err := os.ReadFile(filepath.Join(project, "service.yaml"))
		if err != nil || !strings.Contains(string(declaration), "name: "+name+"\n") || strings.Contains(string(declaration), "{{") {
			t.Errorf("the declaration does not hold the service name in place of its placeholders: %v\n%s", err, declaration)
		}
		signExampleFill(t, project, "rpc-service", nil, "validate")
		if check := stegoIn(t, project, nil, "validate"); check.code != 0 {
			t.Errorf("stego validate refused the scaffold: %s%s", check.stdout, check.stderr)
		}
	})
}

// signExampleFill qualifies the example fill get-greeting of a scaffold that holds it, as the service author does
// (ST-GEN-INV10, decision on ST-EXT-Q30). First the check runs on the unsigned fill: stego validate accepts the
// scaffold with one warning that names the fill and what to run, and each other command refuses it with one fault
// that names the fill and what to run (ST-DECL-UC01-R01). Then stego test runs, and the fill is signed with listed-ssh
// of ST-EXT-TD02, a key that the test run makes, never a real one. It tells whether the scaffold holds the fill.
func signExampleFill(t *testing.T, project, archetype string, environment []string, checks ...string) bool {
	t.Helper()
	if _, err := os.Stat(filepath.Join(project, "fills", "get-greeting")); err != nil {
		return false
	}
	for _, check := range checks {
		run := stegoIn(t, project, environment, check)
		output := run.stdout + run.stderr
		names := strings.Contains(output, `fill "get-greeting" is untested`) && strings.Contains(output, "run stego test, then stego fill sign get-greeting")
		if check == "validate" {
			if run.code != 0 || !names || !strings.Contains(output, "1 warning(s)") || strings.Contains(output, "error(s)") {
				t.Fatalf("%s: stego validate of the unsigned example fill: exit %d, want exit 0 and one warning that names get-greeting and what to run: %s", archetype, run.code, output)
			}
			continue
		}
		if run.code == 0 || !names || strings.Contains(output, "2 error(s)") {
			t.Fatalf("%s: stego %s of the unsigned example fill: exit %d, want one fault that names get-greeting and what to run: %s", archetype, check, run.code, output)
		}
	}
	keys := testFillKeys(t)
	putFile(t, project, "SIGNERS", keys.signers)
	signing := append(append(slices.Clone(environment), keys.goEnv...), "GOFLAGS=-modcacherw",
		"HOME="+keys.homes["listed-ssh"], "XDG_CONFIG_HOME="+filepath.Join(keys.homes["listed-ssh"], ".config"))
	for _, command := range [][]string{{"test"}, {"fill", "sign", "get-greeting"}} {
		if run := stegoIn(t, project, signing, command...); run.code != 0 {
			t.Fatalf("%s: stego %v: exit %d: %s%s", archetype, command, run.code, run.stdout, run.stderr)
		}
	}
	return true
}
