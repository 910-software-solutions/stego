// Package proxytest gives the tests of Building a local module proxy and a local sum database, which play
// proxy.golang.org and sum.golang.org for a download or a build of stego build (ST-STD-LOCAL-R03). It is test
// support: only test files import it.
//
// Start serves, on 127.0.0.1, the GOPROXY protocol for the modules that it is given, from a folder that the test
// fills from its own module cache, and a sum database (golang.org/x/mod/sumdb) whose records are the go.sum lines of
// those modules, signed by a key that Start makes for each run with note.GenerateKey. No key is committed. The sum
// database answers 404 for a module that the proxy does not hold. Use then points internal/appbuild at both servers
// (appbuild.UseTestModuleSources) until the test ends.
//
// The caller gives the module cache that Start copies the modules from (Source). proxytest does not import
// internal/localtest, which gives the checked module cache: Building does not import Supplying (ST-STD-CODE-R03).
// The tests of cmd/stego, which can import each domain, give the module cache of localtest.
package proxytest

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/mod/module"
	"golang.org/x/mod/sumdb"
	"golang.org/x/mod/sumdb/dirhash"
	"golang.org/x/mod/sumdb/note"

	"github.com/910-software-solutions/stego/internal/appbuild"
)

// Source is the module cache that Start copies the modules from, and the environment of the go command that fills
// it. Give a cache whose go command checks each module against go.sum or a sum database before it writes the module,
// such as the module cache of localtest (localtest.ModuleCache, with localtest.FillEnv and GOFLAGS cleared). Do not
// give the shared GOMODCACHE: other tools fill it with modules that no sum database checked (security review of PR
// #510, M1). CheckSource names the sources that Start refuses.
type Source struct {
	Cache string
	Env   []string
}

// Servers are the local module proxy and sum database of one test.
type Servers struct {
	// ProxyURL is the base URL of the module proxy, and SumDBURL that of the sum database.
	ProxyURL, SumDBURL string
	// SumDBKey is the verifier key of the sum database, its name and its public key.
	SumDBKey string
	// Folder is the folder of the proxy: <escaped path>/@v/<version>.{info,mod,zip} and <escaped path>/@v/list.
	Folder string

	mu      sync.Mutex
	records map[module.Version][]byte
}

// Start serves the modules mods, each module@version, from the module cache of from, which the go command goTool
// fills with the environment of from when the cache does not hold a module, and stops both servers when the test
// ends. When mods is not empty, Start fails the test for a source that CheckSource refuses. The record of each module
// in the sum database is its go.sum lines: the hashes that the go command checked, which the zip and the go.mod in
// the proxy folder must have (add).
func Start(t testing.TB, goTool string, from Source, mods ...string) *Servers {
	t.Helper()
	if len(mods) > 0 {
		if err := CheckSource(goTool, from); err != nil {
			t.Fatal("proxytest: " + err.Error())
		}
	}
	s := &Servers{Folder: t.TempDir(), records: map[module.Version][]byte{}}
	versions := make([]module.Version, 0, len(mods))
	for _, mod := range mods {
		path, version, ok := strings.Cut(mod, "@")
		if !ok {
			t.Fatalf("proxytest: %q is not module@version", mod)
		}
		versions = append(versions, module.Version{Path: path, Version: version})
	}
	downloaded := download(t, goTool, from, versions)
	for _, m := range versions {
		s.add(t, from, m, downloaded[m])
	}
	signerKey, verifierKey, err := note.GenerateKey(rand.Reader, "stego-proxytest")
	if err != nil {
		t.Fatal(err)
	}
	s.SumDBKey = verifierKey
	database := sumdb.NewTestServer(signerKey, func(path, version string) ([]byte, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if record, ok := s.records[module.Version{Path: path, Version: version}]; ok {
			return record, nil
		}
		return nil, os.ErrNotExist
	})
	sumServer := httptest.NewServer(sumdb.NewServer(database))
	t.Cleanup(sumServer.Close)
	proxyServer := httptest.NewServer(http.FileServer(http.Dir(s.Folder)))
	t.Cleanup(proxyServer.Close)
	s.ProxyURL, s.SumDBURL = proxyServer.URL, sumServer.URL
	return s
}

