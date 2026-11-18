package registry

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// envComponent is the YAML of a component that declares two variables: an optional duration with a range and a
// default, and a secret one. The test cases change one fact of it.
const envComponent = `kind: component
name: env-a
version: 1.0.0
stage: ga
config:
  issuer: {type: string}
  key: {type: string, secret: true}
  mode: {type: string, enum: [FAST, SLOW]}
requires: []
provides: []
slots: []
architecture: {layer: handler, flow: "Serves the test.", ports: {}}
environment:
  - name: ENV_A_TIMEOUT
    type: duration
    required: false
    default: "5m"
    description: "The longest time of a call. When it has no value, it is 5 minutes."
    secret: false
    example: "2m"
    allowed: {min: "1s", max: "30m"}
  - name: ENV_A_DATABASE_URL
    type: string
    required: false
    description: "The connection string of the database."
    secret: true
`

// parseEnv parses envComponent with one text replaced, and fails the test when the text is not in it.
func parseEnv(t *testing.T, old, replacement string) (*Component, error) {
	t.Helper()
	if !strings.Contains(envComponent, old) {
		t.Fatalf("the fixture holds no %q", old)
	}
	return ParseComponentFromBytes([]byte(strings.Replace(envComponent, old, replacement, 1)), "env-a/component.yaml")
}

func TestVariableType(t *testing.T) {
	t.Run("ST-VO63-C01-T An environment variable type is accepted only when it is one of the nine known types", func(t *testing.T) {
		for _, known := range []string{"string", "integer", "number", "duration", "boolean", "url", "path", "address", "list"} {
			if _, err := ParseVariableType(known); err != nil {
				t.Errorf("the type %s is refused: %v", known, err)
			}
		}
		for _, unknown := range []string{"", "int", "String", "secret", "duration "} {
			if _, err := ParseVariableType(unknown); err == nil {
				t.Errorf("the type %q is accepted", unknown)
			}
		}
	})
}

func TestVariableDescription(t *testing.T) {
	t.Run("ST-VO64-C01-T A description is accepted only as one or two sentences of at most 200 characters on one line", func(t *testing.T) {
		for _, good := range []string{"The port.", "The address 127.0.0.1:9090 of the server. When it has no value, the server uses it.", strings.Repeat("a", 199) + "."} {
			if _, err := ParseVariableDescription(good); err != nil {
				t.Errorf("the description %q is refused: %v", good, err)
			}
		}
		for _, bad := range []string{"", " ", "The port", "One. Two. Three.", "The port.\nIt is a number.", strings.Repeat("a", 200) + ".", " The port.", "The\tport.", "See https://example.com for it.", "See www.example.com for it."} {
			if _, err := ParseVariableDescription(bad); err == nil {
				t.Errorf("the description %q is accepted", bad)
			}
		}
	})
}

// refusal is one case of a component that the registry refuses, with the variable and the fact it must name.
type refusal struct {
	name, old, replacement, variable, fact string
}

// expectRefused fails the test unless the registry refuses each case and names the component, the variable and the
// fact, and it never shows the canary text.
func expectRefused(t *testing.T, cases []refusal) {
	t.Helper()
	for _, tc := range cases {
		_, err := parseEnv(t, tc.old, tc.replacement)
		if err == nil {
			t.Errorf("%s: the component is accepted", tc.name)
			continue
		}
		for _, part := range []string{"env-a", tc.variable, tc.fact} {
			if !strings.Contains(err.Error(), part) {
				t.Errorf("%s: the refusal does not name %s: %v", tc.name, part, err)
			}
		}
		if strings.Contains(err.Error(), "CANARY") {
			t.Errorf("%s: the refusal shows the canary text: %v", tc.name, err)
		}
	}
}

