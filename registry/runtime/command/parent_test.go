package command

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/grpc/codes"

	"github.com/jsell-rh/stego/registry/runtime/authz"
)

// The entities of the reference test: an order that refers to a customer and a product, and a node of a tree whose
// parent is optional (ST-DECL-EN02).
type (
	product     struct{ ID, Name string }
	placedOrder struct{ ID, CustomerID, ProductID string }
	node        struct{ ID, ParentID string }
)

const referenceDDL = `
CREATE TABLE products (tenant_id text NOT NULL, id text NOT NULL, name text NOT NULL,
  version bigint NOT NULL DEFAULT 1, authz_token text NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, id));
CREATE TABLE placed_orders (tenant_id text NOT NULL, id text NOT NULL, customer_id text NOT NULL, product_id text NOT NULL,
  version bigint NOT NULL DEFAULT 1, authz_token text NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, id));
CREATE TABLE nodes (tenant_id text NOT NULL, id text NOT NULL, parent_id text NOT NULL,
  version bigint NOT NULL DEFAULT 1, authz_token text NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, id));
`

// referenceEntities makes the tables and the entities of the reference test: PlacedOrder refers to Customer by
// customer_id with the permission place_order and the relation customer, and to Product by product_id with the
// permission view and the relation product; Node refers to its optional parent Node by parent_id with the permission
// add_child and the relation parent.
func referenceEntities(e *env) (*Entity[product], *Entity[placedOrder], *Entity[node]) {
	t := e.t
	t.Helper()
	e.exec(referenceDDL)
	for _, table := range []string{"products", "placed_orders", "nodes"} {
		e.exec("ALTER TABLE "+table+" ENABLE ROW LEVEL SECURITY", "ALTER TABLE "+table+" FORCE ROW LEVEL SECURITY",
			"CREATE POLICY "+table+"_tenant ON "+table+" USING (tenant_id = current_setting('app.tenant', true)) WITH CHECK (tenant_id = current_setting('app.tenant', true))",
			"GRANT SELECT, INSERT, UPDATE, DELETE ON "+table+" TO "+e.roles[0])
	}
	products, err := NewEntity(spec("Product", "product", "products", []string{"name"},
		func(s Scanner) (product, Meta, error) {
			var v product
			var m Meta
			err := s.Scan(&v.ID, &v.Name, &m.Version, &m.Token)
			m.ID = v.ID
			return v, m, err
		},
		func(v product) []any { return []any{v.ID, v.Name} },
		func(v product) map[string]any { return map[string]any{"id": v.ID, "name": v.Name} }))
	if err != nil {
		t.Fatal(err)
	}
	orderSpec := spec("PlacedOrder", "order", "placed_orders", []string{"customer_id", "product_id"},
		func(s Scanner) (placedOrder, Meta, error) {
			var v placedOrder
			var m Meta
			err := s.Scan(&v.ID, &v.CustomerID, &v.ProductID, &m.Version, &m.Token)
			m.ID = v.ID
			return v, m, err
		},
		func(v placedOrder) []any { return []any{v.ID, v.CustomerID, v.ProductID} },
		func(v placedOrder) map[string]any {
			return map[string]any{"id": v.ID, "customer_id": v.CustomerID, "product_id": v.ProductID}
		})
	orderSpec.Parent = []Reference[placedOrder]{
		{Entity: "Customer", ResourceType: "customer", ID: func(v placedOrder) string { return v.CustomerID },
			Permission: "place_order", Relation: "customer"},
		{Entity: "Product", ResourceType: "product", ID: func(v placedOrder) string { return v.ProductID },
			Permission: "view", Relation: "product"},
	}
	orders, err := NewEntity(orderSpec)
	if err != nil {
		t.Fatal(err)
	}
	nodeSpec := spec("Node", "node", "nodes", []string{"parent_id"},
		func(s Scanner) (node, Meta, error) {
			var v node
			var m Meta
			err := s.Scan(&v.ID, &v.ParentID, &m.Version, &m.Token)
			m.ID = v.ID
			return v, m, err
		},
		func(v node) []any { return []any{v.ID, v.ParentID} },
		func(v node) map[string]any { return map[string]any{"id": v.ID, "parent_id": v.ParentID} })
	nodeSpec.Parent = []Reference[node]{{Entity: "Node", ResourceType: "node", ID: func(v node) string { return v.ParentID },
		Permission: "add_child", Relation: "parent"}}
	nodes, err := NewEntity(nodeSpec)
	if err != nil {
		t.Fatal(err)
	}
	return products, orders, nodes
}

