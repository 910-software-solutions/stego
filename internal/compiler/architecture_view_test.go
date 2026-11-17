package compiler

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// viewFixture reads a model of testdata/architecture and renders its view.
func viewFixture(t *testing.T, name string) ([]byte, []byte, *ArchitectureModel) {
	t.Helper()
	model, err := os.ReadFile("testdata/architecture/" + name)
	if err != nil {
		t.Fatal(err)
	}
	view, err := RenderArchitectureView(model)
	if err != nil {
		t.Fatal(err)
	}
	var m ArchitectureModel
	if err := json.Unmarshal(model, &m); err != nil {
		t.Fatal(err)
	}
	return model, view, &m
}

// parsed is the rendered view as a tree, with a few queries over it.
type parsed struct{ root *html.Node }

func parseView(t *testing.T, view []byte) parsed {
	t.Helper()
	root, err := html.Parse(bytes.NewReader(view))
	if err != nil {
		t.Fatal(err)
	}
	return parsed{root}
}

func attrOf(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func (p parsed) all(match func(*html.Node) bool) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && match(n) {
			out = append(out, n)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(p.root)
	return out
}

func (p parsed) byID(id string) *html.Node {
	if list := p.all(func(n *html.Node) bool { return attrOf(n, "id") == id }); len(list) == 1 {
		return list[0]
	}
	return nil
}

func textOf(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

func within(n *html.Node, match func(*html.Node) bool) []*html.Node {
	return parsed{n}.all(match)
}

func dataIDs(n *html.Node) map[string]bool {
	ids := map[string]bool{}
	for _, x := range within(n, func(x *html.Node) bool { return attrOf(x, "data-id") != "" }) {
		ids[attrOf(x, "data-id")] = true
	}
	return ids
}

func TestArchitectureView(t *testing.T) {
	model, view, m := viewFixture(t, "user-management.json")
	_, accessView, am := viewFixture(t, "access.json")
	page := parseView(t, view)

	t.Run("ST-GEN-UC08-R01-T02 The view of user-management holds the five aspects with every component, entity, field, flow and need", func(t *testing.T) {
		for _, v := range []string{"overview", "components", "data", "flows", "trust", "platform"} {
			if page.byID("form-"+v) == nil {
				t.Fatalf("the view has no text form of the view %s", v)
			}
		}
		components := dataIDs(page.byID("form-components"))
		for _, c := range m.Components {
			if !components[c.ID] {
				t.Errorf("the components view lacks %s", c.ID)
			}
		}
		data := page.byID("form-data")
		for _, e := range m.Entities {
			if !dataIDs(data)[e.ID] {
				t.Errorf("the data model view lacks %s", e.ID)
			}
			for _, f := range e.Fields {
				if !strings.Contains(textOf(data), f.Name) {
					t.Errorf("the data model view lacks the field %s of %s", f.Name, e.Name)
				}
			}
		}
		flows := within(page.byID("form-flows"), func(n *html.Node) bool { return attrOf(n, "class") == "flow" })
		if len(flows) != len(m.Operations) {
			t.Fatalf("the view has %d request flows for %d operations", len(flows), len(m.Operations))
		}
		for i, o := range m.Operations {
			if attrOf(flows[i], "data-id") != o.Flow {
				t.Errorf("flow %d is %s, not the flow %s of %s", i, attrOf(flows[i], "data-id"), o.Flow, o.ID)
			}
		}
		note := page.byID("form-no-flow-actions")
		for _, a := range m.Actions {
			if note == nil || !strings.Contains(textOf(note), a.Name) {
				t.Errorf("the request flows do not say that %s has no flow", a.ID)
			}
		}
		if len(m.Actions) == 0 {
			t.Error("the fixture has no command, query or worker")
		}
		platform := dataIDs(parseView(t, accessView).byID("form-platform"))
		for _, n := range am.Needs {
			if !platform[n.ID] {
				t.Errorf("the platform edges view lacks the need %s", n.ID)
			}
		}
	})

	t.Run("ST-GEN-UC08-R02-T01 The view loads no script, style, font or image from outside its own file", func(t *testing.T) {
		for _, v := range [][]byte{view, accessView} {
			p := parseView(t, v)
			for _, n := range p.all(func(*html.Node) bool { return true }) {
				for _, a := range n.Attr {
					switch a.Key {
					case "src", "href", "srcset", "poster", "data", "action", "formaction", "xlink:href":
						if !strings.HasPrefix(a.Val, "data:") && !strings.HasPrefix(a.Val, "#") {
							t.Errorf("<%s %s=%q> points outside the file", n.Data, a.Key, a.Val)
						}
					}
				}
				if n.Data == "link" || n.Data == "iframe" || n.Data == "object" || n.Data == "embed" || n.Data == "img" {
					t.Errorf("the view holds a <%s>", n.Data)
				}
			}
			code := string(v)
			for _, re := range []string{`@import`, `url\((?:'|")?(?:[^#'")d]|d[^a])`, `https?://(?:[^w]|w[^w]|ww[^w]|www[^.]|www\.[^w]|www\.w[^3])`, `\.src\s*=`, `fetch\(`, `XMLHttpRequest`, `WebSocket`, `EventSource`, `importScripts`, `\bimport\(`, `sendBeacon`} {
				if loc := regexp.MustCompile(re).FindStringIndex(code); loc != nil {
					t.Errorf("the view holds %q, which can load from outside the file", code[loc[0]:min(loc[1]+40, len(code))])
				}
			}
		}
	})

	t.Run("ST-GEN-UC08-R02-T04 The policy of the view admits each of its scripts by the hash of its text, and nothing else", func(t *testing.T) {
		for _, v := range [][]byte{view, accessView} {
			p := parseView(t, v)
			metas := p.all(func(n *html.Node) bool {
				return n.Data == "meta" && attrOf(n, "http-equiv") == "Content-Security-Policy"
			})
			if len(metas) != 1 {
				t.Fatalf("the view has %d policies", len(metas))
			}
			policy := attrOf(metas[0], "content")
			directives := map[string]string{}
			for _, d := range strings.Split(policy, ";") {
				name, value, _ := strings.Cut(strings.TrimSpace(d), " ")
				directives[name] = value
			}
			for name, want := range map[string]string{"default-src": "'none'", "style-src": "'unsafe-inline'", "img-src": "data:", "base-uri": "'none'", "form-action": "'none'", "require-trusted-types-for": "'script'"} {
				if directives[name] != want {
					t.Errorf("the policy gives %s %q, not %q", name, directives[name], want)
				}
			}
			var want []string
			for _, n := range p.all(func(n *html.Node) bool { return n.Data == "script" && attrOf(n, "type") == "" }) {
				want = append(want, scriptHash(textOf(n)))
			}
			if got := strings.Fields(directives["script-src"]); len(want) != 2 || !reflect.DeepEqual(got, want) {
				t.Errorf("script-src is %v, and the scripts of the view hash to %v", got, want)
			}
			if !strings.HasPrefix(string(v), "<!doctype html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n<meta http-equiv=\"Content-Security-Policy\"") {
				t.Error("the policy does not come before every script of the head")
			}
		}
	})

	t.Run("ST-GEN-UC08-R03-T03 Each element in the view names the declaration entry or registry part it comes from", func(t *testing.T) {
		forms := page.byID("forms")
		rows := map[string]string{}
		for _, n := range within(forms, func(n *html.Node) bool { return n.Data == "tr" && attrOf(n, "data-id") != "" }) {
			rows[attrOf(n, "data-id")] += textOf(n) + "\n"
		}
		check := func(id string, s ArchSource) {
			src := sourceText(s)
			if src == "" {
				t.Errorf("%s names no source", id)
				return
			}
			if !strings.Contains(rows[id], src) {
				t.Errorf("the view does not name the source %q of %s", src, id)
			}
		}
		for _, c := range m.Components {
			check(c.ID, c.Source)
		}
		for _, p := range m.Ports {
			check(p.ID, p.Source)
		}
		for _, b := range m.Bindings {
			check(b.ID, b.Source)
		}
		for _, s := range m.Slots {
			check(s.ID, s.Source)
		}
		for _, f := range m.Fills {
			check(f.ID, f.Source)
		}
		for _, e := range m.Entities {
			check(e.ID, e.Source)
			for _, f := range e.Fields {
				check(e.ID, f.Source)
			}
		}
		for _, c := range m.Collections {
			check(c.ID, c.Source)
		}
		for _, b := range m.Boundaries {
			check(b.ID, b.Source)
		}
		for _, c := range m.Access.Checks {
			check(c.ID, c.Source)
		}
		for _, s := range m.Settings {
			check(s.ID, s.Source)
		}
	})

	t.Run("ST-GEN-UC08-R03-T04 The view names the record it was written with", func(t *testing.T) {
		rec := page.byID("rec")
		if rec == nil || !strings.Contains(textOf(rec), m.Record.Path) || m.Record.Path != ".stego/state.yaml" {
			t.Fatalf("the header does not name the record .stego/state.yaml: %q", textOf(rec))
		}
	})

	t.Run("ST-GEN-UC08-R06-T03 An aspect with no element says so, and shows no empty diagram", func(t *testing.T) {
		if m.Aspects.PlatformEdges.State != "empty" {
			t.Fatal("the fixture must have no platform edge")
		}
		form := page.byID("form-platform")
		empty := within(form, func(n *html.Node) bool { return attrOf(n, "class") == "empty" })
		if len(empty) != 1 || !strings.Contains(textOf(empty[0]), m.Aspects.PlatformEdges.Reason) {
			t.Fatalf("the platform edges view does not state its reason %q", m.Aspects.PlatformEdges.Reason)
		}
		if len(within(form, func(n *html.Node) bool { return n.Data == "table" || n.Data == "svg" })) != 0 {
			t.Fatal("the empty platform edges view holds a table or a diagram")
		}
		for _, v := range []string{"overview", "components", "data", "flows", "trust"} {
			if len(within(page.byID("form-"+v), func(n *html.Node) bool { return n.Data == "table" || n.Data == "ol" })) == 0 {
				t.Errorf("the view %s shows no element", v)
			}
		}
		var layout viewLayout
		if err := json.Unmarshal([]byte(textOf(page.byID("layout"))), &layout); err != nil {
			t.Fatal(err)
		}
		if layout.Flows.W == 0 || len(layout.Trust.Wide.Regions) != len(m.Boundaries) {
			t.Fatal("the layout does not place the flows or every boundary")
		}
	})

	t.Run("ST-GEN-UC08-R07-T01 The view embeds the architecture model as the same JSON value", func(t *testing.T) {
		script := page.byID("model")
		if script == nil || attrOf(script, "type") != "application/json" {
			t.Fatal("the view has no model block")
		}
		var embedded, written any
		if err := json.Unmarshal([]byte(textOf(script)), &embedded); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(model, &written); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(embedded, written) {
			t.Fatal("the embedded model is another JSON value than architecture.json")
		}
	})

	t.Run("ST-GEN-UC08-R07-T02 The same model always gives the same view bytes", func(t *testing.T) {
		for i := 0; i < 20; i++ {
			again, err := RenderArchitectureView(bytes.Clone(model))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(again, view) {
				t.Fatalf("render %d gave other bytes", i+2)
			}
		}
		if bytes.Contains(view, []byte("Date.now")) || bytes.Contains(view, []byte("Math.random")) || bytes.Contains(view, []byte("new Date")) {
			t.Fatal("the viewer reads the clock or a random number")
		}
	})

	t.Run("ST-GEN-UC08-R07-T03 Markup in the free text of the model stays inside the model block and the text forms", func(t *testing.T) {
		hostile, value := hostileModel(t)
		out, err := RenderArchitectureView(hostile)
		if err != nil {
			t.Fatal(err)
		}
		p := parseView(t, out)
		if n := len(p.all(func(n *html.Node) bool { return n.Data == "img" })); n != 0 {
			t.Fatalf("the free text of the model became %d img elements", n)
		}
		scripts := p.all(func(n *html.Node) bool { return n.Data == "script" })
		if len(scripts) != 4 {
			t.Fatalf("the view has %d script blocks, not 4: the model text ended its block", len(scripts))
		}
		block := textOf(p.byID("model"))
		for _, bad := range []string{"<", ">", "&", "\u2028", "\u2029"} {
			if strings.Contains(block, bad) {
				t.Errorf("the model block holds %q unescaped", bad)
			}
		}
		var embedded, written any
		_ = json.Unmarshal([]byte(block), &embedded)
		_ = json.Unmarshal(hostile, &written)
		if !reflect.DeepEqual(embedded, written) {
			t.Fatal("the escaped model block is another JSON value")
		}
		if !strings.Contains(textOf(p.byID("form-platform")), value) {
			t.Fatal("the text forms do not show the hostile reason as text")
		}
		// The ancestor names of a nested operation show as text in the matrix head and in the check table.
		heads := p.all(func(n *html.Node) bool { return n.Data == "span" && attrOf(n, "class") == "sub ancestors" })
		if len(heads) == 0 || !strings.Contains(textOf(heads[0]), value) {
			t.Fatal("the matrix head does not show the hostile ancestor name as text")
		}
		cut := 0
		for _, head := range heads {
			if len(within(head, func(n *html.Node) bool { return n.Data == "span" && attrOf(n, "class") == "incomplete" })) > 0 {
				cut++
				if !strings.Contains(textOf(head), value+" organization#view "+incompleteMark) {
					t.Errorf("the head does not show the hostile name of an incomplete ancestor read as text: %q", textOf(head))
				}
			}
		}
		if cut == 0 {
			t.Fatal("the hostile model has no head with an incomplete ancestor read")
		}
		// The head of an operation whose permission a limit cut short shows its hostile resource type as text, and its
		// cells of the roles that may hold it are buttons that say so (security review of PR #383, L1).
		if heads := p.all(func(n *html.Node) bool {
			return n.Data == "th" && attrOf(n, "data-id") == "check:operation:organizations.read"
		}); len(heads) != 1 ||
			!strings.Contains(textOf(heads[0]), value+"#view") || !strings.Contains(attrOf(heads[0], "title"), value+"#view (incomplete: a limit was reached)") {
			t.Error("the head of organizations.read does not show its hostile resource type as text")
		}
		if may := p.all(func(n *html.Node) bool {
			return n.Data == "button" && attrOf(n, "data-check") == "check:operation:organizations.read" && strings.Contains(attrOf(n, "class"), "may-hold")
		}); len(may) == 0 || textOf(may[0]) != mayHoldGlyph {
			t.Error("the hostile model has no cell of organizations.read that says may hold")
		}
		// Each role head of the turned access matrix shows the role name as text, with a break only between its parts.
		var model ArchitectureModel
		if err := json.Unmarshal(hostile, &model); err != nil {
			t.Fatal(err)
		}
		if len(model.Access.Checks) <= len(model.Access.Roles) {
			t.Fatal("the access matrix of the hostile model is not turned")
		}
		for _, r := range model.Access.Roles {
			heads := p.all(func(n *html.Node) bool { return n.Data == "th" && attrOf(n, "data-id") == r.ID })
			if len(heads) != 1 || textOf(heads[0]) != roleName(r) {
				t.Errorf("the head of the role %s is not its name %q as text", r.ID, roleName(r))
				continue
			}
			breaks := 0
			for c := heads[0].FirstChild; c != nil; c = c.NextSibling {
				if c.Type == html.ElementNode && c.Data == "wbr" {
					breaks++
				}
			}
			if breaks != 2 {
				t.Errorf("the head of the role %s has %d breaks, want one after the word as and one after the #", r.ID, breaks)
			}
		}
	})

	t.Run("ST-GEN-UC08-R07-T05 Markup in the stop text of a grant path of a model with no role stays text in the note and the grant paths", func(t *testing.T) {
		out, err := RenderArchitectureView(hostileStopsModel(t))
		if err != nil {
			t.Fatal(err)
		}
		p := parseView(t, out)
		if n := len(p.all(func(n *html.Node) bool { return n.Data == "img" })); n != 0 {
			t.Fatalf("the stop text of the model became %d img elements", n)
		}
		if n := len(p.all(func(n *html.Node) bool { return n.Data == "script" })); n != 4 {
			t.Fatalf("the view has %d script blocks, not 4: the stop text ended its block", n)
		}
		// The note names the limit by the stop text up to its first ";" or " at ": here the whole hostile text.
		note := p.byID("no-role-named")
		if note == nil || !strings.Contains(textOf(note), "before a limit stopped it: "+hostileStops+".") {
			t.Fatalf("the note does not show the hostile stop text as text: %v", note)
		}
		grants := p.all(func(n *html.Node) bool { return n.Data == "ul" && attrOf(n, "class") == "grants" })
		if len(grants) != 1 || strings.Count(textOf(grants[0]), "stops: "+hostileStops) < 2 {
			t.Error("the grant paths do not show the hostile stop text as text")
		}
		for _, b := range p.all(func(n *html.Node) bool {
			return n.Data == "button" && attrOf(n, "data-role") == "" && attrOf(n, "data-check") != ""
		}) {
			if strings.Contains(attrOf(b, "title"), "<") {
				t.Errorf("the cell %s writes the stop text in its title: %q", attrOf(b, "data-check"), attrOf(b, "title"))
			}
		}
	})

	t.Run("ST-GEN-UC08-R08-T05 Every colour token keeps its contrast in both themes", func(t *testing.T) {
		for _, theme := range viewThemes(t) {
			for _, fg := range []string{"ink", "muted", "focus"} {
				for _, bg := range []string{"bg", "panel", "hi"} {
					if fg == "focus" && bg == "hi" {
						continue
					}
					if r := contrast(theme.colours[fg], theme.colours[bg]); r < 4.5 {
						t.Errorf("%s theme: text %s on %s has contrast %.2f, under 4.5", theme.name, fg, bg, r)
					}
				}
			}
			for _, mark := range []string{"line", "focus", "edge", "authentication", "handler", "slot", "repository", "store", "outside"} {
				for _, bg := range []string{"bg", "panel"} {
					if r := contrast(theme.colours[mark], theme.colours[bg]); r < 3 {
						t.Errorf("%s theme: mark %s on %s has contrast %.2f, under 3", theme.name, mark, bg, r)
					}
				}
			}
		}
	})

	t.Run("ST-GEN-UC08-R11-T01 The trust aspect shows each boundary, the tenancy and each default with its rule", func(t *testing.T) {
		form := page.byID("form-trust")
		ids := dataIDs(form)
		kinds := map[string]bool{}
		for _, b := range m.Boundaries {
			kinds[b.Kind] = true
			if !ids[b.ID] {
				t.Errorf("the trust view lacks the boundary %s", b.ID)
			}
		}
		for _, k := range []string{"network", "verified identity", "tenant scope", "fill code", "process"} {
			if !kinds[k] {
				t.Errorf("the fixture has no %s boundary", k)
			}
		}
		rows := map[string]string{}
		for _, n := range within(form, func(n *html.Node) bool { return n.Data == "tr" && attrOf(n, "data-id") != "" }) {
			rows[attrOf(n, "data-id")] = textOf(n)
		}
		for _, s := range m.Settings {
			row := rows[s.ID]
			if s.Default && !strings.Contains(row, "default, rule "+s.Source.Default) {
				t.Errorf("the default %s is not marked with its rule: %q", s.Path, row)
			}
			if !s.Default && strings.Contains(row, "default") {
				t.Errorf("the stated setting %s is marked as a default: %q", s.Path, row)
			}
		}
		for _, p := range []string{"tenancy.kind", "tenancy.claim"} {
			if !strings.Contains(textOf(form), p) {
				t.Errorf("the trust view does not show %s", p)
			}
		}
	})

	t.Run("ST-GEN-UC08-R14-T01 The access matrix has a line for each role and a cell for each operation, marked with the kind of grant", func(t *testing.T) {
		p := parseView(t, accessView)
		tables := p.all(func(n *html.Node) bool { return attrOf(n, "aria-label") == "Access matrix" })
		if len(tables) != 1 {
			t.Fatal("the view has no access matrix")
		}
		// More operations than roles turn the matrix: a column for each role and a line for each operation.
		turned := len(am.Access.Checks) > len(am.Access.Roles)
		if want := map[bool]string{true: "operations in lines", false: "roles in lines"}[turned]; attrOf(tables[0], "data-orientation") != want {
			t.Fatalf("the matrix of %d operations and %d roles is not %s", len(am.Access.Checks), len(am.Access.Roles), want)
		}
		heads := within(tables[0], func(n *html.Node) bool { return n.Data == "th" && strings.HasPrefix(attrOf(n, "data-id"), "role:") })
		if !turned {
			heads = within(tables[0], func(n *html.Node) bool { return n.Data == "tr" && strings.HasPrefix(attrOf(n, "data-id"), "role:") })
		}
		if len(heads) != len(am.Access.Roles) {
			t.Fatalf("the matrix has %d role lines for %d roles", len(heads), len(am.Access.Roles))
		}
		cells := map[string]string{}
		for _, td := range within(tables[0], func(n *html.Node) bool { return n.Data == "td" }) {
			key := attrOf(td, "data-role") + "|" + attrOf(td, "data-check")
			if _, twice := cells[key]; twice {
				t.Fatalf("the matrix has two cells for %s", key)
			}
			cells[key] = textOf(td)
		}
		for _, r := range am.Access.Roles {
			for _, c := range am.Access.Checks {
				if _, ok := cells[r.ID+"|"+c.ID]; !ok {
					t.Errorf("the matrix has no cell for %s and %s", r.ID, c.ID)
				}
			}
		}
		// The operations are grouped by collection, in the order of the collections.
		var groups []string
		for _, n := range within(tables[0], func(n *html.Node) bool { return n.Data == "th" && strings.HasSuffix(attrOf(n, "scope"), "group") }) {
			groups = append(groups, textOf(n))
		}
		var wantGroups []string
		for _, c := range am.Collections {
			wantGroups = append(wantGroups, c.Name)
		}
		if len(am.Actions) > 0 {
			wantGroups = append(wantGroups, "commands, queries and workers")
		}
		if strings.Join(groups, ",") != strings.Join(wantGroups, ",") {
			t.Errorf("the operations are grouped as %v, not by collection %v", groups, wantGroups)
		}
		cell := func(role, check string) string {
			got, ok := cells[role+"|"+check]
			if !ok {
				t.Fatalf("no cell for %s and %s", role, check)
			}
			return got
		}
		for _, c := range []struct{ role, check, want string }{
			{"role:organization#owner@user", "check:operation:organizations.read", "●"},
			{"role:organization#viewer@user", "check:operation:organizations.read", "?"},
			{"role:organization#auditor@user", "check:operation:organizations.read", "&"},
			{"role:organization#public@user:*", "check:operation:organizations.read", "−*"},
			{"role:organization#owner@user", "check:operation:org-users.read", "↑"},
			{"role:organization#viewer@user", "check:operation:org-users.read", "↑?"},
			{"role:member#owner@user", "check:operation:organizations.read", "⚠"},
			{"role:service#operator@user", "check:operation:organizations.delete", "· no grant"},
			{"role:service#operator@user", "check:operation:organizations.create", "◇ public"},
			{"role:organization#guest@user(expiration)", "check:operation:organizations.read", "⌛"},
		} {
			if got := cell(c.role, c.check); got != c.want {
				t.Errorf("the cell of %s and %s is %q, not %q", c.role, c.check, got, c.want)
			}
		}
		if !strings.Contains(textOf(tables[0]), "Paths that stop, with no role") || !strings.Contains(textOf(tables[0]), "↻") {
			t.Error("the matrix does not show the path that a recursion stops")
		}
		if !strings.Contains(textOf(tables[0]), "⊘") {
			t.Error("the matrix does not show the path that the work limit stops")
		}
		if am.Access.RolesComplete || p.byID("roles-incomplete") == nil || !strings.Contains(textOf(p.byID("roles-incomplete")), "Incomplete: a limit was reached") {
			t.Error("the matrix does not say that a limit cut its roles short")
		}
		for _, c := range am.Access.Checks {
			incomplete := false
			for _, perm := range am.Access.Permissions {
				incomplete = incomplete || (perm.ID == c.Grants && !perm.Complete)
			}
			head := p.all(func(n *html.Node) bool { return n.Data == "th" && attrOf(n, "data-id") == c.ID })
			own := 0
			if len(head) == 1 {
				own = len(within(head[0], func(n *html.Node) bool {
					return n.Data == "span" && attrOf(n, "class") == "sub incomplete" && textOf(n) == "⚠ incomplete: a limit was reached"
				}))
			}
			if len(head) != 1 || (own == 1) != incomplete || own > 1 {
				t.Errorf("the column of %s does not show whether its permission is complete (%v)", c.ID, !incomplete)
			}
		}
		if !strings.Contains(textOf(tables[0]), "user as organization#guest, until it expires") {
			t.Error("the matrix does not name the condition of a role with an expiration")
		}
		if strings.Contains(string(accessView), "relationship:") {
			t.Error("the view shows a relationship")
		}
	})

	t.Run("ST-GEN-UC08-R14-T04 The access matrix names the ancestors of a nested operation with what the handler checks on each, and marks each role that alone does not grant the read of a decided parent", func(t *testing.T) {
		p := parseView(t, accessView)
		perms := map[string]map[string]bool{}
		for _, perm := range am.Access.Permissions {
			perms[perm.ID] = map[string]bool{}
			for _, g := range perm.Paths {
				perms[perm.ID][g.Role] = true
			}
		}
		cells := map[string]*html.Node{}
		for _, td := range p.all(func(n *html.Node) bool { return n.Data == "td" && attrOf(n, "data-check") != "" }) {
			cells[attrOf(td, "data-role")+"|"+attrOf(td, "data-check")] = td
		}
		marked, clear := 0, 0
		for _, c := range am.Access.Checks {
			head := p.all(func(n *html.Node) bool { return n.Data == "th" && attrOf(n, "data-id") == c.ID })
			if len(head) != 1 {
				t.Fatalf("the matrix has no head for %s", c.ID)
			}
			names := within(head[0], func(n *html.Node) bool { return n.Data == "span" && attrOf(n, "class") == "sub ancestors" })
			if (len(names) == 1) != (len(c.Ancestors) > 0) {
				t.Errorf("the head of %s names the ancestors %d times; the check has %d", c.ID, len(names), len(c.Ancestors))
			}
			// A decided parent shows its permission; each other ancestor shows its existence check only, and no
			// permission (ST-SUP-RC20-OC24, ST-SUP-Q156).
			for _, a := range c.Ancestors {
				want := shortID(a.Entity) + " " + ancestorLinked
				if a.Decided {
					want = shortID(a.Entity) + " " + a.ResourceType + "#" + a.Permission
				}
				if len(names) == 1 && !strings.Contains(textOf(names[0]), want) {
					t.Errorf("the head of %s does not name the ancestor %s as %q: %q", c.ID, a.Entity, want, textOf(names[0]))
				}
				if len(names) == 1 && !a.Decided && strings.Contains(textOf(names[0]), "#") {
					t.Errorf("the head of %s names a permission of an ancestor that is not decided: %q", c.ID, textOf(names[0]))
				}
			}
			for _, r := range am.Access.Roles {
				held := c.Public || perms[c.Grants][r.ID]
				missing := false
				for _, a := range c.Ancestors {
					missing = missing || (a.Decided && a.Grants != "" && !perms[a.Grants][r.ID])
				}
				text := textOf(cells[r.ID+"|"+c.ID])
				if want := held && missing; strings.Contains(text, "⤒") != want {
					t.Errorf("the cell of %s and %s is %q; the mark of an ancestor read that the role does not grant is wanted: %v", r.ID, c.ID, text, want)
				}
				if held && missing {
					marked++
				} else if held && len(c.Ancestors) > 0 {
					clear++
				}
			}
		}
		if marked == 0 || clear == 0 {
			t.Fatalf("the fixture proves too little: %d marked and %d clear cells of nested operations", marked, clear)
		}
		// The owner of a member who may not view its organization reads and changes the member through the path, as
		// through a query: the read decides only the row. The list decides the read of the organization, so the owner
		// alone does not reach it.
		if got := textOf(cells["role:member#owner@user|check:operation:org-users.read"]); got != "●" {
			t.Errorf("the owner of a member who may not view its organization has the read cell %q, not ●", got)
		}
		if got := textOf(cells["role:member#owner@user|check:operation:org-users.list"]); got != "●⤒" {
			t.Errorf("the owner of a member who may not view its organization has the list cell %q, not ●⤒", got)
		}
		// The table of what each operation checks names the ancestors too, with what is checked on each.
		for id, want := range map[string]string{"check:operation:org-users.read": "Organization " + ancestorLinked, "check:operation:org-users.list": "Organization organization#view"} {
			checks := p.all(func(n *html.Node) bool { return n.Data == "tr" && attrOf(n, "data-id") == id })
			if len(checks) != 1 || !strings.Contains(textOf(checks[0]), want) {
				t.Errorf("the table of what each operation checks does not name the ancestor of %s as %q", id, want)
			}
		}
	})

	t.Run("ST-GEN-UC08-R14-T05 The access matrix marks each ancestor read whose grant paths a limit cut short as incomplete", func(t *testing.T) {
		p := parseView(t, accessView)
		perms := map[string]ArchPermission{}
		for _, perm := range am.Access.Permissions {
			perms[perm.ID] = perm
		}
		withCut, without := 0, 0
		for _, c := range am.Access.Checks {
			var cut []ArchAncestor
			for _, a := range c.Ancestors {
				if perm, ok := perms[a.Grants]; ok && !perm.Complete {
					cut = append(cut, a)
				}
			}
			head := p.all(func(n *html.Node) bool { return n.Data == "th" && attrOf(n, "data-id") == c.ID })
			if len(head) != 1 {
				t.Fatalf("the matrix has no head for %s", c.ID)
			}
			names := within(head[0], func(n *html.Node) bool { return n.Data == "span" && attrOf(n, "class") == "sub ancestors" })
			var marks []*html.Node
			if len(names) == 1 {
				marks = within(names[0], func(n *html.Node) bool { return n.Data == "span" && attrOf(n, "class") == "incomplete" })
			}
			row := p.all(func(n *html.Node) bool { return n.Data == "tr" && attrOf(n, "data-id") == c.ID })
			if len(row) != 1 {
				t.Fatalf("the table of what each operation checks has no row for %s", c.ID)
			}
			if len(cut) == 0 {
				without++
				if len(marks) != 0 || strings.Contains(textOf(row[0]), incompleteMark) || strings.Contains(attrOf(head[0], "title"), incompleteMark) {
					t.Errorf("the head or the check row of %s marks an ancestor read as incomplete, and none is", c.ID)
				}
				continue
			}
			withCut++
			if len(names) != 1 || len(marks) != len(cut) {
				t.Fatalf("the head of %s has %d marks of an incomplete ancestor read, not %d", c.ID, len(marks), len(cut))
			}
			for i, a := range cut {
				want := shortID(a.Entity) + " " + a.ResourceType + "#" + a.Permission + " " + incompleteMark
				if textOf(marks[i]) != incompleteMark {
					t.Errorf("the mark in the head of %s is %q", c.ID, textOf(marks[i]))
				}
				for _, text := range []string{textOf(names[0]), attrOf(head[0], "title"), textOf(row[0])} {
					if !strings.Contains(text, want) {
						t.Errorf("the ancestors of %s do not mark %s as incomplete: %q", c.ID, want, text)
					}
				}
			}
			// Each cell that names the ancestor read as missing says that its paths are incomplete.
			for _, b := range p.all(func(n *html.Node) bool { return n.Data == "button" && attrOf(n, "data-check") == c.ID }) {
				if strings.Contains(textOf(b), "⤒") && !strings.Contains(attrOf(b, "title"), "whose grant paths are incomplete: a limit was reached") {
					t.Errorf("the cell of %s and %s names a missing ancestor read and not that its paths are incomplete: %q", attrOf(b, "data-role"), c.ID, attrOf(b, "title"))
				}
			}
		}
		if withCut == 0 || without == 0 {
			t.Fatalf("the fixture proves too little: %d checks with an incomplete ancestor read and %d without", withCut, without)
		}
	})

	t.Run("ST-GEN-UC08-R14-T07 A cell of a role that no named path gives a permission whose grant paths a limit cut short says may hold, never no grant", func(t *testing.T) {
		// check reads each cell of the matrix and gives the number of cells that say may hold.
		check := func(name string, view []byte, m *ArchitectureModel) int {
			p := parseView(t, view)
			perms := map[string]ArchPermission{}
			for _, perm := range m.Access.Permissions {
				perms[perm.ID] = perm
			}
			cells := map[string]*html.Node{}
			for _, td := range p.all(func(n *html.Node) bool { return n.Data == "td" && attrOf(n, "data-check") != "" }) {
				cells[attrOf(td, "data-role")+"|"+attrOf(td, "data-check")] = td
			}
			may := 0
			for _, c := range m.Access.Checks {
				perm, ok := perms[c.Grants]
				for _, r := range m.Access.Roles {
					named := false
					for _, g := range perm.Paths {
						named = named || g.Role == r.ID
					}
					td := cells[r.ID+"|"+c.ID]
					if td == nil {
						t.Fatalf("%s: no cell for %s and %s", name, r.ID, c.ID)
					}
					buttons := within(td, func(n *html.Node) bool { return n.Data == "button" })
					want := !c.Public && ok && !perm.Complete && !named
					if !want {
						if strings.Contains(textOf(td), mayHoldGlyph) || (len(buttons) == 1 && strings.Contains(attrOf(buttons[0], "class"), "may-hold")) {
							t.Errorf("%s: the cell of %s and %s says may hold: %q", name, r.ID, c.ID, textOf(td))
						}
						continue
					}
					may++
					if strings.Contains(textOf(td), "no grant") || len(buttons) != 1 {
						t.Errorf("%s: the cell of %s and %s reads %q, not may hold", name, r.ID, c.ID, textOf(td))
						continue
					}
					b := buttons[0]
					if !strings.HasPrefix(textOf(b), mayHoldGlyph) || !strings.Contains(attrOf(b, "class"), "may-hold") ||
						!strings.Contains(attrOf(b, "title"), mayHoldText) || attrOf(b, "aria-label") != shortID(c.Subject)+": "+attrOf(b, "title") {
						t.Errorf("%s: the cell of %s and %s does not say may hold in its mark, title and screen reader text: %q, title %q, label %q",
							name, r.ID, c.ID, textOf(b), attrOf(b, "title"), attrOf(b, "aria-label"))
					}
				}
			}
			return may
		}
		if n := check("access.json", accessView, am); n == 0 {
			t.Fatal("the fixture proves too little: no cell of a role with no named path of an incomplete permission")
		}
		// variant renders access.json with the key complete of each named permission changed: true sets it, false
		// deletes it.
		variant := func(complete map[string]bool) ([]byte, *ArchitectureModel) {
			model, err := os.ReadFile("testdata/architecture/access.json")
			if err != nil {
				t.Fatal(err)
			}
			var raw map[string]any
			if err := json.Unmarshal(model, &raw); err != nil {
				t.Fatal(err)
			}
			for _, perm := range raw["access"].(map[string]any)["permissions"].([]any) {
				obj := perm.(map[string]any)
				if set, ok := complete[obj["id"].(string)]; ok && set {
					obj["complete"] = true
				} else if ok {
					delete(obj, "complete")
				}
			}
			if model, err = json.Marshal(raw); err != nil {
				t.Fatal(err)
			}
			view, err := RenderArchitectureView(model)
			if err != nil {
				t.Fatal(err)
			}
			var m ArchitectureModel
			if err := json.Unmarshal(model, &m); err != nil {
				t.Fatal(err)
			}
			return view, &m
		}
		// With the paths of organization#view complete, no cell says may hold, and the role reads no grant.
		view, m := variant(map[string]bool{"permission:organization#view": true})
		if n := check("complete", view, m); n != 0 {
			t.Errorf("the complete matrix has %d cells that say may hold", n)
		}
		p := parseView(t, view)
		if cells := p.all(func(n *html.Node) bool {
			return n.Data == "td" && attrOf(n, "data-role") == "role:member#owner@user" && attrOf(n, "data-check") == "check:operation:organizations.read"
		}); len(cells) != 1 || textOf(cells[0]) != "· no grant" {
			t.Error("the cell of a role with no path of a complete permission does not read no grant")
		}
		// A permission whose model has no key complete counts as cut short (security review of PR #383, L3): its head
		// is marked, and each role with no named path of it may hold it.
		view, m = variant(map[string]bool{"permission:member#view": false})
		if n := check("no key complete", view, m); n <= check("access.json", accessView, am) {
			t.Errorf("the matrix of a permission with no key complete has %d cells that say may hold, no more than the fixture", n)
		}
		p = parseView(t, view)
		if heads := p.all(func(n *html.Node) bool {
			return n.Data == "th" && attrOf(n, "data-id") == "check:operation:all-users.list"
		}); len(heads) != 1 ||
			!strings.Contains(textOf(heads[0]), "⚠ incomplete: a limit was reached") {
			t.Error("the head of a check whose permission has no key complete is not marked incomplete")
		}
	})

	t.Run("ST-GEN-UC08-R14-T09 With no role that a grant path names, the access matrix says that no role is granted only when no limit cut a path short, and else shows the paths that stop", func(t *testing.T) {
		const definite = "grants no role a permission that an operation checks"
		// A chain of rewrites one past the depth limit: every path of every checked permission stops at the limit.
		model, data := deriveFixture(t, chainFixture(t, maxGrantDepth+1))
		if len(model.Access.Roles) != 0 || model.Access.RolesComplete {
			t.Fatalf("the chain fixture proves too little: %d roles, roles complete %v", len(model.Access.Roles), model.Access.RolesComplete)
		}
		for _, perm := range model.Access.Permissions {
			if perm.Complete || len(perm.Paths) == 0 || !strings.Contains(perm.Paths[0].Stops, "depth limit") {
				t.Fatalf("the grant paths of %s do not stop at the depth limit: %+v", perm.ID, perm)
			}
		}
		view, err := RenderArchitectureView(data)
		if err != nil {
			t.Fatal(err)
		}
		p := parseView(t, view)
		if strings.Contains(textOf(p.root), definite) {
			t.Error("the matrix of grant paths that a limit cut short says that no role is granted")
		}
		note := p.byID("no-role-named")
		if note == nil || attrOf(note, "role") != "note" || !strings.Contains(attrOf(note, "class"), "incomplete") ||
			!strings.Contains(textOf(note), "no grant path reached a role before a limit stopped it: the depth limit of 12 nodes. So the model names no role.") ||
			!strings.Contains(textOf(note), "does not say that no role holds one") {
			t.Errorf("the matrix with no named role does not say that a role may hold a permission: %v", note)
		}
		if p.byID("roles-incomplete") == nil {
			t.Error("the matrix with no named role has no notice that a limit was reached")
		}
		tables := p.all(func(n *html.Node) bool { return n.Data == "table" && strings.Contains(attrOf(n, "class"), "access") })
		if len(tables) != 1 {
			t.Fatalf("the matrix with no named role has %d access tables, want the one with the line of the paths that stop", len(tables))
		}
		heads := within(tables[0], func(n *html.Node) bool { return n.Data == "th" && attrOf(n, "class") == "role" })
		if len(heads) != 1 || textOf(heads[0]) != "Paths that stop, with no role" {
			t.Errorf("the matrix with no named role does not have the one line of the paths that stop: %d role heads", len(heads))
		}
		for _, c := range model.Access.Checks {
			cells := within(tables[0], func(n *html.Node) bool {
				return n.Data == "td" && attrOf(n, "data-check") == c.ID && attrOf(n, "data-role") == ""
			})
			buttons := within(tables[0], func(n *html.Node) bool {
				return n.Data == "button" && attrOf(n, "data-check") == c.ID && attrOf(n, "data-role") == ""
			})
			switch {
			case len(cells) != 1:
				t.Errorf("%s has %d cells in the line of the paths that stop", c.ID, len(cells))
			case c.Public && len(buttons) != 0:
				t.Errorf("the public %s has a cell of paths that stop", c.ID)
			case !c.Public && (len(buttons) != 1 || !strings.Contains(attrOf(buttons[0], "title"), "depth limit")):
				t.Errorf("the cell of the paths that stop of %s does not name the depth limit: %q", c.ID, textOf(cells[0]))
			}
		}
		// A schema that grants nothing, with no limit reached: the matrix says that no role is granted, and draws no
		// table.
		model, data = deriveFixture(t, chainFixture(t, 0))
		if len(model.Access.Roles) != 0 || !model.Access.RolesComplete {
			t.Fatalf("the empty schema gives %d roles, roles complete %v", len(model.Access.Roles), model.Access.RolesComplete)
		}
		if view, err = RenderArchitectureView(data); err != nil {
			t.Fatal(err)
		}
		p = parseView(t, view)
		if n := p.byID("no-role"); n == nil || !strings.Contains(textOf(n), definite) {
			t.Error("the matrix of a schema that grants no role does not say so")
		}
		if p.byID("no-role-named") != nil || p.byID("roles-incomplete") != nil {
			t.Error("the matrix of a schema that grants no role with no limit reached says incomplete")
		}
		if n := len(p.all(func(n *html.Node) bool { return n.Data == "table" && strings.Contains(attrOf(n, "class"), "access") })); n != 0 {
			t.Errorf("the matrix of a schema that grants no role draws %d access tables", n)
		}
	})

	t.Run("ST-GEN-UC08-R14-T11 With no role named, the note of the access matrix names the limit that stopped the grant paths, and a cell of the line of the paths that stop with no path of an incomplete permission says may hold", func(t *testing.T) {
		const work = "the work limit of 20000 steps"
		// The work limit stops the expansion of doc#read and doc#write before a path reaches a role.
		model, data := deriveFixture(t, workFixture(t))
		if len(model.Access.Roles) != 0 || model.Access.RolesComplete {
			t.Fatalf("the work fixture proves too little: %d roles, roles complete %v", len(model.Access.Roles), model.Access.RolesComplete)
		}
		for _, perm := range model.Access.Permissions {
			if perm.Complete || len(perm.Paths) != 1 || !slices.Contains(perm.Paths[0].Kinds, "work limit") || slices.Contains(perm.Paths[0].Kinds, "depth limit") {
				t.Fatalf("the grant paths of %s do not stop at the work limit only: %+v", perm.ID, perm)
			}
		}
		matrix := func(data []byte) (parsed, string, *html.Node) {
			t.Helper()
			view, err := RenderArchitectureView(data)
			if err != nil {
				t.Fatal(err)
			}
			p := parseView(t, view)
			note := p.byID("no-role-named")
			if note == nil || attrOf(note, "role") != "note" || !strings.Contains(textOf(note), "does not say that no role holds one") {
				t.Fatalf("the matrix with no named role has no note that a role may hold a permission: %v", note)
			}
			if strings.Contains(textOf(p.root), "grants no role a permission that an operation checks") {
				t.Error("the matrix of grant paths that a limit cut short says that no role is granted")
			}
			tables := p.all(func(n *html.Node) bool { return n.Data == "table" && strings.Contains(attrOf(n, "class"), "access") })
			if len(tables) > 1 {
				t.Fatalf("the matrix has %d access tables", len(tables))
			}
			if len(tables) == 0 {
				return p, textOf(note), nil
			}
			return p, textOf(note), tables[0]
		}
		_, note, table := matrix(data)
		if !strings.Contains(note, "no grant path reached a role before a limit stopped it: "+work+".") || strings.Contains(note, "depth limit") {
			t.Errorf("the note does not name the work limit as the cause: %q", note)
		}
		if table == nil {
			t.Fatal("the matrix of paths that the work limit stopped draws no line of the paths that stop")
		}
		for _, c := range model.Access.Checks {
			buttons := within(table, func(n *html.Node) bool {
				return n.Data == "button" && attrOf(n, "data-check") == c.ID && attrOf(n, "data-role") == ""
			})
			if !c.Public && (len(buttons) != 1 || !strings.Contains(attrOf(buttons[0], "title"), "work limit") || !strings.Contains(textOf(buttons[0]), "⊘")) {
				t.Errorf("the cell of the paths that stop of %s does not show the work limit", c.ID)
			}
		}
		// A model, as a reader can edit it, with no path of doc#write: its cells in the line of the paths that stop
		// say may hold, never no grant, and the note names a limit that the model does not name.
		_, note, table = matrix(withoutPaths(t, data, "permission:doc#write"))
		if !strings.Contains(note, work+" and "+unnamedLimit+".") {
			t.Errorf("the note does not name each limit: %q", note)
		}
		if table == nil {
			t.Fatal("the edited matrix draws no line of the paths that stop")
		}
		may := 0
		for _, c := range model.Access.Checks {
			cells := within(table, func(n *html.Node) bool {
				return n.Data == "td" && attrOf(n, "data-check") == c.ID && attrOf(n, "data-role") == ""
			})
			if len(cells) != 1 {
				t.Fatalf("%s has %d cells in the line of the paths that stop", c.ID, len(cells))
			}
			if strings.Contains(textOf(cells[0]), "no grant") {
				t.Errorf("the cell of the paths that stop of %s says no grant for an incomplete permission", c.ID)
			}
			if c.Grants != "permission:doc#write" {
				continue
			}
			buttons := within(cells[0], func(n *html.Node) bool { return n.Data == "button" })
			if len(buttons) != 1 || textOf(buttons[0]) != mayHoldGlyph || !strings.Contains(attrOf(buttons[0], "class"), "may-hold") ||
				attrOf(buttons[0], "title") != mayHoldStopText || attrOf(buttons[0], "aria-label") != shortID(c.Subject)+": "+mayHoldStopText {
				t.Errorf("the cell of the paths that stop of %s does not say may hold: %q", c.ID, textOf(cells[0]))
			}
			may++
		}
		if may < 2 {
			t.Fatalf("the edited matrix proves too little: %d cells of doc#write", may)
		}
		// With no path of either permission, no line is drawn, and the note alone names the limit.
		_, note, table = matrix(withoutPaths(t, data, "permission:doc#write", "permission:doc#read"))
		if table != nil || !strings.Contains(note, "before a limit stopped it: "+unnamedLimit+".") {
			t.Errorf("the matrix with no path that stops draws a table, or its note is %q", note)
		}
	})

	t.Run("ST-GEN-UC08-R14-T13 A path that the path limit stops shows the path limit glyph and names the path limit in its cell, in the key and in the note, never the work limit", func(t *testing.T) {
		const limit = "the path limit of 512 paths"
		model, data := deriveFixture(t, pathFixture(t))
		view, err := RenderArchitectureView(data)
		if err != nil {
			t.Fatal(err)
		}
		p := parseView(t, view)
		form := p.byID("form-trust")
		keys := within(form, func(n *html.Node) bool {
			return n.Data == "li" && textOf(n) == "⊞ path limit: the expansion stops at its most grant paths"
		})
		if len(keys) != 1 {
			t.Error("the key of the access matrix does not give the path limit glyph")
		}
		note := p.byID("no-role-named")
		if note == nil || !strings.Contains(textOf(note), "no grant path reached a role before a limit stopped it: "+limit+".") || strings.Contains(textOf(note), "work limit") {
			t.Fatalf("the note does not name the path limit alone: %v", note)
		}
		tables := within(form, func(n *html.Node) bool { return n.Data == "table" && strings.Contains(attrOf(n, "class"), "access") })
		if len(tables) != 1 {
			t.Fatalf("the matrix has %d access tables", len(tables))
		}
		n := 0
		for _, c := range model.Access.Checks {
			if c.Public {
				continue
			}
			buttons := within(tables[0], func(n *html.Node) bool {
				return n.Data == "button" && attrOf(n, "data-check") == c.ID && attrOf(n, "data-role") == ""
			})
			if len(buttons) != 1 {
				t.Fatalf("%s has %d buttons in the line of the paths that stop", c.ID, len(buttons))
			}
			title := attrOf(buttons[0], "title")
			if !strings.Contains(textOf(buttons[0]), "⊞") || strings.Contains(textOf(buttons[0]), "⊘") ||
				!strings.Contains(title, "path limit") || strings.Contains(title, "work limit") || !strings.Contains(attrOf(buttons[0], "aria-label"), "path limit") {
				t.Errorf("the cell of the paths that stop of %s does not name the path limit alone: %q, %q", c.ID, textOf(buttons[0]), title)
			}
			n++
		}
		if n < 3 {
			t.Fatalf("the path fixture proves too little: %d checks that are not public", n)
		}
		// The grant paths list writes the stop text of the path limit, and no work limit.
		grants := within(form, func(n *html.Node) bool { return n.Data == "ul" && attrOf(n, "class") == "grants" })
		if len(grants) != 1 || !strings.Contains(textOf(grants[0]), "[arrow, path limit, recursion]") || !strings.Contains(textOf(grants[0]), "stops: "+limit) ||
			strings.Contains(textOf(grants[0]), "work limit") {
			t.Error("the grant paths do not name the path limit alone")
		}
	})

	t.Run("ST-GEN-UC08-R14-T14 A check whose grants name a permission that the model does not hold reads incomplete and may hold, never no grant", func(t *testing.T) {
		// grantsGone gives the model data with the grants of the check of docs.read set to a permission that the
		// model does not hold, as a reader can edit it.
		grantsGone := func(data []byte) []byte {
			t.Helper()
			var raw map[string]any
			if err := json.Unmarshal(data, &raw); err != nil {
				t.Fatal(err)
			}
			for _, c := range raw["access"].(map[string]any)["checks"].([]any) {
				if c.(map[string]any)["subject"] == "operation:docs.read" {
					c.(map[string]any)["grants"] = "permission:doc#gone"
				}
			}
			out, err := json.MarshalIndent(raw, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			return out
		}
		const check = "check:operation:docs.read"
		model, data := deriveFixture(t, archFixture(t))
		if len(model.Access.Roles) < 2 {
			t.Fatalf("the fixture proves too little: %d roles", len(model.Access.Roles))
		}
		view, err := RenderArchitectureView(grantsGone(data))
		if err != nil {
			t.Fatal(err)
		}
		p := parseView(t, view)
		heads := p.all(func(n *html.Node) bool { return n.Data == "th" && attrOf(n, "data-id") == check })
		if len(heads) != 1 || !strings.Contains(attrOf(heads[0], "title"), "doc#read (incomplete: the model holds no grant path of it)") ||
			!strings.Contains(textOf(heads[0]), "⚠ incomplete: the model holds no grant path of it") {
			t.Error("the head of docs.read does not say that its grants are incomplete")
		}
		if len(p.all(func(n *html.Node) bool {
			return n.Data == "li" && strings.HasPrefix(textOf(n), mayHoldGlyph+" in a cell: may hold: ") && strings.Contains(textOf(n), "or the model holds none of them")
		})) == 0 {
			t.Error("the key does not say that a cell may hold a permission that the model holds no grant path of")
		}
		cells := p.all(func(n *html.Node) bool { return n.Data == "td" && attrOf(n, "data-check") == check })
		if len(cells) != len(model.Access.Roles) {
			t.Fatalf("docs.read has %d cells for %d roles", len(cells), len(model.Access.Roles))
		}
		for _, cell := range cells {
			buttons := within(cell, func(n *html.Node) bool { return n.Data == "button" })
			if strings.Contains(textOf(cell), "no grant") || len(buttons) != 1 || textOf(buttons[0]) != mayHoldGlyph ||
				!strings.Contains(attrOf(buttons[0], "class"), "may-hold") || attrOf(buttons[0], "title") != mayHoldMissingText ||
				attrOf(buttons[0], "aria-label") != "docs.read: "+mayHoldMissingText {
				t.Errorf("the cell of docs.read for %s does not say may hold: %q", attrOf(cell, "data-role"), textOf(cell))
			}
		}
		// With no role and the roles complete, the matrix does not say that no role is granted when a check names a
		// permission that the model does not hold.
		_, data = deriveFixture(t, chainFixture(t, 0))
		view, err = RenderArchitectureView(grantsGone(data))
		if err != nil {
			t.Fatal(err)
		}
		p = parseView(t, view)
		if p.byID("no-role") != nil || strings.Contains(textOf(p.root), "grants no role a permission that an operation checks") {
			t.Error("the matrix says that no role is granted for a check whose permission the model does not hold")
		}
		if note := p.byID("no-role-missing"); note == nil || attrOf(note, "role") != "note" ||
			!strings.Contains(textOf(note), "the model holds no grant path of a permission that an operation checks") || !strings.Contains(textOf(note), "does not say that no role holds one") {
			t.Errorf("the matrix with no role has no note that a role may hold the permission that the model does not hold: %v", note)
		}
		if p.byID("no-role-named") != nil {
			t.Error("the matrix with no limit reached names a limit")
		}
	})

	t.Run("ST-GEN-UC08-R14-T16 The access matrix of a service with no authorization bound says that no permission is checked, and never incomplete or may hold", func(t *testing.T) {
		for name, data := range unboundModels(t) {
			var model ArchitectureModel
			if err := json.Unmarshal(data, &model); err != nil {
				t.Fatal(err)
			}
			checks := 0
			for _, c := range model.Access.Checks {
				if !c.Public && c.Grants == "" && c.Permission == "" {
					checks++
				}
			}
			if checks == 0 || len(model.Access.Roles) != 0 || !model.Access.RolesComplete {
				t.Fatalf("%s: the unbound model proves too little: %d checks with no permission, %d roles", name, checks, len(model.Access.Roles))
			}
			view, err := RenderArchitectureView(data)
			if err != nil {
				t.Fatal(err)
			}
			p := parseView(t, view)
			form := textOf(p.byID("form-trust"))
			if p.byID("no-role-missing") != nil || p.byID("no-role-named") != nil || p.byID("roles-incomplete") != nil {
				t.Errorf("%s: the matrix of a service with no authorization has a note that it is incomplete", name)
			}
			if p.byID("no-role") == nil {
				t.Errorf("%s: the matrix of a service with no authorization does not say that no role is granted", name)
			}
			for _, bad := range []string{"⚠ incomplete", "⚠ Incomplete", "holds no grant path of it", "holds no grant path of a permission"} {
				if strings.Contains(form, bad) {
					t.Errorf("%s: the trust view of a service with no authorization says %q", name, bad)
				}
			}
			if len(p.all(func(n *html.Node) bool { return n.Data == "button" && strings.Contains(attrOf(n, "class"), "may-hold") })) != 0 {
				t.Errorf("%s: a cell of a service with no authorization says may hold", name)
			}
		}
	})

	t.Run("ST-GEN-UC08-R14-T17 An ancestor read whose grants name a permission that the model does not hold reads incomplete, and its cells say that the role may hold it", func(t *testing.T) {
		model, data := ancestorGoneModel(t)
		view, err := RenderArchitectureView(data)
		if err != nil {
			t.Fatal(err)
		}
		p := parseView(t, view)
		const mark = "⚠ incomplete: the model holds no grant path of it"
		n, cells := 0, 0
		for _, c := range model.Access.Checks {
			gone := false
			for _, a := range c.Ancestors {
				gone = gone || a.Grants == "permission:shelf#gone"
			}
			if !gone {
				// An ancestor that is not decided is never marked incomplete: it names no permission.
				if heads := p.all(func(n *html.Node) bool { return n.Data == "th" && attrOf(n, "data-id") == c.ID }); len(c.Ancestors) > 0 &&
					(len(heads) != 1 || strings.Contains(textOf(heads[0]), "Shelf shelf#view") || strings.Contains(textOf(heads[0]), mark)) {
					t.Errorf("the head of %s names or marks the read of an ancestor that is not decided: %q", c.ID, textOf(heads[0]))
				}
				continue
			}
			n++
			heads := p.all(func(n *html.Node) bool { return n.Data == "th" && attrOf(n, "data-id") == c.ID })
			if len(heads) != 1 || !strings.Contains(textOf(heads[0]), "Shelf shelf#view "+mark) || !strings.Contains(attrOf(heads[0], "title"), "Shelf shelf#view "+mark) {
				t.Errorf("the head of %s does not mark the ancestor read of Shelf as incomplete", c.ID)
			}
			for _, b := range p.all(func(n *html.Node) bool {
				return n.Data == "button" && attrOf(n, "data-check") == c.ID && attrOf(n, "data-role") != ""
			}) {
				if !strings.Contains(textOf(b), "⤒") || !strings.Contains(attrOf(b, "title"), "Shelf (shelf#view), whose grant paths are incomplete: the model holds no grant path of it, so this role may hold it") {
					t.Errorf("the cell of %s and %s does not say that the role may hold the read of Shelf: %q", attrOf(b, "data-role"), c.ID, attrOf(b, "title"))
				}
				cells++
			}
		}
		if n == 0 || cells == 0 {
			t.Fatalf("the edited model proves too little: %d checks, %d cells", n, cells)
		}
	})

	t.Run("ST-GEN-UC08-R15-T01 The matrix of the data model is titled Operations each collection offers", func(t *testing.T) {
		data := page.byID("form-data")
		tables := within(data, func(n *html.Node) bool { return n.Data == "table" && attrOf(n, "class") == "matrix" })
		if len(tables) != 1 || attrOf(tables[0], "aria-label") != "Operations each collection offers" {
			t.Fatal("the data model has no matrix titled Operations each collection offers")
		}
		heads := within(data, func(n *html.Node) bool { return n.Data == "h3" && textOf(n) == "Operations each collection offers" })
		if len(heads) != 1 {
			t.Fatal("the matrix has no visible title Operations each collection offers")
		}
		if strings.Contains(strings.ToLower(textOf(data)), "access") {
			t.Fatal("the data model names access, so a reader can take its matrix for the access matrix")
		}
		rows := within(tables[0], func(n *html.Node) bool { return n.Data == "tr" && attrOf(n, "data-id") != "" })
		if len(rows) != len(m.Collections) {
			t.Fatalf("the matrix has %d rows for %d collections", len(rows), len(m.Collections))
		}
		for i, c := range m.Collections {
			offered := len(within(rows[i], func(n *html.Node) bool { return attrOf(n, "data-op") != "" }))
			if offered != len(c.Operations) {
				t.Errorf("%s offers %d operations, and the matrix marks %d", c.Name, len(c.Operations), offered)
			}
		}
	})
}

// viewTheme is the colour tokens of one theme of the view.
type viewTheme struct {
	name    string
	colours map[string][3]float64
}

// viewThemes reads the colour tokens of the light and the dark theme from the style of the view.
func viewThemes(t *testing.T) []viewTheme {
	t.Helper()
	read := func(block string) map[string][3]float64 {
		out := map[string][3]float64{}
		for _, m := range regexp.MustCompile(`--([a-z]+):#([0-9a-fA-F]{6})`).FindAllStringSubmatch(block, -1) {
			var c [3]float64
			for i := 0; i < 3; i++ {
				v, _ := strconv.ParseUint(m[2][i*2:i*2+2], 16, 8)
				c[i] = float64(v) / 255
			}
			out[m[1]] = c
		}
		return out
	}
	shell := architectureViewShell
	light := shell[strings.Index(shell, ":root{"):strings.Index(shell, "@media (prefers-color-scheme:dark)")]
	dark := shell[strings.Index(shell, `:root[data-theme="dark"]{`):]
	dark = dark[:strings.Index(dark, "}")]
	themes := []viewTheme{{"light", read(light)}, {"dark", read(dark)}}
	for _, th := range themes {
		if len(th.colours) != 15 {
			t.Fatalf("the %s theme has %d colour tokens", th.name, len(th.colours))
		}
	}
	return themes
}

// contrast is the WCAG contrast ratio of two sRGB colours.
func contrast(a, b [3]float64) float64 {
	lum := func(c [3]float64) float64 {
		var l [3]float64
		for i, v := range c {
			if v <= 0.04045 {
				l[i] = v / 12.92
			} else {
				l[i] = math.Pow((v+0.055)/1.055, 2.4)
			}
		}
		return 0.2126*l[0] + 0.7152*l[1] + 0.0722*l[2]
	}
	x, y := lum(a), lum(b)
	return (max(x, y) + 0.05) / (min(x, y) + 0.05)
}

// hostileValue is free text that would end a script block and run code if the view inserted it as markup.
const hostileValue = "</script><img src=x onerror=alert(1)><!-- \u2028 & end"

// unboundModels gives the models of the fixture and of the nested fixture with no authorization bound: each check
// names no permission, and no permission is checked.
func unboundModels(t *testing.T) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for name, in := range map[string]ArchitectureInput{"flat": archFixture(t), "nested": nestedFixture(t)} {
		in.Service.Authorization = nil
		_, data := deriveFixture(t, in)
		out[name] = data
	}
	return out
}

// ancestorGoneModel gives the model of the nested fixture with the grants of each ancestor read of Shelf set to
// permission:shelf#gone, which the model does not hold, as a reader can edit it.
func ancestorGoneModel(t *testing.T) (*ArchitectureModel, []byte) {
	t.Helper()
	model, _ := deriveFixture(t, nestedFixture(t))
	for i := range model.Access.Checks {
		for j := range model.Access.Checks[i].Ancestors {
			// Only a decided ancestor names grants: the shelf is decided as the parent of shelf-folders.list.
			if a := &model.Access.Checks[i].Ancestors[j]; a.Entity == "entity:Shelf" && a.Decided {
				a.Grants = "permission:shelf#gone"
			}
		}
	}
	data, err := json.MarshalIndent(model, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return model, data
}

// hostileStops is free text, as a reader can edit it into the stop text of a grant path: markup that would end a
// script block and run code, and control and bidirectional characters that would change the colors or reorder a line
// on a terminal. It holds no ";" and no " at ", so the name of the limit is the whole text.
const hostileStops = hostileValue + " \x1b[31m \u202eevil\u2066"

// hostileStopsModel gives the model of workFixture, which names no role because the work limit stops each grant
// path, with hostileStops as the stop text of each grant path (security review of PR #388, L2).
func hostileStopsModel(t *testing.T) []byte {
	t.Helper()
	_, data := deriveFixture(t, workFixture(t))
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if len(m["access"].(map[string]any)["roles"].([]any)) != 0 {
		t.Fatal("the work fixture names a role")
	}
	n := 0
	for _, perm := range m["access"].(map[string]any)["permissions"].([]any) {
		for _, g := range perm.(map[string]any)["paths"].([]any) {
			g.(map[string]any)["stops"] = hostileStops
			n++
		}
	}
	if n < 2 {
		t.Fatalf("the work fixture has %d grant paths", n)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("</script><img")) {
		t.Fatal("the hostile model does not hold the raw text")
	}
	return buf.Bytes()
}

// hostileModel gives the model of user-management with the hostile text in the reason of its empty aspect, the
// reason of each public check, the first step of each flow, the name of the system, and the name and the ID of a
// component, an entity, a collection and an actor, and the path parameter, entity, ID, reason and scope source of
// each ancestor check, whose grants name a role; one fill takes the ID __proto__, which an index of the elements by ID must hold as a plain key (#342 F5, F6).
func hostileModel(t *testing.T) ([]byte, string) {
	t.Helper()
	data, err := os.ReadFile("testdata/architecture/user-management.json")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	m["aspects"].(map[string]any)["platform_edges"].(map[string]any)["reason"] = hostileValue
	for _, c := range m["access"].(map[string]any)["checks"].([]any) {
		if c.(map[string]any)["public"] == true {
			c.(map[string]any)["reason"] = hostileValue
		}
		// The names of the parent chain check: the path parameter, the entity and the ID of each ancestor
		// (ST-SUP-RC20-OC24), as the view shows them in the matrix head, the check table and the cell details.
		// Each ancestor also gets a hostile reason and scope source, and no permission with grants that name a role:
		// the view must show the text as text and must not fail on a grant that is no permission (security review
		// of PR #374, L1, L4).
		ancestors, _ := c.(map[string]any)["ancestors"].([]any)
		for _, a := range ancestors {
			a := a.(map[string]any)
			a["param"] = hostileValue
			a["entity"] = "entity:" + hostileValue
			a["id"] = "ancestor:" + hostileValue
			a["reason"] = hostileValue
			a["scope_source"] = map[string]any{"declaration": hostileValue}
			delete(a, "permission")
			a["grants"] = m["access"].(map[string]any)["roles"].([]any)[0].(map[string]any)["id"]
		}
		// One more ancestor with a hostile name reads organization#view, whose grant paths a limit cut short, so the
		// view writes the name in the marks of an incomplete ancestor read too (security review of PR #374, L2).
		if len(ancestors) > 0 {
			c.(map[string]any)["ancestors"] = append(ancestors, map[string]any{"id": "ancestor:" + hostileValue + "/cut", "entity": "entity:" + hostileValue,
				"param": hostileValue, "resource_type": "organization", "permission": "view", "public": false, "decided": true, "grants": "permission:organization#view",
				"source": map[string]any{"declaration": "entities[0].permissions.read"}, "scope_source": map[string]any{"declaration": hostileValue}})
		}
	}
	for _, perm := range m["access"].(map[string]any)["permissions"].([]any) {
		if perm.(map[string]any)["id"] == "permission:organization#view" {
			perm.(map[string]any)["complete"] = false
		}
	}
	// Each check of organization#view, whose grant paths a limit cut short, gets a hostile resource type, which the
	// cells of the roles that may hold it and their details write (security review of PR #383, L1).
	for _, c := range m["access"].(map[string]any)["checks"].([]any) {
		if c.(map[string]any)["grants"] == "permission:organization#view" {
			c.(map[string]any)["resource_type"] = hostileValue
		}
	}
	for _, f := range m["flows"].([]any) {
		f.(map[string]any)["steps"].([]any)[0].(map[string]any)["text"] = hostileValue
	}
	m["system"].(map[string]any)["name"] = hostileValue
	for _, list := range []string{"components", "entities", "collections", "actors"} {
		e := m[list].([]any)[0].(map[string]any)
		kind, _, _ := strings.Cut(e["id"].(string), ":")
		e["name"], e["id"] = hostileValue, kind+":"+hostileValue
	}
	m["fills"].([]any)[0].(map[string]any)["id"] = "__proto__"
	// A role name with quotes, which the escape writes as entities, and the # and the word as, where the name may
	// break (security review of PR #345, F3).
	m["access"].(map[string]any)["roles"].([]any)[0].(map[string]any)["subject_type"] = `it's "x"`
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("</script><img")) {
		t.Fatal("the hostile model does not hold the raw text")
	}
	return buf.Bytes(), strings.ReplaceAll(hostileValue, "\u2028", "\u2028")
}
