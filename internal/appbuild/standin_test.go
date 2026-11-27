package appbuild_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/910-software-solutions/stego/internal/appbuild"
	"github.com/910-software-solutions/stego/internal/appbuild/proxytest"
)

// The proofs of appbuild.UseTestModuleSources, which points a download at the local stand-ins of proxy.golang.org
// and sum.golang.org that the tests of Building use (ST-STD-LOCAL-R03). The proofs of a download against the
// stand-ins are in cmd/stego (standin_test.go): their modules come from the checked module cache of localtest, which
// Building does not import (ST-STD-CODE-R03).

func standInGo(t *testing.T) string {
	t.Helper()
	if name := os.Getenv("STEGO_TEST_PINNED_GO"); name != "" {
		return name
	}
	return filepath.Join(runtime.GOROOT(), "bin", "go")
}

func TestLocalModuleStandIns(t *testing.T) {
	t.Run("ST-STD-LOCAL-R03-T06 The test module sources take only loopback URLs and one test at a time", func(t *testing.T) {
		servers := proxytest.Start(t, standInGo(t), proxytest.Source{})
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
