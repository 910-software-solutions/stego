package compiler

import "fmt"

// The architecture model (ST-GEN-RM04, ST-GEN-DS04): the data file architecture/architecture.json that apply writes
// at the service root, valid against the schema architecture.schema.json ($id stego:architecture/2). Each struct
// gives its keys in a fixed order, every list is sorted by id except the steps of a flow and the layers, and every
// element names its source (ST-GEN-UC08-R03), so the same inputs give the same bytes (ST-GEN-UC09-R03).

// ArchitectureSchemaVersion is the version of the architecture schema. Each change of the schema raises it, also a new
// value of an enum such as an access kind, because a reader that skips a value it does not know shows less than the
// model holds (decision on ST-GEN-Q43). Version 2 adds the access kind 'path limit'.
const ArchitectureSchemaVersion = 2

// ArchitectureSchemaID is the $id of the architecture schema of this version.
var ArchitectureSchemaID = fmt.Sprintf("stego:architecture/%d", ArchitectureSchemaVersion)

// ArchitectureFile is the path, in the project folder, of the architecture model.
const ArchitectureFile = "architecture/architecture.json"

// ArchitectureModel is the whole model.
type ArchitectureModel struct {
	SchemaVersion int                `json:"schema_version"`
	Record        ArchitectureRecord `json:"record"`
	System        ArchSystem         `json:"system"`
	Aspects       ArchAspects        `json:"aspects"`
	Layers        []string           `json:"layers"`
	// Structure.
	Containers []ArchContainer `json:"containers"`
	// Actors and Relations are the context of the service (architecture_context.go).
	Actors     []ArchActor     `json:"actors"`
	Relations  []ArchRelation  `json:"relations"`
	Components []ArchComponent `json:"components"`
	Mixins     []ArchMixin     `json:"mixins"`
	Ports      []ArchPort      `json:"ports"`
	Bindings   []ArchBinding   `json:"bindings"`
	Slots      []ArchSlot      `json:"slots"`
	Fills      []ArchFill      `json:"fills"`
	// Data model.
	Entities    []ArchEntity     `json:"entities"`
	References  []ArchReference  `json:"references"`
	Collections []ArchCollection `json:"collections"`
	Operations  []ArchOperation  `json:"operations"`
	Actions     []ArchAction     `json:"actions"`
	// Request flows.
	Flows []ArchFlow `json:"flows"`
	// Platform edges.
	Needs []ArchNeed `json:"needs"`
	// Variables are the environment variables of the platform requirements' list, copied from it unchanged
	// (ST-GEN-UC08-R16).
	Variables []ArchVariable `json:"variables"`
	// Trust and tenancy.
	Boundaries  []ArchBoundary `json:"boundaries"`
	Settings    []ArchSetting  `json:"settings"`
	AuthnPoints []ArchPoint    `json:"authn_points"`
	AuthzPoints []ArchPoint    `json:"authz_points"`
	Access      ArchAccess     `json:"access"`
	// Compliance holds the facts of each compliance topic (ST-GEN-UC16-R01), so a list of facts is a projection of
	// the model alone (ST-GEN-UC16-R05).
	Compliance []ArchTopic `json:"compliance"`
}

// ArchitectureRecord names the record that the model was written with, by its path (ST-GEN-UC08-R03). It holds no
// input digest and no compiler build: the record is saved last and holds the model's hash with the input digests, and
// a digest in the model would change the model when the shape does not change, which ST-GEN-UC09-R03 and
// ST-GEN-UC03-R02 forbid.
type ArchitectureRecord struct {
	Path string `json:"path"`
}

// ArchSource is where an element comes from: exactly one of a declaration path such as entities[1].fields[2], a
// registry part, the assembler's wiring, the default rule that sets a value the declaration omits, or a fixed rule
// of STEGO.
type ArchSource struct {
	Declaration string        `json:"declaration,omitempty"`
	Registry    *ArchRegistry `json:"registry,omitempty"`
	Wiring      string        `json:"wiring,omitempty"`
	Default     string        `json:"default,omitempty"`
	Rule        string        `json:"rule,omitempty"`
}

