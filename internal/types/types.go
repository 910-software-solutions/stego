package types

import (
	"fmt"
	"maps"
	"slices"
	"sort"

	"gopkg.in/yaml.v3"
)

// FieldType enumerates the primitive and stego-specific field types.
type FieldType string

const (
	FieldTypeString    FieldType = "string"
	FieldTypeInt32     FieldType = "int32"
	FieldTypeInt64     FieldType = "int64"
	FieldTypeFloat     FieldType = "float"
	FieldTypeDouble    FieldType = "double"
	FieldTypeBool      FieldType = "bool"
	FieldTypeBytes     FieldType = "bytes"
	FieldTypeTimestamp FieldType = "timestamp"
	FieldTypeEnum      FieldType = "enum"
	FieldTypeRef       FieldType = "ref"
	FieldTypeJsonb     FieldType = "jsonb"
	// FieldTypeMoney is an amount in integer minor units with the ISO 4217 currency that the field names
	// (ST-VO61, ST-DECL-INV26).
	FieldTypeMoney FieldType = "money"
)

// ValidFieldTypes is the set of all valid FieldType values.
var ValidFieldTypes = map[FieldType]bool{
	FieldTypeString:    true,
	FieldTypeInt32:     true,
	FieldTypeInt64:     true,
	FieldTypeFloat:     true,
	FieldTypeDouble:    true,
	FieldTypeBool:      true,
	FieldTypeBytes:     true,
	FieldTypeTimestamp: true,
	FieldTypeEnum:      true,
	FieldTypeRef:       true,
	FieldTypeJsonb:     true,
	FieldTypeMoney:     true,
}

// Operation enumerates the CRUD+ operations.
type Operation string

const (
	OpCreate Operation = "create"
	OpRead   Operation = "read"
	OpUpdate Operation = "update"
	OpDelete Operation = "delete"
	OpList   Operation = "list"
	OpUpsert Operation = "upsert"
	OpPatch  Operation = "patch"
)

// ValidOperations is the set of all valid Operation values.
var ValidOperations = map[Operation]bool{
	OpCreate: true,
	OpRead:   true,
	OpUpdate: true,
	OpDelete: true,
	OpList:   true,
	OpUpsert: true,
	OpPatch:  true,
}

// Field represents a single field within an entity definition.
type Field struct {
	Name            string    `yaml:"name"`
	Type            FieldType `yaml:"type"`
	MinLength       *int      `yaml:"min_length,omitempty"`
	MaxLength       *int      `yaml:"max_length,omitempty"`
	Pattern         string    `yaml:"pattern,omitempty"`
	Min             *float64  `yaml:"min,omitempty"`
	Max             *float64  `yaml:"max,omitempty"`
	Unique          bool      `yaml:"unique,omitempty"`
	UniqueComposite []string  `yaml:"unique_composite,omitempty"`
	UniqueWhenLive  bool      `yaml:"unique_when_live,omitempty"`
	Optional        bool      `yaml:"optional,omitempty"`
	Default         any       `yaml:"default,omitempty"`
	Values          []string  `yaml:"values,omitempty"`
	Computed        bool      `yaml:"computed,omitempty"`
	FilledBy        string    `yaml:"filled_by,omitempty"`
	To              string    `yaml:"to,omitempty"` // for ref type
	Unobserved      *string   `yaml:"unobserved,omitempty"`
	Deleting        *string   `yaml:"deleting,omitempty" json:"Deleting,omitempty"`
	// Sensitivity is public, pii or secret (ST-DECL-EN03). Omitted means public.
	Sensitivity string `yaml:"sensitivity,omitempty" json:"Sensitivity,omitempty"`
	// CompareOnly marks a secret string field that the service only checks and never reads back, such as a PIN or a
	// recovery code: it is stored as an Argon2id hash. Omitted means readable: the field is sealed at rest
	// (ST-DECL-EN03 field Compare only, decision ST-DECL-Q44).
	CompareOnly bool `yaml:"compare_only,omitempty" json:"CompareOnly,omitempty"`
	// Currency is the ISO 4217 code of a field of type money (ST-DECL-INV26).
	Currency string `yaml:"currency,omitempty" json:"Currency,omitempty"`
	// ClientSet marks a timestamp field whose value the client sets, such as a due date: the server does not
	// manage it (ST-SUP-RC20-OC17), and the generated code checks that it is an RFC 3339 time and keeps it in UTC.
	// Omitted means server-managed. The Product owner decided ST-DECL-Q16 on 2026-11-14.
	ClientSet bool `yaml:"client_set,omitempty" json:"ClientSet,omitempty"`
	// Writable is always, create or never: which collection writes may set the field (ST-DECL-EN03, ST-DECL-Q45
	// (a)). create lets only a create set it; never lets no collection write set it, and a create stores its
	// default. Omitted means always. A command or a worker may change the field in each case.
	Writable string `yaml:"writable,omitempty" json:"Writable,omitempty"`
	// Column is the stored column of the field when it is not Name: generation names a renamed field in the API by
	// its name before its first rename, and stores it in the column of its declared name (ST-GEN-UC12-R04). The
	// declaration never sets it.
	Column string `yaml:"-" json:"Column,omitempty"`
	// SealedFrom is the clear column of a readable secret whose seal step is open: from its expand step to its
	// contract step the field is in two columns, the clear one, which a release before writes, and Column, its
	// sealed column <column>_sealed (ST-SUP-RC01-OC49). The declaration never sets it.
	SealedFrom string `yaml:"-" json:"SealedFrom,omitempty"`
	// ClearStored marks a field of sensitivity secret that a generator before postgres-adapter 6.13.0 stored in
	// clear, in a column of its type with its CHECKs: the stored shape before a seal from secret (walkthrough 13,
	// W13-B1). The declaration never sets it.
	ClearStored bool `yaml:"-" json:"ClearStored,omitempty"`
}

