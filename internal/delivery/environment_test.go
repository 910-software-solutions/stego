package delivery

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// These tests prove the environment check of Delivering (ST-DLV-DS02, ST-DLV-SP01) on the files of ST-DLV-TD02:
// the platform requirements that stego apply writes for the service of ST-DLV-TD01 and an environment file that
// meets them.

// environmentFixture reads the platform requirements and the environment file of ST-DLV-TD02.
func environmentFixture(t *testing.T) (Requirements, []byte) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "environment", "platform-requirements.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	requirements, err := LoadRequirements(data)
	if err != nil {
		t.Fatal(err)
	}
	environment, err := os.ReadFile(filepath.Join("testdata", "environment", "dev.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return requirements, environment
}

// withoutEntry gives the environment file without the entry of the section with the name.
func withoutEntry(t *testing.T, data []byte, section, name string) []byte {
	t.Helper()
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	top := document.Content[0]
	for i := 0; i < len(top.Content); i += 2 {
		if top.Content[i].Value != section {
			continue
		}
		entries := top.Content[i+1]
		for j := 0; j < len(entries.Content); j += 2 {
			if entries.Content[j].Value == name {
				entries.Content = append(entries.Content[:j], entries.Content[j+2:]...)
				out, err := yaml.Marshal(&document)
				if err != nil {
					t.Fatal(err)
				}
				return out
			}
		}
	}
	t.Fatalf("the environment file has no entry %s in %s", name, section)
	return nil
}

// sectionOf gives the section of the environment file for a requirement kind.
func sectionOf(t *testing.T, kind RequirementKind) string {
	t.Helper()
	for _, section := range environmentSections {
		if section.kind == kind {
			return section.name
		}
	}
	t.Fatalf("no section for the kind %s", kind)
	return ""
}

// unmetRows gives the rows of the fit that are not met.
func unmetRows(fit EnvironmentFit) []FitRow {
	return UnmetRequirements(NewEnvironmentReader(fit))
}

// unmetRequirementRows gives the unmet rows of the fit without the rows of the groups of one of, which
// ST-DLV-UC02-R06-T03 proves: a missing secret also leaves a group with no value when it is the source of a variable
// of the group.
func unmetRequirementRows(fit EnvironmentFit) []FitRow {
	var rows []FitRow
	for _, row := range unmetRows(fit) {
		if !strings.HasPrefix(row.Requirement, "group ") {
			rows = append(rows, row)
		}
	}
	return rows
}

// replaceOnce gives the text with old replaced by new, and fails the test unless old is in it once.
func replaceOnce(t *testing.T, data []byte, old, new string) []byte {
	t.Helper()
	if strings.Count(string(data), old) != 1 {
		t.Fatalf("the environment file does not hold %q once", old)
	}
	return []byte(strings.Replace(string(data), old, new, 1))
}

func TestCheckEnvironment(t *testing.T) {
	t.Run("ST-DLV-DS02-T01 An environment file is accepted only when it meets each requirement, and each unmet one is named", func(t *testing.T) {
		requirements, data := environmentFixture(t)
		fit, err := CheckEnvironment(requirements, data)
		if err != nil {
			t.Fatal(err)
		}
		if !fit.Met() || len(unmetRows(fit)) != 0 {
			t.Fatalf("the check refused the environment file of the test data set: %+v", fit.Rows)
		}
		if groups := len(Groups(requirements.Environment)); len(fit.Rows) != len(requirements.Requirements)+groups {
			t.Fatalf("the fit holds %d rows, want one for each of the %d requirements and of the %d groups of one of", len(fit.Rows), len(requirements.Requirements), groups)
		}
		for _, requirement := range requirements.Requirements {
			changed := withoutEntry(t, data, sectionOf(t, requirement.Kind), requirement.Name)
			fit, err := CheckEnvironment(requirements, changed)
			if err != nil {
				t.Fatalf("without %s: %v", requirement.Name, err)
			}
			unmet := unmetRequirementRows(fit)
			want := string(requirement.Kind) + " " + requirement.Name
			if fit.Met() || len(unmet) != 1 || unmet[0].Requirement != want || unmet[0].Status != Missing {
				t.Errorf("without the entry %s the check gives %+v, want only %s as missing", requirement.Name, unmet, want)
			}
		}
	})

	t.Run("ST-DLV-DS02-T02 An environment file with a value of the wrong type is refused", func(t *testing.T) {
		requirements, data := environmentFixture(t)
		cases := []struct {
			name, row, value, typ string
			change                func([]byte) []byte
		}{
			{"the image named by the tag latest", "image", "quay.io/example/orders:latest", "sha256 digest", func(data []byte) []byte {
				return replaceOnce(t, data, "image: quay.io/example/orders@sha256:3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a", "image: quay.io/example/orders:latest")
			}},
			{"the egress port as the text https", "egress payments", `"https"`, "port number", func(data []byte) []byte {
				return replaceOnce(t, data, "    port: 443\n", "    port: https\n")
			}},
			{"the secret orders-database as a configuration value", "secret orders-database", "configuration", "secret", func(data []byte) []byte {
				data = withoutEntry(t, data, "secrets", "orders-database")
				return replaceOnce(t, data, "    LOG_LEVEL: info\n", "    LOG_LEVEL: info\n    orders-database: CANARY-SECRET-VALUE\n")
			}},
		}
		for _, c := range cases {
			fit, err := CheckEnvironment(requirements, c.change(data))
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			unmet := unmetRequirementRows(fit)
			if fit.Met() || len(unmet) != 1 || unmet[0].Requirement != c.row || unmet[0].Status != WrongType {
				t.Errorf("with %s the check gives %+v, want only %s as wrong type", c.name, unmet, c.row)
				continue
			}
			for _, part := range []string{c.value, c.typ} {
				if !strings.Contains(unmet[0].Detail, part) {
					t.Errorf("with %s the refusal %q does not name %s", c.name, unmet[0].Detail, part)
				}
			}
			if strings.Contains(unmet[0].Detail, "CANARY") {
				t.Errorf("with %s the refusal shows a secret value: %q", c.name, unmet[0].Detail)
			}
		}
	})

	t.Run("ST-DLV-DS02-T03 A configuration value that hides a credential or an unsafe number, and a range bound that does not parse, are refused", func(t *testing.T) {
		cases := []struct {
			variable    AcceptedVariable
			value, want string
		}{
			{AcceptedVariable{Name: "JWK_CERT_URL", Type: "url"}, "https://reader:CANARY@keys.example.com/certs", "user info"},
			{AcceptedVariable{Name: "JWK_CERT_URL", Type: "url"}, "https://CANARY@keys.example.com/certs", "user info"},
			{AcceptedVariable{Name: "SAMPLE_RATIO", Type: "number"}, "+Inf", "not a finite number"},
			{AcceptedVariable{Name: "SAMPLE_RATIO", Type: "number"}, "-Inf", "not a finite number"},
			{AcceptedVariable{Name: "SAMPLE_RATIO", Type: "number", Allowed: "0 to 1"}, "Inf", "not a finite number"},
			{AcceptedVariable{Name: "RETRY_WAIT", Type: "duration"}, "-5s", "zero or more"},
			{AcceptedVariable{Name: "RETRY_WAIT", Type: "duration", Allowed: "at most 30s"}, "-5s", "zero or more"},
			{AcceptedVariable{Name: "STEGO_SPICEDB_ENDPOINT", Type: "address"}, "CANARY@spicedb.example.com:50051", "not an address"},
			{AcceptedVariable{Name: "GOMAXPROCS", Type: "integer", Allowed: "1 to many"}, "4", `range bound "many"`},
			{AcceptedVariable{Name: "GOMAXPROCS", Type: "integer", Allowed: "at least one"}, "4", `range bound "one"`},
			{AcceptedVariable{Name: "RETRY_WAIT", Type: "duration", Allowed: "at most soon"}, "5s", `range bound "soon"`},
			{AcceptedVariable{Name: "SAMPLE_RATIO", Type: "number", Allowed: "0 to NaN"}, "0.5", `range bound "NaN"`},
		}
		for _, c := range cases {
			fault := valueFault(c.variable, c.value)
			if !strings.Contains(fault, c.want) || !strings.Contains(fault, c.variable.Name) {
				t.Errorf("%s=%q (allowed %q) gives %q, want a refusal that names %s and %q", c.variable.Name, c.value, c.variable.Allowed, fault, c.variable.Name, c.want)
			}
			if strings.Contains(fault, "CANARY") {
				t.Errorf("the refusal of %s shows the value: %q", c.variable.Name, fault)
			}
		}
		// Values of the type inside a range that parses pass.
		for _, c := range []struct {
			variable AcceptedVariable
			value    string
		}{
			{AcceptedVariable{Name: "JWK_CERT_URL", Type: "url"}, "https://keys.example.com/certs"},
			{AcceptedVariable{Name: "SAMPLE_RATIO", Type: "number", Allowed: "0 to 1"}, "0.5"},
			{AcceptedVariable{Name: "RETRY_WAIT", Type: "duration", Allowed: "at most 30s"}, "0s"},
			{AcceptedVariable{Name: "STEGO_SPICEDB_ENDPOINT", Type: "address"}, "spicedb.example.com:50051"},
			{AcceptedVariable{Name: "GOMAXPROCS", Type: "integer", Allowed: "1 to 1024"}, "4"},
		} {
			if fault := valueFault(c.variable, c.value); fault != "" {
				t.Errorf("%s=%q (allowed %q) is refused: %s", c.variable.Name, c.value, c.variable.Allowed, fault)
			}
		}
	})
}

// environmentWith gives an environment file of the service orders with the sections given.
func environmentWith(t *testing.T, sections string) environment {
	t.Helper()
	env, err := readEnvironment([]byte(`version: 1
service: orders
environment: dev
image: quay.io/example/orders@sha256:3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a
destination: {cluster: https://api.dev.example.com:6443, namespace: orders-dev, overlay_path: overlays/dev}
` + sections))
	if err != nil {
		t.Fatalf("%v\n%s", err, sections)
	}
	return env
}

func TestRequirementMet(t *testing.T) {
	t.Run("ST-DLV-SP01-T A requirement is met only when the environment file supplies its name, its kind and every part of its shape", func(t *testing.T) {
		secret := Requirement{Kind: Secret, Name: "orders-database", Keys: []string{"password", "url"}, Components: []string{"postgres-adapter"}}
		egress := Requirement{Kind: Egress, Name: "payments", Port: 443, Components: []string{"rh-sso-auth"}}
		cases := []struct {
			name        string
			requirement Requirement
			sections    string
			status      FitStatus
			part        string
		}{
			{"(a) the secret with both keys", secret, "secrets: {orders-database: {url: a, password: b}}\n", Met, ""},
			{"(b) the secret with the key url only", secret, "secrets: {orders-database: {url: a}}\n", Missing, "password"},
			{"(c) both keys under the name orders-db", secret, "secrets: {orders-db: {url: a, password: b}}\n", Missing, "orders-database"},
			{"(d) a configuration value named orders-database", secret, "configuration: {orders-database: a}\n", WrongType, "secret"},
			{"(e) the egress payments with an address and a port", egress, "egresses: {payments: {address: payments.example.com, port: 443}}\n", Met, ""},
			{"(f) the egress payments with an address and no port", egress, "egresses: {payments: {address: payments.example.com}}\n", Missing, "port"},
		}
		for _, c := range cases {
			status, detail := requirementMet(c.requirement, environmentWith(t, c.sections))
			if status != c.status {
				t.Errorf("%s: the requirement is %s (%s), want %s", c.name, status, detail, c.status)
				continue
			}
			if c.status == Met && detail != "" {
				t.Errorf("%s: a met requirement has the detail %q", c.name, detail)
			}
			if !strings.Contains(detail, c.part) {
				t.Errorf("%s: the detail %q does not name %s", c.name, detail, c.part)
			}
		}
	})
}

func TestNeedValues(t *testing.T) {
	t.Run("ST-DLV-UC02-R02-T08 The check refuses a topic or an issuer that differs from the variable that carries it or from the issuer that the declaration fixes", func(t *testing.T) {
		requirementsData, err := os.ReadFile(filepath.Join("testdata", "environment", "platform-requirements.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		// The topic need of kafka-producer names STEGO_KAFKA_TOPIC; the issuer need of rh-sso-auth names
		// STEGO_AUTH_ISSUER and holds the issuer that the declaration fixes (ST-DLV-Q26 (A)).
		text := strings.Replace(string(requirementsData), "    name: order-events\n    components:", "    name: order-events\n    variable: STEGO_KAFKA_TOPIC\n    components:", 1)
		text = strings.Replace(text, "    name: sso\n    components:", "    name: sso\n    variable: STEGO_AUTH_ISSUER\n    value: https://sso.dev.example.com/auth/realms/dev\n    components:", 1)
		requirements, err := LoadRequirements([]byte(text))
		if err != nil {
			t.Fatalf("the requirements with a variable and a value do not load: %v", err)
		}
		_, data := environmentFixture(t)
		check := func(name string, file []byte) []FitRow {
			t.Helper()
			fit, err := CheckEnvironment(requirements, file)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			return unmetRows(fit)
		}
		if unmet := check("the fixture", data); len(unmet) != 0 {
			t.Fatalf("the environment file whose values agree is refused: %+v", unmet)
		}
		withIssuer := replaceOnce(t, data, "configuration:\n", "configuration:\n  STEGO_AUTH_ISSUER: https://sso.dev.example.com/auth/realms/dev\n")
		if unmet := check("an issuer variable of the same value", withIssuer); len(unmet) != 0 {
			t.Fatalf("an issuer variable of the same value is refused: %+v", unmet)
		}
		for _, c := range []struct {
			name, requirement, want string
			file                    []byte
		}{
			{"another topic in the variable", "topic order-events", "the configuration value of STEGO_KAFKA_TOPIC",
				replaceOnce(t, data, "  STEGO_KAFKA_TOPIC: dev.orders.order-events\n", "  STEGO_KAFKA_TOPIC: dev.orders.other\n")},
			{"another topic in the need", "topic order-events", "the configuration value of STEGO_KAFKA_TOPIC",
				replaceOnce(t, data, "    topic: dev.orders.order-events\n", "    topic: dev.orders.other\n")},
			{"another issuer than the declaration fixes", "identity issuer sso", "the declaration fixes the issuer https://sso.dev.example.com/auth/realms/dev",
				replaceOnce(t, data, "    issuer: https://sso.dev.example.com/auth/realms/dev\n", "    issuer: https://sso.other.example.com/auth/realms/dev\n")},
			{"another issuer in the variable", "identity issuer sso", "the configuration value of STEGO_AUTH_ISSUER",
				replaceOnce(t, withIssuer, "  STEGO_AUTH_ISSUER: https://sso.dev.example.com/auth/realms/dev\n", "  STEGO_AUTH_ISSUER: https://sso.other.example.com/auth/realms/dev\n")},
		} {
			unmet := check(c.name, c.file)
			if len(unmet) != 1 || unmet[0].Requirement != c.requirement || unmet[0].Status != WrongType || !strings.Contains(unmet[0].Detail, c.want) {
				t.Errorf("%s: the check gives %+v, want %s of the wrong type with %q", c.name, unmet, c.requirement, c.want)
			}
		}
		// The *_FILE form is not compared, because the check reads no file.
		fileForm := replaceOnce(t, data, "  STEGO_KAFKA_TOPIC: dev.orders.order-events\n", "  STEGO_KAFKA_TOPIC_FILE: /etc/stego/kafka/topic\n")
		if unmet := check("the file form", fileForm); len(unmet) != 0 {
			t.Errorf("the file form of the topic variable is compared: %+v", unmet)
		}
	})
}