func TestEnvironmentDeclaration(t *testing.T) {
	if _, err := parseEnv(t, "", ""); err != nil {
		t.Fatalf("the fixture is refused: %v", err)
	}
	t.Run("ST-SUP-INV22-T A component states its environment list, and each variable its name, type, required flag, description and secret flag", func(t *testing.T) {
		expectRefused(t, []refusal{
			{"no name", "  - name: ENV_A_TIMEOUT\n", "  - name: \"\"\n", "", "name"},
			{"a lower-case name", "  - name: ENV_A_TIMEOUT\n", "  - name: env_a_timeout\n", "env_a_timeout", "name"},
			{"no type", "    type: duration\n", "", "ENV_A_TIMEOUT", "type"},
			{"an unknown type", "    type: duration\n", "    type: seconds\n", "ENV_A_TIMEOUT", "type"},
			{"no required flag", "    required: false\n    default: \"5m\"\n", "    default: \"5m\"\n", "ENV_A_TIMEOUT", "required"},
			{"no secret flag", "    secret: false\n    example", "    example", "ENV_A_TIMEOUT", "secret"},
			{"no description", "    description: \"The longest time of a call. When it has no value, it is 5 minutes.\"\n", "", "ENV_A_TIMEOUT", "description"},
			{"a description of 201 characters", "The longest time of a call. When it has no value, it is 5 minutes.", strings.Repeat("a", 200) + ".", "ENV_A_TIMEOUT", "description"},
			{"a description with a line break", "\"The longest time of a call. When it has no value, it is 5 minutes.\"", "\"The longest time.\\nIt is long.\"", "ENV_A_TIMEOUT", "description"},
			{"a variable twice", "  - name: ENV_A_DATABASE_URL\n", "  - name: ENV_A_TIMEOUT\n", "ENV_A_TIMEOUT", "name"},
			{"a placeholder of a setting with no enum", "  - name: ENV_A_TIMEOUT\n", "  - name: \"ENV_{settings.issuer}\"\n", "", "name"},
		})
		head, _, _ := strings.Cut(envComponent, "environment:\n")
		if _, err := ParseComponentFromBytes([]byte(head), "env-a/component.yaml"); err == nil || !strings.Contains(err.Error(), "env-a: environment:") {
			t.Errorf("a component with no environment list is accepted, or the refusal does not name the list: %v", err)
		}
		component, err := parseEnv(t, "  - name: ENV_A_TIMEOUT\n", "  - name: \"ENV_{settings.mode}_TIMEOUT\"\n")
		if err != nil || len(component.Variables()) != 2 {
			t.Fatalf("a name from a setting of names is refused: %v", err)
		}
	})
	t.Run("ST-SUP-INV23-T A secret variable or setting states no default and no example, and a name holds only a setting of a closed list of names", func(t *testing.T) {
		expectRefused(t, []refusal{
			{"a secret with a default", "    secret: true\n", "    secret: true\n    default: CANARY-SECRET-VALUE\n", "ENV_A_DATABASE_URL", "default"},
			{"a secret with an example", "    secret: true\n", "    secret: true\n    example: CANARY-SECRET-VALUE\n", "ENV_A_DATABASE_URL", "example"},
			{"a secret setting with a default", "key: {type: string, secret: true}", "key: {type: string, secret: true, default: CANARY-SECRET-VALUE}", "setting key", "default"},
			{"a default from a secret setting", "    default: \"5m\"\n", "    default: \"{settings.key}\"\n", "ENV_A_TIMEOUT", "default"},
			{"a variable name from a secret setting", "  - name: ENV_A_TIMEOUT\n", "  - name: \"ENV_{settings.key}\"\n", "", "name"},
		})
		component := `kind: component
name: env-a
version: 1.0.0
stage: ga
config:
  key: {type: string, secret: true}
requires: []
provides: []
slots: []
architecture: {layer: handler, flow: "Serves the test.", ports: {}}
environment: []
needs:
  - {kind: topic, name: "{settings.key}-events"}
`
		_, err := ParseComponentFromBytes([]byte(component), "env-a/component.yaml")
		if err == nil || !strings.Contains(err.Error(), "secret setting key") {
			t.Fatalf("a need name that holds a secret setting is accepted, or the refusal does not name it: %v", err)
		}
		// A need name holds only a text setting whose enum allows DNS labels alone (decision on ST-SUP-Q153).
		const secretSetting = "key: {type: string, secret: true}"
		accepted := strings.Replace(component, secretSetting, "key: {type: string, enum: [orders, stock-2]}", 1)
		if _, err := ParseComponentFromBytes([]byte(accepted), "env-a/component.yaml"); err != nil {
			t.Fatalf("a need name from a setting of DNS labels is refused: %v", err)
		}
		for _, c := range []struct{ name, setting string }{
			{"free text", "key: {type: string}"},
			{"a number", "key: {type: integer, enum: [1, 2]}"},
			{"an upper-case value", "key: {type: string, enum: [orders, Stock]}"},
			{"a value with '_'", "key: {type: string, enum: [stock_items]}"},
			{"a value that ends with '-'", "key: {type: string, enum: [stock-]}"},
			{"a URL", "key: {type: string, enum: [\"https://issuer.example.com\"]}"},
			{"a value longer than 63 characters", "key: {type: string, enum: [" + strings.Repeat("a", 64) + "]}"},
		} {
			_, err := ParseComponentFromBytes([]byte(strings.Replace(component, secretSetting, c.setting, 1)), "env-a/component.yaml")
			if err == nil || !strings.Contains(err.Error(), "setting key") || !strings.Contains(err.Error(), "DNS labels") {
				t.Errorf("a need name from a setting of %s is accepted, or the refusal does not name the setting and the rule: %v", c.name, err)
			}
		}
		// A resource name of a cluster permission holds a setting by the same rule (#404 security review L5), so a
		// declaration cannot name the object of another service.
		const topicNeed = `{kind: topic, name: "{settings.key}-events"}`
		const permissionNeed = `{kind: cluster permission, name: "{service}-read", resource: secrets, verbs: [get], resource_names: ["{settings.key}-tls"]}`
		permission := strings.Replace(component, topicNeed, permissionNeed, 1)
		enumSetting := strings.Replace(permission, secretSetting, "key: {type: string, enum: [orders, stock-2]}", 1)
		if _, err := ParseComponentFromBytes([]byte(enumSetting), "env-a/component.yaml"); err != nil {
			t.Fatalf("a resource name from a setting of DNS labels is refused: %v", err)
		}
		for _, c := range []struct{ name, setting, want string }{
			{"a secret", secretSetting, "secret setting key"},
			{"free text", "key: {type: string}", "DNS labels"},
			{"a URL", "key: {type: string, enum: [\"https://issuer.example.com\"]}", "DNS labels"},
		} {
			_, err := ParseComponentFromBytes([]byte(strings.Replace(permission, secretSetting, c.setting, 1)), "env-a/component.yaml")
			if err == nil || !strings.Contains(err.Error(), "resource_names") || !strings.Contains(err.Error(), c.want) {
				t.Errorf("a resource name from a setting of %s is accepted, or the refusal does not name the field and the rule: %v", c.name, err)
			}
		}
		other := strings.Replace(permission, `"{settings.key}-tls"`, `"{settings.other}-tls"`, 1)
		if _, err := ParseComponentFromBytes([]byte(other), "env-a/component.yaml"); err == nil || !strings.Contains(err.Error(), "resource_names") {
			t.Errorf("a resource name from a setting that the component does not state is accepted: %v", err)
		}
	})
	t.Run("ST-SUP-INV24-T A required variable states no default, and a default or an example is of its type and inside its allowed values", func(t *testing.T) {
		expectRefused(t, []refusal{
			{"a required variable with a default", "    required: false\n    default: \"5m\"\n", "    required: true\n    default: \"5m\"\n", "ENV_A_TIMEOUT", "default"},
			{"a default that is not a duration", "    default: \"5m\"\n", "    default: \"five\"\n", "ENV_A_TIMEOUT", "default"},
			{"a default above the range", "    default: \"5m\"\n", "    default: \"31m\"\n", "ENV_A_TIMEOUT", "default"},
			{"an example below the range", "    example: \"2m\"\n", "    example: \"500ms\"\n", "ENV_A_TIMEOUT", "example"},
			{"a reversed range", "{min: \"1s\", max: \"30m\"}", "{min: \"30m\", max: \"1s\"}", "ENV_A_TIMEOUT", "allowed"},
			{"a range and a list", "{min: \"1s\", max: \"30m\"}", "{min: \"1s\", max: \"30m\", values: [\"5m\"]}", "ENV_A_TIMEOUT", "allowed"},
			{"a default outside the list", "    type: duration\n    required: false\n    default: \"5m\"\n", "    type: string\n    required: false\n    default: \"fast\"\n", "ENV_A_TIMEOUT", "allowed"},
			{"a default of a setting the component has not", "    default: \"5m\"\n", "    default: \"{settings.other}\"\n", "ENV_A_TIMEOUT", "default"},
		})
		// A default holds no credential and no unsafe number (#353 security review L1 and I2).
		block := "    type: duration\n    required: false\n    default: \"5m\"\n    description: \"The longest time of a call. When it has no value, it is 5 minutes.\"\n    secret: false\n    example: \"2m\"\n    allowed: {min: \"1s\", max: \"30m\"}\n"
		for _, c := range []struct{ typ, value, want string }{
			{"url", "https://reader:CANARY@keys.example.com/certs", "user info"},
			{"number", "+Inf", "finite number"},
			{"duration", "-5m", "zero or more"},
			{"address", "CANARY@spicedb.example.com:50051", "not an address"},
		} {
			_, err := parseEnv(t, block, "    type: "+c.typ+"\n    required: false\n    default: \""+c.value+"\"\n    description: \"The value of the test.\"\n    secret: false\n")
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "ENV_A_TIMEOUT") {
				t.Errorf("the %s default %q is accepted, or the refusal does not name the variable and %q: %v", c.typ, c.value, c.want, err)
			} else if strings.Contains(err.Error(), "CANARY") {
				t.Errorf("the refusal shows the default: %v", err)
			}
		}
	})
	t.Run("ST-SUP-UC01-R09-T01 The registry refuses a component that leaves out a fact, or states a default or an example for a secret or a default for a required variable, and names the component, the variable and the fact", func(t *testing.T) {
		expectRefused(t, []refusal{
			{"a left-out description", "    description: \"The connection string of the database.\"\n", "", "ENV_A_DATABASE_URL", "description"},
			{"a left-out type", "    type: string\n    required: false\n    description: \"The connection", "    required: false\n    description: \"The connection", "ENV_A_DATABASE_URL", "type"},
			{"a default for a secret", "    secret: true\n", "    secret: true\n    default: CANARY-SECRET-VALUE\n", "ENV_A_DATABASE_URL", "default"},
			{"an example for a secret", "    secret: true\n", "    secret: true\n    example: CANARY-SECRET-VALUE\n", "ENV_A_DATABASE_URL", "example"},
			{"a default for a required variable", "    required: false\n    default: \"5m\"\n", "    required: true\n    default: \"5m\"\n", "ENV_A_TIMEOUT", "default"},
			{"a name that holds a secret setting", "  - name: ENV_A_TIMEOUT\n", "  - name: \"ENV_{settings.key}\"\n", "", "name"},
			{"a configuration need", "environment:\n", "needs:\n  - {kind: configuration, name: ENV_A_TIMEOUT}\nenvironment:\n", "", "configuration"},
		})
	})
}

