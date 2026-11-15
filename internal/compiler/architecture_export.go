package compiler

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// ExportFormats are the formats of stego architecture export (ST-GEN-CLI08). Mermaid gives only the
// entity-relationship and the sequence diagrams, because Mermaid C4 places shapes by the order of the statements
// (ST-GEN-UC17).
var ExportFormats = []string{"structurizr", "mermaid"}

// The views of an export besides the request flows, whose views are named by the IDs of their flows.
const (
	viewContainers = "containers"
	viewComponents = "components"
	viewDataModel  = "data-model"
)

// ExportArchitecture answers ST-GEN-QRY05: it reads the architecture model of the project folder through
// ReadArchitecture, and gives it as Structurizr DSL or as Mermaid text, all views or the one view that view names,
// after a header comment that names the record and the input digests the model was written with. Every text of the
// model is escaped so that no control or bidirectional character reaches the terminal. It writes nothing.
func ExportArchitecture(projectDir, format, view string) ([]byte, error) {
	if !slices.Contains(ExportFormats, format) {
		return nil, &ArchitectureUsageError{Reason: fmt.Sprintf("the format %s is not known; the formats are %s", strconv.Quote(format), strings.Join(ExportFormats, ", "))}
	}
	read, err := ReadArchitecture(projectDir)
	if err != nil {
		return nil, err
	}
	views := exportViews(&read.Model, format)
	if view != "" && !slices.Contains(views, view) {
		return nil, &ArchitectureUsageError{Reason: fmt.Sprintf("the view %s is not a %s view of the model; the views are %s", quoted(view), format, strings.Join(views, ", "))}
	}
	if view != "" {
		views = []string{view}
	}
	header := exportHeader(read, format, view)
	if format == "structurizr" {
		return []byte(structurizr(&read.Model, header, views)), nil
	}
	return []byte(mermaid(&read.Model, header, views)), nil
}

// exportViews gives the views that a format has of the model, in a fixed order.
func exportViews(m *ArchitectureModel, format string) []string {
	views := []string{viewContainers, viewComponents}
	if format == "mermaid" {
		views = []string{viewDataModel}
	}
	for _, f := range m.Flows {
		views = append(views, printable(f.ID))
	}
	return views
}

// exportHeader gives the lines of the header comment: the service, the format, the model with its hash, the record,
// and the digests of the inputs that the record holds (ST-GEN-UC17-R03). The model holds no digest, so the header
// reads them from the record that ReadArchitecture checked the model against.
func exportHeader(read *ArchitectureRead, format, view string) []string {
	lines := []string{
		fmt.Sprintf("The architecture of %s, as %s text from stego architecture export.", diagramText(read.Model.System.Name), format),
		"Model: " + ArchitectureFile + " sha256:" + HashBytes(read.Data),
		"Record: " + diagramText(read.Model.Record.Path),
	}
	if view != "" {
		lines = append(lines, "View: "+diagramText(view))
	}
	record := read.Record
	digest := func(name, value string) {
		if value == "" {
			value = "not recorded"
		} else {
			value = "sha256:" + diagramText(value)
		}
		lines = append(lines, name+": "+value)
	}
	digest("Declaration service.yaml", record.ServiceHash)
	digest("Registry content", record.RegistryContentSHA256)
	if record.Inputs == nil {
		lines = append(lines, "Inputs: not recorded")
		return lines
	}
	digest("Inputs", record.Inputs.SHA256)
	names := make([]string, 0, len(record.Inputs.Files))
	for name := range record.Inputs.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		file := record.Inputs.Files[name]
		if !file.Exists {
			lines = append(lines, "Input "+diagramText(name)+": absent")
			continue
		}
		digest("Input "+diagramText(name), file.SHA256)
	}
	return lines
}

