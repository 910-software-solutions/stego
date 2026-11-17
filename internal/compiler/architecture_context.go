package compiler

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
)

// The context of the architecture model (ST-GEN-DS04, ST-GEN-UC08-R01, R03): the actors outside the service that
// an input states, the relations of the service container with them and with the containers that needs state, and
// the layer of each step of a request flow. Each actor and relation names the input that states it; the deriver
// adds no actor that no input states.

// ArchActor is one actor outside the service that an input states: the caller of the collection operations, the
// issuer of the tokens that an authentication component checks, the authorization service that the declaration
// binds, an outside system that an outside call component calls, or the store of a repository.
type ArchActor struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Name string `json:"name"`
	// StatedBy is the element of the model that makes the actor known, when one does: a component or a need.
	StatedBy string     `json:"stated_by,omitempty"`
	Source   ArchSource `json:"source"`
}

// ArchRelation is one relation between two of the containers and actors: who calls whom, in plain words, through
// which component, and whether it runs in the request or later.
type ArchRelation struct {
	ID      string     `json:"id"`
	From    string     `json:"from"`
	To      string     `json:"to"`
	Text    string     `json:"text"`
	Through string     `json:"through,omitempty"`
	Mode    string     `json:"mode"`
	Source  ArchSource `json:"source"`
}

// The kinds of actor.
const (
	actorCaller        = "caller"
	actorTokenIssuer   = "token issuer"
	actorAuthorization = "authorization service"
	actorOutside       = "outside system"
	actorStore         = "store"
)

// The port of an authorization decision, which the component that asks the authorization service provides.
const authzDecisionPort = "authz-decision"

// context builds the actors and the relations of the model. It runs after the flows, so the caller is known only
// when a flow enters by an operation.
func (d *deriver) context() {
	m := d.model
	service := "container:" + d.service
	actors := map[string]ArchActor{}
	var relations []ArchRelation
	relate := func(from, to, text, through, mode string, source ArchSource) {
		id := "relation:" + shortID(from) + "/" + shortID(to)
		for _, r := range relations {
			if r.ID == id {
				return
			}
		}
		relations = append(relations, ArchRelation{ID: id, From: from, To: to, Text: text, Through: through, Mode: mode, Source: source})
	}
	actor := func(a ArchActor) string {
		if _, ok := actors[a.ID]; !ok {
			actors[a.ID] = a
		}
		return a.ID
	}
	// The caller: the declaration states the collection operations, and a flow enters by each.
	if len(m.Flows) > 0 && d.handler != nil {
		source := ArchSource{Declaration: "collections"}
		id := actor(ArchActor{ID: "actor:caller", Kind: actorCaller, Name: "Caller", Source: source})
		relate(id, service, "calls "+plural(len(m.Flows), "operation"), componentID(d.handler.Name), "sync", source)
	}
	// The issuer of the tokens of each authentication component whose part takes an issuer: the declaration names
	// it in overrides.<component>.issuer, or the registry part states that the component checks the tokens of one.
	for _, name := range d.in.Names {
		c := d.in.Components[name]
		if c.Architecture.Layer != "authentication" {
			continue
		}
		if _, takes := c.Config["issuer"]; !takes {
			continue
		}
		a := ArchActor{ID: "actor:token-issuer." + name, Kind: actorTokenIssuer, Name: "Token issuer of " + name, StatedBy: componentID(name),
			Source: componentSource(c, "config.issuer")}
		if settings, ok := d.in.Service.Overrides[name].(map[string]any); ok {
			if issuer, ok := settings["issuer"].(string); ok && issuer != "" {
				a.Source = ArchSource{Declaration: "overrides." + name + ".issuer"}
				if shown, ok := issuerName(issuer); ok {
					a.Name = shown
				}
			}
		}
		relate(service, actor(a), "checks tokens", componentID(name), "sync", a.Source)
	}
	// The authorization service that the declaration binds, asked through the component that provides decisions.
	if authz := d.in.Service.Authorization; authz != nil && authz.Provider != "" {
		source := ArchSource{Declaration: "authorization.provider"}
		id := actor(ArchActor{ID: "actor:authorization." + authz.Provider, Kind: actorAuthorization, Name: authz.Provider, Source: source})
		through := ""
		for _, name := range d.in.Names {
			for _, p := range d.in.Components[name].Provides {
				if p.Name == authzDecisionPort && through == "" {
					through = componentID(name)
				}
			}
		}
		relate(service, id, "asks for permission decisions", through, "sync", source)
	}
	// The containers that needs state, and the actors of the other needs that name something outside.
	stated := map[string]bool{}
	for _, n := range m.Needs {
		through := ""
		if len(n.StatedBy) > 0 {
			through = n.StatedBy[0]
		}
		switch n.Kind {
		case "backing store":
			relate(service, "container:"+n.Name, "keeps rows", through, "sync", n.Source)
		case "topic":
			relate(service, "container:"+n.Name, "publishes change messages, later", through, "async", n.Source)
		case "identity issuer":
			id := actor(ArchActor{ID: "actor:identity-issuer." + n.Name, Kind: actorTokenIssuer, Name: n.Name, StatedBy: n.ID, Source: n.Source})
			relate(service, id, "checks tokens", through, "sync", n.Source)
		case "egress":
			id := actor(ArchActor{ID: "actor:egress." + n.Name, Kind: actorOutside, Name: n.Name, StatedBy: n.ID, Source: n.Source})
			relate(service, id, "calls", through, "sync", n.Source)
		default:
			continue
		}
		for _, c := range n.StatedBy {
			stated[c] = true
		}
	}
	// The system that each outside call component calls, unless a need of it names that system already.
	for _, c := range d.byLayer("outside call") {
		id := componentID(c.Name)
		if stated[id] {
			continue
		}
		source := componentSource(c, "architecture.layer")
		a := actor(ArchActor{ID: "actor:outside." + c.Name, Kind: actorOutside, Name: "System that " + c.Name + " calls", StatedBy: id, Source: source})
		text, mode := "calls", "sync"
		if _, binding := d.provider(c.Name, outboxPort); binding.ID != "" {
			text, mode = "sends change messages, later", "async"
		}
		relate(service, a, text, id, mode, source)
	}
	// The store of each repository that the handler writes through, when no store component or need states one:
	// the same store that the store step of a flow names.
	if d.handler != nil && len(d.byLayer("store")) == 0 {
		for _, p := range d.handler.Requires {
			c, _ := d.provider(d.handler.Name, p.Name)
			if c == nil || c.Architecture.Layer != "repository" || stated[componentID(c.Name)] {
				continue
			}
			source := componentSource(c, "architecture.store")
			a := actor(ArchActor{ID: "actor:store." + c.Name, Kind: actorStore, Name: c.Architecture.Store, StatedBy: componentID(c.Name), Source: source})
			relate(service, a, "keeps rows", componentID(c.Name), "sync", source)
		}
	}
	m.Actors = []ArchActor{}
	for _, a := range actors {
		m.Actors = append(m.Actors, a)
	}
	sortByID(m.Actors, func(e ArchActor) string { return e.ID })
	m.Relations = relations
	if m.Relations == nil {
		m.Relations = []ArchRelation{}
	}
	sortByID(m.Relations, func(e ArchRelation) string { return e.ID })
}

