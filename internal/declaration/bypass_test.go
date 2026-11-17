package declaration

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/jsell-rh/stego/internal/types"
)

// bypassSchemas gives the config schemas of the active components of the fixture: the authorization component
// spicedb-authz declares skip_authz, marked as a bypass, and cache_ttl, not marked, as a component.yaml of a
// registry copy written into its files states them. The schema is decoded through the yaml tags of the registry's
// config schema, so the mark is read as a registry reads it.
func bypassSchemas(t *testing.T) map[string]map[string]types.ConfigField {
	t.Helper()
	const config = `
skip_authz:
  type: bool
  optional: true
  bypass: true
cache_ttl:
  type: string
  optional: true
rules:
  type: list
  optional: true
  items:
    name: {type: string}
    unchecked: {type: bool, optional: true, bypass: true}
groups:
  type: list
  optional: true
  items:
    type: list
    items:
      name: {type: string}
      unchecked: {type: bool, optional: true, bypass: true}
`
	var schema map[string]types.ConfigField
	if err := yaml.Unmarshal([]byte(config), &schema); err != nil {
		t.Fatal(err)
	}
	if groups := schema["groups"].Items; groups == nil || groups.Inline == nil || groups.Inline.Items == nil || !groups.Inline.Items.Fields["unchecked"].Bypass {
		t.Fatalf("the fixture schema does not hold the list of lists groups: %+v", schema["groups"])
	}
	if !schema["skip_authz"].Bypass || schema["cache_ttl"].Bypass {
		t.Fatalf("the fixture schema does not mark skip_authz alone: %+v", schema)
	}
	return map[string]map[string]types.ConfigField{"spicedb-authz": schema}
}

// withSettings gives TD03 with the settings of spicedb-authz given.
func withSettings(t *testing.T, service, settings string) string {
	t.Helper()
	return change(t, service, "authorization:\n", "overrides:\n  spicedb-authz:\n"+settings+"authorization:\n")
}

func TestBypassFlagPresent(t *testing.T) {
	service, _ := td03Files(t)
	schemas := bypassSchemas(t)
	cases := []struct {
		name     string
		settings string
		want     bool
	}{
		{"skip_authz marked as a bypass", "    skip_authz: true\n", true},
		{"the marked setting of an object in a list", "    rules:\n      - {name: a, unchecked: true}\n", true},
		{"the marked setting of an object in a list in a list", "    groups:\n      - [{name: a, unchecked: true}]\n", true},
		{"cache_ttl not marked", "    cache_ttl: 30s\n", false},
		{"an object in a list in a list with no marked setting", "    groups:\n      - [{name: a}]\n", false},
		{"an object in a list with no marked setting", "    rules:\n      - {name: a}\n", false},
		{"TD03 unchanged", "", false},
	}
	for _, c := range cases {
		t.Run("ST-DECL-SP07-T "+c.name, func(t *testing.T) {
			declared := service
			if c.settings != "" {
				declared = withSettings(t, service, c.settings)
			}
			if got := BypassFlagPresent(readTD03(t, declared), schemas); got != c.want {
				t.Errorf("BypassFlagPresent = %v, want %v", got, c.want)
			}
		})
	}
}