// diagramText gives text of the model for a diagram: each control and bidirectional character as [U+XXXX], each
// double quote as a single quote and each backslash as a slash, so that the text cannot end a string of the diagram
// or act on a terminal.
func diagramText(text string) string {
	text = strings.NewReplacer(`"`, "'", `\`, "/").Replace(text)
	return escapeRunes(text, unsafeRune, func(r rune) string { return fmt.Sprintf("[U+%04X]", r) })
}

// identifiers gives each element ID of a model a distinct identifier of the diagram language: the ID with each
// character other than a letter, a digit or an underscore as an underscore, and a number when two IDs give the same.
type identifiers struct {
	byID  map[string]string
	taken map[string]bool
}

var identifierRune = regexp.MustCompile(`[^A-Za-z0-9_]`)

func newIdentifiers() *identifiers {
	return &identifiers{byID: map[string]string{}, taken: map[string]bool{}}
}

func (n *identifiers) of(id string) string {
	return n.named(id, id)
}

// named gives an element ID the identifier made from a name, such as the name of an entity.
func (n *identifiers) named(id, name string) string {
	if known, ok := n.byID[id]; ok {
		return known
	}
	base := identifierRune.ReplaceAllString(name, "_")
	if base == "" || (base[0] >= '0' && base[0] <= '9') {
		base = "e_" + base
	}
	name = base
	for i := 2; n.taken[name]; i++ {
		name = fmt.Sprintf("%s_%d", base, i)
	}
	n.byID[id], n.taken[name] = name, true
	return name
}

// flowEdge is one call of a request flow between two elements of a diagram, with the step that it makes.
type flowEdge struct {
	from, to string
	step     ArchStep
}

// componentEdges gives the calls of a flow between the components and the fills of a diagram: each step whose
// element is a component, called from the component of the step before it, and each step of a slot as a call from
// the component that offers the slot to each of its fills.
func componentEdges(m *ArchitectureModel, f ArchFlow) []flowEdge {
	components := map[string]bool{}
	for _, c := range m.Components {
		components[c.ID] = true
	}
	slots := map[string]ArchSlot{}
	for _, s := range m.Slots {
		slots[s.ID] = s
	}
	var edges []flowEdge
	previous := ""
	for _, step := range f.Steps {
		if slot, ok := slots[step.Element]; ok && components[slot.OfferedBy] {
			for _, fill := range slot.Fills {
				edges = append(edges, flowEdge{from: slot.OfferedBy, to: fill, step: step})
			}
			previous = slot.OfferedBy
			continue
		}
		if !components[step.Element] {
			continue
		}
		if previous != "" && previous != step.Element {
			edges = append(edges, flowEdge{from: previous, to: step.Element, step: step})
		}
		previous = step.Element
	}
	return edges
}

// structurizr gives the model as one Structurizr DSL workspace: the service as a software system, each container
// with its components and the fills of owner code, a relationship for each binding, need, slot and call of a flow,
// and the views asked for: the container view, the component view and a dynamic view of each request flow.
func structurizr(m *ArchitectureModel, header, views []string) string {
	var b strings.Builder
	for _, line := range header {
		b.WriteString("// " + line + "\n")
	}
	ids := newIdentifiers()
	system := ids.of(m.System.ID)
	service := ""
	fmt.Fprintf(&b, "workspace %s %s {\n", dslString(m.System.Name), dslString("The architecture model of "+m.System.Name+", from architecture/architecture.json."))
	b.WriteString("    model {\n")
	fmt.Fprintf(&b, "        %s = softwareSystem %s %s {\n", system, dslString(m.System.Name), dslString(m.System.Archetype+" "+m.System.ArchetypeVersion+", "+m.System.Language))
	for _, c := range m.Containers {
		id := ids.of(c.ID)
		if c.Kind == "service" && service == "" {
			service = id
		}
		fmt.Fprintf(&b, "            %s = container %s %s {\n", id, dslString(c.Name), dslString(c.Kind))
		for _, k := range m.Components {
			if k.Container != c.ID {
				continue
			}
			fmt.Fprintf(&b, "                %s = component %s %s %s {\n", ids.of(k.ID), dslString(k.Name), dslString(k.FlowPart), dslString(k.Name+" "+k.Version))
			fmt.Fprintf(&b, "                    tags %s\n", dslString(k.Layer))
			b.WriteString("                }\n")
		}
		if id == service {
			for _, f := range m.Fills {
				fmt.Fprintf(&b, "                %s = component %s %s %s {\n", ids.of(f.ID), dslString(f.Name), dslString("Owner code of "+strings.Join(f.Implements, ", ")), dslString("fill"))
				b.WriteString("                    tags \"fill\"\n")
				b.WriteString("                }\n")
			}
		}
		b.WriteString("            }\n")
	}
	b.WriteString("        }\n")
	type relation struct{ from, to, text string }
	var relations []relation
	seen := map[[2]string]bool{}
	relate := func(from, to, text string) {
		if from == to || seen[[2]string{from, to}] {
			return
		}
		seen[[2]string{from, to}] = true
		relations = append(relations, relation{from, to, text})
	}
	for _, binding := range m.Bindings {
		relate(binding.Consumer, binding.Provider, "uses "+strings.TrimPrefix(binding.Port, "port:"))
	}
	for _, need := range m.Needs {
		for _, by := range need.StatedBy {
			relate(by, "container:"+need.Name, "needs "+need.Kind)
		}
	}
	for _, s := range m.Slots {
		for _, fill := range s.Fills {
			relate(s.OfferedBy, fill, "calls in the slot "+s.Name)
		}
	}
	for _, f := range m.Flows {
		for _, e := range componentEdges(m, f) {
			relate(e.from, e.to, "calls in a request flow")
		}
	}
	for _, r := range relations {
		from, okFrom := ids.byID[r.from]
		to, okTo := ids.byID[r.to]
		if okFrom && okTo {
			fmt.Fprintf(&b, "        %s -> %s %s\n", from, to, dslString(r.text))
		}
	}
	b.WriteString("    }\n")
	b.WriteString("    views {\n")
	flows := map[string]ArchFlow{}
	for _, f := range m.Flows {
		flows[printable(f.ID)] = f
	}
	keys := newIdentifiers()
	for _, view := range views {
		switch view {
		case viewContainers:
			fmt.Fprintf(&b, "        container %s %s {\n            include *\n            autolayout lr\n        }\n", system, dslString(viewContainers))
		case viewComponents:
			if service != "" {
				fmt.Fprintf(&b, "        component %s %s {\n            include *\n            autolayout lr\n        }\n", service, dslString(viewComponents))
			}
		default:
			f := flows[view]
			if service == "" {
				continue
			}
			fmt.Fprintf(&b, "        dynamic %s %s %s {\n", service, dslString(keys.of(f.ID)), dslString(stepText(f)))
			for _, e := range componentEdges(m, f) {
				fmt.Fprintf(&b, "            %s -> %s %s\n", ids.of(e.from), ids.of(e.to), dslString(fmt.Sprintf("%d. %s", e.step.N, e.step.Text)))
			}
			b.WriteString("            autolayout lr\n        }\n")
		}
	}
	b.WriteString("    }\n}\n")
	return b.String()
}

// stepText gives the description of a flow: the text of its first step, which names its operation.
func stepText(f ArchFlow) string {
	if len(f.Steps) == 0 {
		return f.Operation
	}
	return f.Steps[0].Text
}

// dslString gives text of the model as a quoted string of Structurizr DSL.
func dslString(text string) string {
	return `"` + diagramText(text) + `"`
}

// mermaid gives the model as Mermaid text: the entity-relationship diagram and a sequence diagram of each request
// flow. One view gives the text of one diagram; all views give a Markdown block of each, so the output goes as it is
// into a README or a pull request.
func mermaid(m *ArchitectureModel, header, views []string) string {
	var b strings.Builder
	flows := map[string]ArchFlow{}
	for _, f := range m.Flows {
		flows[printable(f.ID)] = f
	}
	for i, view := range views {
		if len(views) > 1 {
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString("```mermaid\n")
		}
		for _, line := range header {
			b.WriteString("%% " + line + "\n")
		}
		if len(views) > 1 {
			b.WriteString("%% View: " + diagramText(view) + "\n")
		}
		if view == viewDataModel {
			mermaidEntities(&b, m)
		} else {
			mermaidFlow(&b, flows[view])
		}
		if len(views) > 1 {
			b.WriteString("```\n")
		}
	}
	return b.String()
}

