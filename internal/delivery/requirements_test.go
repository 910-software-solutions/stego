package delivery

import (
	"strconv"
	"strings"
	"testing"
)

func TestRequirementKind(t *testing.T) {
	t.Run("ST-VO53-C01-T A requirement kind is accepted only when it is one of the nine known kinds", func(t *testing.T) {
		for _, kind := range []string{"secret", "configuration", "backing store", "topic", "identity issuer", "ingress", "egress", "cluster permission", "pod security"} {
			got, err := ParseRequirementKind(kind)
			if err != nil || string(got) != kind {
				t.Errorf("the kind %q is refused: %v", kind, err)
			}
		}
		for _, kind := range []string{"", "database", "Secret", "secret ", "volume"} {
			if _, err := ParseRequirementKind(kind); err == nil {
				t.Errorf("the kind %q is accepted", kind)
			} else if !strings.Contains(err.Error(), strconv.Quote(kind)) {
				t.Errorf("the refusal of %q does not name the kind: %v", kind, err)
			}
		}
	})
}

func TestKubernetesNeedNames(t *testing.T) {
	t.Run("ST-DLV-UC01-R01-T03 A need name or a resource name that is not a Kubernetes name is refused at the requirements stage with a clear message", func(t *testing.T) {
		for _, need := range []Need{
			{Kind: "secret", Name: "{service}-Database", Keys: []string{"url"}},
			{Kind: "secret", Name: "{service}_database", Keys: []string{"url"}},
			{Kind: "secret", Name: "{service}-{settings.store}", Keys: []string{"url"}},
			{Kind: "backing store", Name: "{service}-db-", Engine: "postgres"},
			{Kind: "cluster permission", Name: "{service}-maps", Resource: "configmaps", Verbs: []string{"get"}, ResourceNames: []string{"{service}_Settings"}},
		} {
			component := ComponentNeeds{Component: "store", Settings: map[string]string{"store": "MAIN"}, Needs: []Need{need}}
			_, err := DeriveRequirements("orders", []ComponentNeeds{component})
			if err == nil {
				t.Errorf("the need %+v is accepted", need)
				continue
			}
			for _, want := range []string{"component store", "which a Kubernetes name refuses", "lower case"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal of %+v does not say %q: %v", need, want, err)
				}
			}
		}
		// A lower-case need is accepted, and a required variable keeps its configuration need of its own name.
		component := ComponentNeeds{Component: "store", Needs: []Need{{Kind: "secret", Name: "{service}-database", Keys: []string{"url"}}},
			Variables: []Variable{{Name: "STEGO_STORE_MODE", Type: "string", Required: true, Description: "The mode."}}}
		files, err := WriteRequirements("orders", []ComponentNeeds{component})
		if err != nil {
			t.Fatalf("a lower-case need is refused: %v", err)
		}
		if _, err := LoadRequirements(files.Requirements); err != nil {
			t.Fatalf("the platform requirements with the configuration need STEGO_STORE_MODE do not load: %v", err)
		}
		// A configuration need that a component states is named by its environment variable, in capitals with '_':
		// the base never uses it as a Kubernetes name, so it is accepted, as LoadRequirements accepts it (#375 L4).
		configured := ComponentNeeds{Component: "store", Needs: []Need{{Kind: "configuration", Name: "STEGO_STORE_REGION"}}}
		written, err := WriteRequirements("orders", []ComponentNeeds{configured})
		if err != nil {
			t.Fatalf("the configuration need STEGO_STORE_REGION of a component is refused: %v", err)
		}
		if _, err := LoadRequirements(written.Requirements); err != nil || !strings.Contains(string(written.Requirements), "name: STEGO_STORE_REGION") {
			t.Fatalf("the platform requirements with the configuration need STEGO_STORE_REGION do not hold it or do not load: %v\n%s", err, written.Requirements)
		}
		changed := strings.Replace(string(files.Requirements), "name: orders-database", "name: Orders_Database", 1)
		if _, err := LoadRequirements([]byte(changed)); err == nil || !strings.Contains(err.Error(), "Orders_Database") || !strings.Contains(err.Error(), "a capital letter and '_'") {
			t.Errorf("LoadRequirements does not refuse the secret requirement Orders_Database with a clear message: %v", err)
		}
	})
}

