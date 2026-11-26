package declaration

import (
	"fmt"
	"strings"

	"github.com/910-software-solutions/stego/internal/types"
)

// validateNeededModes checks that the declared modes hold every mode that the declared features need
// (ST-DECL-INV89): api for collections, commands and queries, which the service serves over HTTP or gRPC; worker and
// controller for a service on the runtime data path, whose outbox relay and audit sink run in the worker mode and
// whose audit anchor and maintenance jobs, and the reconcile of the worker tuples, run in the controller mode. A
// process may still run a subset with the mode option or STEGO_MODE. Omitted modes are all three, and a declared
// worker (ST-DECL-INV71) and a backfill (ST-DECL-INV61) have their own faults. STEGO adds no mode itself.
func validateNeededModes(service *types.ServiceDeclaration, facts Facts) []ValidationError {
	if service == nil || len(service.Modes) == 0 {
		return nil
	}
	needs := map[string][]string{}
	if len(service.Collections) > 0 {
		needs["api"] = append(needs["api"], "the collections "+strings.Join(collectionNames(service.Collections), ", "))
	}
	if len(service.Commands) > 0 {
		needs["api"] = append(needs["api"], "the commands "+strings.Join(commandNames(service.Commands), ", "))
	}
	if len(service.Queries) > 0 {
		needs["api"] = append(needs["api"], "the queries "+strings.Join(queryNames(service.Queries), ", "))
	}
	if facts.RuntimeData {
		needs["worker"] = append(needs["worker"], "the outbox relay and the audit sink of the runtime data path")
		controller := "the audit anchor and maintenance jobs of the runtime data path"
		if len(service.Workers) > 0 && service.Authorization != nil {
			controller += " and the reconcile of the worker tuples"
		}
		needs["controller"] = append(needs["controller"], controller)
	}
	if len(service.Subscriptions) > 0 {
		// The Kafka inbox of a subscription runs in the worker mode (ST-SUP-RC38-OC01).
		needs["worker"] = append(needs["worker"], "the Kafka inbox of the subscription "+service.Subscriptions[0].Name)
	}
	var faults []ValidationError
	for _, mode := range knownModes {
		if len(needs[mode]) == 0 || RunsMode(service, mode) {
			continue
		}
		faults = append(faults, ValidationError{Category: "service", Rule: "mode-needed", Path: "modes", Found: strings.Join(service.Modes, ", "), Allowed: knownModes,
			Message: fmt.Sprintf("the modes %s do not hold %s, which %s need: the modes state which modes the service has, and a process may still run a subset with --mode or STEGO_MODE; add %s to modes, or leave modes out to have all three",
				strings.Join(service.Modes, ", "), mode, strings.Join(needs[mode], " and "), mode)})
	}
	return faults
}

func collectionNames(collections []types.Collection) []string {
	names := make([]string, len(collections))
	for i, c := range collections {
		names[i] = c.Name
	}
	return names
}

func commandNames(commands []types.Command) []string {
	names := make([]string, len(commands))
	for i, c := range commands {
		names[i] = c.Name
	}
	return names
}

func queryNames(queries []types.Query) []string {
	names := make([]string, len(queries))
	for i, q := range queries {
		names[i] = q.Name
	}
	return names
}
