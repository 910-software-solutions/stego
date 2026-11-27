package compiler

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/910-software-solutions/stego/internal/types"
)

// These tests prove the exports of the architecture model (ST-GEN-UC17, ST-GEN-QRY05) on models that
// WriteArchitecture derives from the fixture of architecture_test.go. The output of each format is checked by a
// structural parser of the statements that the export writes.

var (
	dslLine = regexp.MustCompile(`^(?:"(?:[^"\\]|\\.)*"|[^\s"]+)`)
	dslID   = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
)

// dslTokens splits one line of Structurizr DSL into its words and quoted strings, and refuses a string that is not
// closed on its line.
func dslTokens(line string) ([]string, error) {
	var tokens []string
	line = strings.TrimSpace(line)
	for line != "" {
		token := dslLine.FindString(line)
		if token == "" || (strings.HasPrefix(token, `"`) != strings.HasSuffix(token, `"`)) || token == `"` {
			return nil, fmt.Errorf("a string is not closed: %s", line)
		}
		tokens = append(tokens, token)
		line = strings.TrimSpace(line[len(token):])
	}
	return tokens, nil
}

// structurizrCheck is a structural check of Structurizr DSL: one workspace with a model and a views block; each
// element defined once with an identifier, a kind and a quoted name; each relationship between two defined elements;
// each view on an element of the kind it takes, with a unique key; each call of a dynamic view a relationship of the
// model; every block closed. It gives the elements by kind and the number of calls of each dynamic view.
func structurizrCheck(text string) (map[string][]string, map[string]int, error) {
	elements := map[string]string{}
	byKind := map[string][]string{}
	relations := map[[2]string]bool{}
	dynamic := map[string]int{}
	keys := map[string]bool{}
	var stack []string
	current := ""
	for n, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		fail := func(format string, args ...any) error {
			return fmt.Errorf("line %d %q: %s", n+1, line, fmt.Sprintf(format, args...))
		}
		if strings.HasPrefix(line, "//") {
			if len(stack) > 0 {
				return nil, nil, fail("a header comment inside a block")
			}
			continue
		}
		tokens, err := dslTokens(line)
		if err != nil {
			return nil, nil, fail("%v", err)
		}
		if len(tokens) == 0 {
			return nil, nil, fail("an empty line")
		}
		parent := ""
		if len(stack) > 0 {
			parent = stack[len(stack)-1]
		}
		opens := tokens[len(tokens)-1] == "{"
		switch {
		case tokens[0] == "}":
			if len(tokens) != 1 || len(stack) == 0 {
				return nil, nil, fail("a brace that closes no block")
			}
			stack = stack[:len(stack)-1]
			continue
		case tokens[0] == "workspace" && parent == "" && len(tokens) == 4 && opens:
			stack = append(stack, "workspace")
		case (tokens[0] == "model" || tokens[0] == "views") && parent == "workspace" && len(tokens) == 2:
			stack = append(stack, tokens[0])
		case len(tokens) >= 4 && tokens[1] == "=" && opens:
			kinds := map[string]string{"softwareSystem": "model", "container": "softwareSystem", "component": "container"}
			if want, ok := kinds[tokens[2]]; !ok || want != parent || !dslID.MatchString(tokens[0]) || !strings.HasPrefix(tokens[3], `"`) {
				return nil, nil, fail("an element that is not defined in its place")
			}
			if elements[tokens[0]] != "" {
				return nil, nil, fail("the identifier %s is defined twice", tokens[0])
			}
			elements[tokens[0]] = tokens[2]
			byKind[tokens[2]] = append(byKind[tokens[2]], strings.Trim(tokens[3], `"`))
			stack = append(stack, tokens[2])
		case tokens[0] == "tags" && len(tokens) == 2 && (parent == "component" || parent == "container"):
		case len(tokens) == 4 && tokens[1] == "->" && (parent == "model" || parent == "dynamic"):
			from, to := tokens[0], tokens[2]
			if elements[from] == "" || elements[to] == "" || !strings.HasPrefix(tokens[3], `"`) {
				return nil, nil, fail("a relationship between elements that are not defined")
			}
			if parent == "model" {
				relations[[2]string{from, to}] = true
			} else {
				if !relations[[2]string{from, to}] {
					return nil, nil, fail("a call of a dynamic view that is no relationship of the model")
				}
				dynamic[current]++
			}
		case parent == "views" && opens && len(tokens) >= 4:
			scope := map[string]string{"container": "softwareSystem", "component": "container", "dynamic": "container"}
			if want, ok := scope[tokens[0]]; !ok || elements[tokens[1]] != want || keys[tokens[2]] {
				return nil, nil, fail("a view on an element that it does not take, or a key given twice")
			}
			keys[tokens[2]] = true
			current = strings.Trim(tokens[2], `"`)
			if tokens[0] == "dynamic" {
				dynamic[current] = 0
			}
			stack = append(stack, tokens[0])
		case strings.Join(tokens, " ") == "include *" && (parent == "container" || parent == "component"):
		case strings.Join(tokens, " ") == "autolayout lr" && (parent == "container" || parent == "component" || parent == "dynamic"):
		default:
			return nil, nil, fail("a statement that is not Structurizr DSL here")
		}
		if opens && len(stack) == 0 {
			return nil, nil, fail("a block that opens nothing")
		}
	}
	if len(stack) != 0 {
		return nil, nil, fmt.Errorf("the blocks %v are not closed", stack)
	}
	if len(byKind["softwareSystem"]) != 1 {
		return nil, nil, fmt.Errorf("the workspace has %d software systems", len(byKind["softwareSystem"]))
	}
	return byKind, dynamic, nil
}

