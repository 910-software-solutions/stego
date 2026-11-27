package proxytest

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/mod/module"
	"golang.org/x/mod/sumdb"
	"golang.org/x/mod/sumdb/dirhash"
	"golang.org/x/mod/sumdb/note"
	modzip "golang.org/x/mod/zip"
)

// The proofs that Start serves only what the go command of the source checked (security review of PR #8, F1 and
// F2). They run with no network: an upstream module proxy in a folder (GOPROXY=file://) and a sum database on
// 127.0.0.1 that signs the true hashes play proxy.golang.org and sum.golang.org for the go command of the source.

const (
	upstreamModule  = "example.com/stand/in"
	upstreamVersion = "v1.0.0"
)

func testGo() string {
	if name := os.Getenv("STEGO_TEST_PINNED_GO"); name != "" {
		return name
	}
	return filepath.Join(runtime.GOROOT(), "bin", "go")
}

// upstream is a module proxy in a folder and a sum database that holds the true go.sum lines of its modules.
type upstream struct {
	folder, sumDB, key string
	zipSum, modSum     string
}

// newUpstream writes the module upstreamModule into a proxy folder, with its zip when zip is true, and serves a sum
// database with its true hashes until the test ends.
func newUpstream(t *testing.T, zip bool) *upstream {
	t.Helper()
	u := &upstream{folder: t.TempDir()}
	at := filepath.Join(u.folder, upstreamModule, "@v")
	if err := os.MkdirAll(at, 0o755); err != nil {
		t.Fatal(err)
	}
	mod := []byte("module " + upstreamModule + "\n\ngo 1.21\n")
	write := func(name string, data []byte) {
		if err := os.WriteFile(filepath.Join(at, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("list", []byte(upstreamVersion+"\n"))
	write(upstreamVersion+".info", []byte(`{"Version":"`+upstreamVersion+`","Time":"2026-01-01T00:00:00Z"}`))
	write(upstreamVersion+".mod", mod)
	var err error
	u.modSum, err = dirhash.Hash1([]string{"go.mod"}, func(string) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(mod)), nil })
	if err != nil {
		t.Fatal(err)
	}
	records := upstreamModule + " " + upstreamVersion + "/go.mod " + u.modSum + "\n"
	if zip {
		var buf bytes.Buffer
		files := []modzip.File{memFile{"go.mod", mod}, memFile{"in.go", []byte("package in\n")}}
		if err := modzip.Create(&buf, module.Version{Path: upstreamModule, Version: upstreamVersion}, files); err != nil {
			t.Fatal(err)
		}
		write(upstreamVersion+".zip", buf.Bytes())
		if u.zipSum, err = dirhash.HashZip(filepath.Join(at, upstreamVersion+".zip"), dirhash.Hash1); err != nil {
			t.Fatal(err)
		}
		records = upstreamModule + " " + upstreamVersion + " " + u.zipSum + "\n" + records
	}
	signer, verifier, err := note.GenerateKey(rand.Reader, "proxytest-upstream")
	if err != nil {
		t.Fatal(err)
	}
	database := sumdb.NewTestServer(signer, func(path, version string) ([]byte, error) {
		if path == upstreamModule && version == upstreamVersion {
			return []byte(records), nil
		}
		return nil, os.ErrNotExist
	})
	server := httptest.NewServer(sumdb.NewServer(database))
	t.Cleanup(server.Close)
	u.sumDB, u.key = server.URL, verifier
	return u
}

// source gives a module cache of the test and the environment of a go command that fills it from the upstream with
// each check on.
func (u *upstream) source(t *testing.T) Source {
	t.Helper()
	cache := filepath.Join(t.TempDir(), "modules")
	env := []string{}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(key, "GO") || key == "GOROOT" || key == "GOCACHE" {
			env = append(env, entry)
		}
	}
	// GOPATH of the test: the go command keeps the state of each sum database in GOPATH/pkg/sumdb, and each upstream
	// has a key of its own under the same name.
	env = append(env, "GOPATH="+filepath.Join(filepath.Dir(cache), "gopath"), "GOMODCACHE="+cache, "GOPROXY=file://"+filepath.ToSlash(u.folder), "GOSUMDB="+u.key+" "+u.sumDB,
		"GONOSUMDB=", "GONOSUMCHECK=", "GOPRIVATE=", "GONOPROXY=", "GOINSECURE=", "GOVCS=*:off", "GOENV=off", "GOFLAGS=",
		"GOWORK=off", "GOTOOLCHAIN=local")
	// The go command makes the files of the cache read-only; go clean -modcache with the same environment, so with
	// this cache and never the shared one, removes them before t.TempDir does.
	t.Cleanup(func() {
		clean := exec.Command(testGo(), "clean", "-modcache")
		clean.Env = env
		_ = clean.Run()
	})
	return Source{Cache: cache, Env: env}
}

type memFile struct {
	name string
	data []byte
}

func (f memFile) Path() string                 { return f.name }
func (f memFile) Lstat() (os.FileInfo, error)  { return memInfo(f), nil }
func (f memFile) Open() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(f.data)), nil }