// ServerTimestampNames are the timestamp field names that always hold a time the server sets: the time a row was
// created or last changed. A declaration cannot mark them client_set (ST-DECL-Q16).
var ServerTimestampNames = map[string]bool{"created_at": true, "updated_at": true, "created_time": true, "updated_time": true}

// ClientSetTime tells whether the field is a timestamp that the client sets (ST-DECL-Q16), not one that the server
// manages (ST-SUP-RC20-OC17).
func (f Field) ClientSetTime() bool {
	return f.ClientSet && f.Type == FieldTypeTimestamp && !f.Computed && !ServerTimestampNames[f.Name]
}

// The values of the writable setting of a field (ST-DECL-EN03).
const (
	WritableAlways = "always"
	WritableCreate = "create"
	WritableNever  = "never"
)

// WritableValues are the values of the writable setting of a field, in the order a fault lists them.
var WritableValues = []string{WritableAlways, WritableCreate, WritableNever}

// Kept tells whether a collection write keeps the stored value of the field: its writable setting is create or
// never, so an update, an upsert of a stored row and a patch refuse another value (ST-SUP-RC20-OC44 to
// ST-SUP-RC20-OC46).
func (f Field) Kept() bool { return f.Writable == WritableCreate || f.Writable == WritableNever }

// Secret tells whether the field is of the sensitivity secret (ST-DECL-EN03).
func (f Field) Secret() bool { return f.Sensitivity == "secret" }

// Sealed tells whether the field is a readable secret, which the generated code seals at rest (decision ST-DECL-Q44).
func (f Field) Sealed() bool { return f.Secret() && !f.CompareOnly }

// ColumnName gives the stored column of the field: Column, else Name.
func (f Field) ColumnName() string {
	if f.Column != "" {
		return f.Column
	}
	return f.Name
}