// CheckSource gives an error for a source whose go command can write a module that no sum database or go.sum
// checked. It refuses:
//
//   - a source with no cache, and an environment whose GOMODCACHE is not the cache;
//   - an environment that reads the go env file (GOENV is not off), which can turn a check off;
//   - a cache that is the shared GOMODCACHE of goTool, or a folder in it, as localtest refuses it;
//   - an environment whose GOPATH is the GOPATH of the test, as the go command writes the state of each sum database
//     into GOPATH/pkg/sumdb;
//   - an environment with GOSUMDB=off, a GONOSUMDB, GONOSUMCHECK, GOPRIVATE or GOINSECURE that is not empty, or
//     GOFLAGS with -insecure;
//   - a GOROOT that is not the GOROOT of goTool: the go command reads GOROOT/go.env also with GOENV=off, and takes a
//     setting from it that the environment leaves empty (security review of PR #8, N1).
//
// CheckSource reads the settings twice: as the entries of Env give them (the last entry of a key), and as the go
// command takes them in effect (go env -json with Env), so a value from GOROOT/go.env or another default is seen.
//
// By design, CheckSource takes a GOSUMDB with another key or host than sum.golang.org: the tests of proxytest use a
// sum database of their own on 127.0.0.1. It also takes any GOPROXY, also direct: the sum database still checks each
// module that the go command writes.
func CheckSource(goTool string, from Source) error {
	if from.Cache == "" {
		return errors.New("no module cache to take the modules from")
	}
	cache, err := filepath.Abs(from.Cache)
	if err != nil {
		return err
	}
	if got := lastSetting(from.Env, "GOMODCACHE"); got != from.Cache {
		return fmt.Errorf("the environment of the source names the module cache %q, not %q", got, from.Cache)
	}
	settings := map[string]string{}
	for _, key := range checkedSettings {
		settings[key] = lastSetting(from.Env, key)
	}
	if err := checkSettings("the environment of the source sets", settings); err != nil {
		return err
	}
	// A GOTOOLCHAIN other than local makes goTool run another go program, from PATH or the module cache, for go env and
	// each later command, so the settings that it gives would not be those of goTool (security review of PR #8,
	// confirmation 2, N2). The check runs before any command.
	if got := lastSetting(from.Env, "GOTOOLCHAIN"); got != "local" {
		return fmt.Errorf("the environment of the source sets GOTOOLCHAIN=%q, not local, so another go program could run in place of %s", got, goTool)
	}
	// The settings in effect, as the go command of the source takes them.
	effective, err := goEnv(goTool, from.Env)
	if err != nil {
		return fmt.Errorf("read the settings of the source with %s: %v", goTool, err)
	}
	// go env gives GOENV as the go env file that the go command reads, and "" when GOENV is off.
	if effective["GOENV"] == "" {
		effective["GOENV"] = "off"
	}
	if err := checkSettings("the go command of the source takes", effective); err != nil {
		return err
	}
	if got := effective["GOTOOLCHAIN"]; got != "local" {
		return fmt.Errorf("the go command of the source takes GOTOOLCHAIN=%q, not local", got)
	}
	if got, _ := filepath.Abs(effective["GOMODCACHE"]); got != cache {
		return fmt.Errorf("the go command of the source takes the module cache %q, not %q", effective["GOMODCACHE"], cache)
	}
	// The settings of the test: the GOROOT of goTool itself, with no GOROOT entry, and the shared GOMODCACHE and the
	// GOPATH that goTool takes from the environment of the test, as localtest.cacheFolder reads them.
	own, err := goEnv(goTool, withoutSetting(os.Environ(), "GOROOT"))
	if err != nil {
		return fmt.Errorf("read the settings of %s: %v", goTool, err)
	}
	if !samePath(effective["GOROOT"], own["GOROOT"]) {
		return fmt.Errorf("the go command of the source takes the GOROOT %s, not the GOROOT %s of %s, whose go.env can turn a check off",
			effective["GOROOT"], own["GOROOT"], goTool)
	}
	ambient, err := goEnv(goTool, os.Environ())
	if err != nil {
		return fmt.Errorf("read the settings of the test with %s: %v", goTool, err)
	}
	if shared := ambient["GOMODCACHE"]; shared != "" {
		if abs, err := filepath.Abs(shared); err == nil && (cache == abs || strings.HasPrefix(cache, abs+string(filepath.Separator))) {
			return fmt.Errorf("the module cache %s is the shared GOMODCACHE %s, or a folder in it", cache, abs)
		}
	}
	if first(effective["GOPATH"]) != "" && samePath(first(effective["GOPATH"]), first(ambient["GOPATH"])) {
		return fmt.Errorf("the go command of the source takes the GOPATH %s of the test, into which it writes the state of the sum database; give the source a GOPATH of its own",
			first(effective["GOPATH"]))
	}
	return nil
}

