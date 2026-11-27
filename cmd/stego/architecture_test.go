package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/910-software-solutions/stego/internal/compiler"
	"github.com/910-software-solutions/stego/internal/registry"
)

// These tests prove that apply writes the architecture model architecture/architecture.json with the code
// (ST-GEN-UC08), on the service orders of ST-DLV-TD01, whose components state a need of each kind.

// readArchitecture reads the model that apply wrote, checks it against the schema, and gives it decoded both ways.
func readArchitecture(t *testing.T, dir string) (compiler.ArchitectureModel, any, []byte) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, compiler.ArchitectureFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := compiler.ValidateArchitecture(data); err != nil {
		t.Fatalf("the written model is not valid: %v", err)
	}
	var model compiler.ArchitectureModel
	var tree any
	if err := json.Unmarshal(data, &model); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &tree); err != nil {
		t.Fatal(err)
	}
	return model, tree, data
}

func TestArchitectureApply(t *testing.T) {
	t.Parallel()
	t.Run("ST-GEN-UC08-R01-T01 Apply writes an architecture model with the five aspects of the service, and the record lists it", func(t *testing.T) {
		registryDir := requirementsRegistry(t)
		dir := requirementsProject(t)
		mustRunInProject(t, dir, requirementsEnv(registryDir), "apply")
		model, _, data := readArchitecture(t, dir)
		state, err := compiler.LoadState(filepath.Join(dir, ".stego", "state.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if state.LastApplied.ProjectFiles[compiler.ArchitectureFile] != compiler.HashBytes(data) {
			t.Fatalf("the record does not list %s with its hash", compiler.ArchitectureFile)
		}
		for name, aspect := range map[string]compiler.ArchAspect{"structure": model.Aspects.Structure, "data model": model.Aspects.DataModel,
			"request flows": model.Aspects.RequestFlows, "platform edges": model.Aspects.PlatformEdges, "trust and tenancy": model.Aspects.TrustAndTenancy} {
			if aspect.State != "present" {
				t.Fatalf("the aspect %s is %+v", name, aspect)
			}
		}
		ids := map[string]bool{}
		for _, c := range model.Components {
			ids[c.ID] = true
		}
		for name := range state.LastApplied.Components {
			if !ids["component:"+name] {
				t.Fatalf("the component %s of the active set is not in the model", name)
			}
		}
		if len(model.Entities) != 1 || model.Entities[0].ID != "entity:Order" || len(model.Entities[0].Fields) != 1 || model.Entities[0].Fields[0].ID != "field:Order.item" {
			t.Fatalf("the data model is %+v", model.Entities)
		}
		var flows []string
		for _, f := range model.Flows {
			flows = append(flows, f.Operation)
		}
		if strings.Join(flows, " ") != "operation:orders.create operation:orders.list operation:orders.read" {
			t.Fatalf("the flows are %v", flows)
		}
		requirements, _ := readRequirements(t, dir)
		if len(model.Needs) != len(requirements.Requirements) || len(model.Needs) < 9 {
			t.Fatalf("the model holds %d needs for %d platform requirements", len(model.Needs), len(requirements.Requirements))
		}
		for _, r := range requirements.Requirements {
			id := "need:" + strings.ReplaceAll(string(r.Kind), " ", "-") + "." + r.Name
			found := false
			for _, n := range model.Needs {
				found = found || n.ID == id
			}
			if !found {
				t.Fatalf("the platform requirement %s %s is not a platform edge", r.Kind, r.Name)
			}
		}
	})

	t.Run("ST-GEN-UC08-R03-T01 Each element of the model names the declaration entry or registry part it comes from, and each source resolves", func(t *testing.T) {
		registryDir := requirementsRegistry(t)
		dir := requirementsProject(t)
		mustRunInProject(t, dir, requirementsEnv(registryDir), "apply")
		_, tree, _ := readArchitecture(t, dir)
		serviceData, err := os.ReadFile(filepath.Join(dir, "service.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		var service yaml.Node
		if err := yaml.Unmarshal(serviceData, &service); err != nil {
			t.Fatal(err)
		}
		reg, err := registry.Load(registryDir)
		if err != nil {
			t.Fatal(err)
		}
		rule := regexp.MustCompile(`^ST-[A-Z]+-[A-Z0-9-]+$`)
		count := 0
		eachElement(t, tree, func(id string, source map[string]any) {
			count++
			if len(source) != 1 {
				t.Fatalf("%s names %d sources", id, len(source))
			}
			switch {
			case source["declaration"] != nil:
				if path := source["declaration"].(string); !resolves(service.Content[0], path) {
					t.Fatalf("%s names the declaration entry %s, which service.yaml does not hold", id, path)
				}
			case source["registry"] != nil:
				part := source["registry"].(map[string]any)
				name, version := part["name"].(string), part["version"].(string)
				var held string
				switch part["part"] {
				case "component":
					if c := reg.Component(name); c != nil {
						held = c.Version
					}
				case "archetype":
					if a := reg.Archetype(name); a != nil {
						held = a.Version
					}
				case "mixin":
					if m := reg.Mixin(name); m != nil {
						held = m.Version
					}
				}
				if held == "" || held != version {
					t.Fatalf("%s names the registry part %s %s %s, which the registry holds at %q", id, part["part"], name, version, held)
				}
			case source["default"] != nil:
				if !rule.MatchString(source["default"].(string)) {
					t.Fatalf("%s names the default rule %v", id, source["default"])
				}
			case source["rule"] != nil:
				if !rule.MatchString(source["rule"].(string)) {
					t.Fatalf("%s names the rule %v", id, source["rule"])
				}
			case source["wiring"] != nil:
			default:
				t.Fatalf("%s names the source %v", id, source)
			}
		})
		if count < 50 {
			t.Fatalf("only %d elements name a source", count)
		}
	})

	t.Run("ST-GEN-UC08-R03-T02 The model names the record it was written with", func(t *testing.T) {
		registryDir := requirementsRegistry(t)
		dir := requirementsProject(t)
		mustRunInProject(t, dir, requirementsEnv(registryDir), "apply")
		model, _, _ := readArchitecture(t, dir)
		if model.Record.Path != ".stego/state.yaml" {
			t.Fatalf("the model names the record %q", model.Record.Path)
		}
		if _, err := os.Stat(filepath.Join(dir, model.Record.Path)); err != nil {
			t.Fatalf("the record the model names is not there: %v", err)
		}
		// A comment in service.yaml changes the record's declaration digest and not the model.
		before, _ := os.ReadFile(filepath.Join(dir, compiler.ArchitectureFile))
		appendTo(t, filepath.Join(dir, "service.yaml"), "# a comment\n")
		mustRunInProject(t, dir, requirementsEnv(registryDir), "apply")
		after, _ := os.ReadFile(filepath.Join(dir, compiler.ArchitectureFile))
		if string(before) != string(after) {
			t.Fatal("a comment in service.yaml changed the architecture model")
		}
	})

	t.Run("ST-GEN-UC08-R06-T01 An aspect with no element says so, and the others show their elements", func(t *testing.T) {
		dir := requirementsProject(t)
		// A registry whose components state no need and no environment variable, so the platform edges are empty.
		registryDir := shippedRegistryCopy(t)
		withoutVariables(t, registryDir)
		mustRunInProject(t, dir, requirementsEnv(registryDir), "apply")
		model, _, _ := readArchitecture(t, dir)
		if len(model.Variables) != 0 {
			t.Fatalf("the model holds %d variables of components that declare none", len(model.Variables))
		}
		if model.Aspects.PlatformEdges.State != "empty" || model.Aspects.PlatformEdges.Reason == "" || len(model.Needs) != 0 {
			t.Fatalf("the platform edges are %+v with %d needs", model.Aspects.PlatformEdges, len(model.Needs))
		}
		if model.Aspects.Structure.State != "present" || model.Aspects.DataModel.State != "present" || model.Aspects.RequestFlows.State != "present" || model.Aspects.TrustAndTenancy.State != "present" {
			t.Fatalf("the other aspects are %+v", model.Aspects)
		}
	})

	t.Run("ST-GEN-UC08-R05-T01 Apply refuses when a component states no layer, flow part or port side, or a slot no phase, names it, and writes nothing", func(t *testing.T) {
		for _, tc := range []struct {
			name, component, from, to string
			want                      []string
		}{
			{"no layer", "postgres-adapter", "  layer: repository\n", "", []string{"postgres-adapter", "architecture.layer"}},
			{"no flow part", "postgres-adapter", "  flow: \"Reads and writes the rows of each entity in PostgreSQL in one transaction.\"\n", "", []string{"postgres-adapter", "architecture.flow"}},
			{"no port side", "rest-api", "    storage-adapter: outbound\n", "", []string{"rest-api", "architecture.ports.storage-adapter"}},
			{"no slot phase", "rest-api", "    phase: after_store\n    operations: [create]\n", "    operations: [create]\n", []string{"rest-api", "slots[after_create].phase"}},
		} {
			registryDir := requirementsRegistry(t)
			path := filepath.Join(registryDir, "components", tc.component, "component.yaml")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			changed := strings.Replace(string(data), tc.from, tc.to, 1)
			if changed == string(data) {
				t.Fatalf("%s: %s holds no %q", tc.name, tc.component, tc.from)
			}
			if err := os.WriteFile(path, []byte(changed), 0o644); err != nil {
				t.Fatal(err)
			}
			dir := requirementsProject(t)
			before := folderDigests(t, dir)
			_, stderr, err := runInProject(t, dir, requirementsEnv(registryDir), "apply")
			if err == nil {
				t.Fatalf("%s: apply accepted the registry", tc.name)
			}
			for _, part := range tc.want {
				if !strings.Contains(stderr, part) {
					t.Fatalf("%s: the refusal does not name %s:\n%s", tc.name, part, stderr)
				}
			}
			if after := folderDigests(t, dir); !maps.Equal(before, after) {
				t.Fatalf("%s: the refused apply changed the project folder", tc.name)
			}
		}
	})
}

// eachElement calls found with the ID and the source of each element of the decoded model, and fails on an element
// with no source.
func eachElement(t *testing.T, value any, found func(id string, source map[string]any)) {
	t.Helper()
	switch v := value.(type) {
	case map[string]any:
		// A compliance topic is a heading of facts, not an element.
		if id, isElement := v["id"].(string); isElement && !strings.HasPrefix(id, "topic:") {
			source, ok := v["source"].(map[string]any)
			if !ok {
				t.Fatalf("the element %s names no source", id)
			}
			found(id, source)
		}
		for key, item := range v {
			if key != "source" {
				eachElement(t, item, found)
			}
		}
	case []any:
		for _, item := range v {
			eachElement(t, item, found)
		}
	}
}

// pathPart is one part of a declaration path: a key with any number of indexes, such as fields[2].
var pathPart = regexp.MustCompile(`^([^\[\]]+)((\[\d+\])*)$`)

// resolves tells whether a declaration path, such as entities[1].fields[2] or collections.orders.operations[0],
// names an entry of the YAML mapping.
func resolves(node *yaml.Node, path string) bool {
	for _, part := range strings.Split(path, ".") {
		match := pathPart.FindStringSubmatch(part)
		if match == nil || node.Kind != yaml.MappingNode {
			return false
		}
		var next *yaml.Node
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value == match[1] {
				next = node.Content[i+1]
			}
		}
		if next == nil {
			return false
		}
		node = next
		for _, index := range regexp.MustCompile(`\d+`).FindAllString(match[2], -1) {
			n, _ := strconv.Atoi(index)
			switch {
			case node.Kind == yaml.SequenceNode && n < len(node.Content):
				node = node.Content[n]
			case node.Kind == yaml.ScalarNode && n == 0:
				// One fill name stands for a list of one.
			default:
				return false
			}
		}
	}
	return true
}

// appendTo adds text at the end of a file.
func appendTo(t *testing.T, path, text string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, []byte(text)...), 0o644); err != nil {
		t.Fatal(fmt.Errorf("appending to %s: %w", path, err))
	}
}