// flowStepLayer gives the layer of a step: the edge for the operation, the authentication layer for the decision
// of a permission, the slot layer for a slot, the store layer for the store, and else the layer of the component
// that the step names.
func flowStepLayer(components map[string]string, s ArchStep) string {
	switch s.Kind {
	case "operation":
		return "edge"
	case "authorize":
		return "authentication"
	case "slot":
		return "slot"
	case "store":
		return "store"
	}
	return components[s.Element]
}

// stepLayers sets the layer of each step of each flow.
func (d *deriver) stepLayers() {
	components := map[string]string{}
	for _, c := range d.model.Components {
		components[c.ID] = c.Layer
	}
	for i := range d.model.Flows {
		for j := range d.model.Flows[i].Steps {
			d.model.Flows[i].Steps[j].Layer = flowStepLayer(components, d.model.Flows[i].Steps[j])
		}
	}
}

// declareContext gives the IDs of the actors and the relations to the rules between the elements of the model.
func declareContext(m *ArchitectureModel, declare func(string)) {
	for _, a := range m.Actors {
		declare(a.ID)
	}
	for _, r := range m.Relations {
		declare(r.ID)
	}
}

// checkContext checks that each actor and relation names elements of the model, that a relation joins the
// service container with a container or an actor, and that each step names a layer that fits its kind and element.
func checkContext(m *ArchitectureModel, refer func(owner string, refs ...string)) error {
	for _, a := range m.Actors {
		refer(a.ID, a.StatedBy)
	}
	ends := map[string]bool{}
	for _, c := range m.Containers {
		ends[c.ID] = true
	}
	for _, a := range m.Actors {
		ends[a.ID] = true
	}
	for _, r := range m.Relations {
		refer(r.ID, r.From, r.To, r.Through)
		if !ends[r.From] || !ends[r.To] {
			return fmt.Errorf("%s joins %s and %s, and a relation joins only containers and actors", r.ID, r.From, r.To)
		}
	}
	components := map[string]string{}
	for _, c := range m.Components {
		components[c.ID] = c.Layer
	}
	for _, f := range m.Flows {
		for _, s := range f.Steps {
			if s.Layer == "" {
				continue
			}
			if want := flowStepLayer(components, s); want != "" && want != s.Layer {
				return fmt.Errorf("%s: step %d (%s %s) is in the layer %s, not %s", f.ID, s.N, s.Kind, s.Element, s.Layer, want)
			}
		}
	}
	return nil
}

// actorKinds are the kinds of actor in the order of the overview, top to bottom.
// issuerName gives the issuer URL that the model may show: its scheme, host and path, with no query or fragment, so
// a user name, a password or a token that a declaration put in the URL by mistake is not published with the model
// (security review of PR #345, F1). It gives false for a value that is not an https URL with a host, which the
// model does not show, and for a value that holds '@' at any position, also as %40 in the path. The parse cuts the
// fragment and then the query before it reads the user info, so a '#', a '?' or a '/' in a user name or a password,
// as in https://u:12#secret@host/realm or https://u:123/secret@host/realm, puts a part of the user info in the host
// or the path (security reviews of PR #350 and PR #362). An issuer URL has no user info, so it needs no '@'.
func issuerName(issuer string) (string, bool) {
	if strings.Contains(issuer, "@") {
		return "", false
	}
	u, err := url.Parse(issuer)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Opaque != "" || strings.Contains(u.Path, "@") {
		return "", false
	}
	u.User, u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment = nil, "", false, "", ""
	return u.String(), true
}

var actorKinds = []string{actorCaller, actorTokenIssuer, actorAuthorization, actorStore, actorOutside}

// actorOrder gives the place of an actor kind in the overview.
func actorOrder(kind string) int { return slices.Index(actorKinds, kind) }

// isActor says whether an ID names an actor.
func isActor(id string) bool { return strings.HasPrefix(id, "actor:") }