type memInfo memFile

func (i memInfo) Name() string       { return filepath.Base(i.name) }
func (i memInfo) Size() int64        { return int64(len(i.data)) }
func (i memInfo) Mode() os.FileMode  { return 0o644 }
func (i memInfo) ModTime() time.Time { return time.Time{} }
func (i memInfo) IsDir() bool        { return false }
func (i memInfo) Sys() any           { return nil }

// fatalRecorder is a testing.TB whose Fatal ends only the goroutine of the call and keeps the message, so a test can
// see that Start fails.
type fatalRecorder struct {
	testing.TB
	mu      sync.Mutex
	message string
}

func (r *fatalRecorder) Fatal(args ...any) { r.fail(fmt.Sprint(args...)) }
func (r *fatalRecorder) Fatalf(format string, args ...any) {
	r.fail(fmt.Sprintf(format, args...))
}
func (r *fatalRecorder) fail(message string) {
	r.mu.Lock()
	r.message = message
	r.mu.Unlock()
	runtime.Goexit()
}

// startFails runs Start and gives the message of its failure, or "" when it served the modules.
func startFails(t *testing.T, from Source, mods ...string) (string, *Servers) {
	t.Helper()
	r := &fatalRecorder{TB: t}
	var servers *Servers
	done := make(chan struct{})
	go func() {
		defer close(done)
		servers = Start(r, testGo(), from, mods...)
	}()
	<-done
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.message, servers
}

