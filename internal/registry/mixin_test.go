package registry

import (
	"slices"
	"strings"
	"testing"
)

// inv04Files are the parts of ST-SUP-TD01 that a mixin test reads: the archetype svc-arch with api-a, which offers
// the slot before_create, store-a, which answers store-port, and auth-a; the component extra-a; and extra-mixin,
// which adds extra-a and the slot after_extra.
func inv04Files() map[string]string {
	component := func(name, namespace, provides, requires, slots string) string {
		sides := ""
		for _, list := range []struct{ names, side string }{{requires, "outbound"}, {provides, "inbound"}} {
			for _, port := range strings.Split(list.names, ",") {
				if port = strings.TrimSpace(port); port != "" {
					sides += "\n    " + port + ": " + list.side
				}
			}
		}
		if sides == "" {
			sides = " {}"
		}
		return "kind: component\nenvironment: []\nname: " + name + "\nversion: 1.0.0\nstage: ga\noutput_namespace: " + namespace +
			"\nrequires: [" + requires + "]\nprovides: [" + provides + "]\nslots: " + slots +
			"\narchitecture:\n  layer: handler\n  flow: Serves the test.\n  ports:" + sides + "\n"
	}
	slot := func(name string) string {
		return "\n  - name: " + name + "\n    phase: before_store\n    operations: [create]\n    proto: stego.slots." + name + "\n"
	}
	proto := func(service string) string {
		return "syntax = \"proto3\";\n\npackage stego.slots;\n\nservice " + service + " {}\n"
	}
	return map[string]string{
		"components/api-a/component.yaml":            component("api-a", "internal/api", "", "store-port, auth-port", slot("before_create")),
		"components/api-a/slots/before_create.proto": proto("BeforeCreate"),
		"components/store-a/component.yaml":          component("store-a", "internal/store", "store-port", "", "[]"),
		"components/store-b/component.yaml":          component("store-b", "internal/store-b", "store-port", "", "[]"),
		"components/auth-a/component.yaml":           component("auth-a", "internal/auth", "auth-port", "", "[]"),
		"components/extra-a/component.yaml":          component("extra-a", "internal/extra", "", "", "[]"),
		"components/twin/component.yaml":             component("twin", "internal/twin", "", "", slot("before_create")),
		"components/twin/slots/before_create.proto":  proto("Other"),
		"components/twin-b/component.yaml":           component("twin-b", "internal/twin-b", "", "", slot("after_twin")),
		"components/twin-b/slots/after_twin.proto":   proto("OtherTwin"),
		"archetypes/svc-arch/archetype.yaml": "kind: archetype\nruntime: {module: github.com/910-software-solutions/stego/registry/runtime, version: v0.1.0, sum: 'h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA='}\n" +
			"name: svc-arch\nlanguage: go\nversion: 1.0.0\ncomponents: [api-a, store-a, auth-a]\nconventions:\n  layout: flat\n" +
			"compatible_mixins: [extra-mixin]\nbindings:\n  store-port: store-a\n  auth-port: auth-a\n",
		"mixins/extra-mixin/mixin.yaml": "kind: mixin\nname: extra-mixin\nversion: 1.0.0\nadds_components: [extra-a]\nadds_slots:" + slot("after_extra") +
			"overrides: none\n",
		"mixins/extra-mixin/slots/after_extra.proto": proto("AfterExtra"),
	}
}