// checkedSettings are the settings that checkSettings reads, and the other settings that CheckSource compares.
var checkedSettings = []string{"GOENV", "GOSUMDB", "GONOSUMDB", "GONOSUMCHECK", "GOPRIVATE", "GOINSECURE", "GOFLAGS"}

// checkSettings gives an error for settings that turn the check of a module off, or let the go env file do it.
func checkSettings(how string, settings map[string]string) error {
	if got := settings["GOENV"]; got != "off" {
		return fmt.Errorf("%s GOENV=%q, not off, so the go env file can turn a check off", how, got)
	}
	if got := settings["GOSUMDB"]; got == "off" {
		return fmt.Errorf("%s GOSUMDB=off", how)
	}
	// GOPRIVATE first: the go command takes it as the default of GONOSUMDB, so its own name is the reason.
	for _, key := range []string{"GOPRIVATE", "GONOSUMDB", "GONOSUMCHECK", "GOINSECURE"} {
		if got := settings[key]; got != "" {
			return fmt.Errorf("%s %s=%s, which turns the check of a module off", how, key, got)
		}
	}
	for _, flag := range strings.Fields(settings["GOFLAGS"]) {
		if flag == "-insecure" || strings.HasPrefix(flag, "-insecure=") || flag == "--insecure" || strings.HasPrefix(flag, "--insecure=") {
			return fmt.Errorf("%s GOFLAGS with %s", how, flag)
		}
	}
	return nil
}

// goEnv gives the settings that the go command goTool takes in effect with the environment env: GOROOT, GOMODCACHE,
// GOPATH, GOTOOLCHAIN and checkedSettings. A setting that the go command does not know, such as GONOSUMCHECK, is the last entry of
// env.
func goEnv(goTool string, env []string) (map[string]string, error) {
	command := exec.Command(goTool, append([]string{"env", "-json", "GOROOT", "GOMODCACHE", "GOPATH", "GOTOOLCHAIN"}, checkedSettings...)...)
	command.Env = env
	command.Dir = os.TempDir()
	var stderr bytes.Buffer
	command.Stderr = &stderr
	out, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("%v: %s", err, stderr.Bytes())
	}
	settings := map[string]string{}
	if err := json.Unmarshal(out, &settings); err != nil {
		return nil, err
	}
	if _, known := settings["GONOSUMCHECK"]; !known || settings["GONOSUMCHECK"] == "" {
		settings["GONOSUMCHECK"] = lastSetting(env, "GONOSUMCHECK")
	}
	return settings, nil
}

// withoutSetting gives env with no entry of key.
func withoutSetting(env []string, key string) []string {
	var out []string
	for _, entry := range env {
		if k, _, _ := strings.Cut(entry, "="); k != key {
			out = append(out, entry)
		}
	}
	return out
}

