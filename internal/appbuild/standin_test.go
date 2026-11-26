package appbuild_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/mod/sumdb/note"

	"github.com/jsell-rh/stego/internal/appbuild"
	"github.com/jsell-rh/stego/internal/appbuild/proxytest"
)

// The proofs of the local stand-ins of proxy.golang.org and sum.golang.org that the tests of Building use
// (ST-STD-LOCAL-R03): appbuild.UseTestModuleSources and proxytest. A download against them takes only a zip whose
// hash the sum database and go.sum state, and only an answer that the key of GOSUMDB signed.

// standInGoSum is the go.sum of the stand-in source that states both hashes of the module.
var standInGoSum = standInModule + " " + standInVersion + " " + standInSum + "\n" + standInModule + " " + standInVersion + "/go.mod " + standInModSum + "\n"

const (
	standInModule  = "github.com/coder/websocket"
	standInVersion = "v1.8.15"
	standInSum     = "h1:6B2JPeOGlpff2Uz6vOEH1Vzpi0iUz20A+lPVhPHtNUA="
	standInModSum  = "h1:NX3SzP+inril6yawo5CQXx8+fk145lPDC6pumgx0mVg="
)

func standInGo(t *testing.T) string {
	t.Helper()
	if name := os.Getenv("STEGO_TEST_PINNED_GO"); name != "" {
		return name
	}
	return filepath.Join(runtime.GOROOT(), "bin", "go")
}