// Entity represents a domain entity with its fields.
type Entity struct {
	Name             string              `yaml:"name"`
	Fields           []Field             `yaml:"fields"`
	Versioned        bool                `yaml:"versioned,omitempty"`
	GenerationFields []string            `yaml:"generation_fields,omitempty"`
	Observations     map[string][]string `yaml:"observations,omitempty"`
	Conditions       map[string][]string `yaml:"conditions,omitempty"`
	CleanupTargets   map[string]string   `yaml:"cleanup_targets,omitempty"`
	CleanupOwners    []string            `yaml:"cleanup_owners,omitempty"`
	// LockAtMost is the most rows that one locked list of the entity may lock, 1 to 1,000, or nil when the entity
	// declares none (ST-DECL-EN02, ST-DECL-INV81). It is read as a number, so checking refuses a value that is not
	// whole, such as 2.5, which YAML would cut to 2 for an int.
	LockAtMost *float64 `yaml:"lock_at_most,omitempty" json:"LockAtMost,omitempty"`
	// Tenant is owned, shared or system (ST-DECL-EN02). Omitted means owned.
	Tenant string `yaml:"tenant,omitempty" json:"Tenant,omitempty"`
	// Kind is data or ledger (ST-DECL-EN02). Omitted means data. A ledger entity selects the ledger component
	// (ST-GEN-INV37).
	Kind string `yaml:"kind,omitempty" json:"Kind,omitempty"`
	// Ledger holds the ledger settings and accounts of an entity of kind ledger (ST-DECL-EN14, ST-DECL-EN15).
	Ledger *Ledger `yaml:"ledger,omitempty" json:"Ledger,omitempty"`
	// EntityAuthorization holds the entity's resource type, permissions and relationships (ST-DECL-UC06).
	EntityAuthorization `yaml:",inline"`
	// DroppedBy is the ID of the schema change of kind drop with no field that removes the entity, or empty
	// (ST-DECL-EN02, ST-GEN-Q47). The entity stays declared as it was, with the collections over it, so that the
	// baseline and the contract come from the declaration alone; the declaration keeps it apart from Entities.
	DroppedBy string `yaml:"dropped_by,omitempty" json:"DroppedBy,omitempty"`
}

// Convention captures the archetype's conventions.
type Convention struct {
	Layout            string `yaml:"layout"`
	ErrorHandling     string `yaml:"error_handling"`
	ResponseFormat    string `yaml:"response_format"`
	Logging           string `yaml:"logging"`
	TestPattern       string `yaml:"test_pattern"`
	RequestValidation string `yaml:"request_validation,omitempty"`
	CORS              string `yaml:"cors,omitempty"`
}

// ConfigFieldItems supports two YAML forms for a config field's items:
//   - A map of named sub-fields: items: { entity: {type: entity-ref}, ... }
//   - A single inline ConfigField: items: { enum: [create, read, ...] }
type ConfigFieldItems struct {
	// Fields holds named sub-fields when items is a map of ConfigField values.
	Fields map[string]ConfigField
	// Inline holds a single ConfigField when items is an inline object.
	Inline *ConfigField
}

// MarshalYAML produces output compatible with UnmarshalYAML: a map of named
// sub-fields when Fields is set, or a single inline ConfigField otherwise.
func (i ConfigFieldItems) MarshalYAML() (any, error) {
	if i.Fields != nil {
		return i.Fields, nil
	}
	if i.Inline != nil {
		return i.Inline, nil
	}
	return nil, nil
}

// UnmarshalYAML detects whether items is a map of named sub-fields or a single
// inline ConfigField, and deserializes accordingly.
func (i *ConfigFieldItems) UnmarshalYAML(unmarshal func(any) error) error {
	// Try map-of-named-fields first.
	var fields map[string]ConfigField
	if err := unmarshal(&fields); err == nil {
		// Disambiguate: if every value has a non-empty Type, treat as map of
		// named sub-fields. Otherwise it could be a single ConfigField that
		// has no Type (e.g. {enum: [...]}).
		allHaveType := len(fields) > 0
		for _, f := range fields {
			if f.Type == "" {
				allHaveType = false
				break
			}
		}
		if allHaveType {
			i.Fields = fields
			return nil
		}
	}
	// Fall back to single inline ConfigField.
	var single ConfigField
	if err := unmarshal(&single); err != nil {
		return err
	}
	i.Inline = &single
	return nil
}

