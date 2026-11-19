package delivery

import (
	"bytes"
	"strings"
	"testing"
)

func text(value string) *string { return &value }

// variableComponents are two components and a declaration group: an optional variable that two components state
// alike, a required one, a secret one and a variable whose name and default come from the settings.
func variableComponents() []ComponentNeeds {
	return []ComponentNeeds{
		{Component: "zeta", Settings: map[string]string{"issuer": "https://sso.example.com", "key": "CANARY-SECRET-VALUE"}, Variables: []Variable{
			{Name: "TIMEOUT", Type: "duration", Default: text("5m"), Description: "The time of a call, as zeta reads it.", Allowed: "1s to 30m", Example: text("2m")},
			{Name: "ISSUER", Type: "url", Default: text("{settings.issuer}"), Description: "The issuer."},
			{Name: "DATABASE_URL", Type: "string", Secret: true, Description: "The connection string."},
		}},
		{Component: "alpha", Variables: []Variable{
			{Name: "TIMEOUT", Type: "duration", Default: text("5m"), Description: "The time of a call, as alpha reads it.", Allowed: "1s to 30m", Example: text("3m")},
			{Name: "LOG_LEVEL", Type: "string", Required: true, Description: "The level of the log."},
		}},
		{Component: "runtime-configuration", Declared: []DeclaredVariables{{Group: "Worker", Variables: []Variable{
			{Name: "WIDGET_TOKEN", Type: "string", Required: true, Secret: true, Description: "The widget token."},
			{Name: "WIDGET_RESYNC", Type: "duration", Default: text("30s"), Description: "The resync time.", Allowed: "1s to 1h"},
		}}}},
	}
}

