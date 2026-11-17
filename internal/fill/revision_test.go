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