// ConfigField represents a single field in a component's config schema.
// Items is recursive to support nested config structures.
type ConfigField struct {
	Type     string            `yaml:"type"`
	Default  any               `yaml:"default,omitempty"`
	Items    *ConfigFieldItems `yaml:"items,omitempty"`
	Optional bool              `yaml:"optional,omitempty"`
	Enum     []string          `yaml:"enum,omitempty"`
	// Secret marks a setting whose value is a secret: it has no default, and no name of a need or of an
	// environment variable may hold it (ST-SUP-EN09 field Config schema, ST-SUP-INV23).
	Secret bool `yaml:"secret,omitempty"`
	// Bypass marks a setting whose value turns off a check of the service. Checking refuses a declaration that gives
	// it a value (ST-SUP-EN09 field Config schema, ST-DECL-INV24, ST-DECL-SP07).
	Bypass bool `yaml:"bypass,omitempty"`
	// AuthorSupplied marks a setting that every declaration with the component must give. A setting that only the
	// running service gets, such as a connection string or an issuer, is not author supplied. An empty scaffold
	// names each one, and checking refuses a declaration that does not give one (ST-SUP-EN09 field Config schema,
	// ST-DECL-INV90).
	AuthorSupplied bool `yaml:"author_supplied,omitempty"`
}

// ConcurrencyMode for upsert operations.
type ConcurrencyMode string

const (
	ConcurrencyOptimistic ConcurrencyMode = "optimistic"
)

// ValidConcurrencyModes is the set of all valid ConcurrencyMode values.
var ValidConcurrencyModes = map[ConcurrencyMode]bool{
	ConcurrencyOptimistic: true,
}

// Collection is a scoped, operation-constrained access pattern over an entity.
// Multiple collections can reference the same entity. Each collection generates
// its own handler, routes, and wiring. The Name field is populated from the map
// key during parsing and is not serialized to YAML.
type Collection struct {
	Name        string            `yaml:"-"`
	Entity      string            `yaml:"entity"`
	Operations  []Operation       `yaml:"operations"`
	Scope       map[string]string `yaml:"scope,omitempty"`
	Implicit    map[string]string `yaml:"implicit,omitempty"`
	PathPrefix  string            `yaml:"path_prefix,omitempty"`
	UpsertKey   []string          `yaml:"upsert_key,omitempty"`
	Concurrency ConcurrencyMode   `yaml:"concurrency,omitempty"`
	Patchable   []string          `yaml:"patchable,omitempty"`
}

// ScopeField returns the scope field name (key) from the Scope map. For
// single-scope collections, this is the only key. Returns empty string if no
// scope is defined.
func (c Collection) ScopeField() string {
	for k := range c.Scope {
		return k
	}
	return ""
}

// UpsertLookup gives the fields that the upsert of the collection finds its stored row by, and that its unique
// index holds: the scope field, the implicit fields in name order, and then the upsert key, each once. So an upsert
// under one parent or implicit value never finds or moves the row of another (ST-SUP-RC20-OC13).
func (c Collection) UpsertLookup() []string {
	var fields []string
	add := func(name string) {
		if name != "" && !slices.Contains(fields, name) {
			fields = append(fields, name)
		}
	}
	add(c.ScopeField())
	for _, name := range slices.Sorted(maps.Keys(c.Implicit)) {
		add(name)
	}
	for _, name := range c.UpsertKey {
		add(name)
	}
	return fields
}

// ParentEntity returns the scope value (entity name) from the Scope map. For
// single-scope collections, this is the only value. Returns empty string if
// no scope is defined.
func (c Collection) ParentEntity() string {
	for _, v := range c.Scope {
		return v
	}
	return ""
}

// ScopeLevel is one ancestor of the parent chain of a scoped collection (ST-SUP-RC20-OC24): the entity of the row
// that the path parameter Param names, Link, the field of that row that names the next ancestor, or empty for the
// top ancestor, and By, the collection whose scope puts the ancestor in the path.
type ScopeLevel struct {
	Entity, Param, Link, By string
}

// FirstCollections gives the first declared collection of each entity, which the path of a child follows.
func FirstCollections(collections []Collection) map[string]Collection {
	first := map[string]Collection{}
	for _, c := range collections {
		if _, ok := first[c.Entity]; !ok {
			first[c.Entity] = c
		}
	}
	return first
}