// withoutVariables gives each component of the registry copy an empty environment list.
func withoutVariables(t *testing.T, registryDir string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(registryDir, "components", "*", "component.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("the registry copy holds no component: %v", err)
	}
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		head, _, found := strings.Cut(string(data), "\nenvironment:")
		if !found {
			t.Fatalf("%s states no environment list", path)
		}
		if err := os.WriteFile(path, []byte(head+"\nenvironment: []\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// appliedArchitecture applies the project of ST-DLV-TD01 once, and gives a copy of it for each test of the queries
// of the architecture model (ST-GEN-QRY05, ST-GEN-QRY06), with the environment of its runs.
func appliedArchitecture(t *testing.T) (func(t *testing.T) string, []string) {
	t.Helper()
	registryDir := requirementsRegistry(t)
	env := requirementsEnv(registryDir)
	dir := requirementsProject(t)
	mustRunInProject(t, dir, env, "apply")
	return func(t *testing.T) string { return copyFolder(t, dir) }, env
}

// queryRefusals runs one query command on a project with no model, with a model changed after apply, and with a
// model that is not valid, and expects each run to exit 1 with no output and a message that names the file and the
// cause.
func queryRefusals(t *testing.T, project func(t *testing.T) string, env []string, args ...string) {
	t.Helper()
	cases := []struct {
		name, cause string
		change      func(model string)
	}{
		{"missing", "missing", func(model string) {
			if err := os.Remove(model); err != nil {
				t.Fatal(err)
			}
		}},
		{"changed after apply", "does not match the record", func(model string) {
			appendTo(t, model, " ")
		}},
		{"not valid", "schema", func(model string) {
			data, err := os.ReadFile(model)
			if err != nil {
				t.Fatal(err)
			}
			version := fmt.Sprintf(`"schema_version": %d`, compiler.ArchitectureSchemaVersion)
			invalid := strings.Replace(string(data), version, version+`, "notes": "hand-written"`, 1)
			if invalid == string(data) {
				t.Fatalf("the model has no %s", version)
			}
			if err := os.WriteFile(model, []byte(invalid), 0o644); err != nil {
				t.Fatal(err)
			}
			// The record lists the bytes of the model, so only the schema refuses it.
			state, err := compiler.LoadState(filepath.Join(filepath.Dir(filepath.Dir(model)), ".stego", "state.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			state.LastApplied.ProjectFiles[compiler.ArchitectureFile] = compiler.HashBytes([]byte(invalid))
			if err := compiler.SaveState(filepath.Join(filepath.Dir(filepath.Dir(model)), ".stego", "state.yaml"), state); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, c := range cases {
		dir := project(t)
		c.change(filepath.Join(dir, compiler.ArchitectureFile))
		before := envFolderState(t, dir)
		stdout, stderr, err := runInProject(t, dir, env, args...)
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			t.Fatalf("stego %s with a model %s: %v\n%s", strings.Join(args, " "), c.name, err, stderr)
		}
		if stdout != "" {
			t.Fatalf("stego %s with a model %s gives output:\n%s", strings.Join(args, " "), c.name, stdout)
		}
		if !strings.Contains(stderr, compiler.ArchitectureFile) || !strings.Contains(stderr, c.cause) {
			t.Fatalf("stego %s with a model %s does not name the file and the cause %q:\n%s", strings.Join(args, " "), c.name, c.cause, stderr)
		}
		if after := envFolderState(t, dir); !maps.Equal(before, after) {
			t.Fatalf("stego %s with a model %s changes the project folder", strings.Join(args, " "), c.name)
		}
	}
}

func TestArchitectureQueries(t *testing.T) {
	project, env := appliedArchitecture(t)

	t.Run("ST-GEN-UC16-R06-T03 stego architecture facts refuses a missing, changed or not valid model, names the file and the cause, and gives no fact", func(t *testing.T) {
		queryRefusals(t, project, env, "architecture", "facts", "--topic", "personal-data")
		queryRefusals(t, project, env, "architecture", "facts", "--topic", "access", "--json")
	})

	t.Run("ST-GEN-UC17-R05-T01 stego architecture export refuses a missing, changed or not valid model, names the file and the cause, and gives no output", func(t *testing.T) {
		queryRefusals(t, project, env, "architecture", "export", "--format", "structurizr")
		queryRefusals(t, project, env, "architecture", "export", "--format", "mermaid", "--view", "data-model")
	})

	t.Run("ST-GEN-UC17-R02-T01 Apply writes no export and the record lists none, and an export writes no file", func(t *testing.T) {
		dir := project(t)
		state, err := compiler.LoadState(filepath.Join(dir, ".stego", "state.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		// The files of the Kubernetes base and of its Component (ST-DLV-UC07) and the Containerfile (ST-DLV-INV07) are
		// project files too; this test looks at the others.
		var projectFiles []string
		for _, name := range slices.Sorted(maps.Keys(state.LastApplied.ProjectFiles)) {
			if !strings.HasPrefix(name, "deploy/base/") && !strings.HasPrefix(name, "deploy/components/") && name != "Containerfile" {
				projectFiles = append(projectFiles, name)
			}
		}
		if strings.Join(projectFiles, " ") != "architecture/architecture.json architecture/index.html deploy/environment-variables.md deploy/platform-requirements.yaml" {
			t.Fatalf("the record lists the project files %v, want the architecture model, the architecture view, the environment variable reference and the platform requirements", projectFiles)
		}
		entries, err := os.ReadDir(filepath.Join(dir, "architecture"))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 2 || entries[0].Name() != "architecture.json" || entries[1].Name() != "index.html" {
			t.Fatalf("apply wrote %v in architecture/", entries)
		}
		for path := range state.LastApplied.Files {
			if strings.HasSuffix(path, ".dsl") || strings.HasSuffix(path, ".mmd") || strings.Contains(path, "structurizr") || strings.Contains(path, "mermaid") {
				t.Fatalf("the record lists the export %s", path)
			}
		}
		before := envFolderState(t, dir)
		for _, format := range compiler.ExportFormats {
			mustRunInProject(t, dir, env, "architecture", "export", "--format", format)
		}
		if after := envFolderState(t, dir); !maps.Equal(before, after) {
			t.Fatal("an export changes the project folder")
		}
	})

	t.Run("ST-GEN-UC17-FL-T The service author exports the architecture, finds the record in the header, and a stale model is refused", func(t *testing.T) {
		dir := project(t)
		// S01: the export gives the diagram text from the model that apply wrote.
		dsl := mustRunInProject(t, dir, env, "architecture", "export", "--format", "structurizr")
		if !strings.Contains(dsl, "workspace \"orders\"") || !strings.Contains(dsl, "= component ") {
			t.Fatalf("the export is not the workspace of orders:\n%s", dsl)
		}
		flow := mustRunInProject(t, dir, env, "architecture", "export", "--format", "mermaid", "--view", "flow:orders.create")
		if !strings.Contains(flow, "\nsequenceDiagram\n") || !strings.Contains(flow, "participant p1 as operation:orders.create") {
			t.Fatalf("the Mermaid view of the flow of orders.create:\n%s", flow)
		}
		// S02: the header comment names the record that the model was written with.
		data, err := os.ReadFile(filepath.Join(dir, compiler.ArchitectureFile))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range []string{"%% Record: .stego/state.yaml\n", "%% Model: architecture/architecture.json sha256:" + compiler.HashBytes(data) + "\n"} {
			if !strings.HasPrefix(flow, "%% ") || !strings.Contains(flow, line) {
				t.Fatalf("the header does not hold %q:\n%s", line, flow)
			}
		}
		// S03: a model that no longer matches the record is refused, with no output.
		appendTo(t, filepath.Join(dir, compiler.ArchitectureFile), "\n")
		stdout, stderr, err := runInProject(t, dir, env, "architecture", "export", "--format", "structurizr")
		if err == nil || stdout != "" || !strings.Contains(stderr, compiler.ArchitectureFile) || !strings.Contains(stderr, "does not match the record") {
			t.Fatalf("a stale model is not refused: %v\n%s\n%s", err, stdout, stderr)
		}
	})

	t.Run("ST-GEN-UC16-FL-T The service author lists the facts of a topic with their sources, and a stale model is refused", func(t *testing.T) {
		dir := project(t)
		before := envFolderState(t, dir)
		// S01 and S02: the facts of each topic, from the model that apply wrote, each with its source.
		for _, topic := range compiler.ComplianceTopics {
			text := mustRunInProject(t, dir, env, "architecture", "facts", "--topic", topic)
			var list compiler.FactList
			if err := json.Unmarshal([]byte(mustRunInProject(t, dir, env, "architecture", "facts", "--topic", topic, "--json")), &list); err != nil || list.Topic != topic {
				t.Fatalf("the JSON facts of %s: %v %+v", topic, err, list)
			}
			sources := strings.Count(text, "\n    source: ") + strings.Count(text, "\n    default, set by the rule ")
			if !strings.HasPrefix(text, "Facts of the topic "+topic+" for the service orders\n") || sources < len(list.Facts) ||
				(len(list.Facts) == 0) != strings.Contains(text, "The model holds no fact of this topic.") {
				t.Fatalf("the facts of %s for %d facts of the JSON:\n%s", topic, len(list.Facts), text)
			}
		}
		text := mustRunInProject(t, dir, env, "architecture", "facts", "--topic", "data-protection")
		if !strings.Contains(text, "encryption at rest: unstated. The inputs do not state it.") {
			t.Fatalf("encryption at rest is not said to be unstated:\n%s", text)
		}
		if after := envFolderState(t, dir); !maps.Equal(before, after) {
			t.Fatal("a list of facts changes the project folder")
		}
		// A topic that is not known is a usage error that names the topics.
		_, stderr, err := runInProject(t, dir, env, "architecture", "facts", "--topic", "everything")
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 2 || !strings.Contains(stderr, "personal-data") {
			t.Fatalf("an unknown topic: %v\n%s", err, stderr)
		}
		// S03: a model that is not there is refused, with no fact.
		if err := os.Remove(filepath.Join(dir, compiler.ArchitectureFile)); err != nil {
			t.Fatal(err)
		}
		stdout, stderr, err := runInProject(t, dir, env, "architecture", "facts", "--topic", "audit")
		if err == nil || stdout != "" || !strings.Contains(stderr, compiler.ArchitectureFile) || !strings.Contains(stderr, "missing") {
			t.Fatalf("a missing model is not refused: %v\n%s\n%s", err, stdout, stderr)
		}
	})
}