func TestStartServesOnlyCheckedModules(t *testing.T) {
	mod := upstreamModule + "@" + upstreamVersion
	t.Run("ST-STD-LOCAL-R03-T14 Start refuses a zip of the cache whose hash is not the hash that the go command checked", func(t *testing.T) {
		u := newUpstream(t, true)
		from := u.source(t)
		failure, servers := startFails(t, from, mod)
		if failure != "" {
			t.Fatalf("Start refused a module of a checked source: %s", failure)
		}
		record := string(servers.records[module.Version{Path: upstreamModule, Version: upstreamVersion}])
		if !strings.Contains(record, u.zipSum) || !strings.Contains(record, u.modSum) {
			t.Fatalf("the record %q does not hold the true hashes %s and %s", record, u.zipSum, u.modSum)
		}
		// The zip in the cache changes after the check; its folder and its .ziphash stay, so the go command still
		// reports the checked hash.
		zip := filepath.Join(from.Cache, "cache", "download", upstreamModule, "@v", upstreamVersion+".zip")
		data, err := os.ReadFile(zip)
		if err != nil {
			t.Fatal(err)
		}
		var changed bytes.Buffer
		files := []modzip.File{memFile{"go.mod", []byte("module " + upstreamModule + "\n\ngo 1.21\n")}, memFile{"in.go", []byte("package in\n\nvar Planted = 1\n")}}
		if err := modzip.Create(&changed, module.Version{Path: upstreamModule, Version: upstreamVersion}, files); err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(changed.Bytes(), data) {
			t.Fatal("the planted zip is the same as the true one")
		}
		if err := os.Chmod(zip, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(zip, changed.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		failure, _ = startFails(t, from, mod)
		if !strings.Contains(failure, "not the checked hash "+u.zipSum) {
			t.Fatalf("Start served a changed zip of the cache; its failure is %q", failure)
		}
	})
	t.Run("ST-STD-LOCAL-R03-T15 Start serves no zip of a module whose zip the go command could not give", func(t *testing.T) {
		u := newUpstream(t, false)
		from := u.source(t)
		// A zip with no .ziphash in the cache, which no go command wrote or checked.
		at := filepath.Join(from.Cache, "cache", "download", upstreamModule, "@v")
		if err := os.MkdirAll(at, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(at, upstreamVersion+".zip"), []byte("planted"), 0o644); err != nil {
			t.Fatal(err)
		}
		failure, servers := startFails(t, from, mod)
		if failure != "" {
			t.Fatalf("Start refused the go.mod of a checked source: %s", failure)
		}
		if _, err := os.Stat(servers.ZipPath(t, upstreamModule, upstreamVersion)); !os.IsNotExist(err) {
			t.Fatalf("Start serves a zip that the go command did not give: %v", err)
		}
		record := string(servers.records[module.Version{Path: upstreamModule, Version: upstreamVersion}])
		if record != upstreamModule+" "+upstreamVersion+"/go.mod "+u.modSum+"\n" {
			t.Fatalf("the record is %q, want the go.mod line alone", record)
		}
	})
	t.Run("ST-STD-LOCAL-R03-T16 Start refuses a source whose go command does not check each module", func(t *testing.T) {
		u := newUpstream(t, true)
		good := u.source(t)
		sharedOut, err := exec.Command(testGo(), "env", "GOMODCACHE").Output()
		if err != nil {
			t.Fatal(err)
		}
		shared := strings.TrimSpace(string(sharedOut))
		ambientOut, err := exec.Command(testGo(), "env", "GOPATH").Output()
		if err != nil {
			t.Fatal(err)
		}
		ambientGOPATH := strings.TrimSpace(string(ambientOut))
		// plantedRoot is a copy of the GOROOT of the go command, by symbolic links, with a go.env of its own. The go
		// command reads GOROOT/go.env also with GOENV=off, and takes a setting from it that the environment leaves
		// empty (security review of PR #8, N1).
		rootOut, err := exec.Command(testGo(), "env", "GOROOT").Output()
		if err != nil {
			t.Fatal(err)
		}
		plantedRoot := func(goEnv string) string {
			real, root := strings.TrimSpace(string(rootOut)), t.TempDir()
			entries, err := os.ReadDir(real)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.Name() != "go.env" {
					if err := os.Symlink(filepath.Join(real, entry.Name()), filepath.Join(root, entry.Name())); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := os.WriteFile(filepath.Join(root, "go.env"), []byte(goEnv), 0o644); err != nil {
				t.Fatal(err)
			}
			return root
		}
		with := func(cache string, extra ...string) Source {
			env := append(append([]string(nil), good.Env...), extra...)
			if cache != good.Cache {
				env = append(env, "GOMODCACHE="+cache)
			}
			return Source{Cache: cache, Env: env}
		}
		for name, c := range map[string]struct {
			from Source
			want string
		}{
			"no cache":           {Source{Env: good.Env}, "no module cache"},
			"another GOMODCACHE": {Source{Cache: good.Cache, Env: append(append([]string(nil), good.Env...), "GOMODCACHE="+t.TempDir())}, "names the module cache"},
			// GOPROXY=off: when the check is gone, the go command still writes nothing into the shared cache.
			"the shared cache":         {with(shared, "GOPROXY=off"), "the shared GOMODCACHE"},
			"a folder in shared cache": {with(filepath.Join(shared, "cache", "download"), "GOPROXY=off"), "the shared GOMODCACHE"},
			"GOSUMDB=off":              {with(good.Cache, "GOSUMDB=off"), "GOSUMDB=off"},
			"GONOSUMDB":                {with(good.Cache, "GONOSUMDB=example.com"), "GONOSUMDB=example.com"},
			"GONOSUMCHECK":             {with(good.Cache, "GONOSUMCHECK=1"), "GONOSUMCHECK=1"},
			"GOPRIVATE":                {with(good.Cache, "GOPRIVATE=*"), "GOPRIVATE=*"},
			"GOINSECURE":               {with(good.Cache, "GOINSECURE=example.com"), "GOINSECURE=example.com"},
			"GOFLAGS -insecure":        {with(good.Cache, "GOFLAGS=-mod=mod -insecure"), "-insecure"},
			"the go env file":          {with(good.Cache, "GOENV="+filepath.Join(t.TempDir(), "env")), "GOENV"},
			// A GOROOT whose go.env turns a check off, with the setting empty in the environment.
			"GOROOT/go.env GONOSUMDB":  {with(good.Cache, "GOROOT="+plantedRoot("GONOSUMDB=*\n")), "takes GONOSUMDB=*"},
			"GOROOT/go.env GOSUMDB":    {with(good.Cache, "GOSUMDB=", "GOROOT="+plantedRoot("GOSUMDB=off\n")), "takes GOSUMDB=off"},
			"GOROOT/go.env GOPRIVATE":  {with(good.Cache, "GOROOT="+plantedRoot("GOPRIVATE=*\n")), "takes GOPRIVATE=*"},
			"GOROOT/go.env GOINSECURE": {with(good.Cache, "GOROOT="+plantedRoot("GOINSECURE=*\n")), "takes GOINSECURE=*"},
			"another GOROOT":           {with(good.Cache, "GOROOT="+plantedRoot("GOSUMDB=sum.golang.org\n")), "not the GOROOT"},
			// Another go program in place of the go command of the test (security review of PR #8, confirmation 2, N2).
			"GOTOOLCHAIN of a version": {with(good.Cache, "GOTOOLCHAIN=go1.26.11"), "GOTOOLCHAIN=\"go1.26.11\", not local"},
			"GOTOOLCHAIN path":         {with(good.Cache, "GOTOOLCHAIN=path"), "GOTOOLCHAIN=\"path\", not local"},
			"no GOTOOLCHAIN":           {with(good.Cache, "GOTOOLCHAIN="), "GOTOOLCHAIN=\"\", not local"},
			// GOPROXY=off: when the check is gone, the go command still writes nothing into the GOPATH of the test.
			"the GOPATH of the test": {with(good.Cache, "GOPATH="+ambientGOPATH, "GOPROXY=off"), "the GOPATH " + ambientGOPATH + " of the test"},
		} {
			failure, _ := startFails(t, c.from, mod)
			if !strings.Contains(failure, c.want) {
				t.Errorf("%s: Start gave %q, want a refusal with %q", name, failure, c.want)
			}
		}
		// The checked source itself is taken, so each refusal comes from its planted setting alone.
		if failure, _ := startFails(t, good, mod); failure != "" {
			t.Fatalf("Start refused the checked source: %s", failure)
		}
	})
}
