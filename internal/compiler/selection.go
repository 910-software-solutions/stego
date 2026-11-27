package compiler

import (
	"fmt"
	"time"

	"github.com/910-software-solutions/stego/internal/declaration"
	"github.com/910-software-solutions/stego/internal/gen"
	"github.com/910-software-solutions/stego/internal/registry"
	"github.com/910-software-solutions/stego/internal/types"
)

// selection is one infrastructure component that a declared setting implies (ST-GEN-INV37), with the setting that
// selects it.
type selection struct {
	Component string
	Setting   string
}

// The infrastructure components that the declared settings select (ST-GEN-INV37).
const (
	auditComponent       = "audit-log"
	idempotencyComponent = "idempotency"
	ledgerComponent      = "ledger"
	// kafkaConsumerComponent is the component of a declared subscription (ST-SUP-RC38).
	kafkaConsumerComponent = "kafka-consumer"
)

// selectComponents gives the infrastructure components that the declared settings of svc imply, in a fixed order:
// a stated tenancy that is not single selects tenancy, a stated audit that is on selects audit-log, a stated
// authorization selects spicedb-authz, a stated idempotency selects idempotency, a declared worker selects worker, a
// declared subscription selects kafka-consumer, and an entity of kind ledger selects ledger (ST-GEN-INV37). A setting that the declaration omits selects nothing, so a declaration that relies
// on the defaults keeps the components of its archetype and mixins.
func selectComponents(svc *types.ServiceDeclaration) []selection {
	var selected []selection
	if t := svc.Tenancy; t != nil && t.Kind != "single" {
		selected = append(selected, selection{Component: gen.TenancyComponent, Setting: "tenancy"})
	}
	if a := svc.Audit; a != nil && (a.Enabled == nil || *a.Enabled) {
		selected = append(selected, selection{Component: auditComponent, Setting: "audit"})
	}
	if svc.Authorization != nil {
		selected = append(selected, selection{Component: authorizationComponent, Setting: "authorization"})
	}
	if svc.Idempotency != nil {
		selected = append(selected, selection{Component: idempotencyComponent, Setting: "idempotency"})
	}
	if len(svc.Workers) > 0 {
		// A declared worker selects the worker component, which generates its runner (ST-SUP-RC35, ST-SUP-Q131).
		selected = append(selected, selection{Component: workerComponent, Setting: "workers"})
	}
	if len(svc.Subscriptions) > 0 {
		// A declared subscription selects the kafka-consumer component, whose Kafka inbox reads its topic
		// (ST-SUP-RC38, decision on ST-SUP-Q158).
		selected = append(selected, selection{Component: kafkaConsumerComponent, Setting: "subscriptions"})
	}
	for _, entity := range svc.Entities {
		if entity.Kind == "ledger" {
			selected = append(selected, selection{Component: ledgerComponent, Setting: "entities." + entity.Name + ".kind"})
			break
		}
	}
	return selected
}

// acceptanceClock is the clock that the expiry of an acceptance reads; tests set a fixed time.
var acceptanceClock = time.Now

// acceptanceDate is the form of the date of an acceptance (ST-DECL-EN07).
const acceptanceDate = "2006-01-02"

// acceptanceFault is the one rule of an experimental acceptance (ST-DECL-EN07): a component whose stage is not ga
// reaches a service only when the declaration accepts it by name, with an acceptance whose date is a date, has not
// run out, which it holds to the end of its date in UTC, and is not later than the sunset of the component
// (ST-DECL-INV15, which Declaring holds in declaration.AcceptanceOutlivesSunset; the compiler gives it the sunset of
// the component from the registry). It gives nil for a ga component and for an acceptance that holds, and else the fault: at path when
// the declaration does not accept the component, and at experimental_acceptances when its acceptance does not hold.
// reached says how the component reaches the service, in the words of the message, such as "the tenancy setting
// selects the component tenancy".
func acceptanceFault(svc *types.ServiceDeclaration, name string, component *registry.Component, path, reached string) *ValidationError {
	if component == nil || registry.FitForGoldenPath(component) {
		return nil
	}
	today := acceptanceClock().UTC().Format(acceptanceDate)
	add := fmt.Sprintf("add an experimental acceptance: experimental_acceptances: [{component: %s, accepted_until: <a date no later than %s>, reason: <why the service accepts it>}]", name, component.Sunset)
	var accepted *types.ExperimentalAcceptance
	for i := range svc.ExperimentalAcceptances {
		if svc.ExperimentalAcceptances[i].Component == name {
			accepted = &svc.ExperimentalAcceptances[i]
		}
	}
	switch {
	case accepted == nil:
		return &ValidationError{Category: "component", Rule: "experimental-acceptance", Path: path, Found: name,
			Message: fmt.Sprintf("%s, which is %s, and the declaration does not accept it by name; %s", reached, component.Stage, add)}
	case !validDate(accepted.AcceptedUntil) || accepted.AcceptedUntil < today:
		return &ValidationError{Category: "component", Rule: "experimental-acceptance", Path: "experimental_acceptances", Found: accepted.AcceptedUntil,
			Message: fmt.Sprintf("%s, which is %s, and its experimental acceptance until %q has run out or is not a date; %s", reached, component.Stage, accepted.AcceptedUntil, add)}
	case declaration.AcceptanceOutlivesSunset(accepted.AcceptedUntil, component.Sunset):
		return &ValidationError{Category: "component", Rule: "experimental-acceptance", Path: "experimental_acceptances", Found: accepted.AcceptedUntil,
			Message: fmt.Sprintf("%s, which is %s, and its experimental acceptance until %s is later than its sunset %s (ST-DECL-INV15); %s", reached, component.Stage, accepted.AcceptedUntil, component.Sunset, add)}
	}
	return nil
}

