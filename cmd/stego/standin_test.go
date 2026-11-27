package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/sumdb/note"

	"github.com/910-software-solutions/stego/internal/appbuild"
	"github.com/910-software-solutions/stego/internal/appbuild/proxytest"
	"github.com/910-software-solutions/stego/internal/localtest"
)

// The proofs that a download against the local stand-ins of proxy.golang.org and sum.golang.org (proxytest and
// appbuild.UseTestModuleSources) takes only a zip whose hash the sum database and go.sum state, and only an answer
// that the key of GOSUMDB signed (ST-STD-LOCAL-R03). They are tests of cmd/stego, and not of internal/appbuild,
// because the stand-ins take their modules from the checked module cache of localtest (checkedModules): this cmd
// package can import Supplying, and Building cannot (ST-STD-CODE-R03).

// standInGoSum is the go.sum of the stand-in source that states both hashes of the module.
var standInGoSum = standInModule + " " + standInVersion + " " + standInSum + "\n" + standInModule + " " + standInVersion + "/go.mod " + standInModSum + "\n"

const (
	standInModule  = "github.com/coder/websocket"
	standInVersion = "v1.8.15"
	standInSum     = "h1:6B2JPeOGlpff2Uz6vOEH1Vzpi0iUz20A+lPVhPHtNUA="
	standInModSum  = "h1:NX3SzP+inril6yawo5CQXx8+fk145lPDC6pumgx0mVg="
)

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
		Target: "cmd/tiny", Go: pinnedGo(t), Output: filepath.Join(t.TempDir(), "cache")})
	if err != nil && strings.Contains(err.Error(), "is not the pinned official release") {
		t.Skipf("the Go SDK of this run is not the pinned one; set STEGO_TEST_PINNED_GO: %v", err)
	}
	return err
}

func TestLocalModuleStandIns(t *testing.T) {
	t.Run("ST-STD-LOCAL-R03-T03 A download from the local module proxy and sum database takes the zip that go.sum states", func(t *testing.T) {
		servers := proxytest.Start(t, pinnedGo(t), checkedModules(t), standInModule+"@"+standInVersion)
		servers.Use(t)
		if err := standInDownload(t, standInGoSum); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("ST-STD-LOCAL-R03-T04 A proxy zip whose hash differs from the sum database and go.sum stops the download", func(t *testing.T) {
		servers := proxytest.Start(t, pinnedGo(t), checkedModules(t), standInModule+"@"+standInVersion)
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
		servers := proxytest.Start(t, pinnedGo(t), checkedModules(t), standInModule+"@"+standInVersion)
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
}

// callsChecked tells if the expression is a call of checkedModules.
func callsChecked(expression ast.Expr) bool {
	call, ok := expression.(*ast.CallExpr)
	if !ok {
		return false
	}
	fn, ok := call.Fun.(*ast.Ident)
	return ok && fn.Name == "checkedModules"
}

func TestStandInsTakeTheCheckedCache(t *testing.T) {
	t.Run("ST-STD-LOCAL-R03-T17 Each proxytest.Start of cmd/stego takes checkedModules, which is the checked module cache of localtest", func(t *testing.T) {
		names, err := filepath.Glob("*.go")
		if err != nil {
			t.Fatal(err)
		}
		fset := token.NewFileSet()
		calls := 0
		for _, name := range names {
			file, err := parser.ParseFile(fset, name, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(file, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || selector.Sel.Name != "Start" {
					return true
				}
				if pkg, ok := selector.X.(*ast.Ident); !ok || pkg.Name != "proxytest" {
					return true
				}
				calls++
				if len(call.Args) < 3 {
					t.Errorf("%s: proxytest.Start has no source", fset.Position(call.Pos()))
					return true
				}
				if !callsChecked(call.Args[2]) {
					t.Errorf("%s: proxytest.Start takes a source that is not checkedModules(t)", fset.Position(call.Pos()))
				}
				return true
			})
		}
		if calls == 0 {
			t.Fatal("no proxytest.Start in cmd/stego: the scan read nothing")
		}
		source := checkedModules(t)
		if want := localtest.ModuleCache(t); source.Cache != want {
			t.Fatalf("checkedModules gives the module cache %s, not the cache of localtest %s", source.Cache, want)
		}
		if err := proxytest.CheckSource(pinnedGo(t), source); err != nil {
			t.Fatalf("proxytest refuses checkedModules: %v", err)
		}
	})
}
