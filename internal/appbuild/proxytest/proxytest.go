// Package proxytest gives the tests of Building a local module proxy and a local sum database, which play
// proxy.golang.org and sum.golang.org for a download or a build of stego build (ST-STD-LOCAL-R03). It is test
// support: only test files import it.
//
// Start serves, on 127.0.0.1, the GOPROXY protocol for the modules that it is given, from a folder that the test
// fills from its own module cache, and a sum database (golang.org/x/mod/sumdb) whose records are the go.sum lines of
// those modules, signed by a key that Start makes for each run with note.GenerateKey. No key is committed. The sum
// database answers 404 for a module that the proxy does not hold. Use then points internal/appbuild at both servers
// (appbuild.UseTestModuleSources) until the test ends.
package proxytest

import (
	"bytes"
	"crypto/rand"
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

	"github.com/jsell-rh/stego/internal/appbuild"
	"github.com/jsell-rh/stego/internal/localtest"
)

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

// Start serves the modules mods, each module@version, from the module cache of the go command goTool, which
// downloads them with the proxy of the environment when the cache does not hold them, and stops both servers when
// the test ends. The record of each module in the sum database is its go.sum lines, as the hashes of its zip and its
// go.mod in the proxy folder give them.
func Start(t testing.TB, goTool string, mods ...string) *Servers {
	t.Helper()
	s := &Servers{Folder: t.TempDir(), records: map[module.Version][]byte{}}
	for _, mod := range mods {
		path, version, ok := strings.Cut(mod, "@")
		if !ok {
			t.Fatalf("proxytest: %q is not module@version", mod)
		}
		s.add(t, goTool, module.Version{Path: path, Version: version})
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

// add copies the info, the go.mod and the zip of the module from the module cache into the proxy folder, and
// keeps its go.sum lines as its record. A module whose zip the cache does not hold, because only its go.mod is in
// a module graph, gets its go.mod only, and its record names the go.mod alone.
func (s *Servers) add(t testing.TB, goTool string, m module.Version) {
	t.Helper()
	// The modules come from the module cache of localtest, which the go command fills with the sum database on, and
	// never from the shared GOMODCACHE, which other tools fill unchecked (security review of PR #510, M1).
	download := exec.Command(goTool, "mod", "download", "-json", m.Path+"@"+m.Version)
	download.Dir = t.TempDir()
	download.Env = append(localtest.FillEnv(t), "GOFLAGS=")
	if output, err := download.Output(); err != nil {
		// A version that the proxy of the environment does not serve whole, such as one that only a module graph
		// names, is served with its go.mod alone.
		info := exec.Command(goTool, "list", "-m", "-json", m.Path+"@"+m.Version)
		info.Dir, info.Env = download.Dir, download.Env
		if out, err := info.Output(); err != nil {
			t.Fatalf("proxytest: download %s: %v\n%s%s", m, err, output, out)
		}
	}
	cache := localtest.ModuleCache(t)
	escapedPath, err := module.EscapePath(m.Path)
	if err != nil {
		t.Fatal(err)
	}
	escapedVersion, err := module.EscapeVersion(m.Version)
	if err != nil {
		t.Fatal(err)
	}
	from := filepath.Join(cache, "cache", "download", filepath.FromSlash(escapedPath), "@v")
	to := filepath.Join(s.Folder, filepath.FromSlash(escapedPath), "@v")
	if err := os.MkdirAll(to, 0o755); err != nil {
		t.Fatal(err)
	}
	whole := true
	for _, extension := range []string{".info", ".mod", ".zip"} {
		data, err := os.ReadFile(filepath.Join(from, escapedVersion+extension))
		if err != nil && extension == ".zip" && os.IsNotExist(err) {
			whole = false
			continue
		}
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
	record := fmt.Sprintf("%s %s/go.mod %s\n", m.Path, m.Version, modSum)
	if whole {
		zipSum, err := dirhash.HashZip(filepath.Join(to, escapedVersion+".zip"), dirhash.Hash1)
		if err != nil {
			t.Fatal(err)
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
