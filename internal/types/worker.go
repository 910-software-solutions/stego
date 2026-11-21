package types

// Worker is one piece of background work that the service's own code carries out on a schedule or for each message
// it consumes (ST-DECL-EN25): its name, its trigger, the entities it writes, the permission it needs, the effects it
// stages and the one fill that carries it out with a worker handle.
type Worker struct {
	// Name is the worker's name in lower kebab case, unique among the commands, queries, workers and controllers.
	Name string `yaml:"name"`
	// Trigger is when the worker runs: every <interval>, such as every 1m, or consumes <destination>.
	Trigger string `yaml:"trigger"`
	// Writes names the entities that the worker handle may read and write.
	Writes []string `yaml:"writes"`
	// Reads names the entities that the worker handle may only read, through a read-only repository whose values hold
	// no secret field (ST-DECL-EN25 Reads, decision ST-SUP-Q186 (a)). No entity is in both Writes and Reads.
	Reads []string `yaml:"reads,omitempty"`
	// ResourceType is the type in the authorization schema that the worker acts on.
	ResourceType string `yaml:"resource_type,omitempty"`
	// Permission is the permission that the worker's service account needs on each resource it reads or writes.
	Permission string `yaml:"permission,omitempty"`
	// Fill names the one fill that carries out the worker at the worker slot.
	Fill FillNames `yaml:"fill"`
	// Effects are the effects the fill may stage, each an outbox destination and an effect name.
	Effects []Effect `yaml:"effects,omitempty"`
	// Single tells, for a worker with an every trigger, whether one instance of the service runs it at a time under
	// a lease. Nil is omitted, which is yes.
	Single *bool `yaml:"single,omitempty"`
	// LeaseTTL is how long the lease of a single worker holds, such as 15s. Empty is 15s.
	LeaseTTL string `yaml:"lease_ttl,omitempty"`
	// Lists are the declared lists that the fill reads through: the generator emits their statements from the entity
	// of the release, so a fill gives no SQL text (ST-DECL-INV94, ST-SUP-RC35-OC15).
	Lists []WorkerList `yaml:"lists,omitempty"`
}

// WorkerList is one declared list of a worker or a command (ST-DECL-INV94): its name, unique in the worker or the
// command, an entity that it writes, a filter of one to eight terms over the entity's declared fields, and whether
// the list locks its rows, which needs the Lock at most of the entity (ST-DECL-INV81).
type WorkerList struct {
	Name   string     `yaml:"name"`
	Entity string     `yaml:"entity"`
	Filter []ListTerm `yaml:"filter"`
	Lock   bool       `yaml:"lock,omitempty"`
}

// The operators of a list term (ST-DECL-INV94).
const (
	ListEquals              = "equals"
	ListNotEquals           = "not_equals"
	ListLessThan            = "less_than"
	ListLessThanOrEquals    = "less_than_or_equals"
	ListGreaterThan         = "greater_than"
	ListGreaterThanOrEquals = "greater_than_or_equals"
	ListIsSet               = "is_set"
)

// ListTerm is one term of a list filter: a declared field of the entity, an operator, and a value that is a constant
// of the field type, Now for the time of the run, or the parameter Param, whose value the fill gives at call time
// through the generated function of the list (ST-DECL-INV94). The operator is_set takes no value.
type ListTerm struct {
	Field string `yaml:"field"`
	Op    string `yaml:"op"`
	Value any    `yaml:"value,omitempty"`
	Now   bool   `yaml:"now,omitempty"`
	// Param is the name of a parameter in lower snake case, unique in its list.
	Param string `yaml:"param,omitempty"`
	// Parent tells that the value is the ID of the loaded parent row, which the fill gives at call time: the term of
	// the reference of a list over a child entity (ST-DECL-INV94).
	Parent bool `yaml:"parent,omitempty"`
}

// Subscription is the change events of one other service that the service consumes from Kafka (ST-DECL-EN28): its
// name, which is the destination that one worker consumes, the source service and the event kinds that it takes. The
// kafka-consumer component writes each record of a declared kind into the outbox for that worker.
type Subscription struct {
	// Name is the subscription's name in lower kebab case: the destination of its messages, which the worker with the
	// trigger consumes <name> consumes.
	Name string `yaml:"name"`
	// Source is the name of the service whose change events the subscription takes.
	Source string `yaml:"source"`
	// Events are the event kinds that the subscription takes, each <source>.<entity in kebab case>.<action>.
	Events []string `yaml:"events"`
}
