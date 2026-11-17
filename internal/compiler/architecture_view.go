package compiler

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"slices"
	"sort"
	"strings"
)

// The architecture view (ST-GEN-TM20, ST-GEN-UC08): the one HTML file architecture/index.html that apply writes
// beside the architecture model. It embeds the model as the same JSON value (ST-GEN-UC08-R07), the layout that apply computes
// from it, a text form of each view, and the viewer script and style, and it loads nothing else (R02).

// ArchitectureViewFile is the path, in the project folder, of the architecture view.
const ArchitectureViewFile = "architecture/index.html"

//go:embed architecture_view.html
var architectureViewShell string

//go:embed architecture_view.js
var architectureViewScript string

// viewSpec is the fixed text of one of the six views: its ID, its name in the navigation, its title before the
// service name, and the question it answers.
type viewSpec struct{ id, nav, title, question, aspect string }

// architectureViews are the six views in their order.
var architectureViews = []viewSpec{
	{"overview", "Overview", "Context of", "What does this service talk to, and what runs where?", "structure"},
	{"components", "Components", "Components of", "What is the service made of, and where does each part sit in a request?", "structure"},
	{"data", "Data model", "Data model of", "What data does it hold, how does it relate, and what may callers do with it?", "data_model"},
	{"flows", "Request flows", "Request flows of", "What happens, step by step, when someone calls an operation, and where can it stop?", "request_flows"},
	{"trust", "Trust and tenancy", "Trust and tenancy of", "Where is a caller trusted, where is the tenant fixed, which code is owner code, and who may do what?", "trust_and_tenancy"},
	{"platform", "Platform edges", "Platform edges of", "What must the platform supply for this service to run?", "platform_edges"},
}

// collectionVerbs are the operations of a collection in the order of the matrix of the data model.
var collectionVerbs = []string{"create", "read", "update", "patch", "delete", "list", "upsert"}

// grantGlyphs gives the mark of each kind of grant path in a cell of the access matrix, with its meaning.
var grantGlyphs = []struct{ kind, glyph, meaning string }{
	{"direct", "●", "direct: the relation grants it"},
	{"arrow", "↑", "via parent: through a relation to a parent resource"},
	{"userset", "◎", "via a group: through the members of a relation"},
	{"caveat", "?", "conditional: only when a caveat holds"},
	{"intersection", "&", "AND: only together with another relation"},
	{"exclusion", "−", "excluded: not where an exclusion removes it"},
	{"wildcard", "*", "wildcard: every subject of the type"},
	{"recursion", "↻", "recursion: the path grants through itself and stops"},
	{"depth limit", "⋯", "depth limit: the expansion stops here"},
	{"expiration", "⌛", "expiration: only until the relationship expires"},
	{"work limit", "⊘", "work limit: the expansion stops at its bound of work"},
	{"path limit", "⊞", "path limit: the expansion stops at its most grant paths"},
}

// RenderArchitectureView renders the architecture view from the bytes of the architecture model alone
// (ST-GEN-INV11). The same bytes always give the same view.
func RenderArchitectureView(model []byte) ([]byte, error) {
	var m ArchitectureModel
	if err := json.Unmarshal(model, &m); err != nil {
		return nil, fmt.Errorf("architecture view: the model does not decode: %w", err)
	}
	layout, err := json.Marshal(layoutArchitecture(&m))
	if err != nil {
		return nil, err
	}
	before, after, ok := strings.Cut(architectureViewShell, "<!--forms-->")
	if !ok {
		return nil, fmt.Errorf("architecture view: the shell has no place for the text forms")
	}
	var out bytes.Buffer
	out.WriteString(strings.NewReplacer("{{csp}}", viewPolicy(architectureViewShell), "{{name}}", html.EscapeString(m.System.Name), "{{record}}", html.EscapeString(m.Record.Path),
		"{{archetype}}", html.EscapeString(m.System.Archetype+" "+m.System.ArchetypeVersion)).Replace(before))
	writeForms(&out, &m)
	out.WriteString(after)
	// The model holds free text, so the view embeds it with <, >, & and U+2028 and U+2029 escaped as \u sequences:
	// the same JSON value, which no text in it can end or turn into markup.
	// It is written compact: the same JSON value in fewer bytes.
	var compact bytes.Buffer
	if err := json.Compact(&compact, model); err != nil {
		return nil, err
	}
	out.WriteString("<script type=\"application/json\" id=\"model\">")
	json.HTMLEscape(&out, compact.Bytes())
	out.WriteString("</script>\n<script type=\"application/json\" id=\"layout\">")
	out.Write(layout)
	out.WriteString("</script>\n<script>")
	out.WriteString(viewerScript())
	out.WriteString("</script>\n</body>\n</html>\n")
	return out.Bytes(), nil
}

// viewerScript is the text of the viewer's script element, which the policy of the view admits by its hash.
func viewerScript() string { return "\n" + architectureViewScript }

// inlineScripts gives the text of each script element of the shell that runs: one with no type.
func inlineScripts(shell string) []string {
	var out []string
	for rest := shell; ; {
		start := strings.Index(rest, "<script>")
		if start < 0 {
			return out
		}
		rest = rest[start+len("<script>"):]
		end := strings.Index(rest, "</script>")
		if end < 0 {
			return out
		}
		out = append(out, rest[:end])
		rest = rest[end:]
	}
}

// scriptHash gives the CSP source of a script by the SHA-256 of its text.
func scriptHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

// viewPolicy gives the content security policy of the view (#342 F1): it loads nothing, and runs only the scripts
// of the view, each named by the hash of its text, which Go computes when it renders the view. The style of the view
// is inline in the file, so style-src admits inline style; the model download is a data: address. The view writes
// the page only through the DOM, never through a sink that parses a text as HTML or script, so it requires trusted
// types for scripts: a browser that knows them refuses such a write (security review of PR #345).
func viewPolicy(shell string) string {
	var hashes []string
	for _, text := range append(inlineScripts(shell), viewerScript()) {
		hashes = append(hashes, scriptHash(text))
	}
	return "default-src 'none'; script-src " + strings.Join(hashes, " ") + "; style-src 'unsafe-inline'; img-src data:; base-uri 'none'; form-action 'none'; require-trusted-types-for 'script'"
}

