package localtest

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestModuleFoldersAreHashedAgain(t *testing.T) {
	t.Run("ST-STD-LOCAL-R03-T13 A file put into the folder of a checked module fails the fill of localtest", func(t *testing.T) {
		// The fill of this test binary hashed the folders of the module cache of localtest. No other code of the binary
		// calls verifyDirs before this line.
		RequireModules(t)
		if verifiedDirs == 0 {
			t.Fatal("the fill of the module cache of localtest checked no module folder")
		}
		// A stand-in cache: the go command extracts example.com/lib v1.0.0 from a file proxy into a module cache of the
		// test, as the fill does from the module proxy.
		proxy, cache := t.TempDir(), t.TempDir()
		plantedModule(t, proxy)
		t.Cleanup(func() { _ = exec.Command("chmod", "-R", "u+w", cache).Run() })
		download := exec.Command(goTool(), "mod", "download", "-json", "example.com/lib@v1.0.0")
		download.Dir = generatedModule(t)
		download.Env = append(os.Environ(), "GOMODCACHE="+cache, "GOPROXY=file://"+filepath.ToSlash(filepath.Join(proxy, "cache", "download")),
			"GOSUMDB=off", "GONOSUMDB=", "GOPRIVATE=", "GOFLAGS=-mod=mod", "GOENV=off", "GOWORK=off", "GOTOOLCHAIN=local")
		out, err := download.Output()
		if err != nil {
			t.Fatalf("go mod download: %v\n%s", err, out)
		}
		var module fillDir
		if err := json.Unmarshal(out, &module); err != nil || module.Dir == "" || module.Sum == "" {
			t.Fatalf("go mod download gave no folder and hash: %v\n%s", err, out)
		}
		stamps := filepath.Join(t.TempDir(), verifiedFile)
		if err := verifyDirs([]fillDir{module}, stamps); err != nil || hashedDirs != 1 {
			t.Fatalf("the unchanged folder fails or is not hashed (%d): %v", hashedDirs, err)
		}
		if err := verifyDirs([]fillDir{module}, stamps); err != nil || hashedDirs != 0 {
			t.Fatalf("the unchanged folder with its stamp fails or is hashed again (%d): %v", hashedDirs, err)
		}
		// A file rewritten with the same size and a new modification time changes the stamp, so the folder is hashed
		// again and refused (security review of PR #7, I1). The folder and the file get their modes back, so only the
		// modification time tells the change.
		folder, err := os.Stat(module.Dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(module.Dir, 0o755); err != nil {
			t.Fatal(err)
		}
		source := filepath.Join(module.Dir, "lib.go")
		original, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		before, err := os.Stat(source)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(source, 0o644); err != nil {
			t.Fatal(err)
		}
		changed := []byte(strings.Replace(string(original), "package lib", "package lix", 1))
		if len(changed) != len(original) || string(changed) == string(original) {
			t.Fatal("the rewrite of lib.go does not keep its size")
		}
		if err := os.WriteFile(source, changed, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(source, before.ModTime().Add(time.Hour), before.ModTime().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(source, before.Mode().Perm()); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(module.Dir, folder.Mode().Perm()); err != nil {
			t.Fatal(err)
		}
		if err := verifyDirs([]fillDir{module}, stamps); err == nil || !strings.Contains(err.Error(), "example.com/lib v1.0.0: dir has been modified") {
			t.Fatalf("a same-size rewrite with a new modification time is not refused: %v", err)
		}
		if err := os.Chmod(module.Dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(source, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(source, original, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(source, before.Mode().Perm()); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(source, before.ModTime(), before.ModTime()); err != nil {
			t.Fatal(err)
		}
		if err := verifyDirs([]fillDir{module}, stamps); err != nil {
			t.Fatalf("the folder put back as it was is refused: %v", err)
		}
		// lib.go put back as a link to a copy of the same bytes outside the folder: the hash, which follows the link, is
		// the checked one, but a later change of the copy would not change the stamp. The go command extracts no link,
		// so the folder is refused (security review of PR #7, I2).
		copied := filepath.Join(t.TempDir(), "lib.go")
		if err := os.WriteFile(copied, original, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(source, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(source); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(copied, source); err != nil {
			t.Fatal(err)
		}
		if err := verifyDirs([]fillDir{module}, stamps); err == nil || !strings.Contains(err.Error(), "lib.go, which is not a regular file or a folder") {
			t.Fatalf("a link in the folder is not refused: %v", err)
		}
		if err := os.Remove(source); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(source, original, before.Mode().Perm()); err != nil {
			t.Fatal(err)
		}
		// Another tool, or a run with -modcacherw, puts a file into the folder.
		if err := os.Chmod(module.Dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(module.Dir, "zz_planted.go"), []byte("package lib\n\nfunc init() { println(\"ALTERED\") }\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		err = verifyDirs([]fillDir{module}, stamps)
		if err == nil || !strings.Contains(err.Error(), "example.com/lib v1.0.0: dir has been modified") {
			t.Fatalf("the changed folder is not refused: %v", err)
		}
	})
}

func TestPinnedGoRoot(t *testing.T) {
	t.Run("ST-STD-LOCAL-R03-T20 A GOROOT whose go.env turns a check off reaches neither the fill nor an inner go command of localtest", func(t *testing.T) {
		// PR #8 builder (decided 2026-11-27): the go command reads GOROOT/go.env also with GOENV=off, and takes from it
		// each setting that the environment leaves empty, such as GONOSUMDB= of the fill.
		RequireModules(t)
		if goRoot == "" {
			t.Fatal("the fill of localtest pinned no GOROOT")
		}
		planted := t.TempDir()
		entries, err := os.ReadDir(goRoot)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.Name() != "go.env" {
				if err := os.Symlink(filepath.Join(goRoot, entry.Name()), filepath.Join(planted, entry.Name())); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := os.WriteFile(filepath.Join(planted, "go.env"), []byte("GONOSUMDB=*\nGOPRIVATE=*\nGOINSECURE=*\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GOROOT", planted)
		for name, c := range map[string]struct {
			env  []string
			want map[string]string
		}{
			"the fill":            {fillEnvironment(), fillSettings},
			"an inner go command": {GoEnv(), innerGoSettings},
		} {
			for _, entry := range c.env {
				if entry == "GOROOT="+planted {
					t.Errorf("%s keeps the GOROOT of the environment", name)
				}
			}
			if err := checkEffective(goTool(), c.env, c.want); err != nil {
				t.Errorf("%s with the planted GOROOT in the environment: %v", name, err)
			}
			// The same environment with the planted GOROOT last is refused, naming the setting or the GOROOT.
			if err := checkEffective(goTool(), append(append([]string(nil), c.env...), "GOROOT="+planted), c.want); err == nil || !strings.Contains(err.Error(), "GOROOT") && !strings.Contains(err.Error(), "=\"*\"") {
				t.Errorf("%s with the planted GOROOT last gave %v, want a refusal", name, err)
			}
		}
		// The settings alone: with no pinned GOROOT, the go.env of the planted GOROOT sets each setting that the
		// environment leaves empty, and the check of the settings in effect refuses it.
		saved := goRoot
		goRoot = ""
		defer func() { goRoot = saved }()
		env := append(GoEnvWith(moduleCache, cacheProxy+",off"), "GONOSUMDB=", "GOROOT="+planted)
		if err := checkEffective(goTool(), env, innerGoSettings); err == nil || !strings.Contains(err.Error(), `="*", not ""`) {
			t.Errorf("an inner go command that takes settings from the planted go.env gave %v, want a refusal that names one", err)
		}
	})
}

func TestUnlistedModuleFoldersAreHashed(t *testing.T) {
	t.Run("ST-STD-LOCAL-R03-T19 A changed folder of a checked module that the fill did not list fails the fill of localtest", func(t *testing.T) {
		// A stand-in cache: the go command extracts example.com/lib v1.0.0 from a file proxy into a module cache of the
		// test, as an earlier fill or a go command of a test did. The fill of this run does not list it.
		proxy, cache := t.TempDir(), t.TempDir()
		plantedModule(t, proxy)
		t.Cleanup(func() { _ = exec.Command("chmod", "-R", "u+w", cache).Run() })
		download := exec.Command(goTool(), "mod", "download", "-json", "example.com/lib@v1.0.0")
		download.Dir = generatedModule(t)
		download.Env = append(os.Environ(), "GOMODCACHE="+cache, "GOPROXY=file://"+filepath.ToSlash(filepath.Join(proxy, "cache", "download")),
			"GOSUMDB=off", "GONOSUMDB=", "GOPRIVATE=", "GOFLAGS=-mod=mod", "GOENV=off", "GOWORK=off", "GOTOOLCHAIN=local")
		out, err := download.Output()
		if err != nil {
			t.Fatalf("go mod download: %v\n%s", err, out)
		}
		var module fillDir
		if err := json.Unmarshal(out, &module); err != nil || module.Dir == "" || module.Sum == "" {
			t.Fatalf("go mod download gave no folder and hash: %v\n%s", err, out)
		}
		// A temporary folder of the go command, whose name is not a valid version, is not a module folder.
		if err := os.MkdirAll(filepath.Join(cache, "example.com", "lib@v1.0.0.tmp-1"), 0o755); err != nil {
			t.Fatal(err)
		}
		checked := map[string]bool{"example.com/lib v1.0.0 " + module.Sum: true, "example.com/lib v1.0.0/go.mod h1:none": true}
		found, err := cacheDirs(cache, nil, checked)
		if err != nil || len(found) != 1 || found[0] != module {
			t.Fatalf("the unlisted folder with a checked hash gives %+v, %v; want only %+v", found, err, module)
		}
		if found, err := cacheDirs(cache, []fillDir{module}, checked); err != nil || len(found) != 0 {
			t.Fatalf("a folder that the fill lists is given again: %+v, %v", found, err)
		}
		if found, err := cacheDirs(cache, nil, map[string]bool{}); err != nil || len(found) != 0 {
			t.Fatalf("a folder with no checked hash is given: %+v, %v", found, err)
		}
		if err := verifyCache(cache, nil, checked); err != nil || verifiedDirs != 1 {
			t.Fatalf("the unchanged unlisted folder fails or is not checked (%d): %v", verifiedDirs, err)
		}
		// Another tool puts a file into the folder that this fill did not list.
		if err := os.Chmod(module.Dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(module.Dir, "zz_planted.go"), []byte("package lib\n\nfunc init() { println(\"ALTERED\") }\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		err = verifyCache(cache, nil, checked)
		if err == nil || !strings.Contains(err.Error(), "example.com/lib v1.0.0: dir has been modified") {
			t.Fatalf("the changed unlisted folder is not refused: %v", err)
		}
	})
}

// downloadedLib extracts example.com/lib v1.0.0 from a file proxy into a new module cache of the test, as an earlier
// fill or a go command of a test did, and gives the cache, the module and its checked go.sum line.
func downloadedLib(t *testing.T) (string, fillDir, map[string]bool) {
	t.Helper()
	proxy, cache := t.TempDir(), t.TempDir()
	plantedModule(t, proxy)
	t.Cleanup(func() { _ = exec.Command("chmod", "-R", "u+w", cache).Run() })
	download := exec.Command(goTool(), "mod", "download", "-json", "example.com/lib@v1.0.0")
	download.Dir = generatedModule(t)
	download.Env = append(os.Environ(), "GOMODCACHE="+cache, "GOPROXY=file://"+filepath.ToSlash(filepath.Join(proxy, "cache", "download")),
		"GOSUMDB=off", "GONOSUMDB=", "GOPRIVATE=", "GOFLAGS=-mod=mod", "GOENV=off", "GOWORK=off", "GOTOOLCHAIN=local")
	out, err := download.Output()
	if err != nil {
		t.Fatalf("go mod download: %v\n%s", err, out)
	}
	var module fillDir
	if err := json.Unmarshal(out, &module); err != nil || module.Dir == "" || module.Sum == "" {
		t.Fatalf("go mod download gave no folder and hash: %v\n%s", err, out)
	}
	if err := exec.Command("chmod", "-R", "u+w", cache).Run(); err != nil {
		t.Fatal(err)
	}
	return cache, module, map[string]bool{"example.com/lib v1.0.0 " + module.Sum: true}
}

// changedCopy copies the folder of the module to a new folder outside the cache, named as the module folder, with
// lib.go changed, and gives the folder that holds the copy.
func changedCopy(t *testing.T, module fillDir) string {
	t.Helper()
	elsewhere := t.TempDir()
	copied := filepath.Join(elsewhere, filepath.Base(module.Dir))
	if out, err := exec.Command("cp", "-r", module.Dir, copied).CombinedOutput(); err != nil {
		t.Fatalf("cp: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(copied, "lib.go"), []byte("package lib\n\nfunc Planted() string { return \"ALTERED\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return elsewhere
}

func TestModuleCacheLinks(t *testing.T) {
	t.Run("ST-STD-LOCAL-R03-T25 A symbolic link or a folder name that does not unescape in the module cache fails the fill of localtest", func(t *testing.T) {
		// Security review of PR #14, L1: the walk of cacheDirs did not follow a link, so a module folder or a folder of
		// a path that is a link to a changed copy was never hashed, and the go command built from it.
		for _, form := range []string{"module folder", "path folder"} {
			cache, module, checked := downloadedLib(t)
			elsewhere := changedCopy(t, module)
			link, target := module.Dir, filepath.Join(elsewhere, filepath.Base(module.Dir))
			if form == "path folder" {
				link, target = filepath.Join(cache, "example.com"), elsewhere
			}
			if err := os.RemoveAll(link); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			for _, dirs := range [][]fillDir{nil, {module}} {
				err := verifyCache(cache, dirs, checked)
				if err == nil || !strings.Contains(err.Error(), "holds the symbolic link") {
					t.Errorf("%s, %d listed folders: a link in the module cache is not refused: %v", form, len(dirs), err)
				}
			}
		}
		// The start of a module path, such as gonum.org/v1, is no module path, and an escaped upper case is fine.
		cache, module, checked := downloadedLib(t)
		for _, name := range []string{filepath.Join("gonum.org", "v1"), filepath.Join("github.com", "!burnt!sushi")} {
			if err := os.MkdirAll(filepath.Join(cache, name), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := verifyCache(cache, []fillDir{module}, checked); err != nil {
			t.Errorf("the folders of the start of a module path fail the fill: %v", err)
		}
		// A link in cache/download is the go command's own business; the walk does not go there.
		cache, module, checked = downloadedLib(t)
		if err := os.Symlink(t.TempDir(), filepath.Join(cache, "cache", "download", "planted")); err != nil {
			t.Fatal(err)
		}
		if err := verifyCache(cache, []fillDir{module}, checked); err != nil {
			t.Errorf("a link under cache/download fails the fill: %v", err)
		}
		// A folder name that is not an escaped path, or path and version: a file system that ignores case opens
		// Example.com for example.com.
		for _, name := range []string{"Example.com", filepath.Join("example.com", "Lib@v1.0.0"), filepath.Join("example.com", "lib@V1.0.0"), filepath.Join("example.com", "!1")} {
			cache, module, checked := downloadedLib(t)
			if err := os.MkdirAll(filepath.Join(cache, name), 0o755); err != nil {
				t.Fatal(err)
			}
			err := verifyCache(cache, []fillDir{module}, checked)
			if err == nil || !strings.Contains(err.Error(), "which is not an escaped module path") {
				t.Errorf("the folder %s in the module cache is not refused: %v", name, err)
			}
		}
	})
	t.Run("ST-STD-LOCAL-R03-T26 A module folder that holds a symbolic link fails the fill of localtest, also after its stamp was kept", func(t *testing.T) {
		// Security review of PR #14, L2: lib.go as a link to a same copy passed the hash, and the stamp then kept the
		// size and time of the link, so an edit of the target was not hashed again.
		for _, dirs := range []bool{false, true} {
			cache, module, checked := downloadedLib(t)
			var listed []fillDir
			if dirs {
				listed = []fillDir{module}
			}
			if err := verifyCache(cache, listed, checked); err != nil || hashedDirs != 1 {
				t.Fatalf("the unchanged folder fails or is not hashed (%d): %v", hashedDirs, err)
			}
			target := filepath.Join(t.TempDir(), "lib.go")
			data, err := os.ReadFile(filepath.Join(module.Dir, "lib.go"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, data, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(module.Dir, "lib.go")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(module.Dir, "lib.go")); err != nil {
				t.Fatal(err)
			}
			err = verifyCache(cache, listed, checked)
			if err == nil || !strings.Contains(err.Error(), "example.com/lib v1.0.0: ") || !strings.Contains(err.Error(), "which is not a regular file or a folder") {
				t.Errorf("listed %v: a module folder with a link to the same content is not refused: %v", dirs, err)
			}
		}
	})
}

func TestFillChecksTheWholeCache(t *testing.T) {
	t.Run("ST-STD-LOCAL-R03-T27 The check at the end of a fill refuses a changed folder of a checked module that the fill did not list", func(t *testing.T) {
		// Security review of PR #14, I1: T19 calls verifyCache, and no test failed when the fill called verifyDirs over
		// its own folders alone.
		cache, module, checked := downloadedLib(t)
		var lines []string
		for line := range checked {
			lines = append(lines, line)
		}
		root, err := filepath.Abs(filepath.Join("..", ".."))
		if err != nil {
			t.Fatal(err)
		}
		if err := verifyFilled(cache, root, nil, lines); err != nil {
			t.Fatalf("the unchanged cache fails the check of the fill: %v", err)
		}
		if err := os.WriteFile(filepath.Join(module.Dir, "zz_planted.go"), []byte("package lib\n\nfunc init() { println(\"ALTERED\") }\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := verifyFilled(cache, root, nil, lines); err == nil || !strings.Contains(err.Error(), "example.com/lib v1.0.0: dir has been modified") {
			t.Fatalf("the check of the fill passes a changed folder that the fill did not list: %v", err)
		}
	})
}

func TestFirstFill(t *testing.T) {
	t.Run("ST-STD-LOCAL-R05-T10 The first fill of the module cache of localtest names the folder, its size and STEGO_TEST_RUNS, and is refused with less than two times its size free", func(t *testing.T) {
		// Owner disk note, 2026-11-27: the cache of about 3 GB went to the user cache folder with no word.
		folder := filepath.Join(t.TempDir(), "runs", "localtest-modules")
		var asked string
		free := func(available uint64) func(string) (uint64, error) {
			return func(path string) (uint64, error) {
				asked = path
				return available, nil
			}
		}
		var out strings.Builder
		if err := firstFill(folder, &out, free(2*cacheSize)); err != nil {
			t.Fatalf("the fill with two times the size free is refused: %v", err)
		}
		notice := out.String()
		if strings.Count(notice, "\n") != 1 || !strings.Contains(notice, folder) || !strings.Contains(notice, "about 3 GB") || !strings.Contains(notice, "STEGO_TEST_RUNS") || !strings.Contains(notice, "remove the folder") {
			t.Errorf("the notice is %q, want one line with the folder, about 3 GB, STEGO_TEST_RUNS and how to remove it", notice)
		}
		if asked != filepath.Dir(filepath.Dir(folder)) {
			t.Errorf("the free space was read at %s, want the nearest folder that exists, %s", asked, filepath.Dir(filepath.Dir(folder)))
		}
		out.Reset()
		err := firstFill(folder, &out, free(2*cacheSize-1))
		if err == nil || !strings.Contains(err.Error(), folder) || !strings.Contains(err.Error(), "less than 6 GB") || !strings.Contains(err.Error(), "STEGO_TEST_RUNS") {
			t.Errorf("the fill with less than two times the size free gave %v, want a refusal that names the folder, 6 GB and STEGO_TEST_RUNS", err)
		}
		// A free space that cannot be read refuses the fill too (security review of PR #13, missing test).
		err = firstFill(folder, &out, func(string) (uint64, error) { return 1 << 40, errors.New("statfs: input/output error") })
		if err == nil || !strings.Contains(err.Error(), "find the free space") || !strings.Contains(err.Error(), "input/output error") {
			t.Errorf("the fill whose free space cannot be read gave %v, want a refusal with the cause", err)
		}
	})
}