// mermaidEntities writes the entity-relationship diagram: each entity with its fields, their types, their
// sensitivity and whether they are required, and each reference as a relationship.
func mermaidEntities(b *strings.Builder, m *ArchitectureModel) {
	ids := newIdentifiers()
	b.WriteString("erDiagram\n")
	for _, e := range m.Entities {
		if len(e.Fields) == 0 {
			fmt.Fprintf(b, "    %s\n", ids.named(e.ID, e.Name))
			continue
		}
		fmt.Fprintf(b, "    %s {\n", ids.named(e.ID, e.Name))
		for _, f := range e.Fields {
			key := ""
			if f.Ref != "" {
				key = " FK"
			}
			required := "optional"
			if f.Required {
				required = "required"
			}
			fmt.Fprintf(b, "        %s %s%s \"%s, %s\"\n", identifierRune.ReplaceAllString(f.Type, "_"), identifierRune.ReplaceAllString(f.Name, "_"), key, required, mermaidText(f.Sensitivity))
		}
		b.WriteString("    }\n")
	}
	required := map[string]bool{}
	for _, e := range m.Entities {
		for _, f := range e.Fields {
			required[f.ID] = f.Required
		}
	}
	for _, r := range m.References {
		to := "o|"
		if required[r.Field] {
			to = "||"
		}
		name := r.Field
		if i := strings.LastIndex(name, "."); i >= 0 {
			name = name[i+1:]
		}
		fmt.Fprintf(b, "    %s }o--%s %s : \"%s\"\n", ids.named(r.From, r.From), to, ids.named(r.To, r.To), mermaidText(name))
	}
}