// ParentChain gives the ancestors of the collection c, the parent first. Each ancestor follows the first collection
// of its entity (first, from FirstCollections), as the path does: its scope field links it to the ancestor above it.
// A chain that meets an entity again stops there. The handler of the collection checks each ancestor, and the
// architecture model states each check, from this one rule.
func ParentChain(c Collection, first map[string]Collection) []ScopeLevel {
	var chain []ScopeLevel
	seen := map[string]bool{c.Entity: true}
	param, entity, by := c.ScopeField(), c.ParentEntity(), c.Name
	for entity != "" && !seen[entity] {
		seen[entity] = true
		level := ScopeLevel{Entity: entity, Param: param, By: by}
		next, nextBy := "", ""
		if parent, ok := first[entity]; ok && len(parent.Scope) > 0 {
			level.Link, next, nextBy = parent.ScopeField(), parent.ParentEntity(), parent.Name
		}
		chain = append(chain, level)
		param, entity, by = level.Link, next, nextBy
	}
	return chain
}

// CreatesTopLevel tells whether a collection of collections serves the create of entity with no scope: the create
// of an entity with no parent. No row names it, so it is decided on the service-level resource of the tenant
// provisioning, <resource type>:<service name> (ST-SUP-RC29-OC01, ST-SUP-RC27-OC09).
func CreatesTopLevel(collections []Collection, entity string) bool {
	for _, c := range collections {
		if c.Entity == entity && len(c.Scope) == 0 && slices.Contains(c.Operations, OpCreate) {
			return true
		}
	}
	return false
}

// SlotDeclaration describes how a slot is bound in a service declaration.
type SlotDeclaration struct {
	Slot         string   `yaml:"slot"`
	Collection   string   `yaml:"collection,omitempty"`
	Gate         []string `yaml:"gate,omitempty"`
	Chain        []string `yaml:"chain,omitempty"`
	FanOut       []string `yaml:"fan-out,omitempty"`
	ShortCircuit bool     `yaml:"short_circuit,omitempty"`
	// RunsAs is tenant or system: the principal the fills run as (ST-DECL-EN05). Omitted means tenant.
	RunsAs string `yaml:"runs_as,omitempty"`
	// SlotAuthorization holds the relations its fills may write (ST-DECL-UC06).
	SlotAuthorization `yaml:",inline"`
	// AuditAnnotations is the string map the fills add to the audit record of the operation. Checking refuses
	// any other value, so it is read as any value.
	AuditAnnotations any `yaml:"audit_annotations,omitempty"`
}

// YAMLRenamedKeys marks SlotDeclaration as a parser.RenamedKeys: the strict
// decoder lets the renamed entity key through, so that UnmarshalYAML reports
// its rename.
func (SlotDeclaration) YAMLRenamedKeys() []string { return []string{"entity"} }

// UnmarshalYAML detects the deprecated `entity:` key (renamed to `collection:`
// in the Collection type system migration) and returns a migration error.
func (sd *SlotDeclaration) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.MappingNode {
		for i := 0; i < len(value.Content)-1; i += 2 {
			if value.Content[i].Value == "entity" {
				return fmt.Errorf("the 'entity' key in slot declarations has been renamed to 'collection' — please update your service.yaml")
			}
		}
	}
	type rawSD SlotDeclaration
	var raw rawSD
	if err := value.Decode(&raw); err != nil {
		return err
	}
	*sd = SlotDeclaration(raw)
	return nil
}

