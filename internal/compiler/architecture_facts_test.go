package compiler

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// These tests prove the list of the compliance facts of a topic (ST-GEN-UC16, ST-GEN-QRY06) on models that
// WriteArchitecture derives from the fixture of architecture_test.go, in a project folder whose record lists them.

// archProject writes a project folder with the model and a record that lists it, with the input digests of the
// record of the example user-management.
func archProject(t *testing.T, data []byte) string {
	t.Helper()
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, "architecture"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ArchitectureFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
	// The record of the example user-management holds the input digests of a real apply.
	state, err := LoadState(filepath.Join("..", "..", "examples", "user-management", ".stego", "state.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	state.LastApplied.ProjectFiles = map[string]string{ArchitectureFile: HashBytes(data)}
	if err := SaveState(filepath.Join(project, ".stego", "state.yaml"), state); err != nil {
		t.Fatal(err)
	}
	return project
}

// factsOf gives the list of a topic as text and as JSON, and fails the test on a refusal.
func factsOf(t *testing.T, project, topic string) (string, FactList, []byte) {
	t.Helper()
	text, err := ListComplianceFacts(project, topic, false)
	if err != nil {
		t.Fatalf("the facts of %s: %v", topic, err)
	}
	data, err := ListComplianceFacts(project, topic, true)
	if err != nil {
		t.Fatalf("the facts of %s as JSON: %v", topic, err)
	}
	var list FactList
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&list); err != nil {
		t.Fatalf("the JSON of %s: %v\n%s", topic, err, data)
	}
	return string(text), list, data
}

// claimFixture is the fixture whose public reason, written by the service author, holds words of a claim.
const claimReason = "the service is compliant, passes each control and meets the law"

// claimFixtureProject derives the fixture with a public reason that holds claim words and writes its project.
func claimFixtureProject(t *testing.T, reason string) (*ArchitectureModel, string) {
	t.Helper()
	in := archFixture(t)
	in.Service.Entities[0].PublicReasons["create"] = reason
	model, data := deriveFixture(t, in)
	return model, archProject(t, data)
}

// testClaimWords is the list of this test of the words that would claim compliance, kept apart from claimWords so a
// change of one is caught by the other.
var testClaimWords = regexp.MustCompile(`(?i)\b(compliant|complies|comply|compliance|passes|passed|pass|meets|meet|fails|failed|fail|satisfies|conforms|certified|secure|safe|protected|approved)\b`)

// quotedText is a quotation in the text output: a double-quoted string with its escapes.
var quotedText = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)

