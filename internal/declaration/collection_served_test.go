package declaration

import "testing"

func TestCollectionsServed(t *testing.T) {
	t.Run("ST-DECL-INV92-T A collection of a service whose active set serves no collection is refused", func(t *testing.T) {
		service, _ := td03Files(t)
		decl := readTD03(t, service)
		expectAccepted(t, validateCollectionsServed(decl, Facts{}))
		decl.Archetype = "rpc-service"
		faults := ValidateComposition(decl, Facts{CollectionsUnserved: true})
		fault := expectOneFault(t, faults, "collection-served", "todos", "rpc-service", "rest-api", "commands", "queries")
		if fault.Path != "collections.todos" {
			t.Errorf("the fault is at %s, want collections.todos", fault.Path)
		}
		decl.Collections = nil
		expectAccepted(t, validateCollectionsServed(decl, Facts{CollectionsUnserved: true}))
	})
}