// ServiceDeclaration is the product team's service definition.
type ServiceDeclaration struct {
	ComponentNamespaces map[string]string `yaml:"component_namespaces,omitempty"`
	Kind                string            `yaml:"kind"`
	Name                string            `yaml:"name"`
	Archetype           string            `yaml:"archetype"`
	Language            string            `yaml:"language"`
	BasePath            string            `yaml:"base_path,omitempty"`
	ErrorTypeBase       string            `yaml:"error_type_base,omitempty"`
	Entities            []Entity          `yaml:"entities"`
	Collections         []Collection      `yaml:"-"`
	// DroppedEntities are the declared entities with dropped_by, and DroppedCollections the collections over them
	// (ST-GEN-Q47): reading the declaration takes them out of Entities and Collections, so that only the code that
	// gives back the stored shape of an earlier step reads them. EntityOrder and CollectionOrder are the names of
	// all entities and of all collections in the order of the declaration.
	DroppedEntities    []Entity          `yaml:"-"`
	DroppedCollections []Collection      `yaml:"-"`
	EntityOrder        []string          `yaml:"-"`
	CollectionOrder    []string          `yaml:"-"`
	Slots              []SlotDeclaration `yaml:"slots"`
	Mixins             []string          `yaml:"mixins,omitempty"`
	Overrides          map[string]any    `yaml:"overrides,omitempty"`
	FillModules        []FillModule      `yaml:"fill_modules,omitempty"`
	Idempotency        *Idempotency      `yaml:"idempotency,omitempty"`
	Tenancy            *Tenancy          `yaml:"tenancy,omitempty"`
	Audit              *Audit            `yaml:"audit,omitempty"`
	Controllers        []Controller      `yaml:"controllers,omitempty"`
	// Commands are the business commands that the service's own code carries out (ST-DECL-EN18).
	Commands []Command `yaml:"commands,omitempty"`
	// Queries are the reads that the service's own code carries out (ST-DECL-EN19).
	Queries []Query `yaml:"queries,omitempty"`
	// Observations are what the service's own code can observe and report (ST-DECL-EN20).
	Observations []Observation `yaml:"observations,omitempty"`
	// Modes are the modes the one binary runs: api, worker and controller; all three when it is empty
	// (ST-DECL-EN01).
	Modes []string `yaml:"modes,omitempty"`
	// Workers are the background work that the service's own code carries out (ST-DECL-EN25).
	Workers []Worker `yaml:"workers,omitempty"`
	// Subscriptions are the change events of other services that the service consumes (ST-DECL-EN28).
	Subscriptions []Subscription `yaml:"subscriptions,omitempty"`
	// Invariants are the rules across stored instances that the declaration states (ST-DECL-EN17).
	Invariants []DeclaredInvariant `yaml:"invariants,omitempty"`
	// SchemaChanges are the changes to the stored shape of the entities (ST-DECL-EN21), and SchemaHistory the
	// ordered steps that move them (ST-DECL-EN22).
	SchemaChanges []SchemaChange `yaml:"schema_changes,omitempty"`
	SchemaHistory []SchemaStep   `yaml:"schema_history,omitempty"`
	Authorization *Authorization `yaml:"authorization,omitempty"`
	// ExperimentalAcceptances are the components that are not ga that the service accepts (ST-DECL-EN07).
	ExperimentalAcceptances []ExperimentalAcceptance `yaml:"experimental_acceptances,omitempty"`
	// Delivery is the delivery stanza: the CI provider and the Argo CD delivery of the service (ST-DECL-EN01 field
	// Delivery stanza, ST-DLV-Q12 (A)). It has no field for a cluster, a namespace, an overlay, a pipeline
	// reference or a signer, so the strict decoder refuses each of them with its path.
	Delivery *Delivery `yaml:"delivery,omitempty"`
}

// Delivery is the delivery stanza of a declaration. Without CI, apply writes no CI file; without ArgoCD, it writes
// no delivery object.
type Delivery struct {
	CI     string          `yaml:"ci,omitempty"`
	ArgoCD *ArgoCDDelivery `yaml:"argocd,omitempty"`
}

// ArgoCDDelivery is the Argo CD part of the delivery stanza: the project, where the Kubernetes base of the service
// is, and where the platform's environment files are.
type ArgoCDDelivery struct {
	Project      string               `yaml:"project"`
	Source       DeliverySource       `yaml:"source"`
	Environments DeliveryEnvironments `yaml:"environments"`
}

// DeliverySource is the address of the service's repository and the path of its Kubernetes base in it.
type DeliverySource struct {
	Repo string `yaml:"repo"`
	Path string `yaml:"path"`
}