// first gives the first folder of a list of folders such as GOPATH.
func first(list string) string {
	return strings.Split(list, string(filepath.ListSeparator))[0]
}

// samePath tells if a and b name the same folder, after symbolic links.
func samePath(a, b string) bool {
	resolve := func(name string) string {
		if abs, err := filepath.Abs(name); err == nil {
			name = abs
		}
		if real, err := filepath.EvalSymlinks(name); err == nil {
			name = real
		}
		return name
	}
	return a != "" && b != "" && resolve(a) == resolve(b)
}

// lastSetting gives the value of the last entry key=value of env, as the go command takes it, or "".
func lastSetting(env []string, key string) string {
	value := ""
	for _, entry := range env {
		if k, v, ok := strings.Cut(entry, "="); ok && k == key {
			value = v
		}
	}
	return value
}

// ModulesOf gives each module@version that the go.sum file names, for Start.
func ModulesOf(t testing.TB, goSum string) []string {
	t.Helper()
	data, err := os.ReadFile(goSum)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	var mods []string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		mod := fields[0] + "@" + strings.TrimSuffix(fields[1], "/go.mod")
		if !seen[mod] {
			seen[mod] = true
			mods = append(mods, mod)
		}
	}
	return mods
}

// downloaded is what the go command of the source gives for one module in go mod download -json: the files of the
// cache and the hashes that it checked, and the error of the module, if any.
type downloaded struct {
	Path, Version                         string
	Info, GoMod, Zip, Sum, GoModSum, Error string
	// whole tells that the go command gave the module with no error, so its zip too.
	whole bool
}