func TestNeedNamedLikeAVariable(t *testing.T) {
	t.Run("ST-DLV-UC01-R04-T03 A need with the name of a listed environment variable is refused, naming the component, the need and the variable", func(t *testing.T) {
		// A configuration need DATABASE_URL beside the secret variable DATABASE_URL: env check would meet the need with
		// the credential given as a plain configuration value (#378 security review L1).
		for name, tc := range map[string]struct {
			component int
			need      Need
			want      []string
		}{
			"a configuration need of a secret variable": {1, Need{Kind: "configuration", Name: "DATABASE_URL"},
				[]string{"component broker", "need DATABASE_URL", "the configuration need has the name of the secret environment variable DATABASE_URL, which store reads"}},
			"a configuration need of a variable of the same component": {0, Need{Kind: "configuration", Name: "DATABASE_URL_FILE"},
				[]string{"component store", "need DATABASE_URL_FILE", "the configuration need has the name of the environment variable DATABASE_URL_FILE, which store reads"}},
			"a configuration need of an optional variable": {0, Need{Kind: "configuration", Name: "BROKERS"},
				[]string{"component store", "need BROKERS", "environment variable BROKERS, which broker reads"}},
		} {
			components := groupComponents()
			components[tc.component].Needs = append(components[tc.component].Needs, tc.need)
			_, err := DeriveRequirements("orders", components)
			if err == nil {
				t.Errorf("%s: the need is accepted", name)
				continue
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%s: the refusal does not say %q: %v", name, want, err)
				}
			}
		}
		// A required variable that is not secret keeps the configuration need of its own name, and a need of another
		// name is accepted.
		// The broker also reads the required secret variable BROKER_TOKEN, which has no configuration need: a
		// configuration requirement of its name would let env check meet it with the plaintext value (#380 security
		// review L1).
		components := groupComponents()
		components[1].Variables[0].Required = true
		components[1].Variables = append(components[1].Variables, Variable{Name: "BROKER_TOKEN", Type: "string", Required: true, Secret: true, Description: "The token of the brokers."})
		components[1].Needs = []Need{{Kind: "configuration", Name: "STEGO_BROKER_REGION"}}
		files, err := WriteRequirements("orders", components)
		if err != nil {
			t.Fatalf("the needs are refused: %v", err)
		}
		if _, err := LoadRequirements(files.Requirements); err != nil {
			t.Fatalf("the platform requirements with the configuration need BROKERS of the required variable do not load: %v", err)
		}
		// LoadRequirements refuses a requirement with the name of a listed variable, but for the configuration need
		// of a required variable that is not secret.
		for name, change := range map[string][3]string{
			"a configuration requirement of a secret variable":          {"name: STEGO_BROKER_REGION", "name: DATABASE_URL", "store"},
			"a configuration requirement of an optional variable":       {"name: STEGO_BROKER_REGION", "name: DATABASE_URL_FILE", "store"},
			"a configuration requirement of a required secret variable": {"name: STEGO_BROKER_REGION", "name: BROKER_TOKEN", "broker"},
		} {
			changed := strings.Replace(string(files.Requirements), change[0], change[1], 1)
			_, err := LoadRequirements([]byte(changed))
			variable := strings.TrimPrefix(change[1], "name: ")
			if err == nil || !strings.Contains(err.Error(), "the configuration requirement "+variable+" of the components broker has the name of the") ||
				!strings.Contains(err.Error(), "environment variable "+variable+", which "+change[2]+" reads") {
				t.Errorf("%s: LoadRequirements does not refuse it with the requirement, the components and the variable: %v", name, err)
			}
		}
	})
}

