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
