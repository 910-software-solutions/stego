package compiler

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
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
		var store string
		b.eval(`(document.querySelector('.steps [data-note="store"]')||{}).textContent || ""`, &store)
		if !strings.Contains(store, "No component of the active set is in the store layer") {
			t.Errorf("the store step does not say that the model names the repository: %q", store)
		}
		var exits int
		b.eval(`document.querySelectorAll(".steps .exit:not([data-note])").length`, &exits)
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

	t.Run("ST-GEN-UC08-R07-T04 The view shows markup in the free text of the model as text only, and runs none of it", func(t *testing.T) {
		hostile, value := hostileModel(t)
		out, err := RenderArchitectureView(hostile)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "index.html")
		if err := os.WriteFile(path, out, 0o644); err != nil {
			t.Fatal(err)
		}
		b.do("Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": "window.__ran = 0; window.alert = function(){ window.__ran++; };"})
		b.problems()
		for _, v := range []string{"platform", "flows", "trust", "data", "overview", "components"} {
			b.open("file://" + path + "#view=" + v)
			var got struct {
				Images, Ran int
				Text, Title string
			}
			b.eval(`(function(){ return {Images: document.querySelectorAll("img").length, Ran: window.__ran, Text: document.getElementById("view").textContent + " " +
				Array.prototype.map.call(document.querySelectorAll("#view [aria-label]"), function(e){ return e.getAttribute("aria-label"); }).join(" "),
				Title: document.querySelector("header h1").textContent}; })()`, &got)
			if got.Images != 0 || got.Ran != 0 {
				t.Fatalf("view %s: the free text of the model became markup: %d images, %d calls", v, got.Images, got.Ran)
			}
			if (v == "platform" || v == "flows" || v == "overview" || v == "components") && !strings.Contains(got.Text, value) {
				t.Errorf("view %s does not show the free text as text", v)
			}
			if !strings.Contains(got.Title, value) {
				t.Errorf("view %s does not show the system name as text: %q", v, got.Title)
			}
		}
		// The details of a cell of a nested operation show the names of its ancestors as text (ST-SUP-RC20-OC24).
		b.open("file://" + path + "#view=trust")
		var cell struct {
			Found       bool
			Text, Cut   string
			Images, Ran int
		}
		b.eval(`(function(){ var M = JSON.parse(document.getElementById("model").textContent), nested = {};
			M.access.checks.forEach(function(c){ if (c.ancestors && c.ancestors.length) nested[c.id] = 1; });
			var btn = Array.prototype.filter.call(document.querySelectorAll("#view button.cell"), function(x){ return nested[x.getAttribute("data-check")]; })[0];
			if (!btn) return {Found: false};
			btn.click();
			var list = document.getElementById("cellAncestors"), cut = document.getElementById("cellAncestorIncomplete");
			return {Found: true, Text: list ? list.textContent : "", Cut: cut ? cut.textContent : "", Images: document.querySelectorAll("img").length, Ran: window.__ran}; })()`, &cell)
		// The list shows the parameter, the reason and the scope source as text: three times the hostile text. The
		// note of the ancestor read that a limit cut short shows the hostile ancestor name as text too.
		if !cell.Found || strings.Count(cell.Text, value) < 3 || !strings.Contains(cell.Cut, value) || cell.Images != 0 || cell.Ran != 0 {
			t.Errorf("the details of a nested cell do not show the hostile ancestor names as text only: %+v", cell)
		}
		// The details of a cell that may hold an incomplete permission show its hostile resource type as text
		// (security review of PR #383, L1).
		var may struct {
			Found       bool
			Text        string
			Images, Ran int
		}
		b.eval(`(function(){ var btn = document.querySelector("#view button.cell.may-hold");
			if (!btn) return {Found: false};
			btn.click();
			return {Found: true, Text: (document.getElementById("cellMayHold")||{}).textContent || "", Images: document.querySelectorAll("img").length, Ran: window.__ran}; })()`, &may)
		if !may.Found || !strings.Contains(may.Text, value) || may.Images != 0 || may.Ran != 0 {
			t.Errorf("the details of a cell that may hold do not show the hostile resource type as text only: %+v", may)
		}
		// The index of the elements by ID has no prototype: an ID of the address that names a property of Object
		// selects nothing, and a model element with the ID __proto__ is an element like any other.
		for _, sel := range []string{"constructor", "toString", "__proto__"} {
			b.open("file://" + path + "#view=components&sel=" + sel)
			var shown struct {
				Title string
				Sel   string
			}
			b.eval(`(function(){ return {Title: (document.getElementById("detailsTitle")||{}).textContent || "", Sel: location.hash}; })()`, &shown)
			if sel != "__proto__" && shown.Title != "" {
				t.Errorf("the address sel=%s selects %q, a property of Object", sel, shown.Title)
			}
			if sel == "__proto__" && shown.Title == "" {
				t.Errorf("the fill with the ID __proto__ is not an element of the index")
			}
		}
		var model bool
		b.eval(`JSON.parse(document.getElementById("model").textContent).aspects.platform_edges.reason === `+strconv.Quote(value), &model)
		if !model {
			t.Error("the page does not read the model's free text back unchanged")
		}
		if blocked, errors := b.problems(); len(blocked)+len(errors) > 0 {
			t.Fatalf("the hostile model made requests %v or errors %v", blocked, errors)
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
		// The overview draws a line for each relation of the service, and its text form lists each.
		b.open(access + "#view=overview")
		var rel struct{ Drawn, Text []string }
		b.eval(`(function(){ var d = {}, x = {};
			document.querySelectorAll("#view svg path[data-rel]").forEach(function(p){ if (p.getAttribute("data-rel")) d[p.getAttribute("data-rel")] = 1; });
			document.querySelectorAll('#form-overview tr[data-id^="relation:"]').forEach(function(r){ x[r.getAttribute("data-id")] = 1; });
			return {Drawn: Object.keys(d).sort(), Text: Object.keys(x).sort()}; })()`, &rel)
		var relations []string
		for _, r := range am.Relations {
			relations = append(relations, r.ID)
		}
		sort.Strings(relations)
		if len(relations) == 0 || !slices.Equal(rel.Drawn, relations) || !slices.Equal(rel.Text, relations) {
			t.Errorf("the overview draws the relations %v and lists %v, and the model holds %v", rel.Drawn, rel.Text, relations)
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

	t.Run("ST-GEN-UC08-R08-T07 At 400 px the details sheet of a selected matrix cell shows above the sticky heads of the access matrix", func(t *testing.T) {
		b.viewport(400, 800)
		defer b.viewport(1280, 900)
		b.open(access + "#view=trust")
		var hidden []string
		b.eval(`(function(){ var btn = document.querySelector('#view button.cell[data-check="check:operation:org-users.read"][data-role="role:member#owner@user"]');
			btn.scrollIntoView({block:"end"}); btn.click();
			var d = document.getElementById("details").getBoundingClientRect(), out = [];
			// Each point of the sheet that is on the screen shows the sheet, not a head of the matrix behind it.
			for (var y = Math.max(d.top, 0) + 8; y < Math.min(d.bottom, innerHeight) - 4; y += 24) for (var x = d.left + 8; x < d.right - 4; x += 40){
				var e = document.elementFromPoint(x, y); if (e && !e.closest("aside")) out.push(Math.round(x)+","+Math.round(y)+" "+e.tagName); }
			return out; })()`, &hidden)
		if len(hidden) > 0 {
			t.Fatalf("the access matrix shows over the details sheet at %v", hidden)
		}
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
		var notice string
		b.eval(`(function(){ document.querySelector('#view button.cell[data-check="check:operation:organizations.read"][data-role="role:organization#viewer@user"]').click();
			return (document.getElementById("cellIncomplete")||{}).textContent || ""; })()`, &notice)
		if !strings.Contains(notice, "Incomplete: a limit was reached") {
			t.Fatalf("the cell of an incomplete permission does not say so: %q", notice)
		}
		// A cell of a nested operation lists the ancestors in the path, and says whether the role grants the read of
		// the decided parent of a list (ST-SUP-RC20-OC24, ST-SUP-Q156).
		var chain []string
		b.eval(`(function(){ document.querySelector('#view button.cell[data-check="check:operation:org-users.list"][data-role="role:member#owner@user"]').click();
			return Array.prototype.map.call(document.querySelectorAll("#cellAncestors li"), function(l){ return l.getAttribute("data-ancestor")+" "+l.textContent; }); })()`, &chain)
		// A limit cut the grant paths of organization#view short, so the role may hold it by a path past the limit.
		if len(chain) != 1 || !strings.HasPrefix(chain[0], "ancestor:org-users.list/Organization Organization (org_id): organization#view") ||
			!strings.Contains(chain[0], "no path that the model names gives it to this role alone") {
			t.Fatalf("the cell of a nested list lists the ancestors %q", chain)
		}
		b.eval(`(function(){ document.querySelector('#view button.cell[data-check="check:operation:org-users.list"][data-role="role:organization#viewer@user"]').click();
			return Array.prototype.map.call(document.querySelectorAll("#cellAncestors li"), function(l){ return l.textContent; }); })()`, &chain)
		if len(chain) != 1 || !strings.Contains(chain[0], "this role grants it") {
			t.Fatalf("the cell of a role that may view the organization lists the ancestors %q", chain)
		}
		// A read is decided on the row: the organization is checked for existence and its link only, with no
		// permission and no word on what the role grants.
		b.eval(`(function(){ document.querySelector('#view button.cell[data-check="check:operation:org-users.read"][data-role="role:member#owner@user"]').click();
			return Array.prototype.map.call(document.querySelectorAll("#cellAncestors li"), function(l){ return l.textContent; }); })()`, &chain)
		if len(chain) != 1 || !strings.HasPrefix(chain[0], "Organization (org_id): exists and linked") || strings.Contains(chain[0], "#") || strings.Contains(chain[0], "grant") || strings.Contains(chain[0], "incomplete") {
			t.Fatalf("the cell of a nested read lists the ancestors %q", chain)
		}
	})

	t.Run("ST-GEN-UC08-R14-T06 The details of a cell mark an ancestor read whose grant paths a limit cut short as incomplete", func(t *testing.T) {
		// The same matrix with the grant paths of organization#view complete: no mark, and the role that holds no path
		// of the read does not get it.
		data, err := os.ReadFile("testdata/architecture/access.json")
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]any
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatal(err)
		}
		for _, perm := range raw["access"].(map[string]any)["permissions"].([]any) {
			if perm.(map[string]any)["id"] == "permission:organization#view" {
				perm.(map[string]any)["complete"] = true
			}
		}
		data, err = json.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		view, err := RenderArchitectureView(data)
		if err != nil {
			t.Fatal(err)
		}
		complete := filepath.Join(t.TempDir(), "index.html")
		if err := os.WriteFile(complete, view, 0o644); err != nil {
			t.Fatal(err)
		}
		type details struct {
			Items []string
			Note  string
		}
		read := func(address, check, role string) details {
			b.open(address + "#view=trust")
			var got details
			b.eval(`(function(){ document.querySelector('#view button.cell[data-check="`+check+`"][data-role="`+role+`"]').click();
				return {Items: Array.prototype.map.call(document.querySelectorAll("#cellAncestors li"), function(l){ return l.textContent; }),
					Note: (document.getElementById("cellAncestorIncomplete")||{}).textContent || ""}; })()`, &got)
			return got
		}
		owner, viewer, top := "role:member#owner@user", "role:organization#viewer@user", "check:operation:organizations.read"
		got := read(access, "check:operation:org-users.list", owner)
		if len(got.Items) != 1 || !strings.Contains(got.Items[0], "organization#view ⚠ incomplete: a limit was reached") ||
			!strings.Contains(got.Items[0], "a limit cut its paths short, so this role may hold it") || strings.Contains(got.Items[0], "this role alone does not grant it") {
			t.Errorf("the cell of a role with no path of the incomplete ancestor read lists %q", got.Items)
		}
		if !strings.Contains(got.Note, "Incomplete: a limit was reached in the grant paths of the ancestor read Organization (organization#view)") {
			t.Errorf("the cell of a nested operation does not note the incomplete ancestor read: %q", got.Note)
		}
		got = read(access, "check:operation:org-users.list", viewer)
		if len(got.Items) != 1 || !strings.Contains(got.Items[0], "⚠ incomplete: a limit was reached") || !strings.Contains(got.Items[0], "this role grants it") || got.Note == "" {
			t.Errorf("the cell of a role that holds the incomplete ancestor read shows %+v", got)
		}
		// A read decides no ancestor, so its cell marks no ancestor read as incomplete (ST-SUP-Q156).
		if got = read(access, "check:operation:org-users.read", owner); len(got.Items) != 1 || strings.Contains(got.Items[0], "incomplete") || got.Note != "" {
			t.Errorf("the cell of a nested read shows %+v", got)
		}
		if got = read(access, top, viewer); len(got.Items) != 0 || got.Note != "" {
			t.Errorf("the cell of an operation with no ancestor shows %+v", got)
		}
		got = read("file://"+complete, "check:operation:org-users.list", owner)
		if len(got.Items) != 1 || strings.Contains(got.Items[0], "incomplete") || !strings.Contains(got.Items[0], "this role alone does not grant it, so the caller also needs it") || got.Note != "" {
			t.Errorf("the cell of a complete ancestor read shows %+v", got)
		}
	})

	t.Run("ST-GEN-UC08-R14-T08 The details of a cell that may hold its permission say may hold, and a permission with no key complete reads as cut short", func(t *testing.T) {
		type details struct {
			Items   []string
			Note    string
			Own     string
			MayHold string
			Paths   int
		}
		read := func(address, check, role string) details {
			b.open(address + "#view=trust")
			var got details
			b.eval(`(function(){ document.querySelector('#view button.cell[data-check="`+check+`"][data-role="`+role+`"]').click();
				function text(id){ return (document.getElementById(id)||{}).textContent || ""; }
				return {Items: Array.prototype.map.call(document.querySelectorAll("#cellAncestors li"), function(l){ return l.textContent; }),
					Note: text("cellAncestorIncomplete"), Own: text("cellIncomplete"), MayHold: text("cellMayHold"),
					Paths: document.querySelectorAll("#details li[data-grant]").length}; })()`, &got)
			return got
		}
		owner, viewer, top := "role:member#owner@user", "role:organization#viewer@user", "check:operation:organizations.read"
		// A role with no named path of organization#view, whose paths a limit cut short, may hold it.
		got := read(access, top, owner)
		if !strings.Contains(got.Own, "Incomplete: a limit was reached, so organization#view may grant more roles") ||
			!strings.Contains(got.MayHold, "May hold: no path that the model names gives member#owner@user the permission organization#view") ||
			!strings.Contains(got.MayHold, "so this role may hold it") || got.Paths != 0 {
			t.Errorf("the cell of a role that may hold an incomplete permission shows %+v", got)
		}
		// A role with a named path holds it: the note of the cut paths and no may hold.
		if got = read(access, top, viewer); got.Own == "" || got.MayHold != "" || got.Paths == 0 {
			t.Errorf("the cell of a role that holds an incomplete permission shows %+v", got)
		}
		// The same model with no key complete in organization#view and member#view (security review of PR #383, L3):
		// the script reads each as cut short, as the Go view does.
		data, err := os.ReadFile("testdata/architecture/access.json")
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]any
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatal(err)
		}
		for _, perm := range raw["access"].(map[string]any)["permissions"].([]any) {
			if id := perm.(map[string]any)["id"]; id == "permission:organization#view" || id == "permission:member#view" {
				delete(perm.(map[string]any), "complete")
			}
		}
		if data, err = json.Marshal(raw); err != nil {
			t.Fatal(err)
		}
		view, err := RenderArchitectureView(data)
		if err != nil {
			t.Fatal(err)
		}
		missing := filepath.Join(t.TempDir(), "index.html")
		if err := os.WriteFile(missing, view, 0o644); err != nil {
			t.Fatal(err)
		}
		got = read("file://"+missing, "check:operation:org-users.list", "role:organization#auditor@user")
		if !strings.Contains(got.Own, "member#view may grant more roles") || !strings.Contains(got.MayHold, "the permission member#view") ||
			len(got.Items) != 1 || !strings.Contains(got.Items[0], "organization#view ⚠ incomplete: a limit was reached") || got.Note == "" {
			t.Errorf("the cell of permissions with no key complete shows %+v", got)
		}
	})

	t.Run("ST-GEN-UC08-R14-T10 The trust view of a model whose every grant path a limit stopped before a role says that a role may hold a permission, and its cells show the paths that stop", func(t *testing.T) {
		// The model of a chain of rewrites one past the depth limit (ST-GEN-UC08-R14-T09).
		_, data := deriveFixture(t, chainFixture(t, maxGrantDepth+1))
		view, err := RenderArchitectureView(data)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "index.html")
		if err := os.WriteFile(path, view, 0o644); err != nil {
			t.Fatal(err)
		}
		type shown struct {
			Note, Heads, Title, Own, MayHold string
			Definite                         bool
			Paths, Scroll, Width             int
		}
		b.problems()
		for _, width := range []int{1280, 400} {
			b.viewport(width, 900)
			b.open("file://" + path + "#view=trust")
			var got shown
			b.eval(`(function(){ var n = document.getElementById("no-role-named");
				var heads = Array.prototype.map.call(document.querySelectorAll("#view table.access thead th.role"), function(th){ return th.textContent; }).join("|");
				document.querySelector('#view button.cell[data-check="check:operation:docs.read"][data-role=""]').click();
				function text(id){ return (document.getElementById(id)||{}).textContent || ""; }
				return {Note: n && n.offsetHeight > 0 ? n.textContent : "", Heads: heads, Title: (document.querySelector("#details h3")||{}).textContent || "",
					Own: text("cellIncomplete"), MayHold: text("cellMayHold"),
					Definite: document.getElementById("view").textContent.indexOf("grants no role a permission") >= 0,
					Paths: Array.prototype.filter.call(document.querySelectorAll("#details li[data-grant]"), function(l){ return l.textContent.indexOf("depth limit") >= 0; }).length,
					Scroll: document.documentElement.scrollWidth, Width: window.innerWidth}; })()`, &got)
			if !strings.Contains(got.Note, "no grant path reached a role before a limit stopped it: the depth limit of 12 nodes. So the model names no role.") || got.Definite {
				t.Errorf("at %d px the trust view does not say that a role may hold a permission: %+v", width, got)
			}
			if got.Heads != "Paths that stop, with no role" || got.Title != "Paths that stop · docs.read" ||
				!strings.Contains(got.Own, "doc#read may grant more roles") || got.MayHold != "" || got.Paths == 0 {
				t.Errorf("at %d px the cell of the paths that stop shows %+v", width, got)
			}
			if got.Scroll > got.Width {
				t.Errorf("at %d px the page is %d px wide", width, got.Scroll)
			}
			if blocked, errors := b.problems(); len(blocked) > 0 || len(errors) > 0 {
				t.Errorf("at %d px: blocked %v, errors %v", width, blocked, errors)
			}
		}
		b.viewport(1280, 900)
	})

	t.Run("ST-GEN-UC08-R14-T12 The trust view of a model whose grant paths the work limit stopped before a role names that limit, and a cell of the paths that stop with no path of an incomplete permission says may hold", func(t *testing.T) {
		// The model of ST-GEN-UC08-R14-T11, as a reader can edit it, with no path of doc#write.
		_, data := deriveFixture(t, workFixture(t))
		view, err := RenderArchitectureView(withoutPaths(t, data, "permission:doc#write"))
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "index.html")
		if err := os.WriteFile(path, view, 0o644); err != nil {
			t.Fatal(err)
		}
		type cell struct {
			Text, Title, Label, Class, Head, Own, MayHold string
			Paths                                         int
		}
		type shown struct {
			Note          string
			Read, Write   cell
			Scroll, Width int
		}
		b.problems()
		for _, width := range []int{1280, 400} {
			b.viewport(width, 900)
			b.open("file://" + path + "#view=trust")
			var got shown
			b.eval(`(function(){ var n = document.getElementById("no-role-named");
				function text(id){ return (document.getElementById(id)||{}).textContent || ""; }
				function pick(check){ var b = document.querySelector('#view button.cell[data-check="'+check+'"][data-role=""]');
					if (!b) return {};
					var c = {Text: b.textContent, Title: b.getAttribute("title"), Label: b.getAttribute("aria-label"), Class: b.className};
					b.click();
					c.Head = (document.querySelector("#details h3")||{}).textContent || "";
					c.Own = text("cellIncomplete"); c.MayHold = text("cellMayHold");
					c.Paths = Array.prototype.filter.call(document.querySelectorAll("#details li[data-grant]"), function(l){ return l.textContent.indexOf("work limit") >= 0; }).length;
					return c; }
				return {Note: n && n.offsetHeight > 0 ? n.textContent : "", Read: pick("check:operation:docs.read"), Write: pick("check:operation:docs.update"),
					Scroll: document.documentElement.scrollWidth, Width: window.innerWidth}; })()`, &got)
			if !strings.Contains(got.Note, "no grant path reached a role before a limit stopped it: the work limit of 20000 steps and "+unnamedLimit+".") {
				t.Errorf("at %d px the note does not name the limits: %q", width, got.Note)
			}
			if !strings.Contains(got.Read.Text, "⊘") || !strings.Contains(got.Read.Title, "work limit") || got.Read.Head != "Paths that stop · docs.read" ||
				!strings.Contains(got.Read.Own, "doc#read may grant more roles") || got.Read.MayHold != "" || got.Read.Paths != 1 {
				t.Errorf("at %d px the cell of the paths that stop of docs.read shows %+v", width, got.Read)
			}
			if got.Write.Text != mayHoldGlyph || got.Write.Title != mayHoldStopText || got.Write.Label != "docs.update: "+mayHoldStopText ||
				!strings.Contains(got.Write.Class, "may-hold") || got.Write.Head != "Paths that stop · docs.update" ||
				!strings.Contains(got.Write.Own, "doc#write may grant more roles") || got.Write.Paths != 0 ||
				!strings.Contains(got.Write.MayHold, "names no path of the permission doc#write that stops") || !strings.Contains(got.Write.MayHold, "a role may hold it") {
				t.Errorf("at %d px the cell of the paths that stop of docs.update shows %+v", width, got.Write)
			}
			if got.Scroll > got.Width {
				t.Errorf("at %d px the page is %d px wide", width, got.Scroll)
			}
			if blocked, errors := b.problems(); len(blocked) > 0 || len(errors) > 0 {
				t.Errorf("at %d px: blocked %v, errors %v", width, blocked, errors)
			}
		}
		b.viewport(1280, 900)
	})

	t.Run("ST-GEN-UC08-R14-T15 The trust view names the path limit in the cell and the details of a path that it stops, and a cell of a check whose permission the model does not hold says may hold", func(t *testing.T) {
		// The model of ST-GEN-UC08-R13-T04, which the path limit stops before a role.
		_, data := deriveFixture(t, pathFixture(t))
		view, err := RenderArchitectureView(data)
		if err != nil {
			t.Fatal(err)
		}
		limitPath := filepath.Join(t.TempDir(), "limit.html")
		if err := os.WriteFile(limitPath, view, 0o644); err != nil {
			t.Fatal(err)
		}
		// The model of the fixture, as a reader can edit it, whose check of docs.read names a permission that the
		// model does not hold.
		_, data = deriveFixture(t, archFixture(t))
		var raw map[string]any
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatal(err)
		}
		for _, c := range raw["access"].(map[string]any)["checks"].([]any) {
			if c.(map[string]any)["subject"] == "operation:docs.read" {
				c.(map[string]any)["grants"] = "permission:doc#gone"
			}
		}
		edited, err := json.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		if view, err = RenderArchitectureView(edited); err != nil {
			t.Fatal(err)
		}
		gonePath := filepath.Join(t.TempDir(), "gone.html")
		if err := os.WriteFile(gonePath, view, 0o644); err != nil {
			t.Fatal(err)
		}
		type cell struct {
			Found                            bool
			Text, Title, Class, Own, MayHold string
			Limit, Work                      int
		}
		pick := `function pick(sel){ var b = document.querySelector(sel);
			if (!b) return {Found: false};
			var c = {Found: true, Text: b.textContent, Title: b.getAttribute("title"), Class: b.className};
			b.click();
			function text(id){ return (document.getElementById(id)||{}).textContent || ""; }
			c.Own = text("cellIncomplete"); c.MayHold = text("cellMayHold");
			var items = Array.prototype.map.call(document.querySelectorAll("#details li[data-grant]"), function(l){ return l.textContent; });
			c.Limit = items.filter(function(x){ return x.split(" ")[0].indexOf("⊞") >= 0 && x.indexOf("stops: the path limit of 512 paths") >= 0; }).length;
			c.Work = items.filter(function(x){ return x.indexOf("⊘") >= 0 || x.indexOf("work limit") >= 0; }).length;
			return c; }`
		b.problems()
		for _, width := range []int{1280, 400} {
			b.viewport(width, 900)
			b.open("file://" + limitPath + "#view=trust")
			var limit struct {
				Read          cell
				Scroll, Width int
			}
			b.eval(`(function(){ `+pick+`
				return {Read: pick('#view button.cell[data-check="check:operation:docs.read"][data-role=""]'), Scroll: document.documentElement.scrollWidth, Width: window.innerWidth}; })()`, &limit)
			if !limit.Read.Found || !strings.Contains(limit.Read.Text, "⊞") || strings.Contains(limit.Read.Text, "⊘") ||
				!strings.Contains(limit.Read.Title, "path limit") || strings.Contains(limit.Read.Title, "work limit") ||
				!strings.Contains(limit.Read.Own, "doc#read may grant more roles") || limit.Read.Limit != 1 || limit.Read.Work != 0 {
				t.Errorf("at %d px the cell of the paths that stop of docs.read shows %+v", width, limit.Read)
			}
			if limit.Scroll > limit.Width {
				t.Errorf("at %d px the page of the path limit is %d px wide", width, limit.Scroll)
			}
			b.open("file://" + gonePath + "#view=trust")
			var gone struct {
				Read          cell
				Cells, May    int
				Scroll, Width int
			}
			b.eval(`(function(){ `+pick+`
				var cells = document.querySelectorAll('#view td[data-check="check:operation:docs.read"]');
				var may = document.querySelectorAll('#view td[data-check="check:operation:docs.read"] button.cell.may-hold');
				return {Read: pick('#view button.cell.may-hold[data-check="check:operation:docs.read"]'), Cells: cells.length, May: may.length,
					Scroll: document.documentElement.scrollWidth, Width: window.innerWidth}; })()`, &gone)
			if !gone.Read.Found || gone.Read.Text != mayHoldGlyph || gone.Read.Title != mayHoldMissingText || gone.Cells == 0 || gone.May != gone.Cells ||
				!strings.Contains(gone.Read.Own, "the model holds no grant path of doc#read") ||
				!strings.Contains(gone.Read.MayHold, "the model holds no grant path of the permission doc#read") || !strings.Contains(gone.Read.MayHold, "may hold it") {
				t.Errorf("at %d px the cells of docs.read, whose permission the model does not hold, show %+v", width, gone)
			}
			if gone.Scroll > gone.Width {
				t.Errorf("at %d px the edited page is %d px wide", width, gone.Scroll)
			}
			if blocked, errors := b.problems(); len(blocked) > 0 || len(errors) > 0 {
				t.Errorf("at %d px: blocked %v, errors %v", width, blocked, errors)
			}
		}
		b.viewport(1280, 900)
	})

	t.Run("ST-GEN-UC08-R14-T18 The trust view of a service with no authorization shows no incomplete or may hold mark, and the details of an ancestor read whose permission the model does not hold say that the role may hold it", func(t *testing.T) {
		dir := t.TempDir()
		var paths []string
		for name, data := range unboundModels(t) {
			view, err := RenderArchitectureView(data)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, name+".html")
			if err := os.WriteFile(path, view, 0o644); err != nil {
				t.Fatal(err)
			}
			paths = append(paths, path)
		}
		sort.Strings(paths)
		_, data := ancestorGoneModel(t)
		view, err := RenderArchitectureView(data)
		if err != nil {
			t.Fatal(err)
		}
		gonePath := filepath.Join(dir, "gone.html")
		if err := os.WriteFile(gonePath, view, 0o644); err != nil {
			t.Fatal(err)
		}
		b.problems()
		for _, width := range []int{1280, 400} {
			b.viewport(width, 900)
			for _, path := range paths {
				b.open("file://" + path + "#view=trust")
				var got struct {
					Text          string
					May, Notes    int
					Scroll, Width int
				}
				b.eval(`(function(){ var v = document.getElementById("view");
					return {Text: v.textContent, May: v.querySelectorAll("button.may-hold").length, Notes: v.querySelectorAll("p.incomplete").length,
						Scroll: document.documentElement.scrollWidth, Width: window.innerWidth}; })()`, &got)
				if got.May != 0 || got.Notes != 0 || strings.Contains(got.Text, "⚠ incomplete") || strings.Contains(got.Text, "holds no grant path") ||
					!strings.Contains(got.Text, "grants no role a permission that an operation checks") {
					t.Errorf("at %d px the trust view of %s, with no authorization, shows %d may hold cells and %d incomplete notes", width, filepath.Base(path), got.May, got.Notes)
				}
				if got.Scroll > got.Width {
					t.Errorf("at %d px %s is %d px wide", width, filepath.Base(path), got.Scroll)
				}
			}
			b.open("file://" + gonePath + "#view=trust")
			var gone struct {
				Found              bool
				List, Unknown, Cut string
				Scroll, Width      int
			}
			b.eval(`(function(){ var btn = Array.prototype.filter.call(document.querySelectorAll('#view button.cell'), function(x){ return x.getAttribute("data-role") && x.textContent.indexOf("⤒") >= 0 && (x.getAttribute("title")||"").indexOf("Shelf") >= 0; })[0];
				if (!btn) return {Found: false};
				btn.click();
				function text(id){ return (document.getElementById(id)||{}).textContent || ""; }
				return {Found: true, List: text("cellAncestors"), Unknown: text("cellAncestorUnknown"), Cut: text("cellAncestorIncomplete"),
					Scroll: document.documentElement.scrollWidth, Width: window.innerWidth}; })()`, &gone)
			if !gone.Found || !strings.Contains(gone.List, "shelf#view ⚠ incomplete: the model holds no grant path of it; the model holds no grant path of it, so this role may hold it") ||
				!strings.Contains(gone.Unknown, "the model holds no grant path of the ancestor read Shelf (shelf#view)") || gone.Cut != "" {
				t.Errorf("at %d px the details of an ancestor read whose permission the model does not hold show %+v", width, gone)
			}
			if gone.Scroll > gone.Width {
				t.Errorf("at %d px the edited page is %d px wide", width, gone.Scroll)
			}
			if blocked, errors := b.problems(); len(blocked) > 0 || len(errors) > 0 {
				t.Errorf("at %d px: blocked %v, errors %v", width, blocked, errors)
			}
		}
		b.viewport(1280, 900)
	})

	t.Run("ST-GEN-UC08-R07-T06 The trust view of a model with no role shows markup in the stop text of a grant path as text only, and runs none of it", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "index.html")
		out, err := RenderArchitectureView(hostileStopsModel(t))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, out, 0o644); err != nil {
			t.Fatal(err)
		}
		b.do("Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": "window.__ran = 0; window.alert = function(){ window.__ran++; };"})
		b.problems()
		b.open("file://" + path + "#view=trust")
		var got struct {
			Note, Grant string
			Found       bool
			Images, Ran int
		}
		b.eval(`(function(){ var n = document.getElementById("no-role-named"), note = n && n.offsetHeight > 0 ? n.textContent : "";
			var btn = document.querySelector('#view button.cell[data-check="check:operation:docs.read"][data-role=""]');
			if (!btn) return {Note: note, Found: false};
			btn.click();
			var li = document.querySelector("#details li[data-grant]");
			return {Note: note, Found: true, Grant: li ? li.textContent : "", Images: document.querySelectorAll("img").length, Ran: window.__ran}; })()`, &got)
		if !strings.Contains(got.Note, "before a limit stopped it: "+hostileStops+".") {
			t.Errorf("the note does not show the hostile stop text as text: %q", got.Note)
		}
		if !got.Found || !strings.Contains(got.Grant, "stops: "+hostileStops) || got.Images != 0 || got.Ran != 0 {
			t.Errorf("the details of the cell of the paths that stop do not show the hostile stop text as text only: %+v", got)
		}
		if blocked, errors := b.problems(); len(blocked) > 0 || len(errors) > 0 {
			t.Errorf("blocked %v, errors %v", blocked, errors)
		}
	})

	t.Run("ST-GEN-UC08-R02-T03 Every view and the tour work under the content security policy of the view, with no violation", func(t *testing.T) {
		b.do("Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": `window.__csp = [];
			document.addEventListener("securitypolicyviolation", function(e){ window.__csp.push(e.violatedDirective+" "+e.blockedURI+" "+e.sample); });`})
		b.problems()
		b.eval(`localStorage.clear()`, nil)
		b.open(access)
		var policy string
		b.eval(`(document.querySelector('meta[http-equiv="Content-Security-Policy"]')||{}).content || ""`, &policy)
		if !strings.HasPrefix(policy, "default-src 'none'; script-src 'sha256-") || strings.Contains(policy, "unsafe-eval") || strings.Contains(policy, "script-src 'unsafe-inline'") ||
			!strings.HasSuffix(policy, "; require-trusted-types-for 'script'") {
			t.Fatalf("the view states no strict policy: %q", policy)
		}
		b.eval(`document.getElementById("tourStart").click(); for (var i = 0; i < 5 && document.getElementById("tour"); i++){ var r = document.getElementById("tourReveal"); if (r) r.click(); document.getElementById("tourNext").click(); }`, nil)
		for _, v := range []string{"overview", "components", "data", "flows", "trust", "platform"} {
			b.eval(`document.querySelector('#nav button[data-view="`+v+`"]').click(); var c = document.querySelector("#view .el, #view button.cell"); if (c) c.dispatchEvent(new MouseEvent("click", {bubbles: true}));
				document.getElementById("textToggle") && document.getElementById("textToggle").click();`, nil)
		}
		var drawn bool
		b.eval(`!!document.querySelector("#nav button") && document.documentElement.classList.contains("js")`, &drawn)
		var violations []string
		b.eval(`window.__csp`, &violations)
		if !drawn || len(violations) > 0 {
			t.Fatalf("the view did not run under its policy (%v), or the policy refused %v", drawn, violations)
		}
		if blocked, errors := b.problems(); len(blocked)+len(errors) > 0 {
			t.Fatalf("requests %v or errors %v under the policy", blocked, errors)
		}
		// The browser holds the view to trusted types: a write of a text as HTML is refused.
		var refused string
		b.eval(`(function(){ try { document.body.insertAdjacentHTML("beforeend", "<b id=tt>x</b>"); return "accepted"; } catch (e) { return e.name; } })()`, &refused)
		if refused != "TypeError" {
			t.Fatalf("a write of a text as HTML under the policy gives %q, want a TypeError of trusted types", refused)
		}
		b.problems()
		b.eval(`localStorage.setItem("stego-arch-tour", "seen")`, nil)
	})

	t.Run("ST-GEN-UC08-R09-T02 The tour card covers no part of the view that it explains, and shows together with it", func(t *testing.T) {
		defer b.viewport(1280, 900)
		for _, width := range []int{1280, 400} {
			b.viewport(width, 860)
			b.eval(`localStorage.setItem("stego-arch-tour", "seen")`, nil)
			b.open(plain + "#view=overview")
			b.eval(`document.getElementById("tourBtn").click()`, nil)
			for i := 0; i < 6; i++ {
				var got struct {
					Title     string
					Covers    []string
					CardShown bool
					PartShown bool
				}
				b.eval(`(function(){ var card = document.getElementById("tour"); if (!card) return {Title: ""};
					var r = card.getBoundingClientRect(), out = [];
					document.querySelectorAll("#view .canvas, #view .steps li.cur, #flowTitle, .stepper").forEach(function(e){ var q = e.getBoundingClientRect();
						if (q.width && q.height && r.left < q.right && q.left < r.right && r.top < q.bottom && q.top < r.bottom) out.push(e.className || e.id); });
					var part = document.getElementById("flowTitle") || document.querySelector("#view .canvas") || document.getElementById("view"), p = part.getBoundingClientRect();
					return {Title: document.getElementById("tourTitle").textContent, Covers: out,
						CardShown: r.top >= 0 && r.top < innerHeight, PartShown: p.top < innerHeight && p.bottom > 0}; })()`, &got)
				if got.Title == "" {
					break
				}
				if len(got.Covers) > 0 || !got.CardShown || !got.PartShown {
					t.Errorf("%d px, stop %q: the card covers %v, or the card (%v) and the part it explains (%v) do not show together", width, got.Title, got.Covers, got.CardShown, got.PartShown)
				}
				b.eval(`document.getElementById("tourNext").click()`, nil)
			}
		}
	})

	t.Run("ST-GEN-UC08-R11-T02 The trust view leads with a readable boundary map and the access matrix, and folds its tables behind controls that open on demand", func(t *testing.T) {
		b.open(access + "#view=trust")
		var got struct {
			First     string
			Regions   []string
			Scale     float64
			Order     []string
			Folded    []string
			Open      int
			Decided   string
			Crossings int
		}
		b.eval(`(function(){ var v = document.getElementById("view"), svg = v.querySelector(".canvas svg");
			var out = {First: v.firstElementChild.className, Regions: Array.prototype.map.call(svg.querySelectorAll(".region"), function(g){ return g.getAttribute("data-id"); }).sort(),
				Scale: svg.getBoundingClientRect().width / svg.viewBox.baseVal.width, Order: [], Folded: [], Open: v.querySelectorAll("details[open]").length,
				Crossings: svg.querySelectorAll(".el").length};
			v.querySelectorAll(":scope > .canvas, :scope > h3, :scope > details, :scope > .matrix-wrap").forEach(function(e){ out.Order.push(e.tagName === "H3" ? e.textContent : e.tagName === "DETAILS" ? "details" : e.className); });
			v.querySelectorAll("details > summary .t").forEach(function(x){ out.Folded.push(x.textContent); });
			document.querySelector('#view button.cell[data-check="check:operation:org-users.read"]').click();
			out.Decided = document.getElementById("details").textContent;
			// With the details open beside it, the map keeps a size that reads.
			out.Scale = Math.min(out.Scale, svg.getBoundingClientRect().width / svg.viewBox.baseVal.width);
			return out; })()`, &got)
		var want []string
		for _, bd := range am.Boundaries {
			want = append(want, bd.ID)
		}
		sort.Strings(want)
		if !strings.Contains(got.First, "canvas") || !slices.Equal(got.Regions, want) {
			t.Fatalf("the trust view does not lead with a map of every boundary: first %q, regions %v", got.First, got.Regions)
		}
		if got.Scale < 0.78 {
			t.Errorf("the boundary map is drawn at %.2f of its size, too small to read", got.Scale)
		}
		if len(got.Order) < 3 || got.Order[1] != "Access matrix: who may call each operation" {
			t.Errorf("the access matrix does not follow the map: %v", got.Order)
		}
		for _, title := range []string{"Where a caller is authenticated, and where authorization is decided", "What each operation checks", "Grant paths"} {
			if !slices.Contains(got.Folded, title) {
				t.Errorf("%q is not folded behind a control: %v", title, got.Folded)
			}
		}
		if got.Open != 0 {
			t.Errorf("%d folded parts are open before the reader opens them", got.Open)
		}
		if !strings.Contains(got.Decided, "Decided") || !strings.Contains(got.Decided, "Decides the permission view on the member") {
			t.Errorf("the details of a cell do not say where the operation is decided: %q", got.Decided)
		}
	})

	t.Run("ST-GEN-UC08-R14-T03 The access matrix shows every operation at 1280 px, grouped by collection, and keeps its headers in view as it scrolls", func(t *testing.T) {
		b.viewport(1280, 900)
		b.open(access + "#view=trust")
		var got struct {
			Heads, Groups    int
			Sticky           []string
			HeadTop, WrapTop float64
		}
		b.eval(`(function(){ var t = document.querySelector("#view table.access"), wrap = t.closest(".matrix-wrap");
			var heads = t.querySelectorAll('th[data-id^="check:"]');
			var out = {Heads: heads.length, Groups: t.querySelectorAll('th[scope$="group"]').length,
				Sticky: [getComputedStyle(t.querySelector("thead th")).position, getComputedStyle(heads[0]).position]};
			wrap.scrollTop = 400;
			out.HeadTop = t.querySelector("thead th").getBoundingClientRect().top; out.WrapTop = wrap.getBoundingClientRect().top;
			return out; })()`, &got)
		if got.Heads != len(am.Access.Checks) {
			t.Errorf("the matrix shows %d of %d operations", got.Heads, len(am.Access.Checks))
		}
		if got.Groups < len(am.Collections) {
			t.Errorf("the matrix groups the operations in %d groups for %d collections", got.Groups, len(am.Collections))
		}
		if !slices.Equal(got.Sticky, []string{"sticky", "sticky"}) || got.HeadTop-got.WrapTop > 2 {
			t.Errorf("the headers do not stay in view: %v, head at %.0f, matrix at %.0f", got.Sticky, got.HeadTop, got.WrapTop)
		}
	})

	t.Run("ST-GEN-UC08-R08-T06 Each label that a diagram cuts short has its full form on hover and on focus, and no diagram shows the layer by colour alone", func(t *testing.T) {
		b.viewport(400, 860)
		defer b.viewport(1280, 900)
		cut := 0
		for _, v := range []string{"overview", "components", "data", "trust", "flows"} {
			b.open(access + "#view=" + v + "&flow=org-users.create")
			var got struct {
				Cut     int
				NoTitle []string
				Focus   []string
			}
			b.eval(`(function(){ var out = {Cut: 0, NoTitle: [], Focus: []};
				document.querySelectorAll("#view svg text").forEach(function(t){ var own = t.firstChild && t.firstChild.nodeType === 3 ? t.firstChild.textContent : "";
					if (own.slice(-1) !== "…") return; out.Cut++;
					var title = t.querySelector("title"), full = title ? title.textContent : "";
					if (!full || full.indexOf(own.slice(0, -1)) !== 0) out.NoTitle.push(own);
					var el = t.closest(".el"); if (!el) return;
					el.focus(); var info = el.closest(".canvas").querySelector(".hover-info").textContent;
					if (info.indexOf(full.replace(/^→ /, "")) < 0) out.Focus.push(own+" shows "+info); });
				return out; })()`, &got)
			cut += got.Cut
			if len(got.NoTitle) > 0 || len(got.Focus) > 0 {
				t.Errorf("view %s: labels cut with no full form %v, or no full form on focus %v", v, got.NoTitle, got.Focus)
			}
		}
		if cut == 0 {
			t.Error("no label is cut at 400 px, so the test proves nothing")
		}
		b.open(access + "#view=flows")
		var places map[string][]float64
		b.eval(`(function(){ var out = {}, f = document.querySelector(".mini button[data-flow]"), id = f.getAttribute("data-flow");
			var steps = JSON.parse(document.getElementById("model").textContent).flows.filter(function(x){ return x.id === id; })[0].steps;
			f.querySelectorAll("circle").forEach(function(c, i){ (out[steps[i].layer] = out[steps[i].layer] || []).push(Number(c.getAttribute("cy"))); });
			return out; })()`, &places)
		seen := map[float64]string{}
		for layer, ys := range places {
			for _, y := range ys {
				if other, ok := seen[y]; (ok && other != layer) || y != ys[0] {
					t.Errorf("the overview of flows does not give each layer its own place: %v", places)
				}
				seen[y] = layer
			}
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
	log     *os.File
	conn    *websocket.Conn
	ctx     context.Context
	cancel  context.CancelFunc
	session string
	mu      sync.Mutex
	next    int
	waiting map[int]chan cdpReply
	exited  chan struct{} // closed when the browser process has exited; exit then holds what Wait gave
	exit    error
	drained chan struct{} // closed when all of stderr is in browser.log
	stderr  *os.File      // the read end of the browser's stderr pipe
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
	if path == "" && hostedRunner() {
		// A GitHub-hosted runner has a browser: the proof run uses it, so the browser tests never skip there. A run
		// that only sets GITHUB_ACTIONS, such as act on a workstation, does not look for one (#345 F5).
		for _, name := range []string{"google-chrome", "chromium", "chromium-browser"} {
			if found, err := exec.LookPath(name); err == nil {
				path = found
				break
			}
		}
		if path == "" {
			t.Fatal("the CI runner has no Chrome or Chromium for the browser tests of the architecture view")
		}
	}
	if path == "" {
		if os.Getenv("STEGO_REQUIRE_BROWSER_VIEW") == "1" {
			t.Fatal("STEGO_REQUIRE_BROWSER_VIEW is set, and STEGO_BROWSER names no browser")
		}
		t.Skip("set STEGO_BROWSER to a Chrome or Chromium executable to run the browser tests of the architecture view")
	}
	dir := t.TempDir()
	// The flags after --disable-sync remove start work that needs no page: the key store over D-Bus, the component
	// updater and the crash reporter. A slow start on a loaded runner then has less to do.
	args := []string{"--headless=new", "--remote-debugging-port=0", "--user-data-dir=" + dir, "--no-first-run",
		"--no-default-browser-check", "--disable-background-networking", "--disable-gpu", "--disable-dev-shm-usage",
		"--disable-extensions", "--disable-sync", "--password-store=basic", "--use-mock-keychain",
		"--disable-component-update", "--disable-breakpad", "--mute-audio", "--window-size=1280,900", "about:blank"}
	// The browser runs with its sandbox, except on a GitHub-hosted runner, whose throwaway machine cannot start it
	// (#342 F2). No variable turns the sandbox off anywhere else.
	if hostedRunner() {
		args = append([]string{"--no-sandbox"}, args...)
	}
	cmd := exec.Command(path, args...)
	// The browser dies with the test process, even when the test is killed (#342 F3).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	log, err := os.Create(filepath.Join(dir, "browser.log"))
	if err != nil {
		t.Fatal(err)
	}
	// The browser writes its DevTools address to stderr. The test reads stderr through a pipe and copies each line to
	// browser.log; stdout goes to browser.log directly.
	stderr, w, err := os.Pipe()
	if err != nil {
		_ = log.Close()
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = log, w
	if err := cmd.Start(); err != nil {
		_, _ = stderr.Close(), w.Close()
		_ = log.Close()
		t.Fatalf("cannot start the browser: %v", err)
	}
	_ = w.Close()
	b := &cdpBrowser{t: t, cmd: cmd, log: log, waiting: map[int]chan cdpReply{}, exited: make(chan struct{}),
		drained: make(chan struct{}), stderr: stderr}
	go func() {
		b.exit = cmd.Wait()
		close(b.exited)
	}()
	t.Cleanup(b.stop)
	ws := b.devToolsAddress(stderr, filepath.Join(dir, "browser.log"))
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
	b.signal(syscall.SIGTERM)
	select {
	case <-b.exited:
	case <-time.After(5 * time.Second):
	}
	b.release()
	if b.log != nil {
		_ = b.log.Close()
	}
}

// signal sends sig to the process group of the browser, only while the browser has not exited: after Wait reaped
// it, its process group ID may belong to another group.
func (b *cdpBrowser) signal(sig syscall.Signal) {
	select {
	case <-b.exited:
	default:
		_ = syscall.Kill(-b.cmd.Process.Pid, sig)
	}
}

// release kills the browser group, waits until the browser exited and no process of its group is left, and then
// closes the read end of the stderr pipe, so the copy to browser.log ends even when a helper that left the group
// holds the pipe open. No timer bounds it.
func (b *cdpBrowser) release() {
	b.signal(syscall.SIGKILL)
	<-b.exited
	for groupAlive(b.cmd.Process.Pid) {
		time.Sleep(50 * time.Millisecond)
	}
	select {
	case <-b.drained:
	default:
		_ = b.stderr.Close()
		<-b.drained
	}
}

// groupAlive tells whether a process of the process group pgid is left, from the process group field of each
// /proc/<pid>/stat.
func groupAlive(pgid int) bool {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		// The command name is in parentheses and may hold spaces; the fields after its ")" are state, ppid and pgrp.
		rest := string(data)
		if i := strings.LastIndexByte(rest, ')'); i >= 0 {
			rest = rest[i+1:]
		}
		if fields := strings.Fields(rest); len(fields) >= 3 && fields[2] == strconv.Itoa(pgid) {
			return true
		}
	}
	return false
}

// devToolsAddress reads the browser's stderr until the line "DevTools listening on ws://..." comes, and gives that
// address. It has no time limit: a slow start on a loaded runner is not a failure. When the browser exits or closes
// stderr first, the test fails and shows the exit status and browser.log. Each line before the address goes to the
// test log and to stderr as it comes, so a start that does not end is diagnosable from the test output.
func (b *cdpBrowser) devToolsAddress(stderr *os.File, logPath string) string {
	b.t.Helper()
	lines := make(chan string)
	go func() {
		defer close(b.drained)
		defer stderr.Close()
		defer close(lines)
		reader := bufio.NewReader(stderr)
		for {
			line, err := reader.ReadString('\n')
			if line != "" {
				_, _ = b.log.WriteString(line)
				lines <- strings.TrimRight(line, "\r\n")
			}
			if err != nil {
				return
			}
		}
	}()
	started := time.Now()
	for {
		select {
		case line, open := <-lines:
			if !open {
				b.startFailed("the browser closed stderr before it wrote its DevTools address", logPath, lines)
			}
			if _, ws, found := strings.Cut(line, "DevTools listening on "); found && strings.HasPrefix(ws, "ws://") {
				b.t.Logf("the browser started in %s", time.Since(started).Round(time.Millisecond))
				// The rest of stderr goes on to browser.log only, so that the browser never stops on a full pipe.
				go func() {
					for range lines {
					}
				}()
				return ws
			}
			if line != "" {
				b.t.Logf("browser: %s", line)
				// A run without -v keeps the test log until the test ends, and a timeout panic loses it: each line
				// also goes to stderr at once, so a start that does not end is in the CI log.
				fmt.Fprintf(os.Stderr, "%s: chrome: %s\n", b.t.Name(), line)
			}
		case <-b.exited:
			b.startFailed("the browser exited before it wrote its DevTools address", logPath, lines)
		}
	}
}

// startFailed stops the browser group, waits until browser.log has all of stderr, and fails the test with the exit
// status and the log.
func (b *cdpBrowser) startFailed(reason, logPath string, lines <-chan string) {
	b.t.Helper()
	go func() {
		for range lines {
		}
	}()
	b.release()
	data, err := os.ReadFile(logPath)
	if err != nil {
		data = []byte("(cannot read browser.log: " + err.Error() + ")")
	}
	status := "no exit status"
	if b.cmd.ProcessState != nil {
		status = b.cmd.ProcessState.String()
	} else if b.exit != nil {
		status = b.exit.Error()
	}
	b.t.Fatalf("%s (%s)\nbrowser.log:\n%s", reason, status, data)
}

// hostedRunner says whether the test runs on a GitHub-hosted runner: GITHUB_ACTIONS and RUNNER_ENVIRONMENT both
// say so. A self-hosted runner or a developer machine keeps the browser's sandbox.
func hostedRunner() bool {
	return os.Getenv("GITHUB_ACTIONS") == "true" && os.Getenv("RUNNER_ENVIRONMENT") == "github-hosted"
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
