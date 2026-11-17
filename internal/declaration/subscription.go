package declaration

import (
	"fmt"
	"regexp"

	"github.com/jsell-rh/stego/internal/types"
)

// MaxSubscriptions is the most subscriptions of a service while the kafka-consumer component is alpha: its topic and
// group variables have fixed names, so one service reads one topic (decision on ST-SUP-Q158, 2026-11-17).
const MaxSubscriptions = 1

// MaxSubscriptionEvents is the most event kinds of one subscription.
const MaxSubscriptionEvents = 32

// subscriptionEvent is an event kind of a change event: the source service, the entity in lower kebab case and the
// action, joined by periods (ST-SUP-RC20-OC40).
var subscriptionEvent = regexp.MustCompile(`^([a-z][a-z0-9-]{0,62})\.([a-z][a-z0-9-]{0,62})\.(created|updated|deleted)$`)

func subscriptionPath(i int) string { return fmt.Sprintf("subscriptions[%d]", i) }

// ValidateSubscriptions checks the subscriptions of a service (ST-DECL-INV96 to ST-DECL-INV98): each name, its
// source and its events, the one worker that consumes it, and that no command or worker stages for it. It reads
// the declaration and the facts, and it returns every fault it finds.
func ValidateSubscriptions(service *types.ServiceDeclaration, facts Facts) []ValidationError {
	var faults []ValidationError
	faults = append(faults, validateSubscriptionNames(service, facts)...)
	faults = append(faults, validateSubscriptionEvents(service)...)
	faults = append(faults, validateSubscriptionConsumer(service)...)
	return faults
}

// validateSubscriptionNames checks ST-DECL-INV96: at most MaxSubscriptions subscriptions; each name in lower kebab
// case, unique, not the name of a command, a query, a worker or a controller, and not a destination that an active
// component declares or that is reserved.
func validateSubscriptionNames(service *types.ServiceDeclaration, facts Facts) []ValidationError {
	var faults []ValidationError
	if len(service.Subscriptions) > MaxSubscriptions {
		faults = append(faults, ValidationError{Category: "subscription", Rule: "subscription-count", Path: "subscriptions",
			Found:   fmt.Sprint(len(service.Subscriptions)),
			Message: fmt.Sprintf("the declaration has %d subscriptions; while the kafka-consumer component is alpha a service has at most %d, because its topic and group settings have fixed names; a later version takes more", len(service.Subscriptions), MaxSubscriptions)})
	}
	others := map[string]string{}
	for _, c := range service.Commands {
		others[c.Name] = "command"
	}
	for _, q := range service.Queries {
		others[q.Name] = "query"
	}
	for _, w := range service.Workers {
		others[w.Name] = "worker"
	}
	for _, c := range service.Controllers {
		others[c.Name] = "controller"
	}
	seen := map[string]bool{}
	for i, s := range service.Subscriptions {
		path := subscriptionPath(i) + ".name"
		fault := func(message string) {
			faults = append(faults, ValidationError{Category: "subscription", Rule: "subscription-name", Path: path, Found: s.Name, Message: message})
		}
		switch {
		case !kebabName.MatchString(s.Name):
			fault(fmt.Sprintf("the subscription name %q must match %s: lower kebab case, at most 63 characters", s.Name, kebabName))
		case seen[s.Name]:
			fault(fmt.Sprintf("the subscription name %q is used twice; give each subscription its own name", s.Name))
		case others[s.Name] != "":
			fault(fmt.Sprintf("the subscription name %q is the name of a %s; it names the stager of each message, so give it a name that no command, query, worker or controller uses", s.Name, others[s.Name]))
		case reservedDestinations[s.Name] || facts.Destinations[s.Name]:
			fault(fmt.Sprintf("the subscription name %q is a destination that the service or an active component has; give the subscription a destination of its own", s.Name))
		}
		seen[s.Name] = true
	}
	return faults
}