func TestSecretSource(t *testing.T) {
	t.Run("ST-SUP-UC01-R09-T02 The registry refuses a secret source on a variable that is not secret or a path, of a secret setting or a bad key, or of a need that no component states", func(t *testing.T) {
		source := "    secret: true\n    secret_source: {name: \"{service}-database\", key: url}\n"
		if _, err := parseEnv(t, "    secret: true\n", source); err != nil {
			t.Fatalf("a secret source on a secret variable is refused: %v", err)
		}
		expectRefused(t, []refusal{
			{"a source on a duration", "    secret: false\n    example: \"2m\"\n", "    secret: false\n    secret_source: {name: \"{service}-database\", key: url}\n    example: \"2m\"\n", "ENV_A_TIMEOUT", "secret_source"},
			{"a source of a secret setting", "    secret: true\n", "    secret: true\n    secret_source: {name: \"{settings.key}\", key: url}\n", "ENV_A_DATABASE_URL", "secret_source"},
			{"a source with a bad key", "    secret: true\n", "    secret: true\n    secret_source: {name: \"{service}-database\", key: \"a/b\"}\n", "ENV_A_DATABASE_URL", "secret_source"},
			{"a mode that is not migrate, worker or api", "    secret: true\n", "    secret: true\n    read_by_mode: serve\n", "ENV_A_DATABASE_URL", "read_by_mode"},
			{"a required variable of one mode", "    required: false\n    description: \"The connection", "    required: true\n    read_by_mode: migrate\n    description: \"The connection", "ENV_A_DATABASE_URL", "read_by_mode"},
		})
		component, err := parseEnv(t, "    secret: true\n", source)
		if err != nil {
			t.Fatal(err)
		}
		reg := &Registry{components: map[string]*Component{"env-a": component}}
		if err := checkSecretSources(reg); err == nil || !strings.Contains(err.Error(), "{service}-database") || !strings.Contains(err.Error(), "env-a") {
			t.Errorf("a source of a need that no component states is not refused with the component and the need: %v", err)
		}
		component.Needs = []Need{{Kind: "secret", Name: "{service}-database", Keys: []string{"url"}}}
		if err := checkSecretSources(reg); err != nil {
			t.Errorf("a source of a stated need is refused: %v", err)
		}
		component.Needs[0].Keys = []string{"password"}
		if err := checkSecretSources(reg); err == nil {
			t.Error("a source of a key that the need does not state is accepted")
		}
		// Each source of the shipped registry names a need and a key that a shipped component states.
		shipped, err := Load(filepath.Join("..", "..", "registry"))
		if err != nil {
			t.Fatal(err)
		}
		if err := checkSecretSources(shipped); err != nil {
			t.Error(err)
		}
		sources := 0
		for _, component := range shipped.Components() {
			for _, variable := range component.Variables() {
				if variable.SecretSource != nil {
					sources++
				}
			}
		}
		// Only the migration job reads the credential of the owner role, so the serving Deployment never wires it.
		for _, name := range []string{"STEGO_MIGRATION_DATABASE_URL", "STEGO_MIGRATION_DATABASE_URL_FILE"} {
			found := false
			for _, variable := range shipped.Component("postgres-adapter").Variables() {
				if variable.Name == name {
					found = variable.ReadByMode == "migrate"
				}
			}
			if !found {
				t.Errorf("postgres-adapter does not state %s as read by the migrate mode", name)
			}
		}
		// The Kafka components give the SCRAM password file and the certificate authority file from the Secret
		// {service}-kafka-client, which only the worker mode reads, so only the worker Deployment mounts it
		// (walkthrough 10, N1).
		for _, component := range []string{"kafka-producer", "kafka-consumer"} {
			for name, key := range map[string]string{"STEGO_KAFKA_PASSWORD_FILE": "password", "STEGO_KAFKA_CA_FILE": "ca-crt"} {
				found := false
				for _, variable := range shipped.Component(component).Variables() {
					if variable.Name == name {
						found = variable.SecretSource != nil && variable.SecretSource.Name == "{service}-kafka-client" &&
							variable.SecretSource.Key == key && variable.ReadByMode == "worker"
					}
				}
				if !found {
					t.Errorf("%s does not state %s with the secret source {service}-kafka-client key %s, read by the worker mode", component, name, key)
				}
			}
		}
		if sources < 18 {
			t.Errorf("the shipped registry states %d secret sources, fewer than the 18 it ships", sources)
		}
	})
}

