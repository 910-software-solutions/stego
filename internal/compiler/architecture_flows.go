package compiler

import (
	"fmt"
	"slices"
	"strings"

	"github.com/jsell-rh/stego/internal/declaration"
	"github.com/jsell-rh/stego/internal/registry"
)

// The request flows, the access matrix, the trust boundaries, the settings and the compliance facts of the
// architecture model (ST-GEN-DS04). A flow follows the layer order alone (ST-SUP-Q130 (A)): the operation, the
// authentication, the authorization the operation checks, the handler, the slots of phase before_store in the order
// the offering parts list them, the repository and the store, the outbox, the slots of phase after_store, and each
// event, with each point where it can stop.

// outboxPort is the port of the durable outbox: each event that the store writes to it is a sync outbox step and
// then an async event step (ST-SUP-Q130 (A)).
const outboxPort = "durable-outbox"

// writeOperations are the operations that write a row.
var writeOperations = []string{"create", "update", "delete", "upsert", "patch"}

// bodyOperations are the operations whose request has a body that the handler checks.
var bodyOperations = []string{"create", "update", "upsert", "patch"}

// provider gives the component bound to the port of the consumer, or nil.
func (d *deriver) provider(consumer, port string) (*registry.Component, ArchBinding) {
	binding, ok := d.bindings[consumer+"#"+port]
	if !ok {
		return nil, binding
	}
	return d.in.Components[strings.TrimPrefix(binding.Provider, "component:")], binding
}

// byLayer gives the active components of a layer, in the order of the active set.
func (d *deriver) byLayer(layer string) []*registry.Component {
	var list []*registry.Component
	for _, name := range d.in.Names {
		if c := d.in.Components[name]; c.Architecture.Layer == layer {
			list = append(list, c)
		}
	}
	return list
}

