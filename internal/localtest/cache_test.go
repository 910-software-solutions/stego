package localtest

import (
	"archive/zip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The proofs that the module cache of localtest takes only checked modules (ST-STD-LOCAL-R03): an inner go command of
// GoEnv refuses a module that only the shared GOMODCACHE holds, and CheckSums refuses a module that only the cache of
// localtest holds with no checked go.sum line. This is the fix of finding M1 of the security review of PR #510.

// plantedModule writes the module example.com/lib v1.0.0, whose Planted function stands for code that no sum database
// checked, into the download folder of the module cache cache: its info, its go.mod, its zip and its list.
func plantedModule(t *testing.T, cache string) {
	t.Helper()
	folder := filepath.Join(cache, "cache", "download", "example.com", "lib", "@v")
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"list":        "v1.0.0\n",
		"v1.0.0.info": `{"Version":"v1.0.0","Time":"2026-01-01T00:00:00Z"}` + "\n",
		"v1.0.0.mod":  "module example.com/lib\n\ngo 1.26\n",
	} {
		if err := os.WriteFile(filepath.Join(folder, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	file, err := os.Create(filepath.Join(folder, "v1.0.0.zip"))
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	for name, data := range map[string]string{
		"example.com/lib@v1.0.0/go.mod": "module example.com/lib\n\ngo 1.26\n",
		"example.com/lib@v1.0.0/lib.go": "package lib\n\n// Planted stands for code that no sum database checked.\nfunc Planted() string { return \"planted\" }\n",
	} {
		writer, err := archive.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

// generatedModule writes a module that imports the planted module, with no go.sum line for it.
func generatedModule(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, data := range map[string]string{
		"go.mod":  "module example.com/gen\n\ngo 1.26\n\nrequire example.com/lib v1.0.0\n",
		"main.go": "package main\n\nimport (\n\t\"fmt\"\n\n\t\"example.com/lib\"\n)\n\nfunc main() { fmt.Println(lib.Planted()) }\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func goTool() string {
	if name := os.Getenv("STEGO_TEST_PINNED_GO"); name != "" {
		return name
	}
	return "go"
}

func TestModuleCacheTakesOnlyCheckedModules(t *testing.T) {
	t.Run("ST-STD-LOCAL-R03-T08 An inner go command does not take a module that only the shared module cache holds", func(t *testing.T) {
		RequireModules(t)
		// The shared cache of this test: another tool filled it with a module that no sum database checked, as the
		// security review planted one (probe1).
		shared := t.TempDir()
		plantedModule(t, shared)
		dir := generatedModule(t)
		// The environment of the test names the shared cache; GoEnv replaces it with the cache of localtest.
		t.Setenv("GOMODCACHE", shared)
		tidy := exec.Command(goTool(), "mod", "tidy")
		tidy.Dir = dir
		tidy.Env = GoEnv()
		for _, entry := range tidy.Env {
			if entry == "GOMODCACHE="+shared {
				t.Fatal("GoEnv kept the shared module cache")
			}
		}
		output, err := tidy.CombinedOutput()
		if err == nil {
			t.Fatalf("an inner go mod tidy took example.com/lib from the shared module cache:\n%s", output)
		}
		if data, _ := os.ReadFile(filepath.Join(dir, "go.sum")); strings.Contains(string(data), "example.com/lib") {
			t.Fatalf("the inner go mod tidy wrote the planted module into go.sum:\n%s", data)
		}
	})
	t.Run("ST-STD-LOCAL-R03-T09 A module that only the cache of localtest holds, with no checked go.sum line, fails the check of go.sum", func(t *testing.T) {
		RequireModules(t)
		// The planted module is put into the cache of localtest itself, past the fill and its checks.
		plantedModule(t, moduleCache)
		t.Cleanup(func() {
			_ = os.RemoveAll(filepath.Join(moduleCache, "cache", "download", "example.com", "lib"))
			_ = filepath.WalkDir(filepath.Join(moduleCache, "example.com"), func(name string, entry os.DirEntry, err error) error {
				if err == nil && entry.IsDir() {
					_ = os.Chmod(name, 0o755)
				}
				return nil
			})
			_ = os.RemoveAll(filepath.Join(moduleCache, "example.com", "lib@v1.0.0"))
		})
		dir := generatedModule(t)
		tidy := exec.Command(goTool(), "mod", "tidy")
		tidy.Dir = dir
		tidy.Env = GoEnv()
		if output, err := tidy.CombinedOutput(); err != nil {
			t.Fatalf("the inner go mod tidy failed before the check of go.sum could show anything: %v\n%s", err, output)
		}
		err := SumsChecked(dir)
		if err == nil || !strings.Contains(err.Error(), "example.com/lib v1.0.0") {
			t.Fatalf("the check of go.sum gave %v, want a refusal that names example.com/lib v1.0.0", err)
		}
	})
	t.Run("ST-STD-LOCAL-R03-T10 The module cache of localtest is neither the shared GOMODCACHE nor in the temporary folder", func(t *testing.T) {
		RequireModules(t)
		shared, err := exec.Command(goTool(), "env", "GOMODCACHE").Output()
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(moduleCache, strings.TrimSpace(string(shared))) {
			t.Fatalf("the module cache of localtest %s is in the shared GOMODCACHE %s", moduleCache, shared)
		}
		if tmp := os.TempDir(); strings.HasPrefix(moduleCache, tmp) {
			t.Fatalf("the module cache of localtest %s is in the temporary folder %s", moduleCache, tmp)
		}
		for _, want := range []string{"GOMODCACHE=" + moduleCache, "GOSUMDB=off", "GONOPROXY=none", "GONOSUMDB=none", "GOINSECURE=", "GOVCS=*:off", "GOAUTH=off", "GOENV=off"} {
			found := false
			for _, entry := range GoEnv() {
				found = found || entry == want
			}
			if !found {
				t.Errorf("GoEnv does not set %s", want)
			}
		}
	})
}

// TestFailClosedProbe is the child of ST-STD-LOCAL-R03-T11: with LOCALTEST_PROBE set, it runs go mod tidy with GoEnv
// in the module that LOCALTEST_PROBE names and prints the outcome; without it, it does nothing.
func TestFailClosedProbe(t *testing.T) {
	dir := os.Getenv("LOCALTEST_PROBE")
	if dir == "" {
		return
	}
	env := GoEnv()
	for _, entry := range env {
		if strings.HasPrefix(entry, "GOMODCACHE=") || strings.HasPrefix(entry, "GOPROXY=") {
			t.Log("probe env " + entry)
		}
	}
	tidy := exec.Command(goTool(), "mod", "tidy")
	tidy.Dir, tidy.Env = dir, env
	output, err := tidy.CombinedOutput()
	t.Logf("probe tidy err=%v\n%s", err, output)
}

func TestModuleCacheFailsClosed(t *testing.T) {
	t.Run("ST-STD-LOCAL-R03-T11 When the module cache of localtest cannot be made, GoEnv gives no shared cache and a tidy of a planted module fails", func(t *testing.T) {
		shared, err := exec.Command(goTool(), "env", "GOMODCACHE").Output()
		if err != nil {
			t.Fatal(err)
		}
		// The shared cache of the child: GOPATH/pkg/mod, which an empty GOMODCACHE names, holds a planted module. The
		// child takes it as its shared cache, so STEGO_TEST_RUNS inside it names the shared cache.
		gopath := t.TempDir()
		childShared := filepath.Join(gopath, "pkg", "mod")
		plantedModule(t, childShared)
		_ = shared
		for name, runs := range map[string]string{
			"STEGO_TEST_RUNS in the shared module cache": childShared,
			"STEGO_TEST_RUNS in the temporary folder":    t.TempDir(),
		} {
			t.Run(name, func(t *testing.T) {
				dir := generatedModule(t)
				self, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				child := exec.Command(self, "-test.run", "^TestFailClosedProbe$", "-test.v")
				child.Env = append(os.Environ(), "LOCALTEST_PROBE="+dir, "STEGO_TEST_RUNS="+runs, "GOPATH="+gopath, "GOMODCACHE=")
				output, _ := child.CombinedOutput()
				text := string(output)
				if !strings.Contains(text, "probe tidy err=") {
					t.Fatalf("the probe did not run:\n%s", text)
				}
				if strings.Contains(text, "probe tidy err=<nil>") {
					t.Fatalf("a tidy of the planted module succeeded with the module cache of localtest refused:\n%s", text)
				}
				for _, bad := range []string{"probe env GOMODCACHE=\n", "probe env GOMODCACHE=" + childShared} {
					if strings.Contains(text, bad) {
						t.Fatalf("GoEnv gave the shared module cache (%q):\n%s", strings.TrimSpace(bad), text)
					}
				}
				if !strings.Contains(text, "probe env GOPROXY=off") {
					t.Fatalf("GoEnv gave a module proxy with the module cache of localtest refused:\n%s", text)
				}
				if data, _ := os.ReadFile(filepath.Join(dir, "go.sum")); strings.Contains(string(data), "example.com/lib") {
					t.Fatalf("the tidy wrote the planted module into go.sum:\n%s", data)
				}
			})
		}
	})
}
