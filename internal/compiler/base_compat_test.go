package compiler

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/declaration"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/parser"
	"github.com/jsell-rh/stego/internal/registry"
	"github.com/jsell-rh/stego/internal/types"
)

// userManagementBase reads the declaration of ST-GEN-TD01, examples/user-management, and a record that names it.
func userManagementBase(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "examples", "user-management", "service.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func recordFor(declaration []byte) []byte {
	return []byte("format_version: 5\nlast_applied:\n  service_hash: " + HashBytes(declaration) + "\n")
}

func parseHead(t *testing.T, data string) *types.ServiceDeclaration {
	t.Helper()
	head, err := parser.ParseServiceDeclarationFromBytes([]byte(data), "service.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return head
}

// withChanges appends schema changes and schema history to a declaration.
func withChanges(declaration, changes, history string) string {
	return strings.TrimRight(declaration, "\n") + "\nschema_changes:\n" + changes + "schema_history:\n" + history
}

const (
	emailRename       = "  - {id: user-email-rename, kind: rename, entity: User, field: contact_email, from: email}\n"
	teamAdd           = "  - {id: add-team, kind: add, entity: Team}\n"
	nicknameAdd       = "  - {id: add-user-nickname, kind: add, entity: User, field: nickname}\n"
	emailRenameExpand = "  - {change: user-email-rename, phase: expand}\n"
	teamAddExpand     = "  - {change: add-team, phase: expand}\n"
	nicknameAddExpand = "  - {change: add-user-nickname, phase: expand}\n"
	team              = "  - name: Team\n    resource_type: team\n    permissions: {read: view, list: view}\n    fields:\n      - { name: label, type: string }\n\n"
	nickname          = "      - { name: nickname, type: string, optional: true }\n"
)

func renameEmail(declaration string) string {
	return strings.Replace(declaration, "{ name: email, type: string", "{ name: contact_email, type: string", 1)
}

func addTeam(declaration string) string {
	return strings.Replace(declaration, "\ncollections:\n", "\n"+team+"collections:\n", 1)
}

func addNickname(declaration string) string {
	return strings.Replace(declaration, "      - { name: metadata, type: jsonb, optional: true }\n", "      - { name: metadata, type: jsonb, optional: true }\n"+nickname, 1)
}

func TestBaseCompat(t *testing.T) {
	t.Parallel()
	t.Run("ST-GEN-INV33-T An undeclared change to a base entity is refused, and a declared one that moves one phase is accepted", func(t *testing.T) {
		t.Parallel()
		td01 := string(userManagementBase(t))
		// A base with two finished-or-open schema steps, so that the order and the removal of the base's steps
		// can be tested.
		stepped := withChanges(addNickname(addTeam(td01)), teamAdd+nicknameAdd, teamAddExpand+nicknameAddExpand)
		cases := []struct {
			name, base, head string
			refused          bool
			entity, says     string
		}{
			{"undeclared rename", td01, renameEmail(td01), true, "User", "renames User.email to contact_email"},
			{"declared rename at expand", td01, withChanges(renameEmail(td01), emailRename, emailRenameExpand), false, "", ""},
			{"undeclared new entity", td01, addTeam(td01), true, "Team", "adds the entity Team"},
			{"declared add of the new entity", td01, withChanges(addTeam(td01), teamAdd, teamAddExpand), false, "", ""},
			{"base steps in another order", stepped, withChanges(addNickname(addTeam(td01)), teamAdd+nicknameAdd, nicknameAddExpand+teamAddExpand), true, "Team", "must start with the base's steps in the same order"},
			{"base schema change removed", stepped, withChanges(addNickname(addTeam(td01)), teamAdd, teamAddExpand), true, "User", "removes the schema change add-user-nickname"},
			{"expand and contract in one release", td01, withChanges(renameEmail(td01), emailRename, emailRenameExpand+"  - {change: user-email-rename, phase: contract}\n"), true, "User", "its expand and its contract step in one release"},
		}
		for _, c := range cases {
			base := []byte(c.base)
			check, err := CheckBase(BaseSource{Ref: "main", Commit: "c0ffee", Declaration: base, Record: recordFor(base)}, parseHead(t, c.head))
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if !c.refused {
				if check.HasFaults() {
					t.Errorf("%s: want accepted, got %+v", c.name, check.Faults)
				}
				continue
			}
			if !check.HasFaults() {
				t.Errorf("%s: want refused, got no fault", c.name)
				continue
			}
			named := false
			for _, fault := range check.Faults {
				if fault.Rule != BaseCompatRule {
					t.Errorf("%s: fault %+v names the rule %q, want %s", c.name, fault, fault.Rule, BaseCompatRule)
				}
				named = named || (fault.Entity == c.entity && strings.Contains(fault.Message, c.says))
			}
			if !named {
				t.Errorf("%s: no fault names the entity %s and says %q: %+v", c.name, c.entity, c.says, check.Faults)
			}
		}
	})
	t.Run("ST-GEN-INV33-T A within added to a reference of a base entity needs a within step, and a within that the head drops is refused", func(t *testing.T) {
		t.Parallel()
		td01 := string(userManagementBase(t))
		// The base: a User refers to an optional OrgSetting, and both refer to their Organization.
		setting := func(within string) string {
			declaration := strings.Replace(td01, "      org_id: {relation: organization, permission: edit}\n    fields:\n      - { name: email,",
				"      org_id: {relation: organization, permission: edit}\n      setting_id: {relation: setting, permission: edit"+within+"}\n    fields:\n      - { name: email,", 1)
			return strings.Replace(declaration, "      - { name: metadata, type: jsonb, optional: true }\n", "      - { name: metadata, type: jsonb, optional: true }\n      - { name: setting_id, type: ref, to: OrgSetting, optional: true }\n", 1)
		}
		within := "  - {id: user-setting-id-within, kind: within, entity: User, field: setting_id}\n"
		withinExpand := "  - {change: user-setting-id-within, phase: expand}\n"
		cases := []struct {
			name, base, head string
			says             string
			suggest          bool
		}{
			{"within with no step", setting(""), setting(", within: org_id"), "holds the reference User.setting_id within org_id, which no schema step declares", true},
			{"within with its within step", setting(""), withChanges(setting(", within: org_id"), within, withinExpand), "", false},
			{"within: none with no step", setting(""), setting(", within: none"), "", false},
			{"within dropped", withChanges(setting(", within: org_id"), within, withinExpand), withChanges(setting(""), within, withinExpand), "changes the within of the reference User.setting_id from org_id", false},
		}
		for _, c := range cases {
			base := []byte(c.base)
			check, err := CheckBase(BaseSource{Ref: "main", Commit: "c0ffee", Declaration: base, Record: recordFor(base)}, parseHead(t, c.head))
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if c.says == "" {
				if check.HasFaults() {
					t.Errorf("%s: want accepted, got %+v", c.name, check.Faults)
				}
				continue
			}
			found := false
			for _, fault := range check.Faults {
				if fault.Entity != "User" || fault.Field != "setting_id" || !strings.Contains(fault.Message, c.says) {
					continue
				}
				found = true
				if c.suggest && (len(fault.Declare) != 1 || fault.Declare[0].Kind != "within" || fault.Declare[0].Field != "setting_id" || fault.Declare[0].Phase != "expand") {
					t.Errorf("%s: the fault suggests %+v, want the within step of User.setting_id", c.name, fault.Declare)
				}
			}
			if !found {
				t.Errorf("%s: no fault on User.setting_id says %q: %+v", c.name, c.says, check.Faults)
			}
		}
	})
	t.Run("ST-GEN-INV33-T A change to what a step of the base added, required or retyped, or to a unique invariant or an entity's table settings, is refused", func(t *testing.T) {
		t.Parallel()
		td01 := string(userManagementBase(t))
		const metadata = "      - { name: metadata, type: jsonb, optional: true }\n"
		added := withChanges(addNickname(td01), nicknameAdd, nicknameAddExpand)
		withInvariant := strings.TrimRight(td01, "\n") + "\ninvariants:\n  - {name: user-email-org, kind: unique, on: User, by: [email, org_id]}\n"
		retyped := withChanges(strings.Replace(td01, "{ name: display_name, type: string, min_length: 1, max_length: 100 }", "{ name: display_name, type: int64, optional: true }", 1),
			"  - {id: user-display-name-retype, kind: retype, entity: User, field: display_name, from: string, conversion: text_to_int64, inverse_conversion: int64_to_text}\n",
			"  - {change: user-display-name-retype, phase: expand}\n")
		required := withChanges(strings.Replace(td01, metadata, metadata+"      - { name: priority, type: int64 }\n", 1),
			"  - {id: user-priority-require, kind: require, entity: User, field: priority, fill_value: 0}\n",
			"  - {change: user-priority-require, phase: expand}\n")
		teamAdded := withChanges(addTeam(td01), teamAdd, teamAddExpand)
		cases := []struct{ name, base, head, says string }{
			{"retype of an added field", added, strings.Replace(added, nickname, "      - { name: nickname, type: int64, optional: true }\n", 1), "changes the stored shape of User.nickname"},
			{"added field made required with no default", added, strings.Replace(added, nickname, "      - { name: nickname, type: string }\n", 1), "changes the stored shape of User.nickname"},
			{"added field with a narrower max_length", added, strings.Replace(added, nickname, "      - { name: nickname, type: string, optional: true, max_length: 3 }\n", 1), "changes the stored shape of User.nickname"},
			{"added field made unique", added, strings.Replace(added, nickname, "      - { name: nickname, type: string, optional: true, unique: true }\n", 1), "the head makes User.nickname unique, which no schema step declares"},
			{"unique invariant changed under its name", withInvariant, strings.Replace(withInvariant, "by: [email, org_id]", "by: [display_name]", 1), "changes the invariant user-email-org"},
			{"retyped field narrowed and made required", retyped, strings.Replace(retyped, "{ name: display_name, type: int64, optional: true }", "{ name: display_name, type: int64, max: 2, unique: true }", 1), "changes the stored shape of User.display_name"},
			{"required field retyped", required, strings.Replace(required, "{ name: priority, type: int64 }", "{ name: priority, type: string, max_length: 1 }", 1), "changes the stored shape of User.priority"},
			{"field of an added entity retyped", teamAdded, strings.Replace(teamAdded, "{ name: label, type: string }", "{ name: label, type: int64, unique: true }", 1), "changes the stored shape of Team.label"},
			{"declared add of a unique field with a default", td01, withChanges(strings.Replace(td01, metadata, metadata+"      - { name: code, type: string, unique: true, default: x }\n", 1), "  - {id: add-user-code, kind: add, entity: User, field: code}\n", "  - {change: add-user-code, phase: expand}\n"), "both unique and a default"},
			{"entity made versioned", td01, strings.Replace(td01, "  - name: User\n", "  - name: User\n    versioned: true\n", 1), "changes the table settings of User"},
			{"base unique invariant removed", withInvariant, td01, "removes or changes the invariant user-email-org"},
			{"new unique change takes over a base invariant's name", withInvariant, withChanges(strings.Replace(withInvariant, "by: [email, org_id]", "by: [display_name]", 1), "  - {id: user-email-org-unique, kind: unique, entity: User, field: user-email-org}\n", "  - {change: user-email-org-unique, phase: expand}\n"), "names user-email-org, an invariant that the base declares"},
			{"declared add of a field unique when live with a default", td01, withChanges(strings.Replace(td01, metadata, metadata+"      - { name: code, type: string, unique_when_live: true, default: x }\n", 1), "  - {id: add-user-code, kind: add, entity: User, field: code}\n", "  - {change: add-user-code, phase: expand}\n"), "both unique and a default"},
			{"constraints of a base field changed with its type kept", td01, strings.Replace(td01, "{ name: display_name, type: string, min_length: 1, max_length: 100 }", "{ name: display_name, type: string, max_length: 5, unique: true }", 1), "keeps its type"},
			{"entity tenant changed", td01, strings.Replace(td01, "  - name: OrgSetting\n", "  - name: OrgSetting\n    tenant: shared\n", 1), "changes the table settings of OrgSetting"},
		}
		for _, c := range cases {
			if c.head == c.base {
				t.Fatalf("%s: the head is the base", c.name)
			}
			base := []byte(c.base)
			check, err := CheckBase(BaseSource{Ref: "main", Commit: "c0ffee", Declaration: base, Record: recordFor(base)}, parseHead(t, c.head))
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			found := false
			for _, fault := range check.Faults {
				found = found || (fault.Rule == BaseCompatRule && strings.Contains(fault.Message, c.says))
			}
			if !found {
				t.Errorf("%s: no fault says %q: %+v", c.name, c.says, check.Faults)
			}
		}
		for _, c := range []struct{ name, base, head string }{
			{"the base itself, with its add", added, added},
			{"the base with its retype", retyped, retyped},
			{"a new add after the base's add", added, withChanges(addNickname(addTeam(td01)), nicknameAdd+teamAdd, nicknameAddExpand+teamAddExpand)},
		} {
			base := []byte(c.base)
			check, err := CheckBase(BaseSource{Ref: "main", Commit: "c0ffee", Declaration: base, Record: recordFor(base)}, parseHead(t, c.head))
			if err != nil || check.HasFaults() {
				t.Errorf("%s: want accepted, got %v %+v", c.name, err, check.Faults)
			}
		}
	})

	t.Run("ST-GEN-INV33-T A retype of a field with a default keeps the default of its Definition, so the field before the change is the base's", func(t *testing.T) {
		t.Parallel()
		// Walkthrough 6, K2: the Definition of the retype names the default of the field before the change. The
		// check took it away, so the base column "points" integer NOT NULL DEFAULT 0 never matched the head.
		td01 := string(userManagementBase(t))
		const metadata = "      - { name: metadata, type: jsonb, optional: true }\n"
		base := strings.Replace(td01, metadata, metadata+"      - { name: points, type: int32, min: 0, max: 100, default: 0 }\n", 1)
		head := func(definition string) string {
			return withChanges(strings.Replace(base, "{ name: points, type: int32, min: 0, max: 100, default: 0 }", "{ name: points, type: int64, min: 0, max: 100, default: 0 }", 1),
				"  - {id: user-points-retype, kind: retype, entity: User, field: points, from: int32, definition: \""+definition+"\", conversion: integer_to_int64, inverse_conversion: int64_to_integer}\n",
				"  - {change: user-points-retype, phase: expand}\n")
		}
		for _, c := range []struct {
			name, definition string
			accepted         bool
		}{
			{"the Definition names the base's default", "int32, min 0, max 100, default 0", true},
			{"the Definition names another default", "int32, min 0, max 100, default 1", false},
			{"the Definition names no default", "int32, min 0, max 100", false},
		} {
			data := []byte(base)
			check, err := CheckBase(BaseSource{Ref: "main", Commit: "c0ffee", Declaration: data, Record: recordFor(data)}, parseHead(t, head(c.definition)))
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			switch {
			case c.accepted && check.HasFaults():
				t.Errorf("%s: want accepted, got %+v", c.name, check.Faults)
			case !c.accepted && !check.HasFaults():
				t.Errorf("%s: want refused, got no fault", c.name)
			}
		}
		// Walkthrough 14, W14-B3: a retype with no definition is refused with a fault that names the column, its two
		// forms and the cause, and not with the two whole CREATE TABLE texts.
		data := []byte(base)
		noDefinition := strings.Replace(head("x"), `definition: "x", `, "", 1)
		check, err := CheckBase(BaseSource{Ref: "main", Commit: "c0ffee", Declaration: data, Record: recordFor(data)}, parseHead(t, noDefinition))
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, fault := range check.Faults {
			if fault.Rule != BaseCompatRule || !strings.Contains(fault.Message, "the column points is") {
				continue
			}
			found = true
			for _, want := range []string{`"points" integer NOT NULL DEFAULT 0`, "the retype user-points-retype gives no definition", "such as its default"} {
				if !strings.Contains(fault.Message, want) {
					t.Errorf("the fault of a retype with no definition does not say %q: %s", want, fault.Message)
				}
			}
			if strings.Contains(fault.Message, "CREATE TABLE") {
				t.Errorf("the fault of a retype with no definition gives the whole tables: %s", fault.Message)
			}
		}
		if !found {
			t.Errorf("a retype with no definition gives no fault that names the column points: %+v", check.Faults)
		}
	})

	t.Run("ST-GEN-INV33-T A change of the indexes that a collection gives a base entity is refused, and a change that gives no index is accepted", func(t *testing.T) {
		t.Parallel()
		td01 := string(userManagementBase(t))
		for _, c := range []struct{ name, head string }{
			{"list taken from the only collection that lists an entity", strings.Replace(td01, "    operations: [create, read, delete, list]\n", "    operations: [create, read, delete]\n", 1)},
			{"an upsert key changed", strings.Replace(td01, "    upsert_key: [org_id, key]\n", "    upsert_key: [org_id, key, value]\n", 1)},
			{"a searched field of a listed entity marked pii", strings.Replace(td01, "{ name: display_name, type: string, min_length: 1, max_length: 100 }", "{ name: display_name, type: string, min_length: 1, max_length: 100, sensitivity: pii }", 1)},
			{"a list taken away under a rename, which generation does not carry out yet", strings.Replace(withChanges(renameEmail(td01), emailRename, emailRenameExpand), "    operations: [create, read, delete, list]\n", "    operations: [create, read, delete]\n", 1)},
		} {
			if c.head == td01 {
				t.Fatalf("%s: the head is the base", c.name)
			}
			base := []byte(td01)
			check, err := CheckBase(BaseSource{Ref: "main", Commit: "c0ffee", Declaration: base, Record: recordFor(base)}, parseHead(t, c.head))
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			found := false
			for _, fault := range check.Faults {
				found = found || (fault.Rule == BaseCompatRule && strings.Contains(fault.Message, "change its indexes"))
			}
			if !found {
				t.Errorf("%s: no fault says that the collections change the indexes: %+v", c.name, check.Faults)
			}
		}
		renamed := withChanges(strings.ReplaceAll(strings.Replace(td01, "{ name: source_type, type: string, max_length: 50 }", "{ name: origin_type, type: string, max_length: 50 }", 1), "implicit: { source_type:", "implicit: { origin_type:"),
			"  - {id: activityentry-source-type-rename, kind: rename, entity: ActivityEntry, field: origin_type, from: source_type}\n",
			"  - {change: activityentry-source-type-rename, phase: expand}\n")
		for _, c := range []struct{ name, base, head string }{
			{"a permission changed", td01, strings.Replace(td01, "permissions: {create: public, read: view, list: view, delete: edit}", "permissions: {create: public, read: view, list: view, delete: view}", 1)},
			{"a new list with a scope field that a search index has already", td01, strings.Replace(td01, "    operations: [create, read, delete, list]\n", "    operations: [create, read, delete, list]\n\n  all-teams:\n    entity: Organization\n    scope: { name: Organization }\n    operations: [list]\n", 1)},
			{"a new implicit field of a list that a search index has already", td01, strings.Replace(td01, "    implicit: { source_type: \"User\" }\n", "    implicit: { source_type: \"User\", action: \"login\" }\n", 1)},
			{"a field that is not searched marked pii", td01, strings.Replace(td01, "{ name: metadata, type: jsonb, optional: true }", "{ name: metadata, type: jsonb, optional: true, sensitivity: pii }", 1)},
			{"a collection with no list and no upsert key", td01, strings.Replace(td01, "\nslots:", "\n  one-org:\n    entity: Organization\n    path_prefix: /one-org\n    operations: [read]\n\nslots:", 1)},
			{"a rename of an implicit field of a list", td01, renamed},
			// A field written public is a field with no sensitivity: its search index stays (decision ST-DECL-Q46 (a)).
			{"a searched field written public", td01, strings.Replace(td01, "{ name: display_name, type: string, min_length: 1, max_length: 100 }", "{ name: display_name, type: string, min_length: 1, max_length: 100, sensitivity: public }", 1)},
		} {
			base := []byte(c.base)
			check, err := CheckBase(BaseSource{Ref: "main", Commit: "c0ffee", Declaration: base, Record: recordFor(base)}, parseHead(t, c.head))
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			for _, fault := range check.Faults {
				if strings.Contains(fault.Message, "change its indexes") {
					t.Errorf("%s: want no index fault, got %+v", c.name, fault)
				}
			}
		}
	})

	t.Run("ST-GEN-INV33-T A base that writes sensitivity: public on a searched field is refused when the generator of its release built no search index on it", func(t *testing.T) {
		t.Parallel()
		td01 := string(userManagementBase(t))
		written := strings.Replace(td01, "{ name: display_name, type: string, min_length: 1, max_length: 100 }", "{ name: display_name, type: string, min_length: 1, max_length: 100, sensitivity: public }", 1)
		base := []byte(written)
		for _, c := range []struct {
			adapter string
			refused bool
		}{{"6.17.0", true}, {PublicSearchVersion, false}} {
			check, err := CheckBase(BaseSource{Ref: "main", Commit: "c0ffee", Declaration: base, Record: adapterRecord(t, base, c.adapter)}, parseHead(t, written))
			if err != nil {
				t.Fatal(err)
			}
			refused := false
			for _, fault := range check.Faults {
				refused = refused || (fault.Rule == BaseCompatRule && fault.Entity == "User" && strings.Contains(fault.Message, "writes sensitivity: public") &&
					strings.Contains(fault.Message, `"users_display_name_search"`) && strings.Contains(fault.Message, "ST-DECL-Q46"))
			}
			if refused != c.refused {
				t.Errorf("postgres-adapter %s: gives %+v, want refused %t", c.adapter, check.Faults, c.refused)
			}
		}
		// A base that writes no sensitivity: public is not refused for it over an older generator.
		plain := []byte(td01)
		check, err := CheckBase(BaseSource{Ref: "main", Commit: "c0ffee", Declaration: plain, Record: adapterRecord(t, plain, "6.17.0")}, parseHead(t, td01))
		if err != nil || check.HasFaults() {
			t.Fatalf("the same declaration with no field written public is refused: %v %+v", err, check.Faults)
		}
	})

	t.Run("ST-GEN-INV33-T The index check of the collections, where generation cannot compare the indexes, traces renames in history order and counts the fields that a list indexes anyway", func(t *testing.T) {
		t.Parallel()
		// Review of PR #263, L11: two renames of one field whose schema changes are listed in the other order than
		// their steps, and a new implicit field that the search of the list indexes already, were refused.
		td01 := string(userManagementBase(t))
		base := parseHead(t, td01)
		sealed := strings.Replace(td01, "{ name: display_name, type: string, min_length: 1, max_length: 100 }", "{ name: display_name, type: string, min_length: 1, max_length: 100, sensitivity: secret }", 1)
		const displayNameSeal = "  - {id: user-display-name-seal, kind: seal, entity: User, field: display_name, from: public}\n"
		const displayNameSealExpand = "  - {change: user-display-name-seal, phase: expand}\n"
		chained := withChanges(strings.ReplaceAll(strings.Replace(td01, "{ name: source_type, type: string, max_length: 50 }", "{ name: kind_type, type: string, max_length: 50 }", 1), "implicit: { source_type:", "implicit: { kind_type:"),
			"  - {id: activityentry-origin-type-rename, kind: rename, entity: ActivityEntry, field: kind_type, from: origin_type}\n"+
				"  - {id: activityentry-source-type-rename, kind: rename, entity: ActivityEntry, field: origin_type, from: source_type}\n",
			"  - {change: activityentry-source-type-rename, phase: expand}\n  - {change: activityentry-origin-type-rename, phase: expand}\n")
		for _, c := range []struct {
			name, head string
			fault      bool
		}{
			{"two renames of an implicit field, listed in the other order than their steps", chained, false},
			{"a searchable field that an add step adds, whose step migration builds its index", withChanges(addNickname(td01), nicknameAdd, nicknameAddExpand), false},
			{"a new implicit field of a list that the search of the list indexes already", strings.Replace(td01, "    implicit: { source_type: \"User\" }\n", "    implicit: { source_type: \"User\", action: \"login\" }\n", 1), false},
			{"a searched field of a listed entity marked pii", strings.Replace(td01, "{ name: display_name, type: string, min_length: 1, max_length: 100 }", "{ name: display_name, type: string, min_length: 1, max_length: 100, sensitivity: pii }", 1), true},
			{"list taken from the only collection that lists an entity", strings.Replace(td01, "    operations: [create, read, delete, list]\n", "    operations: [create, read, delete]\n", 1), true},
			// Walkthrough 17, W17-B2: a seal from public keeps the search index of the clear column, also beside a
			// rename in the same release; the seal and the rename are each checked on their own.
			{"a seal from public of a searched field, with a rename in the same release", withChanges(renameEmail(sealed), displayNameSeal+emailRename, displayNameSealExpand+emailRenameExpand), false},
			{"a seal with no from of a searched field, with a rename in the same release", withChanges(renameEmail(sealed), strings.Replace(displayNameSeal, ", from: public", "", 1)+emailRename, displayNameSealExpand+emailRenameExpand), false},
			{"a seal from pii of a field that the base searched", withChanges(sealed, strings.Replace(displayNameSeal, "from: public", "from: pii", 1), displayNameSealExpand), true},
		} {
			faults := collectionIndexFaults(base, parseHead(t, c.head))
			if (len(faults) > 0) != c.fault {
				t.Errorf("%s: the faults are %+v, want a fault %v", c.name, faults, c.fault)
			}
		}
	})
	t.Run("ST-GEN-INV33-T The index check of the collections reads a retyped field with its type before the change, and follows a rename chain back and forth only in history order", func(t *testing.T) {
		t.Parallel()
		// Review of PR #314, L14, and the reviewer's red-team cases: the baseline has a retyped field of its type
		// From, so its search index follows that field, with the sensitivity of the head.
		td01 := string(userManagementBase(t))
		pii := func(s, field string) string {
			return strings.Replace(s, "{ name: "+field+", type: string", "{ name: "+field+", type: string, sensitivity: pii", 1)
		}
		there := "  - {id: act-r1, kind: rename, entity: ActivityEntry, field: verb, from: action}\n"
		back := "  - {id: act-r2, kind: rename, entity: ActivityEntry, field: action, from: verb}\n"
		thereStep, backStep := "  - {change: act-r1, phase: expand}\n", "  - {change: act-r2, phase: expand}\n"
		retype := "  - {id: dn-retype, kind: retype, entity: User, field: display_name, from: string}\n"
		retypeStep := "  - {change: dn-retype, phase: expand}\n"
		basePii := pii(td01, "display_name")
		for _, c := range []struct {
			name, base, head string
			fault            bool
		}{
			{"a rename there and back, with no other change", td01, withChanges(td01, there+back, thereStep+backStep), false},
			{"a rename there and back, the field marked pii at the end", td01, withChanges(pii(td01, "action"), there+back, thereStep+backStep), true},
			{"a rename there and back listed in the other order, the field marked pii", td01, withChanges(pii(td01, "action"), back+there, thereStep+backStep), true},
			{"a rename there and back whose steps are in the other order", td01, withChanges(td01, there+back, backStep+thereStep), true},
			{"a retype of a pii field to a type that is not searched, with pii taken away", basePii,
				withChanges(strings.Replace(td01, "{ name: display_name, type: string, min_length: 1, max_length: 100 }", "{ name: display_name, type: jsonb }", 1), retype, retypeStep), true},
			{"a retype of a pii field to a type that is not searched, still pii", basePii,
				withChanges(strings.Replace(basePii, "{ name: display_name, type: string, sensitivity: pii, min_length: 1, max_length: 100 }", "{ name: display_name, type: jsonb, sensitivity: pii }", 1), retype, retypeStep), false},
		} {
			faults := collectionIndexFaults(parseHead(t, c.base), parseHead(t, c.head))
			if (len(faults) > 0) != c.fault {
				t.Errorf("%s: the faults are %+v, want a fault %v", c.name, faults, c.fault)
			}
		}
	})
	t.Run("ST-GEN-INV33-T A new order of the fields of a base entity, which changes 001_initial and the marker, is refused", func(t *testing.T) {
		t.Parallel()
		td01 := string(userManagementBase(t))
		head := strings.Replace(td01, "      - { name: email, type: string, unique: true, max_length: 255 }\n      - { name: display_name, type: string, min_length: 1, max_length: 100 }\n",
			"      - { name: display_name, type: string, min_length: 1, max_length: 100 }\n      - { name: email, type: string, unique: true, max_length: 255 }\n", 1)
		if head == td01 {
			t.Fatal("the head is the base")
		}
		base := []byte(td01)
		check, err := CheckBase(BaseSource{Ref: "main", Commit: "c0ffee", Declaration: base, Record: recordFor(base)}, parseHead(t, head))
		if err != nil {
			t.Fatal(err)
		}
		if len(check.Faults) != 1 || check.Faults[0].Rule != BaseCompatRule || check.Faults[0].Entity != "User" || !strings.Contains(check.Faults[0].Message, "changes the table users of 001_initial") {
			t.Errorf("want one fault of %s on User that names the table users of 001_initial, got %+v", BaseCompatRule, check.Faults)
		}
		// The same order in the base and the head, and a record that names no postgres-adapter, give no fault.
		if check, err := CheckBase(BaseSource{Ref: "main", Commit: "c0ffee", Declaration: base, Record: recordFor(base)}, parseHead(t, td01)); err != nil || check.HasFaults() {
			t.Errorf("the base against itself: %+v, %v", check.Faults, err)
		}
		// A record that names components and no postgres-adapter has no 001_initial to compare, and its indexes are
		// compared as the collections describe them.
		noAdapter := []byte("format_version: 5\nlast_applied:\n  service_hash: " + HashBytes(base) + "\n  components:\n    jwt-auth: {version: 1.0.0}\n")
		if check, err := CheckBase(BaseSource{Ref: "main", Commit: "c0ffee", Declaration: base, Record: noAdapter}, parseHead(t, head)); err != nil || check.HasFaults() {
			t.Errorf("a base with no postgres-adapter: %+v, %v", check.Faults, err)
		}
		unlisted := strings.Replace(td01, "    operations: [create, read, delete, list]\n", "    operations: [create, read, delete]\n", 1)
		check, err = CheckBase(BaseSource{Ref: "main", Commit: "c0ffee", Declaration: base, Record: noAdapter}, parseHead(t, unlisted))
		if err != nil || len(check.Faults) != 1 || !strings.Contains(check.Faults[0].Message, "change its indexes") {
			t.Errorf("a list taken away on a base with no postgres-adapter: want one index fault, got %+v, %v", check.Faults, err)
		}
	})

	t.Run("ST-GEN-INV33-T A head that adds pooled tenancy to a base with no tenancy, which adds the tenant column to 001_initial, is refused", func(t *testing.T) {
		t.Parallel()
		td01 := string(userManagementBase(t))
		single := strings.Replace(td01, "tenancy:\n  kind: pooled\n  key: tenant_id\n  claim: tenant\n  provisioning: {resource_type: service, provision: provision, retire: retire}\n", "", 1)
		if single == td01 {
			t.Fatal("the base still has its tenancy")
		}
		base := []byte(single)
		adapterOnly := []byte("format_version: 5\nlast_applied:\n  service_hash: " + HashBytes(base) + "\n  components:\n    postgres-adapter: {version: 6.2.0}\n")
		for name, record := range map[string][]byte{"a record that names postgres-adapter and no tenancy": adapterOnly, "a record that names no component": recordFor(base)} {
			check, err := CheckBase(BaseSource{Ref: "main", Commit: "c0ffee", Declaration: base, Record: record}, parseHead(t, td01))
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			found := false
			for _, fault := range check.Faults {
				found = found || (fault.Rule == BaseCompatRule && strings.Contains(fault.Message, "of 001_initial with no schema step"))
			}
			if !found {
				t.Errorf("%s: the head that adds pooled tenancy gives no fault on 001_initial: %+v", name, check.Faults)
			}
		}
		// The settings alone select the tenancy component: no archetype or mixin of the registry holds it. The
		// example content of an archetype is a declaration, which gives the settings, so it is not read here.
		for _, folder := range []string{"archetypes", "mixins"} {
			err := filepath.WalkDir(filepath.Join("..", "..", "registry", folder), func(path string, entry fs.DirEntry, err error) error {
				if err == nil && entry.IsDir() && entry.Name() == "example" {
					return fs.SkipDir
				}
				// A Markdown guide of an archetype is a user doc, not registry data: it shows the tenancy settings.
				if err != nil || entry.IsDir() || strings.HasSuffix(path, ".md") {
					return err
				}
				data, err := os.ReadFile(path)
				if err == nil && strings.Contains(string(data), gen.TenancyComponent) {
					t.Errorf("%s names the component %s, so the settings alone do not select it", path, gen.TenancyComponent)
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	})
}

// relaxUser gives the declaration with the field line of User changed, and with the relax and its expand step when
// relax is not empty.
func relaxUser(t *testing.T, declaration, from, to, relax string) string {
	t.Helper()
	if !strings.Contains(declaration, from) {
		t.Fatalf("the declaration has no %q", from)
	}
	head := strings.Replace(declaration, from, to, 1)
	if relax == "" {
		return head
	}
	id := strings.SplitN(strings.TrimPrefix(relax, "{id: "), ",", 2)[0]
	return withChanges(head, "  - "+relax+"\n", "  - {change: "+id+", phase: expand}\n")
}

const (
	displayName = "{ name: display_name, type: string, min_length: 1, max_length: 100 }"
	roleField   = "{ name: role, type: enum, values: [admin, member, viewer] }"
	emailField  = "{ name: email, type: string, unique: true, max_length: 255 }"
)

func TestBaseRelax(t *testing.T) {
	t.Run("ST-GEN-INV33-T A relax of a field of the base is accepted, a narrowing is refused, and an undeclared relax names the relax to declare", func(t *testing.T) {
		t.Parallel()
		td01 := string(userManagementBase(t))
		cases := []struct {
			name, head string
			refused    bool
			says       string
		}{
			{"a longer max_length", relaxUser(t, td01, displayName, "{ name: display_name, type: string, min_length: 1, max_length: 200 }",
				"{id: user-display-name-relax, kind: relax, entity: User, field: display_name, definition: max_length 100}"), false, ""},
			{"a new enum value", relaxUser(t, td01, roleField, "{ name: role, type: enum, values: [admin, member, viewer, owner] }",
				"{id: user-role-relax, kind: relax, entity: User, field: role, definition: values admin member viewer}"), false, ""},
			{"a new default", relaxUser(t, td01, roleField, "{ name: role, type: enum, values: [admin, member, viewer], default: member }",
				"{id: user-role-relax, kind: relax, entity: User, field: role, definition: no default}"), false, ""},
			{"a required field made optional", relaxUser(t, td01, emailField, "{ name: email, type: string, unique: true, max_length: 255, optional: true }",
				"{id: user-email-relax, kind: relax, entity: User, field: email, definition: required}"), false, ""},
			{"an enum made a string in place", relaxUser(t, td01, roleField, "{ name: role, type: string }",
				"{id: user-role-relax, kind: relax, entity: User, field: role, definition: \"type enum, values admin member viewer\"}"), false, ""},
			{"two relaxes of role, the later one an enum made a string", withChanges(strings.Replace(td01, roleField, "{ name: role, type: string }", 1),
				"  - {id: user-role-relax, kind: relax, entity: User, field: role, definition: values admin member viewer}\n  - {id: user-role-relax-2, kind: relax, entity: User, field: role, definition: \"type enum, values admin member viewer owner\"}\n",
				"  - {change: user-role-relax, phase: expand}\n  - {change: user-role-relax-2, phase: expand}\n"), false, ""},
			// Each narrowing, declared as a relax, is refused.
			{"a shorter max_length", relaxUser(t, td01, displayName, "{ name: display_name, type: string, min_length: 1, max_length: 50 }",
				"{id: user-display-name-relax, kind: relax, entity: User, field: display_name, definition: max_length 100}"), true, "only makes a max_length longer"},
			{"an enum value taken away", relaxUser(t, td01, roleField, "{ name: role, type: enum, values: [admin, member] }",
				"{id: user-role-relax, kind: relax, entity: User, field: role, definition: values admin member viewer}"), true, "only adds enum values"},
			{"an optional field made required", relaxUser(t, td01, "{ name: metadata, type: jsonb, optional: true }", "{ name: metadata, type: jsonb }",
				"{id: user-metadata-relax, kind: relax, entity: User, field: metadata, definition: required}"), true, "needs a require"},
			{"a retype that needs a rewrite", relaxUser(t, td01, roleField, "{ name: role, type: string, max_length: 20 }",
				"{id: user-role-relax, kind: relax, entity: User, field: role, definition: \"type enum, values admin member viewer\"}"), true, "a string with no max_length"},
			{"a relax that also changes what it does not name", relaxUser(t, td01, displayName, "{ name: display_name, type: string, min_length: 2, max_length: 200 }",
				"{id: user-display-name-relax, kind: relax, entity: User, field: display_name, definition: max_length 100}"), true, "User.display_name"},
			// With no schema change, a narrowing is refused with no relax to declare, and a relax names one.
			{"a relax of the same max_length", relaxUser(t, td01, displayName, displayName,
				"{id: user-display-name-relax, kind: relax, entity: User, field: display_name, definition: max_length 100}"), true, "only makes a max_length longer"},
			{"an undeclared shorter max_length", relaxUser(t, td01, displayName, "{ name: display_name, type: string, min_length: 1, max_length: 50 }", ""), true, "a relax only makes a max_length longer"},
			{"an undeclared optional field made required", relaxUser(t, td01, "{ name: metadata, type: jsonb, optional: true }", "{ name: metadata, type: jsonb }", ""), true, "keeps its type"},
		}
		for _, c := range cases {
			base := []byte(td01)
			check, err := CheckBase(BaseSource{Ref: "main", Commit: "c0ffee", Declaration: base, Record: recordFor(base)}, parseHead(t, c.head))
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			var messages []string
			for _, fault := range check.Faults {
				messages = append(messages, fault.Rule+": "+fault.Message)
				for _, change := range fault.Declare {
					if change.Kind == "relax" {
						t.Errorf("%s: the fault suggests the relax %s, which does not relax the field", c.name, change.ID)
					}
				}
			}
			switch {
			case !c.refused && check.HasFaults():
				t.Errorf("%s: refused: %v", c.name, messages)
			case c.refused && !strings.Contains(strings.Join(messages, "\n"), c.says):
				t.Errorf("%s: the faults %v do not say %q", c.name, messages, c.says)
			}
		}
		// A default changed or taken away, a min_length changed and a shorter max_length are no relax.
		n := func(v int) *int { return &v }
		for _, c := range []struct {
			name          string
			before, after fieldShape
		}{
			{"a default changed", fieldShape{Type: types.FieldTypeString, Default: "viewer"}, fieldShape{Type: types.FieldTypeString, Default: "member"}},
			{"a default taken away", fieldShape{Type: types.FieldTypeString, Default: "viewer"}, fieldShape{Type: types.FieldTypeString}},
			{"a shorter max_length", fieldShape{Type: types.FieldTypeString, MaxLength: n(9)}, fieldShape{Type: types.FieldTypeString, MaxLength: n(5)}},
			{"a min_length changed", fieldShape{Type: types.FieldTypeString, MinLength: n(2)}, fieldShape{Type: types.FieldTypeString, MinLength: n(1)}},
			{"an enum made a string with a max_length", fieldShape{Type: types.FieldTypeEnum, Values: []string{"a"}}, fieldShape{Type: types.FieldTypeString, MaxLength: n(5)}},
		} {
			if definition, ok := relaxDefinition(c.before, c.after); ok {
				t.Errorf("%s: relaxDefinition gives the relax %q", c.name, definition)
			}
		}
		// An undeclared relax is refused with the relax to declare, whose definition the check then accepts.
		for _, c := range []struct{ from, to, definition string }{
			{displayName, "{ name: display_name, type: string, min_length: 1, max_length: 200 }", "max_length 100"},
			{roleField, "{ name: role, type: enum, values: [admin, member, viewer, owner], default: member }", "values admin member viewer, no default"},
			{emailField, "{ name: email, type: string, unique: true, max_length: 255, optional: true }", "required"},
			{roleField, "{ name: role, type: string }", "type enum, values admin member viewer"},
		} {
			base := []byte(td01)
			check, err := CheckBase(BaseSource{Ref: "main", Commit: "c0ffee", Declaration: base, Record: recordFor(base)}, parseHead(t, relaxUser(t, td01, c.from, c.to, "")))
			if err != nil {
				t.Fatal(err)
			}
			if len(check.Faults) != 1 || len(check.Faults[0].Declare) != 1 || check.Faults[0].Declare[0].Kind != "relax" || check.Faults[0].Declare[0].Needs != "the definition "+c.definition {
				t.Errorf("%s: the faults are %+v, want one that declares a relax with the definition %s", c.to, check.Faults, c.definition)
				continue
			}
			declared := relaxUser(t, td01, c.from, c.to, "{id: "+check.Faults[0].Declare[0].ID+", kind: relax, entity: User, field: "+check.Faults[0].Declare[0].Field+", definition: \""+c.definition+"\"}")
			if again, err := CheckBase(BaseSource{Ref: "main", Commit: "c0ffee", Declaration: base, Record: recordFor(base)}, parseHead(t, declared)); err != nil || again.HasFaults() {
				t.Errorf("%s: the declared relax is refused: %v %+v", c.to, err, again.Faults)
			}
		}
	})

	t.Run("ST-GEN-INV33-T A new relax step on a base whose runtime library refuses the relaxed column is refused, naming the version to deploy first", func(t *testing.T) {
		t.Parallel()
		td01 := string(userManagementBase(t))
		base := parseHead(t, td01)
		head := parseHead(t, relaxUser(t, td01, displayName, "{ name: display_name, type: string, min_length: 1, max_length: 200 }",
			"{id: user-display-name-relax, kind: relax, entity: User, field: display_name, definition: max_length 100}"))
		applied := func(runtime string) *AppliedState {
			state := &AppliedState{Components: map[string]ComponentState{"postgres-adapter": {Version: "6.4.0"}}, Archetype: &ArchetypeState{Version: "3.0.0"}}
			if runtime != "" {
				state.Archetype.Runtime = &registry.RuntimePin{Version: runtime}
			}
			return state
		}
		for _, runtime := range []string{"", "v0.107.0", "v0.95.0"} {
			faults := relaxRuntimeFaults(applied(runtime), base, head)
			if len(faults) != 1 || faults[0].Rule != StepLedgerRule || !strings.Contains(faults[0].Message, RelaxRuntimeVersion) || !strings.Contains(faults[0].Message, "user-display-name-relax") {
				t.Errorf("runtime %q: the faults are %+v, want one of %s that names %s and the change", runtime, faults, StepLedgerRule, RelaxRuntimeVersion)
			}
		}
		for _, runtime := range []string{RelaxRuntimeVersion, "v0.111.0"} {
			if faults := relaxRuntimeFaults(applied(runtime), base, head); len(faults) != 0 {
				t.Errorf("runtime %s: refused: %+v", runtime, faults)
			}
		}
		if faults := relaxRuntimeFaults(&AppliedState{Components: map[string]ComponentState{}}, base, head); len(faults) != 0 {
			t.Errorf("a base with no postgres-adapter: refused: %+v", faults)
		}
		// The check refuses the head when the record at the base pins such a runtime.
		data := []byte(td01)
		record := []byte("format_version: 5\nlast_applied:\n  service_hash: " + HashBytes(data) + "\n  components:\n    postgres-adapter:\n      version: 6.4.0\n  archetype:\n    name: rest-crud\n    version: 3.0.0\n    runtime:\n      version: v0.107.0\n")
		check, err := CheckBase(BaseSource{Ref: "main", Commit: "c0ffee", Declaration: data, Record: record}, head)
		if err != nil {
			t.Fatal(err)
		}
		if len(check.Faults) == 0 || !strings.Contains(check.Faults[len(check.Faults)-1].Message, RelaxRuntimeVersion) {
			t.Errorf("a base record that pins v0.107.0: the faults are %+v, want one that names %s", check.Faults, RelaxRuntimeVersion)
		}
		// A relax of the base is no new step.
		if faults := relaxRuntimeFaults(applied("v0.95.0"), head, head); len(faults) != 0 {
			t.Errorf("a relax that the base has: refused: %+v", faults)
		}
	})
}

// The base check of a relax reads the field at the relax's step (declaration.FieldAtStep); a later retype with no
// field cannot be undone, and the check names it and does not stop (security review of PR #372, M1).
func TestBaseRelaxLaterNoField(t *testing.T) {
	t.Run("ST-GEN-INV33-T A relax and then a retype with no field are refused, naming the retype", func(t *testing.T) {
		max := 500
		head := &types.ServiceDeclaration{
			Entities: []types.Entity{{Name: "Todo", Fields: []types.Field{{Name: "title", Type: types.FieldTypeString, MaxLength: &max}}}},
			SchemaChanges: []types.SchemaChange{{ID: "a", Kind: "relax", Entity: "Todo", Field: "title", Definition: "max_length 255"},
				{ID: "b", Kind: "retype", Entity: "Todo", From: "int64", Conversion: "int64_to_text", InverseConversion: "text_to_int64"}},
			SchemaHistory: []types.SchemaStep{{Change: "a", Phase: "expand"}, {Change: "b", Phase: "expand"}},
		}
		faults := relaxFaults(&types.ServiceDeclaration{}, head)
		if len(faults) != 1 || !strings.Contains(faults[0].Message, `"b"`) || !strings.Contains(faults[0].Message, "names no field") {
			t.Errorf("the check gives %+v, want one fault that names the retype b as one that cannot be undone", faults)
		}
	})
}

func TestBaseDrop(t *testing.T) {
	t.Run("ST-GEN-INV33-T A declared drop of a field or an entity of the base is accepted, and a drop that does not give back the base is refused", func(t *testing.T) {
		t.Parallel()
		td01 := string(userManagementBase(t))
		description := "      - { name: description, type: string, optional: true, max_length: 1000 }\n"
		noDescription := func(change string) string {
			head := strings.Replace(td01, description, "", 1)
			if change == "" {
				return head
			}
			return withChanges(head, "  - "+change+"\n", "  - {change: organization-description-drop, phase: expand}\n")
		}
		activity := "  - name: ActivityEntry\n"
		dropActivity := func(dropped bool) string {
			head := td01
			if dropped {
				head = strings.Replace(td01, activity, activity+"    dropped_by: activity-entry-drop\n", 1)
			} else {
				start := strings.Index(head, activity)
				end := strings.Index(head, "\ncollections:\n")
				head = head[:start] + head[end+1:]
				for _, name := range []string{"user-activity", "org-activity"} {
					at := strings.Index(head, "  "+name+":\n")
					next := strings.Index(head[at+1:], "\n\n")
					head = head[:at] + head[at+1+next+2:]
				}
			}
			return withChanges(head, "  - {id: activity-entry-drop, kind: drop, entity: ActivityEntry}\n", "  - {change: activity-entry-drop, phase: expand}\n")
		}
		cases := []struct {
			name, head string
			refused    bool
			says       string
		}{
			{"a drop of the listed field Organization.description in its place", noDescription(
				"{id: organization-description-drop, kind: drop, entity: Organization, field: description, definition: \"string, max_length 1000, optional, after name\"}"), false, ""},
			{"a drop of the entity ActivityEntry, kept with dropped_by", dropActivity(true), false, ""},
			{"a drop whose Definition puts the field in another place", noDescription(
				"{id: organization-description-drop, kind: drop, entity: Organization, field: description, definition: \"string, max_length 1000, optional, first\"}"), true, "organizations"},
			{"a drop whose Definition leaves out the max_length", noDescription(
				"{id: organization-description-drop, kind: drop, entity: Organization, field: description, definition: \"string, optional, after name\"}"), true, "Organization.description"},
			{"an undeclared removal of the field", noDescription(""), true, "the Definition string, max_length 1000, optional, after name"},
			{"a removal of the entity ActivityEntry with no dropped_by", dropActivity(false), true, "dropped_by"},
		}
		for _, c := range cases {
			base := []byte(td01)
			check, err := CheckBase(BaseSource{Ref: "main", Commit: "c0ffee", Declaration: base, Record: recordFor(base)}, parseHead(t, c.head))
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			var messages []string
			for _, fault := range check.Faults {
				messages = append(messages, fault.Rule+": "+fault.Message)
				for _, change := range fault.Declare {
					messages = append(messages, "declare: "+change.String())
				}
			}
			switch {
			case !c.refused && check.HasFaults():
				t.Errorf("%s: refused: %v", c.name, messages)
			case c.refused && !strings.Contains(strings.Join(messages, "\n"), c.says):
				t.Errorf("%s: the faults %v do not say %q", c.name, messages, c.says)
			}
		}
	})
	t.Run("ST-GEN-INV33-T A drop whose definition gives the field another place names the column out of place and its place in the base", func(t *testing.T) {
		t.Parallel()
		// Walkthrough 20, W20-P-B: the fault gave the two CREATE TABLE texts and named no column.
		td01 := string(userManagementBase(t))
		head := strings.Replace(td01, "      - { name: description, type: string, optional: true, max_length: 1000 }\n", "", 1)
		head = withChanges(head, "  - {id: organization-description-drop, kind: drop, entity: Organization, field: description, definition: \"string, max_length 1000, optional, first\"}\n",
			"  - {change: organization-description-drop, phase: expand}\n")
		base := []byte(td01)
		check, err := CheckBase(BaseSource{Ref: "main", Commit: "c0ffee", Declaration: base, Record: recordFor(base)}, parseHead(t, head))
		if err != nil {
			t.Fatal(err)
		}
		var messages []string
		for _, fault := range check.Faults {
			messages = append(messages, fault.Message)
		}
		text := strings.Join(messages, "\n")
		for _, want := range []string{
			"the column description is out of place: it comes after id in the head, and after name in the base.",
			`the definition "string, max_length 1000, optional, first" of the drop organization-description-drop gives the field description the place "first"`,
			`end the definition with "after name"`,
		} {
			if !strings.Contains(text, want) {
				t.Errorf("the faults do not say %q:\n%s", want, text)
			}
		}
		if strings.Contains(text, "the base creates\n") {
			t.Errorf("the faults still give the two CREATE TABLE texts:\n%s", text)
		}
	})
}

// eventBase is a base whose Transaction declares the event fields wallet_id, kind and amount at version 1, with the
// events line given in place of them.
func eventBase(kind, amount, events string) string {
	return `kind: service
name: wallets
archetype: rest-crud
language: go
base_path: /api/wallets/v1
entities:
  - name: Transaction
    fields:
      - { name: wallet_id, type: string }
      - { name: kind, type: enum, values: [` + kind + `] }
      - { name: amount, type: ` + amount + ` }
      - { name: note, type: string, optional: true }
` + events + `collections:
  transactions:
    entity: Transaction
    operations: [create, read, list]
mixins:
  - durable-events
  - kafka-events
`
}

func TestBaseEventFields(t *testing.T) {
	t.Parallel()
	const kinds = "deposit, withdrawal"
	base := []byte(eventBase(kinds, "int64", "    events: {fields: [wallet_id, kind, amount]}\n"))
	cases := []struct {
		name, head string
		field      string
		refused    bool
	}{
		{"the same head", string(base), "", false},
		{"a head that adds the field note", eventBase(kinds, "int64", "    events: {fields: [wallet_id, kind, amount, note]}\n"), "", false},
		{"a head without amount", eventBase(kinds, "int64", "    events: {fields: [wallet_id, kind]}\n"), "amount", true},
		{"a head with amount retyped", eventBase(kinds, "string", "    events: {fields: [wallet_id, kind, amount]}\n"), "amount", true},
		{"a head with a value of kind removed", eventBase("deposit", "int64", "    events: {fields: [wallet_id, kind, amount]}\n"), "kind", true},
		{"a head with no events", eventBase(kinds, "int64", ""), "", true},
		{"a head with events at version 0", eventBase(kinds, "int64", "    events: {fields: [wallet_id, kind, amount], version: 0}\n"), "", true},
		{"a head without amount at version 2", eventBase(kinds, "int64", "    events: {fields: [wallet_id, kind], version: 2}\n"), "", false},
	}
	for _, c := range cases {
		t.Run("ST-GEN-INV39-T "+c.name, func(t *testing.T) {
			check, err := CheckBase(BaseSource{Ref: "main", Commit: "c0ffee", Declaration: base, Record: recordFor(base)}, parseHead(t, c.head))
			if err != nil {
				t.Fatal(err)
			}
			var faults []BaseFault
			for _, fault := range check.Faults {
				if fault.Rule == EventFieldsRule {
					faults = append(faults, fault)
				}
			}
			if !c.refused {
				if len(faults) != 0 {
					t.Fatalf("want accepted, got %+v", faults)
				}
				return
			}
			if len(faults) != 1 {
				t.Fatalf("want one fault of %s, got %+v", EventFieldsRule, faults)
			}
			f := faults[0]
			if f.Entity != "Transaction" || f.Field != c.field || !strings.Contains(f.Message, "events.version to 2") {
				t.Fatalf("the fault does not name Transaction, the field %q and the remedy: %+v", c.field, f)
			}
		})
	}
}

func TestBaseUniqueOfField(t *testing.T) {
	const unique = "{id: user-display-name-unique, kind: unique, entity: User, field: display_name}"
	t.Run("ST-GEN-INV33-T A field of the base made unique needs a unique of the field, which makes it unique in place", func(t *testing.T) {
		t.Parallel()
		td01 := string(userManagementBase(t))
		cases := []struct {
			name, head string
			refused    bool
			says       string
		}{
			{"unique: true with its unique step", relaxUser(t, td01, displayName, "{ name: display_name, type: string, min_length: 1, max_length: 100, unique: true }", unique), false, ""},
			{"a unique_composite with its unique step", relaxUser(t, td01, displayName, "{ name: display_name, type: string, min_length: 1, max_length: 100, unique_composite: [display_name, role] }", unique), false, ""},
			{"unique_when_live with its unique step", relaxUser(t, td01, displayName, "{ name: display_name, type: string, min_length: 1, max_length: 100, unique: true, unique_when_live: true }", unique), false, ""},
			{"unique: true with no step", relaxUser(t, td01, displayName, "{ name: display_name, type: string, min_length: 1, max_length: 100, unique: true }", ""), true, "the head makes User.display_name unique, which no schema step declares"},
			{"unique: true and a longer max_length with a unique step", relaxUser(t, td01, displayName, "{ name: display_name, type: string, min_length: 1, max_length: 200, unique: true }", unique), true, "User.display_name"},
		}
		for _, c := range cases {
			base := []byte(td01)
			check, err := CheckBase(BaseSource{Ref: "main", Commit: "c0ffee", Declaration: base, Record: recordFor(base)}, parseHead(t, c.head))
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			var messages []string
			for _, fault := range check.Faults {
				messages = append(messages, fault.Rule+": "+fault.Message)
			}
			switch {
			case !c.refused && check.HasFaults():
				t.Errorf("%s: refused: %v", c.name, messages)
			case c.refused && !strings.Contains(strings.Join(messages, "\n"), c.says):
				t.Errorf("%s: the faults %v do not say %q", c.name, messages, c.says)
			}
			if c.name == "unique: true with no step" {
				if len(check.Faults) != 1 || len(check.Faults[0].Declare) != 1 || check.Faults[0].Declare[0].Kind != "unique" || check.Faults[0].Declare[0].Field != "display_name" ||
					check.Faults[0].Declare[0].ID != "user-display-name-unique" || check.Faults[0].Declare[0].Phase != "expand" {
					t.Errorf("%s: the faults are %+v, want one that declares the unique of User.display_name at expand", c.name, check.Faults)
					continue
				}
				// The suggested step makes the head pass.
				declared := relaxUser(t, td01, displayName, "{ name: display_name, type: string, min_length: 1, max_length: 100, unique: true }", unique)
				if again, err := CheckBase(BaseSource{Ref: "main", Commit: "c0ffee", Declaration: base, Record: recordFor(base)}, parseHead(t, declared)); err != nil || again.HasFaults() {
					t.Errorf("the suggested unique step is refused: %v %+v", err, again.Faults)
				}
			}
		}
	})

	t.Run("ST-GEN-INV33-T A new unique step of a field on a base whose runtime library refuses its index is refused, naming the version to deploy first", func(t *testing.T) {
		t.Parallel()
		td01 := string(userManagementBase(t))
		base := parseHead(t, td01)
		head := parseHead(t, relaxUser(t, td01, displayName, "{ name: display_name, type: string, min_length: 1, max_length: 100, unique: true }", unique))
		applied := func(runtime string) *AppliedState {
			state := &AppliedState{Components: map[string]ComponentState{"postgres-adapter": {Version: "6.4.0"}}, Archetype: &ArchetypeState{Version: "3.0.0"}}
			if runtime != "" {
				state.Archetype.Runtime = &registry.RuntimePin{Version: runtime}
			}
			return state
		}
		for _, runtime := range []string{"", "v0.182.0", "v0.108.0"} {
			faults := uniqueRuntimeFaults(applied(runtime), base, head)
			if len(faults) != 1 || faults[0].Rule != StepLedgerRule || !strings.Contains(faults[0].Message, UniqueRuntimeVersion) || !strings.Contains(faults[0].Message, "user-display-name-unique") {
				t.Errorf("runtime %q: the faults are %+v, want one of %s that names %s and the change", runtime, faults, StepLedgerRule, UniqueRuntimeVersion)
			}
		}
		for _, runtime := range []string{UniqueRuntimeVersion, "v0.196.0"} {
			if faults := uniqueRuntimeFaults(applied(runtime), base, head); len(faults) != 0 {
				t.Errorf("runtime %s: refused: %+v", runtime, faults)
			}
		}
		if faults := uniqueRuntimeFaults(&AppliedState{Components: map[string]ComponentState{}}, base, head); len(faults) != 0 {
			t.Errorf("a base with no postgres-adapter: refused: %+v", faults)
		}
		// A unique step of the base is no new step, and a relax is no unique step.
		if faults := uniqueRuntimeFaults(applied("v0.95.0"), head, head); len(faults) != 0 {
			t.Errorf("a unique step that the base has: refused: %+v", faults)
		}
		relaxed := parseHead(t, relaxUser(t, td01, displayName, "{ name: display_name, type: string, min_length: 1, max_length: 200 }",
			"{id: user-display-name-relax, kind: relax, entity: User, field: display_name, definition: max_length 100}"))
		if faults := uniqueRuntimeFaults(applied("v0.108.0"), base, relaxed); len(faults) != 0 {
			t.Errorf("a relax: refused as a unique step: %+v", faults)
		}
	})
}

func TestBaseRelateFaults(t *testing.T) {
	t.Parallel()
	t.Run("ST-GEN-INV33-T A relation that the head writes for each row of a base entity and the base does not write needs a relate step", func(t *testing.T) {
		t.Parallel()
		declared := userManagementBase(t)
		schema, err := os.ReadFile(filepath.Join("..", "..", "examples", "user-management", "authz", "schema.zed"))
		if err != nil {
			t.Fatal(err)
		}
		// The head gives organization the relation tenant of the subject type tenant, which the runtime writes for
		// each row (walkthrough 24, P2-R2).
		tenantSchema := strings.Replace(string(schema), "definition organization {\n\trelation owner: user\n", "definition tenant {\n\trelation auditor: user\n}\n\ndefinition organization {\n\trelation owner: user\n\trelation tenant: tenant\n", 1)
		if tenantSchema == string(schema) {
			t.Fatal("the schema of the example has no definition organization to edit")
		}
		compile := func(source string) *declaration.AuthzSchema {
			t.Helper()
			compiled, err := declaration.CompileAuthzSchema("authz/schema.zed", []byte(source))
			if err != nil {
				t.Fatal(err)
			}
			return compiled
		}
		related := withChanges(string(declared), "  - {id: organization-tenant-relate, kind: relate, entity: Organization, relation: tenant}\n", "  - {change: organization-tenant-relate, phase: expand}\n")
		base := BaseSource{Ref: "main", Commit: "c0ffee", Declaration: declared, Record: recordFor(declared), Schema: schema}
		cases := []struct {
			name, head string
			schema     *declaration.AuthzSchema
			fault      bool
		}{
			{"the same schema", string(declared), compile(string(schema)), false},
			{"a new relation tenant with no relate step", string(declared), compile(tenantSchema), true},
			{"a new relation tenant with its relate step", related, compile(tenantSchema), false},
			{"a head that binds no schema", string(declared), nil, false},
		}
		for _, c := range cases {
			head := parseHead(t, c.head)
			check, err := CheckBase(base, head)
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			faults, err := RelateFaults(check, base, head, c.schema)
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if !c.fault {
				if len(faults) != 0 {
					t.Errorf("%s: want no fault, got %+v", c.name, faults)
				}
				continue
			}
			if len(faults) != 1 || faults[0].Rule != BaseCompatRule || faults[0].Entity != "Organization" || !strings.Contains(faults[0].Message, "organization#tenant") ||
				len(faults[0].Declare) != 1 || faults[0].Declare[0].Kind != "relate" || faults[0].Declare[0].ID != "organization-tenant-relate" || faults[0].Declare[0].Needs != "relation: tenant" {
				t.Errorf("%s: want one fault that names Organization, organization#tenant and the relate step, got %+v", c.name, faults)
			}
		}
		// The base that has the relation already gives no fault, and a base whose commit does not hold its schema
		// fails, so the check never takes a base with no schema as one that writes no relation.
		withTenant := base
		withTenant.Schema = []byte(tenantSchema)
		check, err := CheckBase(withTenant, parseHead(t, string(declared)))
		if err != nil {
			t.Fatal(err)
		}
		if faults, err := RelateFaults(check, withTenant, parseHead(t, string(declared)), compile(tenantSchema)); err != nil || len(faults) != 0 {
			t.Errorf("a base that writes the relation tenant: want no fault, got %+v, %v", faults, err)
		}
		missing := base
		missing.Schema = nil
		if _, err := RelateFaults(check, missing, parseHead(t, string(declared)), compile(tenantSchema)); err == nil || !strings.Contains(err.Error(), "does not hold") {
			t.Errorf("a base whose commit does not hold its schema: want an error, got %v", err)
		}
	})
	// The relations that no relate step gives the rows of the base, and a base that binds no schema (security review
	// of PR #495, L1, L2 and I1).
	{
		declared := string(userManagementBase(t))
		schema, err := os.ReadFile(filepath.Join("..", "..", "examples", "user-management", "authz", "schema.zed"))
		if err != nil {
			t.Fatal(err)
		}
		compile := func(source string) *declaration.AuthzSchema {
			t.Helper()
			compiled, err := declaration.CompileAuthzSchema("authz/schema.zed", []byte(source))
			if err != nil {
				t.Fatal(err)
			}
			return compiled
		}
		edit := func(text, old, new string) string {
			t.Helper()
			edited := strings.Replace(text, old, new, 1)
			if edited == text {
				t.Fatalf("the text has no %q to edit", old)
			}
			return edited
		}
		faultsOf := func(t *testing.T, baseText string, baseSchema []byte, headText string, headSchema *declaration.AuthzSchema) []BaseFault {
			t.Helper()
			base := BaseSource{Ref: "main", Commit: "c0ffee", Declaration: []byte(baseText), Record: recordFor([]byte(baseText)), Schema: baseSchema}
			head := parseHead(t, headText)
			check, err := CheckBase(base, head)
			if err != nil {
				t.Fatal(err)
			}
			faults, err := RelateFaults(check, base, head, headSchema)
			if err != nil {
				t.Fatal(err)
			}
			return faults
		}
		// otherPath tells whether the faults are one fault of the entity and the relation that suggests no schema change
		// and tells the author to give the permission another path.
		otherPath := func(faults []BaseFault, entity, relation string) bool {
			return len(faults) == 1 && faults[0].Rule == BaseCompatRule && faults[0].Entity == entity && len(faults[0].Declare) == 0 &&
				strings.Contains(faults[0].Message, relation) && strings.Contains(faults[0].Message, "Give the permission another path")
		}
		t.Run("ST-GEN-INV33-T A new relation from the caller of the create on a base entity with rows is refused, and the fault tells the author to give the permission another path", func(t *testing.T) {
			t.Parallel()
			caller := "    relationships:\n      - {relation: owner, subject_source: caller, subject_type: user}\n"
			head := edit(declared, "    public_reasons: {create: an activity entry of a caller has no resource to decide on before it exists}\n", "    public_reasons: {create: an activity entry of a caller has no resource to decide on before it exists}\n"+caller)
			if faults := faultsOf(t, declared, schema, head, compile(string(schema))); !otherPath(faults, "ActivityEntry", "activity_entry#owner") || !strings.Contains(faults[0].Message, "caller") {
				t.Errorf("a new caller relation: want one fault of ActivityEntry and activity_entry#owner with no schema change, got %+v", faults)
			}
			// A relate step cannot correct it, as no row holds the caller of its create.
			related := withChanges(head, "  - {id: activity-entry-owner-relate, kind: relate, entity: ActivityEntry, relation: owner}\n", "  - {change: activity-entry-owner-relate, phase: expand}\n")
			if faults := faultsOf(t, declared, schema, related, compile(string(schema))); !otherPath(faults, "ActivityEntry", "activity_entry#owner") {
				t.Errorf("a new caller relation with a relate step: want the same fault, got %+v", faults)
			}
			// A base that writes the caller relation already gives no fault.
			if faults := faultsOf(t, head, schema, head, compile(string(schema))); len(faults) != 0 {
				t.Errorf("a base with the caller relation: want no fault, got %+v", faults)
			}
		})
		t.Run("ST-GEN-INV33-T A new field or reference relation of a shared or system entity of the base is refused, and the fault tells the author to give the permission another path", func(t *testing.T) {
			t.Parallel()
			plans := "  - name: Plan\n    tenant: shared\n    resource_type: plan\n    permissions: {read: view, list: view}\n    fields:\n      - { name: label, type: string }\n      - { name: owner_id, type: string, max_length: 255 }\n      - { name: org_id, type: ref, to: Organization }\n\n"
			planSchema := string(schema) + "\ndefinition plan {\n\trelation owner: user\n\trelation organization: organization\n\trelation tenant: tenant\n\tpermission view = owner + organization->view\n}\n\ndefinition tenant {\n\trelation auditor: user\n}\n"
			base := edit(declared, "  - name: ActivityEntry\n", plans+"  - name: ActivityEntry\n")
			field := edit(base, "    resource_type: plan\n", "    resource_type: plan\n    relationships:\n      - {relation: owner, subject_source: field, subject_field: owner_id, subject_type: user}\n")
			reference := edit(base, "    resource_type: plan\n", "    resource_type: plan\n    references:\n      org_id: {relation: organization, permission: view}\n")
			// The base binds the schema with the definition plan, so only the new relation of the head differs; the
			// relation tenant, which a shared row cannot have, gives no fault.
			for _, kind := range []string{"shared", "system"} {
				for _, c := range []struct{ name, head, relation, field string }{
					{"a field relation", field, "plan#owner", "owner_id"},
					{"a reference relation", reference, "plan#organization", "org_id"},
				} {
					baseText := strings.Replace(base, "tenant: shared", "tenant: "+kind, 1)
					headText := strings.Replace(c.head, "tenant: shared", "tenant: "+kind, 1)
					faults := faultsOf(t, baseText, []byte(planSchema), headText, compile(planSchema))
					if !otherPath(faults, "Plan", c.relation) || faults[0].Field != c.field || !strings.Contains(faults[0].Message, "is "+kind) {
						t.Errorf("%s of a %s entity: want one fault of Plan.%s and %s that names %s and no schema change, got %+v", c.name, kind, c.field, c.relation, kind, faults)
					}
					if faults := faultsOf(t, headText, []byte(planSchema), headText, compile(planSchema)); len(faults) != 0 {
						t.Errorf("%s of a %s entity that the base writes already: want no fault, got %+v", c.name, kind, faults)
					}
				}
			}
		})
		t.Run("ST-GEN-INV33-T A head that binds an authorization schema that the base did not bind needs a relate step for each relation of the rows of the base", func(t *testing.T) {
			t.Parallel()
			unbound := edit(declared, "authorization:\n  provider: spicedb\n  schema_path: authz/schema.zed\n  subject_from_identity: user\n", "")
			// The base binds no schema, so the commit holds none that the check reads.
			faults := faultsOf(t, unbound, nil, declared, compile(string(schema)))
			var named []string
			for _, f := range faults {
				if f.Rule != BaseCompatRule || len(f.Declare) != 1 || f.Declare[0].Kind != "relate" {
					t.Errorf("want a fault that suggests a relate step, got %+v", f)
				}
				named = append(named, f.Entity+"."+f.Field+" "+f.Declare[0].ID)
			}
			if want := []string{"User.org_id user-organization-relate", "OrgSetting.org_id orgsetting-organization-relate"}; !slices.Equal(named, want) {
				t.Errorf("a head that adds the schema: want the faults %v, got %v", want, named)
			}
			related := withChanges(declared, "  - {id: user-organization-relate, kind: relate, entity: User, relation: organization}\n  - {id: orgsetting-organization-relate, kind: relate, entity: OrgSetting, relation: organization}\n", "  - {change: user-organization-relate, phase: expand}\n  - {change: orgsetting-organization-relate, phase: expand}\n")
			if faults := faultsOf(t, unbound, nil, related, compile(string(schema))); len(faults) != 0 {
				t.Errorf("a head that adds the schema with its relate steps: want no fault, got %+v", faults)
			}
		})
	}
}
