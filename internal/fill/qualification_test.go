package fill

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/types"
)

func TestIsQualified(t *testing.T) {
	t.Run("ST-EXT-SP01-T A fill is qualified only when its evidence names both current digests and a listed key signed that evidence", func(t *testing.T) {
		k := makeTestKeys(t)
		signers := mustSigners(t, k.signers)
		fillDigest, contractDigest := strings.Repeat("1", 64), strings.Repeat("2", 64)
		signed := func(change func(*Fill), key string) *Fill {
			f := unsignedFill()
			change(f)
			digest := mustEvidenceDigest(t, f)
			f.SignOff = &FillSignOff{EvidenceDigest: digest, Signature: Signature{KeyType: KeySSH, Signer: "listed@example.com", Armored: k.sshSign(t, key, digest)}, SignedAt: time.Now()}
			return f
		}
		same := func(*Fill) {}
		if f := signed(same, k.listedSSH); !IsQualified(f, fillDigest, contractDigest, signers) {
			t.Fatal("a fill with current evidence and a sign-off by listed-ssh is not qualified")
		}
		otherEvidence := signed(same, k.listedSSH)
		earlier := unsignedFill()
		earlier.Evidence.PassedAt = earlier.Evidence.PassedAt.Add(-time.Hour)
		earlierDigest := mustEvidenceDigest(t, earlier)
		otherEvidence.SignOff.EvidenceDigest = earlierDigest
		otherEvidence.SignOff.Signature.Armored = k.sshSign(t, k.listedSSH, earlierDigest)
		noSignOff := signed(same, k.listedSSH)
		noSignOff.SignOff = nil
		noEvidence := signed(same, k.listedSSH)
		noEvidence.Evidence = nil
		for name, f := range map[string]*Fill{
			"an earlier fill digest":       signed(func(f *Fill) { f.Evidence.FillDigest = strings.Repeat("9", 64) }, k.listedSSH),
			"an earlier contract digest":   signed(func(f *Fill) { f.Evidence.ContractDigest = strings.Repeat("9", 64) }, k.listedSSH),
			"no evidence":                  noEvidence,
			"evidence and no sign-off":     noSignOff,
			"a sign-off by unlisted-ssh":   signed(same, k.unlistedSSH),
			"a sign-off of other evidence": otherEvidence,
		} {
			if IsQualified(f, fillDigest, contractDigest, signers) {
				t.Errorf("a fill with %s is qualified", name)
			}
		}
	})
}

func TestStandingValues(t *testing.T) {
	t.Run("ST-VO45-C01-T A qualification standing is exactly one of qualified, stale, unsigned or untested", func(t *testing.T) {
		for _, text := range []string{"qualified", "stale", "unsigned", "untested"} {
			if standing, err := ParseStanding(text); err != nil || string(standing) != text {
				t.Errorf("%q: refused %v", text, err)
			}
		}
		for _, text := range []string{"Qualified", "qualified ", "signed", "expired", ""} {
			if standing, err := ParseStanding(text); err == nil {
				t.Errorf("%q: accepted as %q", text, standing)
			}
		}
	})
}

// builtInCheck is the fill check of a declaration with the collection
// org-users only, with the slots of rest-api, the one component of the
// built-in registry's rest-crud set that declares slots, as values in memory.
func builtInCheck() compiler.FillCheck {
	check := compiler.FillCheck{Collections: []string{"org-users"}}
	for _, slot := range []string{"before_create", "before_upsert", "before_patch", "before_delete", "after_create", "after_upsert", "after_patch", "after_delete", "validate"} {
		check.Slots = append(check.Slots, compiler.FillSlot{Ref: types.SlotRef{Component: "rest-api", Slot: slot}, Contract: "components/rest-api/slots/" + slot + ".proto"})
	}
	return check
}

