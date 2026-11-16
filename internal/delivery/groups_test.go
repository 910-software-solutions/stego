package delivery

import (
	"slices"
	"strings"
	"testing"
)

// groupComponents are a component with the pair DATABASE_URL and DATABASE_URL_FILE in the group database-url, with
// the secret need of DATABASE_URL, and a component with a variable of its own.
func groupComponents() []ComponentNeeds {
	return []ComponentNeeds{
		{Component: "store", Needs: []Need{{Kind: "secret", Name: "{service}-database", Keys: []string{"url"}}}, Variables: []Variable{
			{Name: "DATABASE_URL", Type: "string", Secret: true, Description: "The connection string.", OneOf: "database-url",
				SecretSource: &SecretSource{Name: "{service}-database", Key: "url"}},
			{Name: "DATABASE_URL_FILE", Type: "path", Description: "The file of the connection string.", OneOf: "database-url"},
		}},
		{Component: "broker", Variables: []Variable{{Name: "BROKERS", Type: "list", Description: "The brokers."}}},
	}
}

func TestGroupsOfOneOf(t *testing.T) {
	t.Run("ST-DLV-UC01-R06-T05 The list carries the group of one of of each variable, and a group of one variable, of two components or with a required variable is refused", func(t *testing.T) {
		files, err := WriteRequirements("orders", groupComponents())
		if err != nil {
			t.Fatal(err)
		}
		requirements, err := LoadRequirements(files.Requirements)
		if err != nil {
			t.Fatal(err)
		}
		groups := Groups(requirements.Environment)
		if len(groups) != 1 || len(groups["database-url"]) != 2 || groups["database-url"][0].Name != "DATABASE_URL" || groups["database-url"][1].Name != "DATABASE_URL_FILE" {
			t.Fatalf("the platform requirements hold the groups %v, want database-url with DATABASE_URL and DATABASE_URL_FILE", groups)
		}
		if strings.Count(string(files.Requirements), "one_of: database-url\n") != 2 {
			t.Errorf("the platform requirements do not write one_of on each variable of the group:\n%s", files.Requirements)
		}
		for name, change := range map[string]struct {
			change func([]ComponentNeeds)
			want   []string
		}{
			"a group of one variable": {func(c []ComponentNeeds) { c[0].Variables[1].OneOf = "" }, []string{"DATABASE_URL", "only one of the group of one of database-url"}},
			"a group of two components": {func(c []ComponentNeeds) { c[1].Variables[0].OneOf = "database-url"; c[0].Variables[1].OneOf = "" },
				[]string{"BROKERS", "broker", "store", "of one component"}},
			"a required variable in a group": {func(c []ComponentNeeds) { c[0].Variables[1].Required = true }, []string{"DATABASE_URL_FILE", "is required and in the group of one of database-url"}},
			"a group name with '_'":          {func(c []ComponentNeeds) { c[0].Variables[0].OneOf = "database_url" }, []string{"DATABASE_URL", "database_url", "lower-case"}},
		} {
			components := groupComponents()
			change.change(components)
			_, err := WriteRequirements("orders", components)
			if err == nil {
				t.Errorf("%s is accepted", name)
				continue
			}
			for _, want := range change.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal of %s does not name %q: %v", name, want, err)
				}
			}
		}
		// Two statements of one variable with different groups are a conflict of the two sources.
		components := groupComponents()
		components[1].Variables = append(components[1].Variables, Variable{Name: "DATABASE_URL_FILE", Type: "path", Description: "The file."})
		if _, err := WriteRequirements("orders", components); err == nil || !strings.Contains(err.Error(), "with a different group of one of") || !strings.Contains(err.Error(), "component broker and component store") {
			t.Errorf("two statements of DATABASE_URL_FILE with different groups are not refused as a conflict: %v", err)
		}
		// LoadRequirements holds the group rules too.
		for name, edit := range map[string][2]string{
			"a group of one variable":        {"    one_of: database-url\n", ""},
			"a required variable in a group": {"    required: false\n    description: The file of the connection string.\n", "    required: true\n    description: The file of the connection string.\n"},
		} {
			if !strings.Contains(string(files.Requirements), edit[0]) {
				t.Fatalf("%s: the platform requirements hold no %q", name, edit[0])
			}
			if _, err := LoadRequirements([]byte(strings.Replace(string(files.Requirements), edit[0], edit[1], 1))); err == nil {
				t.Errorf("LoadRequirements accepts %s", name)
			}
		}
	})

	t.Run("ST-DLV-UC01-R07-T04 The reference names the group of one of of each variable, and lists each group with its variables", func(t *testing.T) {
		files, err := WriteRequirements("orders", groupComponents())
		if err != nil {
			t.Fatal(err)
		}
		reference := string(files.Reference)
		for _, want := range []string{
			"| `DATABASE_URL` | string | one of `database-url` |", "| `DATABASE_URL_FILE` | path | one of `database-url` |", "| `BROKERS` | list | no |",
			"## Groups of one of", "The service needs exactly one variable of each group", "| `database-url` | `DATABASE_URL`, `DATABASE_URL_FILE` |",
			"The key of the Secret of each of these variables is the one value of its group:\n`DATABASE_URL`.\n", "never the *_FILE variable",
		} {
			if !strings.Contains(reference, want) {
				t.Errorf("the reference does not hold %q:\n%s", want, reference)
			}
		}
		files, err = WriteRequirements("orders", groupComponents()[1:])
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(files.Reference), "Groups of one of") || strings.Contains(string(files.Reference), "The key of the Secret of") {
			t.Errorf("the reference of a list with no group has a section of groups:\n%s", files.Reference)
		}
	})

	t.Run("ST-DLV-UC02-R06-T03 The environment check fails for a group of one of with no value or more than one, and names the group and how to give each variable", func(t *testing.T) {
		requirements := groupRequirements(t)
		for name, tc := range map[string]struct {
			sections string
			group    string
			status   FitStatus
			want     []string
		}{
			"the Secret of the source": {"secrets: {orders-database: {url: CANARY-SECRET-VALUE}}\nconfiguration: {BROKERS: kafka-0:9093}\n", "database-url", Met, nil},
			"no variable of the group": {"secrets: {orders-database: {password: CANARY-SECRET-VALUE}}\nconfiguration: {DATABASE_URL_FILE: \"\", BROKERS: kafka-0:9093}\n", "database-url", Missing,
				[]string{"group of one of database-url", "give exactly one of", "the key url of the secret orders-database (DATABASE_URL, or DATABASE_URL_FILE when the overlay lists the Component deploy/components/secret-files)"}},
			"one variable of a pair with no source": {"secrets: {orders-database: {url: CANARY-SECRET-VALUE}}\nconfiguration: {BROKERS_FILE: /etc/orders/brokers}\n", "brokers", Met, nil},
			"no variable of a pair with no source": {"secrets: {orders-database: {url: CANARY-SECRET-VALUE}}\nconfiguration: {BROKERS: \"\"}\n", "brokers", Missing,
				[]string{"group of one of brokers", "a configuration value BROKERS; a configuration value BROKERS_FILE"}},
			"the two variables": {"secrets: {orders-database: {url: CANARY-SECRET-VALUE}}\nconfiguration: {BROKERS: kafka-0:9093, BROKERS_FILE: /etc/orders/brokers}\n", "brokers", WrongType,
				[]string{"more than one variable of the group of one of brokers", "BROKERS, BROKERS_FILE", "refuses them together"}},
		} {
			row, _ := groupRow(t, requirements, tc.sections, tc.group)
			if row == nil || row.Status != tc.status {
				t.Errorf("%s: the group row is %+v, want the status %s", name, row, tc.status)
				continue
			}
			for _, want := range tc.want {
				if !strings.Contains(row.Detail, want) {
					t.Errorf("%s: the row does not name %q: %s", name, want, row.Detail)
				}
			}
			if strings.Contains(row.Detail, "CANARY") {
				t.Errorf("%s: the row shows a secret value: %s", name, row.Detail)
			}
		}
	})

	t.Run("ST-DLV-UC02-R06-T04 The environment check counts a value of a group as the base wires it: the key of the secret source in both forms, never a *_FILE value or a Secret named like the variable", func(t *testing.T) {
		requirements := groupRequirements(t)
		const brokers = "configuration: {BROKERS: kafka-0:9093}\n"
		for name, tc := range map[string]struct {
			sections string
			status   FitStatus
			notice   []string
		}{
			// The Component secret-files gives the key of the Secret as DATABASE_URL_FILE, so the Secret is the one
			// value of the pair, and a DATABASE_URL_FILE of the file is never wired: a notice, not a second value.
			"the Secret and the file form": {"secrets: {orders-database: {url: CANARY-SECRET-VALUE}}\nconfiguration: {DATABASE_URL_FILE: orders/url, BROKERS: kafka-0:9093}\n", Met,
				[]string{"configuration DATABASE_URL_FILE: the base never wires DATABASE_URL_FILE from the environment file", "the key url of the secret orders-database",
					"as DATABASE_URL, or as the file DATABASE_URL_FILE when the overlay lists the Component deploy/components/secret-files", "remove DATABASE_URL_FILE"}},
			"the file form alone": {"secrets: {orders-database: {password: CANARY-SECRET-VALUE}}\nconfiguration: {DATABASE_URL_FILE: /etc/orders/url, BROKERS: kafka-0:9093}\n", Missing,
				[]string{"configuration DATABASE_URL_FILE: the base never wires"}},
			"the file form as a Secret": {"secrets: {orders-database: {url: CANARY-SECRET-VALUE}, DATABASE_URL_FILE: {DATABASE_URL_FILE: CANARY-SECRET-VALUE}}\n" + brokers, Met,
				[]string{"secret DATABASE_URL_FILE: the base never wires"}},
			// A Secret named like a variable with a source is not its value, with any key (#375 security review L2).
			"a Secret named like the variable":         {"secrets: {orders-database: {password: CANARY-SECRET-VALUE}, DATABASE_URL: {url: CANARY-SECRET-VALUE, DATABASE_URL: CANARY-SECRET-VALUE}}\n" + brokers, Missing, nil},
			"the Secret of the source without the key": {"secrets: {orders-database: {password: CANARY-SECRET-VALUE}}\n" + brokers, Missing, nil},
		} {
			row, fit := groupRow(t, requirements, tc.sections, "database-url")
			if row == nil || row.Status != tc.status {
				t.Errorf("%s: the group row is %+v, want the status %s", name, row, tc.status)
				continue
			}
			// A value that the base never wires does not fail the check, even of a wrong type.
			if tc.status == Met && !fit.Met() {
				t.Errorf("%s: the check fails: %+v", name, fit.Rows)
			}
			notices := strings.Join(fit.Notices, "\n")
			for _, want := range tc.notice {
				if !strings.Contains(notices, want) {
					t.Errorf("%s: the notices do not say %q:\n%s", name, want, notices)
				}
			}
			if len(tc.notice) == 0 && strings.Contains(notices, "never wires") {
				t.Errorf("%s: the check gives a notice of a value that the file does not give:\n%s", name, notices)
			}
			if strings.Contains(notices+row.Detail, "CANARY") {
				t.Errorf("%s: the check shows a secret value: %s\n%s", name, row.Detail, notices)
			}
		}
		// A secret variable with no source never has a value: the base writes no entry for it and no envFrom, so a
		// Secret of its name, with any key or none, does not reach the service (#378 security review L2). The group
		// row and a notice say so; the other variable of the group still gives the group its value.
		components := groupComponents()
		components[0].Variables[0].SecretSource = nil
		files, err := WriteRequirements("orders", components)
		if err != nil {
			t.Fatal(err)
		}
		unsourced, err := LoadRequirements(files.Requirements)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"{DATABASE_URL: CANARY-SECRET-VALUE}", "{url: CANARY-SECRET-VALUE}", "{}", "{DATABASE_URL: \"\"}"} {
			row, fit := groupRow(t, unsourced, "secrets: {orders-database: {url: CANARY-SECRET-VALUE}, DATABASE_URL: "+secret+"}\n", "database-url")
			if row == nil || row.Status != Missing || !strings.Contains(row.Detail, "none for DATABASE_URL, which the base does not wire because it has no secret source") {
				t.Errorf("the Secret DATABASE_URL %s: the group row is %+v, want missing with the reason", secret, row)
			}
			notices := strings.Join(fit.Notices, "\n")
			if !strings.Contains(notices, "secret DATABASE_URL: the base wires no value for the secret environment variable DATABASE_URL: it has no secret source, and the base writes no envFrom") ||
				!strings.Contains(notices, "state a secret source for it in the component that reads it (store)") {
				t.Errorf("the Secret DATABASE_URL %s: the notices do not say that the base does not wire it:\n%s", secret, notices)
			}
			if strings.Contains(notices, "CANARY") || slices.Contains(fit.Unused, "secret DATABASE_URL") {
				t.Errorf("the Secret DATABASE_URL %s: the check shows a secret value or lists the Secret as unused: %v %v", secret, fit.Notices, fit.Unused)
			}
		}
		row, fit := groupRow(t, unsourced, "configuration: {DATABASE_URL_FILE: /etc/orders/database-url}\n", "database-url")
		if row == nil || row.Status != Met || strings.Contains(strings.Join(fit.Notices, "\n"), "no secret source") {
			t.Errorf("DATABASE_URL_FILE does not give the group its value, or the check gives a notice of a Secret that the file does not give: %+v %v", row, fit.Notices)
		}
	})
}

