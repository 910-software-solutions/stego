package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/registry"
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