// formWriter writes the text forms, escaping every text that comes from the model.
type formWriter struct{ b *bytes.Buffer }

func (w formWriter) raw(s string)  { w.b.WriteString(s) }
func (w formWriter) text(s string) { w.b.WriteString(html.EscapeString(s)) }
func (w formWriter) tag(t, attrs, s string) {
	w.raw("<" + t + attrs + ">")
	w.text(s)
	w.raw("</" + t + ">")
}
func attr(name, value string) string { return " " + name + "=\"" + html.EscapeString(value) + "\"" }
func (w formWriter) head(cols ...string) {
	w.raw("<div class=\"scroll\"><table><thead><tr>")
	for _, c := range cols {
		w.tag("th", " scope=\"col\"", c)
	}
	w.raw("</tr></thead><tbody>")
}
func (w formWriter) row(id string, cells ...string) {
	if id != "" {
		w.raw("<tr" + attr("data-id", id) + ">")
	} else {
		w.raw("<tr>")
	}
	for _, c := range cells {
		w.tag("td", "", c)
	}
	w.raw("</tr>")
}
func (w formWriter) end()        { w.raw("</tbody></table></div>") }
func (w formWriter) h3(s string) { w.tag("h3", "", s) }
func (w formWriter) key(items ...string) {
	w.raw("<ul class=\"key\" aria-label=\"Key\">")
	for _, it := range items {
		w.tag("li", "", it)
	}
	w.raw("</ul>")
}

// sourceText names where an element comes from (ST-GEN-UC08-R03).
func sourceText(s ArchSource) string {
	switch {
	case s.Declaration != "":
		return "service.yaml " + s.Declaration
	case s.Registry != nil:
		t := "registry " + s.Registry.Part + " " + s.Registry.Name + " " + s.Registry.Version
		if s.Registry.Field != "" {
			t += ", field " + s.Registry.Field
		}
		return t
	case s.Wiring != "":
		return "wiring " + s.Wiring
	case s.Default != "":
		return "default, rule " + s.Default
	case s.Rule != "":
		return "STEGO rule " + s.Rule
	}
	return ""
}

// shortID gives the name part of an ID of the form kind:name.
func shortID(id string) string {
	if _, n, ok := strings.Cut(id, ":"); ok {
		return n
	}
	return id
}

// aspectOf gives the state of an aspect by its key.
func aspectOf(m *ArchitectureModel, key string) ArchAspect {
	switch key {
	case "structure":
		return m.Aspects.Structure
	case "data_model":
		return m.Aspects.DataModel
	case "request_flows":
		return m.Aspects.RequestFlows
	case "platform_edges":
		return m.Aspects.PlatformEdges
	}
	return m.Aspects.TrustAndTenancy
}

// writeForms writes the text form of each view: the same elements as its diagram, as tables and lists. The view
// shows them on "Show as text", and print shows them, each view on its own sheet with its title and its key.
func writeForms(b *bytes.Buffer, m *ArchitectureModel) {
	w := formWriter{b}
	for _, v := range architectureViews {
		w.raw("<section class=\"form\"" + attr("id", "form-"+v.id) + attr("data-view", v.id) + ">")
		w.tag("h2", "", v.title+" "+m.System.Name)
		w.tag("p", " class=\"question\"", v.question)
		if a := aspectOf(m, v.aspect); a.State == "empty" {
			w.raw("<div class=\"empty\" role=\"note\"><strong>This aspect has no element yet.</strong>")
			w.tag("p", "", a.Reason)
			w.raw("</div></section>")
			continue
		}
		switch v.id {
		case "overview":
			overviewForm(w, m)
		case "components":
			componentForm(w, m)
		case "data":
			dataForm(w, m)
		case "flows":
			flowForm(w, m)
		case "trust":
			trustForm(w, m)
		case "platform":
			platformForm(w, m)
		}
		w.raw("</section>")
	}
}

func overviewForm(w formWriter, m *ArchitectureModel) {
	w.key("Person: the caller", "Entry: a collection that callers reach through the network edge", "Container: a unit that runs or stores",
		"Double box: a system outside the service", "Line: a relation of the service; a dashed line runs later")
	w.head("Element", "Kind", "What it is", "Source")
	for _, a := range m.Actors {
		w.row(a.ID, a.Name, "actor, "+a.Kind, actorText(a), sourceText(a.Source))
	}
	for _, c := range m.Containers {
		what := "the service process"
		if c.Kind != "service" {
			what = "a " + c.Kind + " that a need states"
		}
		w.row(c.ID, c.Name, "container, "+c.Kind, what, sourceText(c.Source))
	}
	ops := map[string]ArchOperation{}
	for _, o := range m.Operations {
		ops[o.ID] = o
	}
	for _, e := range networkEntries(m) {
		for _, c := range m.Collections {
			if c.ID != e.id {
				continue
			}
			var routes []string
			for _, id := range c.Operations {
				if o, ok := ops[id]; ok && o.Method != "" {
					routes = append(routes, o.Method+" "+o.Path)
				}
			}
			w.row(c.ID, c.Name, "entry, "+plural(e.ops, "operation"), strings.Join(routes, "; "), sourceText(c.Source))
		}
	}
	w.end()
	if len(m.Relations) > 0 {
		names := map[string]string{}
		for _, c := range m.Containers {
			names[c.ID] = c.Name
		}
		for _, a := range m.Actors {
			names[a.ID] = a.Name
		}
		w.h3("Relations")
		w.head("From", "Relation", "To", "Through", "When", "Source")
		for _, r := range m.Relations {
			when := "in the request"
			if r.Mode == "async" {
				when = "later, after the response"
			}
			w.row(r.ID, names[r.From], r.Text, names[r.To], shortID(r.Through), when, sourceText(r.Source))
		}
		w.end()
	}
}