func TestShippedEnvironment(t *testing.T) {
	t.Run("ST-SUP-UC01-R08-T01 Each shipped component declares its environment list, and each variable states its facts in a valid form", func(t *testing.T) {
		reg, err := Load(filepath.Join("..", "..", "registry"))
		if err != nil {
			t.Fatal(err)
		}
		declared := 0
		for name, component := range reg.Components() {
			if component.Environment == nil {
				t.Errorf("the component %s states no environment list", name)
			}
			for _, variable := range component.Variables() {
				declared++
				if _, err := ParseVariableDescription(variable.Description); err != nil {
					t.Errorf("component %s variable %s: %v", name, variable.Name, err)
				}
				if variable.IsSecret() && (variable.Default != nil || variable.Example != nil) {
					t.Errorf("component %s: the secret variable %s states a default or an example", name, variable.Name)
				}
			}
		}
		if declared < 50 {
			t.Fatalf("the shipped components declare %d variables, fewer than the code reads", declared)
		}
	})
}

func TestOneOfGroups(t *testing.T) {
	t.Run("ST-SUP-UC01-R09-T03 The registry refuses a group of one of with one variable, a required variable or a bad name, and the shipped pairs are groups", func(t *testing.T) {
		pair := "  - name: ENV_A_DATABASE_URL\n    type: string\n    required: false\n    description: \"The connection string of the database.\"\n    secret: true\n    one_of: database-url\n" +
			"  - name: ENV_A_DATABASE_URL_FILE\n    type: path\n    required: false\n    description: \"The file of the connection string.\"\n    secret: false\n    one_of: database-url\n"
		whole := "  - name: ENV_A_DATABASE_URL\n    type: string\n    required: false\n    description: \"The connection string of the database.\"\n    secret: true\n"
		component, err := parseEnv(t, whole, pair)
		if err != nil {
			t.Fatalf("a group of two variables is refused: %v", err)
		}
		if got := component.Variables()[1].OneOf; got != "database-url" {
			t.Errorf("the group of ENV_A_DATABASE_URL is %q, want database-url", got)
		}
		expectRefused(t, []refusal{
			{"a group of one variable", "    secret: true\n", "    secret: true\n    one_of: database-url\n", "ENV_A_DATABASE_URL", "one_of"},
			{"a required variable in a group", whole, strings.Replace(pair, "    required: false\n    description: \"The file", "    required: true\n    description: \"The file", 1), "ENV_A_DATABASE_URL_FILE", "one_of"},
			{"a group name with a capital letter", whole, strings.ReplaceAll(pair, "one_of: database-url", "one_of: Database_URL"), "ENV_A_DATABASE_URL", "one_of"},
		})
		_, err = parseEnv(t, "    secret: true\n", "    secret: true\n    one_of: database-url\n")
		if err == nil || !strings.Contains(err.Error(), "at least two variables") {
			t.Errorf("the refusal of a group of one variable does not say that a group has at least two: %v", err)
		}
		// Each pair of the shipped registry whose process needs one of the two is a group of two.
		shipped, err := Load(filepath.Join("..", "..", "registry"))
		if err != nil {
			t.Fatal(err)
		}
		want := map[string][]string{
			"postgres-adapter/database-url":           {"DATABASE_URL", "DATABASE_URL_FILE"},
			"postgres-adapter/migration-database-url": {"STEGO_MIGRATION_DATABASE_URL", "STEGO_MIGRATION_DATABASE_URL_FILE"},
			"postgres-adapter/worker-database-url":    {"STEGO_WORKER_DATABASE_URL", "STEGO_WORKER_DATABASE_URL_FILE"},
			"postgres-adapter/relay-database-url":     {"STEGO_RELAY_DATABASE_URL", "STEGO_RELAY_DATABASE_URL_FILE"},
			"kafka-producer/kafka-brokers":            {"STEGO_KAFKA_BROKERS", "STEGO_KAFKA_BROKERS_FILE"},
			"kafka-producer/kafka-topic":              {"STEGO_KAFKA_TOPIC", "STEGO_KAFKA_TOPIC_FILE"},
			"kafka-consumer/kafka-brokers":            {"STEGO_KAFKA_BROKERS", "STEGO_KAFKA_BROKERS_FILE"},
			"kafka-consumer/kafka-consumer-topic":     {"STEGO_KAFKA_CONSUMER_TOPIC", "STEGO_KAFKA_CONSUMER_TOPIC_FILE"},
			// The mode migrate of browser-backend reads the same owner connection as postgres-adapter (ST-SUP-RC02-OC56).
			"browser-backend/migration-database-url": {"STEGO_MIGRATION_DATABASE_URL", "STEGO_MIGRATION_DATABASE_URL_FILE"},
		}
		got := map[string][]string{}
		for _, component := range shipped.Components() {
			for _, variable := range component.Variables() {
				if variable.OneOf != "" {
					got[component.Name+"/"+variable.OneOf] = append(got[component.Name+"/"+variable.OneOf], variable.Name)
				}
			}
		}
		for key, names := range want {
			if !slices.Equal(got[key], names) {
				t.Errorf("the shipped group %s holds %v, want %v", key, got[key], names)
			}
		}
		if len(got) != len(want) {
			t.Errorf("the shipped registry states the groups %v, want only %v", got, want)
		}
	})
}