// ArchRegistry is a registry part at its version, and the field of it that states the element.
type ArchRegistry struct {
	Part    string `json:"part"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Field   string `json:"field,omitempty"`
}

// ArchSystem is the service.
type ArchSystem struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	Archetype        string     `json:"archetype"`
	ArchetypeVersion string     `json:"archetype_version"`
	Language         string     `json:"language"`
	BasePath         string     `json:"base_path"`
	Source           ArchSource `json:"source"`
}

// ArchAspects holds the state of each of the five aspects (ST-GEN-UC08-R01, R06).
type ArchAspects struct {
	Structure       ArchAspect `json:"structure"`
	DataModel       ArchAspect `json:"data_model"`
	RequestFlows    ArchAspect `json:"request_flows"`
	PlatformEdges   ArchAspect `json:"platform_edges"`
	TrustAndTenancy ArchAspect `json:"trust_and_tenancy"`
}

// ArchAspect is present or empty; empty gives the reason in fixed text (ST-GEN-UC08-R06).
type ArchAspect struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

// ArchContainer is one runtime unit: the service process, or a backing store or topic that a need states.
type ArchContainer struct {
	ID     string     `json:"id"`
	Name   string     `json:"name"`
	Kind   string     `json:"kind"`
	Source ArchSource `json:"source"`
}

// ArchComponent is one component of the active set, placed by its architecture role alone.
type ArchComponent struct {
	ID        string              `json:"id"`
	Name      string              `json:"name"`
	Version   string              `json:"version"`
	Stage     string              `json:"stage"`
	Layer     string              `json:"layer"`
	FlowPart  string              `json:"flow_part"`
	Namespace string              `json:"namespace"`
	Container string              `json:"container"`
	Ports     []ArchComponentPort `json:"ports"`
	Offers    []ArchOffer         `json:"offers"`
	Source    ArchSource          `json:"source"`
}

// ArchMixin is one mixin that the declaration binds, with the slots it offers.
type ArchMixin struct {
	ID      string      `json:"id"`
	Name    string      `json:"name"`
	Version string      `json:"version"`
	Offers  []ArchOffer `json:"offers"`
	Source  ArchSource  `json:"source"`
}

// ArchComponentPort is one port of a component with its direction and side.
type ArchComponentPort struct {
	Port      string `json:"port"`
	Direction string `json:"direction"`
	Side      string `json:"side"`
}

// ArchOffer is one slot that a component or a mixin offers, with its phase and operations.
type ArchOffer struct {
	Name       string   `json:"name"`
	Phase      string   `json:"phase"`
	Operations []string `json:"operations"`
}

// ArchPort is one port with the components that provide and require it.
type ArchPort struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	ProvidedBy []string   `json:"provided_by"`
	RequiredBy []string   `json:"required_by"`
	Source     ArchSource `json:"source"`
}

// ArchBinding is the component that the wiring binds to a port for a component that requires it.
type ArchBinding struct {
	ID       string     `json:"id"`
	Port     string     `json:"port"`
	Provider string     `json:"provider"`
	Consumer string     `json:"consumer"`
	Source   ArchSource `json:"source"`
}

// ArchSlot is one slot binding of the declaration: a slot of a collection with its fills in order.
type ArchSlot struct {
	ID           string     `json:"id"`
	Collection   string     `json:"collection"`
	Name         string     `json:"name"`
	Phase        string     `json:"phase"`
	Operations   []string   `json:"operations"`
	Strategy     string     `json:"strategy"`
	ShortCircuit bool       `json:"short_circuit"`
	OfferedBy    string     `json:"offered_by"`
	Fills        []string   `json:"fills"`
	Source       ArchSource `json:"source"`
}

// ArchFill is one fill, owner code inside a STEGO contract, with what it implements.
type ArchFill struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Implements []string   `json:"implements"`
	Source     ArchSource `json:"source"`
}

// ArchEntity is one entity with its fields.
type ArchEntity struct {
	ID           string      `json:"id"`
	Name         string      `json:"name"`
	TenantKind   string      `json:"tenant_kind"`
	ResourceType string      `json:"resource_type,omitempty"`
	Fields       []ArchField `json:"fields"`
	Source       ArchSource  `json:"source"`
}

// ArchField is one field of an entity, in the order of the declaration, with its sensitivity.
type ArchField struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Type        string     `json:"type"`
	Required    bool       `json:"required"`
	Sensitivity string     `json:"sensitivity"`
	Ref         string     `json:"ref,omitempty"`
	Source      ArchSource `json:"source"`
}

// ArchReference is one ref field from an entity to another.
type ArchReference struct {
	ID       string     `json:"id"`
	From     string     `json:"from"`
	To       string     `json:"to"`
	Field    string     `json:"field"`
	Relation string     `json:"relation,omitempty"`
	Source   ArchSource `json:"source"`
}

// ArchCollection is one collection of the declaration.
type ArchCollection struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Entity     string     `json:"entity"`
	Scope      string     `json:"scope,omitempty"`
	Operations []string   `json:"operations"`
	Source     ArchSource `json:"source"`
}

// ArchOperation is one operation of a collection, with the route the wiring registers for it when it registers one.
type ArchOperation struct {
	ID         string     `json:"id"`
	Collection string     `json:"collection"`
	Verb       string     `json:"verb"`
	Method     string     `json:"method,omitempty"`
	Path       string     `json:"path,omitempty"`
	Flow       string     `json:"flow"`
	Source     ArchSource `json:"source"`
}

// ArchAction is one command, query or worker of the declaration.
type ArchAction struct {
	ID     string     `json:"id"`
	Kind   string     `json:"kind"`
	Name   string     `json:"name"`
	Fills  []string   `json:"fills"`
	Source ArchSource `json:"source"`
}

// ArchFlow is the request flow of one operation: its steps in order and the points where it can stop.
type ArchFlow struct {
	ID        string     `json:"id"`
	Operation string     `json:"operation"`
	Steps     []ArchStep `json:"steps"`
	Exits     []ArchExit `json:"exits"`
	Source    ArchSource `json:"source"`
}

// ArchStep is one step of a flow.
type ArchStep struct {
	N       int    `json:"n"`
	Kind    string `json:"kind"`
	Element string `json:"element"`
	ViaPort string `json:"via_port,omitempty"`
	Text    string `json:"text"`
	Mode    string `json:"mode"`
	// Layer is the layer of the step, which the deriver sets (flowStepLayer).
	Layer  string     `json:"layer,omitempty"`
	Source ArchSource `json:"source"`
}

// ArchExit is a point where a flow can stop, after a step, and the result it then gives.
type ArchExit struct {
	AfterStep int        `json:"after_step"`
	When      string     `json:"when"`
	Result    string     `json:"result"`
	Source    ArchSource `json:"source"`
}

// ArchNeed is one need of the platform requirements (ST-DLV-EN01).
type ArchNeed struct {
	ID       string     `json:"id"`
	Kind     string     `json:"kind"`
	Name     string     `json:"name"`
	Shape    string     `json:"shape,omitempty"`
	StatedBy []string   `json:"stated_by"`
	Source   ArchSource `json:"source"`
}

// ArchVariable is one environment variable of the platform requirements' list (ST-DLV-EN08, ST-GEN-UC08-R16): the
// components that read it, its type, whether it is required, its default and whether it is secret. A secret
// variable holds no default.
type ArchVariable struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	ReadBy   []string   `json:"read_by"`
	Type     string     `json:"type"`
	Required bool       `json:"required"`
	Default  *string    `json:"default,omitempty"`
	Secret   bool       `json:"secret"`
	Source   ArchSource `json:"source"`
}

// ArchBoundary is one trust boundary, what it holds and what crosses it.
type ArchBoundary struct {
	ID        string     `json:"id"`
	Kind      string     `json:"kind"`
	Label     string     `json:"label"`
	Contains  []string   `json:"contains"`
	CrossedBy []string   `json:"crossed_by"`
	Source    ArchSource `json:"source"`
}

// ArchSetting is one setting of the declaration as checking resolves it, each default marked with its rule.
type ArchSetting struct {
	ID      string     `json:"id"`
	Path    string     `json:"path"`
	Value   any        `json:"value"`
	Default bool       `json:"default"`
	Source  ArchSource `json:"source"`
}

// ArchPoint is one point where a caller is authenticated, or where authorization is decided.
type ArchPoint struct {
	ID      string     `json:"id"`
	Element string     `json:"element"`
	Text    string     `json:"text"`
	Source  ArchSource `json:"source"`
}

// ArchAccess is the access matrix (ST-GEN-UC08-R12, R13): what each operation and action checks, and the grant
// paths of each permission, expanded statically from the declared authorization schema. It holds no relationship.
type ArchAccess struct {
	Schema      string           `json:"schema,omitempty"`
	Checks      []ArchCheck      `json:"checks"`
	Permissions []ArchPermission `json:"permissions"`
	Roles       []ArchRole       `json:"roles"`
	// RolesComplete is false when a limit cut the paths of a permission short, so the roles may be more than the
	// list holds.
	RolesComplete bool `json:"roles_complete"`
}

// ArchCheck is what one operation or action checks: a resource type and a permission, or public with its reason.
type ArchCheck struct {
	ID           string `json:"id"`
	Subject      string `json:"subject"`
	ResourceType string `json:"resource_type,omitempty"`
	Permission   string `json:"permission,omitempty"`
	Public       bool   `json:"public"`
	Reason       string `json:"reason,omitempty"`
	Grants       string `json:"grants,omitempty"`
	// Ancestors are the checks of the parent chain of an operation of a scoped collection, in the order the handler
	// reads them, the top ancestor first (ST-SUP-RC20-OC24): the handler reads each ancestor of the path before the
	// row, and answers 404 when one is missing, denied or of another parent. An operation with no scope has none.
	Ancestors []ArchAncestor `json:"ancestors,omitempty"`
	Source    ArchSource     `json:"source"`
}

// ArchAncestor is the check of one ancestor in the path of an operation: the entity of the row that the path
// parameter names, the field of that row that names the ancestor above it, and the read permission of the entity,
// which the handler decides on the row. With no bound authorization it gets no permission check, and Reason says
// why; the row must still exist in the tenant and be of the ancestor above it. The derivation refuses an ancestor
// with no resource type, which no generator builds.
type ArchAncestor struct {
	ID           string `json:"id"`
	Entity       string `json:"entity"`
	Param        string `json:"param"`
	Link         string `json:"link,omitempty"`
	ResourceType string `json:"resource_type,omitempty"`
	Permission   string `json:"permission,omitempty"`
	Public       bool   `json:"public"`
	Reason       string `json:"reason,omitempty"`
	Grants       string `json:"grants,omitempty"`
	// Source is what sets the check: the read permission of the entity, or the rule that gives it none. ScopeSource
	// is the scope entry of the collection that puts the ancestor in the path.
	Source      ArchSource `json:"source"`
	ScopeSource ArchSource `json:"scope_source"`
}

// ArchPermission is one permission of a resource type with its grant paths.
type ArchPermission struct {
	ID           string      `json:"id"`
	ResourceType string      `json:"resource_type"`
	Name         string      `json:"name"`
	Paths        []ArchGrant `json:"paths"`
	// Complete is false when a limit cut a path short, so the permission may grant more roles than its paths name.
	Complete bool       `json:"complete"`
	Source   ArchSource `json:"source"`
}

// ArchGrant is one grant path: the role it grants (a subject type and its relation), the kinds of the path, the hops
// from the permission to the relation, and the conditions on it.
type ArchGrant struct {
	ID       string   `json:"id"`
	Role     string   `json:"role,omitempty"`
	Kinds    []string `json:"kinds"`
	Hops     []string `json:"hops"`
	Caveat   string   `json:"caveat,omitempty"`
	Requires []string `json:"requires,omitempty"`
	Except   []string `json:"except,omitempty"`
	Stops    string   `json:"stops,omitempty"`
	// Source is the declared schema the path is expanded from.
	Source ArchSource `json:"source"`
}

// ArchRole is one role of the access matrix: a subject type, with the relation of the resource type that holds it.
type ArchRole struct {
	ID          string `json:"id"`
	SubjectType string `json:"subject_type"`
	Relation    string `json:"relation"`
	On          string `json:"on"`
	Wildcard    bool   `json:"wildcard"`
	// Caveat is the caveat the relation requires of the subject, and Expiration whether it requires an expiration;
	// either makes each grant to the role conditional.
	Caveat     string `json:"caveat,omitempty"`
	Expiration bool   `json:"expiration"`
	// Source is the declared schema that states the relation.
	Source ArchSource `json:"source"`
}

// ArchTopic is one compliance topic with its facts.
type ArchTopic struct {
	ID    string     `json:"id"`
	Facts []ArchFact `json:"facts"`
}

// ArchFact is one fact: what it is about, its value when the inputs state it, and whether it is stated, a default
// or unstated (ST-GEN-UC16-R04).
type ArchFact struct {
	ID     string     `json:"id"`
	About  string     `json:"about"`
	Name   string     `json:"name"`
	Value  any        `json:"value"`
	State  string     `json:"state"`
	Source ArchSource `json:"source"`
}