func TestMixinAddsAndNeverReplaces(t *testing.T) {
	t.Run("ST-SUP-INV04-T A mixin that replaces a part of its archetype is refused, and a mixin that only adds is accepted", func(t *testing.T) {
		// extend loads the registry with one more mixin, which svc-arch names as compatible, and gives the refusal.
		extend := func(name, body string, files map[string]string) error {
			all := inv04Files()
			all["archetypes/svc-arch/archetype.yaml"] = strings.Replace(all["archetypes/svc-arch/archetype.yaml"], "[extra-mixin]", "[extra-mixin, "+name+"]", 1)
			all["mixins/"+name+"/mixin.yaml"] = "kind: mixin\nname: " + name + "\nversion: 1.0.0\n" + body
			for path, data := range files {
				all["mixins/"+name+"/"+path] = data
			}
			_, err := LoadDirectories(writeRegistryFolder(t, all))
			return err
		}
		for _, tc := range []struct {
			name, body string
			files      map[string]string
			part       string
		}{
			// store-b provides store-port, which store-a answers in svc-arch: the mixin would put a second provider in
			// place of the one the archetype chose.
			{"other-store", "adds_components: [store-b]\nadds_slots: []\noverrides: none\n", nil, "store-port"},
			{"again-slot", "adds_components: []\nadds_slots:\n  - name: before_create\n    phase: before_store\n    operations: [create]\n    proto: stego.slots.before_create\noverrides: none\n",
				map[string]string{"slots/before_create.proto": "syntax = \"proto3\";\n\npackage stego.slots;\n\nservice BeforeCreate {}\n"}, "before_create"},
			{"override-all", "adds_components: []\nadds_slots: []\noverrides: all\n", nil, "overrides"},
			// twin offers before_create, which api-a of svc-arch offers: two contracts for one slot (sec-500 M1).
			{"twin-mixin", "adds_components: [twin]\nadds_slots: []\noverrides: none\n", nil, "before_create"},
			// twin offers before_create, and the mixin adds before_create as a slot of its own too.
			{"self-twin", "adds_components: [twin-b]\nadds_slots:\n  - name: after_twin\n    phase: after_store\n    operations: [create]\n    proto: stego.slots.after_twin\noverrides: none\n",
				map[string]string{"slots/after_twin.proto": "syntax = \"proto3\";\n\npackage stego.slots;\n\nservice AfterTwin {}\n"}, "after_twin"},
			// A mixin has no field for a binding or a convention, so the decoder refuses each by its path.
			{"bind-store", "adds_components: []\nadds_slots: []\nbindings:\n  store-port: extra-a\noverrides: none\n", nil, "bindings"},
			{"change-layout", "adds_components: []\nadds_slots: []\nconventions:\n  layout: nested\noverrides: none\n", nil, "conventions"},
		} {
			err := extend(tc.name, tc.body, tc.files)
			if err == nil {
				t.Fatalf("the mixin %s, which replaces %s, was accepted", tc.name, tc.part)
			}
			for _, word := range []string{tc.name, tc.part} {
				if !strings.Contains(err.Error(), word) {
					t.Fatalf("the refusal of %s does not name %q: %v", tc.name, word, err)
				}
			}
			if tc.part != "bindings" && tc.part != "conventions" && !strings.Contains(err.Error(), "svc-arch") {
				t.Fatalf("the refusal of %s does not name svc-arch: %v", tc.name, err)
			}
		}
		// An archetype that binds a port to extra-a, which only extra-mixin adds, is accepted: the binding names the
		// component that the mixin adds (sec-500 I2).
		bound := inv04Files()
		bound["components/extra-a/component.yaml"] = strings.Replace(strings.Replace(bound["components/extra-a/component.yaml"], "provides: []", "provides: [extra-port]", 1), "ports: {}", "ports:\n    extra-port: inbound", 1)
		bound["archetypes/svc-arch/archetype.yaml"] = strings.Replace(bound["archetypes/svc-arch/archetype.yaml"], "  auth-port: auth-a\n", "  auth-port: auth-a\n  extra-port: extra-a\n", 1)
		if _, err := LoadDirectories(writeRegistryFolder(t, bound)); err != nil {
			t.Fatalf("an archetype that binds extra-port to extra-a, which extra-mixin adds, was refused: %v", err)
		}
		// A mixin that names a component the archetype already holds adds nothing, and replaces nothing.
		if err := extend("same-store", "adds_components: [store-a]\nadds_slots: []\noverrides: none\n", nil); err != nil {
			t.Fatalf("a mixin that names store-a, which svc-arch holds, was refused: %v", err)
		}
		reg, err := LoadDirectories(writeRegistryFolder(t, inv04Files()))
		if err != nil {
			t.Fatal(err)
		}
		archetype, mixin := reg.Archetype("svc-arch"), reg.Mixin("extra-mixin")
		if err := reg.CheckExtension(archetype, mixin); err != nil {
			t.Fatalf("extra-mixin, which only adds, was refused: %v", err)
		}
		// The parts of svc-arch with extra-mixin are the parts without it, and only extra-a and after_extra besides.
		before := slices.Clone(archetype.Components)
		with := append(slices.Clone(before), mixin.AddsComponents...)
		if !slices.Equal(with, []string{"api-a", "store-a", "auth-a", "extra-a"}) || len(mixin.AddsSlots) != 1 || mixin.AddsSlots[0].Name != "after_extra" {
			t.Fatalf("svc-arch with extra-mixin holds %v and the slots %v", with, mixin.AddsSlots)
		}
	})
}
