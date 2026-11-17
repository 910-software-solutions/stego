package fill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/types"
)

// revisionEvidence is evidence with the form that ParseFill accepts. Its
// digests match nothing, so a fill that gets past the slot and contract
// checks is stale or unsigned, not qualified.
const revisionEvidence = "evidence:\n  fill_digest: " + zeroDigest + "\n  contract_digest: " + zeroDigest + "\n  passed_at: 2026-11-17T00:00:00Z\n"

const zeroDigest = "0000000000000000000000000000000000000000000000000000000000000000"

// revisionProject writes a project folder whose declaration binds the fill
// place-order to the command place-order: the fill manifest implements the
// slot given, and out/commands holds one generated Go file.
func revisionProject(t *testing.T, implements string) (string, *types.ServiceDeclaration) {
	t.Helper()
	dir := writeFiles(t, map[string]string{
		"fills/place-order/fill.yaml":    "kind: fill\nname: place-order\nimplements: " + implements + "\n" + revisionEvidence,
		"fills/place-order/fill.go":      "package place_order\n",
		"fills/place-order/fill_test.go": "package place_order\n",
		"out/commands/contract.go":       "package commands\n",
		"SIGNERS":                        "",
	}, []string{"fills/place-order/fill.yaml", "fills/place-order/fill.go", "fills/place-order/fill_test.go", "out/commands/contract.go", "SIGNERS"})
	service := &types.ServiceDeclaration{Commands: []types.Command{{Name: "place-order", Fill: types.FillNames{"place-order"}}}}
	return dir, service
}

func TestRevisionFaultsCapabilityFill(t *testing.T) {
	t.Run("ST-EXT-UC02-R02-T07 stego build refuses a fill of a command that implements another slot, and a contract file that is a link", func(t *testing.T) {
		faults := func(dir string, service *types.ServiceDeclaration) string {
			t.Helper()
			found, err := RevisionFaults(dir, []string{"place-order"}, compiler.RevisionFillCheck(dir, "example.com/inventory", service))
			if err != nil {
				t.Fatal(err)
			}
			return strings.Join(found, "\n")
		}

		// A fill bound to a command whose manifest names the query slot is refused for its slot, before its contract.
		dir, service := revisionProject(t, "query.query")
		if got := faults(dir, service); !strings.Contains(got, `fill "place-order" implements query.query, not the bound slot command.command, for the command place-order`) {
			t.Fatalf("the build did not refuse the fill of another slot: %q", got)
		}
		// A manifest whose slot does not parse is refused the same way.
		dir, service = revisionProject(t, "command")
		if got := faults(dir, service); !strings.Contains(got, `fill "place-order" implements command, not the bound slot command.command`) {
			t.Fatalf("the build did not refuse the fill whose slot does not parse: %q", got)
		}

		// The fill of the bound slot reaches the contract check; its evidence names another contract, so it is stale.
		dir, service = revisionProject(t, "command.command")
		if got := faults(dir, service); !strings.Contains(got, "signed against an older contract") || strings.Contains(got, "not the bound slot") {
			t.Fatalf("the fill of the bound slot did not reach the contract check: %q", got)
		}

		// A Go file of the contract package that is a link is refused, since go build would compile its target.
		outside := filepath.Join(t.TempDir(), "elsewhere.go")
		if err := os.WriteFile(outside, []byte("package commands\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(dir, "out", "commands", "linked.go")); err != nil {
			t.Fatal(err)
		}
		if got := faults(dir, service); !strings.Contains(got, "out/commands holds linked.go, which is not a regular file") {
			t.Fatalf("the build did not refuse the linked contract file: %q", got)
		}

		// A contract package folder that is a link is refused too.
		dir, service = revisionProject(t, "command.command")
		real := filepath.Join(dir, "out", "real")
		if err := os.Rename(filepath.Join(dir, "out", "commands"), real); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, filepath.Join(dir, "out", "commands")); err != nil {
			t.Fatal(err)
		}
		if got := faults(dir, service); !strings.Contains(got, "the contract package out/commands is not a folder") {
			t.Fatalf("the build did not refuse the linked contract folder: %q", got)
		}
	})
}