// tuples gives the relationship touches and deletes that the outbox holds for the relay in tenant t1, each as
// op resource_type#relation@subject_type:subject_key with the tenant part of each ID dropped, in the order of the
// outbox.
func (e *env) tuples() []string {
	e.t.Helper()
	rows, err := e.admin.QueryContext(e.ctx, `SELECT p->>'op', p->'tuple'->>'resource_type', p->'tuple'->>'resource_id',
  p->'tuple'->>'relation', p->'tuple'->>'subject_type', p->'tuple'->>'subject_id'
 FROM (SELECT convert_from(payload, 'UTF8')::jsonb AS p, created_at, id FROM stego_outbox.messages
 WHERE tenant_id = 't1' AND destination = 'authz') m ORDER BY created_at, id`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var op, resourceType, resourceID, relation, subjectType, subjectID string
		if err := rows.Scan(&op, &resourceType, &resourceID, &relation, &subjectType, &subjectID); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, op+" "+resourceType+":"+keyOf(resourceID)+"#"+relation+"@"+subjectType+":"+keyOf(subjectID))
	}
	if err := rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	return out
}

// keyOf is the key of a tenant-qualified resource ID of the tenant t1, or the ID with a mark when it is not one.
func keyOf(id string) string {
	tenantPart, keyPart, ok := strings.Cut(id, "/")
	tenantID, err1 := base64.RawURLEncoding.DecodeString(tenantPart)
	key, err2 := base64.RawURLEncoding.DecodeString(keyPart)
	if !ok || err1 != nil || err2 != nil || string(tenantID) != "t1" {
		return "UNQUALIFIED(" + id + ")"
	}
	return string(key)
}