// actorText says what an actor is, in fixed words.
func actorText(a ArchActor) string {
	switch a.Kind {
	case actorCaller:
		return "calls the operations of the collections"
	case actorTokenIssuer:
		return "issues the tokens that " + shortID(a.StatedBy) + " checks"
	case actorAuthorization:
		return "decides each permission that an operation checks"
	case actorStore:
		return "keeps the rows that " + shortID(a.StatedBy) + " writes"
	}
	if a.StatedBy != "" {
		return "a system outside the service that " + shortID(a.StatedBy) + " calls"
	}
	return "a system outside the service"
}

func portText(p ArchComponentPort) string {
	return shortID(p.Port) + " (" + p.Direction + ", " + p.Side + ")"
}

func componentForm(w formWriter, m *ArchitectureModel) {
	w.key("Column and colour: the layer", "Box: a component", "Hexagon: a slot, where owner fills run", "Filled square: a provided port; open square: a required port; left: inbound, right: outbound", "Line: a binding from the consumer to the provider of a port")
	w.h3("Components")
	w.head("Layer", "Component", "Part in a request", "Ports", "Source")
	for _, l := range m.Layers {
		for _, c := range m.Components {
			if c.Layer != l {
				continue
			}
			var ports []string
			for _, p := range c.Ports {
				ports = append(ports, portText(p))
			}
			w.row(c.ID, l, c.Name+" "+c.Version, c.FlowPart, strings.Join(ports, "; "), sourceText(c.Source))
		}
	}
	w.end()
	w.h3("Ports")
	w.head("Port", "Provided by", "Required by", "Source")
	for _, p := range m.Ports {
		w.row(p.ID, p.Name, idList(p.ProvidedBy), idList(p.RequiredBy), sourceText(p.Source))
	}
	w.end()
	w.h3("Bindings")
	w.head("Port", "Consumer", "Provider", "Source")
	for _, bd := range m.Bindings {
		w.row(bd.ID, shortID(bd.Port), shortID(bd.Consumer), shortID(bd.Provider), sourceText(bd.Source))
	}
	w.end()
	w.h3("Slots")
	w.head("Slot", "Phase", "Operations", "Strategy", "Fills in order", "Offered by", "Source")
	for _, s := range m.Slots {
		strategy := s.Strategy
		if s.ShortCircuit {
			strategy += ", stops at the first fill that stops"
		}
		w.row(s.ID, shortID(s.Collection)+"."+s.Name, strings.ReplaceAll(s.Phase, "_", " "), strings.Join(s.Operations, ", "), strategy, idList(s.Fills), shortID(s.OfferedBy), sourceText(s.Source))
	}
	w.end()
	w.h3("Fills")
	w.head("Fill", "Implements", "Source")
	for _, f := range m.Fills {
		w.row(f.ID, f.Name, idList(f.Implements), sourceText(f.Source))
	}
	w.end()
	if len(m.Mixins) > 0 {
		w.h3("Mixins")
		w.head("Mixin", "Offers", "Source")
		for _, x := range m.Mixins {
			var offers []string
			for _, o := range x.Offers {
				offers = append(offers, o.Name+" ("+strings.ReplaceAll(o.Phase, "_", " ")+"; "+strings.Join(o.Operations, ", ")+")")
			}
			w.row(x.ID, x.Name+" "+x.Version, strings.Join(offers, "; "), sourceText(x.Source))
		}
		w.end()
	}
}

func idList(ids []string) string {
	names := make([]string, len(ids))
	for i, id := range ids {
		names[i] = shortID(id)
	}
	return strings.Join(names, ", ")
}

func dataForm(w formWriter, m *ArchitectureModel) {
	w.key("Box: an entity with its fields; * marks a required field", "Line with a crow's foot: many rows refer to one", "●: the collection offers the operation; –: it does not")
	w.h3("Entities")
	w.head("Entity", "Field", "Type", "Required", "Sensitivity", "Source")
	for _, e := range m.Entities {
		w.row(e.ID, e.Name, "", "tenant "+e.TenantKind, "", "", sourceText(e.Source))
		for _, f := range e.Fields {
			req := "no"
			if f.Required {
				req = "yes"
			}
			w.row(e.ID, "", f.Name, f.Type, req, f.Sensitivity, sourceText(f.Source))
		}
	}
	w.end()
	if len(m.References) > 0 {
		w.h3("References")
		w.head("From", "Field", "To", "Relation", "Source")
		for _, r := range m.References {
			w.row(r.ID, shortID(r.From), shortID(r.Field), shortID(r.To), r.Relation, sourceText(r.Source))
		}
		w.end()
	}
	w.h3("Operations each collection offers")
	w.raw("<div class=\"scroll\"><table class=\"matrix\" aria-label=\"Operations each collection offers\"><thead><tr>")
	w.tag("th", " scope=\"col\"", "Collection")
	for _, v := range collectionVerbs {
		w.tag("th", " scope=\"col\"", v)
	}
	w.raw("</tr></thead><tbody>")
	for _, c := range m.Collections {
		w.raw("<tr" + attr("data-id", c.ID) + ">")
		w.tag("th", " scope=\"row\"", c.Name+" ("+shortID(c.Entity)+")")
		for _, v := range collectionVerbs {
			op := "operation:" + c.Name + "." + v
			has := false
			for _, id := range c.Operations {
				has = has || id == op
			}
			if has {
				w.raw("<td" + attr("data-op", op) + ">●<span class=\"sr\"> offers " + html.EscapeString(v) + "</span></td>")
			} else {
				w.raw("<td>–<span class=\"sr\"> no " + html.EscapeString(v) + "</span></td>")
			}
		}
		w.raw("</tr>")
	}
	w.raw("</tbody></table></div>")
	if len(m.Actions) > 0 {
		w.h3("Commands, queries and workers")
		w.head("Action", "Kind", "Fills", "Source")
		for _, a := range m.Actions {
			w.row(a.ID, a.Name, a.Kind, idList(a.Fills), sourceText(a.Source))
		}
		w.end()
	}
}