// flows builds one request flow for each collection operation.
func (d *deriver) flows() {
	h := d.handler
	if h == nil {
		return
	}
	var outbox *registry.Component
	var events []*registry.Component
	for _, name := range d.in.Names {
		c := d.in.Components[name]
		for _, p := range c.Provides {
			if p.Name == outboxPort && outbox == nil {
				outbox = c
			}
		}
	}
	for _, c := range d.byLayer("outside call") {
		if _, binding := d.provider(c.Name, outboxPort); binding.ID != "" {
			events = append(events, c)
		}
	}
	for _, op := range d.model.Operations {
		flow := ArchFlow{ID: op.Flow, Operation: op.ID, Steps: []ArchStep{}, Exits: []ArchExit{}, Source: op.Source}
		step := func(kind, element, via, text, mode string, source ArchSource) int {
			flow.Steps = append(flow.Steps, ArchStep{N: len(flow.Steps) + 1, Kind: kind, Element: element, ViaPort: via, Text: text, Mode: mode, Source: source})
			return len(flow.Steps)
		}
		exit := func(after int, when, result string, source ArchSource) {
			flow.Exits = append(flow.Exits, ArchExit{AfterStep: after, When: when, Result: result, Source: source})
		}
		text := op.Verb + " on " + strings.TrimPrefix(op.Collection, "collection:")
		if op.Method != "" {
			text = op.Method + " " + op.Path
		}
		step("operation", op.ID, "", text, "sync", op.Source)
		// Authentication: the providers bound to the handler's ports first, then each other component of the layer.
		seen := map[string]bool{}
		for _, p := range h.Requires {
			if c, binding := d.provider(h.Name, p.Name); c != nil && c.Architecture.Layer == "authentication" {
				seen[c.Name] = true
				n := step("authenticate", componentID(c.Name), binding.Port, c.Architecture.Flow, "sync", componentSource(c, "architecture"))
				exit(n, "the request has no valid token", "401 Unauthorized", componentSource(c, "architecture"))
			}
		}
		for _, c := range d.byLayer("authentication") {
			if !seen[c.Name] {
				step("authenticate", componentID(c.Name), "", c.Architecture.Flow, "sync", componentSource(c, "architecture"))
			}
		}
		if check, ok := d.checks[op.ID]; ok && check.Permission != "" {
			text := fmt.Sprintf("Decides the permission %s on the %s for the caller.", check.Permission, check.ResourceType)
			if op.Verb == "list" {
				text = fmt.Sprintf("Keeps only the rows on whose %s the caller holds the permission %s.", check.ResourceType, check.Permission)
			}
			n := step("authorize", check.ID, "", text, "sync", check.Source)
			if op.Verb != "list" {
				exit(n, "the caller does not hold the permission "+check.Permission, "403 Forbidden", check.Source)
			}
		}
		n := step("handle", componentID(h.Name), "", h.Architecture.Flow, "sync", componentSource(h, "architecture"))
		if slices.Contains(bodyOperations, op.Verb) {
			exit(n, "the request body is not valid for the entity", "400 Bad Request, with each failed member", componentSource(h, "architecture"))
		}
		before, after := d.flowSlots(op)
		for _, slot := range before {
			n := step("slot", slot.ID, "", slotText(slot), "sync", slot.Source)
			switch {
			case slot.Strategy == "gate":
				exit(n, "a fill of the gate denies", "the request stops with the refusal of the fill", slot.Source)
			case slot.Strategy == "chain" && slot.ShortCircuit:
				exit(n, "a fill of the chain stops it", "the request stops with the answer of the fill", slot.Source)
			}
		}
		for _, p := range h.Requires {
			c, binding := d.provider(h.Name, p.Name)
			if c == nil || c.Architecture.Layer != "repository" {
				continue
			}
			step("repository", componentID(c.Name), binding.Port, c.Architecture.Flow, "sync", componentSource(c, "architecture"))
			stores := d.byLayer("store")
			if len(stores) == 0 {
				step("store", componentID(c.Name), "", "The store of "+c.Name+" keeps the rows.", "sync", componentSource(c, "architecture"))
			}
			for _, s := range stores {
				step("store", componentID(s.Name), "", s.Architecture.Flow, "sync", componentSource(s, "architecture"))
			}
		}
		write := slices.Contains(writeOperations, op.Verb)
		afterMode := "sync"
		if write && outbox != nil {
			step("outbox", componentID(outbox.Name), "port:"+outboxPort, outbox.Architecture.Flow, "sync", componentSource(outbox, "architecture"))
			afterMode = "async"
		}
		for _, slot := range after {
			step("slot", slot.ID, "", slotText(slot), afterMode, slot.Source)
		}
		if write {
			for _, c := range events {
				step("event", componentID(c.Name), "port:"+outboxPort, c.Architecture.Flow, "async", componentSource(c, "architecture"))
			}
		}
		d.model.Flows = append(d.model.Flows, flow)
	}
}

// flowSlots gives the bound slots of the operation's collection that the operation calls, by phase, each phase in
// the order the offering parts list the slots.
func (d *deriver) flowSlots(op ArchOperation) (before, after []ArchSlot) {
	var bound []ArchSlot
	for _, slot := range d.model.Slots {
		if slot.Collection == op.Collection && slices.Contains(slot.Operations, op.Verb) {
			bound = append(bound, slot)
		}
	}
	slices.SortStableFunc(bound, func(a, b ArchSlot) int { return d.offers[a.Name].order - d.offers[b.Name].order })
	for _, slot := range bound {
		if slot.Phase == "before_store" {
			before = append(before, slot)
		} else {
			after = append(after, slot)
		}
	}
	return before, after
}

// slotText gives the text of a slot step: its strategy and its fills in order.
func slotText(slot ArchSlot) string {
	var names []string
	for _, fill := range slot.Fills {
		names = append(names, strings.TrimPrefix(fill, "fill:"))
	}
	fills := strings.Join(names, ", ")
	switch slot.Strategy {
	case "gate":
		return "Gate: " + fills + ". Every fill must allow."
	case "chain":
		if slot.ShortCircuit {
			return "Chain: " + fills + ", in order. A fill can stop the chain."
		}
		return "Chain: " + fills + ", in order."
	}
	return "Fan-out: " + fills + "."
}