func TestNeedVariable(t *testing.T) {
	t.Run("ST-SUP-UC01-R09-T04 The registry refuses a need variable or value on the wrong kind, of a variable that the component does not state or that is secret, or that is not one text setting", func(t *testing.T) {
		component := `kind: component
name: env-a
version: 1.0.0
stage: ga
config:
  issuer: {type: string}
  key: {type: string, secret: true}
  count: {type: integer}
requires: []
provides: []
slots: []
architecture: {layer: handler, flow: "Serves the test.", ports: {}}
environment:
  - name: ENV_A_ISSUER
    type: url
    required: false
    description: "The issuer of the tokens."
    secret: false
  - name: ENV_A_TOKEN
    type: string
    required: false
    description: "The token of the test."
    secret: true
needs:
  - {kind: identity issuer, name: "{service}-issuer", variable: ENV_A_ISSUER, value: "{settings.issuer}"}
`
		parsed, err := ParseComponentFromBytes([]byte(component), "env-a/component.yaml")
		if err != nil {
			t.Fatalf("an issuer need with a variable and a value is refused: %v", err)
		}
		if need := parsed.Needs[0]; need.Variable != "ENV_A_ISSUER" || need.Value != "{settings.issuer}" {
			t.Fatalf("the need holds the variable %q and the value %q", need.Variable, need.Value)
		}
		const need = `  - {kind: identity issuer, name: "{service}-issuer", variable: ENV_A_ISSUER, value: "{settings.issuer}"}`
		for _, c := range []struct{ name, need, want string }{
			{"a variable on an egress", `  - {kind: egress, name: "{service}-keys", port: 443, variable: ENV_A_ISSUER}`, "the egress need has no field variable"},
			{"a value on a topic", `  - {kind: topic, name: "{service}-events", value: "{settings.issuer}"}`, "the topic need has no field value"},
			{"a variable the component does not state", strings.Replace(need, "variable: ENV_A_ISSUER", "variable: ENV_A_OTHER", 1), "not an environment variable of the component"},
			{"a secret variable", strings.Replace(need, "variable: ENV_A_ISSUER", "variable: ENV_A_TOKEN", 1), "is secret"},
			{"a fixed value", strings.Replace(need, `value: "{settings.issuer}"`, `value: "https://sso.example.com"`, 1), "not one {settings.<key>}"},
			{"a secret setting", strings.Replace(need, "{settings.issuer}", "{settings.key}", 1), "not one {settings.<key>}"},
			{"a setting that is not text", strings.Replace(need, "{settings.issuer}", "{settings.count}", 1), "not one {settings.<key>}"},
			{"a setting the component has not", strings.Replace(need, "{settings.issuer}", "{settings.gone}", 1), "not one {settings.<key>}"},
		} {
			_, err := ParseComponentFromBytes([]byte(strings.Replace(component, need, c.need, 1)), "env-a/component.yaml")
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "env-a") {
				t.Errorf("%s: got %v, want a refusal that names the component and says %q", c.name, err, c.want)
			}
		}
	})
}

func TestTopicAccess(t *testing.T) {
	t.Run("ST-SUP-RC38-OC02-T02 Each shipped topic need gives its access, and a need with a wrong access is refused", func(t *testing.T) {
		reg, err := Load("../../registry")
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"kafka-consumer": "read", "kafka-producer": "write"}
		for name, c := range reg.Components() {
			for _, need := range c.Needs {
				if need.Kind == "topic" && need.Access != want[name] {
					t.Errorf("component %s: the topic need %s has the access %q, want %q", name, need.Name, need.Access, want[name])
				}
			}
		}
		for _, bad := range []Need{{Kind: "topic", Name: "{service}-events", Access: "all"}, {Kind: "egress", Name: "{service}-kafka", Port: 9093, Access: "read"}} {
			component := &Component{Name: "env-a", Needs: []Need{bad}}
			if err := checkNeeds(component); err == nil || !strings.Contains(err.Error(), "access") {
				t.Errorf("the need %+v gave %v, want a refusal of its access", bad, err)
			}
		}
	})
}