// standInSource commits a module that requires the stand-in module, and gives the repository and its revision.
// goSum is its go.sum.
func standInSource(t *testing.T, goSum string) (string, string) {
	t.Helper()
	repository := filepath.Join(t.TempDir(), "source")
	for name, data := range map[string]string{
		"go.mod":           "module example.com/tiny\n\ngo 1.26.11\n\nrequire " + standInModule + " " + standInVersion + "\n",
		"go.sum":           goSum,
		"cmd/tiny/main.go": "package main\n\nimport _ \"" + standInModule + "\"\n\nfunc main() {}\n",
	} {
		full := filepath.Join(repository, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) string {
		command := exec.Command("git", append([]string{"-c", "user.name=STEGO", "-c", "user.email=stego@example.test", "-c", "commit.gpgsign=false"}, args...)...)
		command.Dir = repository
		command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	git("init", "-q")
	git("add", "-A")
	git("commit", "-q", "-m", "source")
	return repository, git("rev-parse", "HEAD")
}

// standInDownload downloads the module cache of the stand-in source with the stand-ins in use, and gives the error.
// A Go SDK that is not the pinned one skips the test.
func standInDownload(t *testing.T, goSum string) error {
	t.Helper()
	source, revision := standInSource(t, goSum)
	_, err := appbuild.Download(context.Background(), appbuild.DownloadOptions{Source: source, Revision: revision, Module: ".",
		Target: "cmd/tiny", Go: standInGo(t), Output: filepath.Join(t.TempDir(), "cache")})
	if err != nil && strings.Contains(err.Error(), "is not the pinned official release") {
		t.Skipf("the Go SDK of this run is not the pinned one; set STEGO_TEST_PINNED_GO: %v", err)
	}
	return err
}

func TestLocalModuleStandIns(t *testing.T) {
	t.Run("ST-STD-LOCAL-R03-T03 A download from the local module proxy and sum database takes the zip that go.sum states", func(t *testing.T) {
		servers := proxytest.Start(t, standInGo(t), standInModule+"@"+standInVersion)
		servers.Use(t)
		if err := standInDownload(t, standInGoSum); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("ST-STD-LOCAL-R03-T04 A proxy zip whose hash differs from the sum database and go.sum stops the download", func(t *testing.T) {
		servers := proxytest.Start(t, standInGo(t), standInModule+"@"+standInVersion)
		zip := servers.ZipPath(t, standInModule, standInVersion)
		data, err := os.ReadFile(zip)
		if err != nil {
			t.Fatal(err)
		}
		data[len(data)/2] ^= 0xff
		if err := os.WriteFile(zip, data, 0o644); err != nil {
			t.Fatal(err)
		}
		servers.Use(t)
		if err := standInDownload(t, standInGoSum); err == nil || !strings.Contains(err.Error(), standInModule) {
			t.Fatalf("a download from a proxy with a changed zip gave %v, want a refusal that names %s", err, standInModule)
		}
	})
	t.Run("ST-STD-LOCAL-R03-T05 A sum database whose answers another key signed stops the download", func(t *testing.T) {
		servers := proxytest.Start(t, standInGo(t), standInModule+"@"+standInVersion)
		// GOSUMDB names a verifier key of the same name that did not sign the answers, as a forged database has.
		_, other, err := note.GenerateKey(nil, "stego-proxytest")
		if err != nil {
			t.Fatal(err)
		}
		restore, err := appbuild.UseTestModuleSources(servers.ProxyURL, other, servers.SumDBURL)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(restore)
		// go.sum states the hash of the zip and not that of the go.mod, so the go command asks the sum database
		// for the go.mod, and must refuse an answer that the key of GOSUMDB did not sign.
		err = standInDownload(t, standInModule+" "+standInVersion+" "+standInSum+"\n")
		if err == nil {
			t.Fatal("a download whose sum database answers another key signed succeeded")
		}
		// The same go.sum against the right key downloads, so the refusal comes from the key alone.
		restore()
		servers.Use(t)
		if err := standInDownload(t, standInModule+" "+standInVersion+" "+standInSum+"\n"); err != nil {
			t.Fatalf("the same download against the key that signed the answers failed: %v", err)
		}
	})
	t.Run("ST-STD-LOCAL-R03-T06 The test module sources take only loopback URLs and one test at a time", func(t *testing.T) {
		servers := proxytest.Start(t, standInGo(t))
		for _, bad := range [][2]string{
			{"http://example.com", servers.SumDBURL},
			{servers.ProxyURL, "http://10.0.0.1:80"},
			{"https://127.0.0.1:1", servers.SumDBURL},
			{"http://user:pw@127.0.0.1:1", servers.SumDBURL},
		} {
			if restore, err := appbuild.UseTestModuleSources(bad[0], servers.SumDBKey, bad[1]); err == nil {
				restore()
				t.Errorf("the test module sources %v were taken", bad)
			}
		}
		restore, err := appbuild.UseTestModuleSources(servers.ProxyURL, servers.SumDBKey, servers.SumDBURL)
		if err != nil {
			t.Fatal(err)
		}
		if again, err := appbuild.UseTestModuleSources(servers.ProxyURL, servers.SumDBKey, servers.SumDBURL); err == nil {
			again()
			t.Error("a second test took the test module sources while the first held them")
		}
		restore()
		if again, err := appbuild.UseTestModuleSources(servers.ProxyURL, servers.SumDBKey, servers.SumDBURL); err != nil {
			t.Errorf("the restore did not give the test module sources back: %v", err)
		} else {
			again()
		}
	})
	t.Run("ST-STD-LOCAL-R03-T07 A program that is not a test binary panics when it calls UseTestModuleSources", func(t *testing.T) {
		// A main package of the root module, in testdata so that no build of the module reaches it, calls the
		// function: built with go build, it is no test binary.
		root, err := filepath.Abs(filepath.Join("..", ".."))
		if err != nil {
			t.Fatal(err)
		}
		binary := filepath.Join(t.TempDir(), "notatest")
		build := exec.Command(standInGo(t), "build", "-o", binary, "./internal/appbuild/testdata/notatest")
		build.Dir = root
		build.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=readonly", "GOTOOLCHAIN=local")
		if output, err := build.CombinedOutput(); err != nil {
			t.Fatalf("go build of a program that calls UseTestModuleSources: %v\n%s", err, output)
		}
		output, err := exec.Command(binary).CombinedOutput()
		if err == nil || !strings.Contains(string(output), "UseTestModuleSources is only for the local stand-ins of a test binary") {
			t.Fatalf("the program gave %v, want the panic of UseTestModuleSources:\n%s", err, output)
		}
	})
}