// access builds what each operation and action checks, and the grant paths of each permission (ST-GEN-UC08-R12,
// R13).
func (d *deriver) access() {
	svc, m := d.in.Service, d.model
	m.Access = ArchAccess{Checks: []ArchCheck{}, Permissions: []ArchPermission{}, Roles: []ArchRole{}, RolesComplete: true}
	bound := svc.Authorization != nil
	if bound {
		m.Access.Schema = svc.Authorization.SchemaPath
	}
	add := func(subject, resourceType, permission, path string) {
		check := ArchCheck{ID: "check:" + subject, Subject: subject, Source: ArchSource{Declaration: path}}
		switch {
		case !bound:
			check.Reason, check.Source = "No authorization is bound, so no permission is checked.", ArchSource{Rule: "ST-DECL-UC06-R01"}
		case permission == "public":
			check.Public, check.ResourceType = true, resourceType
		default:
			check.ResourceType, check.Permission = resourceType, permission
			check.Grants = "permission:" + resourceType + "#" + permission
		}
		d.checks[subject] = check
		m.Access.Checks = append(m.Access.Checks, check)
	}
	entities := map[string]int{}
	for i, e := range svc.Entities {
		entities[e.Name] = i
	}
	for _, c := range svc.Collections {
		i, ok := entities[c.Entity]
		if !ok {
			continue
		}
		e := svc.Entities[i]
		for _, op := range c.Operations {
			subject := "operation:" + c.Name + "." + string(op)
			add(subject, e.ResourceType, e.Permissions[op], fmt.Sprintf("entities[%d].permissions.%s", i, op))
			if check := d.checks[subject]; check.Public {
				check.Reason = e.PublicReasons[op]
				d.checks[subject] = check
				m.Access.Checks[len(m.Access.Checks)-1] = check
			}
		}
	}
	for i, c := range svc.Commands {
		add("command:"+c.Name, c.ResourceType, c.Permission, fmt.Sprintf("commands[%d].permission", i))
	}
	for i, q := range svc.Queries {
		add("query:"+q.Name, q.ResourceType, q.Permission, fmt.Sprintf("queries[%d].permission", i))
	}
	for i, w := range svc.Workers {
		add("worker:"+w.Name, w.ResourceType, w.Permission, fmt.Sprintf("workers[%d].permission", i))
	}
	if p := tenantProvisioning(svc); p != nil {
		add("command:tenant-provision", p.ResourceType, p.Provision, "tenancy.provisioning.provision")
		add("command:tenant-retire", p.ResourceType, p.Retire, "tenancy.provisioning.retire")
	}
	permissions := map[string]bool{}
	roles := map[string]ArchRole{}
	for _, check := range m.Access.Checks {
		if check.Grants == "" || permissions[check.Grants] {
			continue
		}
		permissions[check.Grants] = true
		permission := ArchPermission{ID: check.Grants, ResourceType: check.ResourceType, Name: check.Permission, Paths: []ArchGrant{},
			Complete: d.in.Schema != nil, Source: ArchSource{Declaration: "authorization.schema_path"}}
		if d.in.Schema != nil {
			var granted []ArchRole
			permission.Paths, granted, permission.Complete = grantPaths(d.in.Schema, check.ResourceType, check.Permission)
			m.Access.RolesComplete = m.Access.RolesComplete && permission.Complete
			for _, role := range granted {
				roles[role.ID] = role
			}
		}
		m.Access.Permissions = append(m.Access.Permissions, permission)
	}
	for _, role := range roles {
		m.Access.Roles = append(m.Access.Roles, role)
	}
	for _, check := range m.Access.Checks {
		if check.Permission != "" {
			m.AuthzPoints = append(m.AuthzPoints, ArchPoint{ID: "authz:" + check.Subject, Element: check.Subject,
				Text: fmt.Sprintf("Decides the permission %s on the %s for the verified caller.", check.Permission, check.ResourceType), Source: check.Source})
		}
	}
}