// DeliveryEnvironments is the repository, the revision and the path pattern of the platform's environment files.
type DeliveryEnvironments struct {
	Repo     string `yaml:"repo"`
	Revision string `yaml:"revision"`
	Path     string `yaml:"path"`
}

// UnmarshalYAML implements custom YAML unmarshaling for ServiceDeclaration.
// It parses the `collections:` key as a named map (preserving insertion order)
// and rejects the legacy `expose:` key with a migration error.
func (sd *ServiceDeclaration) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("expected a mapping node for service declaration")
	}

	// First pass: check for legacy expose: key and extract collections.
	for i := 0; i < len(value.Content)-1; i += 2 {
		key := value.Content[i].Value
		if key == "expose" {
			return fmt.Errorf("'expose' is no longer supported; use 'collections' instead — see spec for the named-map format")
		}
	}

	// Decode all standard fields using an alias to avoid recursion.
	type rawSD ServiceDeclaration
	var raw rawSD
	if err := value.Decode(&raw); err != nil {
		return err
	}

	// Parse collections: as an ordered named map.
	for i := 0; i < len(value.Content)-1; i += 2 {
		keyNode := value.Content[i]
		valNode := value.Content[i+1]
		if keyNode.Value != "collections" {
			continue
		}
		if valNode.Kind != yaml.MappingNode {
			return fmt.Errorf("'collections' must be a mapping")
		}
		for j := 0; j < len(valNode.Content)-1; j += 2 {
			collName := valNode.Content[j].Value
			var coll Collection
			if err := valNode.Content[j+1].Decode(&coll); err != nil {
				return fmt.Errorf("parsing collection %q: %w", collName, err)
			}
			coll.Name = collName
			raw.Collections = append(raw.Collections, coll)
		}
		break
	}

	*sd = ServiceDeclaration(raw)
	sd.splitDropped()
	return nil
}

// splitDropped takes each entity with dropped_by, and each collection over such an entity, out of Entities and
// Collections into DroppedEntities and DroppedCollections, and keeps the order of the declaration in EntityOrder and
// CollectionOrder (ST-GEN-Q47).
func (sd *ServiceDeclaration) splitDropped() {
	sd.EntityOrder, sd.CollectionOrder = nil, nil
	dropped := map[string]bool{}
	var live []Entity
	for _, entity := range sd.Entities {
		sd.EntityOrder = append(sd.EntityOrder, entity.Name)
		if entity.DroppedBy != "" {
			dropped[entity.Name] = true
			sd.DroppedEntities = append(sd.DroppedEntities, entity)
			continue
		}
		live = append(live, entity)
	}
	if len(dropped) == 0 {
		for _, collection := range sd.Collections {
			sd.CollectionOrder = append(sd.CollectionOrder, collection.Name)
		}
		return
	}
	sd.Entities = live
	var kept []Collection
	for _, collection := range sd.Collections {
		sd.CollectionOrder = append(sd.CollectionOrder, collection.Name)
		if dropped[collection.Entity] {
			sd.DroppedCollections = append(sd.DroppedCollections, collection)
			continue
		}
		kept = append(kept, collection)
	}
	sd.Collections = kept
}

// AllEntities gives every declared entity in the order of the declaration: the live ones and those with dropped_by.
func (sd *ServiceDeclaration) AllEntities() []Entity {
	return inOrder(sd.Entities, sd.DroppedEntities, sd.EntityOrder, func(e Entity) string { return e.Name })
}

// AllCollections gives every declared collection in the order of the declaration, with those over dropped entities.
func (sd *ServiceDeclaration) AllCollections() []Collection {
	return inOrder(sd.Collections, sd.DroppedCollections, sd.CollectionOrder, func(c Collection) string { return c.Name })
}

// inOrder joins live and dropped in the order of the names, with an item whose name order does not hold last.
func inOrder[T any](live, dropped []T, order []string, name func(T) string) []T {
	if len(dropped) == 0 {
		return live
	}
	all := append(append([]T(nil), live...), dropped...)
	rank := map[string]int{}
	for i, n := range order {
		rank[n] = i
	}
	at := func(item T) int {
		if i, ok := rank[name(item)]; ok {
			return i
		}
		return len(order)
	}
	sort.SliceStable(all, func(i, j int) bool { return at(all[i]) < at(all[j]) })
	return all
}

