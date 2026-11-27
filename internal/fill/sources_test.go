package fill

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/910-software-solutions/stego/internal/localtest"
)

func TestTestModuleSources(t *testing.T) {
	t.Run("ST-STD-LOCAL-R03-T21 The fill checks of a test binary take their modules from the checked module cache of localtest and reach no network, and a program that is not a test binary panics", func(t *testing.T) {
		// Decision on the sec-510c side note (2026-11-27): moduleSources put proxy.golang.org in place of each GOPROXY
		// that is not off, so stego plan and apply in tests reached the network.
		cache := localtest.ModuleCache(t)
		proxy := "file://" + filepath.ToSlash(filepath.Join(cache, "cache", "download"))
		for _, bad := range []string{"https://proxy.golang.org", "file://", "file://host/x", proxy + ",https://proxy.golang.org", proxy + "|direct", "direct"} {
			if restore, err := UseTestModuleSources(bad); err == nil {
				restore()
				t.Errorf("the test module proxy %q is taken", bad)
			}
		}
		restore, err := UseTestModuleSources(proxy)
		if err != nil {
			t.Fatal(err)
		}
		if again, err := UseTestModuleSources(proxy); err == nil {
			again()
			t.Error("a second test took the test module sources while the first held them")
		}
		env := moduleSources(filepath.Join(t.TempDir(), "runtime"))
		got := ""
		for _, entry := range env {
			if value, ok := strings.CutPrefix(entry, "GOPROXY="); ok {
				got = value
			}
		}
		if !strings.HasSuffix(got, ","+proxy+",off") || strings.Contains(got, "proxy.golang.org") || !strings.HasPrefix(got, "file://") {
			t.Errorf("the module sources give GOPROXY=%q, want the runtime folder, the checked cache and off", got)
		}
		// A module that the cache does not hold is not found, and no request leaves the host: the sources end in off.
		module := t.TempDir()
		if err := os.WriteFile(filepath.Join(module, "go.mod"), []byte("module example.com/probe\n\ngo 1.26\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		download := exec.Command("go", "mod", "download", "-json", "example.com/not-in-the-cache@v1.0.0")
		download.Dir = module
		download.Env = append(localtest.GoEnv(), env...)
		output, err := download.CombinedOutput()
		if err == nil || !strings.Contains(string(output), "module lookup disabled by GOPROXY=off") && !strings.Contains(string(output), "not found") {
			t.Errorf("a download of a module that the cache does not hold gave %v, want not found with no network:\n%s", err, output)
		}
		restore()
		for _, entry := range moduleSources("") {
			if entry == "GOPROXY="+proxy+",off" {
				t.Error("the restore did not give the module sources back")
			}
		}
		// A main package of the root module, in testdata so that no build of the module reaches it, calls the
		// function: built with go build, it is no test binary.
		root, err := filepath.Abs(filepath.Join("..", ".."))
		if err != nil {
			t.Fatal(err)
		}
		binary := filepath.Join(t.TempDir(), "notatest")
		build := exec.Command("go", "build", "-o", binary, "./internal/fill/testdata/notatest")
		build.Dir = root
		build.Env = localtest.GoEnv("GOFLAGS=-mod=readonly")
		if output, err := build.CombinedOutput(); err != nil {
			t.Fatalf("go build of a program that calls UseTestModuleSources: %v\n%s", err, output)
		}
		output, err = exec.Command(binary).CombinedOutput()
		if err == nil || !strings.Contains(string(output), "UseTestModuleSources is only for the checked module cache of a test binary") {
			t.Fatalf("the program gave %v, want the panic of UseTestModuleSources:\n%s", err, output)
		}
	})
}