func TestComplianceFactList(t *testing.T) {
	t.Run("ST-GEN-UC16-R01-T03 The list of a topic holds each fact that the model holds for it, and the access list names who each permission grants", func(t *testing.T) {
		model, project := claimFixtureProject(t, "a new doc has no resource before it exists")
		for _, topic := range ComplianceTopics {
			_, list, _ := factsOf(t, project, topic)
			var want, got []string
			for _, top := range model.Compliance {
				if top.ID == "topic:"+topic {
					for _, f := range top.Facts {
						want = append(want, f.ID)
					}
				}
			}
			for _, f := range list.Facts {
				got = append(got, f.ID)
			}
			// The configuration topic of a model with no environment variable holds no fact (ST-GEN-UC16-R07).
			empty := len(want) == 0 && !(topic == "configuration" && len(model.Variables) == 0)
			if empty || strings.Join(got, " ") != strings.Join(want, " ") || list.Topic != topic || list.Service != "docs" {
				t.Fatalf("the topic %s lists %v, want %v", topic, got, want)
			}
		}
		text, list, _ := factsOf(t, project, "access")
		var update *FactOfList
		for i := range list.Facts {
			if list.Facts[i].About == "operation:docs.update" && list.Facts[i].Name == "checks" {
				update = &list.Facts[i]
			}
		}
		permission := pathsOf(model, "permission:doc#write")
		if update == nil || update.Grants == nil || update.Grants.Permission != "permission:doc#write" || len(update.Grants.Paths) != len(permission) || len(permission) == 0 {
			t.Fatalf("the check of operation:docs.update names no grant paths: %+v", update)
		}
		for _, p := range update.Grants.Paths {
			if p.Role != "" && !strings.Contains(text, "granted to "+p.Role) {
				t.Fatalf("the text does not name the role %s of operation:docs.update:\n%s", p.Role, text)
			}
		}
		if _, err := ListComplianceFacts(project, "secrets-at-rest", false); err == nil || !strings.Contains(err.Error(), "personal-data") {
			t.Fatalf("a topic that is not known: %v", err)
		}
	})

	t.Run("ST-GEN-UC16-R01-T04 The access list gives each ancestor check of a nested operation: the decided parent of a list with its permission and who it grants, each other ancestor as exists and linked, and unstated with its rule when no authorization is bound", func(t *testing.T) {
		model, data := deriveFixture(t, nestedFixture(t))
		text, list, _ := factsOf(t, archProject(t, data), "access")
		facts := map[string]FactOfList{}
		for _, f := range list.Facts {
			facts[f.About+"|"+f.Name] = f
		}
		// The list is decided on the folder, its parent; the shelf above it is checked for existence and its link.
		folder, ok := facts["operation:folder-docs.list|checks ancestor 2 Folder"]
		permission := pathsOf(model, "permission:folder#view")
		if !ok || folder.Value != "folder#view" || folder.State != "stated" || folder.Source.Declaration != "entities[1].permissions.read" ||
			folder.Grants == nil || folder.Grants.Permission != "permission:folder#view" || len(folder.Grants.Paths) != len(permission) || len(permission) == 0 {
			t.Fatalf("the ancestor check of the folder is %+v", folder)
		}
		// An update and a read are decided on the row: each ancestor is checked for existence and its link only, and
		// grants nothing (ST-SUP-RC20-OC24, ST-SUP-Q156).
		for _, key := range []string{"operation:folder-docs.list|checks ancestor 1 Shelf", "operation:folder-docs.update|checks ancestor 1 Shelf",
			"operation:folder-docs.update|checks ancestor 2 Folder", "operation:folder-docs.read|checks ancestor 2 Folder"} {
			f, ok := facts[key]
			if !ok || f.Value != ancestorLinkedFact || f.State != "stated" || f.Source.Rule != "ST-SUP-RC20-OC24" || f.Grants != nil || f.GrantsUnknown || len(f.Quoted) != 0 {
				t.Fatalf("the ancestor check %s is %+v", key, f)
			}
		}
		if _, ok := facts["operation:shelf-folders.read|checks ancestor 1 Shelf"]; !ok {
			t.Fatal("the list holds no ancestor check of shelf-folders.read")
		}
		for _, f := range list.Facts {
			if f.About == "operation:docs.read" && strings.HasPrefix(f.Name, "checks ancestor") {
				t.Fatalf("an operation with no scope has an ancestor check: %+v", f)
			}
		}
		// The text gives the shelf before the folder, the top one first, and who folder#view grants.
		at := strings.Index(text, "operation:folder-docs.list\n")
		if at < 0 {
			t.Fatalf("the text names no operation:folder-docs.list:\n%s", text)
		}
		block := text[at:]
		if s, f := strings.Index(block, "checks ancestor 1 Shelf: "+ancestorLinkedFact), strings.Index(block, "checks ancestor 2 Folder: folder#view"); s < 0 || f < s {
			t.Fatalf("the text does not give the shelf, then the folder:\n%s", block)
		}
		for _, p := range folder.Grants.Paths {
			if p.Role != "" && !strings.Contains(block, "granted to "+p.Role) {
				t.Fatalf("the text does not name the role %s of the folder check:\n%s", p.Role, block)
			}
		}
		// With no bound authorization, the fact of a decided parent is unstated, with the rule that binds none; each
		// other ancestor is still checked for existence and its link.
		in := nestedFixture(t)
		in.Service.Authorization, in.Schema = nil, nil
		_, unbound := deriveFixture(t, in)
		_, list, _ = factsOf(t, archProject(t, unbound), "access")
		decided, linked := 0, 0
		for _, f := range list.Facts {
			if !strings.HasPrefix(f.Name, "checks ancestor") {
				continue
			}
			switch {
			case f.State == "unstated" && f.Source.Rule == "ST-DECL-UC06-R01" && f.Grants == nil:
				decided++
			case f.State == "stated" && f.Value == ancestorLinkedFact && f.Source.Rule == "ST-SUP-RC20-OC24" && f.Grants == nil:
				linked++
			default:
				t.Errorf("with no authorization, the ancestor fact is %+v", f)
			}
		}
		if decided == 0 || linked == 0 {
			t.Errorf("with no authorization, the list holds %d decided and %d linked ancestor facts", decided, linked)
		}
	})

	t.Run("ST-GEN-UC16-R01-T05 The checks fact of a nested operation names each ancestor read whose grant paths a limit cut short", func(t *testing.T) {
		_, data := deriveFixture(t, nestedFixture(t))
		_, list, _ := factsOf(t, archProject(t, data), "access")
		for _, f := range list.Facts {
			if len(f.AncestorsIncomplete) != 0 {
				t.Fatalf("a complete model gives an incomplete ancestor read: %+v", f)
			}
		}
		// The same model with a limit in the grant paths of folder#view.
		var raw map[string]any
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatal(err)
		}
		access := raw["access"].(map[string]any)
		access["roles_complete"] = false
		for _, perm := range access["permissions"].([]any) {
			if perm.(map[string]any)["id"] == "permission:folder#view" {
				perm.(map[string]any)["complete"] = false
			}
		}
		cut, err := json.MarshalIndent(raw, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		text, list, out := factsOf(t, archProject(t, append(cut, '\n')), "access")
		facts := map[string]FactOfList{}
		for _, f := range list.Facts {
			facts[f.About+"|"+f.Name] = f
		}
		// Only the list decides the folder read; the update and the read of a doc decide no ancestor, so they name no
		// incomplete ancestor read (ST-SUP-RC20-OC24, ST-SUP-Q156).
		want := []FactAncestor{{Fact: "checks ancestor 2 Folder", Permission: "permission:folder#view"}}
		if got := facts["operation:folder-docs.list|checks"].AncestorsIncomplete; !slices.Equal(got, want) {
			t.Errorf("the checks fact of operation:folder-docs.list names the incomplete ancestor reads %+v, not %+v", got, want)
		}
		for _, op := range []string{"operation:docs.read", "operation:shelf-folders.read", "operation:folder-docs.update", "operation:folder-docs.read"} {
			if got := facts[op+"|checks"].AncestorsIncomplete; len(got) != 0 {
				t.Errorf("the checks fact of %s, which reads no ancestor with a permission, names %+v", op, got)
			}
		}
		if !bytes.Contains(out, []byte(`"ancestors_incomplete"`)) {
			t.Error("the JSON list does not name the incomplete ancestor read")
		}
		at := strings.Index(text, "operation:folder-docs.list\n")
		if at < 0 {
			t.Fatalf("the text names no operation:folder-docs.list:\n%s", text)
		}
		block := text[at:]
		checks := strings.Index(block, "  checks: ")
		line := strings.Index(block, "a limit cut a path of the ancestor read permission:folder#view short (checks ancestor 2 Folder), so it can grant more roles than its paths name")
		if checks < 0 || line < checks || strings.Index(block, "checks ancestor 1 Shelf") < line {
			t.Fatalf("the text does not mark the incomplete ancestor read at the checks fact of folder-docs.list:\n%s", block)
		}
	})

	t.Run("ST-GEN-UC16-R01-T06 The checks fact of an operation whose grant paths a limit cut short names the roles that may hold it, as the cells of the access matrix do", func(t *testing.T) {
		// mayHoldCells gives, for each check of the view, the roles whose cell of the access matrix says may hold.
		mayHoldCells := func(data []byte) map[string][]string {
			view, err := RenderArchitectureView(data)
			if err != nil {
				t.Fatal(err)
			}
			out := map[string][]string{}
			for _, b := range parseView(t, view).all(func(n *html.Node) bool {
				return n.Data == "button" && strings.Contains(attrOf(n, "class"), "may-hold")
			}) {
				out[attrOf(b, "data-check")] = append(out[attrOf(b, "data-check")], attrOf(b, "data-role"))
			}
			for k := range out {
				slices.Sort(out[k])
			}
			return out
		}
		// agree lists the facts of the model and compares the roles that may hold the permission of each checks fact
		// with the cells of the matrix; it gives the number of checks facts with such roles.
		agree := func(name string, data []byte) (int, string) {
			text, list, out := factsOf(t, archProject(t, data), "access")
			cells := mayHoldCells(data)
			n := 0
			for _, f := range list.Facts {
				if f.Name != "checks" {
					continue
				}
				var got []string
				if f.Grants != nil {
					got = slices.Sorted(slices.Values(f.Grants.MayHold))
					if len(got) > 0 && f.Grants.Complete {
						t.Errorf("%s: the checks fact of %s names roles that may hold a complete permission: %v", name, f.About, got)
					}
				}
				if want := cells["check:"+f.About]; !slices.Equal(got, want) {
					t.Errorf("%s: the checks fact of %s names the roles that may hold it %v; the matrix says %v", name, f.About, got, want)
				}
				if len(got) > 0 {
					n++
					if !strings.Contains(text, "no path that the model names gives it to "+strings.Join(f.Grants.MayHold, ", ")+", but each may hold it by a path past the limit") {
						t.Errorf("%s: the text does not name the roles that may hold %s", name, f.About)
					}
				}
			}
			if n > 0 && !bytes.Contains(out, []byte(`"may_hold"`)) {
				t.Errorf("%s: the JSON list does not name the roles that may hold", name)
			}
			return n, text
		}
		data, err := os.ReadFile("testdata/architecture/access.json")
		if err != nil {
			t.Fatal(err)
		}
		if n, _ := agree("access.json", data); n == 0 {
			t.Fatal("the fixture proves too little: no checks fact of a permission whose paths a limit cut short")
		}
		// variant gives access.json with the key complete of each named permission set to the value, or deleted
		// when the value is nil.
		variant := func(complete map[string]any) []byte {
			var raw map[string]any
			if err := json.Unmarshal(data, &raw); err != nil {
				t.Fatal(err)
			}
			for _, perm := range raw["access"].(map[string]any)["permissions"].([]any) {
				obj := perm.(map[string]any)
				if v, ok := complete[obj["id"].(string)]; ok && v != nil {
					obj["complete"] = v
				} else if ok {
					delete(obj, "complete")
				}
			}
			out, err := json.MarshalIndent(raw, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			return append(out, '\n')
		}
		// With the paths complete, no role may hold, and the text says no more than the paths.
		if n, text := agree("complete", variant(map[string]any{"permission:organization#view": true})); n != 0 || strings.Contains(text, "may hold") {
			t.Errorf("the facts of a complete model name %d checks with roles that may hold", n)
		}
		// The list refuses a model with no key complete in a permission, which the view reads as cut short (security
		// review of PR #383, L3): neither tells a reader that the paths are complete.
		if _, err := ListComplianceFacts(archProject(t, variant(map[string]any{"permission:member#view": nil})), "access", true); err == nil ||
			!strings.Contains(err.Error(), "complete") {
			t.Errorf("the list of a model with no key complete gives %v, not a refusal", err)
		}
	})

	t.Run("ST-GEN-UC16-R01-T07 The checks fact of a permission whose every grant path a limit stopped before a role says that a role may hold it, never that no role holds it", func(t *testing.T) {
		// A chain of rewrites one past the depth limit: the model names no role, as the access matrix of
		// ST-GEN-UC08-R14-T09 shows it.
		_, data := deriveFixture(t, chainFixture(t, maxGrantDepth+1))
		text, list, _ := factsOf(t, archProject(t, data), "access")
		n := 0
		for _, f := range list.Facts {
			if f.Name != "checks" || f.Grants == nil {
				continue
			}
			n++
			if f.Grants.Complete || len(f.Grants.Paths) == 0 {
				t.Errorf("the checks fact of %s says that its grant paths are complete, or has none: %+v", f.About, f.Grants)
			}
			for _, g := range f.Grants.Paths {
				if g.Role != "" || !slices.Contains(g.Kinds, "depth limit") {
					t.Errorf("the checks fact of %s has a path that does not stop at the depth limit: %+v", f.About, g)
				}
			}
			line := "no path of " + f.Grants.Permission + " reached a role before a limit stopped it (the depth limit of 12 nodes), so the model names no role that holds it, and a role may hold it by a path past the limit"
			if !strings.Contains(text, line) {
				t.Errorf("the text does not say that a role may hold %s", f.Grants.Permission)
			}
		}
		if n == 0 {
			t.Fatal("the chain fixture proves too little: no checks fact with grants")
		}
		if strings.Contains(text, "so no role holds it") || strings.Contains(text, "more roles than these") {
			t.Error("the text of grant paths that a limit stopped before a role says that no role holds them, or names roles it does not give")
		}
		// A schema that grants nothing, with no limit reached: the text says that no role holds it.
		_, data = deriveFixture(t, chainFixture(t, 0))
		if text, _, _ = factsOf(t, archProject(t, data), "access"); !strings.Contains(text, "permission:doc#read has no grant path, so no role holds it") ||
			strings.Contains(text, "before a limit stopped it") {
			t.Error("the text of a schema that grants nothing does not say that no role holds the permission")
		}
	})

	t.Run("ST-GEN-UC16-R01-T08 The checks fact of a permission whose paths a limit stopped before a role names that limit, and a limit that the model does not name when no path names one", func(t *testing.T) {
		const work = "the work limit of 20000 steps"
		// The work limit stops the expansion of doc#read and doc#write before a path reaches a role, as the access
		// matrix of ST-GEN-UC08-R14-T11 shows it.
		_, data := deriveFixture(t, workFixture(t))
		for _, c := range []struct {
			name   string
			data   []byte
			limits map[string]string
		}{
			{"derived", data, map[string]string{"permission:doc#read": work, "permission:doc#write": work}},
			{"edited", withoutPaths(t, data, "permission:doc#write"), map[string]string{"permission:doc#read": work, "permission:doc#write": unnamedLimit}},
		} {
			text, list, _ := factsOf(t, archProject(t, c.data), "access")
			n := 0
			for _, f := range list.Facts {
				if f.Name != "checks" || f.Grants == nil {
					continue
				}
				n++
				want := c.limits[f.Grants.Permission]
				if f.Grants.Complete || strings.Join(f.Grants.Limits, ",") != want {
					t.Errorf("%s: the checks fact of %s names the limits %v, not %q", c.name, f.About, f.Grants.Limits, want)
				}
				line := "no path of " + f.Grants.Permission + " reached a role before a limit stopped it (" + want + "), so the model names no role that holds it, and a role may hold it by a path past the limit"
				if !strings.Contains(text, line) {
					t.Errorf("%s: the text does not name the limit of %s", c.name, f.Grants.Permission)
				}
			}
			if n < 3 {
				t.Fatalf("%s: the work fixture proves too little: %d checks facts with grants", c.name, n)
			}
			if strings.Contains(text, "no role holds it") || strings.Contains(text, "depth limit") || strings.Contains(text, "more roles than these") {
				t.Errorf("%s: the text says that no role holds a permission, or names a limit that was not reached", c.name)
			}
		}
	})

	t.Run("ST-GEN-UC16-R01-T09 The checks fact of a permission that the path limit stopped names the path limit, and never the work limit", func(t *testing.T) {
		const limit = "the path limit of 512 paths"
		_, data := deriveFixture(t, pathFixture(t))
		text, list, _ := factsOf(t, archProject(t, data), "access")
		n := 0
		for _, f := range list.Facts {
			if f.Name != "checks" || f.Grants == nil {
				continue
			}
			n++
			if f.Grants.Complete || strings.Join(f.Grants.Limits, ",") != limit {
				t.Errorf("the checks fact of %s names the limits %v, not %q", f.About, f.Grants.Limits, limit)
			}
			if !strings.Contains(text, "no path of "+f.Grants.Permission+" reached a role before a limit stopped it ("+limit+")") {
				t.Errorf("the text does not name the path limit of %s", f.Grants.Permission)
			}
		}
		if n < 3 {
			t.Fatalf("the path fixture proves too little: %d checks facts with grants", n)
		}
		if strings.Contains(text, "work limit") || strings.Contains(text, "no role holds it") {
			t.Error("the text names the work limit, or says that no role holds a permission")
		}
	})

	t.Run("ST-GEN-UC16-R01-T10 The access facts of a service with no authorization bound never say that grants are unknown or incomplete, and an ancestor read whose permission the model does not hold is named as incomplete", func(t *testing.T) {
		for name, data := range unboundModels(t) {
			text, list, raw := factsOf(t, archProject(t, data), "access")
			for _, f := range list.Facts {
				if f.GrantsUnknown || f.Grants != nil || len(f.AncestorsIncomplete) > 0 {
					t.Errorf("%s: the fact %s of a service with no authorization has grants: %+v", name, f.ID, f)
				}
			}
			for _, bad := range []string{"not known", "may hold", "incomplete", "grant path"} {
				if strings.Contains(text, bad) {
					t.Errorf("%s: the facts of a service with no authorization say %q:\n%s", name, bad, text)
				}
			}
			if bytes.Contains(raw, []byte("grants_unknown")) || !strings.Contains(text, "unstated") && !strings.Contains(text, "none") {
				t.Errorf("%s: the facts do not say that no permission is checked:\n%s", name, text)
			}
		}
		// The reader refuses grants that name no element of the model, so the facts of an edited model are taken
		// from the model itself.
		model, _ := ancestorGoneModel(t)
		list := complianceFacts(model, "access")
		gone := 0
		for _, f := range list.Facts {
			for _, a := range f.AncestorsIncomplete {
				if a.Permission == "permission:shelf#gone" && a.Unknown {
					gone++
				}
			}
		}
		text := factListText(list)
		if gone == 0 || !strings.Contains(text, "    the model holds no grant path of the ancestor read permission:shelf#gone (") ||
			!strings.Contains(text, "so it can grant roles that the model does not name\n") {
			t.Fatalf("the facts do not name the ancestor read whose permission the model does not hold:\n%s", text)
		}
	})

	t.Run("ST-GEN-UC16-R02-T01 Each fact of a list names its one source, in the text and in the JSON", func(t *testing.T) {
		_, project := claimFixtureProject(t, "a new doc has no resource before it exists")
		for _, topic := range ComplianceTopics {
			text, list, _ := factsOf(t, project, topic)
			for _, f := range list.Facts {
				kinds := 0
				for _, set := range []bool{f.Source.Declaration != "", f.Source.Registry != nil, f.Source.Wiring != "", f.Source.Default != "", f.Source.Rule != ""} {
					if set {
						kinds++
					}
				}
				if kinds != 1 {
					t.Fatalf("the fact %s names %d sources: %+v", f.ID, kinds, f.Source)
				}
			}
			lines := strings.Split(text, "\n")
			facts := 0
			for i, line := range lines {
				if !strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "    ") {
					continue
				}
				facts++
				if i+1 >= len(lines) || !(strings.HasPrefix(lines[i+1], "    source: ") || strings.HasPrefix(lines[i+1], "    default, set by the rule ")) {
					t.Fatalf("the fact %q of %s is not followed by its source:\n%s", line, topic, text)
				}
			}
			if facts != len(list.Facts) {
				t.Fatalf("the text of %s shows %d facts, the JSON %d:\n%s", topic, facts, len(list.Facts), text)
			}
		}
	})

	t.Run("ST-GEN-UC16-R02-T02 A value of the model with control or bidirectional characters is printed escaped, in the text and in the JSON", func(t *testing.T) {
		_, project := claimFixtureProject(t, "opens \x1b]8;;http://x\x07 here \u202eevil\u2066 and \u0085 and \x7f")
		text, _, data := factsOf(t, project, "access")
		for _, output := range []string{text, string(data)} {
			for _, bad := range []string{"\x1b", "\x07", "\u202e", "\u2066", "\u0085", "\x7f"} {
				if strings.Contains(output, bad) {
					t.Fatalf("the output holds the character %q:\n%s", bad, output)
				}
			}
			for _, escaped := range []string{`\u001b`, `\u202e`, `\u2066`, `\u0085`, `\u007f`} {
				if !strings.Contains(output, escaped) {
					t.Fatalf("the output does not show %s:\n%s", escaped, output)
				}
			}
		}
		var list FactList
		if err := json.Unmarshal(data, &list); err != nil {
			t.Fatalf("the escaped JSON is not JSON: %v", err)
		}
		for _, f := range list.Facts {
			for _, q := range f.Quoted {
				if strings.Contains(q.Text, "opens \x1b]8") && strings.Contains(q.Text, "\u202eevil") {
					return
				}
			}
		}
		t.Fatalf("the JSON does not give the reason as the declaration states it: %s", data)
	})

	t.Run("ST-GEN-UC16-R02-T03 The limits of a model with no role, whose stop text holds markup, control and bidirectional characters, are printed escaped in the text and given unchanged in the JSON", func(t *testing.T) {
		text, list, data := factsOf(t, archProject(t, hostileStopsModel(t)), "access")
		for _, bad := range []string{"\x1b", "\u202e", "\u2066"} {
			if strings.Contains(text, bad) || strings.Contains(string(data), bad) {
				t.Fatalf("the output holds the character %q:\n%s", bad, text)
			}
		}
		line := "reached a role before a limit stopped it (" + nameText(hostileStops) + "), so the model names no role that holds it"
		if !strings.Contains(text, line) || !strings.Contains(text, `\u001b[31m`) || !strings.Contains(text, `\u202eevil\u2066`) {
			t.Fatalf("the text does not name the hostile limit escaped:\n%s", text)
		}
		n := 0
		for _, f := range list.Facts {
			if f.Name != "checks" || f.Grants == nil {
				continue
			}
			n++
			if len(f.Grants.Limits) != 1 || f.Grants.Limits[0] != hostileStops {
				t.Errorf("the JSON of %s gives the limits %q, not the stop text unchanged", f.About, f.Grants.Limits)
			}
		}
		if n < 3 {
			t.Fatalf("the hostile model proves too little: %d checks facts with grants", n)
		}
	})

	t.Run("ST-GEN-UC16-R03-T01 No list says that the service complies; a claim word of the declaration is only quoted", func(t *testing.T) {
		_, project := claimFixtureProject(t, claimReason)
		seen := false
		for _, topic := range ComplianceTopics {
			text, list, data := factsOf(t, project, topic)
			own := quotedText.ReplaceAllString(text, "")
			if word := testClaimWords.FindString(own); word != "" {
				t.Fatalf("the text of %s says %q outside a quotation:\n%s", topic, word, text)
			}
			for _, match := range quotedText.FindAllStringIndex(text, -1) {
				before, after := text[:match[0]], text[match[1]:]
				if !strings.HasSuffix(before, ": ") || !strings.Contains(before[strings.LastIndex(before, "\n")+1:], "quoted from ") {
					if !strings.HasPrefix(after, " (quoted)") {
						t.Fatalf("the quotation %s of %s is not marked as quoted:\n%s", text[match[0]:match[1]], topic, text)
					}
				}
			}
			if strings.Contains(text, `"`+claimReason+`"`) {
				seen = true
			}
			// In the JSON, a claim word may stand only in a quoted text of the declaration.
			for _, f := range list.Facts {
				f.Quoted = nil
				own, err := json.Marshal(f)
				if err != nil {
					t.Fatal(err)
				}
				if word := testClaimWords.FindString(string(own)); word != "" {
					t.Fatalf("the JSON of %s says %q outside a quoted text: %s", topic, word, own)
				}
			}
			keys := regexp.MustCompile(`"([a-z_]+)":`).FindAllStringSubmatch(string(data), -1)
			for _, key := range keys {
				if testClaimWords.MatchString(key[1]) {
					t.Fatalf("the JSON key %s claims compliance", key[1])
				}
			}
		}
		if !seen {
			t.Fatalf("no list quotes the reason of the declaration")
		}
		if got := textValue("passes-check"); got != `"passes-check" (quoted)` {
			t.Fatalf("a value with a claim word is shown as %s", got)
		}
	})

	t.Run("ST-GEN-UC16-R04-T01 A default is marked with the rule that sets it, and an unstated fact is said to be unstated with no value", func(t *testing.T) {
		_, project := claimFixtureProject(t, "a new doc has no resource before it exists")
		text, list, _ := factsOf(t, project, "audit")
		if !strings.Contains(text, "  anchor: yes\n    default, set by the rule ST-DECL-UC05-R01\n") {
			t.Fatalf("the default audit.anchor is not marked with its rule:\n%s", text)
		}
		defaults := 0
		for _, f := range list.Facts {
			if f.State == "default" {
				defaults++
				if f.DefaultRule == "" || f.DefaultRule != f.Source.Default {
					t.Fatalf("the default %s names the rule %q", f.ID, f.DefaultRule)
				}
			} else if f.DefaultRule != "" {
				t.Fatalf("the fact %s, %s, names a default rule", f.ID, f.State)
			}
		}
		if defaults == 0 {
			t.Fatalf("the audit topic has no default: %+v", list.Facts)
		}
		text, list, _ = factsOf(t, project, "data-protection")
		for _, name := range []string{"encryption at rest", "encryption in transit"} {
			if !strings.Contains(text, "  "+name+": unstated. The inputs do not state it.\n    source: the rule ST-GEN-UC16-R04 of STEGO\n") {
				t.Fatalf("%s is not said to be unstated:\n%s", name, text)
			}
		}
		for _, f := range list.Facts {
			if f.State != "unstated" || f.Value != nil {
				t.Fatalf("the fact %s infers a value: %+v", f.ID, f)
			}
		}
	})

	t.Run("ST-GEN-UC16-R05-T01 The same model gives the same list, with or without hand-written notes or a home folder, and the JSON keys keep their order", func(t *testing.T) {
		_, project := claimFixtureProject(t, "a new doc has no resource before it exists")
		data, err := os.ReadFile(filepath.Join(project, ArchitectureFile))
		if err != nil {
			t.Fatal(err)
		}
		other := archProject(t, data)
		for name, text := range map[string]string{"README.md": "The service stores cards in a vault.", "docs/architecture.md": "Every column is encrypted at rest."} {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(other, name)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(other, name), []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		t.Setenv("HOME", t.TempDir())
		for _, topic := range ComplianceTopics {
			for _, structured := range []bool{false, true} {
				first, err := ListComplianceFacts(project, topic, structured)
				if err != nil {
					t.Fatal(err)
				}
				second, _ := ListComplianceFacts(project, topic, structured)
				elsewhere, _ := ListComplianceFacts(other, topic, structured)
				if !bytes.Equal(first, second) || !bytes.Equal(first, elsewhere) {
					t.Fatalf("the list of %s (JSON %v) differs between two reads of the same model", topic, structured)
				}
				keys := "{\n  \"topic\": \"" + topic + "\",\n  \"service\": \"docs\",\n  \"model\": \"architecture/architecture.json\",\n  \"record\": \".stego/state.yaml\",\n"
				if topic == "access" {
					keys += "  \"scope\": \"" + accessScope + "\",\n"
				}
				if structured && !bytes.HasPrefix(first, []byte(keys+"  \"facts\": [")) {
					t.Fatalf("the JSON keys are not in their order:\n%s", first)
				}
				if strings.Contains(string(first), "encrypted") || strings.Contains(string(first), "vault") {
					t.Fatalf("the list takes text from a hand-written note:\n%s", first)
				}
			}
		}
	})

	t.Run("ST-GEN-UC16-R04-T02 The access list states its limits: it covers only the declared operations, a permission with no known grant paths is said to be unknown, and a path that reaches no role says so", func(t *testing.T) {
		model, project := claimFixtureProject(t, "a new doc has no resource before it exists")
		text, list, _ := factsOf(t, project, "access")
		if list.Scope != accessScope || !strings.Contains(text, "\nThe list covers only the operations, commands, queries and workers that the declaration states.") {
			t.Fatalf("the access list does not state what it covers:\n%s", text)
		}
		if strings.Contains(text, "granted to no role") {
			t.Fatalf("a path with no role reads as a grant:\n%s", text)
		}
		stopped := false
		for _, p := range model.Access.Permissions {
			for _, g := range p.Paths {
				stopped = stopped || g.Role == ""
			}
		}
		if stopped && !strings.Contains(text, "    a path that reaches no role, by a path of kind ") {
			t.Fatalf("a path that reaches no role is not said so:\n%s", text)
		}
		// A check that names a permission of which the model holds no grant path.
		for i := range model.Access.Checks {
			if model.Access.Checks[i].Subject == "operation:docs.read" {
				model.Access.Checks[i].Grants = ""
			}
		}
		data, err := json.MarshalIndent(model, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		text, list, _ = factsOf(t, archProject(t, append(data, '\n')), "access")
		unknown := false
		for _, f := range list.Facts {
			if f.About == "operation:docs.read" && f.Name == "checks" {
				unknown = f.GrantsUnknown && f.Grants == nil
			}
		}
		if !unknown || !strings.Contains(text, "    the grants of doc#read are not known: the model holds no grant path of it, so they are incomplete, and a role may hold it\n") {
			t.Fatalf("a check with no known grants is not said to be unknown:\n%s", text)
		}
	})
}