func TestEnvironmentList(t *testing.T) {
	t.Run("ST-DLV-INV11-T The platform requirements list each variable once with its facts, no other variable, and one configuration need for each required variable that is not secret", func(t *testing.T) {
		requirements, err := DeriveRequirements("orders", variableComponents())
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, v := range requirements.Environment {
			names = append(names, v.Name)
		}
		if got := strings.Join(names, " "); got != "DATABASE_URL ISSUER LOG_LEVEL TIMEOUT WIDGET_RESYNC WIDGET_TOKEN" {
			t.Fatalf("the list is %s", got)
		}
		timeout := requirements.Environment[3]
		if strings.Join(timeout.ReadBy, ",") != "alpha,zeta" || timeout.Type != "duration" || timeout.Required || *timeout.Default != "5m" ||
			timeout.Allowed != "1s to 30m" || timeout.Description != "The time of a call, as alpha reads it." || timeout.Secret {
			t.Fatalf("TIMEOUT is %+v", timeout)
		}
		if issuer := requirements.Environment[1]; issuer.Default == nil || *issuer.Default != "https://sso.example.com" {
			t.Fatalf("the default of ISSUER is not the setting: %+v", issuer)
		}
		if resync := requirements.Environment[4]; strings.Join(resync.ReadBy, ",") != "runtime-configuration (Worker)" {
			t.Fatalf("WIDGET_RESYNC is read by %v", resync.ReadBy)
		}
		var configuration []string
		for _, r := range requirements.Requirements {
			if r.Kind == Configuration {
				configuration = append(configuration, r.Name+"="+strings.Join(r.Components, ","))
			}
		}
		if got := strings.Join(configuration, " "); got != "LOG_LEVEL=alpha" {
			t.Fatalf("the configuration needs are %s, want LOG_LEVEL of alpha alone", got)
		}
	})
	t.Run("ST-DLV-INV12-T A secret variable is listed by its name, readers, type, required flag, description and secret flag only", func(t *testing.T) {
		components := variableComponents()
		components[0].Variables[2].Allowed = "CANARY-SECRET-VALUE"
		files, err := WriteRequirements("orders", components)
		if err != nil {
			t.Fatal(err)
		}
		requirements, err := LoadRequirements(files.Requirements)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range requirements.Environment {
			if v.Secret && (v.Default != nil || v.Allowed != "") {
				t.Errorf("the secret variable %s is listed with a default or allowed values", v.Name)
			}
		}
		for name, data := range map[string][]byte{"requirements": files.Requirements, "reference": files.Reference} {
			if bytes.Contains(data, []byte("CANARY-SECRET-VALUE")) {
				t.Errorf("the %s hold the canary text:\n%s", name, data)
			}
		}
		components[0].Variables[2].Default = text("CANARY-SECRET-VALUE")
		if _, err := WriteRequirements("orders", components); err == nil || strings.Contains(err.Error(), "CANARY") {
			t.Fatalf("a secret variable with a default is listed, or the refusal shows it: %v", err)
		}
	})
	t.Run("ST-DLV-UC01-R09-T01 Two sources that state one variable with different facts are refused, naming both sources and the variable", func(t *testing.T) {
		for fact, change := range map[string]func(*Variable){
			"type":           func(v *Variable) { v.Type = "string" },
			"required flag":  func(v *Variable) { v.Required, v.Default = true, nil },
			"default":        func(v *Variable) { v.Default = text("6m") },
			"secret flag":    func(v *Variable) { v.Secret, v.Default, v.Example = true, nil, nil },
			"allowed values": func(v *Variable) { v.Allowed = "1s to 1h" },
		} {
			components := variableComponents()
			change(&components[1].Variables[0])
			files, err := WriteRequirements("orders", components)
			if err == nil {
				t.Errorf("%s: two statements that differ are accepted", fact)
				continue
			}
			if files.Requirements != nil || files.Reference != nil {
				t.Errorf("%s: the refused derivation gives a file", fact)
			}
			for _, part := range []string{"component alpha", "component zeta", "TIMEOUT", fact} {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("%s: the refusal does not name %s: %v", fact, part, err)
				}
			}
		}
		components := variableComponents()
		components[2].Declared[0].Variables[1].Name = "TIMEOUT"
		if _, err := WriteRequirements("orders", components); err == nil || !strings.Contains(err.Error(), "component runtime-configuration group Worker") {
			t.Fatalf("a declaration field that differs from a component is accepted, or the refusal does not name the group: %v", err)
		}
	})
	t.Run("ST-DLV-UC01-R07-T01 The reference has one section for each reader in name order and one row for each variable in name order, rendered from the list alone", func(t *testing.T) {
		files, err := WriteRequirements("orders", variableComponents())
		if err != nil {
			t.Fatal(err)
		}
		reference := string(files.Reference)
		order := []string{"## alpha", "`LOG_LEVEL`", "`TIMEOUT`", "## runtime-configuration (Worker)", "`WIDGET_RESYNC`", "`WIDGET_TOKEN`", "## zeta", "`DATABASE_URL`", "`ISSUER`", "`TIMEOUT`"}
		at := 0
		for _, part := range order {
			next := strings.Index(reference[at:], part)
			if next < 0 {
				t.Fatalf("the reference does not hold %s after the place %d:\n%s", part, at, reference)
			}
			at += next + len(part)
		}
		if !strings.Contains(reference, "| `TIMEOUT` | duration | no | `5m` | no | 1s to 30m | `3m` | The time of a call, as alpha reads it. |") {
			t.Fatalf("the row of TIMEOUT does not show its facts and the example of the first reader:\n%s", reference)
		}
		if !strings.Contains(reference, "| `DATABASE_URL` | string | no | - | yes | - | - | The connection string. |") {
			t.Fatalf("the row of the secret variable shows more than its name and facts:\n%s", reference)
		}
		requirements, err := DeriveRequirements("orders", variableComponents())
		if err != nil {
			t.Fatal(err)
		}
		if again := renderReference(requirements.Service, requirements.Environment); !bytes.Equal(again, files.Reference) {
			t.Fatal("the reference is not the rendering of the derived list")
		}
	})
	t.Run("ST-DLV-UC01-R07-T05 The reference shows the condition of a variable that is required when it holds, and the list keeps it not required", func(t *testing.T) {
		components := variableComponents()
		components[0].Variables = append(components[0].Variables, Variable{Name: "KEY_RING_FILE", Type: "path", Description: "The key ring.",
			RequiredWhen: "the service has a secret field or a pii field"})
		files, err := WriteRequirements("orders", components)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(files.Reference), "| `KEY_RING_FILE` | path | yes, when the service has a secret field or a pii field | none | no | - | - | The key ring. |") {
			t.Fatalf("the row of KEY_RING_FILE does not show its condition (walkthrough 14):\n%s", files.Reference)
		}
		// The platform requirements, which the environment check and the base read, keep the variable not required and
		// hold no condition: a secret source gives its value.
		if strings.Contains(string(files.Requirements), "secret field or a pii field") {
			t.Fatalf("the platform requirements hold the condition:\n%s", files.Requirements)
		}
		requirements, err := DeriveRequirements("orders", components)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range requirements.Environment {
			if v.Name == "KEY_RING_FILE" && v.Required {
				t.Fatal("the list takes the variable as required")
			}
		}
	})
	t.Run("ST-DLV-UC01-R07-T02 A text of the list cannot break a row of the reference", func(t *testing.T) {
		components := variableComponents()
		components[1].Variables[1].Description = "The level | of the log `x` <b>."
		components[1].Variables[0].Example = text("a|b")
		components[0].Variables[0].Example = components[1].Variables[0].Example
		files, err := WriteRequirements("orders", components)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(files.Reference), "\n") {
			if strings.HasPrefix(line, "| `") && strings.Count(strings.ReplaceAll(line, "\\|", ""), "|") != 9 {
				t.Errorf("a row has a cell too many: %s", line)
			}
		}
	})
}
