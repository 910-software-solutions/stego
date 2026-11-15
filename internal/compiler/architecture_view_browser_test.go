package compiler

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// writeView renders a model of testdata/architecture into a folder and gives the file address of the view.
func writeView(t *testing.T, name string) (string, *ArchitectureModel) {
	t.Helper()
	_, view, m := viewFixture(t, name)
	dir := filepath.Join(t.TempDir(), "architecture")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "index.html")
	if err := os.WriteFile(path, view, 0o644); err != nil {
		t.Fatal(err)
	}
	return "file://" + path, m
}

// modelIndex gives each element of the model by its ID, as the decoded JSON, with every ID that it holds.
func modelIndex(t *testing.T, name string) map[string]map[string]bool {
	t.Helper()
	data, err := os.ReadFile("testdata/architecture/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	elements := map[string]any{}
	add := func(list any) {
		items, _ := list.([]any)
		for _, it := range items {
			if e, ok := it.(map[string]any); ok {
				if id, ok := e["id"].(string); ok {
					elements[id] = e
				}
			}
		}
	}
	for _, key := range []string{"containers", "components", "mixins", "ports", "bindings", "slots", "fills", "entities", "references",
		"collections", "operations", "actions", "flows", "needs", "boundaries", "settings", "authn_points", "authz_points"} {
		add(raw[key])
	}
	access := raw["access"].(map[string]any)
	add(access["checks"])
	add(access["permissions"])
	add(access["roles"])
	for _, e := range raw["entities"].([]any) {
		add(e.(map[string]any)["fields"])
	}
	for _, p := range access["permissions"].([]any) {
		add(p.(map[string]any)["paths"])
	}
	refs := map[string]map[string]bool{}
	for id, e := range elements {
		held := map[string]bool{}
		var walk func(v any, top bool)
		walk = func(v any, top bool) {
			switch x := v.(type) {
			case string:
				if _, ok := elements[x]; ok && x != id {
					held[x] = true
				}
			case []any:
				for _, y := range x {
					walk(y, false)
				}
			case map[string]any:
				for k, y := range x {
					if k == "source" || (top && k == "id") {
						continue
					}
					walk(y, false)
				}
			}
		}
		walk(e, true)
		refs[id] = held
	}
	return refs
}

// expectedLinks gives what the model connects to an element: each element that it holds or that holds it, and the
// other ends of each binding and reference that holds it.
func expectedLinks(refs map[string]map[string]bool, id string) []string {
	set := map[string]bool{}
	for x := range refs[id] {
		set[x] = true
	}
	for other, held := range refs {
		if !held[id] {
			continue
		}
		set[other] = true
		if strings.HasPrefix(other, "binding:") || strings.HasPrefix(other, "reference:") {
			for x := range held {
				set[x] = true
			}
		}
	}
	delete(set, id)
	var out []string
	for x := range set {
		out = append(out, x)
	}
	sort.Strings(out)
	return out
}

func TestArchitectureViewBrowser(t *testing.T) {
	b := startBrowser(t)
	plain, m := writeView(t, "user-management.json")
	access, am := writeView(t, "access.json")
	b.viewport(1280, 900)
	b.media("screen", "light", "no-preference")

	t.Run("ST-GEN-UC08-R02-T02 The view opens from its file address with no server, no network request and no error", func(t *testing.T) {
		b.open(plain + "#view=overview")
		for _, v := range []string{"overview", "components", "data", "flows", "trust", "platform"} {
			var drawn struct{ Title, Shape string }
			b.eval(`(function(){ document.querySelector('#nav button[data-view="`+v+`"]').click();
				var v = document.getElementById("view");
				return {Title: document.getElementById("vtitle").textContent,
					Shape: v.querySelector("svg") ? "svg" : v.querySelector(".empty") ? "empty" : v.querySelector("table") ? "table" : "none"}; })()`, &drawn)
			if !strings.HasSuffix(drawn.Title, m.System.Name) || drawn.Shape == "none" {
				t.Errorf("the view %s did not render: %+v", v, drawn)
			}
		}
		blocked, errors := b.problems()
		if len(blocked) > 0 || len(errors) > 0 {
			t.Fatalf("the view made network requests %v or errors %v", blocked, errors)
		}
	})

	t.Run("ST-GEN-UC08-R04-T01 Selecting an element of each kind shows what the model connects to it, and only that", func(t *testing.T) {
		refs := modelIndex(t, "access.json")
		b.open(access + "#view=components")
		for _, id := range []string{"component:rest-api", "port:storage-adapter", "component:postgres-adapter", "slot:org-users.before_create",
			"fill:rbac-policy", "entity:User", "operation:org-users.create", "need:backing-store/user-management-db"} {
			var shown []string
			b.eval(`(function(){ location.hash = "view=components&sel=`+id+`"; window.dispatchEvent(new HashChangeEvent("hashchange"));
				var out = {};
				document.querySelectorAll("#details li[data-id], #details li[data-flow]").forEach(function(li){ out[li.getAttribute("data-id") || li.getAttribute("data-flow")] = 1; });
				if (document.getElementById("detailsTitle") === null) return ["no details"];
				return Object.keys(out).sort(); })()`, &shown)
			want := expectedLinks(refs, id)
			if !slices.Equal(shown, want) {
				t.Errorf("selecting %s shows\n %v\nand the model connects\n %v", id, shown, want)
			}
		}
		var source string
		b.eval(`document.querySelector("#details dd[data-source]").textContent`, &source)
		if !strings.HasPrefix(source, "registry ") && !strings.HasPrefix(source, "service.yaml ") {
			t.Errorf("the details name no source: %q", source)
		}
		_ = am
	})

	t.Run("ST-GEN-UC08-R04-T02 A reader follows the flow that creates a User step by step from its operation to its store and its events", func(t *testing.T) {
		b.open(plain + "#view=flows&flow=org-users.create&step=1")
		var all int
		b.eval(`document.querySelectorAll(".mini button[data-flow]").length`, &all)
		if all != len(m.Flows) {
			t.Fatalf("the overview of flows shows %d flows for %d", all, len(m.Flows))
		}
		var flow ArchFlow
		for _, f := range m.Flows {
			if f.ID == "flow:org-users.create" {
				flow = f
			}
		}
		var kinds []string
		for i := 0; i < len(flow.Steps)+2; i++ {
			var cur struct{ Step, ID, Kind, Live string }
			b.eval(`(function(){ var c = document.querySelector(".steps li.cur button");
				return {Step: c.getAttribute("data-step"), ID: c.getAttribute("data-id"), Kind: c.querySelector(".k").textContent.split(" · ")[0], Live: document.getElementById("live").textContent}; })()`, &cur)
			if len(kinds) == 0 || kinds[len(kinds)-1] != cur.Step+" "+cur.Kind {
				kinds = append(kinds, cur.Step+" "+cur.Kind)
				st := flow.Steps[len(kinds)-1]
				if cur.ID != st.Element || !strings.Contains(cur.Live, st.Text) {
					t.Errorf("step %s shows %s, not %s, or the live region says %q", cur.Step, cur.ID, st.Element, cur.Live)
				}
			}
			b.key("ArrowRight")
		}
		var want []string
		for _, s := range flow.Steps {
			want = append(want, itoaTest(s.N)+" "+s.Kind)
		}
		if !slices.Equal(kinds, want) {
			t.Fatalf("the steps were\n %v\nnot\n %v", kinds, want)
		}
		order := strings.Join(kinds, ",")
		for _, k := range []string{"operation", "authenticate", "handle", "slot", "repository", "store", "event"} {
			if !strings.Contains(order, " "+k) {
				t.Errorf("the flow has no %s step", k)
			}
		}
		if !strings.HasSuffix(order, " event") {
			t.Error("the flow does not end with its event")
		}
		var exits int
		b.eval(`document.querySelectorAll(".steps .exit").length`, &exits)
		if exits != len(flow.Exits) {
			t.Errorf("the flow shows %d exits for %d", exits, len(flow.Exits))
		}
	})

	t.Run("ST-GEN-UC08-R06-T04 The platform edges view of a service with no need shows the reason and no diagram", func(t *testing.T) {
		b.open(plain + "#view=platform")
		var got struct {
			Text  string
			Shape bool
		}
		b.eval(`(function(){ var v = document.getElementById("view"); return {Text: v.textContent, Shape: !!v.querySelector("svg, table")}; })()`, &got)
		if !strings.Contains(got.Text, m.Aspects.PlatformEdges.Reason) || got.Shape {
			t.Fatalf("the empty platform edges view shows %+v", got)
		}
	})

	t.Run("ST-GEN-UC08-R08-T01 A keyboard alone reaches every view and diagram element, with a visible focus", func(t *testing.T) {
		b.eval(`localStorage.setItem("stego-arch-tour", "seen")`, nil)
		b.open(plain + "#view=components")
		var reached struct{ Nav, El bool }
		var active struct{ ID, Outline, Stroke string }
		for i := 0; i < 80 && !reached.El; i++ {
			b.key("Tab")
			b.eval(`(function(){ var a = document.activeElement, box = a.querySelector && a.querySelector(".box");
				return {ID: a.getAttribute("data-id") || a.getAttribute("data-view") || a.id || a.tagName,
					Outline: getComputedStyle(a).outlineStyle+" "+getComputedStyle(a).outlineWidth,
					Stroke: box ? getComputedStyle(box).strokeWidth : ""}; })()`, &active)
			t.Logf("tab %d: %s", i, active.ID)
			if strings.HasPrefix(active.ID, "component:") || strings.HasPrefix(active.ID, "slot:") {
				reached.El = true
				if active.Stroke != "4px" {
					t.Errorf("the focused element %s shows no focus ring: stroke %s", active.ID, active.Stroke)
				}
			} else if active.ID == "components" || active.ID == "overview" {
				reached.Nav = true
				if !strings.HasPrefix(active.Outline, "solid") {
					t.Errorf("the focused view button shows no focus ring: %s", active.Outline)
				}
			}
		}
		if !reached.Nav || !reached.El {
			t.Fatalf("Tab did not reach the views and a diagram element: %+v", reached)
		}
		first := active.ID
		b.key("Enter")
		var title string
		b.eval(`(document.getElementById("detailsTitle")||{}).textContent || ""`, &title)
		if title == "" {
			t.Fatalf("Enter on %s opened no details", first)
		}
		b.key("ArrowDown")
		b.key("ArrowRight")
		var moved string
		b.eval(`document.activeElement.getAttribute("data-id") || ""`, &moved)
		if moved == "" || moved == first {
			t.Errorf("the arrow keys did not move the focus from %s", first)
		}
		b.key("Escape")
		b.eval(`(document.getElementById("detailsTitle")||{}).textContent || ""`, &title)
		if title != "" {
			t.Error("Escape did not close the details")
		}
		var unnamed []string
		b.eval(`(function(){ var out = []; document.querySelectorAll("button, a[href], [tabindex='0'], input").forEach(function(e){
			if (e.offsetParent === null && !(e instanceof SVGElement)) return;
			var n = (e.getAttribute("aria-label") || e.textContent || e.getAttribute("placeholder") || "").trim(); if (!n) out.push(e.outerHTML.slice(0, 80)); }); return out; })()`, &unnamed)
		if len(unnamed) > 0 {
			t.Errorf("elements with no accessible name: %v", unnamed)
		}
	})

	t.Run("ST-GEN-UC08-R08-T02 The text form of each diagram holds the same elements as the diagram", func(t *testing.T) {
		for _, view := range []struct{ name, kinds string }{{"overview", ""}, {"components", ""}, {"data", "entity:"}} {
			b.open(access + "#view=" + view.name)
			var got struct{ Drawn, Text []string }
			b.eval(`(function(){ var d = {}, x = {};
				document.querySelectorAll("#view svg .el").forEach(function(g){ d[g.getAttribute("data-id")] = 1; });
				document.getElementById("textToggle").click();
				document.querySelectorAll("#view [data-id]").forEach(function(r){ var id = r.getAttribute("data-id"); if (d[id]) x[id] = 1; });
				return {Drawn: Object.keys(d).sort(), Text: Object.keys(x).sort()}; })()`, &got)
			if len(got.Drawn) == 0 || !slices.Equal(got.Drawn, got.Text) {
				t.Errorf("the %s diagram draws %v and its text form holds %v", view.name, got.Drawn, got.Text)
			}
		}
		b.open(access + "#view=flows&flow=org-users.create")
		var steps struct{ Drawn, Text []string }
		b.eval(`(function(){ var d = [], x = [];
			document.querySelectorAll(".steps li button").forEach(function(s){ d.push(s.getAttribute("data-id")); });
			document.querySelectorAll('#form-flows .flow[data-id="flow:org-users.create"] li').forEach(function(s){ x.push(s.getAttribute("data-id")); });
			return {Drawn: d, Text: x}; })()`, &steps)
		if len(steps.Drawn) == 0 || !slices.Equal(steps.Drawn, steps.Text) {
			t.Errorf("the flow shows %v and its text form %v", steps.Drawn, steps.Text)
		}
	})

	t.Run("ST-GEN-UC08-R08-T03 All text of every view keeps its contrast in the light and the dark theme", func(t *testing.T) {
		for _, theme := range []string{"light", "dark"} {
			b.media("screen", theme, "no-preference")
			b.open(access + "#view=overview")
			for _, v := range []string{"overview", "components", "data", "flows", "trust", "platform"} {
				var bad []string
				b.eval(`(function(){ document.querySelector('#nav button[data-view="`+v+`"]').click(); document.getElementById("details").classList.remove("closed");
					location.hash.indexOf("sel=") < 0 && document.querySelector("#view .el") && document.querySelector("#view .el").dispatchEvent(new MouseEvent("click", {bubbles: true}));
					`+contrastScript+` })()`, &bad)
				if len(bad) > 0 {
					t.Errorf("%s theme, view %s: text under its contrast: %v", theme, v, bad)
				}
			}
		}
		b.media("screen", "light", "no-preference")
	})

	t.Run("ST-GEN-UC08-R08-T04 Every view fits 400 px with no page scroll to the side, in both themes and with reduced motion", func(t *testing.T) {
		b.viewport(400, 800)
		defer b.viewport(1280, 900)
		for _, theme := range []string{"light", "dark"} {
			b.media("screen", theme, "reduce")
			for _, text := range []string{"", "&text=1"} {
				for _, v := range []string{"overview", "components", "data", "flows", "trust", "platform"} {
					b.open(access + "#view=" + v + text)
					var got struct {
						Scroll, Width int
						Motion        string
					}
					b.eval(`(function(){ return {Scroll: document.documentElement.scrollWidth, Width: window.innerWidth,
						Motion: matchMedia("(prefers-reduced-motion: reduce)").matches ? getComputedStyle(document.querySelector(".btn")).transitionDuration : "not emulated"}; })()`, &got)
					if got.Scroll > got.Width {
						var wide []string
						b.eval(`Array.prototype.filter.call(document.querySelectorAll("body *"), function(e){ if (e.getBoundingClientRect().right <= 401) return false; for (var p = e.parentElement; p; p = p.parentElement){ var o = getComputedStyle(p).overflowX; if (o === "auto" || o === "scroll" || o === "hidden") return false; } return true; }).slice(0, 8).map(function(e){ return e.tagName+"."+e.className+" "+Math.round(e.getBoundingClientRect().right); })`, &wide)
						t.Log(wide)
						t.Errorf("%s theme, view %s%s: the page is %d px wide at %d px", theme, v, text, got.Scroll, got.Width)
					}
					if got.Motion != "0s" {
						t.Errorf("reduced motion is not respected: %s", got.Motion)
					}
				}
			}
		}
		b.media("screen", "light", "no-preference")
	})

	t.Run("ST-GEN-UC08-R09-T01 The guided tour can be skipped and started again, names the key terms, walks one flow and ends with a self-check question", func(t *testing.T) {
		b.eval(`localStorage.clear()`, nil)
		b.open(plain)
		var offer bool
		b.eval(`!!document.getElementById("tourOffer")`, &offer)
		if !offer {
			t.Fatal("the first visit offers no tour")
		}
		b.eval(`document.getElementById("tourNo").click()`, nil)
		b.open(plain)
		b.eval(`!!document.getElementById("tourOffer")`, &offer)
		if offer {
			t.Fatal("the tour is offered again after the reader skipped it")
		}
		b.eval(`document.getElementById("tourBtn").click()`, nil)
		var terms []string
		b.eval(`Array.prototype.map.call(document.querySelectorAll("#tour dt"), function(d){ return d.textContent; })`, &terms)
		if !slices.Equal(terms, []string{"Component", "Layer", "Port", "Slot", "Fill"}) {
			t.Fatalf("the tour does not name the key terms first: %v", terms)
		}
		var walked bool
		var question string
		for i := 0; i < 8; i++ {
			var stop struct{ View, Title, Question string }
			b.eval(`(function(){ return {View: location.hash, Title: document.getElementById("tourTitle").textContent, Question: (document.getElementById("tourQuestion")||{}).textContent || ""}; })()`, &stop)
			if strings.Contains(stop.View, "view=flows") && strings.Contains(stop.Title, "worked example") {
				walked = true
			}
			if stop.Question != "" {
				question = stop.Question
				break
			}
			b.eval(`document.getElementById("tourNext").click()`, nil)
		}
		if !walked || question == "" {
			t.Fatalf("the tour did not walk a flow (%v) or end with a question (%q)", walked, question)
		}
		for _, study := range bo09StudyQuestions {
			if strings.EqualFold(strings.TrimSpace(question), study) {
				t.Fatalf("the self-check question is in the ST-BO09 study set: %q", question)
			}
		}
		var answer struct {
			Hidden bool
			Text   string
		}
		b.eval(`(function(){ var a = document.getElementById("tourAnswer"); var h = a.hidden; document.getElementById("tourReveal").click(); return {Hidden: h && !a.hidden, Text: a.textContent}; })()`, &answer)
		if !answer.Hidden || !strings.HasPrefix(answer.Text, "Answer: ") {
			t.Fatalf("the answer is not given on demand: %+v", answer)
		}
		b.eval(`document.getElementById("tourNext").click()`, nil)
		var open bool
		b.eval(`!!document.getElementById("tour")`, &open)
		if open {
			t.Fatal("Done did not close the tour")
		}
		b.eval(`document.getElementById("tourBtn").click(); document.getElementById("tourNext").click(); document.getElementById("tourSkip").click()`, nil)
		b.eval(`!!document.getElementById("tour")`, &open)
		if open {
			t.Fatal("Skip did not close a started tour")
		}
	})

	t.Run("ST-GEN-UC08-R10-T01 Printing gives each view on its own sheet with its title and key, and each flow as a numbered list", func(t *testing.T) {
		b.open(access + "#view=components")
		b.media("print", "light", "no-preference")
		defer b.media("screen", "light", "no-preference")
		var got struct {
			App      string
			Sections []struct{ Title, Break, Key string }
			Flows    []int
		}
		b.eval(`(function(){ var out = {App: getComputedStyle(document.querySelector(".app")).display, Sections: [], Flows: []};
			document.querySelectorAll("#forms > section").forEach(function(s){ out.Sections.push({Title: s.querySelector("h2").textContent,
				Break: getComputedStyle(s).breakAfter, Key: s.querySelector(".key, .empty") && s.querySelector(".key, .empty").offsetHeight > 0 ? "shown" : "none"}); });
			document.querySelectorAll("#form-flows ol.flowlist").forEach(function(o){ out.Flows.push(o.offsetHeight > 0 ? o.children.length : -1); });
			return out; })()`, &got)
		if got.App != "none" || len(got.Sections) != 6 {
			t.Fatalf("print shows the app (%s) or %d views", got.App, len(got.Sections))
		}
		for _, s := range got.Sections {
			if !strings.HasSuffix(s.Title, am.System.Name) || s.Break != "page" || s.Key != "shown" {
				t.Errorf("the printed view %+v has no title, sheet or key", s)
			}
		}
		if len(got.Flows) != len(am.Flows) {
			t.Fatalf("print shows %d flows for %d", len(got.Flows), len(am.Flows))
		}
		for i, f := range am.Flows {
			if got.Flows[i] != len(f.Steps) {
				t.Errorf("the printed flow %s lists %d steps for %d", f.ID, got.Flows[i], len(f.Steps))
			}
		}
	})

	t.Run("ST-GEN-UC08-R14-T02 Selecting a cell of the access matrix shows its full grant paths", func(t *testing.T) {
		b.open(access + "#view=trust")
		var got struct {
			Paths []string
			Text  string
		}
		b.eval(`(function(){ document.querySelector('#view button.cell[data-check="check:operation:org-users.read"][data-role="role:organization#viewer@user"]').click();
			return {Paths: Array.prototype.map.call(document.querySelectorAll("#details li[data-grant]"), function(l){ return l.getAttribute("data-grant"); }), Text: document.getElementById("details").textContent}; })()`, &got)
		var want []string
		for _, p := range am.Access.Permissions {
			if p.ID == "permission:member#view" {
				for _, g := range p.Paths {
					if g.Role == "role:organization#viewer@user" {
						want = append(want, g.ID)
						for _, hop := range g.Hops {
							if !strings.Contains(got.Text, hop) {
								t.Errorf("the grant path %s does not show its hop %s", g.ID, hop)
							}
						}
						if g.Caveat != "" && !strings.Contains(got.Text, g.Caveat) {
							t.Errorf("the grant path %s does not show its caveat", g.ID)
						}
					}
				}
			}
		}
		if len(want) == 0 || !slices.Equal(got.Paths, want) {
			t.Fatalf("the cell shows the grant paths %v, not %v", got.Paths, want)
		}
	})
}

// bo09StudyQuestions are the questions of the ST-BO09 study that the platform team keeps. The set is not recorded
// yet (ST-GEN-Q26); these are the examples that the design names.
var bo09StudyQuestions = []string{
	"Which fills can stop organizations.create?",
	"Now open organizations.delete yourself. Which fill can stop it?",
}

func itoaTest(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// contrastScript gives each visible text of the view, the details and the header whose contrast with what is
// behind it is under 4.5:1, or 3:1 for large text.
const contrastScript = `
function rgb(s){ var m = s.match(/rgba?\(([^)]+)\)/); if (m){ var p = m[1].split(/[ ,\/]+/).filter(Boolean).map(Number); return [p[0]/255, p[1]/255, p[2]/255, p.length > 3 ? p[3] : 1]; }
  m = s.match(/color\(srgb ([^)]+)\)/); if (m){ var q = m[1].split(/[ \/]+/).filter(Boolean).map(Number); return [q[0], q[1], q[2], q.length > 3 ? q[3] : 1]; }
  return null; }
function lum(c){ return [0,1,2].map(function(i){ var v = c[i]; return v <= 0.04045 ? v/12.92 : Math.pow((v+0.055)/1.055, 2.4); }).reduce(function(a, v, i){ return a + v*[0.2126,0.7152,0.0722][i]; }, 0); }
function ratio(a, b){ var x = lum(a), y = lum(b); return (Math.max(x, y)+0.05)/(Math.min(x, y)+0.05); }
function over(top, under){ var a = top[3]; return [top[0]*a+under[0]*(1-a), top[1]*a+under[1]*(1-a), top[2]*a+under[2]*(1-a), 1]; }
function behind(el){ var layers = [];
  for (var e = el; e; e = e.parentElement){ var c = rgb(getComputedStyle(e).backgroundColor); if (c && c[3] > 0){ layers.push(c); if (c[3] >= 1) break; } }
  var base = rgb(getComputedStyle(document.body).backgroundColor);
  for (var i = layers.length-1; i >= 0; i--) base = layers[i][3] >= 1 ? layers[i] : over(layers[i], base);
  return base; }
function svgBehind(t){ var bb = t.getBoundingClientRect(), stack = document.elementsFromPoint(bb.left+bb.width/2, bb.top+bb.height/2), panel = behind(t.ownerSVGElement);
  for (var k = 0; k < stack.length; k++){ var x = stack[k]; if (x === t || !(x instanceof SVGGeometryElement) || x.tagName === "text" || x.tagName === "line" || x.tagName === "svg") continue;
    var f = rgb(getComputedStyle(x).fill); if (f && f[3] > 0) return over(f, panel); if (x.closest("svg") === t.ownerSVGElement && x.classList.contains("box")) continue; }
  return panel; }
var bad = [];
document.querySelectorAll("header *, #view *, #details *, nav *").forEach(function(el){
  var own = Array.prototype.some.call(el.childNodes, function(n){ return n.nodeType === 3 && n.textContent.trim(); });
  if (!own) return;
  var r = el.getBoundingClientRect(); if (!r.width || !r.height) return;
  if (el.closest(".sr") || el.closest(".dim")) return;
  var cs = getComputedStyle(el), fg, bg;
  if (el instanceof SVGElement){ fg = rgb(cs.fill); bg = svgBehind(el); } else { fg = rgb(cs.color); bg = behind(el); }
  if (!fg || !bg) { bad.push("unread colour: "+el.tagName); return; }
  var op = 1; for (var e = el; e; e = e.parentElement || e.parentNode){ if (e.nodeType !== 1) break; op *= Number(getComputedStyle(e).opacity); }
  fg = over([fg[0], fg[1], fg[2], fg[3]*op], bg);
  var size = parseFloat(cs.fontSize), large = size >= 24 || (size >= 18.66 && Number(cs.fontWeight) >= 700);
  var need = large ? 3 : 4.5, got = ratio(fg, bg);
  if (got < need) bad.push(el.tagName+" '"+el.textContent.trim().slice(0, 30)+"' "+got.toFixed(2));
});
return bad.slice(0, 12);`

// A small driver of a headless browser over the DevTools protocol, from Go (ST-STD-LOCAL; the owner's decision
// Q-G (1) keeps this driver and not Playwright). It starts the browser that STEGO_BROWSER names, with no network
// for the page: every request that is not a file: or data: address fails and is counted.

// cdpBrowser is one headless browser with one page attached.
type cdpBrowser struct {
	t       *testing.T
	cmd     *exec.Cmd
	conn    *websocket.Conn
	ctx     context.Context
	cancel  context.CancelFunc
	session string
	mu      sync.Mutex
	next    int
	waiting map[int]chan cdpReply
	blocked []string
	errors  []string
}

type cdpReply struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type cdpMessage struct {
	ID        int             `json:"id"`
	Method    string          `json:"method"`
	SessionID string          `json:"sessionId"`
	Params    json.RawMessage `json:"params"`
	Result    json.RawMessage `json:"result"`
	Error     *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// startBrowser starts the browser, or skips the test when no browser is named. A run in CI must name one.
func startBrowser(t *testing.T) *cdpBrowser {
	t.Helper()
	path := os.Getenv("STEGO_BROWSER")
	if path == "" {
		if os.Getenv("STEGO_REQUIRE_BROWSER_VIEW") == "1" {
			t.Fatal("STEGO_REQUIRE_BROWSER_VIEW is set, and STEGO_BROWSER names no browser")
		}
		t.Skip("set STEGO_BROWSER to a Chrome or Chromium executable to run the browser tests of the architecture view")
	}
	dir := t.TempDir()
	args := []string{"--headless=new", "--remote-debugging-port=0", "--user-data-dir=" + dir, "--no-first-run",
		"--no-default-browser-check", "--disable-background-networking", "--disable-gpu", "--disable-dev-shm-usage",
		"--disable-extensions", "--disable-sync", "--mute-audio", "--window-size=1280,900", "about:blank"}
	if os.Getenv("STEGO_BROWSER_NO_SANDBOX") == "1" {
		args = append([]string{"--no-sandbox"}, args...)
	}
	cmd := exec.Command(path, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	log, err := os.Create(filepath.Join(dir, "browser.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatalf("cannot start the browser: %v", err)
	}
	b := &cdpBrowser{t: t, cmd: cmd, waiting: map[int]chan cdpReply{}}
	t.Cleanup(b.stop)
	var ws string
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		data, err := os.ReadFile(filepath.Join(dir, "DevToolsActivePort"))
		if lines := strings.Split(strings.TrimSpace(string(data)), "\n"); err == nil && len(lines) == 2 {
			ws = "ws://127.0.0.1:" + lines[0] + lines[1]
			break
		}
	}
	if ws == "" {
		t.Fatal("the browser wrote no DevTools port")
	}
	b.ctx, b.cancel = context.WithTimeout(context.Background(), 3*time.Minute)
	b.conn, _, err = websocket.Dial(b.ctx, ws, nil)
	if err != nil {
		t.Fatalf("cannot reach the DevTools socket: %v", err)
	}
	b.conn.SetReadLimit(64 << 20)
	go b.read()
	var target struct {
		TargetID string `json:"targetId"`
	}
	b.call("", "Target.createTarget", map[string]any{"url": "about:blank"}, &target)
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	b.call("", "Target.attachToTarget", map[string]any{"targetId": target.TargetID, "flatten": true}, &attached)
	b.session = attached.SessionID
	b.do("Runtime.enable", nil)
	b.do("Page.enable", nil)
	b.do("Log.enable", nil)
	b.do("Fetch.enable", map[string]any{"patterns": []map[string]any{{"urlPattern": "*"}}})
	return b
}

func (b *cdpBrowser) stop() {
	if b.cancel != nil {
		b.cancel()
	}
	if b.conn != nil {
		_ = b.conn.CloseNow()
	}
	_ = syscall.Kill(-b.cmd.Process.Pid, syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- b.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-b.cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
}

// read takes each message of the socket: a reply goes to its caller, and an event is handled here.
func (b *cdpBrowser) read() {
	for {
		_, data, err := b.conn.Read(b.ctx)
		if err != nil {
			return
		}
		var msg cdpMessage
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		if msg.ID != 0 {
			b.mu.Lock()
			ch := b.waiting[msg.ID]
			delete(b.waiting, msg.ID)
			b.mu.Unlock()
			if ch != nil {
				ch <- cdpReply{Result: msg.Result, Error: msg.Error}
			}
			continue
		}
		switch msg.Method {
		case "Fetch.requestPaused":
			var p struct {
				RequestID string `json:"requestId"`
				Request   struct {
					URL string `json:"url"`
				} `json:"request"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			go func(session string) {
				if strings.HasPrefix(p.Request.URL, "file:") || strings.HasPrefix(p.Request.URL, "data:") {
					b.send(session, "Fetch.continueRequest", map[string]any{"requestId": p.RequestID})
					return
				}
				b.mu.Lock()
				b.blocked = append(b.blocked, p.Request.URL)
				b.mu.Unlock()
				b.send(session, "Fetch.failRequest", map[string]any{"requestId": p.RequestID, "errorReason": "BlockedByClient"})
			}(msg.SessionID)
		case "Runtime.exceptionThrown":
			b.mu.Lock()
			b.errors = append(b.errors, string(msg.Params))
			b.mu.Unlock()
		case "Runtime.consoleAPICalled":
			var p struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			if p.Type == "error" || p.Type == "assert" {
				b.mu.Lock()
				b.errors = append(b.errors, string(msg.Params))
				b.mu.Unlock()
			}
		case "Log.entryAdded":
			var p struct {
				Entry struct {
					Level string `json:"level"`
					Text  string `json:"text"`
				} `json:"entry"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			if p.Entry.Level == "error" {
				b.mu.Lock()
				b.errors = append(b.errors, p.Entry.Text)
				b.mu.Unlock()
			}
		}
	}
}

// send sends a command and returns its reply channel.
func (b *cdpBrowser) send(session, method string, params any) chan cdpReply {
	b.mu.Lock()
	b.next++
	id := b.next
	ch := make(chan cdpReply, 1)
	b.waiting[id] = ch
	b.mu.Unlock()
	msg := map[string]any{"id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if session != "" {
		msg["sessionId"] = session
	}
	data, _ := json.Marshal(msg)
	if err := b.conn.Write(b.ctx, websocket.MessageText, data); err != nil {
		ch <- cdpReply{Error: &struct {
			Message string `json:"message"`
		}{err.Error()}}
	}
	return ch
}

// call sends a command and waits for its result.
func (b *cdpBrowser) call(session, method string, params, out any) {
	b.t.Helper()
	select {
	case r := <-b.send(session, method, params):
		if r.Error != nil {
			b.t.Fatalf("%s: %s", method, r.Error.Message)
		}
		if out != nil {
			if err := json.Unmarshal(r.Result, out); err != nil {
				b.t.Fatalf("%s: %v", method, err)
			}
		}
	case <-time.After(30 * time.Second):
		b.t.Fatalf("%s: no reply", method)
	}
}

// do sends a command to the page.
func (b *cdpBrowser) do(method string, params any) {
	b.t.Helper()
	b.call(b.session, method, params, nil)
}

// eval runs a script in the page and decodes its value into out; a thrown error fails the test.
func (b *cdpBrowser) eval(script string, out any) {
	b.t.Helper()
	var r struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	b.call(b.session, "Runtime.evaluate", map[string]any{"expression": script, "returnByValue": true, "awaitPromise": true}, &r)
	if r.ExceptionDetails != nil {
		b.t.Fatalf("script failed: %s %s\n%s", r.ExceptionDetails.Text, r.ExceptionDetails.Exception.Description, script)
	}
	if out != nil && len(r.Result.Value) > 0 {
		if err := json.Unmarshal(r.Result.Value, out); err != nil {
			b.t.Fatalf("script value: %v: %s", err, r.Result.Value)
		}
	}
}

// open loads the page by its file address and waits until the viewer has drawn it.
func (b *cdpBrowser) open(url string) {
	b.t.Helper()
	var ready bool
	b.do("Page.navigate", map[string]any{"url": "about:blank"})
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && !ready; time.Sleep(20 * time.Millisecond) {
		b.eval(`location.href === "about:blank" && document.readyState === "complete"`, &ready)
	}
	ready = false
	b.do("Page.navigate", map[string]any{"url": url})
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		b.eval(`document.readyState === "complete" && !!document.querySelector("#nav button")`, &ready)
		if ready {
			return
		}
	}
	b.t.Fatal("the view did not render")
}

// key presses one key, with its text when it types one.
func (b *cdpBrowser) key(name string) {
	b.t.Helper()
	codes := map[string]int{"Tab": 9, "Enter": 13, "Escape": 27, " ": 32, "ArrowLeft": 37, "ArrowUp": 38, "ArrowRight": 39, "ArrowDown": 40}
	code := name
	if name == " " {
		code = "Space"
	}
	down := map[string]any{"type": "rawKeyDown", "key": name, "code": code, "windowsVirtualKeyCode": codes[name], "nativeVirtualKeyCode": codes[name]}
	if name == "Enter" {
		down["type"], down["text"] = "keyDown", "\r"
	}
	if name == " " {
		down["type"], down["text"] = "keyDown", " "
	}
	b.do("Input.dispatchKeyEvent", down)
	b.do("Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": name, "code": code, "windowsVirtualKeyCode": codes[name], "nativeVirtualKeyCode": codes[name]})
}

// viewport sets the width of the page in CSS pixels.
func (b *cdpBrowser) viewport(width, height int) {
	b.t.Helper()
	b.do("Emulation.setDeviceMetricsOverride", map[string]any{"width": width, "height": height, "deviceScaleFactor": 1, "mobile": false})
}

// media sets the media type and the preferred colour scheme and motion of the page.
func (b *cdpBrowser) media(kind, scheme, motion string) {
	b.t.Helper()
	b.do("Emulation.setEmulatedMedia", map[string]any{"media": kind, "features": []map[string]string{
		{"name": "prefers-color-scheme", "value": scheme}, {"name": "prefers-reduced-motion", "value": motion}}})
}

// problems gives the blocked requests and the errors of the page since the last call.
func (b *cdpBrowser) problems() (blocked, errors []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	blocked, errors = b.blocked, b.errors
	b.blocked, b.errors = nil, nil
	return
}
