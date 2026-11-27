package declaration

import (
	"fmt"
	"slices"
	"strings"

	"github.com/910-software-solutions/stego/internal/types"
)

// maxEventFields is the most fields of the event fields of one entity (ST-DECL-INV105).
const maxEventFields = 64

// eventsMixin is the mixin that publishes the change events of the entities (ST-SUP-RC20-OC40).
const eventsMixin = "kafka-events"

// validateEvents enforces ST-DECL-INV105 (decision ST-SUP-Q184 (c)): the event fields of an entity name one to 64
// declared fields of the entity, each once and none secret, with a reason for each pii field and no other reason, a
// version from 1 or none, and only in a declaration whose mixins publish change events. Each fault names the entity
// and the field, and never a reason.
func validateEvents(service *types.ServiceDeclaration) []ValidationError {
	var faults []ValidationError
	publishes := slices.Contains(service.Mixins, eventsMixin)
	for i, entity := range service.Entities {
		events := entity.Events
		if events == nil {
			continue
		}
		path := fmt.Sprintf("entities[%d].events", i)
		fault := func(at, found, why string) {
			faults = append(faults, ValidationError{Category: "entity", Rule: "event-fields", Path: path + at, Found: found,
				Message: fmt.Sprintf("the event fields of the entity %q: %s (ST-DECL-INV105)", entity.Name, why)})
		}
		if !publishes {
			fault("", entity.Name, "the declaration publishes no change events; add the mixin "+eventsMixin+", or remove events")
		}
		if n := len(events.Fields); n < 1 || n > maxEventFields {
			fault(".fields", fmt.Sprint(n), fmt.Sprintf("events names %d fields; name one to %d", n, maxEventFields))
		}
		if events.Version != nil && *events.Version < 1 {
			fault(".version", fmt.Sprint(*events.Version), "the version is a whole number from 1")
		}
		byName := map[string]types.Field{}
		for _, f := range entity.Fields {
			byName[f.Name] = f
		}
		seen := map[string]bool{}
		for j, name := range events.Fields {
			at := fmt.Sprintf(".fields[%d]", j)
			f, ok := byName[name]
			switch {
			case !ok:
				fault(at, name, fmt.Sprintf("the entity declares no field %q", name))
			case seen[name]:
				fault(at, name, fmt.Sprintf("the field %q is named twice", name))
			case f.Secret():
				fault(at, name, fmt.Sprintf("the field %q is secret, and no secret leaves the service in an event", name))
			case f.Sensitivity == "pii" && strings.TrimSpace(events.PIIReasons[name]) == "":
				fault(at, name, fmt.Sprintf("the field %q is pii; give the reason why it leaves the service in pii_reasons", name))
			}
			seen[name] = true
		}
		for _, name := range sortedKeys(events.PIIReasons) {
			if f, ok := byName[name]; !ok || !seen[name] || f.Sensitivity != "pii" {
				fault(".pii_reasons", name, fmt.Sprintf("pii_reasons gives a reason for %q, which is not a pii field of the event fields", name))
			}
		}
	}
	return faults
}