var (
	mermaidAttribute = regexp.MustCompile(`^        [A-Za-z_][A-Za-z0-9_]* [A-Za-z_][A-Za-z0-9_]*( FK)? "[^"]*"$`)
	mermaidRelation  = regexp.MustCompile(`^    ([A-Za-z0-9_]+) \}o--(\|\||o\|) ([A-Za-z0-9_]+) : "[^"]*"$`)
	mermaidPart      = regexp.MustCompile(`^    participant (p[0-9]+) as [^;#"]+$`)
	mermaidMessage   = regexp.MustCompile(`^    (p[0-9]+)(->>|-\))(p[0-9]+): [^;"]+$`)
	mermaidNote      = regexp.MustCompile(`^    Note over (p[0-9]+): [^;"]+$`)
	mermaidCode      = regexp.MustCompile(`#[0-9]+;`)
)

// mermaidCheck is a structural check of one Mermaid diagram: the header comments, then an erDiagram of entity blocks
// and relationships between entities it defines, or a sequenceDiagram of participants, and messages and notes between
// the participants it declares; no ; or # outside an entity code. It gives the kind and its entities or messages.
func mermaidCheck(text string) (string, []string, error) {
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	i := 0
	for i < len(lines) && strings.HasPrefix(lines[i], "%% ") {
		i++
	}
	if i == 0 || i >= len(lines) {
		return "", nil, fmt.Errorf("no header comment or no diagram:\n%s", text)
	}
	kind := lines[i]
	declared := map[string]bool{}
	var items []string
	inEntity := false
	for _, line := range lines[i+1:] {
		bare := mermaidCode.ReplaceAllString(line, "")
		fail := fmt.Errorf("a statement that is not %s here: %q", kind, line)
		switch kind {
		case "erDiagram":
			switch {
			case inEntity && line == "    }":
				inEntity = false
			case inEntity && mermaidAttribute.MatchString(line):
			case !inEntity && strings.HasSuffix(line, " {") && dslID.MatchString(strings.TrimSpace(strings.TrimSuffix(line, " {"))):
				name := strings.TrimSpace(strings.TrimSuffix(line, " {"))
				declared[name], inEntity = true, true
				items = append(items, name)
			case !inEntity && mermaidRelation.MatchString(line):
				match := mermaidRelation.FindStringSubmatch(line)
				if !declared[match[1]] || !declared[match[3]] {
					return "", nil, fmt.Errorf("a relationship of an entity that is not defined: %q", line)
				}
			default:
				return "", nil, fail
			}
		case "sequenceDiagram":
			switch {
			case mermaidPart.MatchString(bare):
				declared[mermaidPart.FindStringSubmatch(bare)[1]] = true
			case mermaidMessage.MatchString(bare) && !strings.Contains(bare, "#"):
				match := mermaidMessage.FindStringSubmatch(bare)
				if !declared[match[1]] || !declared[match[3]] {
					return "", nil, fmt.Errorf("a message of a participant that is not declared: %q", line)
				}
				items = append(items, line)
			case mermaidNote.MatchString(bare) && !strings.Contains(bare, "#") && declared[mermaidNote.FindStringSubmatch(bare)[1]]:
				items = append(items, line)
			default:
				return "", nil, fail
			}
		default:
			return "", nil, fmt.Errorf("the diagram %q is not an erDiagram or a sequenceDiagram", kind)
		}
	}
	if inEntity {
		return "", nil, fmt.Errorf("an entity block is not closed")
	}
	return kind, items, nil
}