func TestRequirementsFileNames(t *testing.T) {
	t.Run("ST-DLV-UC01-R01-T04 The platform requirements load only with a service name that is a DNS label and components and readers that are plain names", func(t *testing.T) {
		component := ComponentNeeds{Component: "store", Needs: []Need{{Kind: "secret", Name: "{service}-database", Keys: []string{"url"}}},
			Variables: []Variable{{Name: "STEGO_STORE_MODE", Type: "string", Required: true, Description: "The mode."}}}
		files, err := WriteRequirements("orders", []ComponentNeeds{component})
		if err != nil {
			t.Fatal(err)
		}
		text := string(files.Requirements)
		if _, err := LoadRequirements(files.Requirements); err != nil {
			t.Fatalf("the written platform requirements do not load: %v", err)
		}
		// A reader of runtime-configuration carries its group in brackets.
		grouped := strings.Replace(text, "read_by:\n      - store", "read_by:\n      - runtime-configuration (Worker)", 1)
		if _, err := LoadRequirements([]byte(grouped)); err != nil {
			t.Fatalf("the reader runtime-configuration (Worker) is refused: %v", err)
		}
		for name, edit := range map[string][2]string{
			"a service name with a capital":         {"service: orders", "service: Orders"},
			"a service name with a terminal escape": {"service: orders", `service: "orders\e[2Kmeets"`},
			"a component with a space":              {"components:\n      - store", "components:\n      - \"store meets\""},
			"a component with a new line":           {"components:\n      - store", "components:\n      - \"store\\nmeets\""},
			"a reader with a terminal escape":       {"read_by:\n      - store", "read_by:\n      - \"store\\e[1A\""},
			"a reader of 129 letters":               {"read_by:\n      - store", "read_by:\n      - " + strings.Repeat("s", 129)},
			"a reader group with a new line":        {"read_by:\n      - store", "read_by:\n      - \"store (x\\ny)\""},
		} {
			changed := strings.Replace(text, edit[0], edit[1], 1)
			if changed == text {
				t.Fatalf("%s: the edit does not apply to the platform requirements:\n%s", name, text)
			}
			_, err := LoadRequirements([]byte(changed))
			if err == nil {
				t.Errorf("%s: LoadRequirements accepts the file", name)
				continue
			}
			if !strings.Contains(err.Error(), "not a DNS label") && !strings.Contains(err.Error(), "not a name of at most 128 letters") {
				t.Errorf("%s: the refusal does not say what the name must be: %v", name, err)
			}
		}
	})
}