// validateSelectedComponents enforces ST-GEN-INV37 on the components that the declared settings select and the
// registry holds: one that is not ga needs an experimental acceptance that holds (acceptanceFault). Each fault names
// the component, its stage, the setting that selects it and the acceptance to add. A selected component that the
// registry does not hold is a refusal from outside the declaration, at generation (selectedComponentMissing).
func validateSelectedComponents(svc *types.ServiceDeclaration, reg *registry.Registry) []ValidationError {
	var faults []ValidationError
	for _, s := range selectComponents(svc) {
		reached := fmt.Sprintf("the %s setting selects the component %s", s.Setting, s.Component)
		if fault := acceptanceFault(svc, s.Component, reg.Component(s.Component), s.Setting, reached); fault != nil {
			faults = append(faults, *fault)
		}
	}
	return faults
}

// validateAcceptedComponents enforces ST-SUP-UC04-R02 on the active component set: a component whose stage is not
// ga reaches a service only when the declaration accepts it by name, with an acceptance that holds
// (acceptanceFault). The components of an archetype and of a mixin are ga (ST-SUP-SP01), and
// validateSelectedComponents holds a component that a setting selects, so this holds the rest: a component that a
// port override brings into the set. The registry refuses an archetype whose default_auth is not ga when it loads
// (checkGoldenPaths). Each fault names the component, its stage, what brings it and the acceptance to add.
func validateAcceptedComponents(svc *types.ServiceDeclaration, active map[string]*registry.Component) []ValidationError {
	selected := map[string]bool{}
	for _, s := range selectComponents(svc) {
		selected[s.Component] = true
	}
	var faults []ValidationError
	for _, name := range sortedKeys(active) {
		if selected[name] {
			continue
		}
		path, reached := "overrides", "the component "+name+" is in the active set"
		for _, port := range sortedKeys(svc.Overrides) {
			if bound, ok := svc.Overrides[port].(string); ok && bound == name {
				path, reached = "overrides."+port, "the override of "+port+" brings the component "+name
				break
			}
		}
		if fault := acceptanceFault(svc, name, active[name], path, reached); fault != nil {
			faults = append(faults, *fault)
		}
	}
	return faults
}

// validatePipelineComponent holds the ci-pipeline component to the acceptance rule when the declaration names a CI
// provider, because the run then reads the pipeline pins of the component, which is not part of the active set
// (sec-501 L1).
func validatePipelineComponent(svc *types.ServiceDeclaration, reg *registry.Registry) []ValidationError {
	if svc.Delivery == nil || svc.Delivery.CI == "" {
		return nil
	}
	reached := fmt.Sprintf("the delivery.ci setting %s reads the pipeline pins of the component %s", svc.Delivery.CI, registry.PipelineComponent)
	if fault := acceptanceFault(svc, registry.PipelineComponent, reg.Component(registry.PipelineComponent), "delivery.ci", reached); fault != nil {
		return []ValidationError{*fault}
	}
	return nil
}

func validDate(text string) bool {
	_, err := time.Parse(acceptanceDate, text)
	return err == nil
}

// selectedComponentMissing refuses generation of a declaration whose settings select a component that the registry
// does not hold (ST-GEN-INV37, ST-DECL-UC03-R03): the refusal is of the registry, not of the declaration, so
// checking accepts the declaration.
func selectedComponentMissing(svc *types.ServiceDeclaration, reg *registry.Registry) error {
	for _, s := range selectComponents(svc) {
		if reg.Component(s.Component) == nil {
			return fmt.Errorf("generation refused: the %s setting selects the component %s, which the registry does not hold; generation runs once the registry holds %s", s.Setting, s.Component, s.Component)
		}
	}
	return nil
}