// validateSubscriptionEvents checks ST-DECL-INV97: a source that is a service name other than the service's own,
// and 1 to MaxSubscriptionEvents event kinds, each <source>.<entity>.<action> of that source, with no kind twice.
func validateSubscriptionEvents(service *types.ServiceDeclaration) []ValidationError {
	var faults []ValidationError
	for i, s := range service.Subscriptions {
		path := subscriptionPath(i)
		switch {
		case !kebabName.MatchString(s.Source):
			faults = append(faults, ValidationError{Category: "subscription", Rule: "subscription-source", Path: path + ".source", Found: s.Source,
				Message: fmt.Sprintf("subscription %q has the source %q; a source is the name of a service in lower kebab case", s.Name, s.Source)})
		case s.Source == service.Name:
			faults = append(faults, ValidationError{Category: "subscription", Rule: "subscription-source", Path: path + ".source", Found: s.Source,
				Message: fmt.Sprintf("subscription %q takes the change events of the service itself; a worker takes the service's own messages through an effect instead", s.Name)})
		}
		if len(s.Events) == 0 || len(s.Events) > MaxSubscriptionEvents {
			faults = append(faults, ValidationError{Category: "subscription", Rule: "subscription-events", Path: path + ".events", Found: fmt.Sprint(len(s.Events)),
				Message: fmt.Sprintf("subscription %q names %d event kinds; give 1 to %d", s.Name, len(s.Events), MaxSubscriptionEvents)})
		}
		seen := map[string]bool{}
		for j, kind := range s.Events {
			at := fmt.Sprintf("%s.events[%d]", path, j)
			match := subscriptionEvent.FindStringSubmatch(kind)
			switch {
			case match == nil:
				faults = append(faults, ValidationError{Category: "subscription", Rule: "subscription-events", Path: at, Found: kind,
					Message: fmt.Sprintf("subscription %q names the event kind %q; a kind is <source>.<entity in kebab case>.<created, updated or deleted>, such as %s.order.created", s.Name, kind, s.Source)})
			case match[1] != s.Source:
				faults = append(faults, ValidationError{Category: "subscription", Rule: "subscription-events", Path: at, Found: kind,
					Message: fmt.Sprintf("subscription %q names the event kind %q of the service %s; each kind starts with the source %s", s.Name, kind, match[1], s.Source)})
			case seen[kind]:
				faults = append(faults, ValidationError{Category: "subscription", Rule: "subscription-events", Path: at, Found: kind,
					Message: fmt.Sprintf("subscription %q names the event kind %q twice", s.Name, kind)})
			}
			seen[kind] = true
		}
	}
	return faults
}

// validateSubscriptionConsumer checks ST-DECL-INV98: exactly one worker has the trigger consumes <name> of each
// subscription, and no command or worker stages an effect for the subscription, whose messages come only from Kafka.
func validateSubscriptionConsumer(service *types.ServiceDeclaration) []ValidationError {
	consumers := map[string][]string{}
	for _, w := range service.Workers {
		if trigger, reason := parseTrigger(w.Trigger); reason == "" && trigger.consumes != "" {
			consumers[trigger.consumes] = append(consumers[trigger.consumes], w.Name)
		}
	}
	subscribed := map[string]bool{}
	var faults []ValidationError
	for i, s := range service.Subscriptions {
		subscribed[s.Name] = true
		if len(consumers[s.Name]) != 1 {
			faults = append(faults, ValidationError{Category: "subscription", Rule: "subscription-consumer", Path: subscriptionPath(i) + ".name", Found: s.Name,
				Message: fmt.Sprintf("subscription %q has %d workers with the trigger consumes %s; declare exactly one, which acts on each event with its own service account and declared writes", s.Name, len(consumers[s.Name]), s.Name)})
		}
	}
	stages := func(kind, name, path string, effects []types.Effect) {
		for j, effect := range effects {
			if subscribed[effect.Destination] {
				faults = append(faults, ValidationError{Category: kind, Rule: kind + "-effect", Path: fmt.Sprintf("%s.effects[%d].destination", path, j), Found: effect.Destination,
					Message: fmt.Sprintf("%s %q stages the effect %q for the subscription %s, whose messages come only from Kafka; stage it for another destination", kind, name, effect.String(), effect.Destination)})
			}
		}
	}
	for i, c := range service.Commands {
		stages("command", c.Name, fmt.Sprintf("commands[%d]", i), c.Effects)
	}
	for i, w := range service.Workers {
		stages("worker", w.Name, workerPath(i), w.Effects)
	}
	return faults
}

// SubscriptionOf gives the subscription named name, or nil.
func SubscriptionOf(service *types.ServiceDeclaration, name string) *types.Subscription {
	for i := range service.Subscriptions {
		if service.Subscriptions[i].Name == name {
			return &service.Subscriptions[i]
		}
	}
	return nil
}