func TestReferences(t *testing.T) {
	t.Run("ST-SUP-RP10-LC05-T12 A create decides the declared permission of each set reference and writes its relation", func(t *testing.T) {
		e := newEnv(t)
		products, orders, nodes := referenceEntities(e)
		e.exec(`INSERT INTO customers (tenant_id, id, name, email) VALUES ('t1', 'c1', 'C1', 'c1@example.com'), ('t1', 'c2', 'C2', 'c2@example.com')`,
			`INSERT INTO products (tenant_id, id, name) VALUES ('t1', 'p1', 'P1')`,
			`INSERT INTO nodes (tenant_id, id, parent_id) VALUES ('t1', 'n0', '')`)
		allowAll := []string{
			relation(t, "user:u-7", "view", "t1", "customer", "c1"), relation(t, "user:u-7", "view", "t1", "customer", "c2"),
			relation(t, "user:u-7", "view", "t1", "product", "p1"), relation(t, "user:u-7", "view", "t1", "order", "po1"),
			relation(t, "user:u-7", "place_order", "t1", "customer", "c1"), relation(t, "user:u-7", "place_order", "t1", "customer", "c2"),
			relation(t, "user:u-7", "place", "t1", "order", "po1"),
		}
		place := e.command("place", func(s *CommandSpec[testProbe]) {
			s.Permission, s.Resource = "place", &Resource{Type: "service", Service: "orders"}
		}, orders, e.customerE, products, nodes)
		run := func(key string, fill func(ctx context.Context, h *Handle[testProbe]) error) error {
			_, err := e.run(place, e.request("t1", u7(t), key, `{}`), fillOf(fill))
			return err
		}
		create := func(id, customerID string) func(ctx context.Context, h *Handle[testProbe]) error {
			return func(ctx context.Context, h *Handle[testProbe]) error {
				c, err := repo(t, h, e.customerE).Get(ctx, customerID)
				if err != nil {
					return err
				}
				p, err := repo(t, h, products).Get(ctx, "p1")
				if err != nil {
					return err
				}
				_, err = repo(t, h, orders).Create(ctx, placedOrder{ID: id, CustomerID: customerID, ProductID: "p1"}, c, p)
				return err
			}
		}

		// The first create: place on the service, place_order on customer:c1 and view on product:p1.
		e.decision.setRelations(append(allowAll, relation(t, "user:u-7", "place", "t1", "service", "orders"))...)
		before := e.tuples()
		e.decision.taken()
		if err := run("k-lc05-t12-a", create("po1", "c1")); err != nil {
			t.Fatal(err)
		}
		var decided []string
		for _, d := range e.decision.taken() {
			decided = append(decided, d.Action+" "+d.Resource)
		}
		for _, want := range []string{"place service:orders", "place_order customer:c1", "view product:p1"} {
			if !slices.Contains(decided, want) {
				t.Fatalf("the create was decided as %v, want %s among them", decided, want)
			}
		}
		if e.count(`SELECT count(*) FROM placed_orders WHERE id = 'po1'`) != 1 {
			t.Fatal("the create did not commit")
		}
		added := e.tuples()[len(before):]
		if !slices.Equal(added, []string{"touch order:po1#customer@customer:c1", "touch order:po1#product@product:p1"}) {
			t.Fatalf("the create enqueued the tuples %v, want order#customer@customer:c1 and order#product@product:p1", added)
		}

		// The second create: place_order is denied on c1, so the create is not found and writes nothing.
		denied := slices.DeleteFunc(slices.Clone(allowAll), func(r string) bool {
			return r == relation(t, "user:u-7", "place_order", "t1", "customer", "c1")
		})
		e.decision.setRelations(append(denied, relation(t, "user:u-7", "place", "t1", "service", "orders"))...)
		before = e.tuples()
		if err := run("k-lc05-t12-b", create("po2", "c1")); !errors.Is(err, ErrNotFound) {
			t.Fatalf("the create with place_order denied on c1 gave %v, want not found", err)
		}
		if e.count(`SELECT count(*) FROM placed_orders WHERE id = 'po2'`) != 0 || len(e.tuples()) != len(before) ||
			e.keyRows("k-lc05-t12-b") != 0 {
			t.Fatal("the denied create left a row, a tuple or a key row")
		}

		// A tree row whose optional parent is not set: no reference is decided and no relation is written.
		e.decision.setRelations(append(allowAll, relation(t, "user:u-7", "place", "t1", "service", "orders"))...)
		before = e.tuples()
		e.decision.taken()
		if err := run("k-lc05-t12-c", func(ctx context.Context, h *Handle[testProbe]) error {
			_, err := repo(t, h, nodes).Create(ctx, node{ID: "n1"}, nil)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if taken := e.decision.taken(); len(taken) != 1 || taken[0].Resource != "service:orders" {
			t.Fatalf("the tree row was decided as %+v, want only the command's decision on service:orders", taken)
		}
		if len(e.tuples()) != len(before) {
			t.Fatal("the tree row with no parent wrote a relation")
		}

		// The move of po1 to Customer c2: place_order on c2, and the relation moves.
		e.decision.setRelations(append(allowAll, relation(t, "user:u-7", "place", "t1", "service", "orders"))...)
		before = e.tuples()
		e.decision.taken()
		if err := run("k-lc05-t12-d", func(ctx context.Context, h *Handle[testProbe]) error {
			o, err := repo(t, h, orders).GetForUpdate(ctx, "po1")
			if err != nil {
				return err
			}
			c2, err := repo(t, h, e.customerE).Get(ctx, "c2")
			if err != nil {
				return err
			}
			v := o.Value()
			v.CustomerID = "c2"
			_, err = repo(t, h, orders).Update(ctx, o, v, c2)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		decided = nil
		for _, d := range e.decision.taken() {
			decided = append(decided, d.Action+" "+d.Resource)
		}
		if !slices.Contains(decided, "place_order customer:c2") {
			t.Fatalf("the move was decided as %v, want place_order on customer:c2", decided)
		}
		if moved := e.tuples()[len(before):]; !slices.Equal(moved, []string{"delete order:po1#customer@customer:c1", "touch order:po1#customer@customer:c2"}) {
			t.Fatalf("the move enqueued %v, want the delete of the old relation and the touch of the new one", moved)
		}

		// A fill cannot write a relation of a reference itself: only the runtime does.
		if err := run("k-lc05-t12-e", func(ctx context.Context, h *Handle[testProbe]) error {
			o, err := repo(t, h, orders).GetForUpdate(ctx, "po1")
			if err != nil {
				return err
			}
			if _, err := h.Authz().Relate(ctx, o, orderTuple(t, "po1", "customer", "c1")); err == nil {
				t.Error("the fill wrote the relation of a reference")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}

// orderTuple is the tuple order:<id>#<relation>@customer:<customerID> of the tenant t1.
func orderTuple(t *testing.T, id, relation, customerID string) authz.Tuple {
	t.Helper()
	r, err := authz.NewResource(mustTenant(t, "t1"), "order", id)
	if err != nil {
		t.Fatal(err)
	}
	subject, err := authz.ResourceID(mustTenant(t, "t1"), customerID)
	if err != nil {
		t.Fatal(err)
	}
	return authz.Tuple{Resource: r, Relation: relation, Subject: authz.Subject{Type: "customer", ID: subject}}
}

// The entities of the within test: a project, a sprint of a project, and a ticket of a project whose optional
// sprint is held within project_id (ST-DECL-INV93, ST-SUP-RP10-LC37).
type (
	wProject struct{ ID, Name string }
	wSprint  struct{ ID, ProjectID string }
	wTicket  struct{ ID, ProjectID, SprintID string }
)

// withinKey is the composite key of the sprint of a ticket, as the postgres-adapter names it (ST-SUP-RC01-OC46).
const withinKey = "wtickets_sprint_id_within_fkey"

// withinCheck is the CHECK of the optional within field of a ticket, as the postgres-adapter names it
// (ST-SUP-RC01-OC46).
const withinCheck = "wtickets_sprint_id_wcheck"

// withinEntities makes the tables and the entities of the within test. The tables hold the composite key of the
// sprint of a ticket, as the postgres-adapter emits it, so a write past the runtime check meets the key.
func withinEntities(e *env) (*Entity[wProject], *Entity[wSprint], *Entity[wTicket]) {
	t := e.t
	t.Helper()
	e.exec(`CREATE TABLE wprojects (tenant_id text NOT NULL, id text NOT NULL, name text NOT NULL,
  version bigint NOT NULL DEFAULT 1, authz_token text NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, id))`,
		`CREATE TABLE wsprints (tenant_id text NOT NULL, id text NOT NULL, project_id text NOT NULL,
  version bigint NOT NULL DEFAULT 1, authz_token text NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, project_id) REFERENCES wprojects (tenant_id, id))`,
		`CREATE UNIQUE INDEX wsprints_project_id_id_key ON wsprints (tenant_id, project_id, id)`,
		`CREATE TABLE wtickets (tenant_id text NOT NULL, id text NOT NULL, project_id text NOT NULL, sprint_id text,
  version bigint NOT NULL DEFAULT 1, authz_token text NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, project_id) REFERENCES wprojects (tenant_id, id),
  CONSTRAINT `+withinKey+` FOREIGN KEY (tenant_id, project_id, sprint_id) REFERENCES wsprints (tenant_id, project_id, id))`)
	for _, table := range []string{"wprojects", "wsprints", "wtickets"} {
		e.exec("ALTER TABLE "+table+" ENABLE ROW LEVEL SECURITY", "ALTER TABLE "+table+" FORCE ROW LEVEL SECURITY",
			"CREATE POLICY "+table+"_tenant ON "+table+" USING (tenant_id = current_setting('app.tenant', true)) WITH CHECK (tenant_id = current_setting('app.tenant', true))",
			"GRANT SELECT, INSERT, UPDATE, DELETE ON "+table+" TO "+e.roles[0])
	}
	projects, err := NewEntity(spec("WProject", "project", "wprojects", []string{"name"},
		func(s Scanner) (wProject, Meta, error) {
			var v wProject
			var m Meta
			err := s.Scan(&v.ID, &v.Name, &m.Version, &m.Token)
			m.ID = v.ID
			return v, m, err
		},
		func(v wProject) []any { return []any{v.ID, v.Name} },
		func(v wProject) map[string]any { return map[string]any{"id": v.ID, "name": v.Name} }))
	if err != nil {
		t.Fatal(err)
	}
	sprintSpec := spec("WSprint", "sprint", "wsprints", []string{"project_id"},
		func(s Scanner) (wSprint, Meta, error) {
			var v wSprint
			var m Meta
			err := s.Scan(&v.ID, &v.ProjectID, &m.Version, &m.Token)
			m.ID = v.ID
			return v, m, err
		},
		func(v wSprint) []any { return []any{v.ID, v.ProjectID} },
		func(v wSprint) map[string]any { return map[string]any{"id": v.ID, "project_id": v.ProjectID} })
	sprintSpec.Parent = []Reference[wSprint]{{Entity: "WProject", ResourceType: "project", ID: func(v wSprint) string { return v.ProjectID },
		Permission: "edit", Relation: "project"}}
	sprintSpec.Within = map[string]func(wSprint) string{"project_id": func(v wSprint) string { return v.ProjectID }}
	sprintSpec.Referrers = []Referrer{{Key: withinKey, Field: "WTicket.sprint_id", Within: "project_id"}}
	sprints, err := NewEntity(sprintSpec)
	if err != nil {
		t.Fatal(err)
	}
	ticketSpec := spec("WTicket", "ticket", "wtickets", []string{"project_id", "sprint_id"},
		func(s Scanner) (wTicket, Meta, error) {
			var v wTicket
			var m Meta
			var sprint sql.NullString
			err := s.Scan(&v.ID, &v.ProjectID, &sprint, &m.Version, &m.Token)
			v.SprintID, m.ID = sprint.String, v.ID
			return v, m, err
		},
		func(v wTicket) []any {
			var sprint any
			if v.SprintID != "" {
				sprint = v.SprintID
			}
			return []any{v.ID, v.ProjectID, sprint}
		},
		func(v wTicket) map[string]any {
			return map[string]any{"id": v.ID, "project_id": v.ProjectID, "sprint_id": v.SprintID}
		})
	ticketSpec.Parent = []Reference[wTicket]{
		{Entity: "WProject", ResourceType: "project", ID: func(v wTicket) string { return v.ProjectID }, Permission: "edit", Relation: "project"},
		{Entity: "WSprint", ResourceType: "sprint", ID: func(v wTicket) string { return v.SprintID }, Permission: "edit", Relation: "sprint",
			Field: "sprint_id", Within: "project_id", Key: withinKey, Check: withinCheck},
	}
	ticketSpec.Within = map[string]func(wTicket) string{"project_id": func(v wTicket) string { return v.ProjectID }}
	tickets, err := NewEntity(ticketSpec)
	if err != nil {
		t.Fatal(err)
	}
	return projects, sprints, tickets
}

func TestWithin(t *testing.T) {
	// setup gives the environment with the projects core and web, the sprint s1 of core and s2 of web, and the
	// ticket t1 of core in s1, with every decision allowed, and a run of one fill in a command that writes all three.
	setup := func(t *testing.T) (*env, *Entity[wProject], *Entity[wSprint], *Entity[wTicket], func(string, func(context.Context, *Handle[testProbe]) error) error) {
		e := newEnv(t)
		projects, sprints, tickets := withinEntities(e)
		e.exec(`INSERT INTO wprojects (tenant_id, id, name) VALUES ('t1', 'core', 'Core'), ('t1', 'web', 'Web')`,
			`INSERT INTO wsprints (tenant_id, id, project_id) VALUES ('t1', 's1', 'core'), ('t1', 's2', 'web')`,
			`INSERT INTO wtickets (tenant_id, id, project_id, sprint_id) VALUES ('t1', 't1', 'core', 's1')`)
		var allow []string
		for _, r := range [][2]string{{"project", "core"}, {"project", "web"}, {"sprint", "s1"}, {"sprint", "s2"}, {"ticket", "t1"}, {"ticket", "t2"}, {"ticket", "t3"}} {
			allow = append(allow, relation(t, "user:u-7", "view", "t1", r[0], r[1]), relation(t, "user:u-7", "edit", "t1", r[0], r[1]),
				relation(t, "user:u-7", "plan", "t1", r[0], r[1]))
		}
		e.decision.setRelations(append(allow, relation(t, "user:u-7", "plan", "t1", "service", "tracker"))...)
		plan := e.command("plan", func(s *CommandSpec[testProbe]) {
			s.Permission, s.Resource = "plan", &Resource{Type: "service", Service: "tracker"}
		}, projects, sprints, tickets)
		run := func(key string, fill func(context.Context, *Handle[testProbe]) error) error {
			_, err := e.run(plan, e.request("t1", u7(t), key, `{}`), fillOf(fill))
			return err
		}
		return e, projects, sprints, tickets, run
	}
	// refusedOn checks that err is ErrInvalid that OutcomeOf answers with 400 invalid naming field.
	refusedOn := func(t *testing.T, err error, field string) {
		t.Helper()
		o := OutcomeOf(err)
		if !errors.Is(err, ErrInvalid) || o.Status != http.StatusBadRequest || o.Code != ErrorCodeInvalid || o.Field != field {
			t.Fatalf("the write gave %v (%+v), want ErrInvalid, 400 invalid naming %s", err, o, field)
		}
		if strings.Contains(o.Reason, "core") || strings.Contains(o.Reason, "web") || strings.Contains(o.Reason, "s1") {
			t.Fatalf("the reason %q names a value", o.Reason)
		}
	}

	t.Run("ST-SUP-RP10-LC37-T01 A create compares the field of a reference held within it with the loaded referenced row before the statement", func(t *testing.T) {
		e, projects, sprints, tickets, run := setup(t)
		create := func(id, project, sprint string) func(context.Context, *Handle[testProbe]) error {
			return func(ctx context.Context, h *Handle[testProbe]) error {
				p, err := repo(t, h, projects).Get(ctx, project)
				if err != nil {
					return err
				}
				parents := []Ref{p}
				if sprint != "" {
					s, err := repo(t, h, sprints).Get(ctx, sprint)
					if err != nil {
						return err
					}
					parents = append(parents, s)
				}
				_, err = repo(t, h, tickets).Create(ctx, wTicket{ID: id, ProjectID: project, SprintID: sprint}, parents...)
				return err
			}
		}
		// The runtime check alone: with no composite key, a ticket of web in the sprint of core is still refused before
		// the statement, and nothing is written.
		e.exec("ALTER TABLE wtickets DROP CONSTRAINT " + withinKey)
		if err := run("k-lc37-t01-a", create("t2", "web", "s1")); err != nil {
			refusedOn(t, err, "sprint_id")
		} else {
			t.Fatal("a ticket of web in the sprint of core was written")
		}
		if e.count(`SELECT count(*) FROM wtickets WHERE id = 't2'`) != 0 || e.keyRows("k-lc37-t01-a") != 0 {
			t.Fatal("the refused create left a row or a key row")
		}
		// A ticket in a sprint of its own project and a ticket with no sprint are written.
		if err := run("k-lc37-t01-b", create("t2", "web", "s2")); err != nil {
			t.Fatalf("a ticket of web in the sprint of web: %v", err)
		}
		if err := run("k-lc37-t01-c", create("t3", "core", "")); err != nil {
			t.Fatalf("a ticket of core with no sprint: %v", err)
		}
		if e.count(`SELECT count(*) FROM wtickets WHERE id IN ('t2', 't3')`) != 2 {
			t.Fatal("the creates did not commit")
		}
	})

	t.Run("ST-SUP-RP10-LC37-T05 A create whose reference decision is denied is not found before the within compare, never invalid", func(t *testing.T) {
		e, projects, sprints, tickets, run := setup(t)
		// edit on the sprint s1 of core is denied; view stays, so the fill can load the sprint.
		var allow []string
		for _, r := range [][2]string{{"project", "core"}, {"project", "web"}, {"sprint", "s1"}, {"sprint", "s2"}, {"ticket", "t4"}} {
			allow = append(allow, relation(t, "user:u-7", "view", "t1", r[0], r[1]), relation(t, "user:u-7", "plan", "t1", r[0], r[1]))
			if r != [2]string{"sprint", "s1"} {
				allow = append(allow, relation(t, "user:u-7", "edit", "t1", r[0], r[1]))
			}
		}
		e.decision.setRelations(append(allow, relation(t, "user:u-7", "plan", "t1", "service", "tracker"))...)
		// A ticket of web in the sprint of core: the projects differ, but the denied decision answers first, so the
		// answer does not tell that the sprint is of another project.
		err := run("k-lc37-t05", func(ctx context.Context, h *Handle[testProbe]) error {
			web, err := repo(t, h, projects).Get(ctx, "web")
			if err != nil {
				return err
			}
			s1, err := repo(t, h, sprints).Get(ctx, "s1")
			if err != nil {
				return err
			}
			_, err = repo(t, h, tickets).Create(ctx, wTicket{ID: "t4", ProjectID: "web", SprintID: "s1"}, web, s1)
			return err
		})
		if !errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalid) {
			t.Fatalf("the create with edit denied on the sprint of another project gave %v, want not found and not invalid", err)
		}
		if e.count(`SELECT count(*) FROM wtickets WHERE id = 't4'`) != 0 || e.keyRows("k-lc37-t05") != 0 {
			t.Fatal("the denied create left a row or a key row")
		}
	})

	t.Run("ST-SUP-RP10-LC37-T02 An update that moves the reference or the field is compared, and needs the referenced row", func(t *testing.T) {
		e, projects, sprints, tickets, run := setup(t)
		// A move of the sprint of t1 to the sprint of web is refused.
		if err := run("k-lc37-t02-a", func(ctx context.Context, h *Handle[testProbe]) error {
			from, err := repo(t, h, tickets).GetForUpdate(ctx, "t1")
			if err != nil {
				return err
			}
			s2, err := repo(t, h, sprints).Get(ctx, "s2")
			if err != nil {
				return err
			}
			v := from.Value()
			v.SprintID = "s2"
			_, err = repo(t, h, tickets).Update(ctx, from, v, s2)
			return err
		}); err == nil {
			t.Fatal("the move of t1 into the sprint of web was written")
		} else {
			refusedOn(t, err, "sprint_id")
		}
		// A move of t1 to web that keeps the sprint of core is refused too: the sprint stays, and its row is compared.
		if err := run("k-lc37-t02-b", func(ctx context.Context, h *Handle[testProbe]) error {
			from, err := repo(t, h, tickets).GetForUpdate(ctx, "t1")
			if err != nil {
				return err
			}
			web, err := repo(t, h, projects).Get(ctx, "web")
			if err != nil {
				return err
			}
			s1, err := repo(t, h, sprints).Get(ctx, "s1")
			if err != nil {
				return err
			}
			v := from.Value()
			v.ProjectID = "web"
			_, err = repo(t, h, tickets).Update(ctx, from, v, web, s1)
			return err
		}); err == nil {
			t.Fatal("the move of t1 to web in the sprint of core was written")
		} else {
			refusedOn(t, err, "sprint_id")
		}
		// The same move without the row of the sprint is a fault of the fill: the runtime cannot compare it.
		if err := run("k-lc37-t02-c", func(ctx context.Context, h *Handle[testProbe]) error {
			from, err := repo(t, h, tickets).GetForUpdate(ctx, "t1")
			if err != nil {
				return err
			}
			web, err := repo(t, h, projects).Get(ctx, "web")
			if err != nil {
				return err
			}
			v := from.Value()
			v.ProjectID = "web"
			_, err = repo(t, h, tickets).Update(ctx, from, v, web)
			return err
		}); !errors.Is(err, ErrInternal) {
			t.Fatalf("the move of the field without the row of the held reference gave %v, want an internal error", err)
		}
		// A move of both to web is written.
		if err := run("k-lc37-t02-d", func(ctx context.Context, h *Handle[testProbe]) error {
			from, err := repo(t, h, tickets).GetForUpdate(ctx, "t1")
			if err != nil {
				return err
			}
			web, err := repo(t, h, projects).Get(ctx, "web")
			if err != nil {
				return err
			}
			s2, err := repo(t, h, sprints).Get(ctx, "s2")
			if err != nil {
				return err
			}
			v := from.Value()
			v.ProjectID, v.SprintID = "web", "s2"
			_, err = repo(t, h, tickets).Update(ctx, from, v, web, s2)
			return err
		}); err != nil {
			t.Fatalf("the move of t1 with its sprint to web: %v", err)
		}
		if e.count(`SELECT count(*) FROM wtickets WHERE id = 't1' AND project_id = 'web' AND sprint_id = 's2'`) != 1 {
			t.Fatal("the move did not commit")
		}
	})

	t.Run("ST-SUP-RP10-LC37-T03 A violation of the composite key gives ErrInvalid naming the reference, never internal and never a run again", func(t *testing.T) {
		e, projects, sprints, tickets, run := setup(t)
		// The move of the sprint s1 of core to web, while the ticket t1 of core refers to it, meets the key of the
		// tickets: the refusal names the field of the sprint that the reference is held within.
		calls := 0
		if err := run("k-lc37-t03-a", func(ctx context.Context, h *Handle[testProbe]) error {
			calls++
			from, err := repo(t, h, sprints).GetForUpdate(ctx, "s1")
			if err != nil {
				return err
			}
			web, err := repo(t, h, projects).Get(ctx, "web")
			if err != nil {
				return err
			}
			v := from.Value()
			v.ProjectID = "web"
			_, err = repo(t, h, sprints).Update(ctx, from, v, web)
			return err
		}); err == nil {
			t.Fatal("the move of a sprint with a ticket to another project was written")
		} else {
			refusedOn(t, err, "project_id")
		}
		if calls != 1 {
			t.Fatalf("the fill ran %d times, want once", calls)
		}
		if e.count(`SELECT count(*) FROM wsprints WHERE id = 's1' AND project_id = 'core'`) != 1 {
			t.Fatal("the refused move changed the sprint")
		}
		// A row statement of a ticket that the key refuses, as from a race or a worker that gave no parent, names the
		// reference; a violation of another key stays internal.
		// A violation of the CHECK of the within field names the reference too (wcheck, from fu8); a CHECK of
		// another name stays internal.
		cases := []struct {
			name, code, key string
			field           string
		}{{"the key of the reference", "23503", withinKey, "sprint_id"}, {"another key", "23503", "wtickets_project_id_fkey", ""},
			{"the check of the within field", "23514", withinCheck, "sprint_id"}, {"another check", "23514", "wtickets_name_check", ""},
			{"the key name as a check", "23514", withinKey, ""}}
		for _, c := range cases {
			if err := run("k-lc37-t03-"+c.code+c.key, func(ctx context.Context, h *Handle[testProbe]) error {
				return repo(t, h, tickets).failWrite(ctx, "create WTicket", &pgconn.PgError{Code: c.code, ConstraintName: c.key})
			}); c.field != "" {
				refusedOn(t, err, c.field)
			} else if !errors.Is(err, ErrInternal) || errors.Is(err, ErrInvalid) {
				t.Fatalf("%s gave %v, want an internal error", c.name, err)
			}
		}
	})
}

func TestFieldChecks(t *testing.T) {
	t.Run("ST-SUP-RP10-LC42-T01 A value that breaks a declared constraint of a field is invalid with the field, before any statement or by the table, and a unique violation stays a conflict", func(t *testing.T) {
		e := newEnv(t)
		_, orders, _ := referenceEntities(e)
		// The table holds the CHECK and the foreign key that the postgres adapter writes, by their names.
		e.exec(`ALTER TABLE placed_orders ADD CONSTRAINT placed_orders_customer_id_check CHECK (char_length(customer_id) >= 2)`,
			`CREATE UNIQUE INDEX placed_orders_product_unique ON placed_orders (tenant_id, product_id)`,
			`ALTER TABLE placed_orders ADD CONSTRAINT placed_orders_product_id_fkey FOREIGN KEY (tenant_id, product_id) REFERENCES products (tenant_id, id)`,
			`INSERT INTO products (tenant_id, id, name) VALUES ('t1', 'p1', 'P1'), ('t1', 'p2', 'P2')`)
		s := orders.spec
		s.Parent, s.ResourceType, s.ReadPermission = nil, "", ""
		checked := s
		checked.Check = func(v placedOrder) (string, string, bool) {
			if !MinLength(v.CustomerID, 2) {
				return "customer_id", "shorter than min_length 2", false
			}
			if !MaxLength(v.ProductID, 8) {
				return "product_id", "longer than max_length 8", false
			}
			return "", "", true
		}
		checked.Constraints = map[string]string{"placed_orders_customer_id_check": "customer_id",
			"placed_orders_product_id_fkey": "product_id", "customer_id": "customer_id", "product_id": "product_id"}
		withCheck, err := NewEntity(checked)
		if err != nil {
			t.Fatal(err)
		}
		// The same entity with no Check: the table refuses the value, and Constraints names the field.
		tableOnly := checked
		tableOnly.Check = nil
		byTable, err := NewEntity(tableOnly)
		if err != nil {
			t.Fatal(err)
		}
		create := func(entity *Entity[placedOrder], key string, v placedOrder) error {
			cmd := e.command("place-"+key, nil, entity)
			_, err := e.run(cmd, e.request("t1", u7(t), key, `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
				_, err := repo(t, h, entity).Create(ctx, v)
				return err
			}))
			return err
		}
		invalid := func(err error) Outcome {
			t.Helper()
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("the create gave %v, want ErrInvalid", err)
			}
			return OutcomeOf(err)
		}
		rows := func() int { return e.count(`SELECT count(*) FROM placed_orders`) }

		// The generated Check refuses before any statement: the reason is the one of the Check, and no row is written.
		o := invalid(create(withCheck, "k-f1", placedOrder{ID: "f1", CustomerID: "c", ProductID: "p1"}))
		if o.Status != http.StatusBadRequest || o.Field != "customer_id" || o.Reason != "shorter than min_length 2" || RPCCode(o.Status) != codes.InvalidArgument {
			t.Fatalf("a customer_id under min_length gave %+v, want 400 invalid customer_id with the reason of the Check", o)
		}
		if o := invalid(create(withCheck, "k-f2", placedOrder{ID: "f2", CustomerID: "c1", ProductID: "p-123456789"})); o.Field != "product_id" {
			t.Fatalf("a product_id over max_length gave %+v, want the field product_id", o)
		}
		// With no Check, the CHECK of the table refuses the value, and the answer names the field of the constraint.
		if o := invalid(create(byTable, "k-f3", placedOrder{ID: "f3", CustomerID: "c", ProductID: "p1"})); o.Status != http.StatusBadRequest ||
			o.Field != "customer_id" || o.Reason != reasonConstraint {
			t.Fatalf("a value that the table CHECK refused gave %+v, want 400 invalid customer_id", o)
		}
		// A foreign key (23503) names its field too.
		if o := invalid(create(byTable, "k-f4", placedOrder{ID: "f4", CustomerID: "c1", ProductID: "p9"})); o.Field != "product_id" {
			t.Fatalf("a missing product gave %+v, want the field product_id", o)
		}
		if rows() != 0 {
			t.Fatalf("a refused create wrote %d rows, want none", rows())
		}
		// A valid value commits, and a unique violation (23505) stays a conflict, never invalid.
		if err := create(withCheck, "k-f5", placedOrder{ID: "f5", CustomerID: "c1", ProductID: "p1"}); err != nil {
			t.Fatal(err)
		}
		err = create(withCheck, "k-f6", placedOrder{ID: "f6", CustomerID: "c2", ProductID: "p1"})
		if errors.Is(err, ErrInvalid) || OutcomeOf(err).Status != http.StatusConflict {
			t.Fatalf("a unique violation gave %v (%+v), want 409 conflict", err, OutcomeOf(err))
		}
		// An update checks the new value as a create does.
		cmd := e.command("change", nil, withCheck)
		_, err = e.run(cmd, e.request("t1", u7(t), "k-f7", `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			r := repo(t, h, withCheck)
			loaded, err := r.GetForUpdate(ctx, "f5")
			if err != nil {
				return err
			}
			v := loaded.Value()
			v.CustomerID = "x"
			_, err = r.Update(ctx, loaded, v)
			return err
		}))
		if o := invalid(err); o.Field != "customer_id" {
			t.Fatalf("an update under min_length gave %+v, want the field customer_id", o)
		}
	})
}
