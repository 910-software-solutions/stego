package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/declaration"
)

// experimentComponent is a component of the stage given, with a sunset date, that provides its own port.
func experimentComponent(name, stage string) string {
	return "kind: component\nenvironment: []\nname: " + name + "\nversion: 0.1.0\nstage: " + stage + "\nsunset: " + daysFromToday(90) +
		"\noutput_namespace: internal/" + name + "\nrequires: []\nprovides: [" + name + "-port]\nslots: []\n" +
		"architecture:\n  layer: handler\n  flow: Serves the test.\n  ports:\n    " + name + "-port: inbound\n"
}

func TestGoldenPathHoldsOnlyGA(t *testing.T) {
	t.Parallel()
	t.Run("ST-SUP-UC03-R02-T01 An archetype that includes components below ga is refused, naming each with its stage", func(t *testing.T) {
		f := newSupplyingFixture(t)
		archetype := strings.Replace(commonRegistryFiles()["archetypes/svc-arch/archetype.yaml"], "  - audit-log\n", "  - audit-log\n  - beta-x\n  - alpha-y\n", 1)
		commonSHA := commitRegistry(t, f.common, map[string]string{
			"components/beta-x/component.yaml":   experimentComponent("beta-x", "beta"),
			"components/alpha-y/component.yaml":  experimentComponent("alpha-y", "alpha"),
			"archetypes/svc-arch/archetype.yaml": archetype,
		})
		registryProject(t, f.project, "", pinned(f.common, commonSHA), pinned(f.local, f.localSHA))
		f.compose(t).refused(t, "archetype svc-arch", "beta-x (beta)", "alpha-y (alpha)")
		// The default_auth of an archetype is a part of it too (sec-501 L2).
		g := newSupplyingFixture(t)
		withAuth := strings.Replace(commonRegistryFiles()["archetypes/svc-arch/archetype.yaml"], "components:\n", "default_auth: beta-x\ncomponents:\n", 1)
		commonSHA = commitRegistry(t, g.common, map[string]string{
			"components/beta-x/component.yaml":   experimentComponent("beta-x", "beta"),
			"archetypes/svc-arch/archetype.yaml": withAuth,
		})
		registryProject(t, g.project, "", pinned(g.common, commonSHA), pinned(g.local, g.localSHA))
		g.compose(t).refused(t, "archetype svc-arch", "beta-x (beta)")
	})

	t.Run("ST-SUP-UC03-R02-T02 A mixin that brings a component below ga is refused, naming the component and its stage", func(t *testing.T) {
		f := newSupplyingFixture(t)
		commonSHA := commitRegistry(t, f.common, map[string]string{
			"components/beta-x/component.yaml": experimentComponent("beta-x", "beta"),
			"mixins/beta-mixin/mixin.yaml":     "kind: mixin\nname: beta-mixin\nversion: 1.0.0\nadds_components: [beta-x]\nadds_slots: []\noverrides: none\n",
		})
		registryProject(t, f.project, "", pinned(f.common, commonSHA), pinned(f.local, f.localSHA))
		f.compose(t).refused(t, "mixin beta-mixin", "beta-x (beta)")
	})
}

// acceptanceFaults runs stego validate --json on a declaration of svc-arch in the project of ST-SUP-TD02 that brings
// local-a (beta) in by the override local-port, with the acceptances given as YAML. It gives the faults of the rule
// experimental-acceptance, and fails the test on a fault of another rule than generator: no component of ST-SUP-TD01
// has a generator in the stego binary, so checking names each, and that is no fault of the acceptance.
func acceptanceFaults(t *testing.T, acceptances string) []string {
	t.Helper()
	f := newSupplyingFixture(t)
	putFile(t, f.project, "service.yaml", "kind: service\nname: project\narchetype: svc-arch\nlanguage: go\noverrides:\n  local-port: local-a\n"+acceptances)
	run := f.runStegoIn(t, f.project, []string{"STEGO_MODULE=example.com/project", "STEGO_GO_VERSION=1.26.11"}, "validate", "--json")
	var document declaration.FaultsDocument
	if err := json.Unmarshal([]byte(run.stdout), &document); err != nil {
		t.Fatalf("stego validate --json printed no document: %v\n%s%s", err, run.stdout, run.stderr)
	}
	var faults []string
	for _, fault := range document.Faults {
		switch fault.Rule {
		case "experimental-acceptance":
			faults = append(faults, fault.Path+": "+fault.Message)
		case "generator":
		default:
			t.Fatalf("stego validate gave a fault of the rule %s: %s", fault.Rule, fault.Message)
		}
	}
	return faults
}

func TestExperimentReachableOnlyOnPurpose(t *testing.T) {
	t.Parallel()
	t.Run("ST-SUP-UC04-R02-T01 A declaration that uses a beta component without accepting it by name is refused", func(t *testing.T) {
		for name, acceptances := range map[string]string{
			"no acceptance": "",
			"an acceptance of another component only": "experimental_acceptances:\n  - {component: beta-y, accepted_until: " + daysFromToday(30) + ", reason: pilot}\n",
		} {
			faults := acceptanceFaults(t, acceptances)
			if len(faults) != 1 || !strings.Contains(faults[0], "overrides.local-port") || !strings.Contains(faults[0], "local-a, which is beta") || !strings.Contains(faults[0], "does not accept it by name") {
				t.Fatalf("%s: want one fault that names local-a, its stage and that the declaration does not accept it, got %v", name, faults)
			}
		}
		faults := acceptanceFaults(t, "experimental_acceptances:\n  - {component: local-a, accepted_until: "+daysFromToday(-1)+", reason: pilot}\n")
		if len(faults) != 1 || !strings.Contains(faults[0], "has run out") {
			t.Fatalf("an acceptance of local-a that ran out yesterday: want one fault, got %v", faults)
		}
	})

	t.Run("ST-SUP-UC04-R02-T02 A declaration that accepts a beta component by name may use it", func(t *testing.T) {
		if faults := acceptanceFaults(t, "experimental_acceptances:\n  - {component: local-a, accepted_until: "+daysFromToday(30)+", reason: the pilot of the local store}\n"); len(faults) != 0 {
			t.Fatalf("the declaration that accepts local-a was refused: %v", faults)
		}
	})
}