func TestNoBypassFlag(t *testing.T) {
	service, schema := td03Files(t)
	schemas := bypassSchemas(t)
	t.Run("ST-DECL-INV24-T A setting marked as a bypass is refused under the no-bypass rule, naming the component and the setting", func(t *testing.T) {
		faults := ValidateNoBypass(readTD03(t, withSettings(t, service, "    skip_authz: true\n")), schemas)
		if len(faults) != 1 || faults[0].Rule != "no-bypass" || faults[0].Path != "overrides.spicedb-authz.skip_authz" ||
			!strings.Contains(faults[0].Message, "spicedb-authz") || !strings.Contains(faults[0].Message, "skip_authz") {
			t.Fatalf("want one no-bypass fault that names spicedb-authz and skip_authz, got %+v", faults)
		}
		// A fault names the setting by its path and never holds its value.
		if faults[0].Found != "" || strings.Contains(faults[0].Message, "true") {
			t.Errorf("the fault holds the value of the setting: %+v", faults[0])
		}
	})
	t.Run("ST-DECL-INV24-T A marked setting of an object in a list in a list is refused at its path", func(t *testing.T) {
		faults := ValidateNoBypass(readTD03(t, withSettings(t, service, "    groups:\n      - [{name: a, unchecked: true}]\n")), schemas)
		if len(faults) != 1 || faults[0].Path != "overrides.spicedb-authz.groups[0][0].unchecked" || faults[0].Found != "" {
			t.Fatalf("want one no-bypass fault at overrides.spicedb-authz.groups[0][0].unchecked with no value, got %+v", faults)
		}
	})
	t.Run("ST-DECL-INV24-T A setting that is not marked, and TD03 unchanged, are accepted", func(t *testing.T) {
		for _, declared := range []string{withSettings(t, service, "    cache_ttl: 30s\n"), service} {
			if faults := ValidateNoBypass(readTD03(t, declared), schemas); len(faults) != 0 {
				t.Errorf("want no fault, got %+v", faults)
			}
		}
	})
	t.Run("ST-DECL-INV24-T A bypass setting that states a default in the schema, also in a list in a list, is refused at the default's path", func(t *testing.T) {
		const config = `
skip_authz: {type: bool, optional: true, bypass: true, default: true}
cache_ttl: {type: string, optional: true, default: 30s}
groups:
  type: list
  optional: true
  items:
    type: list
    items:
      name: {type: string}
      unchecked: {type: bool, optional: true, bypass: true, default: true}
`
		var withDefaults map[string]types.ConfigField
		if err := yaml.Unmarshal([]byte(config), &withDefaults); err != nil {
			t.Fatal(err)
		}
		faults := ValidateBypassDefaults(map[string]map[string]types.ConfigField{"spicedb-authz": withDefaults}, nil)
		want := []string{"components.spicedb-authz.config.groups.items.items.unchecked.default", "components.spicedb-authz.config.skip_authz.default"}
		if len(faults) != len(want) {
			t.Fatalf("want the no-bypass faults %v, got %+v", want, faults)
		}
		for i, fault := range faults {
			if fault.Rule != "no-bypass" || fault.Path != want[i] || fault.Found != "" || !strings.Contains(fault.Message, "spicedb-authz") || strings.Contains(fault.Message, "true") {
				t.Errorf("fault %d: want a no-bypass fault at %s that names the component and holds no value, got %+v", i, want[i], fault)
			}
		}
	})
	t.Run("ST-DECL-INV24-T An environment variable whose default is a bypass setting is refused, and one whose default is another setting is accepted", func(t *testing.T) {
		defaults := map[string]map[string]string{"spicedb-authz": {"SKIP_AUTHZ": "skip_authz", "CACHE_TTL": "cache_ttl"}}
		faults := ValidateBypassDefaults(schemas, defaults)
		if len(faults) != 1 || faults[0].Rule != "no-bypass" || faults[0].Path != "components.spicedb-authz.environment.SKIP_AUTHZ.default" ||
			!strings.Contains(faults[0].Message, "skip_authz") {
			t.Fatalf("want one no-bypass fault at components.spicedb-authz.environment.SKIP_AUTHZ.default, got %+v", faults)
		}
		if faults := ValidateBypassDefaults(schemas, nil); len(faults) != 0 {
			t.Errorf("a schema whose bypass settings state no default gives faults: %+v", faults)
		}
	})
	binding := func(systemType string) string {
		declared := service + "slots:\n  - collection: todos\n    slot: before_create\n    gate: [policy]\n    runs_as: system\n"
		if systemType != "" {
			declared = change(t, declared, "  subject_from_identity: user\n", "  subject_from_identity: user\n  system_subject_type: "+systemType+"\n")
		}
		return declared
	}
	noBypass := func(faults []ValidationError) []ValidationError {
		var found []ValidationError
		for _, fault := range faults {
			if fault.Rule == "no-bypass" {
				found = append(found, fault)
			}
		}
		return found
	}
	for _, refused := range []struct{ name, systemType, names string }{
		{"no system subject type", "", "slots[0].runs_as"},
		{"a type the schema does not define", "robot", "robot"},
		{"a type that no relation grants", "folder", "folder"},
	} {
		t.Run("ST-DECL-INV24-T A binding that runs as system with "+refused.name+" is refused, naming the binding and the type", func(t *testing.T) {
			faults := noBypass(checkTD03(t, binding(refused.systemType), schema))
			if len(faults) != 1 || !strings.Contains(faults[0].Path+" "+faults[0].Found+" "+faults[0].Message, refused.names) {
				t.Fatalf("want one no-bypass fault that names %s, got %+v", refused.names, faults)
			}
			if !strings.Contains(faults[0].Message, "todos at before_create") {
				t.Errorf("the fault does not name the binding of todos at before_create: %+v", faults[0])
			}
		})
	}
	t.Run("ST-DECL-INV24-T A binding that runs as system with service_account, which the relation viewer of document grants, is accepted", func(t *testing.T) {
		if faults := noBypass(checkTD03(t, binding("service_account"), schema)); len(faults) != 0 {
			t.Fatalf("want no no-bypass fault, got %+v", faults)
		}
	})
}