func TestFillManifestReader(t *testing.T) {
	t.Run("ST-EXT-INV01-T A fill manifest that does not name exactly one known slot and one declared collection is refused, naming the field", func(t *testing.T) {
		check := builtInCheck()
		check.Slots = append(check.Slots, compiler.FillSlot{Ref: types.SlotRef{Component: "command", Slot: "command"}, Contract: "components/command/component.yaml"})
		manifests := []struct {
			name, body, field, value string
		}{
			{"first-fill", "implements: rest-api.before_create\ncollection: org-users\n", "", ""},
			{"second-fill", "collection: org-users\n", "implements", ""},
			{"third-fill", "implements: [rest-api.before_create, rest-api.before_delete]\ncollection: org-users\n", "implements", ""},
			{"fourth-fill", "implements: rest-api.no_such_slot\ncollection: org-users\n", "implements", "rest-api.no_such_slot"},
			{"fifth-fill", "implements: no-such-component.before_create\ncollection: org-users\n", "implements", "no-such-component.before_create"},
			{"sixth-fill", "implements: rest-api.before_create\ncollection: org-groups\n", "collection", "org-groups"},
			{"seventh-fill", "implements: rest-api.before_create\n", "collection", ""},
			// A fill of the command slot names no collection: its binding is the command that names it (decision
			// on ST-EXT-Q30).
			{"command-fill", "implements: command.command\n", "", ""},
			{"collection-command-fill", "implements: command.command\ncollection: org-users\n", "collection", "org-users"},
		}
		check.ProjectDir = t.TempDir()
		for _, m := range manifests {
			dir := filepath.Join(check.ProjectDir, "fills", m.name)
			if err := os.MkdirAll(dir, 0755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, ManifestFile)
			if err := os.WriteFile(path, []byte("kind: fill\nname: "+m.name+"\n"+m.body), 0644); err != nil {
				t.Fatal(err)
			}
			f, err := ReadManifest(check, m.name)
			if m.name == "command-fill" {
				if err != nil || f == nil || f.Implements != "command.command" || f.Collection != "" {
					t.Fatalf("%s: refused or misread: %+v, %v", m.name, f, err)
				}
				continue
			}
			if m.field == "" {
				if err != nil || f == nil || f.Implements != "rest-api.before_create" || f.Collection != "org-users" {
					t.Fatalf("%s: refused or misread: %+v, %v", m.name, f, err)
				}
				continue
			}
			if f != nil || err == nil {
				t.Fatalf("%s: accepted: %+v", m.name, f)
			}
			for _, part := range []string{m.name, m.field + ":", m.value} {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("%s: the fault %q does not name %q", m.name, err, part)
				}
			}
		}
	})
}