// defaultRule gives the rule that sets the default of a setting at the path.
func defaultRule(path string) string {
	switch {
	case strings.HasPrefix(path, "tenancy.") || strings.HasPrefix(path, "audit."):
		return "ST-DECL-UC05-R01"
	case strings.HasSuffix(path, ".tenant"):
		return "ST-DECL-UC05-R04"
	case strings.HasSuffix(path, ".sensitivity"):
		return "ST-DECL-UC05-R06"
	}
	return "ST-DECL-INV18"
}

// settingValue gives a setting value as a JSON scalar: a list becomes its items joined by a comma and a space.
func settingValue(value any) any {
	switch v := value.(type) {
	case string, bool, int, int64, float64:
		return v
	case []string:
		return strings.Join(v, ", ")
	case nil:
		return ""
	}
	return fmt.Sprint(value)
}

// trust builds the settings, the authentication points and the trust boundaries.
func (d *deriver) trust() {
	m, svc := d.model, d.in.Service
	settings := map[string]ArchSetting{}
	for _, s := range declaration.ResolvedSettings(svc) {
		setting := ArchSetting{ID: "setting:" + s.Path, Path: s.Path, Value: settingValue(s.Value), Default: s.Default, Source: ArchSource{Declaration: s.Path}}
		if s.Default {
			setting.Source = ArchSource{Default: defaultRule(s.Path)}
		}
		settings[s.Path] = setting
		m.Settings = append(m.Settings, setting)
	}
	authenticated := map[string]bool{}
	for _, b := range m.Bindings {
		c := d.in.Components[strings.TrimPrefix(b.Provider, "component:")]
		if c.Architecture.Layer == "authentication" && !authenticated[c.Name] {
			authenticated[c.Name] = true
			m.AuthnPoints = append(m.AuthnPoints, ArchPoint{ID: "authn:" + c.Name, Element: componentID(c.Name), Text: c.Architecture.Flow, Source: componentSource(c, "architecture")})
		}
	}
	rule := ArchSource{Rule: "ST-GEN-UC08-R11"}
	network := ArchBoundary{ID: "boundary:network", Kind: "network", Label: "The network edge of the service", Contains: []string{"container:" + d.service}, CrossedBy: []string{}, Source: rule}
	for _, op := range m.Operations {
		network.CrossedBy = append(network.CrossedBy, op.ID)
	}
	for _, need := range m.Needs {
		if need.Kind == "ingress" {
			network.CrossedBy = append(network.CrossedBy, need.ID)
		}
	}
	m.Boundaries = append(m.Boundaries, network)
	if len(m.AuthnPoints) > 0 {
		identity := ArchBoundary{ID: "boundary:verified-identity", Kind: "verified identity", Label: "The verified identity of the caller", Contains: []string{}, CrossedBy: []string{}, Source: rule}
		after := slices.Index(registry.Layers, "authentication")
		for _, c := range m.Components {
			if slices.Index(registry.Layers, c.Layer) > after {
				identity.Contains = append(identity.Contains, c.ID)
			}
		}
		for _, p := range m.AuthnPoints {
			identity.CrossedBy = append(identity.CrossedBy, p.Element)
		}
		m.Boundaries = append(m.Boundaries, identity)
	}
	if kind, ok := settings["tenancy.kind"]; ok && kind.Value == "pooled" {
		scope := ArchBoundary{ID: "boundary:tenant-scope", Kind: "tenant scope", Label: "The rows of one tenant", Contains: []string{}, CrossedBy: []string{}, Source: kind.Source}
		for _, e := range m.Entities {
			if e.TenantKind == "owned" {
				scope.Contains = append(scope.Contains, e.ID)
			}
		}
		if _, ok := settings["tenancy.claim"]; ok {
			scope.CrossedBy = append(scope.CrossedBy, "setting:tenancy.claim")
		}
		m.Boundaries = append(m.Boundaries, scope)
	}
	if len(m.Fills) > 0 {
		fills := ArchBoundary{ID: "boundary:fill-code", Kind: "fill code", Label: "The owner code of each fill", Contains: []string{}, CrossedBy: []string{}, Source: rule}
		for _, f := range m.Fills {
			fills.Contains = append(fills.Contains, f.ID)
			for _, implements := range f.Implements {
				if !slices.Contains(fills.CrossedBy, implements) {
					fills.CrossedBy = append(fills.CrossedBy, implements)
				}
			}
		}
		slices.Sort(fills.CrossedBy)
		m.Boundaries = append(m.Boundaries, fills)
	}
	for _, f := range m.Flows {
		for _, s := range f.Steps {
			if s.Kind == "outbox" {
				process := ArchBoundary{ID: "boundary:process", Kind: "process", Label: "The work that runs later, from the outbox", Contains: []string{}, CrossedBy: []string{s.Element}, Source: s.Source}
				for _, flow := range m.Flows {
					for _, later := range flow.Steps {
						if later.Mode == "async" && !slices.Contains(process.Contains, later.Element) {
							process.Contains = append(process.Contains, later.Element)
						}
					}
				}
				slices.Sort(process.Contains)
				m.Boundaries = append(m.Boundaries, process)
				return
			}
		}
	}
}