func TestNeedVariable(t *testing.T) {
	t.Run("ST-DLV-UC01-R01-T05 The requirements state the variable of a topic or an issuer and the issuer that the declaration fixes, and refuse one of another kind or that no component reads", func(t *testing.T) {
		components := func() []ComponentNeeds {
			return []ComponentNeeds{{Component: "auth", Settings: map[string]string{"issuer": "https://sso.example.com/realms/main"},
				Needs: []Need{{Kind: "identity issuer", Name: "{service}-issuer", Variable: "STEGO_AUTH_ISSUER", Value: "{settings.issuer}"}},
				Variables: []Variable{
					{Name: "STEGO_AUTH_ISSUER", Type: "url", Description: "The issuer."},
					{Name: "STEGO_AUTH_KEY", Type: "string", Secret: true, Description: "The key."},
				}}}
		}
		files, err := WriteRequirements("orders", components())
		if err != nil {
			t.Fatal(err)
		}
		loaded, err := LoadRequirements(files.Requirements)
		if err != nil {
			t.Fatalf("the written requirements do not load: %v", err)
		}
		got := loaded.Requirements[0]
		if got.Variable != "STEGO_AUTH_ISSUER" || got.Value != "https://sso.example.com/realms/main" {
			t.Fatalf("the issuer requirement holds the variable %q and the value %q", got.Variable, got.Value)
		}
		// A setting that the declaration does not give fixes no value.
		unset := components()
		unset[0].Settings = map[string]string{}
		if requirements, err := DeriveRequirements("orders", unset); err != nil || requirements.Requirements[0].Value != "" {
			t.Fatalf("an issuer setting that the declaration does not give: %v, %+v", err, requirements.Requirements)
		}
		for name, c := range map[string]struct {
			change func([]ComponentNeeds)
			want   string
		}{
			"a variable on a secret need": {func(c []ComponentNeeds) {
				c[0].Needs = []Need{{Kind: "secret", Name: "db", Keys: []string{"url"}, Variable: "STEGO_AUTH_ISSUER"}}
			}, "the secret need has no field variable"},
			"a value on a topic": {func(c []ComponentNeeds) {
				c[0].Needs = []Need{{Kind: "topic", Name: "events", Value: "{settings.issuer}"}}
			}, "the topic need has no field value"},
			"a variable no component reads": {func(c []ComponentNeeds) { c[0].Needs[0].Variable = "STEGO_OTHER" }, "the variable STEGO_OTHER is not an environment variable"},
			"a secret variable":             {func(c []ComponentNeeds) { c[0].Needs[0].Variable = "STEGO_AUTH_KEY" }, "the variable STEGO_AUTH_KEY is not an environment variable"},
			"a value that is not a setting": {func(c []ComponentNeeds) { c[0].Needs[0].Value = "https://sso.example.com" }, "is not one {settings.<key>}"},
			"an issuer that is not https":   {func(c []ComponentNeeds) { c[0].Settings["issuer"] = "http://sso.example.com/realms/main" }, "is not an https URL"},
		} {
			changed := components()
			c.change(changed)
			if _, err := DeriveRequirements("orders", changed); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s: got %v, want a refusal that says %q", name, err, c.want)
			}
		}
		// LoadRequirements refuses the same faults in a file that apply did not write.
		text := string(files.Requirements)
		for name, edit := range map[string][3]string{
			"a variable that the list does not hold": {"variable: STEGO_AUTH_ISSUER", "variable: STEGO_OTHER", "not an environment variable of the list"},
			"a secret variable":                      {"variable: STEGO_AUTH_ISSUER", "variable: STEGO_AUTH_KEY", "not an environment variable of the list"},
			"a value that is not https":              {"value: https://sso.example.com/realms/main", "value: http://sso.example.com/realms/main", "not an https URL"},
			"a value on another kind":                {"kind: identity issuer", "kind: topic", "the topic need has no field value"},
		} {
			changed := strings.Replace(text, edit[0], edit[1], 1)
			if changed == text {
				t.Fatalf("%s: the edit does not apply:\n%s", name, text)
			}
			if _, err := LoadRequirements([]byte(changed)); err == nil || !strings.Contains(err.Error(), edit[2]) {
				t.Errorf("%s: LoadRequirements gives %v, want a refusal that says %q", name, err, edit[2])
			}
		}
	})
}

func TestTopicAccessRequirement(t *testing.T) {
	t.Run("ST-SUP-RC38-OC02-T03 The access of a topic need reaches the platform requirements, and a wrong one is refused", func(t *testing.T) {
		needs := []ComponentNeeds{{Component: "kafka-consumer", Needs: []Need{{Kind: "topic", Name: "{service}-subscription", Access: "read"}}},
			{Component: "kafka-producer", Needs: []Need{{Kind: "topic", Name: "{service}-events", Access: "write"}}}}
		got, err := DeriveRequirements("fulfilment", needs)
		if err != nil {
			t.Fatal(err)
		}
		access := map[string]string{}
		for _, r := range got.Requirements {
			access[r.Name] = r.Access
		}
		if access["fulfilment-subscription"] != "read" || access["fulfilment-events"] != "write" {
			t.Fatalf("the requirements give the access %v", access)
		}
		for _, bad := range []Need{{Kind: "topic", Name: "x", Access: "all"}, {Kind: "egress", Name: "y", Port: 9093, Access: "read"}} {
			if _, err := DeriveRequirements("fulfilment", []ComponentNeeds{{Component: "c", Needs: []Need{bad}}}); err == nil || !strings.Contains(err.Error(), "access") {
				t.Errorf("the need %+v gave %v, want a refusal of its access", bad, err)
			}
		}
	})
}