// mermaidFlow writes the sequence diagram of one request flow: each element of a step as a participant in the order
// of the steps, each step as a call from the element of the step before it, and each exit as a note.
func mermaidFlow(b *strings.Builder, f ArchFlow) {
	b.WriteString("sequenceDiagram\n")
	participants := map[string]string{}
	for _, step := range f.Steps {
		if _, ok := participants[step.Element]; !ok {
			participants[step.Element] = fmt.Sprintf("p%d", len(participants)+1)
			fmt.Fprintf(b, "    participant %s as %s\n", participants[step.Element], mermaidText(step.Element))
		}
	}
	previous := ""
	for _, step := range f.Steps {
		to := participants[step.Element]
		text := mermaidText(fmt.Sprintf("%d. %s", step.N, step.Text))
		switch {
		case previous == "":
			fmt.Fprintf(b, "    Note over %s: %s\n", to, text)
		case step.Mode == "async":
			fmt.Fprintf(b, "    %s-)%s: %s\n", previous, to, text)
		default:
			fmt.Fprintf(b, "    %s->>%s: %s\n", previous, to, text)
		}
		for _, exit := range f.Exits {
			if exit.AfterStep == step.N {
				fmt.Fprintf(b, "    Note over %s: %s\n", to, mermaidText("Stops when "+exit.When+": "+exit.Result))
			}
		}
		previous = to
	}
}

// mermaidText gives text of the model for Mermaid: the text of diagramText, with each character that Mermaid reads
// in the text of a message as its entity code.
func mermaidText(text string) string {
	return strings.NewReplacer("#", "#35;", ";", "#59;", "%", "#37;", "<", "#60;", ">", "#62;").Replace(diagramText(text))
}