// fact gives one fact of a topic.
func fact(topic, about, name string, value any, state string, source ArchSource) ArchFact {
	return ArchFact{ID: "fact:" + topic + "/" + about + "/" + strings.ReplaceAll(name, " ", "-"), About: about, Name: name, Value: value, State: state, Source: source}
}

// settingFact gives the fact of a setting, stated or default, or unstated when the declaration and the defaults
// give it no value.
func settingFact(topic, about, name string, setting ArchSetting, ok bool) ArchFact {
	if !ok {
		return fact(topic, about, name, nil, "unstated", ArchSource{Rule: "ST-GEN-UC16-R04"})
	}
	state := "stated"
	if setting.Default {
		state = "default"
	}
	return fact(topic, setting.ID, name, setting.Value, state, setting.Source)
}

// ComplianceTopics is the one list of the compliance topics, in the order of the model (ST-GEN-UC16-R01): the model
// holds a topic for each, and stego architecture facts (ST-GEN-CLI09) takes each as its --topic. A new topic is added
// here and given its facts in compliance.
var ComplianceTopics = []string{"personal-data", "tenant-isolation", "access", "audit", "data-protection", "owner-code", "retention"}

// compliance builds the facts of each compliance topic (ST-GEN-UC16-R01) from the model alone.
func (d *deriver) compliance() {
	m := d.model
	settings := map[string]ArchSetting{}
	for _, s := range m.Settings {
		settings[s.Path] = s
	}
	setting := func(topic, name, path string) ArchFact {
		s, ok := settings[path]
		return settingFact(topic, "system:"+d.service, name, s, ok)
	}
	system := "system:" + d.service
	unstated := ArchSource{Rule: "ST-GEN-UC16-R04"}
	topics := map[string][]ArchFact{}
	put := func(topic string, f ArchFact) { topics[topic] = append(topics[topic], f) }
	var store string
	var storeSource ArchSource
	if d.handler != nil {
		for _, p := range d.handler.Requires {
			if c, binding := d.provider(d.handler.Name, p.Name); c != nil && c.Architecture.Layer == "repository" && store == "" {
				store, storeSource = componentID(c.Name), binding.Source
			}
		}
	}
	marked := 0
	for i, e := range m.Entities {
		personal := false
		for j, f := range e.Fields {
			if f.Sensitivity == "public" {
				continue
			}
			marked++
			personal = true
			path := fmt.Sprintf("entities[%d].fields[%d].sensitivity", i, j)
			put("personal-data", fact("personal-data", f.ID, "sensitivity", f.Sensitivity, "stated", ArchSource{Declaration: path}))
		}
		if personal {
			if store != "" {
				put("personal-data", fact("personal-data", e.ID, "stored by", store, "stated", storeSource))
			} else {
				put("personal-data", fact("personal-data", e.ID, "stored by", nil, "unstated", unstated))
			}
			// A read of an entity with a pii field is audited at every level, but only when audit is on (security
			// review of #341, M2).
			if enabled, ok := settings["audit.enabled"]; ok && enabled.Value == false {
				state := "stated"
				if enabled.Default {
					state = "default"
				}
				put("audit", fact("audit", e.ID, "reads audited", false, state, enabled.Source))
			} else {
				put("audit", fact("audit", e.ID, "reads audited", true, "default", ArchSource{Default: "ST-DECL-UC05-R06"}))
			}
		}
	}
	put("personal-data", fact("personal-data", system, "fields marked pii or secret", marked, "stated", ArchSource{Rule: "ST-DECL-UC05-R06"}))
	for _, name := range []string{"kind", "key", "claim"} {
		put("tenant-isolation", setting("tenant-isolation", "tenancy "+name, "tenancy."+name))
	}
	pooled := settings["tenancy.kind"].Value == "pooled"
	// The tenancy component makes the tenant column and the row-level security policy of each owned table
	// (ST-SUP-RC27-OC03, ST-SUP-RC01-OC40); without it the inputs state neither (security review of #341, L1).
	var tenancy *registry.Component
	if slices.Contains(d.in.Names, "tenancy") {
		tenancy = d.in.Components["tenancy"]
	}
	for _, e := range m.Entities {
		state, source := "stated", ArchSource{Declaration: e.Source.Declaration + ".tenant"}
		if s, ok := settings[e.Source.Declaration+".tenant"]; ok && s.Default {
			state, source = "default", s.Source
		}
		put("tenant-isolation", fact("tenant-isolation", e.ID, "tenant kind", e.TenantKind, state, source))
		if pooled && e.TenantKind == "owned" {
			if tenancy == nil {
				put("tenant-isolation", fact("tenant-isolation", e.ID, "tenant key column", nil, "unstated", unstated))
				put("tenant-isolation", fact("tenant-isolation", e.ID, "row-level security policy", nil, "unstated", unstated))
				continue
			}
			key, ok := settings["tenancy.key"]
			keyFact := settingFact("tenant-isolation", e.ID, "tenant key column", key, ok)
			keyFact.About, keyFact.ID = e.ID, "fact:tenant-isolation/"+e.ID+"/tenant-key-column"
			put("tenant-isolation", keyFact)
			put("tenant-isolation", fact("tenant-isolation", e.ID, "row-level security policy", true, "stated", componentSource(tenancy, "")))
		}
	}
	if pooled && m.Access.Schema != "" {
		put("tenant-isolation", fact("tenant-isolation", system, "tenant-qualified resource IDs", true, "default", ArchSource{Default: "ST-SUP-RP06-LC22"}))
	}
	for _, p := range m.AuthnPoints {
		put("access", fact("access", p.Element, "authenticates the caller", true, "stated", p.Source))
	}
	for _, c := range m.Access.Checks {
		value := any(nil)
		state := "unstated"
		switch {
		case c.Public:
			value, state = "public", "stated"
		case c.Permission != "":
			value, state = c.ResourceType+"#"+c.Permission, "stated"
		}
		put("access", fact("access", c.Subject, "checks", value, state, c.Source))
	}
	for _, path := range []string{"audit.enabled", "audit.level", "audit.chain", "audit.anchor", "audit.sink", "audit.pii"} {
		put("audit", setting("audit", strings.TrimPrefix(path, "audit."), path))
	}
	put("data-protection", fact("data-protection", system, "encryption at rest", nil, "unstated", unstated))
	put("data-protection", fact("data-protection", system, "encryption in transit", nil, "unstated", unstated))
	for _, f := range m.Fills {
		put("owner-code", fact("owner-code", f.ID, "implements", strings.Join(f.Implements, ", "), "stated", f.Source))
	}
	put("retention", setting("retention", "audit retention", "audit.retention"))
	put("retention", fact("retention", system, "data retention", nil, "unstated", unstated))
	for _, topic := range ComplianceTopics {
		facts := topics[topic]
		if facts == nil {
			facts = []ArchFact{}
		}
		m.Compliance = append(m.Compliance, ArchTopic{ID: "topic:" + topic, Facts: facts})
	}
}

