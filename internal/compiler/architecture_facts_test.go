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

	t.Run("ST-GEN-UC16-R01-T04 The access list gives each ancestor check of a nested operation, with its permission and who it grants, or the rule that gives it none", func(t *testing.T) {
		model, data := deriveFixture(t, nestedFixture(t))
		text, list, _ := factsOf(t, archProject(t, data), "access")
		facts := map[string]FactOfList{}
		for _, f := range list.Facts {
			facts[f.About+"|"+f.Name] = f
		}
		folder, ok := facts["operation:folder-docs.update|checks ancestor 2 Folder"]
		permission := pathsOf(model, "permission:folder#view")
		if !ok || folder.Value != "folder#view" || folder.State != "stated" || folder.Source.Declaration != "entities[1].permissions.read" ||
			folder.Grants == nil || folder.Grants.Permission != "permission:folder#view" || len(folder.Grants.Paths) != len(permission) || len(permission) == 0 {
			t.Fatalf("the ancestor check of the folder is %+v", folder)
		}
		shelf, ok := facts["operation:folder-docs.update|checks ancestor 1 Shelf"]
		if !ok || shelf.Value != "none" || shelf.Source.Rule != "ST-SUP-RC20-OC24" || shelf.Grants != nil || len(shelf.Quoted) != 0 {
			t.Fatalf("the ancestor check of the shelf is %+v", shelf)
		}
		if _, ok := facts["operation:shelf-folders.read|checks ancestor 1 Shelf"]; !ok {
			t.Fatal("the list holds no ancestor check of shelf-folders.read")
		}
		for _, f := range list.Facts {
			if f.About == "operation:docs.read" && strings.HasPrefix(f.Name, "checks ancestor") {
				t.Fatalf("an operation with no scope has an ancestor check: %+v", f)
			}
		}
		// The text gives the shelf before the folder, the order the handler reads them, and who folder#view grants.
		at := strings.Index(text, "operation:folder-docs.update\n")
		if at < 0 {
			t.Fatalf("the text names no operation:folder-docs.update:\n%s", text)
		}
		block := text[at:]
		if s, f := strings.Index(block, "checks ancestor 1 Shelf: none"), strings.Index(block, "checks ancestor 2 Folder: folder#view"); s < 0 || f < s {
			t.Fatalf("the text does not give the shelf, then the folder:\n%s", block)
		}
		for _, p := range folder.Grants.Paths {
			if p.Role != "" && !strings.Contains(block, "granted to "+p.Role) {
				t.Fatalf("the text does not name the role %s of the folder check:\n%s", p.Role, block)
			}
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
		want := []FactAncestor{{Fact: "checks ancestor 2 Folder", Permission: "permission:folder#view"}}
		for _, op := range []string{"operation:folder-docs.update", "operation:folder-docs.read"} {
			if got := facts[op+"|checks"].AncestorsIncomplete; !slices.Equal(got, want) {
				t.Errorf("the checks fact of %s names the incomplete ancestor reads %+v, not %+v", op, got, want)
			}
		}
		for _, op := range []string{"operation:docs.read", "operation:shelf-folders.read"} {
			if got := facts[op+"|checks"].AncestorsIncomplete; len(got) != 0 {
				t.Errorf("the checks fact of %s, which reads no ancestor with a permission, names %+v", op, got)
			}
		}
		if !bytes.Contains(out, []byte(`"ancestors_incomplete"`)) {
			t.Error("the JSON list does not name the incomplete ancestor read")
		}
		at := strings.Index(text, "operation:folder-docs.update\n")
		if at < 0 {
			t.Fatalf("the text names no operation:folder-docs.update:\n%s", text)
		}
		block := text[at:]
		checks := strings.Index(block, "  checks: ")
		line := strings.Index(block, "a limit cut a path of the ancestor read permission:folder#view short (checks ancestor 2 Folder), so it can grant more roles than its paths name")
		if checks < 0 || line < checks || strings.Index(block, "checks ancestor 1 Shelf") < line {
			t.Fatalf("the text does not mark the incomplete ancestor read at the checks fact of folder-docs.update:\n%s", block)
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
		if !unknown || !strings.Contains(text, "    the grants of doc#read are not known: the model holds no grant path of it\n") {
			t.Fatalf("a check with no known grants is not said to be unknown:\n%s", text)
		}
	})
}