// mermaidBlocks splits the Markdown of all the views into the text of each diagram.
func mermaidBlocks(t *testing.T, text string) []string {
	t.Helper()
	var blocks []string
	for _, part := range strings.Split(text, "```mermaid\n")[1:] {
		end := strings.Index(part, "```\n")
		if end < 0 {
			t.Fatalf("a Mermaid block is not closed:\n%s", text)
		}
		blocks = append(blocks, part[:end])
	}
	if !strings.HasPrefix(text, "```mermaid\n") || len(blocks) == 0 {
		t.Fatalf("the Mermaid export is not Markdown blocks:\n%s", text)
	}
	return blocks
}

// exportOf exports the project in a format and view, and fails the test on a refusal.
func exportOf(t *testing.T, project, format, view string) string {
	t.Helper()
	data, err := ExportArchitecture(project, format, view)
	if err != nil {
		t.Fatalf("the %s export of the view %q: %v", format, view, err)
	}
	return string(data)
}

func TestArchitectureExport(t *testing.T) {
	t.Parallel()
	t.Run("ST-GEN-UC17-R01-T01 The export gives Structurizr DSL and Mermaid entity-relationship and sequence diagrams that parse, from the model alone", func(t *testing.T) {
		in := archFixture(t)
		in.Service.Entities = append(in.Service.Entities, types.Entity{Name: "Note", Fields: []types.Field{{Name: "doc", Type: "ref", To: "Doc"}, {Name: "text", Type: "string", Optional: true}}})
		model, data := deriveFixture(t, in)
		project := archProject(t, data)
		dsl := exportOf(t, project, "structurizr", "")
		byKind, dynamic, err := structurizrCheck(dsl)
		if err != nil {
			t.Fatalf("the Structurizr DSL does not parse: %v\n%s", err, dsl)
		}
		if len(byKind["component"]) != len(model.Components)+len(model.Fills) || len(byKind["container"]) != len(model.Containers) {
			t.Fatalf("the workspace holds %v for %d components, %d fills and %d containers", byKind, len(model.Components), len(model.Fills), len(model.Containers))
		}
		if len(dynamic) != len(model.Flows) || !strings.Contains(dsl, `container system_docs "containers"`) || !strings.Contains(dsl, `component container_docs "components"`) {
			t.Fatalf("the views are not one per flow with the container and component views: %v\n%s", dynamic, dsl)
		}
		for key, calls := range dynamic {
			if calls == 0 {
				t.Fatalf("the dynamic view %s has no call:\n%s", key, dsl)
			}
		}
		blocks := mermaidBlocks(t, exportOf(t, project, "mermaid", ""))
		if len(blocks) != 1+len(model.Flows) {
			t.Fatalf("the Mermaid export has %d diagrams for %d flows", len(blocks), len(model.Flows))
		}
		for i, block := range blocks {
			kind, items, err := mermaidCheck(block)
			if err != nil {
				t.Fatalf("a Mermaid diagram does not parse: %v\n%s", err, block)
			}
			if i == 0 && (kind != "erDiagram" || strings.Join(items, " ") != "Doc Note" || !strings.Contains(block, "\n    Note }o--|| Doc : \"doc\"\n")) {
				t.Fatalf("the first diagram is %s of %v", kind, items)
			}
			if i > 0 && (kind != "sequenceDiagram" || len(items) < len(model.Flows[i-1].Steps)) {
				t.Fatalf("the diagram of %s is %s with %d steps for %d", model.Flows[i-1].ID, kind, len(items), len(model.Flows[i-1].Steps))
			}
		}
		if strings.Contains(dsl, "C4") || strings.Contains(strings.Join(blocks, ""), "C4") {
			t.Fatalf("an export holds a Mermaid C4 diagram")
		}
		// A change of the model alone changes the export.
		in = archFixture(t)
		in.Components["auth"].Architecture.Flow = "Checks the badge of the caller."
		_, changed := deriveFixture(t, in)
		other := exportOf(t, archProject(t, changed), "structurizr", "")
		if other == dsl || !strings.Contains(other, "Checks the badge of the caller.") {
			t.Fatalf("the export does not follow the model:\n%s", other)
		}
		for _, view := range []string{"flow:docs.create", "containers"} {
			one := exportOf(t, project, "structurizr", view)
			if _, dynamic, err := structurizrCheck(one); err != nil || (view == "containers") != (len(dynamic) == 0) {
				t.Fatalf("the view %s: %v %v\n%s", view, err, dynamic, one)
			}
		}
		single := exportOf(t, project, "mermaid", "flow:docs.create")
		if kind, _, err := mermaidCheck(single); err != nil || kind != "sequenceDiagram" || strings.Contains(single, "```") {
			t.Fatalf("the Mermaid view of one flow: %v\n%s", err, single)
		}
		for _, wrong := range [][2]string{{"plantuml", ""}, {"mermaid", "components"}, {"structurizr", "data-model"}, {"structurizr", "flow:nothing"}} {
			if _, err := ExportArchitecture(project, wrong[0], wrong[1]); err == nil {
				t.Fatalf("the format %s with the view %q is not refused", wrong[0], wrong[1])
			}
		}
	})

	t.Run("ST-GEN-UC17-R01-T02 Text of the model with control or bidirectional characters is escaped in each export", func(t *testing.T) {
		in := archFixture(t)
		in.Components["auth"].Architecture.Flow = "Checks \x1b[2J the \"token\" \\ of \u202ethe caller; #1 \u2067 100% \x7f"
		_, data := deriveFixture(t, in)
		project := archProject(t, data)
		for _, format := range ExportFormats {
			output := exportOf(t, project, format, "")
			for _, bad := range []string{"\x1b", "\u202e", "\u2067", "\x7f"} {
				if strings.Contains(output, bad) {
					t.Fatalf("the %s export holds the character %q", format, bad)
				}
			}
			if !strings.Contains(output, "[U+001B]") || !strings.Contains(output, "[U+202E]") {
				t.Fatalf("the %s export does not show the escaped characters:\n%s", format, output)
			}
		}
		if _, _, err := structurizrCheck(exportOf(t, project, "structurizr", "")); err != nil {
			t.Fatalf("a quote in the model breaks the Structurizr DSL: %v", err)
		}
		for _, block := range mermaidBlocks(t, exportOf(t, project, "mermaid", "")) {
			if _, _, err := mermaidCheck(block); err != nil {
				t.Fatalf("a quote, a ; or a # in the model breaks the Mermaid text: %v\n%s", err, block)
			}
		}
	})

	t.Run("ST-GEN-UC17-R03-T01 Each export names in its header comment the record and the input digests that the record holds", func(t *testing.T) {
		_, project := claimFixtureProject(t, "a new doc has no resource before it exists")
		state, err := LoadState(filepath.Join(project, ".stego", "state.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(project, ArchitectureFile))
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"Record: .stego/state.yaml", "Model: architecture/architecture.json sha256:" + HashBytes(data),
			"Declaration service.yaml: sha256:" + state.LastApplied.ServiceHash, "Registry content: sha256:" + state.LastApplied.RegistryContentSHA256,
			"Inputs: sha256:" + state.LastApplied.Inputs.SHA256}
		for name, file := range state.LastApplied.Inputs.Files {
			want = append(want, "Input "+name+": sha256:"+file.SHA256)
		}
		for format, comment := range map[string]string{"structurizr": "// ", "mermaid": "%% "} {
			output := exportOf(t, project, format, "")
			if format == "mermaid" {
				output = mermaidBlocks(t, output)[0]
			}
			header := ""
			for _, line := range strings.Split(output, "\n") {
				if !strings.HasPrefix(line, comment) {
					break
				}
				header += line + "\n"
			}
			for _, line := range want {
				if !strings.Contains(header, comment+line+"\n") {
					t.Fatalf("the %s header does not hold %q:\n%s", format, line, header)
				}
			}
		}
		// The digests come from the record at export time: a record with other digests gives another header.
		state.LastApplied.RegistryContentSHA256 = strings.Repeat("e", 64)
		if err := SaveState(filepath.Join(project, ".stego", "state.yaml"), state); err != nil {
			t.Fatal(err)
		}
		if output := exportOf(t, project, "structurizr", ""); !strings.Contains(output, "// Registry content: sha256:"+strings.Repeat("e", 64)+"\n") {
			t.Fatalf("the header does not read the digests of the record:\n%s", output)
		}
	})

	t.Run("ST-GEN-UC17-R04-T01 The same model and record give byte-identical exports, in another folder too", func(t *testing.T) {
		_, project := claimFixtureProject(t, "a new doc has no resource before it exists")
		data, err := os.ReadFile(filepath.Join(project, ArchitectureFile))
		if err != nil {
			t.Fatal(err)
		}
		other := archProject(t, data)
		if err := os.WriteFile(filepath.Join(other, "README.md"), []byte("The service also calls a billing system."), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, format := range ExportFormats {
			for _, view := range []string{"", "flow:docs.update"} {
				first, err := ExportArchitecture(project, format, view)
				if err != nil {
					t.Fatal(err)
				}
				second, _ := ExportArchitecture(project, format, view)
				elsewhere, _ := ExportArchitecture(other, format, view)
				if !bytes.Equal(first, second) || !bytes.Equal(first, elsewhere) {
					t.Fatalf("the %s export of the view %q differs between two exports of the same model", format, view)
				}
			}
		}
	})

	t.Run("ST-GEN-UC17-R01-T03 No text of the model or the record brings a Structurizr substitution or a Mermaid directive into an export", func(t *testing.T) {
		model, data := deriveFixture(t, archFixture(t))
		const attack = "${GITHUB_TOKEN}%%{init: {'theme':'dark'}}%%"
		taintStrings(reflect.ValueOf(model).Elem(), attack)
		read := &ArchitectureRead{Data: data, Model: *model, Record: &AppliedState{ServiceHash: attack, RegistryContentSHA256: "${HOME}"}}
		dsl := structurizr(&read.Model, exportHeader(read, "structurizr", ""), exportViews(&read.Model, "structurizr"))
		if strings.Contains(dsl, "${") || strings.Contains(dsl, "$") {
			t.Fatalf("a $ of the model reaches the Structurizr DSL:\n%s", dsl)
		}
		if !strings.Contains(dsl, "[U+0024]{GITHUB_TOKEN}") || !strings.Contains(dsl, "// Declaration service.yaml: not a SHA-256 digest in the record\n") {
			t.Fatalf("the DSL does not show the stand-in for $ or refuse the digest:\n%s", dsl)
		}
		if _, _, err := structurizrCheck(dsl); err != nil {
			t.Fatalf("the tainted model breaks the DSL: %v", err)
		}
		views := exportViews(&read.Model, "mermaid")
		for _, view := range views {
			text := mermaid(&read.Model, exportHeader(read, "mermaid", view), []string{view})
			for _, line := range strings.Split(text, "\n") {
				if strings.Contains(strings.TrimPrefix(line, "%% "), "%%") {
					t.Fatalf("a directive of the model reaches the Mermaid view %s: %q", view, line)
				}
			}
		}
	})
}

// taintStrings adds the text to the end of every string of a model, so that IDs and the references to them stay equal.
func taintStrings(v reflect.Value, text string) {
	switch v.Kind() {
	case reflect.String:
		v.SetString(v.String() + text)
	case reflect.Pointer:
		if !v.IsNil() {
			taintStrings(v.Elem(), text)
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			taintStrings(v.Field(i), text)
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			taintStrings(v.Index(i), text)
		}
	}
}