func flowForm(w formWriter, m *ArchitectureModel) {
	w.key("Each request flow is a numbered list of its steps", "later: the step runs after the response, from the outbox", "↳ stops: a point where the flow can stop, and the result it then gives")
	ops := map[string]ArchOperation{}
	for _, o := range m.Operations {
		ops[o.ID] = o
	}
	layer := map[string]string{}
	for _, c := range m.Components {
		layer[c.ID] = c.Layer
	}
	actions := map[string]ArchAction{}
	flowOf := map[string]bool{}
	for _, f := range m.Flows {
		flowOf[f.Operation] = true
	}
	var names []string
	for _, a := range m.Actions {
		actions[a.ID] = a
		if !flowOf[a.ID] {
			names = append(names, a.Name+" ("+a.Kind+")")
		}
	}
	if len(names) > 0 {
		w.tag("p", " class=\"question\" id=\"form-no-flow-actions\"", "The model has a request flow for each operation of a collection and for each declared command, query and worker. These actions of the platform have no flow here; the access matrix shows what each one checks: "+strings.Join(names, ", ")+".")
	}
	for _, f := range m.Flows {
		o := ops[f.Operation]
		w.raw("<div class=\"flow\"" + attr("data-id", f.ID) + ">")
		title := shortID(f.Operation)
		if o.Method != "" {
			title = o.Method + " " + o.Path + " (" + title + ")"
		} else if a, isAction := actions[f.Operation]; isAction {
			title = a.Kind + " " + a.Name
		}
		w.h3(title)
		w.raw("<ol class=\"flowlist\">")
		for _, s := range f.Steps {
			w.raw("<li" + attr("data-id", s.Element) + ">")
			label := s.Kind
			if s.Mode == "async" {
				label += ", later"
			}
			w.tag("span", " class=\"k\"", label+" · "+shortID(s.Element))
			w.text(" " + s.Text)
			if l, ok := layer[s.Element]; ok && s.Kind == "store" && l != "store" {
				w.tag("span", " class=\"exit\"", "No component of the active set is in the store layer, so the model names "+shortID(s.Element)+", the "+l+" component that writes the store.")
			}
			for _, x := range f.Exits {
				if x.AfterStep == s.N {
					w.tag("span", " class=\"exit\"", "↳ stops: "+x.Result+", when "+x.When)
				}
			}
			w.raw("</li>")
		}
		w.raw("</ol></div>")
	}
}

// matrixCell gives the grant paths of a permission whose role is the given role.
func matrixCell(p *ArchPermission, role string) []ArchGrant {
	var out []ArchGrant
	if p == nil {
		return nil
	}
	for _, g := range p.Paths {
		if g.Role == role {
			out = append(out, g)
		}
	}
	return out
}

// cellGlyphs gives the marks of the kinds of the grant paths in a cell, in the order of the key, and their names.
func cellGlyphs(paths []ArchGrant) (string, []string) {
	kinds := map[string]bool{}
	for _, g := range paths {
		for _, k := range g.Kinds {
			kinds[k] = true
		}
	}
	var glyphs, names []string
	for _, g := range grantGlyphs {
		if kinds[g.kind] {
			glyphs = append(glyphs, g.glyph)
			names = append(names, g.kind)
		}
	}
	return strings.Join(glyphs, ""), names
}

func trustForm(w formWriter, m *ArchitectureModel) {
	w.key("Each boundary has its own line style and its name: network edge long dashes, verified identity solid, tenant scope short dashes, fill code hatched, process dotted", "default: the declaration does not set the value, and the named rule sets it")
	w.h3("Trust boundaries")
	w.head("Boundary", "What it is", "Holds", "Crossed by", "Source")
	for _, b := range m.Boundaries {
		w.row(b.ID, b.Kind, b.Label, idList(b.Contains), idList(b.CrossedBy), sourceText(b.Source))
	}
	w.end()
	w.h3("Tenancy")
	w.head("Setting", "Value", "Set by")
	for _, s := range m.Settings {
		if strings.HasPrefix(s.Path, "tenancy.") {
			w.row(s.ID, s.Path, fmt.Sprint(s.Value), settingBy(s))
		}
	}
	w.end()
	w.h3("Where a caller is authenticated, and where authorization is decided")
	w.head("Point", "Element", "What it does", "Source")
	for _, p := range m.AuthnPoints {
		w.row(p.ID, "authentication", shortID(p.Element), p.Text, sourceText(p.Source))
	}
	for _, p := range m.AuthzPoints {
		w.row(p.ID, "authorization", shortID(p.Element), p.Text, sourceText(p.Source))
	}
	w.end()
	accessForm(w, m)
	w.h3("Every setting, with its defaults")
	w.head("Setting", "Value", "Set by")
	for _, s := range m.Settings {
		w.row(s.ID, s.Path, fmt.Sprint(s.Value), settingBy(s))
	}
	w.end()
}

func settingBy(s ArchSetting) string {
	if s.Default && s.Source.Default == "" {
		return "default, " + sourceText(s.Source)
	}
	return sourceText(s.Source)
}

// accessGroup is one group of the columns (or the lines) of the access matrix: the operations of one collection,
// or the commands, queries and workers.
type accessGroup struct {
	name   string
	checks []ArchCheck
}