// MarshalYAML implements custom YAML marshaling for ServiceDeclaration.
// It serializes Collections as an ordered map under the `collections:` key.
func (sd ServiceDeclaration) MarshalYAML() (any, error) {
	// Build an ordered map for collections.
	collMap := yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, c := range sd.AllCollections() {
		keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: c.Name}
		// Marshal the collection without the Name field.
		valNode := &yaml.Node{}
		valBytes, err := yaml.Marshal(c)
		if err != nil {
			return nil, fmt.Errorf("marshaling collection %q: %w", c.Name, err)
		}
		if err := yaml.Unmarshal(valBytes, valNode); err != nil {
			return nil, fmt.Errorf("re-encoding collection %q: %w", c.Name, err)
		}
		// valNode is a document node; get the inner mapping.
		if len(valNode.Content) > 0 {
			collMap.Content = append(collMap.Content, keyNode, valNode.Content[0])
		}
	}

	// Build a top-level ordered map preserving field order.
	m := yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}

	addField := func(key, value string) {
		m.Content = append(m.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value},
		)
	}

	addField("kind", sd.Kind)
	addField("name", sd.Name)
	addField("archetype", sd.Archetype)
	addField("language", sd.Language)
	if sd.BasePath != "" {
		addField("base_path", sd.BasePath)
	}
	if sd.ErrorTypeBase != "" {
		addField("error_type_base", sd.ErrorTypeBase)
	}

	if len(sd.ComponentNamespaces) > 0 {
		node := &yaml.Node{}
		if err := node.Encode(sd.ComponentNamespaces); err != nil {
			return nil, err
		}
		m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "component_namespaces"}, node)
	}

	// Entities.
	if entities := sd.AllEntities(); len(entities) > 0 {
		entNode := &yaml.Node{}
		entBytes, err := yaml.Marshal(entities)
		if err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal(entBytes, entNode); err != nil {
			return nil, err
		}
		m.Content = append(m.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "entities"},
			entNode.Content[0],
		)
	}

	// Collections.
	if len(collMap.Content) > 0 {
		m.Content = append(m.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "collections"},
			&collMap,
		)
	}

	// Slots.
	if len(sd.Slots) > 0 {
		sNode := &yaml.Node{}
		sBytes, err := yaml.Marshal(sd.Slots)
		if err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal(sBytes, sNode); err != nil {
			return nil, err
		}
		m.Content = append(m.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "slots"},
			sNode.Content[0],
		)
	}

	// Fill modules.
	if len(sd.FillModules) > 0 {
		fNode := &yaml.Node{}
		if err := fNode.Encode(sd.FillModules); err != nil {
			return nil, err
		}
		m.Content = append(m.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "fill_modules"},
			fNode,
		)
	}

	// Idempotency.
	if sd.Idempotency != nil {
		iNode := &yaml.Node{}
		if err := iNode.Encode(sd.Idempotency); err != nil {
			return nil, err
		}
		m.Content = append(m.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "idempotency"},
			iNode,
		)
	}

	// Mixins.
	if len(sd.Mixins) > 0 {
		mNode := &yaml.Node{}
		mBytes, err := yaml.Marshal(sd.Mixins)
		if err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal(mBytes, mNode); err != nil {
			return nil, err
		}
		m.Content = append(m.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "mixins"},
			mNode.Content[0],
		)
	}

	// Overrides.
	if len(sd.Overrides) > 0 {
		keys := make([]string, 0, len(sd.Overrides))
		for k := range sd.Overrides {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		oNode := &yaml.Node{}
		oBytes, err := yaml.Marshal(sd.Overrides)
		if err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal(oBytes, oNode); err != nil {
			return nil, err
		}
		m.Content = append(m.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "overrides"},
			oNode.Content[0],
		)
	}

	if sd.Authorization != nil {
		aNode := &yaml.Node{}
		if err := aNode.Encode(sd.Authorization); err != nil {
			return nil, err
		}
		m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "authorization"}, aNode)
	}

	return &m, nil
}
