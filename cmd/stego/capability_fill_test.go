package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/910-software-solutions/stego/internal/compiler"
	"github.com/910-software-solutions/stego/internal/fill"
	"github.com/910-software-solutions/stego/internal/types"
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

// signingSettings sets the Git configuration with which stego fill sign signs in this process: that of listed-ssh,
// and no SSH agent.
func signingSettings(t *testing.T, keys *fillKeys) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(keys.homes["listed-ssh"], ".gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("SSH_AUTH_SOCK", "")
}

// qualifyInProcess qualifies the fills of the project in the working folder in this process, as the service author
// does: it writes the SIGNERS file of ST-EXT-TD02, runs stego test, and signs each fill with evidence with listed-ssh, a key that
// the test run makes for the test only. Plan and apply then accept the fills (ST-GEN-INV10).
func qualifyInProcess(t *testing.T) {
	t.Helper()
	keys := testFillKeys(t)
	writeProjectFile(t, "SIGNERS", keys.signers)
	signingSettings(t, keys)
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

		// An argument that changes which tests run or which files they build is refused, and no evidence is written.
		for _, arg := range []string{"-run=^$", "-short", "-overlay=other.json", "-tags=other", "./other/..."} {
			if stdout, stderr, err := p.stego(t, "no-key", "test", "--", arg); err == nil || !strings.Contains(stderr, "does not pass") {
				t.Fatalf("stego test accepted %s: %v\n%s\n%s", arg, err, stdout, stderr)
			}
			if p.manifest(t, "place-order").Evidence != nil {
				t.Fatalf("stego test with %s wrote evidence", arg)
			}
		}

		// stego test runs their tests against the contracts that apply plans, before that apply, and records the
		// evidence.
		stdout := p.must(t, "no-key", "test", "--", "-count=1", "-v")
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

		// A fill that imports its contract with a dot is refused, as it hides the names that its digest counts.
		p.write(t, "fills/place-order/dot.go", "package placeorder\n\nimport . \"example.com/fills/out/commands\"\n\nvar _ PlaceOrderFill\n")
		expectRefused(t, p, "place-order", "with a dot")
		if err := os.Remove(p.path("fills/place-order/dot.go")); err != nil {
			t.Fatal(err)
		}
		p.must(t, "no-key", "plan")

		// A Go environment file that filters the tests out does not turn a failing test into evidence, and neither
		// does a count of 0.
		p.write(t, "fills/place-order/fail_test.go", "package placeorder\n\nimport \"testing\"\n\nfunc TestFails(t *testing.T) { t.Fatal(\"fails\") }\n")
		goEnv := filepath.Join(t.TempDir(), "go.env")
		if err := os.WriteFile(goEnv, []byte("GOFLAGS=-run=^$\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		before := p.read(t, "fills/place-order/fill.yaml")
		for _, run := range []struct {
			goEnv string
			args  []string
		}{{goEnv, []string{"test"}}, {"", []string{"test", "--", "-count=0"}}} {
			stdout, stderr, err := stegoWithGoEnvFile(t, p, run.goEnv, run.args...)
			if err == nil || !bytes.Equal(before, p.read(t, "fills/place-order/fill.yaml")) {
				t.Fatalf("stego test %v with the go env file %q passed a failing test or wrote evidence: %v\n%s\n%s", run.args, run.goEnv, err, stdout, stderr)
			}
		}
		if err := os.Remove(p.path("fills/place-order/fail_test.go")); err != nil {
			t.Fatal(err)
		}

		// A test file that imports a module that go.mod does not hold fails: stego test fetches no module for it.
		p.write(t, "fills/place-order/quote_test.go", "package placeorder\n\nimport (\n\t\"testing\"\n\n\t\"rsc.io/quote\"\n)\n\nfunc TestQuote(t *testing.T) { _ = quote.Go() }\n")
		if stdout, stderr, err := p.stego(t, "no-key", "test"); err == nil || !strings.Contains(stdout, "fill place-order: no evidence recorded") {
			t.Fatalf("stego test passed a test that imports a module that go.mod does not hold: %v\n%s\n%s", err, stdout, stderr)
		}
		if err := os.Remove(p.path("fills/place-order/quote_test.go")); err != nil {
			t.Fatal(err)
		}

		// A fill of the query slot that names a collection is refused, naming the field.
		p.replace(t, "fills/open-orders/fill.yaml", "implements: query.query\n", "implements: query.query\ncollection: warehouses\n")
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
		probeContractDigest(t)
	})
}

