package compiler

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"reflect"
	"regexp"
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
			{"role:member#owner@user", "check:operation:organizations.read", "· no grant"},
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
			if len(head) != 1 || strings.Contains(textOf(head[0]), "incomplete: a limit was reached") != incomplete {
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

	t.Run("ST-GEN-UC08-R14-T04 The access matrix names the ancestors that a nested operation reads first, and marks each role that alone does not grant the read of one", func(t *testing.T) {
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
			for _, a := range c.Ancestors {
				if len(names) == 1 && !strings.Contains(textOf(names[0]), shortID(a.Entity)+" "+a.ResourceType+"#"+a.Permission) {
					t.Errorf("the head of %s does not name the ancestor %s and its permission: %q", c.ID, a.Entity, textOf(names[0]))
				}
			}
			for _, r := range am.Access.Roles {
				held := c.Public || perms[c.Grants][r.ID]
				missing := false
				for _, a := range c.Ancestors {
					missing = missing || (a.Grants != "" && !perms[a.Grants][r.ID])
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
		if got := textOf(cells["role:member#owner@user|check:operation:org-users.read"]); got != "●⤒" {
			t.Errorf("the owner of a member who may not view its organization has the cell %q, not ●⤒", got)
		}
		// The table of what each operation checks names the ancestors too.
		checks := p.all(func(n *html.Node) bool {
			return n.Data == "tr" && attrOf(n, "data-id") == "check:operation:org-users.read"
		})
		if len(checks) != 1 || !strings.Contains(textOf(checks[0]), "Organization organization#view") {
			t.Error("the table of what each operation checks does not name the ancestor of org-users.read")
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