// accessGroups gives the checks of the access matrix grouped by collection, in the order of the collections and of
// the operations of the data model, then the commands, queries and workers.
func accessGroups(m *ArchitectureModel) []accessGroup {
	bySubject := map[string]ArchCheck{}
	for _, c := range m.Access.Checks {
		bySubject[c.Subject] = c
	}
	var groups []accessGroup
	used := map[string]bool{}
	for _, col := range m.Collections {
		g := accessGroup{name: col.Name}
		for _, verb := range collectionVerbs {
			if c, ok := bySubject["operation:"+col.Name+"."+verb]; ok {
				g.checks = append(g.checks, c)
				used[c.ID] = true
			}
		}
		if len(g.checks) > 0 {
			groups = append(groups, g)
		}
	}
	rest := accessGroup{name: "commands, queries and workers"}
	for _, c := range m.Access.Checks {
		if !used[c.ID] {
			rest.checks = append(rest.checks, c)
		}
	}
	if len(rest.checks) > 0 {
		groups = append(groups, rest)
	}
	return groups
}

// accessHead writes the head of the operation of a check: its name in its group, what it checks, and whether the
// grant paths of its permission are complete.
func accessHead(w formWriter, c ArchCheck, group string, perms map[string]*ArchPermission, attrs string) {
	what := c.ResourceType + "#" + c.Permission
	if c.Public {
		what = "public"
	}
	name := shortID(c.Subject)
	if strings.HasPrefix(c.Subject, "operation:") {
		name = strings.TrimPrefix(name, group+".")
	}
	title := shortID(c.Subject) + ": " + what
	if checkIncomplete(c, perms) {
		title += " (incomplete: " + incompleteWhy(perms[c.Grants]) + ")"
	}
	if len(c.Ancestors) > 0 {
		title += "; " + ancestorsText(c, perms)
	}
	w.raw("<th" + attrs + attr("data-id", c.ID) + attr("title", title) + ">")
	w.text(name)
	w.tag("span", " class=\"sub\"", what)
	if len(c.Ancestors) > 0 {
		// The read of an ancestor whose grant paths a limit cut short may be held by more roles than the matrix shows,
		// so the head marks it next to the ancestor: the view never shows an incomplete grant set as complete (security
		// review of PR #374, L2).
		w.raw("<span class=\"sub ancestors\">⤒ ")
		for i, a := range c.Ancestors {
			if i > 0 {
				w.text(", ")
			}
			w.text(shortID(a.Entity) + " " + ancestorText(a))
			if ancestorIncomplete(a, perms) {
				w.text(" ")
				w.tag("span", " class=\"incomplete\"", ancestorMark(a, perms))
			}
		}
		w.raw("</span>")
	}
	if checkIncomplete(c, perms) {
		w.tag("span", " class=\"sub incomplete\"", "⚠ incomplete: "+incompleteWhy(perms[c.Grants]))
	}
	w.raw("</th>")
}

// ancestorText gives what the check of one ancestor decides: exists and linked for an ancestor that is not decided,
// else its permission, public, or none.
func ancestorText(a ArchAncestor) string {
	switch {
	case !a.Decided:
		return ancestorLinked
	case a.Permission != "":
		return a.ResourceType + "#" + a.Permission
	case a.Public:
		return "public"
	}
	return "no permission"
}