// download runs one go mod download -json of the source for all the modules, not one for each: the go command checks
// each module as it does in a run of its own, against go.sum or the sum database of the source, and gives one result
// for each (Error for a module that it could not give whole). For the 59 modules of the go.sum of the runtime library
// one run takes a third of the time of a run for each, and the tests of Building start them about twenty times. A run
// that fails with no module that reports an error, or that gives no result for a module, fails the test: no module is
// then taken as checked.
func download(t testing.TB, goTool string, from Source, mods []module.Version) map[module.Version]downloaded {
	t.Helper()
	results := map[module.Version]downloaded{}
	if len(mods) == 0 {
		return results
	}
	args := []string{"mod", "download", "-json"}
	for _, m := range mods {
		args = append(args, m.Path+"@"+m.Version)
	}
	command := exec.Command(goTool, args...)
	command.Dir = t.TempDir()
	command.Env = append([]string(nil), from.Env...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, downloadErr := command.Output()
	decoder := json.NewDecoder(bytes.NewReader(output))
	failed := false
	for decoder.More() {
		var got downloaded
		if err := decoder.Decode(&got); err != nil {
			t.Fatalf("proxytest: download %v: %v\n%s%s", mods, err, output, stderr.Bytes())
		}
		got.whole = got.Error == ""
		failed = failed || !got.whole
		results[module.Version{Path: got.Path, Version: got.Version}] = got
	}
	if downloadErr != nil && !failed {
		t.Fatalf("proxytest: download %v: %v, and no module gives an error\n%s%s", mods, downloadErr, output, stderr.Bytes())
	}
	for _, m := range mods {
		if _, ok := results[m]; !ok {
			t.Fatalf("proxytest: download %s: the go command gave no result for it: %v\n%s%s", m, downloadErr, output, stderr.Bytes())
		}
	}
	return results
}

// add copies the info, the go.mod and the zip of the module from the module cache of the source into the proxy
// folder, and keeps its go.sum lines as its record. got is what the go command of the source gave for the module
// (download): the files of the cache and the hashes that it checked (Sum and GoModSum). add fails the test when the
// zip or the go.mod that it copied does not have that hash, as a file of the cache that was changed after the check
// has not (security review of PR #8, F1). A module whose zip the go command cannot give, because only its go.mod is
// in a module graph, gets its go.mod only, and its record names the go.mod alone: add never serves a zip that the go
// command did not give.
func (s *Servers) add(t testing.TB, from Source, m module.Version, got downloaded) {
	t.Helper()
	if got.GoModSum == "" || got.GoMod == "" || got.Info == "" {
		// Not even the go.mod of the version is checked.
		t.Fatalf("proxytest: download %s: %s", m, got.Error)
	}
	whole := got.whole
	if whole && (got.Zip == "" || got.Sum == "") {
		t.Fatalf("proxytest: download %s gave no zip or no hash of it: %+v", m, got)
	}
	escapedPath, err := module.EscapePath(m.Path)
	if err != nil {
		t.Fatal(err)
	}
	escapedVersion, err := module.EscapeVersion(m.Version)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := filepath.Abs(from.Cache)
	if err != nil {
		t.Fatal(err)
	}
	cached := filepath.Join(cache, "cache", "download", filepath.FromSlash(escapedPath), "@v")
	to := filepath.Join(s.Folder, filepath.FromSlash(escapedPath), "@v")
	if err := os.MkdirAll(to, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{".info": got.Info, ".mod": got.GoMod}
	if whole {
		files[".zip"] = got.Zip
	}
	for extension, name := range files {
		// The go command names the files of the cache of the source; a file elsewhere is not of that cache.
		if want := filepath.Join(cached, escapedVersion+extension); filepath.Clean(name) != want {
			t.Fatalf("proxytest: the go command gives %s for %s, not the file %s of the cache of the source", name, m, want)
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("proxytest: %s: %v", m, err)
		}
		if err := os.WriteFile(filepath.Join(to, escapedVersion+extension), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	list, _ := os.ReadFile(filepath.Join(to, "list"))
	if err := os.WriteFile(filepath.Join(to, "list"), append(list, []byte(m.Version+"\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	modData, err := os.ReadFile(filepath.Join(to, escapedVersion+".mod"))
	if err != nil {
		t.Fatal(err)
	}
	modSum, err := dirhash.Hash1([]string{"go.mod"}, func(string) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(modData)), nil })
	if err != nil {
		t.Fatal(err)
	}
	if modSum != got.GoModSum {
		t.Fatalf("proxytest: the go.mod of %s in the cache has the hash %s, not the checked hash %s", m, modSum, got.GoModSum)
	}
	record := fmt.Sprintf("%s %s/go.mod %s\n", m.Path, m.Version, modSum)
	if whole {
		zipSum, err := dirhash.HashZip(filepath.Join(to, escapedVersion+".zip"), dirhash.Hash1)
		if err != nil {
			t.Fatal(err)
		}
		if zipSum != got.Sum {
			t.Fatalf("proxytest: the zip of %s in the cache has the hash %s, not the checked hash %s", m, zipSum, got.Sum)
		}
		record = fmt.Sprintf("%s %s %s\n", m.Path, m.Version, zipSum) + record
	}
	s.mu.Lock()
	s.records[m] = []byte(record)
	s.mu.Unlock()
}

// Use points internal/appbuild at the servers until the test ends.
func (s *Servers) Use(t testing.TB) {
	t.Helper()
	restore, err := appbuild.UseTestModuleSources(s.ProxyURL, s.SumDBKey, s.SumDBURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restore)
}

// ZipPath is the path of the zip of the module in the proxy folder, which a test can change to make a proxy that
// serves a zip whose hash differs from the record of the sum database and from go.sum.
func (s *Servers) ZipPath(t testing.TB, path, version string) string {
	t.Helper()
	escapedPath, err := module.EscapePath(path)
	if err != nil {
		t.Fatal(err)
	}
	escapedVersion, err := module.EscapeVersion(version)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(s.Folder, filepath.FromSlash(escapedPath), "@v", escapedVersion+".zip")
}