func TestReadManifestStaysInFills(t *testing.T) {
	t.Run("ST-EXT-UC01-R02-T05 The fill manifest reader refuses a name outside the fill-name form and a link out of the fills folder, and reads no file outside it", func(t *testing.T) {
		// Security review of PR #518, finding 1: ReadManifest read <project>/fills/<name>/fill.yaml for any name, so
		// the name ../../outside read and quoted a manifest outside the project.
		check := builtInCheck()
		parent := t.TempDir()
		check.ProjectDir = filepath.Join(parent, "project")
		outside := filepath.Join(parent, "outside")
		secret := "kind: fill\nname: outside\nimplements: rest-api.before_create\ncollection: secret-collection\n"
		for _, dir := range []string{outside, filepath.Join(check.ProjectDir, "fills", "real")} {
			if err := os.MkdirAll(dir, 0755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(outside, ManifestFile), []byte(secret), 0644); err != nil {
			t.Fatal(err)
		}
		// fills/linked is a link to the folder outside, and fills/real/fill.yaml is a link to the manifest outside.
		if err := os.Symlink(outside, filepath.Join(check.ProjectDir, "fills", "linked")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(outside, ManifestFile), filepath.Join(check.ProjectDir, "fills", "real", ManifestFile)); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"../../outside", "../outside", "x/../../../outside", "..", ".", "", "Outside", "a\\..\\outside"} {
			f, err := ReadManifest(check, name)
			if err == nil || f != nil {
				t.Fatalf("the name %q was read: %+v", name, f)
			}
			if strings.Contains(err.Error(), "secret-collection") || !strings.Contains(err.Error(), "fill-name form") {
				t.Errorf("the name %q gave the fault %q, want the fill-name form and no quote of the manifest outside", name, err)
			}
		}
		for _, name := range []string{"linked", "real"} {
			f, err := ReadManifest(check, name)
			if err == nil || f != nil {
				t.Fatalf("the link of %q was read through: %+v", name, f)
			}
			if strings.Contains(err.Error(), "secret-collection") || !strings.Contains(err.Error(), "escapes") {
				t.Errorf("the link of %q gave the fault %q, want a refused link and no quote of the manifest outside", name, err)
			}
		}
		// The catalog and the revision check read through the same root.
		entries, err := projectCatalog(check, types.SlotRef{Component: "rest-api", Slot: "before_create"})
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.Contains(fmt.Sprint(entry), "secret-collection") || entry.Unreadable == "" {
				t.Errorf("the catalog read the manifest outside through %q: %+v", entry.Fill, entry)
			}
		}
		faults, err := RevisionFaults(check.ProjectDir, []string{"linked", "real"}, compiler.FillCheck{})
		if err != nil {
			t.Fatal(err)
		}
		if len(faults) != 2 || strings.Contains(strings.Join(faults, "\n"), "secret-collection") {
			t.Errorf("the revision check gave %q, want a fault for each link and no quote of the manifest outside", faults)
		}
	})
}

func TestFitsBinding(t *testing.T) {
	t.Run("ST-EXT-SP02-T A fill fits a binding only when it implements the bound slot and serves the bound collection", func(t *testing.T) {
		check := builtInCheck()
		check.Slots = append(check.Slots, compiler.FillSlot{Ref: types.SlotRef{Component: "other-api", Slot: "before_create"}})
		binding := types.SlotRef{Component: "rest-api", Slot: "before_create"}
		fill := func(implements, collection string) *Fill {
			return &Fill{Kind: "fill", Name: "a-fill", Implements: implements, Collection: collection}
		}
		if !FitsBinding(fill("rest-api.before_create", "org-users"), binding, "org-users") {
			t.Fatal("the fill of the bound slot and collection does not fit")
		}
		for _, f := range []*Fill{
			fill("rest-api.before_delete", "org-users"),
			fill("rest-api.before_create", "org-groups"),
			fill("other-api.before_create", "org-users"),
		} {
			if FitsBinding(f, binding, "org-users") {
				t.Errorf("%s for %s fits the binding of rest-api.before_create for org-users", f.Implements, f.Collection)
			}
		}
	})
}

func TestReadTestReport(t *testing.T) {
	t.Run("ST-EXT-UC02-R01-T04 A go test report in which no test passed, or a test failed, records no evidence", func(t *testing.T) {
		event := func(action, test, output string) string {
			return fmt.Sprintf("{\"Action\":%q,\"Test\":%q,\"Output\":%q}\n", action, test, output)
		}
		for name, c := range map[string]struct {
			report string
			pass   bool
		}{
			"one test passed":      {event("run", "TestA", "") + event("pass", "TestA", "") + event("output", "", "ok  \tx\n") + event("pass", "", ""), true},
			"no test to run":       {event("output", "", "ok  \tx\t[no tests to run]\n") + event("pass", "", ""), false},
			"a test failed":        {event("pass", "TestA", "") + event("fail", "TestB", "") + event("fail", "", ""), false},
			"a report not in JSON": {"FAIL x [build failed]\n", false},
		} {
			var printed strings.Builder
			err := ReadTestReport(strings.NewReader(c.report), &printed)
			if (err == nil) != c.pass {
				t.Errorf("%s: got %v, want pass %v", name, err, c.pass)
			}
		}
	})
}
