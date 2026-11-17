package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/fill"
	"github.com/jsell-rh/stego/internal/types"
)

// The fills of a command, a query and a worker are qualified as every fill is (decision on ST-EXT-Q30): stego test
// runs their tests and records evidence, stego fill status lists them, stego fill sign signs them, and plan and apply
// refuse such a fill that is not qualified. Each fill names no collection; its binding is the command, query or
// worker that names it.

// capabilityFills are the fills of the walkthrough project, each by its slot.
var capabilityFills = []struct{ name, slot string }{
	{"place-order", "command.command"}, {"open-orders", "query.query"}, {"fulfill-orders", "worker.worker"},
}

// newCapabilityProject makes the inventory project of the fifth walkthrough (testdata/walkthrough5) in a new folder,
// with the SIGNERS file of ST-EXT-TD02, whose keys the test run makes for the test only, and runs stego init and
// stego fill create for the fill of its command, its query and its worker, as the service author does. No apply runs.
func newCapabilityProject(t *testing.T) *fillProject {
	t.Helper()
	p := &fillProject{dir: filepath.Join(t.TempDir(), "inventory"), keys: testFillKeys(t)}
	if err := os.MkdirAll(p.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p.must(t, "no-key", "init", "--archetype", "rest-crud")
	for name, target := range map[string]string{"schema.zed": "authz/schema.zed", "inventory.proto": "proto/inventory.proto", "service.yaml": "service.yaml"} {
		p.write(t, target, walkthroughFixture(t, name))
	}
	p.write(t, "SIGNERS", p.keys.signers)
	for _, f := range capabilityFills {
		p.must(t, "no-key", "fill", "create", f.name, "--slot", f.slot)
	}
	return p
}

// qualifyInProcess qualifies the fills of the project in the working folder in this process, as the service author
// does: it writes the SIGNERS file of ST-EXT-TD02, runs stego test, and signs each fill with evidence with listed-ssh, a key that
// the test run makes for the test only. Plan and apply then accept the fills (ST-GEN-INV10).
func qualifyInProcess(t *testing.T) {
	t.Helper()
	keys := testFillKeys(t)
	writeProjectFile(t, "SIGNERS", keys.signers)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(keys.homes["listed-ssh"], ".gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("SSH_AUTH_SOCK", "")
	mustRun(t, runTest)
	entries, err := os.ReadDir("fills")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// A fill with no evidence, such as one that holds only fill.yaml, has nothing to sign.
		if f, err := fill.ParseFill(filepath.Join("fills", entry.Name(), fill.ManifestFile)); err == nil && f.Evidence != nil {
			mustRun(t, runFillSign, entry.Name())
		}
	}
}

// writeWalkthroughFills writes the fills of the walkthrough project (walkthroughFills), each with its manifest, its
// Go file and a test file, so that stego test can qualify it.
func writeWalkthroughFills(t *testing.T) {
	t.Helper()
	for name, f := range walkthroughFills {
		writeProjectFile(t, "fills/"+name+"/fill.yaml", "kind: fill\nname: "+name+"\nimplements: "+f.implements+"\n")
		writeProjectFile(t, "fills/"+name+"/fill.go", f.source)
		pkg := strings.ReplaceAll(name, "-", "_")
		writeProjectFile(t, "fills/"+name+"/fill_test.go", "package "+pkg+"\n\nimport \"testing\"\n\n// TestBuilds holds that the fill builds against its contract.\nfunc TestBuilds(t *testing.T) {}\n")
	}
}

func TestCapabilityFillQualification(t *testing.T) {
	t.Parallel()
	t.Run("ST-EXT-UC02-R02-T05 Plan and apply refuse a fill of a command, a query or a worker until stego test and a listed key qualify it, and again when it changes", func(t *testing.T) {
		p := newCapabilityProject(t)
		// The scaffolded fills have no evidence: fill status lists them, and plan and apply refuse each and name
		// what to run.
		status := p.must(t, "no-key", "fill", "status")
		for _, f := range capabilityFills {
			expectStanding(t, status, f.name, fill.Untested)
		}
		texts := []string{"run stego test, then stego fill sign"}
		for _, f := range capabilityFills {
			texts = append(texts, `fill "`+f.name+`" is untested`)
		}
		expectRefused(t, p, texts...)

		// stego test runs their tests against the contracts that apply plans, before that apply, and records the
		// evidence.
		stdout := p.must(t, "no-key", "test")
		for _, f := range capabilityFills {
			if !strings.Contains(stdout, "ok  \texample.com/fills/fills/"+f.name) {
				t.Fatalf("stego test did not run the tests of %s:\n%s", f.name, stdout)
			}
			manifest := p.manifest(t, f.name)
			if manifest.Evidence == nil || manifest.Collection != "" {
				t.Fatalf("stego test recorded no evidence for %s, or the fill names a collection: %+v", f.name, manifest)
			}
		}
		if _, err := os.Stat(p.path("out")); err == nil {
			t.Fatal("stego test wrote the out folder")
		}
		expectRefused(t, p, `fill "place-order" is unsigned`, "run stego fill sign place-order")

		// A key that SIGNERS does not list does not qualify a fill.
		p.must(t, "unlisted-ssh", "fill", "sign", "place-order")
		expectRefused(t, p, "place-order", "SIGNERS does not list its key")

		// The listed test key qualifies the three fills, and plan and apply pass.
		for _, f := range capabilityFills {
			p.must(t, "listed-ssh", "fill", "sign", f.name)
			manifest := p.manifest(t, f.name)
			if err := sshVerify(t, p.keys.listedSSHPublic, manifest.SignOff, evidenceDigest(t, manifest)); err != nil {
				t.Fatalf("ssh-keygen -Y verify refuses the sign-off of %s: %v", f.name, err)
			}
		}
		status = p.must(t, "no-key", "fill", "status")
		for _, f := range capabilityFills {
			expectStanding(t, status, f.name, fill.Qualified)
		}
		p.must(t, "no-key", "plan")
		p.must(t, "no-key", "apply")
		for _, contract := range []string{"out/commands", "out/queries", "out/workers"} {
			if _, err := os.Stat(p.path(contract)); err != nil {
				t.Fatalf("apply did not write %s: %v", contract, err)
			}
		}

		// A change to a fill makes it stale, and plan and apply refuse it and do not name the other fills.
		p.replace(t, "fills/place-order/fill.go", "the fill is not implemented", "the fill is not written")
		expectRefused(t, p, `fill "place-order" is stale`, "the file fill.go changed after its evidence", "run stego test, then stego fill sign place-order")
		if stdout, stderr, _ := p.stego(t, "no-key", "plan"); strings.Contains(stdout+stderr, "open-orders") || strings.Contains(stdout+stderr, "fulfill-orders") {
			t.Fatalf("plan names a fill that did not change:\n%s\n%s", stdout, stderr)
		}
		p.replace(t, "fills/place-order/fill.go", "the fill is not written", "the fill is not implemented")
		p.must(t, "no-key", "plan")

		// A change to the declaration of the command changes its contract, so its fill is stale too.
		p.replace(t, "service.yaml", "{name: not_draft, code: order.not_draft, status: 409}", "{name: not_draft, code: order.not_draft, status: 422}")
		expectRefused(t, p, `fill "place-order" is stale`, "the contract command.command changed after its evidence")
		p.replace(t, "service.yaml", "status: 422}", "status: 409}")
		p.must(t, "no-key", "plan")

		// A fill of the query slot that names a collection is refused, naming the field.
		p.replace(t, "fills/open-orders/fill.yaml", "collection: \"\"", "collection: warehouses")
		expectRefused(t, p, "open-orders", "collection: a fill of the slot query.query names no collection")
	})
}

func TestCapabilityContractDigest(t *testing.T) {
	t.Run("ST-EXT-DS01-T06 The contract digest of the fill of a command, a query or a worker changes exactly when the contract that the fill compiles against changes", func(t *testing.T) {
		walkthroughProject(t)
		for _, f := range capabilityFills {
			writeProjectFile(t, "fills/"+f.name+"/fill.yaml", "kind: fill\nname: "+f.name+"\nimplements: "+f.slot+"\n")
		}
		// place-order uses the shared repository of Order too.
		writeProjectFile(t, "fills/place-order/fill.go", `package place_order

import (
	"context"
	"errors"

	commands "example.com/inventory/out/commands"
)

func New() commands.PlaceOrderFill {
	return func(ctx context.Context, req *commands.PlaceOrderRequest, h *commands.PlaceOrderHandle) (*commands.PlaceOrderResponse, error) {
		_, err := commands.OrderRepository(h)
		return nil, errors.Join(err, errors.New("place-order: not written"))
	}
}
`)
		registryDir := shippedRegistryCopy(t)
		t.Setenv("STEGO_REGISTRY", registryDir)
		digests := func() map[string]string {
			t.Helper()
			input, err := buildReconcilerInput()
			if err != nil {
				t.Fatal(err)
			}
			check, err := compiler.ResolveFillCheck(input)
			if err != nil {
				t.Fatal(err)
			}
			result := map[string]string{}
			for _, f := range capabilityFills {
				ref, err := types.ParseSlotRef(f.slot)
				if err != nil {
					t.Fatal(err)
				}
				if result[f.name], err = fill.CapabilityContractDigest(check, f.name, ref); err != nil {
					t.Fatal(err)
				}
			}
			return result
		}
		base := digests()
		if again := digests(); again["place-order"] != base["place-order"] || again["open-orders"] != base["open-orders"] || again["fulfill-orders"] != base["fulfill-orders"] {
			t.Fatal("the contract digest is not the same for the same inputs")
		}
		replaceIn := func(name, old, new string) {
			t.Helper()
			data, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), old) {
				t.Fatalf("%s does not hold %q", name, old)
			}
			if err := os.WriteFile(name, []byte(strings.Replace(string(data), old, new, 1)), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		for _, c := range []struct {
			change        string
			file, old     string
			new           string
			changed, same []string
		}{
			{"a new refusal of the command", "service.yaml", "      - {name: not_draft, code: order.not_draft, status: 409}\n", "      - {name: not_draft, code: order.not_draft, status: 409}\n      - {name: closed, code: order.closed, status: 409}\n", []string{"place-order"}, []string{"open-orders", "fulfill-orders"}},
			{"a new field of the command request message", "proto/inventory.proto", "message PlaceOrderRequest {\n", "message PlaceOrderRequest {\n  string note = 99;\n", []string{"place-order"}, []string{"open-orders", "fulfill-orders"}},
			{"a new command in the same contract package", "service.yaml", "\nqueries:", walkthroughFixture(t, "grant-clerk.yaml") + "\nqueries:", nil, []string{"place-order", "open-orders", "fulfill-orders"}},
			{"a new field of Order, which place-order uses through the shared repository and the query reads", "service.yaml", "      - { name: total_units, type: int32, min: 0, max: 1000000, default: 0 }\n", "      - { name: total_units, type: int32, min: 0, max: 1000000, default: 0 }\n      - { name: note, type: string, max_length: 80 }\n", []string{"place-order", "open-orders"}, []string{"fulfill-orders"}},
			{"a new entity that the worker writes, which no fill uses", "service.yaml", "trigger: consumes fulfillment\n    writes: [Order, StockItem]", "trigger: consumes fulfillment\n    writes: [Order, StockItem, OrderLine]", nil, []string{"place-order", "open-orders", "fulfill-orders"}},
			{"a description-only edit of the command component manifest", filepath.Join(registryDir, "components/command/component.yaml"), "kind: component", "# A comment.\nkind: component", nil, []string{"place-order", "open-orders", "fulfill-orders"}},
		} {
			if c.change == "a new command in the same contract package" {
				writeProjectFile(t, "fills/grant-clerk/fill.yaml", "kind: fill\nname: grant-clerk\nimplements: command.command\n")
			}
			before := digests()
			replaceIn(c.file, c.old, c.new)
			after := digests()
			for _, name := range c.changed {
				if after[name] == before[name] {
					t.Errorf("%s: the contract digest of %s did not change", c.change, name)
				}
			}
			for _, name := range c.same {
				if after[name] != before[name] {
					t.Errorf("%s: the contract digest of %s changed", c.change, name)
				}
			}
		}
	})
}
