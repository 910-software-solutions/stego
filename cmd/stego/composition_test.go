package main

import (
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// generatedMigrations gives each file under a folder named migrations of the generated output of the project, by
// its path from the project, with its content.
func generatedMigrations(t *testing.T, project string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(filepath.Join(project, "out"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || filepath.Base(filepath.Dir(path)) != "migrations" {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(project, path)
		files[relative] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files[filepath.Join("out", "internal", "storage", "migrations", "001_initial.sql")]; !ok {
		t.Fatalf("stego apply generated no 001_initial.sql: %v", slices.Sorted(maps.Keys(files)))
	}
	return files
}

// provisionerController is the controller provisioner of the composition tests (sources scan and watch, resync 10m,
// replicas 2, lease TTL 15s, tenants each), with the field given left out.
func provisionerController(without string) string {
	var kept []string
	for _, field := range []string{"name: provisioner", "sources: [scan, watch]", "resync: 10m", "replicas: 2", "lease_ttl: 15s", "tenants: each"} {
		if !strings.HasPrefix(field, without+":") {
			kept = append(kept, field)
		}
	}
	return "controllers:\n  - {" + strings.Join(kept, ", ") + "}\n"
}

// td02WithTimes is ST-DECL-TD02 with the two time fields starts_at and ends_at, which bound the range of a
// no_overlap invariant, and the field owner_id, which scopes it.
var td02WithTimes = strings.Replace(td02, "      - {name: done, type: bool, default: false}\n",
	"      - {name: done, type: bool, default: false}\n      - {name: starts_at, type: timestamp}\n      - {name: ends_at, type: timestamp}\n      - {name: owner_id, type: string}\n", 1)

func TestDeclareComposition(t *testing.T) {
	t.Parallel()
	t.Run("ST-DECL-UC09-R01-T01 Modes that are a subset of api, worker and controller are accepted", func(t *testing.T) {
		// ST-DECL-TD02 is on the runtime data path, so it needs its three modes (ST-DECL-INV89); they are accepted in
		// any order.
		for _, modes := range []string{"[api, worker, controller]", "[controller, api, worker]"} {
			document, run := validateJSON(t, td02Project(t, td02+"modes: "+modes+"\n"))
			if run.code != 0 || !document.Accepted || len(document.Faults) != 0 {
				t.Errorf("the modes %s were refused: exit %d\n%s%s", modes, run.code, run.stdout, run.stderr)
			}
		}
	})
	t.Run("ST-DECL-UC09-R01-T03 An unknown mode is refused, naming the mode", func(t *testing.T) {
		fault := refusedFault(t, td02+"modes: [api, batch]\n", "modes[1]")
		if fault.Found != "batch" || !slices.Equal(fault.Allowed, []string{"api", "worker", "controller"}) || !strings.Contains(fault.Message, `"batch"`) {
			t.Errorf("want a fault that names batch and allows api, worker and controller, got %+v", fault)
		}
	})

	t.Run("ST-DECL-UC09-R07-T01 A service selects its generated clients by their mixins, and a client mixin that the archetype does not list is refused", func(t *testing.T) {
		project := td02Project(t, td02+"mixins: [go-sdk, cli-application]\n")
		document, run := validateJSON(t, project)
		if run.code != 0 || !document.Accepted || len(document.Faults) != 0 {
			t.Fatalf("the client mixins were refused: exit %d\n%s%s", run.code, run.stdout, run.stderr)
		}
		if apply := stegoIn(t, project, nil, "apply"); apply.code != 0 {
			t.Fatalf("stego apply of the client mixins: %s%s", apply.stdout, apply.stderr)
		}
		for _, name := range []string{"out/sdk/api.go", "out/cli/app/commands.go", "out/cli/cmd/main.go"} {
			if _, err := os.Stat(filepath.Join(project, name)); err != nil {
				t.Errorf("the generated client file %s is not there: %v", name, err)
			}
		}
		// The settings of a client are accepted under overrides only with its mixin.
		if !strings.Contains(td02, "\noverrides:\n") {
			t.Fatal("ST-DECL-TD02 has no overrides")
		}
		refused := stegoIn(t, td02Project(t, strings.Replace(td02, "\noverrides:\n", "\noverrides:\n  go-sdk: {document: api.yaml}\n", 1)), nil, "validate")
		if refused.code == 0 || !strings.Contains(refused.stdout+refused.stderr, "overrides.go-sdk") {
			t.Errorf("overrides.go-sdk with no mixin was accepted: %s%s", refused.stdout, refused.stderr)
		}
		// A registry whose rest-crud does not list go-sdk refuses the mixin and names the mixins that it lists.
		source, _ := declaringRegistry(t)
		registry := filepath.Join(t.TempDir(), "registry")
		if output, err := exec.Command("cp", "-R", source, registry).CombinedOutput(); err != nil {
			t.Fatalf("copy the registry: %v\n%s", err, output)
		}
		archetype := filepath.Join(registry, "archetypes", "rest-crud", "archetype.yaml")
		data, err := os.ReadFile(archetype)
		if err != nil || !strings.Contains(string(data), "\n  - go-sdk\n") {
			t.Fatalf("the rest-crud archetype does not list go-sdk: %v", err)
		}
		if err := os.WriteFile(archetype, []byte(strings.Replace(string(data), "\n  - go-sdk\n", "\n", 1)), 0o644); err != nil {
			t.Fatal(err)
		}
		other := stegoIn(t, td02Project(t, td02+"mixins: [go-sdk]\n"), []string{"STEGO_REGISTRY=" + registry}, "validate")
		if text := other.stdout + other.stderr; other.code == 0 || !strings.Contains(text, `mixin "go-sdk" is not compatible`) || !strings.Contains(text, "cli-application") {
			t.Errorf("a client mixin that the archetype does not list was not refused with the mixins it lists: %s", text)
		}
	})

	t.Run("ST-DECL-UC09-R02-T01 kafka-events without durable-events is refused, naming the missing mixin", func(t *testing.T) {
		refusedNaming(t, td02+"mixins: [kafka-events]\n", "mixins[0]", "kafka-events", "durable-events")
	})

	t.Run("ST-DECL-UC09-R02-T03 event-publisher is accepted as a deprecated alias, with a warning", func(t *testing.T) {
		project := td02Project(t, td02+"mixins: [event-publisher]\n")
		document, run := validateJSON(t, project)
		if run.code != 0 || !document.Accepted || len(document.Faults) != 0 {
			t.Fatalf("the mixin event-publisher was refused: exit %d\n%s%s", run.code, run.stdout, run.stderr)
		}
		if len(document.Warnings) != 1 {
			t.Fatalf("want one warning, got %+v", document.Warnings)
		}
		warning := document.Warnings[0]
		for _, text := range []string{"event-publisher", "deprecated alias", "durable-events", "kafka-events"} {
			if !strings.Contains(warning.Message, text) {
				t.Errorf("the warning does not name %s: %+v", text, warning)
			}
		}
		if warning.Path != "mixins[0]" || warning.Found != "event-publisher" || warning.Rule != "deprecated-alias" {
			t.Errorf("want the warning at mixins[0] with the rule deprecated-alias, got %+v", warning)
		}
		// A person reads the same warning after the faults.
		if prose := stegoIn(t, project, nil, "validate"); prose.code != 0 || !strings.Contains(prose.stdout, "1 warning(s)") || !strings.Contains(prose.stdout, "[deprecated-alias] mixins[0]") {
			t.Errorf("the prose of stego validate gives no warning: exit %d\n%s", prose.code, prose.stdout)
		}
		// A declaration with no alias gets no warning.
		if plain, _ := validateJSON(t, td02Project(t, td02)); plain.Warnings == nil || len(plain.Warnings) != 0 {
			t.Errorf("want an empty list of warnings for ST-DECL-TD02, got %+v", plain.Warnings)
		}
	})

	t.Run("ST-DECL-UC09-R03-T01 A controller with no lease TTL is refused, naming the controller and the field", func(t *testing.T) {
		refusedNaming(t, td02+provisionerController("lease_ttl"), "controllers[0].lease_ttl", `"provisioner"`, "lease TTL")
	})
	t.Run("ST-DECL-UC09-R03-T02 A controller with no sources is refused, naming the controller and the field", func(t *testing.T) {
		refusedNaming(t, td02+provisionerController("sources"), "controllers[0].sources", `"provisioner"`, "sources")
	})
	t.Run("ST-DECL-UC09-R03-T03 A controller with no resync is refused, naming the controller and the field", func(t *testing.T) {
		refusedNaming(t, td02+provisionerController("resync"), "controllers[0].resync", `"provisioner"`, "resync")
	})
	t.Run("ST-DECL-UC09-R03-T04 A controller with no replicas is refused, naming the controller and the field", func(t *testing.T) {
		refusedNaming(t, td02+provisionerController("replicas"), "controllers[0].replicas", `"provisioner"`, "replicas")
	})

	t.Run("ST-DECL-UC09-R04-T01 A declared invariant of each known kind with its fields is accepted", func(t *testing.T) {
		service := td02WithTimes + `invariants:
  - {name: unique_open_title, kind: unique, on: Todo, by: [title], where: done = false}
  - {name: no_double_booking, kind: no_overlap, on: Todo, range: 'starts_at, ends_at', by: [owner_id]}
  - {name: within_budget, kind: sum_limit, on: Todo, parent: budget.limit}
`
		document, run := validateJSON(t, td02Project(t, service))
		if run.code != 0 || !document.Accepted || len(document.Faults) != 0 {
			t.Fatalf("the three declared invariants were refused: exit %d\n%s%s", run.code, run.stdout, run.stderr)
		}
	})
	t.Run("ST-DECL-UC09-R04-T02 A declared invariant of another kind is refused, naming the invariant and the kind", func(t *testing.T) {
		service := td02 + "invariants:\n  - {name: one_open, kind: exclusive, on: Todo}\n"
		refusedNaming(t, service, "invariants[0].kind", `"one_open"`, `"exclusive"`)
		if fault := refusedFault(t, service, "invariants[0].kind"); !slices.Equal(fault.Allowed, []string{"unique", "no_overlap", "sum_limit"}) {
			t.Errorf("want unique, no_overlap and sum_limit allowed, got %+v", fault)
		}
	})
	t.Run("ST-DECL-UC09-R04-T03 A declared invariant that lacks a field its kind needs is refused, naming the invariant and the field", func(t *testing.T) {
		for _, c := range []struct{ invariant, field string }{
			{"{name: unique_title, kind: unique, on: Todo}", "by"},
			{"{name: no_double_booking, kind: no_overlap, on: Todo, by: [owner_id]}", "range"},
			{"{name: within_budget, kind: sum_limit, on: Todo}", "parent"},
		} {
			name := strings.TrimPrefix(strings.Split(c.invariant, ",")[0], "{name: ")
			refusedNaming(t, td02WithTimes+"invariants:\n  - "+c.invariant+"\n", "invariants[0]."+c.field, `"`+name+`"`, "needs "+c.field)
		}
	})

	t.Run("ST-DECL-UC09-R04-T04 A rule with no known emission stays a slot, and generation emits no store constraint for it", func(t *testing.T) {
		plain := td02Project(t, td02)
		putFile(t, plain, "go.mod", "module example.com/fills\n\ngo 1.26.11\n")
		bound := td02Project(t, td02+"slots:\n  - collection: todos\n    slot: before_create\n    gate: [policy]\n")
		putQualifiedPolicy(t, bound)
		for _, project := range []string{plain, bound} {
			for _, command := range []string{"validate", "apply"} {
				if run := stegoIn(t, project, nil, command); run.code != 0 {
					t.Fatalf("stego %s exited %d:\n%s%s", command, run.code, run.stdout, run.stderr)
				}
			}
		}
		want, got := generatedMigrations(t, plain), generatedMigrations(t, bound)
		if len(got) != len(want) {
			t.Errorf("the binding changes the migration files: want %d, got %d", len(want), len(got))
		}
		for name, content := range want {
			if got[name] != content {
				t.Errorf("the binding changes the migration %s", name)
			}
		}
	})
}
