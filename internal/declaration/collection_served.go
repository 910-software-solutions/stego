package declaration

import (
	"fmt"

	"github.com/910-software-solutions/stego/internal/types"
)

// CollectionServer is the one component that serves declared collections today (ST-SUP-RC20), and CollectionPort the
// port that it provides, by which checking finds a component that serves collections.
const (
	CollectionServer = "rest-api"
	CollectionPort   = "http-server"
)

// validateCollectionsServed refuses each declared collection of a service whose active component set has no
// component that serves collections (ST-DECL-INV92). Such a collection would pass checking and get no route, so
// the fault tells the author to create and change rows through commands and read them through queries.
func validateCollectionsServed(service *types.ServiceDeclaration, facts Facts) []ValidationError {
	if service == nil || !facts.CollectionsUnserved {
		return nil
	}
	var faults []ValidationError
	for _, collection := range service.Collections {
		faults = append(faults, ValidationError{Category: "collection", Rule: "collection-served", Path: collectionPath(collection.Name), Found: collection.Name,
			Message: fmt.Sprintf("collection %q is declared, but no component of the archetype %s serves collections (only %s does); a service with no %s creates and changes rows through commands and reads them through queries, so remove the collection and declare a command and a query", collection.Name, service.Archetype, CollectionServer, CollectionServer)})
	}
	return faults
}
