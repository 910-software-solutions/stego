package compiler

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"html"
	"sort"
	"strings"
)

// The architecture view (ST-GEN-TM20, ST-GEN-UC08): the one HTML file architecture/index.html that apply writes
// beside the architecture model. It embeds the model unchanged (ST-GEN-UC08-R07), the layout that apply computes
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
}

// RenderArchitectureView renders the architecture view from the bytes of the architecture model alone
// (ST-GEN-INV11). The same bytes always give the same view.
func RenderArchitectureView(model []byte) ([]byte, error) {
	var m ArchitectureModel
	if err := json.Unmarshal(model, &m); err != nil {
		return nil, fmt.Errorf("architecture view: the model does not decode: %w", err)
	}
	lower := bytes.ToLower(model)
	if bytes.Contains(lower, []byte("</script")) || bytes.Contains(lower, []byte("<!--")) {
		return nil, fmt.Errorf("architecture view: the model holds </script or <!--, so the view cannot embed it unchanged")
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
	out.WriteString(strings.NewReplacer("{{name}}", html.EscapeString(m.System.Name), "{{record}}", html.EscapeString(m.Record.Path),
		"{{archetype}}", html.EscapeString(m.System.Archetype+" "+m.System.ArchetypeVersion)).Replace(before))
	writeForms(&out, &m)
	out.WriteString(after)
	out.WriteString("<script type=\"application/json\" id=\"model\">")
	out.Write(model)
	out.WriteString("</script>\n<script type=\"application/json\" id=\"layout\">")
	out.Write(layout)
	out.WriteString("</script>\n<script>\n")
	out.WriteString(architectureViewScript)
	out.WriteString("</script>\n</body>\n</html>\n")
	return out.Bytes(), nil
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
	w.key("Entry: a collection that callers reach through the network edge", "Container: a unit that runs or stores", "Line: a call into the service, or a store it uses")
	w.head("Element", "Kind", "What it is", "Source")
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
	if len(m.Actions) > 0 {
		var names []string
		for _, a := range m.Actions {
			names = append(names, a.Name+" ("+a.Kind+")")
		}
		w.tag("p", " class=\"question\" id=\"form-no-flow-actions\"", "The model has a request flow only for each operation of a collection. These commands, queries and workers have no flow here; the access matrix shows what each one checks: "+strings.Join(names, ", ")+".")
	}
	for _, f := range m.Flows {
		o := ops[f.Operation]
		w.raw("<div class=\"flow\"" + attr("data-id", f.ID) + ">")
		title := shortID(f.Operation)
		if o.Method != "" {
			title = o.Method + " " + o.Path + " (" + title + ")"
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

// accessForm writes the access matrix (ST-GEN-UC08-R14): a line for each role and a cell for each operation,
// command, query and worker, with the marks of the kinds of grant in it. It shows no relationship of a real
// subject, because the model holds none.
func accessForm(w formWriter, m *ArchitectureModel) {
	w.h3("Access matrix: who may call each operation")
	keys := []string{"◇ public: the operation checks no permission", "· no grant"}
	for _, g := range grantGlyphs {
		keys = append(keys, g.glyph+" "+g.meaning)
	}
	w.key(keys...)
	perms := map[string]*ArchPermission{}
	for i := range m.Access.Permissions {
		perms[m.Access.Permissions[i].ID] = &m.Access.Permissions[i]
	}
	checks := m.Access.Checks
	w.raw("<div class=\"scroll\"><table class=\"matrix access\" aria-label=\"Access matrix\"><thead><tr>")
	w.tag("th", " scope=\"col\"", "Role")
	for _, c := range checks {
		what := c.ResourceType + "#" + c.Permission
		if c.Public {
			what = "public"
		}
		w.raw("<th scope=\"col\"" + attr("data-id", c.ID) + ">")
		w.text(shortID(c.Subject))
		w.tag("span", " class=\"sub\"", what)
		w.raw("</th>")
	}
	w.raw("</tr></thead><tbody>")
	roles := m.Access.Roles
	if len(roles) == 0 {
		w.raw("<tr><td" + attr("colspan", fmt.Sprint(len(checks)+1)) + ">The declared authorization schema grants no role a permission that an operation checks.</td></tr>")
	}
	for _, r := range roles {
		w.raw("<tr" + attr("data-id", r.ID) + ">")
		name := r.SubjectType + " as " + r.On + "#" + r.Relation
		if r.Wildcard {
			name = r.SubjectType + ":* as " + r.On + "#" + r.Relation
		}
		w.tag("th", " scope=\"row\"", name)
		for _, c := range checks {
			matrixTD(w, c, r.ID, perms[c.Grants])
		}
		w.raw("</tr>")
	}
	stopped := false
	for _, c := range checks {
		if len(matrixCell(perms[c.Grants], "")) > 0 {
			stopped = true
		}
	}
	if stopped {
		w.raw("<tr>")
		w.tag("th", " scope=\"row\"", "Paths that stop, with no role")
		for _, c := range checks {
			matrixTD(w, c, "", perms[c.Grants])
		}
		w.raw("</tr>")
	}
	w.raw("</tbody></table></div>")
	w.h3("What each operation checks")
	w.head("Operation", "Resource type", "Permission", "Source")
	for _, c := range checks {
		perm := c.Permission
		if c.Public {
			perm = "public: " + c.Reason
		}
		w.row(c.ID, shortID(c.Subject), c.ResourceType, perm, sourceText(c.Source))
	}
	w.end()
	w.h3("Grant paths")
	w.raw("<ul class=\"grants\">")
	for _, p := range m.Access.Permissions {
		for _, g := range p.Paths {
			w.raw("<li" + attr("data-id", g.ID) + ">")
			w.text(grantText(p, g))
			w.raw("</li>")
		}
	}
	w.raw("</ul>")
}

// matrixTD writes one cell of the access matrix.
func matrixTD(w formWriter, c ArchCheck, role string, p *ArchPermission) {
	if c.Public {
		if role == "" {
			w.raw("<td></td>")
			return
		}
		w.raw("<td class=\"public\">◇<span class=\"sr\"> public</span></td>")
		return
	}
	paths := matrixCell(p, role)
	if len(paths) == 0 {
		w.raw("<td>·<span class=\"sr\"> no grant</span></td>")
		return
	}
	glyphs, names := cellGlyphs(paths)
	label := strings.Join(names, ", ")
	w.raw("<td><button type=\"button\" class=\"cell\"" + attr("data-check", c.ID) + attr("data-role", role) + attr("aria-label", shortID(c.Subject)+": "+label) + ">")
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