func TestRevisionFaultsContractFolder(t *testing.T) {
	t.Run("ST-EXT-UC02-R02-T10 stego build refuses a contract package that holds a file go build takes that is not a Go file, any file that is a link, or a linked out folder", func(t *testing.T) {
		faults := func(dir string, service *types.ServiceDeclaration) string {
			t.Helper()
			found, err := RevisionFaults(dir, []string{"place-order"}, compiler.RevisionFillCheck(dir, "example.com/inventory", service))
			if err != nil {
				t.Fatal(err)
			}
			return strings.Join(found, "\n")
		}

		// Each file that go build puts in the binary, other than a Go file, is refused by its name.
		for _, name := range []string{"stub.s", "blob.syso", "glue.c", "glue.h"} {
			dir, service := revisionProject(t, "command.command")
			if err := os.WriteFile(filepath.Join(dir, "out", "commands", name), []byte("x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			want := "out/commands holds " + name + ", which is not a Go file"
			if got := faults(dir, service); !strings.Contains(got, want) {
				t.Fatalf("the build did not refuse %s: %q", name, got)
			}
		}

		// A file that go build does not take is no fault, and the fill reaches the contract check.
		dir, service := revisionProject(t, "command.command")
		if err := os.WriteFile(filepath.Join(dir, "out", "commands", "README.md"), []byte("notes\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := faults(dir, service); !strings.Contains(got, "signed against an older contract") || strings.Contains(got, "README.md") {
			t.Fatalf("the build refused a file that go build does not take: %q", got)
		}

		// A link that is not a Go file is refused too.
		outside := filepath.Join(t.TempDir(), "notes.txt")
		if err := os.WriteFile(outside, []byte("notes\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(dir, "out", "commands", "notes.txt")); err != nil {
			t.Fatal(err)
		}
		if got := faults(dir, service); !strings.Contains(got, "out/commands holds notes.txt, which is not a regular file") {
			t.Fatalf("the build did not refuse the linked file: %q", got)
		}

		// An out folder that is a link is refused, though the contract folder under it is a real folder.
		dir, service = revisionProject(t, "command.command")
		real := filepath.Join(t.TempDir(), "out")
		if err := os.Rename(filepath.Join(dir, "out"), real); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, filepath.Join(dir, "out")); err != nil {
			t.Fatal(err)
		}
		if got := faults(dir, service); !strings.Contains(got, "the generated folder out is not a folder") {
			t.Fatalf("the build did not refuse the linked out folder: %q", got)
		}
	})
}

func TestRevisionFaultsGeneratedTree(t *testing.T) {
	t.Run("ST-EXT-UC02-R02-T11 stego build refuses a package under out, at any depth, that holds a file go build takes that is not a Go file, or a link", func(t *testing.T) {
		faults := func(dir string, service *types.ServiceDeclaration) string {
			t.Helper()
			found, err := RevisionFaults(dir, []string{"place-order"}, compiler.RevisionFillCheck(dir, "example.com/inventory", service))
			if err != nil {
				t.Fatal(err)
			}
			return strings.Join(found, "\n")
		}
		write := func(t *testing.T, dir, name, text string) {
			t.Helper()
			full := filepath.Join(dir, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
		}

		// The Go files of a sub-package, such as the handlers of the commands, are no fault.
		dir, service := revisionProject(t, "command.command")
		write(t, dir, "out/commands/handlers/handlers.go", "package handlers\n")
		write(t, dir, "out/cmd/server/main.go", "package main\n")
		if got := faults(dir, service); !strings.Contains(got, "signed against an older contract") || strings.Contains(got, "generated package") {
			t.Fatalf("the build refused the Go files of the generated packages: %q", got)
		}

		// Each file that go build puts in the binary, other than a Go file, is refused in the handlers package and in
		// a package outside the contract folders.
		for _, name := range []string{"out/commands/handlers/stub.s", "out/commands/handlers/blob.syso", "out/queries/handlers/glue.c", "out/cmd/server/glue.h"} {
			dir, service := revisionProject(t, "command.command")
			write(t, dir, name, "x\n")
			want := "the generated package " + filepath.ToSlash(filepath.Dir(name)) + " holds " + filepath.Base(name) + ", which is not a Go file"
			if got := faults(dir, service); !strings.Contains(got, want) {
				t.Fatalf("the build did not refuse %s: %q", name, got)
			}
		}

		// A Go file that is a link in a sub-package is refused, and so is a sub-package folder that is a link.
		outside := filepath.Join(t.TempDir(), "elsewhere.go")
		if err := os.WriteFile(outside, []byte("package handlers\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		dir, service = revisionProject(t, "command.command")
		write(t, dir, "out/commands/handlers/handlers.go", "package handlers\n")
		if err := os.Symlink(outside, filepath.Join(dir, "out", "commands", "handlers", "fills.go")); err != nil {
			t.Fatal(err)
		}
		if got := faults(dir, service); !strings.Contains(got, "the generated package out/commands/handlers holds fills.go, which is not a regular file") {
			t.Fatalf("the build did not refuse the linked handler file: %q", got)
		}
		dir, service = revisionProject(t, "command.command")
		write(t, dir, "out/cmd/main.go", "package main\n")
		if err := os.Symlink(filepath.Dir(outside), filepath.Join(dir, "out", "cmd", "server")); err != nil {
			t.Fatal(err)
		}
		if got := faults(dir, service); !strings.Contains(got, "the generated package out/cmd holds server, which is not a regular file") {
			t.Fatalf("the build did not refuse the linked handler folder: %q", got)
		}
	})
}