// probeContractDigest holds the contract digest to the parts of a contract that a declaration alone does not show: the
// place of a constant in a group with iota, an init function, the keyword of a declaration and the build constraints
// of a file. It computes the digest of a fill that uses PlaceOrderFill, Second and Limit over a contract package in
// memory.
func probeContractDigest(t *testing.T) {
	t.Helper()
	project := t.TempDir()
	const contract = "example.com/probe/out/commands"
	putFile(t, project, "fills/probe/fill.go", "package probe\n\nimport commands \""+contract+"\"\n\nvar _ commands.PlaceOrderFill\n\nvar _ = commands.Limit + commands.Second\n")
	base := "package commands\n\n// PlaceOrderFill is the fill.\ntype PlaceOrderFill func() error\n\nconst (\n\tFirst = iota\n\tSecond\n)\n\nconst Limit = 1\n\nvar registered []string\n\nfunc init() { registered = append(registered, \"place-order\") }\n\nvar _ = \"one\"\n\nvar _ = \"two\"\n"
	digest := func(source string) string {
		t.Helper()
		check := compiler.FillCheck{ProjectDir: project, Module: "example.com/probe",
			Bindings:     []compiler.FillBinding{{Fill: "probe", Slot: "command", Path: "commands[0].fill", Owner: "command place-order", GoName: "PlaceOrder"}},
			CommandFills: []compiler.CommandFill{{Fill: "probe", Command: "place-order", Path: "commands[0].fill", Contract: contract}},
			CapabilityContract: func(string) (map[string][]byte, error) {
				return map[string][]byte{"out/commands/commands.go": []byte(source)}, nil
			}}
		value, err := fill.CapabilityContractDigest(check, "probe", types.SlotRef{Component: "command", Slot: "command"})
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := digest(base)
	for change, source := range map[string]string{
		"a constant inserted before Second in its iota group": strings.Replace(base, "\tFirst = iota\n", "\tFirst = iota\n\tThird\n", 1),
		"a change to the body of init":                        strings.Replace(base, `"place-order")`, `"other")`, 1),
		"a constant that becomes a variable":                  strings.Replace(base, "const Limit = 1", "var Limit = 1", 1),
		"a build constraint on the file":                      "//go:build linux\n\n" + base,
		"a change to the first of two blank declarations":     strings.Replace(base, `var _ = "one"`, `var _ = "uno"`, 1),
		"a change to the second of two blank declarations":    strings.Replace(base, `var _ = "two"`, `var _ = "dos"`, 1),
	} {
		if source == base {
			t.Fatalf("%s: the probe does not change the contract", change)
		}
		if digest(source) == before {
			t.Errorf("%s: the contract digest did not change", change)
		}
	}
}

func TestBuildRefusesUnqualifiedFill(t *testing.T) {
	t.Run("ST-EXT-UC02-R02-T06 A fill written after the apply of its contract does not reach a build until it is qualified", func(t *testing.T) {
		walkthroughProject(t)
		for name, f := range walkthroughFills {
			writeProjectFile(t, "fills/"+name+"/fill.yaml", "kind: fill\nname: "+name+"\nimplements: "+f.implements+"\n")
		}
		// Fills that hold no Go code do not stop apply, which writes their contracts and names what to do next.
		if printed, err := capture(t, runApply); err == nil || !strings.Contains(err.Error(), "holds no Go code yet") ||
			!strings.Contains(err.Error(), "then run stego test, stego fill sign place-order and stego apply") {
			t.Fatalf("apply did not name the fills to write and what to run next: %v\n%s", err, printed)
		}
		// The author writes the fills and builds without stego test or stego fill sign.
		writeWalkthroughFills(t)
		project, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		git(t, project, "init", "-q")
		build := func() error {
			t.Helper()
			git(t, project, "add", "-A")
			git(t, project, "commit", "-q", "--allow-empty", "-m", "change")
			revision := strings.TrimSpace(git(t, project, "rev-parse", "HEAD"))
			root := t.TempDir()
			cache := filepath.Join(root, "cache")
			if err := os.Mkdir(cache, 0o700); err != nil {
				t.Fatal(err)
			}
			_, err := capture(t, runBuild, "--source", project, "--revision", revision, "--target", "out", "--go", pinnedGo(t),
				"--work", filepath.Join(root, "work"), "--output", filepath.Join(root, "result"), "--module-cache", cache)
			if err != nil && strings.Contains(err.Error(), "binds fills that are not qualified") {
				for _, folder := range []string{"work", "result"} {
					if _, statErr := os.Stat(filepath.Join(root, folder)); statErr == nil {
						t.Errorf("the refused build wrote %s", folder)
					}
				}
			}
			return err
		}
		if err := build(); err == nil || !strings.Contains(err.Error(), `fill "place-order" is untested`) || !strings.Contains(err.Error(), "run stego test, then stego fill sign place-order") {
			t.Fatalf("stego build did not refuse the unqualified fill and name what to run: %v", err)
		}
		// stego deps refuses the fill too.
		if _, err := capture(t, runDependencies); err == nil || !strings.Contains(err.Error(), "run stego test, then stego fill sign place-order") {
			t.Fatalf("stego deps did not refuse the unqualified fill: %v", err)
		}

		// Once the fills are qualified, the fill check of the build passes; the build then stops at the empty module
		// cache, which is no fill fault.
		qualifyInProcess(t)
		mustRun(t, runApply)
		if err := build(); err == nil || strings.Contains(err.Error(), "not qualified") {
			t.Fatalf("stego build refused qualified fills, or built from an empty module cache: %v", err)
		}

		// A sign-off by a key that SIGNERS does not list stops the build. The fill holds a current sign-off by a listed
		// key, which fill sign keeps, so the unlisted key signs over it with --force.
		keys := testFillKeys(t)
		t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(keys.homes["unlisted-ssh"], ".gitconfig"))
		mustRun(t, runFillSign, "--force", "place-order")
		if err := build(); err == nil || !strings.Contains(err.Error(), "SIGNERS does not list its key") {
			t.Fatalf("stego build did not refuse the sign-off of an unlisted key: %v", err)
		}
		qualifyInProcess(t)

		// A fill signed against an older contract stops the build: its Go files go, the command changes, apply
		// writes the new contract and passes the fill that holds no code, and the same files come back.
		saved := map[string][]byte{}
		for _, name := range []string{"fill.go", "fill_test.go"} {
			data, err := os.ReadFile(filepath.Join("fills", "place-order", name))
			if err != nil {
				t.Fatal(err)
			}
			saved[name] = data
			if err := os.Remove(filepath.Join("fills", "place-order", name)); err != nil {
				t.Fatal(err)
			}
		}
		declaration, err := os.ReadFile("service.yaml")
		if err != nil {
			t.Fatal(err)
		}
		changed := strings.Replace(string(declaration), "{name: not_draft, code: order.not_draft, status: 409}", "{name: not_draft, code: order.not_draft, status: 422}", 1)
		if changed == string(declaration) {
			t.Fatal("the declaration holds no refusal not_draft")
		}
		writeProjectFile(t, "service.yaml", changed)
		if _, err := capture(t, runApply); err == nil || !strings.Contains(err.Error(), "holds no Go code yet") {
			t.Fatalf("apply did not pass the fill that holds no code: %v", err)
		}
		for name, data := range saved {
			writeProjectFile(t, filepath.Join("fills", "place-order", name), string(data))
		}
		if err := build(); err == nil || !strings.Contains(err.Error(), "signed against an older contract") || !strings.Contains(err.Error(), "run stego test, then stego fill sign place-order") {
			t.Fatalf("stego build did not refuse the fill signed against an older contract: %v", err)
		}
	})
}

// stegoWithGoEnvFile runs the stego binary in the project as the holder of no key, as stegoWith does, with the go env
// file goEnv as GOENV when it is not "": a file of the person that a go command reads unless GOENV is off. It is the
// one run of stego whose environment sets a go setting after localtest.GoEnv, which innerGoExempt of
// internal/assure names: stego sets GOENV=off for each go command that it runs (internal/fill moduleSources), and
// the test shows that it does.
func stegoWithGoEnvFile(t *testing.T, p *fillProject, goEnv string, args ...string) (string, string, error) {
	t.Helper()
	env := keyEnv(p.keys, p.keys.homes["no-key"])
	if p.registry != "" {
		env = append(env, "STEGO_REGISTRY="+p.registry)
	}
	if goEnv != "" {
		env = append(env, "GOENV="+goEnv)
	}
	command := exec.Command(stegoBinary(t), args...)
	command.Dir, command.Env = p.dir, env
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	return stdout.String(), stderr.String(), err
}
