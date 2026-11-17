package command

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/postgresadapter"
	"github.com/jsell-rh/stego/internal/types"
)

// The grammars of the declared names that the generated code holds.
var (
	operationName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	fillName      = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
	refusalName   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
	refusalCode   = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,62}$`)
	identifier    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
	relationText  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}#[a-z][a-z0-9_]{0,62}$`)
	sqlName       = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)
	effectName    = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,62}$`)
)

// KafkaComponent is the component whose change events the Kafka destination of the outbox delivers
// (ST-SUP-RC20-OC40).
const KafkaComponent = "kafka-producer"

// The actions of a change event: created for a create, updated for an update and deleted for a delete.
const (
	EventCreated = "created"
	EventUpdated = "updated"
	EventDeleted = "deleted"
)

// changeEventName is an event name that is both an outbox message kind and a Kafka topic name that Kafka does not
// refuse: at most 128 characters of letters, digits, periods, underscores and hyphens, starting with a letter or a
// digit (ST-SUP-RC20-OC40).
var changeEventName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

// ChangeEvents tells whether the service publishes the change events of its entities to Kafka: it has the
// kafka-producer component (ST-SUP-RC20-OC40, ST-SUP-RC32-OC14, ST-SUP-RC35-OC14).
func ChangeEvents(ctx gen.Context) bool { return ctx.PeerNamespaces[KafkaComponent] != "" }

// ChangeEventType gives the name of the change event of the entity with action: the service name, the entity name
// in lower-case words joined by hyphens, and the action, joined by periods, such as
// user-management.org-setting.updated. It is the kind of the outbox message and, when the settings declare no
// topic, the Kafka topic (ST-SUP-RC20-OC40, ST-SUP-RP01-LC30).
func ChangeEventType(service, entity, action string) string {
	var b strings.Builder
	for i, r := range entity {
		if 'A' <= r && r <= 'Z' {
			if i > 0 {
				b.WriteByte('-')
			}
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return service + "." + b.String() + "." + action
}

// CheckChangeEvents refuses, for a service with the kafka-producer component, a service without the outbox
// component, which delivers the change events, and an entity of entities whose change event name is not an outbox
// kind and a Kafka topic name (ST-SUP-RC20-OC40, ST-SUP-RC32-OC14, ST-SUP-RC35-OC14). component names the generator
// in the error, and record the output contract.
func CheckChangeEvents(ctx gen.Context, component, record string, entities []string) error {
	if !ChangeEvents(ctx) {
		return nil
	}
	if ctx.PeerNamespaces["outbox"] == "" {
		return fmt.Errorf("%s: the change events of the written entities go to Kafka through the outbox, so the service needs the outbox component (%s)", component, record)
	}
	for _, entity := range entities {
		for _, action := range []string{EventCreated, EventUpdated, EventDeleted} {
			if name := ChangeEventType(ctx.ServiceName, entity, action); !changeEventName.MatchString(name) {
				return fmt.Errorf("%s: the event type %q of the entity %s is not an outbox kind and a Kafka topic name: [a-zA-Z0-9._-], at most 128 characters, starting with a letter or a digit (%s)", component, name, entity, record)
			}
		}
	}
	return nil
}

// ChangeEventsSpec gives the Go text of the ChangeEvents of a CommandSpec or a WorkerSpec for the written entities
// entities, or an empty text when the service has no change events (ST-SUP-RC32-OC14, ST-SUP-RC35-OC14).
func ChangeEventsSpec(ctx gen.Context, entities []string) string {
	if !ChangeEvents(ctx) || len(entities) == 0 {
		return ""
	}
	parts := make([]string, len(entities))
	for i, entity := range entities {
		parts[i] = fmt.Sprintf("{Entity: %q, Created: %q, Updated: %q, Deleted: %q}", entity,
			ChangeEventType(ctx.ServiceName, entity, EventCreated), ChangeEventType(ctx.ServiceName, entity, EventUpdated),
			ChangeEventType(ctx.ServiceName, entity, EventDeleted))
	}
	return "\tChangeEvents: []command.ChangeEvents{" + strings.Join(parts, ", ") + "},\n"
}

// refusalStatus holds the statuses that a declared refusal may have (ST-DECL-INV56).
var refusalStatus = map[int]bool{400: true, 403: true, 404: true, 409: true, 412: true, 422: true}

// mixedCase gives the exported Go name of a lower-case name with hyphens or underscores, such as ReserveAndCharge
// for reserve-and-charge and OrderID for order_id.
func mixedCase(name string) string {
	var b strings.Builder
	for _, part := range strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' || r == '.' }) {
		if part == "id" {
			b.WriteString("ID")
			continue
		}
		runes := []rune(part)
		runes[0] = unicode.ToUpper(runes[0])
		b.WriteString(string(runes))
	}
	return b.String()
}

// tableName is the table of an entity, as the PostgreSQL adapter creates it (ST-SUP-RC01-OC41).
func tableName(entity string) string { return postgresadapter.TableName(entity) }

// TableName is the table in public of the declared entity entity, as the PostgreSQL adapter names it.
func TableName(entity string) string { return tableName(entity) }

// retryAndLock reads the command's Retry on conflict and Lock timeout (ST-DECL-EN18): the retries from 0 to 3, and
// the lock timeout as a Go duration expression, empty for the runtime's default.
func retryAndLock(decl types.Command) (int, string, error) {
	retry := 0
	if decl.RetryOnConflict != "" {
		n, err := strconv.Atoi(decl.RetryOnConflict)
		if err != nil || n < 0 || n > 3 {
			return 0, "", fmt.Errorf("command: the command %s has the retry on conflict %q; it must be a whole number from 0 to 3", decl.Name, decl.RetryOnConflict)
		}
		retry = n
	}
	lock := ""
	if decl.LockTimeout != "" {
		d, err := time.ParseDuration(decl.LockTimeout)
		if err != nil || d < 100*time.Millisecond || d > 10*time.Second {
			return 0, "", fmt.Errorf("command: the command %s has the lock timeout %q; it must be a duration from 100ms to 10s", decl.Name, decl.LockTimeout)
		}
		lock = fmt.Sprintf("%d * time.Millisecond", d.Milliseconds())
	}
	return retry, lock, nil
}