// groupRequirements are the platform requirements of the service orders with the components of groupComponents and
// the pair BROKERS and BROKERS_FILE of the group brokers, which has no secret source.
func groupRequirements(t *testing.T) Requirements {
	t.Helper()
	components := groupComponents()
	components[1].Variables = []Variable{
		{Name: "BROKERS", Type: "list", Description: "The brokers.", OneOf: "brokers"},
		{Name: "BROKERS_FILE", Type: "path", Description: "The file of the brokers.", OneOf: "brokers"},
	}
	files, err := WriteRequirements("orders", components)
	if err != nil {
		t.Fatal(err)
	}
	requirements, err := LoadRequirements(files.Requirements)
	if err != nil {
		t.Fatal(err)
	}
	return requirements
}

// groupRow checks an environment file of the service orders with the sections and gives the row of the group, or nil,
// and the fit.
func groupRow(t *testing.T, requirements Requirements, sections, group string) (*FitRow, EnvironmentFit) {
	t.Helper()
	head := "version: 1\nservice: orders\nenvironment: dev\nimage: quay.io/example/orders@sha256:" + strings.Repeat("ab", 32) + "\n" +
		"destination: {cluster: https://api.example.com:6443, namespace: orders-dev, overlay_path: overlays/dev}\n"
	fit, err := CheckEnvironment(requirements, []byte(head+sections))
	if err != nil {
		t.Fatalf("%s: %v", sections, err)
	}
	for i := range fit.Rows {
		if fit.Rows[i].Requirement == "group "+group {
			return &fit.Rows[i], fit
		}
	}
	return nil, fit
}