// ancestorsShort gives the ancestors of a check, the top one first, each with what it decides, and the mark
// incomplete on each whose grant paths a limit cut short.
func ancestorsShort(c ArchCheck, perms map[string]*ArchPermission) string {
	var parts []string
	for _, a := range c.Ancestors {
		part := shortID(a.Entity) + " " + ancestorText(a)
		if ancestorIncomplete(a, perms) {
			part += " " + ancestorMark(a, perms)
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", ")
}

// ancestorsText says in words what the handler of a nested operation checks on the ancestors (ST-SUP-RC20-OC24).
func ancestorsText(c ArchCheck, perms map[string]*ArchPermission) string {
	return "checks that each ancestor in the path, the top one first, exists and is of the ancestor above it, decides a permission only on the parent of a list, a create or an upsert, and answers 404 when one is missing, denied or of another parent: " + ancestorsShort(c, perms)
}

// incompleteMark marks the read of an ancestor whose grant paths a limit cut short.
const incompleteMark = "⚠ incomplete: a limit was reached"

// ancestorMark marks the read of an ancestor whose grant paths are incomplete: a limit cut them short, or the model
// holds no permission of the name that the ancestor grants by (security review of PR #390, L1).
func ancestorMark(a ArchAncestor, perms map[string]*ArchPermission) string {
	return "⚠ incomplete: " + incompleteWhy(perms[a.Grants])
}

// mayHoldGlyph and mayHoldText mark a cell of a role that no named path gives the permission of the operation, when
// a limit cut the grant paths of that permission short: the role may hold it by a path past the limit.
const (
	mayHoldGlyph = "⚠"
	mayHoldText  = "may hold: no path that the model names gives this role the permission, but a limit cut its grant paths short, so this role may hold it"
)

// mayHoldStopText marks the cell of the line of the paths that stop, when a limit cut the grant paths of the
// permission short and the model names no path of it that stops: a role may hold it by a path past the limit, so the
// cell never reads as no grant (security review of PR #385, L2).
const mayHoldStopText = "may hold: the model names no path of this permission that stops, but a limit cut its grant paths short, so a role may hold it by a path past the limit"

// mayHoldMissingText and mayHoldMissingStopText mark a cell of a check whose grants name a permission that the model
// does not hold, as in a model that a reader edited: the cell of a role and the cell of the line of the paths that
// stop. A role may hold the permission, so the cell never reads as no grant (security review of PR #388, Info).
const (
	mayHoldMissingText     = "may hold: the model holds no grant path of this permission, so this role may hold it"
	mayHoldMissingStopText = "may hold: the model holds no grant path of this permission, so a role may hold it"
)

// unnamedLimit names the limit of a permission that a limit cut short, when no path of it names the limit, as in a
// model that a reader edited.
const unnamedLimit = "a limit that the model does not name"

// limitKinds are the kinds of a grant path that a limit stops, in the order in which a path without stop text names
// its limit.
var limitKinds = []string{"depth limit", "work limit", "path limit"}

// limitName gives the limit at which a grant path stops, as its stop text names it, with no node and no rest: the
// depth limit, the work limit or the path limit. A path that no limit stops, such as a recursion, gives "".
func limitName(g ArchGrant) string {
	kind := ""
	for _, k := range limitKinds {
		if slices.Contains(g.Kinds, k) {
			kind = k
			break
		}
	}
	if kind == "" {
		return ""
	}
	name, _, _ := strings.Cut(g.Stops, ";")
	name, _, _ = strings.Cut(name, " at ")
	if name = strings.TrimSpace(name); name != "" {
		return name
	}
	return "the " + kind
}

// permissionLimits gives each limit that cut the grant paths of the permissions short, once, in the order of the
// permissions and their paths. A permission cut short with no path that names its limit gives unnamedLimit, so that
// a cut never reads as no cut (security review of PR #385, L2).
func permissionLimits(perms ...*ArchPermission) []string {
	var out []string
	for _, p := range perms {
		if !permissionIncomplete(p) {
			continue
		}
		named := false
		for _, g := range p.Paths {
			if name := limitName(g); name != "" {
				named = true
				if !slices.Contains(out, name) {
					out = append(out, name)
				}
			}
		}
		if !named && !slices.Contains(out, unnamedLimit) {
			out = append(out, unnamedLimit)
		}
	}
	return out
}

// permissionIncomplete says whether a limit cut the grant paths of the permission short. A permission whose model
// does not say that its paths are complete counts as cut short, so the view fails toward "may hold" (security
// review of PR #383, L3); the JSON decode gives a missing key complete the value false.
func permissionIncomplete(p *ArchPermission) bool { return p != nil && !p.Complete }

// checkIncomplete says whether the roles that the matrix names for a check may be fewer than the roles that hold its
// permission: a limit cut the grant paths of the permission short, or the model holds no permission of the name that
// the check grants by, as in a model that a reader edited. So the view fails toward "may hold" (security review of PR
// #388, Info). A public check needs no permission.
func checkIncomplete(c ArchCheck, perms map[string]*ArchPermission) bool {
	if !checkNamesPermission(c) {
		return false
	}
	p := perms[c.Grants]
	return p == nil || permissionIncomplete(p)
}

// checkNamesPermission says whether a check states a permission: a check that is not public and names a permission
// or the permission that it grants by. A check of a service with no authorization bound names none, and no permission
// is checked; it is never incomplete (security review of PR #390, M1).
func checkNamesPermission(c ArchCheck) bool {
	return !c.Public && (c.Permission != "" || c.Grants != "")
}

// ancestorNamesPermission says whether the read of an ancestor states a permission, as checkNamesPermission does for
// a check. Only a decided ancestor states one (ST-SUP-RC20-OC24).
func ancestorNamesPermission(a ArchAncestor) bool {
	return a.Decided && !a.Public && (a.Permission != "" || a.Grants != "")
}

// incompleteWhy says why the grant paths of the permission of a check are incomplete.
func incompleteWhy(p *ArchPermission) string {
	if p == nil {
		return "the model holds no grant path of it"
	}
	return "a limit was reached"
}

// ancestorIncomplete says whether a limit cut short the grant paths of the read permission of an ancestor.
// An ancestor whose grants name a permission that the model does not hold is incomplete too, so its read never reads
// as a definite no (security review of PR #390, L1).
func ancestorIncomplete(a ArchAncestor, perms map[string]*ArchPermission) bool {
	if !ancestorNamesPermission(a) {
		return false
	}
	p := perms[a.Grants]
	return p == nil || permissionIncomplete(p)
}

// missingAncestors gives the ancestors whose read permission the role alone does not hold by a path that the model
// names: a caller with only that role passes the check of the operation, and still gets 404 from the parent chain
// check. An ancestor whose grant paths a limit cut short is named as incomplete, because the role may hold its read
// by a path past the limit.
func missingAncestors(c ArchCheck, role string, perms map[string]*ArchPermission) []string {
	var out []string
	for _, a := range c.Ancestors {
		if ancestorNamesPermission(a) && len(matrixCell(perms[a.Grants], role)) == 0 {
			name := shortID(a.Entity) + " (" + ancestorText(a) + ")"
			if ancestorIncomplete(a, perms) {
				// The role may hold the read by a path that the model does not name (security review of PR #390, L1).
				name += ", whose grant paths are incomplete: " + incompleteWhy(perms[a.Grants]) + ", so this role may hold it"
			}
			out = append(out, name)
		}
	}
	return out
}

// roleName gives the name of a role of the access matrix, with its conditions.
func roleName(r ArchRole) string {
	name := r.SubjectType + " as " + r.On + "#" + r.Relation
	if r.Wildcard {
		name = r.SubjectType + ":* as " + r.On + "#" + r.Relation
	}
	if r.Caveat != "" {
		name += ", when " + r.Caveat + " holds"
	}
	if r.Expiration {
		name += ", until it expires"
	}
	return name
}

// breakableName gives the escaped markup of a role name that may break after the # of its relation and after the
// word as, never inside a word. It escapes each part apart and puts <wbr> between the parts, so a break never falls
// inside an entity such as &#39; that the escape of a quote writes (security review of PR #345, F3).
func breakableName(name string) string {
	var b strings.Builder
	for name != "" {
		at, n := len(name), 0
		if i := strings.Index(name, "#"); i >= 0 {
			at, n = i, 1
		}
		if i := strings.Index(name, " as "); i >= 0 && i < at {
			at, n = i, len(" as")
		}
		if n == 0 {
			b.WriteString(html.EscapeString(name))
			break
		}
		b.WriteString(html.EscapeString(name[:at+n]))
		b.WriteString("<wbr>")
		name = name[at+n:]
	}
	return b.String()
}

// accessForm writes the access matrix (ST-GEN-UC08-R14): a line for each role and a cell for each operation,
// command, query and worker, with the marks of the kinds of grant in it, the operations grouped by collection.
// When the operations outnumber the roles, the matrix is turned: a column for each role and a line for each
// operation, so that every operation shows on a screen. It shows no relationship of a real subject, because the
// model holds none.
func accessForm(w formWriter, m *ArchitectureModel) {
	w.h3("Access matrix: who may call each operation")
	keys := []string{"◇ public: the operation checks no permission", "· no grant"}
	for _, g := range grantGlyphs {
		keys = append(keys, g.glyph+" "+g.meaning)
	}
	keys = append(keys, mayHoldGlyph+" in a cell: may hold: a limit cut the grant paths of the permission short, or the model holds none of them, so this role may hold it by a path that the model does not name")
	keys = append(keys, "⤒ under a head: the ancestors in the path; each must exist and be of the ancestor above it, and a permission is shown only where the handler decides it; in a cell: this role alone does not grant that read, so the caller also needs it")
	w.key(keys...)
	perms := map[string]*ArchPermission{}
	for i := range m.Access.Permissions {
		perms[m.Access.Permissions[i].ID] = &m.Access.Permissions[i]
	}
	checks := m.Access.Checks
	groups := accessGroups(m)
	if !m.Access.RolesComplete {
		w.tag("p", " class=\"incomplete\" role=\"note\" id=\"roles-incomplete\"", "⚠ Incomplete: a limit was reached. A limit cut the grant paths of a permission short, so more roles may get a permission than this matrix shows.")
	}
	roles := m.Access.Roles
	stopped := false
	for _, c := range checks {
		if len(matrixCell(perms[c.Grants], "")) > 0 {
			stopped = true
		}
	}
	type line struct{ id, name string }
	var lines []line
	for _, r := range roles {
		lines = append(lines, line{r.ID, roleName(r)})
	}
	if stopped {
		lines = append(lines, line{"", "Paths that stop, with no role"})
	}
	turned := len(checks) > len(roles)
	orientation := "roles in lines"
	if turned {
		orientation = "operations in lines"
	}
	// With no role, the matrix says that no role is granted only when no limit cut a grant path short. When a limit
	// cut the paths short, the model names no role, but a role may hold a permission by a path past the limit, so
	// the matrix says so and keeps the line of the paths that stop (security review of PR #384, L1).
	// A check whose permission the model does not hold, as in a model that a reader edited, can grant a role that the
	// model does not name, so the matrix never says that no role is granted then (security review of PR #388, Info).
	missing := false
	for _, c := range checks {
		missing = missing || (checkNamesPermission(c) && perms[c.Grants] == nil)
	}
	if len(roles) == 0 && missing {
		w.tag("p", " class=\"incomplete\" role=\"note\" id=\"no-role-missing\"", "⚠ Incomplete: the model holds no grant path of a permission that an operation checks, so it names no role that holds it. A role may hold that permission; this matrix does not say that no role holds one.")
	}
	if len(roles) == 0 && m.Access.RolesComplete && !missing {
		w.tag("p", " id=\"no-role\"", "The declared authorization schema grants no role a permission that an operation checks.")
	} else if len(roles) == 0 && !m.Access.RolesComplete {
		// The note names each limit that stopped the paths, so the reader knows the cause (security review of PR
		// #385, L2).
		var all []*ArchPermission
		for i := range m.Access.Permissions {
			all = append(all, &m.Access.Permissions[i])
		}
		limits := permissionLimits(all...)
		if len(limits) == 0 {
			limits = []string{unnamedLimit}
		}
		w.tag("p", " class=\"incomplete\" role=\"note\" id=\"no-role-named\"", "⚠ Incomplete: no grant path reached a role before a limit stopped it: "+strings.Join(limits, " and ")+". So the model names no role. A role may hold a permission by a path past the limit; this matrix does not say that no role holds one.")
	}
	if len(lines) == 0 {
		// No line to draw: the sentence above is the whole matrix.
	} else if turned {
		w.raw("<div class=\"scroll matrix-wrap\"><table class=\"matrix access\" aria-label=\"Access matrix\"" + attr("data-orientation", orientation) + ">")
		w.raw("<thead><tr>")
		w.tag("th", " scope=\"col\"", "Operation")
		for _, l := range lines {
			if l.id == "" {
				w.tag("th", " scope=\"col\" class=\"role\"", l.name)
				continue
			}
			// A role name breaks after its subject and after the # of its relation, never inside a word.
			w.raw("<th scope=\"col\" class=\"role\"" + attr("data-id", l.id) + attr("title", l.name) + ">")
			w.raw(breakableName(l.name))
			w.raw("</th>")
		}
		w.raw("</tr></thead>")
		for _, g := range groups {
			w.raw("<tbody><tr class=\"group\">")
			w.tag("th", " scope=\"rowgroup\""+attr("colspan", fmt.Sprint(len(lines)+1)), g.name)
			w.raw("</tr>")
			for _, c := range g.checks {
				w.raw("<tr>")
				accessHead(w, c, g.name, perms, " scope=\"row\"")
				for _, l := range lines {
					matrixTD(w, c, l.id, perms)
				}
				w.raw("</tr>")
			}
			w.raw("</tbody>")
		}
		w.raw("</table></div>")
	} else {
		w.raw("<div class=\"scroll matrix-wrap\"><table class=\"matrix access\" aria-label=\"Access matrix\"" + attr("data-orientation", orientation) + ">")
		w.raw("<thead><tr class=\"group\">")
		w.tag("th", " scope=\"col\" rowspan=\"2\"", "Role")
		for _, g := range groups {
			w.tag("th", " scope=\"colgroup\""+attr("colspan", fmt.Sprint(len(g.checks))), g.name)
		}
		w.raw("</tr><tr>")
		for _, g := range groups {
			for _, c := range g.checks {
				accessHead(w, c, g.name, perms, " scope=\"col\"")
			}
		}
		w.raw("</tr></thead><tbody>")
		for _, l := range lines {
			if l.id != "" {
				w.raw("<tr" + attr("data-id", l.id) + ">")
			} else {
				w.raw("<tr>")
			}
			w.tag("th", " scope=\"row\"", l.name)
			for _, g := range groups {
				for _, c := range g.checks {
					matrixTD(w, c, l.id, perms)
				}
			}
			w.raw("</tr>")
		}
		w.raw("</tbody></table></div>")
	}
	w.h3("What each operation checks")
	w.head("Operation", "Resource type", "Permission", "Ancestors in the path, the top one first", "Source")
	for _, c := range checks {
		perm := c.Permission
		if c.Public {
			perm = "public: " + c.Reason
		}
		w.row(c.ID, shortID(c.Subject), c.ResourceType, perm, ancestorsShort(c, perms), sourceText(c.Source))
	}
	w.end()
	w.h3("Grant paths")
	w.raw("<ul class=\"grants\">")
	for _, p := range m.Access.Permissions {
		if !p.Complete {
			w.raw("<li class=\"incomplete\"" + attr("data-id", p.ID) + ">")
			w.text(p.ResourceType + "#" + p.Name + ": ⚠ incomplete: a limit was reached, so it may grant more roles than its paths name")
			w.raw("</li>")
		}
		for _, g := range p.Paths {
			w.raw("<li" + attr("data-id", g.ID) + ">")
			w.text(grantText(p, g))
			w.raw("</li>")
		}
	}
	w.raw("</ul>")
}

// matrixTD writes one cell of the access matrix, which names its check and its role. A cell whose role holds the
// operation, but not the read of the decided parent in its path, has the mark ⤒ too.
func matrixTD(w formWriter, c ArchCheck, role string, perms map[string]*ArchPermission) {
	cell := attr("data-check", c.ID) + attr("data-role", role)
	var missing []string
	if role != "" {
		missing = missingAncestors(c, role, perms)
	}
	if c.Public {
		if role == "" {
			w.raw("<td" + cell + "></td>")
			return
		}
		if len(missing) == 0 {
			w.raw("<td class=\"public\"" + cell + ">◇<span class=\"sr\"> public</span></td>")
			return
		}
	}
	paths := matrixCell(perms[c.Grants], role)
	// A role with no named path of a permission whose grant paths a limit cut short may hold it by a path past the
	// limit, so its cell says "may hold" and never "no grant" (security review of PR #383, L1). The line of the paths
	// that stop says so too, when the model names no path of the permission that stops (security review of PR #385,
	// L2).
	// A check whose permission the model does not hold says "may hold" too (security review of PR #388, Info).
	may := !c.Public && len(paths) == 0 && checkIncomplete(c, perms)
	if !c.Public && len(paths) == 0 && !may {
		w.raw("<td" + cell + ">·<span class=\"sr\"> no grant</span></td>")
		return
	}
	glyphs, names := cellGlyphs(paths)
	if c.Public {
		glyphs, names = "◇", []string{"public"}
	}
	switch {
	case may && perms[c.Grants] == nil && role != "":
		glyphs, names = mayHoldGlyph, []string{mayHoldMissingText}
	case may && perms[c.Grants] == nil:
		glyphs, names = mayHoldGlyph, []string{mayHoldMissingStopText}
	case may && role != "":
		glyphs, names = mayHoldGlyph, []string{mayHoldText}
	case may:
		glyphs, names = mayHoldGlyph, []string{mayHoldStopText}
	}
	if len(missing) > 0 {
		glyphs += "⤒"
		names = append(names, "also needs the read of "+strings.Join(missing, ", "))
	}
	label := strings.Join(names, ", ")
	class := "cell"
	if c.Public {
		class += " public"
	}
	if may {
		class += " may-hold"
	}
	w.raw("<td" + cell + "><button type=\"button\"" + attr("class", class) + attr("data-check", c.ID) + attr("data-role", role) + attr("aria-label", shortID(c.Subject)+": "+label) + attr("title", label) + ">")
	w.text(glyphs)
	w.raw("</button></td>")
}

// grantText writes one grant path in words: the role, the hops and its conditions.
func grantText(p ArchPermission, g ArchGrant) string {
	t := p.ResourceType + "#" + p.Name + ": "
	if g.Role != "" {
		t += shortID(g.Role)
	} else {
		t += "no role"
	}
	t += " [" + strings.Join(g.Kinds, ", ") + "] via " + strings.Join(g.Hops, " → ")
	if g.Caveat != "" {
		t += "; only when caveat " + g.Caveat + " holds"
	}
	if len(g.Requires) > 0 {
		t += "; only together with " + strings.Join(g.Requires, ", ")
	}
	if len(g.Except) > 0 {
		t += "; not where " + strings.Join(g.Except, ", ")
	}
	if g.Stops != "" {
		t += "; stops: " + g.Stops
	}
	return t
}

func platformForm(w formWriter, m *ArchitectureModel) {
	w.key("A table on purpose: each need is a fact with its kind, its name, its shape and the components that state it")
	needs := append([]ArchNeed(nil), m.Needs...)
	order := map[string]int{}
	for i, k := range []string{"ingress", "egress", "identity issuer", "backing store", "topic", "secret", "configuration", "cluster permission", "pod security"} {
		order[k] = i
	}
	sort.SliceStable(needs, func(i, j int) bool { return order[needs[i].Kind] < order[needs[j].Kind] })
	w.head("Kind", "Name", "Shape", "Stated by", "Source")
	for _, n := range needs {
		w.row(n.ID, n.Kind, n.Name, n.Shape, idList(n.StatedBy), sourceText(n.Source))
	}
	w.end()
}