// aspects states each of the five aspects as present or empty, with the reason of an empty one (ST-GEN-UC08-R06).
func (d *deriver) aspects() {
	m := d.model
	state := func(present bool, reason string) ArchAspect {
		if present {
			return ArchAspect{State: "present"}
		}
		return ArchAspect{State: "empty", Reason: reason}
	}
	flowReason := reasonNoOperation
	if len(m.Operations) > 0 && d.handler == nil {
		flowReason = reasonNoServer
	}
	m.Aspects = ArchAspects{
		Structure:       state(len(m.Components) > 0, reasonNoComponents),
		DataModel:       state(len(m.Entities) > 0, reasonNoEntity),
		RequestFlows:    state(len(m.Flows) > 0, flowReason),
		PlatformEdges:   state(len(m.Needs) > 0, reasonNoNeed),
		TrustAndTenancy: state(true, ""),
	}
}

// emptyLists gives each list of the model that holds nothing as [], as the schema requires.
func emptyLists(m *ArchitectureModel) {
	empty := func(list any) {
		switch l := list.(type) {
		case *[]ArchContainer:
			if *l == nil {
				*l = []ArchContainer{}
			}
		case *[]ArchComponent:
			if *l == nil {
				*l = []ArchComponent{}
			}
		case *[]ArchMixin:
			if *l == nil {
				*l = []ArchMixin{}
			}
		case *[]ArchPort:
			if *l == nil {
				*l = []ArchPort{}
			}
		case *[]ArchBinding:
			if *l == nil {
				*l = []ArchBinding{}
			}
		case *[]ArchSlot:
			if *l == nil {
				*l = []ArchSlot{}
			}
		case *[]ArchFill:
			if *l == nil {
				*l = []ArchFill{}
			}
		case *[]ArchEntity:
			if *l == nil {
				*l = []ArchEntity{}
			}
		case *[]ArchReference:
			if *l == nil {
				*l = []ArchReference{}
			}
		case *[]ArchCollection:
			if *l == nil {
				*l = []ArchCollection{}
			}
		case *[]ArchOperation:
			if *l == nil {
				*l = []ArchOperation{}
			}
		case *[]ArchAction:
			if *l == nil {
				*l = []ArchAction{}
			}
		case *[]ArchFlow:
			if *l == nil {
				*l = []ArchFlow{}
			}
		case *[]ArchNeed:
			if *l == nil {
				*l = []ArchNeed{}
			}
		case *[]ArchBoundary:
			if *l == nil {
				*l = []ArchBoundary{}
			}
		case *[]ArchSetting:
			if *l == nil {
				*l = []ArchSetting{}
			}
		case *[]ArchPoint:
			if *l == nil {
				*l = []ArchPoint{}
			}
		}
	}
	for _, list := range []any{&m.Containers, &m.Components, &m.Mixins, &m.Ports, &m.Bindings, &m.Slots, &m.Fills, &m.Entities, &m.References,
		&m.Collections, &m.Operations, &m.Actions, &m.Flows, &m.Needs, &m.Boundaries, &m.Settings, &m.AuthnPoints, &m.AuthzPoints} {
		empty(list)
	}
}
