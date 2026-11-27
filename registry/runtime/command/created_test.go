package command

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/910-software-solutions/stego/registry/runtime/authz"
	"github.com/910-software-solutions/stego/registry/runtime/idempotency"
	"github.com/910-software-solutions/stego/registry/runtime/internal/authzclock"
	"github.com/910-software-solutions/stego/registry/runtime/internal/loaded"
	"github.com/910-software-solutions/stego/registry/runtime/outbox"
	"github.com/910-software-solutions/stego/registry/runtime/tenant"
)

// The entities of the tests of ST-SUP-RP10-LC59: the catalog of erp-demo, where a CostItem goes under an Org and a
// PriceHistory row refers to its CostItem with the permission edit, and cost_item.edit = org->catalog_edit.
type (
	costOrg   struct{ ID, Name string }
	costGroup struct{ ID, OrgID string }
	costItem  struct{ ID, OrgID, BillingOrgID, GroupID string }
	priceRow  struct{ ID, CostItemID, Price string }
	costNote  struct{ ID, CostItemID, Body string }
	kitRow    struct{ ID, Name string }
	kitPart   struct{ ID, KitID string }
)

var costTables = map[string][]string{
	"orgs": {"name"}, "cost_groups": {"org_id"}, "cost_items": {"org_id", "billing_org_id", "group_id"},
	"price_histories": {"cost_item_id", "price"}, "cost_notes": {"cost_item_id", "body"}, "kit_items": {"name"},
	"kits": {"name"}, "kit_parts": {"kit_id"},
}

// costSchema is the .zed schema of the release of the tests, which the schema gate of the runtime takes.
const costSchema = `definition user {}
definition org {
	relation owner: user
	relation admin: user
	permission catalog_edit = owner + admin
	permission view = owner + admin
}
definition cost_item {
	relation org: org
	relation owner: user
	permission edit = org->catalog_edit
}
definition price_history {
	relation cost_item: cost_item
	permission edit = cost_item->edit
}`

// costSchemaReordered is costSchema with its definitions and members in another order and other comments.
const costSchemaReordered = `// The catalog, written again.
definition price_history {
	/** the item */
	permission edit = cost_item->edit
	relation cost_item: cost_item
}
definition cost_item {
	permission edit = org->catalog_edit // the arrow
	relation owner: user
	relation org: org
}
definition user {}
definition org {
	permission view = owner + admin
	relation admin: user
	relation owner: user
	permission catalog_edit = owner + admin
}`

// costSchemaR2 is the schema of a release r2 that changes only the .zed schema: edit now needs the Org not frozen.
const costSchemaR2 = `definition user {}
definition org {
	relation owner: user
	relation admin: user
	relation frozen: user
	permission catalog_edit = owner + admin
	permission view = owner + admin
}
definition cost_item {
	relation org: org
	relation owner: user
	permission edit = org->catalog_edit - org->frozen
}
definition price_history {
	relation cost_item: cost_item
	permission edit = cost_item->edit
}`

// servedSchema is a fake schema reader of SpiceDB: it serves text, or fails with err, or blocks until its context
// ends while hang is set, and counts its calls.
type servedSchema struct {
	mu    sync.Mutex
	text  string
	err   error
	hang  bool
	gate  chan struct{}
	calls int
}

func (s *servedSchema) set(text string, err error, hang bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.text, s.err, s.hang = text, err, hang
}

func (s *servedSchema) ReadSchema(ctx context.Context) (string, error) {
	s.mu.Lock()
	s.calls++
	text, err, hang, gate := s.text, s.err, s.hang, s.gate
	s.mu.Unlock()
	if gate != nil {
		<-gate
	}
	if hang {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return text, err
}

func (s *servedSchema) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// gateClock is the clock of the digest bound that a test moves.
type gateClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *gateClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *gateClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// costEnv is the env of the tests of ST-SUP-RP10-LC59 with the catalog entities, a schema gate over a fake reader
// that serves costSchema, and a runtime with that gate.
type costEnv struct {
	*env
	t        *testing.T
	served   *servedSchema
	clock    *gateClock
	gate     *authz.SchemaGate
	crt      *Runtime
	orgs     *Entity[costOrg]
	groups   *Entity[costGroup]
	items    *Entity[costItem]
	grouped  *Entity[costItem]
	owned    *Entity[costItem]
	bare     *Entity[costItem]
	tenanted *Entity[costItem]
	ownedAll *Entity[costItem]
	prices   *Entity[priceRow]
	notes    *Entity[costNote]
	kitItems *Entity[kitRow]
	kits     *Entity[kitRow]
	parts    *Entity[kitPart]
}

func newCostEnv(t *testing.T) *costEnv {
	t.Helper()
	return newCostEnvOn(t, newEnv(t), nil)
}

// newCostEnvOn makes the catalog tables and entities on e; rt, when given, is the runtime of the env, else one with
// the schema gate of costSchema.
func newCostEnvOn(t *testing.T, e *env, rt *Runtime) *costEnv {
	t.Helper()
	c := &costEnv{env: e, t: t, served: &servedSchema{text: costSchema}, clock: &gateClock{now: time.Unix(1793966400, 0)}}
	for table, cols := range costTables {
		e.exec("CREATE TABLE "+table+" (tenant_id text NOT NULL, id text NOT NULL, "+strings.Join(cols, " text NOT NULL DEFAULT '', ")+
			" text NOT NULL DEFAULT '', version bigint NOT NULL DEFAULT 1, authz_token text NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, id))",
			"ALTER TABLE "+table+" ENABLE ROW LEVEL SECURITY", "ALTER TABLE "+table+" FORCE ROW LEVEL SECURITY",
			"CREATE POLICY "+table+"_tenant ON "+table+" USING (tenant_id = current_setting('app.tenant', true)) WITH CHECK (tenant_id = current_setting('app.tenant', true))",
			"GRANT SELECT, INSERT, UPDATE, DELETE ON "+table+" TO "+e.roles[0])
	}
	var err error
	if c.gate, err = authz.NewSchemaGate(costSchema, c.served, 0); err != nil {
		t.Fatal(err)
	}
	if !authzclock.SetGate(c.gate, c.clock.Now) {
		t.Fatal("the authz package registers no clock setter of a schema gate")
	}
	c.crt = rt
	if c.crt == nil {
		c.crt = e.runtime(func(cfg *Config) { cfg.SchemaGate = c.gate })
	}
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	c.orgs, err = NewEntity(spec("Org", "org", "orgs", []string{"name"},
		func(s Scanner) (costOrg, Meta, error) {
			var v costOrg
			var m Meta
			err := s.Scan(&v.ID, &v.Name, &m.Version, &m.Token)
			m.ID = v.ID
			return v, m, err
		},
		func(v costOrg) []any { return []any{v.ID, v.Name} },
		func(v costOrg) map[string]any { return map[string]any{"id": v.ID, "name": v.Name} }))
	must(err)
	groupSpec := spec("CostGroup", "cost_group", "cost_groups", []string{"org_id"},
		func(s Scanner) (costGroup, Meta, error) {
			var v costGroup
			var m Meta
			err := s.Scan(&v.ID, &v.OrgID, &m.Version, &m.Token)
			m.ID = v.ID
			return v, m, err
		},
		func(v costGroup) []any { return []any{v.ID, v.OrgID} },
		func(v costGroup) map[string]any { return map[string]any{"id": v.ID, "org_id": v.OrgID} })
	groupSpec.Parent = []Reference[costGroup]{{Entity: "Org", ResourceType: "org", ID: func(v costGroup) string { return v.OrgID },
		Permission: "catalog_edit", Relation: "org", Field: "org_id", Required: true}}
	c.groups, err = NewEntity(groupSpec)
	must(err)
	itemSpec := spec("CostItem", "cost_item", "cost_items", []string{"org_id", "billing_org_id", "group_id"},
		func(s Scanner) (costItem, Meta, error) {
			var v costItem
			var m Meta
			err := s.Scan(&v.ID, &v.OrgID, &v.BillingOrgID, &v.GroupID, &m.Version, &m.Token)
			m.ID = v.ID
			return v, m, err
		},
		func(v costItem) []any { return []any{v.ID, v.OrgID, v.BillingOrgID, v.GroupID} },
		func(v costItem) map[string]any { return map[string]any{"id": v.ID, "org_id": v.OrgID} })
	orgRef := Reference[costItem]{Entity: "Org", ResourceType: "org", ID: func(v costItem) string { return v.OrgID },
		Permission: "catalog_edit", Relation: "org", Field: "org_id", Required: true}
	billingRef := Reference[costItem]{Entity: "Org", ResourceType: "org", ID: func(v costItem) string { return v.BillingOrgID },
		Permission: "catalog_edit", Relation: "billing_org", Field: "billing_org_id", Required: true}
	s := itemSpec
	s.Parent = []Reference[costItem]{orgRef, billingRef}
	c.items, err = NewEntity(s)
	must(err)
	s = itemSpec
	s.Parent = []Reference[costItem]{{Entity: "CostGroup", ResourceType: "cost_group", ID: func(v costItem) string { return v.GroupID },
		Permission: "add_item", Relation: "cost_group", Field: "group_id", Required: true}}
	c.grouped, err = NewEntity(s)
	must(err)
	noPermission := orgRef
	noPermission.Permission = ""
	s = itemSpec
	s.Parent = []Reference[costItem]{noPermission}
	c.bare, err = NewEntity(s)
	must(err)
	s.Relationships = []Relationship[costItem]{{Relation: "owner", SubjectType: "user"}}
	c.owned, err = NewEntity(s)
	must(err)
	s = itemSpec
	s.Parent = []Reference[costItem]{orgRef, billingRef}
	s.Relationships = []Relationship[costItem]{{Relation: "owner", SubjectType: "user"}}
	c.ownedAll, err = NewEntity(s)
	must(err)
	s = itemSpec
	s.Parent = []Reference[costItem]{orgRef}
	s.Tenant = true
	c.tenanted, err = NewEntity(s)
	must(err)
	priceSpec := spec("PriceHistory", "price_history", "price_histories", []string{"cost_item_id", "price"},
		func(s Scanner) (priceRow, Meta, error) {
			var v priceRow
			var m Meta
			err := s.Scan(&v.ID, &v.CostItemID, &v.Price, &m.Version, &m.Token)
			m.ID = v.ID
			return v, m, err
		},
		func(v priceRow) []any { return []any{v.ID, v.CostItemID, v.Price} },
		func(v priceRow) map[string]any {
			return map[string]any{"id": v.ID, "cost_item_id": v.CostItemID, "price": v.Price}
		})
	priceSpec.Parent = []Reference[priceRow]{{Entity: "CostItem", ResourceType: "cost_item", ID: func(v priceRow) string { return v.CostItemID },
		Permission: "edit", Relation: "cost_item", Field: "cost_item_id", Required: true}}
	c.prices, err = NewEntity(priceSpec)
	must(err)
	noteSpec := spec("Note", "", "cost_notes", []string{"cost_item_id", "body"},
		func(s Scanner) (costNote, Meta, error) {
			var v costNote
			var m Meta
			err := s.Scan(&v.ID, &v.CostItemID, &v.Body, &m.Version, &m.Token)
			m.ID = v.ID
			return v, m, err
		},
		func(v costNote) []any { return []any{v.ID, v.CostItemID, v.Body} },
		func(v costNote) map[string]any { return map[string]any{"id": v.ID, "body": v.Body} })
	noteSpec.Parent = []Reference[costNote]{{Entity: "CostItem", ResourceType: "cost_item", ID: func(v costNote) string { return v.CostItemID },
		Permission: "edit", Field: "cost_item_id", Required: true}}
	c.notes, err = NewEntity(noteSpec)
	must(err)
	kit := func(name, table string) *Entity[kitRow] {
		ks := spec(name, "item", table, []string{"name"},
			func(s Scanner) (kitRow, Meta, error) {
				var v kitRow
				var m Meta
				err := s.Scan(&v.ID, &v.Name, &m.Version, &m.Token)
				m.ID = v.ID
				return v, m, err
			},
			func(v kitRow) []any { return []any{v.ID, v.Name} },
			func(v kitRow) map[string]any { return map[string]any{"id": v.ID, "name": v.Name} })
		ks.Relationships = []Relationship[kitRow]{{Relation: "owner", SubjectType: "user"}}
		k, err := NewEntity(ks)
		must(err)
		return k
	}
	c.kitItems, c.kits = kit("Item", "kit_items"), kit("Kit", "kits")
	partSpec := spec("KitPart", "kit_part", "kit_parts", []string{"kit_id"},
		func(s Scanner) (kitPart, Meta, error) {
			var v kitPart
			var m Meta
			err := s.Scan(&v.ID, &v.KitID, &m.Version, &m.Token)
			m.ID = v.ID
			return v, m, err
		},
		func(v kitPart) []any { return []any{v.ID, v.KitID} },
		func(v kitPart) map[string]any { return map[string]any{"id": v.ID, "kit_id": v.KitID} })
	partSpec.Parent = []Reference[kitPart]{{Entity: "Kit", ResourceType: "item", ID: func(v kitPart) string { return v.KitID },
		Permission: "edit", Relation: "kit", Field: "kit_id", Required: true}}
	c.parts, err = NewEntity(partSpec)
	must(err)
	return c
}

// The witnesses that checking computes for add-cost-item (ST-DECL-INV108): the decision of edit on the CostItem of
// a PriceHistory is proved by the decision of catalog_edit on the Org of its reference org_id.
var priceWitness = Witness{Entity: "PriceHistory", Field: "cost_item_id", Subject: SubjectCaller, Permission: "edit",
	Kind: WitnessReference, Relation: "org", Decided: "catalog_edit"}

// addCostItem is the command add-cost-item, decided with catalog_edit on the Org of the request, which declares
// creates_with_children when opted.
func (c *costEnv) addCostItem(opted bool, witnesses []Witness, change func(*CommandSpec[testProbe]), writes ...Declared) *Command[testProbe] {
	c.t.Helper()
	return c.command("add-cost-item", func(s *CommandSpec[testProbe]) {
		s.Permission, s.Resource = "catalog_edit", &Resource{Type: "org",
			Token: `SELECT authz_token FROM orgs WHERE tenant_id = $1 AND id = $2 FOR SHARE`, ValidID: func(id string) bool { return id != "" }}
		s.CreatesWithChildren, s.Witnesses = opted, witnesses
		if change != nil {
			change(s)
		}
	}, writes...)
}

// grants are the relationships of the provider that u-7 holds on the Org o1: catalog_edit and view.
func (c *costEnv) grants(extra ...string) []string {
	return append([]string{relation(c.t, "user:u-7", "catalog_edit", "t1", "org", "o1"), relation(c.t, "user:u-7", "view", "t1", "org", "o1")}, extra...)
}

// call runs cmd as u-7 in t1 on the Org o1 with fill, on the runtime of the env.
func (c *costEnv) call(cmd *Command[testProbe], key string, fill func(ctx context.Context, h *Handle[testProbe]) error) (Result, error) {
	req := c.request("t1", u7(c.t), key, `{}`)
	req.Resource = "o1"
	return c.runOn(c.crt, cmd, req, fillOf(fill))
}

// pair is the fill of add-cost-item: the CostItem item under the Org o1, then the PriceHistory price of it, with the
// loaded value that the create of the CostItem returned.
func (c *costEnv) pair(items *Entity[costItem], item, price string) func(ctx context.Context, h *Handle[testProbe]) error {
	return func(ctx context.Context, h *Handle[testProbe]) error {
		o, err := repo(c.t, h, c.orgs).Get(ctx, "o1")
		if err != nil {
			return err
		}
		p, err := repo(c.t, h, items).Create(ctx, costItem{ID: item, OrgID: "o1"}, o)
		if err != nil {
			return err
		}
		_, err = repo(c.t, h, c.prices).Create(ctx, priceRow{ID: price, CostItemID: item, Price: "12.50"}, p)
		return err
	}
}

// decided gives the decisions of the provider since the last call as action resource, or subject action resource
// with subjects.
func (c *costEnv) decided(subjects bool) []string {
	var out []string
	for _, d := range c.decision.taken() {
		if subjects {
			out = append(out, d.Subject+" "+d.Action+" "+d.Resource)
		} else {
			out = append(out, d.Action+" "+d.Resource)
		}
	}
	return out
}

// none fails the test when the call left a row of the tables, a tuple or a key row of key.
func (c *costEnv) none(key string, before int, ids ...string) {
	c.t.Helper()
	for _, id := range ids {
		if c.count(`SELECT (SELECT count(*) FROM cost_items WHERE id = $1) + (SELECT count(*) FROM price_histories WHERE id = $1)
 + (SELECT count(*) FROM cost_notes WHERE id = $1) + (SELECT count(*) FROM kit_parts WHERE id = $1) + (SELECT count(*) FROM kit_items WHERE id = $1)`, id) != 0 {
			c.t.Fatalf("the call %s left the row %s", key, id)
		}
	}
	if len(c.tuples()) != before || c.keyRows(key) != 0 {
		c.t.Fatalf("the call %s left a tuple or a key row", key)
	}
}

func count(list []string, want string) int {
	n := 0
	for _, s := range list {
		if s == want {
			n++
		}
	}
	return n
}

// relayShim is the relationships port of the relay of T01: it writes the tuples into the fake decision port as the
// schema of the release gives them, cost_item.edit = org->catalog_edit, so after the relay the caller may edit and
// view the new CostItem.
type relayShim struct {
	c *costEnv
}

func (r relayShim) Touch(_ context.Context, tuples []authz.Tuple, _ []authz.Precondition) (authz.Token, error) {
	d := r.c.decision
	d.mu.Lock()
	for _, tuple := range tuples {
		if tuple.Resource.Type == "cost_item" && tuple.Relation == "org" {
			org, _ := authz.NewResource(mustTenant(r.c.t, "t1"), "org", "o1")
			if tuple.Subject.ID == org.ID && d.relations["user:u-7 catalog_edit "+org.String()] {
				d.relations["user:u-7 edit "+tuple.Resource.String()] = true
				d.relations["user:u-7 view "+tuple.Resource.String()] = true
			}
		}
	}
	d.mu.Unlock()
	return authz.NewToken("relayed")
}

func (r relayShim) Delete(context.Context, []authz.Filter, []authz.Precondition) (authz.Token, error) {
	return authz.NewToken("relayed")
}

func (r relayShim) Write(ctx context.Context, touches []authz.Tuple, _ []authz.Filter, p []authz.Precondition) (authz.Token, error) {
	return r.Touch(ctx, touches, p)
}

// startCostRelay runs the relay of port over the outbox of e until the test ends.
func startCostRelay(t *testing.T, e *env, port authz.Relationships) {
	t.Helper()
	relations := []authz.Relation{{ResourceType: "cost_item", Name: "org"}, {ResourceType: "cost_item", Name: "billing_org"},
		{ResourceType: "price_history", Name: "cost_item"}, {ResourceType: "cost_group", Name: "org"}, {ResourceType: "cost_item", Name: "cost_group"},
		{ResourceType: "cost_item", Name: "owner"}, {ResourceType: "cost_item", Name: "tenant"}}
	relay, err := authz.NewRelay(authz.RelayConfig{Name: "authz-relay", Relations: relations, Relationships: port,
		DB: e.openPool(e.dsn(e.roles[2])), Schema: "authz"})
	if err != nil {
		t.Fatal(err)
	}
	config := outbox.DefaultWorkerConfig()
	config.PollInterval, config.RetryMin, config.RetryMax, config.AttemptTimeout = 10*time.Millisecond, 10*time.Millisecond, 50*time.Millisecond, 2*time.Second
	worker, err := outbox.NewWorker(e.wqueue, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Register(relay.Destination()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = worker.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

// revocationOf gives the revocation of a result and its tuples in the revocation table.
func (c *costEnv) revocationOf(res Result) (uuid.UUID, int) {
	c.t.Helper()
	location := res.Response.Header.Get(authz.RevocationLocationHeader)
	id, err := uuid.Parse(strings.TrimPrefix(location, authz.RevocationPath))
	if err != nil {
		c.t.Fatalf("the call answered the revocation %q", location)
	}
	return id, c.count(`SELECT count(*) FROM authz.authz_revocation_tuples WHERE revocation_id = $1`, id)
}

func TestCreatedRows(t *testing.T) {
	t.Run("ST-SUP-RP10-LC59-T01 A command creates a row and a row that refers to it in one call, and both commit with their tuples", func(t *testing.T) {
		c := newCostEnv(t)
		c.exec(`INSERT INTO orgs (tenant_id, id, name) VALUES ('t1', 'o1', 'O1')`)
		c.decision.setRelations(c.grants()...)
		cmd := c.addCostItem(true, []Witness{priceWitness}, nil, c.orgs, c.items, c.prices)
		before := c.tuples()
		var inFill []string
		res, err := c.call(cmd, "k-lc59-t01-a", func(ctx context.Context, h *Handle[testProbe]) error {
			c.decision.taken()
			err := c.pair(c.items, "p1", "h1")(ctx, h)
			inFill = c.decided(true)
			return err
		})
		if err != nil {
			t.Fatalf("the create of a CostItem and its PriceHistory gave %v, want a commit", err)
		}
		if count(inFill, "user:u-7 catalog_edit org:o1") != 1 ||
			slices.ContainsFunc(inFill, func(d string) bool { return strings.HasSuffix(d, " cost_item:p1") }) {
			t.Fatalf("the fill was decided as %v, want one catalog_edit on org:o1 for the caller and no decision on cost_item:p1", inFill)
		}
		if c.count(`SELECT count(*) FROM cost_items WHERE id = 'p1'`) != 1 || c.count(`SELECT count(*) FROM price_histories WHERE id = 'h1'`) != 1 {
			t.Fatal("the rows did not commit")
		}
		added := c.tuples()[len(before):]
		if !slices.Equal(added, []string{"touch cost_item:p1#org@org:o1", "touch price_history:h1#cost_item@cost_item:p1"}) {
			t.Fatalf("the call enqueued %v, want the relation of each reference", added)
		}
		id, tuples := c.revocationOf(res)
		status, err := c.authz.Status(c.ctx, mustTenant(t, "t1"), u7(t), id)
		if err != nil || status.State != authz.RevocationPending || tuples != 2 {
			t.Fatalf("the revocation of the call is %+v (%v) with %d tuples, want pending with both tuples", status, err, tuples)
		}
		// The relay writes the tuples: the revocation completes, and edit on p1 is allowed at the provider.
		startCostRelay(t, c.env, relayShim{c: c})
		deadline := time.Now().Add(10 * time.Second)
		for status.State != authz.RevocationComplete && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
			status, _ = c.authz.Status(c.ctx, mustTenant(t, "t1"), u7(t), id)
		}
		if status.State != authz.RevocationComplete {
			t.Fatalf("the revocation is %s after the relay, want complete", status.State)
		}
		c.decision.taken()
		if _, err := c.call(cmd, "k-lc59-t01-b", func(ctx context.Context, h *Handle[testProbe]) error {
			p, err := repo(t, h, c.items).Get(ctx, "p1")
			if err != nil {
				return err
			}
			_, err = repo(t, h, c.prices).Create(ctx, priceRow{ID: "h1b", CostItemID: "p1", Price: "13"}, p)
			return err
		}); err != nil || !slices.Contains(c.decided(false), "edit cost_item:p1") {
			t.Fatalf("a second call on p1 after the relay gave %v, want edit on cost_item:p1 allowed at the provider", err)
		}

		// A chain: a CostGroup g1 under o1, a CostItem p2 in g1 and the PriceHistory h2 of p2. The reference of p2 is
		// proved by the record of g1, and the reference of h2 by the decision that the runtime made for p2.
		chain := c.addCostItem(true, []Witness{
			{Entity: "CostItem", Field: "group_id", Subject: SubjectCaller, Permission: "add_item", Kind: WitnessReference, Relation: "org", Decided: "catalog_edit"},
			{Entity: "PriceHistory", Field: "cost_item_id", Subject: SubjectCaller, Permission: "edit", Kind: WitnessReference, Relation: "cost_group", Decided: "add_item"},
		}, nil, c.orgs, c.groups, c.grouped, c.prices)
		c.decision.taken()
		if _, err := c.call(chain, "k-lc59-t01-c", func(ctx context.Context, h *Handle[testProbe]) error {
			o, err := repo(t, h, c.orgs).Get(ctx, "o1")
			if err != nil {
				return err
			}
			g, err := repo(t, h, c.groups).Create(ctx, costGroup{ID: "g1", OrgID: "o1"}, o)
			if err != nil {
				return err
			}
			p, err := repo(t, h, c.grouped).Create(ctx, costItem{ID: "p2", GroupID: "g1"}, g)
			if err != nil {
				return err
			}
			_, err = repo(t, h, c.prices).Create(ctx, priceRow{ID: "h2", CostItemID: "p2", Price: "1"}, p)
			return err
		}); err != nil {
			t.Fatalf("the chain gave %v, want a commit", err)
		}
		if got := c.decided(false); slices.Contains(got, "add_item cost_group:g1") || slices.Contains(got, "edit cost_item:p2") {
			t.Fatalf("the chain was decided as %v, want no decision on g1 and p2 at the provider", got)
		}
	})

	t.Run("ST-SUP-RP10-LC59-T02 A reference to an existing row, or to a row of this call with no witness record, is still decided at the provider", func(t *testing.T) {
		c := newCostEnv(t)
		c.exec(`INSERT INTO orgs (tenant_id, id, name) VALUES ('t1', 'o1', 'O1'), ('t1', 'o3', 'O3')`,
			`INSERT INTO cost_items (tenant_id, id, org_id) VALUES ('t1', 'p0', 'o1')`)
		c.decision.setRelations(c.grants(relation(t, "user:u-7", "view", "t1", "cost_item", "p0"),
			relation(t, "user:u-7", "catalog_edit", "t1", "org", "o3"), relation(t, "user:u-7", "view", "t1", "org", "o3"))...)
		cmd := c.addCostItem(true, []Witness{priceWitness}, nil, c.orgs, c.items, c.prices)
		before := c.tuples()
		c.decision.taken()
		if _, err := c.call(cmd, "k-lc59-t02-a", func(ctx context.Context, h *Handle[testProbe]) error {
			p, err := repo(t, h, c.items).Get(ctx, "p0")
			if err != nil {
				return err
			}
			_, err = repo(t, h, c.prices).Create(ctx, priceRow{ID: "h0", CostItemID: "p0"}, p)
			return err
		}); !errors.Is(err, ErrNotFound) || !slices.Contains(c.decided(false), "edit cost_item:p0") {
			t.Fatalf("a reference to the existing p0 gave %v, want not found from edit on cost_item:p0 at the provider", err)
		}
		c.none("k-lc59-t02-a", len(before), "h0")
		// p3 holds only the reference billing_org_id to o3, whose decision is no witness of edit.
		if _, err := c.call(cmd, "k-lc59-t02-b", func(ctx context.Context, h *Handle[testProbe]) error {
			o, err := repo(t, h, c.orgs).Get(ctx, "o3")
			if err != nil {
				return err
			}
			p, err := repo(t, h, c.items).Create(ctx, costItem{ID: "p3", BillingOrgID: "o3"}, o)
			if err != nil {
				return err
			}
			_, err = repo(t, h, c.prices).Create(ctx, priceRow{ID: "h3", CostItemID: "p3"}, p)
			return err
		}); !errors.Is(err, ErrNotFound) || !slices.Contains(c.decided(false), "edit cost_item:p3") {
			t.Fatalf("a reference to p3 with no witness record gave %v, want not found from edit on cost_item:p3 at the provider", err)
		}
		c.none("k-lc59-t02-b", len(before), "p3", "h3")
	})

	t.Run("ST-SUP-RP10-LC59-T04 A loaded value that is not the value of an open created-by-Create record cannot pass as one, and the row version is not the test", func(t *testing.T) {
		c := newCostEnv(t)
		c.exec(`INSERT INTO orgs (tenant_id, id, name) VALUES ('t1', 'o1', 'O1')`, `INSERT INTO cost_items (tenant_id, id, org_id) VALUES ('t1', 'p0', 'o1')`)
		c.decision.setRelations(c.grants(relation(t, "user:u-7", "view", "t1", "cost_item", "p0"),
			relation(t, "user:u-7", "catalog_edit", "t1", "cost_item", "p5"), relation(t, "user:u-7", "catalog_edit", "t1", "cost_item", "p8"),
			relation(t, "user:u-7", "view", "t1", "user", "u-c"), relation(t, "user:u-7", "catalog_edit", "t1", "user", "u-c"))...)
		cmd := c.addCostItem(true, []Witness{priceWitness}, func(s *CommandSpec[testProbe]) { s.RetryOnConflict = 1 }, c.orgs, c.items, c.prices, c.users)
		c.seedOrg("t1", "o1", 0)
		c.seedUser("t1", "u-c", "o1", false, 0, 1)
		create := func(ctx context.Context, h *Handle[testProbe], id string) (Loaded[costItem], error) {
			o, err := repo(t, h, c.orgs).Get(ctx, "o1")
			if err != nil {
				return nil, err
			}
			return repo(t, h, c.items).Create(ctx, costItem{ID: id, OrgID: "o1"}, o)
		}
		price := func(ctx context.Context, h *Handle[testProbe], p Ref) error {
			_, err := repo(t, h, c.prices).Create(ctx, priceRow{ID: "h-t04", CostItemID: p.(Loaded[costItem]).ID()}, p)
			return err
		}
		var kept Loaded[costItem]
		if _, err := c.call(cmd, "k-lc59-t04-keep", func(ctx context.Context, h *Handle[testProbe]) error {
			var err error
			kept, err = create(ctx, h, "p4k")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		before := c.tuples()
		attempt := 0
		var first Loaded[costItem]
		cases := map[string]func(ctx context.Context, h *Handle[testProbe]) error{
			"a value of an earlier call": func(ctx context.Context, h *Handle[testProbe]) error { return price(ctx, h, kept) },
			"a value of an earlier attempt": func(ctx context.Context, h *Handle[testProbe]) error {
				attempt++
				if attempt == 1 {
					var err error
					if first, err = create(ctx, h, "p5a"); err != nil {
						return err
					}
					return c.conflictOn(ctx, h, "u-c")
				}
				return price(ctx, h, first)
			},
			"a value of a query handle": func(ctx context.Context, h *Handle[testProbe]) error {
				return price(ctx, h, loaded.New(costItem{ID: "p5q", OrgID: "o1"}, nil, loaded.Ref{Owner: &struct{ query bool }{true},
					Entity: c.items, EntityName: "CostItem", ResourceType: "cost_item", ID: "p5q", Version: 1, Write: 1}))
			},
			"the value of a create after an update": func(ctx context.Context, h *Handle[testProbe]) error {
				p, err := create(ctx, h, "p5")
				if err != nil {
					return err
				}
				if _, err := repo(t, h, c.items).Update(ctx, p, costItem{ID: "p5", OrgID: "o1", GroupID: "x"}); err != nil {
					return err
				}
				return price(ctx, h, p)
			},
			"the value of a create after a delete": func(ctx context.Context, h *Handle[testProbe]) error {
				p, err := create(ctx, h, "p5")
				if err != nil {
					return err
				}
				if err := repo(t, h, c.items).Delete(ctx, p); err != nil {
					return err
				}
				return price(ctx, h, p)
			},
			"the first create after create, delete and create": func(ctx context.Context, h *Handle[testProbe]) error {
				p, err := create(ctx, h, "p8")
				if err != nil {
					return err
				}
				if err := repo(t, h, c.items).Delete(ctx, p); err != nil {
					return err
				}
				if _, err := create(ctx, h, "p8"); err != nil {
					return err
				}
				return price(ctx, h, p)
			},
		}
		for name, fill := range cases {
			c.decision.taken()
			key := "k-lc59-t04-" + strings.ReplaceAll(name, " ", "-")
			if _, err := c.call(cmd, key, fill); !errors.Is(err, ErrInternal) {
				t.Fatalf("%s gave %v, want an internal error", name, err)
			}
			if got := c.decided(false); slices.ContainsFunc(got, func(d string) bool { return strings.HasPrefix(d, "edit cost_item:") }) {
				t.Fatalf("%s was decided at the provider as %v", name, got)
			}
			c.none(key, len(before), "p5", "p5a", "p8", "h-t04")
		}
		// The value that the update returned and a value of a Get are decided at the provider, as before.
		for name, fill := range map[string]func(ctx context.Context, h *Handle[testProbe]) error{
			"the value of the update": func(ctx context.Context, h *Handle[testProbe]) error {
				p, err := create(ctx, h, "p5")
				if err != nil {
					return err
				}
				updated, err := repo(t, h, c.items).Update(ctx, p, costItem{ID: "p5", OrgID: "o1", GroupID: "x"})
				if err != nil {
					return err
				}
				return price(ctx, h, updated)
			},
			"a value of a get": func(ctx context.Context, h *Handle[testProbe]) error {
				p, err := repo(t, h, c.items).Get(ctx, "p0")
				if err != nil {
					return err
				}
				return price(ctx, h, p)
			},
		} {
			c.decision.taken()
			key := "k-lc59-t04-p-" + strings.ReplaceAll(name, " ", "-")
			if _, err := c.call(cmd, key, fill); !errors.Is(err, ErrNotFound) || errors.Is(err, ErrInternal) ||
				!slices.ContainsFunc(c.decided(false), func(d string) bool { return strings.HasPrefix(d, "edit cost_item:") }) {
				t.Fatalf("%s gave %v, want not found from a decision at the provider", name, err)
			}
			c.none(key, len(before), "p5", "h-t04")
		}
	})

	t.Run("ST-SUP-RP10-LC59-T05 Each create of the call writes its audit row with the command and the caller, and the skipped decision is logged at info", func(t *testing.T) {
		c := newCostEnv(t)
		c.exec(`INSERT INTO orgs (tenant_id, id, name) VALUES ('t1', 'o1', 'O1')`)
		c.decision.setRelations(c.grants()...)
		cmd := c.addCostItem(true, []Witness{priceWitness}, nil, c.orgs, c.items, c.prices)
		var logs bytes.Buffer
		previous := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
		t.Cleanup(func() { slog.SetDefault(previous) })
		if _, err := c.call(cmd, "k-lc59-t05", c.pair(c.items, "p1", "h1")); err != nil {
			t.Fatal(err)
		}
		slog.SetDefault(previous)
		rows, err := c.admin.QueryContext(c.ctx, `SELECT resource_id, action, actor_id, operation, request_id, before IS NULL AND after IS NOT NULL, seq
 FROM audit_event WHERE tenant_id = 't1' AND resource_id IN ('p1', 'h1') ORDER BY seq`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var got []string
		var seqs []int64
		for rows.Next() {
			var id, action, actor, operation, request string
			var created bool
			var seq int64
			if err := rows.Scan(&id, &action, &actor, &operation, &request, &created, &seq); err != nil {
				t.Fatal(err)
			}
			got = append(got, strings.Join([]string{id, action, actor, operation, request}, " "))
			seqs = append(seqs, seq)
			if !created {
				t.Errorf("the audit row of %s is not the row of a create", id)
			}
		}
		if !slices.Equal(got, []string{"p1 add-cost-item user:u-7 add-cost-item req-k-lc59-t05", "h1 add-cost-item user:u-7 add-cost-item req-k-lc59-t05"}) ||
			seqs[1] != seqs[0]+1 {
			t.Fatalf("the audit rows are %v at %v, want one create row of p1 and one of h1 in one chain", got, seqs)
		}
		var lines []string
		for _, line := range strings.Split(logs.String(), "\n") {
			if strings.Contains(line, "command.reference.decided_by_create") {
				lines = append(lines, line)
			}
		}
		if len(lines) != 1 || !strings.Contains(lines[0], "level=INFO") || !strings.Contains(lines[0], "action=add-cost-item") ||
			!strings.Contains(lines[0], "entity=PriceHistory") || !strings.Contains(lines[0], "field=cost_item_id") ||
			!strings.Contains(lines[0], "subject_kind=caller") || !strings.Contains(lines[0], "witness_kind=reference") ||
			strings.Contains(lines[0], "p1") || strings.Contains(lines[0], "h1") || strings.Contains(lines[0], "o1") || strings.Contains(lines[0], "12.50") {
			t.Fatalf("the log has %q, want one info line command.reference.decided_by_create with no row ID and no value", lines)
		}
	})

	t.Run("ST-SUP-RP10-LC59-T06 A move to a new parent, and an update or a delete of a new child, are decided at the provider as today", func(t *testing.T) {
		c := newCostEnv(t)
		c.exec(`INSERT INTO orgs (tenant_id, id, name) VALUES ('t1', 'o1', 'O1')`, `INSERT INTO cost_items (tenant_id, id, org_id) VALUES ('t1', 'p0', 'o1')`,
			`INSERT INTO price_histories (tenant_id, id, cost_item_id, price) VALUES ('t1', 'h0', 'p0', '1')`)
		c.decision.setRelations(c.grants(relation(t, "user:u-7", "catalog_edit", "t1", "price_history", "h0"), relation(t, "user:u-7", "view", "t1", "price_history", "h0"))...)
		noteWitness := Witness{Entity: "Note", Field: "cost_item_id", Subject: SubjectCaller, Permission: "edit", Kind: WitnessReference, Relation: "org", Decided: "catalog_edit"}
		cmd := c.addCostItem(true, []Witness{priceWitness, noteWitness}, nil, c.orgs, c.items, c.prices, c.notes)
		before := c.tuples()
		createItem := func(ctx context.Context, h *Handle[testProbe], id string) (Loaded[costItem], error) {
			o, err := repo(t, h, c.orgs).Get(ctx, "o1")
			if err != nil {
				return nil, err
			}
			return repo(t, h, c.items).Create(ctx, costItem{ID: id, OrgID: "o1"}, o)
		}
		c.decision.taken()
		if _, err := c.call(cmd, "k-lc59-t06-move", func(ctx context.Context, h *Handle[testProbe]) error {
			p6, err := createItem(ctx, h, "p6")
			if err != nil {
				return err
			}
			h0, err := repo(t, h, c.prices).GetForUpdate(ctx, "h0")
			if err != nil {
				return err
			}
			v := h0.Value()
			v.CostItemID = "p6"
			_, err = repo(t, h, c.prices).Update(ctx, h0, v, p6)
			return err
		}); !errors.Is(err, ErrNotFound) || !slices.Contains(c.decided(false), "edit cost_item:p6") {
			t.Fatalf("the move to p6 gave %v, want not found from edit on cost_item:p6 at the provider", err)
		}
		c.none("k-lc59-t06-move", len(before), "p6")
		c.decision.taken()
		if _, err := c.call(cmd, "k-lc59-t06-note", func(ctx context.Context, h *Handle[testProbe]) error {
			p7, err := createItem(ctx, h, "p7")
			if err != nil {
				return err
			}
			_, err = repo(t, h, c.notes).Create(ctx, costNote{ID: "n7", CostItemID: "p7", Body: "b"}, p7)
			return err
		}); err != nil || slices.Contains(c.decided(false), "edit cost_item:p7") {
			t.Fatalf("the create of the note n7 under p7 gave %v, want a commit decided by the record of p7", err)
		}
		before = c.tuples()
		for verb, write := range map[string]func(ctx context.Context, h *Handle[testProbe], n Loaded[costNote], p Ref) error{
			"update": func(ctx context.Context, h *Handle[testProbe], n Loaded[costNote], p Ref) error {
				_, err := repo(t, h, c.notes).Update(ctx, n, costNote{ID: n.ID(), CostItemID: "p7" + n.ID(), Body: "c"}, p)
				return err
			},
			"delete": func(ctx context.Context, h *Handle[testProbe], n Loaded[costNote], p Ref) error {
				return repo(t, h, c.notes).Delete(ctx, n, p)
			},
		} {
			c.decision.taken()
			key := "k-lc59-t06-" + verb
			if _, err := c.call(cmd, key, func(ctx context.Context, h *Handle[testProbe]) error {
				p, err := createItem(ctx, h, "p7"+verb)
				if err != nil {
					return err
				}
				n, err := repo(t, h, c.notes).Create(ctx, costNote{ID: verb, CostItemID: "p7" + verb, Body: "b"}, p)
				if err != nil {
					return err
				}
				return write(ctx, h, n, p)
			}); !errors.Is(err, ErrNotFound) || !slices.Contains(c.decided(false), "edit cost_item:p7"+verb) {
				t.Fatalf("the %s of a new note gave %v, want not found from edit on its parent at the provider", verb, err)
			}
			c.none(key, len(before), "p7"+verb, verb)
		}
	})

	t.Run("ST-SUP-RP10-LC59-T08 With no opt-in, or for a reference with no witness, a reference to a row of the same attempt is decided as before", func(t *testing.T) {
		c := newCostEnv(t)
		c.exec(`INSERT INTO orgs (tenant_id, id, name) VALUES ('t1', 'o1', 'O1')`)
		c.decision.setRelations(c.grants()...)
		before := c.tuples()
		// A command that does not declare creates_with_children takes no witness, so nothing can prove a reference.
		if _, err := NewCommand(CommandSpec[testProbe]{Name: "add-cost-item", Probe: newTestProbe, Writes: []Declared{c.orgs, c.items, c.prices},
			Witnesses: []Witness{priceWitness}}); err == nil || !strings.Contains(err.Error(), "creates_with_children") {
			t.Fatalf("a command with witnesses and no opt-in gave %v, want a refusal", err)
		}
		// With edit = org->catalog_edit - org->frozen, checking gives no witness of edit, so the table holds none.
		for name, cmd := range map[string]*Command[testProbe]{
			"no opt-in":  c.addCostItem(false, nil, nil, c.orgs, c.items, c.prices),
			"no witness": c.addCostItem(true, []Witness{}, nil, c.orgs, c.items, c.prices),
		} {
			c.decision.taken()
			key := "k-lc59-t08-" + strings.ReplaceAll(name, " ", "-")
			if _, err := c.call(cmd, key, c.pair(c.items, "p1", "h1")); !errors.Is(err, ErrNotFound) || !slices.Contains(c.decided(false), "edit cost_item:p1") {
				t.Fatalf("%s gave %v, want not found from edit on cost_item:p1 at the provider", name, err)
			}
			c.none(key, len(before), "p1", "h1")
		}
	})

	t.Run("ST-SUP-RP10-LC59-T11 A command with no permission records no witness from a reference that names none", func(t *testing.T) {
		c := newCostEnv(t)
		c.exec(`INSERT INTO orgs (tenant_id, id, name) VALUES ('t1', 'o1', 'O1')`)
		c.decision.setRelations(c.grants()...)
		noPermission := func(s *CommandSpec[testProbe]) { s.Permission, s.Resource = "", nil }
		owner := Witness{Entity: "PriceHistory", Field: "cost_item_id", Subject: SubjectCaller, Permission: "edit", Kind: WitnessRelationship, Relation: "owner"}
		cmd := c.addCostItem(true, []Witness{owner}, noPermission, c.orgs, c.owned, c.prices)
		c.decision.taken()
		if _, err := c.call(cmd, "k-lc59-t11-a", c.pair(c.owned, "p1", "h1")); err != nil {
			t.Fatalf("the create by the caller relationship witness gave %v, want a commit", err)
		}
		if got := c.decided(false); !slices.Equal(got, []string{"view org:o1"}) {
			t.Fatalf("the call was decided as %v, want only the read of o1: the create of p1 decides nothing on org:o1", got)
		}
		// The same with no caller relationship: the table gets a reference witness that checking would not give, so
		// the runtime alone is seen; decideReference returned with no evaluate on o1, so no record matches it.
		bare := c.addCostItem(true, nil, noPermission, c.orgs, c.bare, c.prices)
		bare.witnesses = witnessTable{{entity: "PriceHistory", field: "cost_item_id", subject: SubjectCaller, permission: "edit"}: {
			{Entity: "PriceHistory", Field: "cost_item_id", Subject: SubjectCaller, Permission: "edit", Kind: WitnessReference, Relation: "org"}}}
		before := c.tuples()
		c.decision.taken()
		if _, err := c.call(bare, "k-lc59-t11-b", c.pair(c.bare, "p2", "h2")); !errors.Is(err, ErrNotFound) || !slices.Contains(c.decided(false), "edit cost_item:p2") {
			t.Fatalf("the create with no witness record gave %v, want not found from edit on cost_item:p2 at the provider", err)
		}
		c.none("k-lc59-t11-b", len(before), "p2", "h2")
	})

	t.Run("ST-SUP-RP10-LC59-T12 Only Create writes the created-by-Create record: an updated existing row and a row of another entity of the same resource type are decided at the provider", func(t *testing.T) {
		c := newCostEnv(t)
		c.exec(`INSERT INTO orgs (tenant_id, id, name) VALUES ('t1', 'o1', 'O1')`, `INSERT INTO cost_items (tenant_id, id, org_id) VALUES ('t1', 'p0', 'o1')`,
			`INSERT INTO kits (tenant_id, id, name) VALUES ('t1', 'k1', 'K1')`)
		c.decision.setRelations(c.grants(relation(t, "user:u-7", "catalog_edit", "t1", "cost_item", "p0"),
			relation(t, "user:u-7", "view", "t1", "cost_item", "p0"), relation(t, "user:u-7", "view", "t1", "item", "k1"))...)
		cmd := c.addCostItem(true, []Witness{priceWitness}, nil, c.orgs, c.items, c.prices)
		before := c.tuples()
		c.decision.taken()
		if _, err := c.call(cmd, "k-lc59-t12-a", func(ctx context.Context, h *Handle[testProbe]) error {
			p0, err := repo(t, h, c.items).GetForUpdate(ctx, "p0")
			if err != nil {
				return err
			}
			updated, err := repo(t, h, c.items).Update(ctx, p0, costItem{ID: "p0", OrgID: "o1", GroupID: "x"})
			if err != nil {
				return err
			}
			_, err = repo(t, h, c.prices).Create(ctx, priceRow{ID: "h12", CostItemID: "p0"}, updated)
			return err
		}); !errors.Is(err, ErrNotFound) || !slices.Contains(c.decided(false), "edit cost_item:p0") {
			t.Fatalf("a reference to an updated existing row gave %v, want not found from edit on cost_item:p0 at the provider", err)
		}
		c.none("k-lc59-t12-a", len(before), "h12")
		if c.count(`SELECT count(*) FROM cost_items WHERE id = 'p0' AND group_id = ''`) != 1 {
			t.Fatal("the update of p0 committed")
		}
		kit := Witness{Entity: "KitPart", Field: "kit_id", Subject: SubjectCaller, Permission: "edit", Kind: WitnessRelationship, Relation: "owner"}
		kits := c.addCostItem(true, []Witness{kit}, nil, c.orgs, c.kitItems, c.kits, c.parts)
		c.decision.taken()
		if _, err := c.call(kits, "k-lc59-t12-b", func(ctx context.Context, h *Handle[testProbe]) error {
			if _, err := repo(t, h, c.kitItems).Create(ctx, kitRow{ID: "k1", Name: "I"}); err != nil {
				return err
			}
			k, err := repo(t, h, c.kits).Get(ctx, "k1")
			if err != nil {
				return err
			}
			_, err = repo(t, h, c.parts).Create(ctx, kitPart{ID: "kp1", KitID: "k1"}, k)
			return err
		}); !errors.Is(err, ErrNotFound) || !slices.Contains(c.decided(false), "edit item:k1") {
			t.Fatalf("a reference to the Kit k1 gave %v, want not found from edit on item:k1 at the provider", err)
		}
		c.none("k-lc59-t12-b", len(before), "kp1", "k1")
	})

	t.Run("ST-SUP-RP10-LC59-T13 A pod uses a proof only while the served schema is its own release's: an older pod beside a newer schema, a failed read and a mismatched read-back fail closed, and order and comments do not change the digest", func(t *testing.T) {
		// The runtime half: the schema gate of the pod of r1 over a reader of the served schema (the migration job
		// half is in registry/runtime/service/migrate_test.go).
		c := newCostEnv(t)
		c.exec(`INSERT INTO orgs (tenant_id, id, name) VALUES ('t1', 'o1', 'O1')`)
		c.decision.setRelations(c.grants()...)
		cmd := c.addCostItem(true, []Witness{priceWitness}, nil, c.orgs, c.items, c.prices)
		n := 0
		run := func() (error, bool) {
			n++
			c.decision.taken()
			id := strings.Repeat("x", n)
			_, err := c.call(cmd, "k-lc59-t13-"+id, c.pair(c.items, "p"+id, "h"+id))
			return err, slices.Contains(c.decided(false), "edit cost_item:p"+id)
		}
		if err, provider := run(); err != nil || provider {
			t.Fatalf("the run of r1 gave %v (provider %t), want a commit with no decision of edit", err, provider)
		}
		// r2 writes another schema of the same Release name: within the bound the reading of before holds.
		c.served.set(costSchemaR2, nil, false)
		c.clock.add(time.Second)
		if err, provider := run(); err != nil || provider {
			t.Fatalf("the run within the bound gave %v (provider %t), want the reading of before", err, provider)
		}
		c.clock.add(authz.SchemaDigestBound)
		if err, provider := run(); !errors.Is(err, ErrNotFound) || !provider {
			t.Fatalf("the run of the r1 pod beside the r2 schema gave %v (provider %t), want not found at the provider", err, provider)
		}
		// A ReadSchema error and a ReadSchema past its time limit decide at the provider.
		c.served.set(costSchema, errors.New("unavailable"), false)
		c.clock.add(authz.SchemaDigestBound)
		if err, provider := run(); !errors.Is(err, ErrNotFound) || !provider {
			t.Fatalf("the run with a ReadSchema error gave %v (provider %t), want not found at the provider", err, provider)
		}
		c.served.set(costSchema, nil, true)
		start := time.Now()
		if err, provider := run(); !errors.Is(err, ErrNotFound) || !provider || time.Since(start) > 3*authz.SchemaReadLimit {
			t.Fatalf("the run with a ReadSchema past its limit gave %v (provider %t) after %v, want not found at the provider", err, provider, time.Since(start))
		}
		// Eight decisions at once after the bound make one ReadSchema call.
		c.served.set(costSchema, nil, false)
		c.served.gate = make(chan struct{})
		c.clock.add(authz.SchemaDigestBound)
		from := c.served.count()
		var wg sync.WaitGroup
		results := make(chan bool, 8)
		for range 8 {
			wg.Add(1)
			go func() { defer wg.Done(); results <- c.gate.Holds(context.Background()) }()
		}
		time.Sleep(200 * time.Millisecond)
		close(c.served.gate)
		wg.Wait()
		close(results)
		c.served.gate = nil
		for holds := range results {
			if !holds {
				t.Fatal("a decision of the eight did not take the shared reading")
			}
		}
		if got := c.served.count() - from; got != 1 {
			t.Fatalf("eight decisions at once made %d ReadSchema calls, want one", got)
		}
		// The schema of r1 written again in another order and with other comments has the digest of r1.
		c.served.set(costSchemaReordered, nil, false)
		c.clock.add(authz.SchemaDigestBound)
		if err, provider := run(); err != nil || provider {
			t.Fatalf("the run beside the reordered schema of r1 gave %v (provider %t), want a commit with no decision of edit", err, provider)
		}
	})

	t.Run("ST-SUP-RP10-LC59-T14 The witness tables are frozen when the handle is bound, and a witness that is not a runtime relation is refused", func(t *testing.T) {
		c := newCostEnv(t)
		c.exec(`INSERT INTO orgs (tenant_id, id, name) VALUES ('t1', 'o3', 'O3')`)
		c.decision.setRelations(relation(t, "user:u-7", "catalog_edit", "t1", "org", "o1"), relation(t, "user:u-7", "catalog_edit", "t1", "org", "o3"),
			relation(t, "user:u-7", "view", "t1", "org", "o3"))
		c.exec(`INSERT INTO orgs (tenant_id, id, name) VALUES ('t1', 'o1', 'O1')`)
		witnesses := []Witness{priceWitness}
		cmd := c.addCostItem(true, witnesses, nil, c.orgs, c.ownedAll, c.prices)
		// The spec that was bound changes: a relationship witness of PriceHistory.cost_item_id is added.
		witnesses[0] = Witness{Entity: "PriceHistory", Field: "cost_item_id", Subject: SubjectCaller, Permission: "edit", Kind: WitnessRelationship, Relation: "owner"}
		before := c.tuples()
		c.decision.taken()
		if _, err := c.call(cmd, "k-lc59-t14", func(ctx context.Context, h *Handle[testProbe]) error {
			o, err := repo(t, h, c.orgs).Get(ctx, "o3")
			if err != nil {
				return err
			}
			p, err := repo(t, h, c.ownedAll).Create(ctx, costItem{ID: "p3", BillingOrgID: "o3"}, o)
			if err != nil {
				return err
			}
			_, err = repo(t, h, c.prices).Create(ctx, priceRow{ID: "h3", CostItemID: "p3"}, p)
			return err
		}); !errors.Is(err, ErrNotFound) || !slices.Contains(c.decided(false), "edit cost_item:p3") {
			t.Fatalf("the run after the change of the bound spec gave %v, want not found from edit on cost_item:p3 at the provider", err)
		}
		c.none("k-lc59-t14", len(before), "p3", "h3")
		for name, w := range map[string]Witness{
			"a relation of no runtime relation": {Entity: "PriceHistory", Field: "cost_item_id", Subject: SubjectCaller, Permission: "edit", Kind: WitnessRelationship, Relation: "viewer"},
			"a stager in a command":             {Entity: "PriceHistory", Field: "cost_item_id", Subject: SubjectStager, Permission: "edit", Kind: WitnessReference, Relation: "org", Decided: "edit"},
			"a permission that is not decided":  {Entity: "PriceHistory", Field: "cost_item_id", Subject: SubjectCaller, Permission: "view", Kind: WitnessReference, Relation: "org", Decided: "catalog_edit"},
			"a decided permission not decided":  {Entity: "PriceHistory", Field: "cost_item_id", Subject: SubjectCaller, Permission: "edit", Kind: WitnessReference, Relation: "org", Decided: "view"},
		} {
			_, err := NewCommand(CommandSpec[testProbe]{Name: "add-cost-item", Probe: newTestProbe, Writes: []Declared{c.orgs, c.items, c.prices},
				Permission: "catalog_edit", Resource: &Resource{Type: "org", Token: `SELECT authz_token FROM orgs WHERE tenant_id = $1 AND id = $2 FOR SHARE`, ValidID: func(string) bool { return true }},
				CreatesWithChildren: true, Witnesses: []Witness{w}})
			if err == nil || !strings.Contains(err.Error(), "PriceHistory.cost_item_id") || !strings.Contains(err.Error(), w.Relation) {
				t.Fatalf("the bind of %s gave %v, want a refusal that names the entity and the witness", name, err)
			}
		}
		// Security review of PR #30, L2: a relationship witness is the caller's only, so a worker's is refused; and only
		// a required reference of P is a witness, so a reference witness through an optional one is refused.
		_, err := NewWorker(WorkerSpec[testProbe]{Name: "sync-items", Permission: "sync", Interval: time.Minute, Probe: newTestProbe,
			Writes: []Declared{c.orgs, c.ownedAll, c.prices}, CreatesWithChildren: true,
			Witnesses: []Witness{{Entity: "PriceHistory", Field: "cost_item_id", Subject: SubjectWorker, Permission: "sync", Kind: WitnessRelationship, Relation: "owner"}}})
		if err == nil || !strings.Contains(err.Error(), "PriceHistory.cost_item_id") || !strings.Contains(err.Error(), "owner") {
			t.Fatalf("the bind of a worker's relationship witness gave %v, want a refusal that names the entity and the witness", err)
		}
		optional := c.items.spec
		optional.Parent = slices.Clone(optional.Parent)
		optional.Parent[0].Required = false
		optionalItems, err := NewEntity(optional)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := NewCommand(CommandSpec[testProbe]{Name: "add-cost-item", Probe: newTestProbe, Writes: []Declared{c.orgs, optionalItems, c.prices},
			Permission: "catalog_edit", Resource: &Resource{Type: "org", Token: `SELECT authz_token FROM orgs WHERE tenant_id = $1 AND id = $2 FOR SHARE`, ValidID: func(string) bool { return true }},
			CreatesWithChildren: true, Witnesses: []Witness{priceWitness}}); err == nil || !strings.Contains(err.Error(), "PriceHistory.cost_item_id") ||
			!strings.Contains(err.Error(), "required reference") {
			t.Fatalf("the bind of a witness through an optional reference gave %v, want a refusal that names the entity and the witness", err)
		}
		if _, err := NewCommand(CommandSpec[testProbe]{Name: "add-cost-item", Probe: newTestProbe, Writes: []Declared{c.orgs, c.items, c.prices},
			Permission: "catalog_edit", Resource: &Resource{Type: "org", Token: `SELECT authz_token FROM orgs WHERE tenant_id = $1 AND id = $2 FOR SHARE`, ValidID: func(string) bool { return true }},
			CreatesWithChildren: true, Witnesses: []Witness{priceWitness}}); err != nil {
			t.Fatalf("the bind of the same witness through the required reference gave %v", err)
		}
	})

	t.Run("ST-SUP-RP10-LC59-T15 A witness record matches only the reference and the object that the new row's tuple names", func(t *testing.T) {
		c := newCostEnv(t)
		c.exec(`INSERT INTO orgs (tenant_id, id, name) VALUES ('t1', 'o1', 'O1'), ('t1', 'o2', 'O2')`)
		c.decision.setRelations(c.grants(relation(t, "user:u-7", "catalog_edit", "t1", "org", "o2"), relation(t, "user:u-7", "view", "t1", "org", "o2"))...)
		cmd := c.addCostItem(true, []Witness{priceWitness}, nil, c.orgs, c.items, c.prices)
		var hook func(records []witnessRecord) []witnessRecord
		c.crt.witnessRecordHook = func(entity string, records []witnessRecord) []witnessRecord {
			if entity == "CostItem" && hook != nil {
				return hook(records)
			}
			return records
		}
		fill := func(item, price string) func(ctx context.Context, h *Handle[testProbe]) error {
			return func(ctx context.Context, h *Handle[testProbe]) error {
				o1, err := repo(t, h, c.orgs).Get(ctx, "o1")
				if err != nil {
					return err
				}
				o2, err := repo(t, h, c.orgs).Get(ctx, "o2")
				if err != nil {
					return err
				}
				p, err := repo(t, h, c.items).Create(ctx, costItem{ID: item, OrgID: "o1", BillingOrgID: "o2"}, o1, o2)
				if err != nil {
					return err
				}
				_, err = repo(t, h, c.prices).Create(ctx, priceRow{ID: price, CostItemID: item}, p)
				return err
			}
		}
		before := c.tuples()
		for i, h := range []func([]witnessRecord) []witnessRecord{
			// A later release skips the duplicate decision: only the allow on o2 for billing_org_id is left.
			func(records []witnessRecord) []witnessRecord {
				return slices.DeleteFunc(records, func(r witnessRecord) bool { return r.field == "org_id" })
			},
			nil,
			// A builder stores the reference wrongly: the record of o2 is written for org_id.
			func(records []witnessRecord) []witnessRecord {
				for i := range records {
					if records[i].field == "org_id" {
						records[i].resourceID = "o2"
					}
				}
				return records
			},
		} {
			hook = h
			c.decision.taken()
			id := []string{"a", "b", "c"}[i]
			_, err := c.call(cmd, "k-lc59-t15-"+id, fill("p1"+id, "h1"+id))
			provider := slices.Contains(c.decided(false), "edit cost_item:p1"+id)
			if (i == 1) != (err == nil) || (i == 1) == provider {
				t.Fatalf("run %d gave %v (provider %t), want the second to commit with no decision on p1 and the others not found at the provider", i+1, err, provider)
			}
			if i != 1 {
				c.none("k-lc59-t15-"+id, len(before), "p1"+id, "h1"+id)
			} else {
				before = c.tuples()
			}
		}
	})

	t.Run("ST-SUP-RP10-LC59-T16 A client-chosen ID of an existing row, an upsert and a move never open a created-by-Create record", func(t *testing.T) {
		c := newCostEnv(t)
		c.exec(`INSERT INTO orgs (tenant_id, id, name) VALUES ('t1', 'o1', 'O1')`, `INSERT INTO cost_items (tenant_id, id, org_id) VALUES ('t1', 'p0', 'o1'), ('t1', 'p00', 'o1')`,
			`INSERT INTO price_histories (tenant_id, id, cost_item_id, price) VALUES ('t1', 'h0', 'p00', '1')`)
		c.decision.setRelations(c.grants(relation(t, "user:u-7", "view", "t1", "cost_item", "p0"), relation(t, "user:u-7", "catalog_edit", "t1", "cost_item", "p0"),
			relation(t, "user:u-7", "edit", "t1", "cost_item", "p00"),
			relation(t, "user:u-7", "view", "t1", "price_history", "h0"), relation(t, "user:u-7", "catalog_edit", "t1", "price_history", "h0"))...)
		cmd := c.addCostItem(true, []Witness{priceWitness}, nil, c.orgs, c.items, c.prices)
		before := c.tuples()
		open := func(h *Handle[testProbe], id string) bool {
			rec := h.st.created[rowKey{"CostItem", id}]
			return rec != nil && rec.open
		}

		// The create of the ID of the existing p0 is a unique violation: no record opens, the call is broken, and a
		// later reference to p0 in the call is never decided by a record.
		var createErr, laterErr error
		var recorded bool
		_, err := c.call(cmd, "k-lc59-t16-taken", func(ctx context.Context, h *Handle[testProbe]) error {
			o, err := repo(t, h, c.orgs).Get(ctx, "o1")
			if err != nil {
				return err
			}
			_, createErr = repo(t, h, c.items).Create(ctx, costItem{ID: "p0", OrgID: "o1"}, o)
			recorded = open(h, "p0") || len(h.st.createTokens) != 0
			// The fill goes on as if the create had written p0: the handle is broken.
			r, err := Repo(h, c.items)
			if err == nil {
				var p Loaded[costItem]
				if p, err = r.Get(ctx, "p0"); err == nil {
					var prices *Repository[priceRow]
					if prices, err = Repo(h, c.prices); err == nil {
						_, err = prices.Create(ctx, priceRow{ID: "h16a", CostItemID: "p0"}, p)
					}
				}
			}
			laterErr = err
			return nil
		})
		if !errors.Is(createErr, ErrConflict) || recorded || laterErr == nil || !errors.Is(err, ErrConflict) {
			t.Fatalf("the create of the existing ID p0 gave %v (record %t), the later create %v and the call %v, want a conflict, no record and a broken call",
				createErr, recorded, laterErr, err)
		}
		c.none("k-lc59-t16-taken", len(before), "h16a")

		// The update branch of an upsert of the client-chosen ID p0, and a move of the existing h0 to p0, write no
		// record: a reference to the value that the update returned is decided at the provider.
		for name, fill := range map[string]func(ctx context.Context, h *Handle[testProbe]) (Loaded[costItem], error){
			"the update branch of an upsert": func(ctx context.Context, h *Handle[testProbe]) (Loaded[costItem], error) {
				p0, err := repo(t, h, c.items).GetForUpdate(ctx, "p0")
				if err != nil {
					return nil, err
				}
				return repo(t, h, c.items).Update(ctx, p0, costItem{ID: "p0", OrgID: "o1", GroupID: "upsert"})
			},
			"a move to the existing row": func(ctx context.Context, h *Handle[testProbe]) (Loaded[costItem], error) {
				p0, err := repo(t, h, c.items).Get(ctx, "p0")
				if err != nil {
					return nil, err
				}
				h0, err := repo(t, h, c.prices).GetForUpdate(ctx, "h0")
				if err != nil {
					return nil, err
				}
				if _, err := repo(t, h, c.prices).Update(ctx, h0, priceRow{ID: "h0", CostItemID: "p0", Price: "2"}, p0); err != nil {
					return nil, err
				}
				return p0, nil
			},
		} {
			key := "k-lc59-t16-" + strings.ReplaceAll(name, " ", "-")
			recorded = false
			c.decision.taken()
			_, err := c.call(cmd, key, func(ctx context.Context, h *Handle[testProbe]) error {
				p, err := fill(ctx, h)
				if err != nil {
					return err
				}
				recorded = open(h, "p0") || len(h.st.created) != 0 || len(h.st.createTokens) != 0
				_, err = repo(t, h, c.prices).Create(ctx, priceRow{ID: "h16b", CostItemID: "p0"}, p)
				return err
			})
			if recorded || !errors.Is(err, ErrNotFound) || !slices.Contains(c.decided(false), "edit cost_item:p0") {
				t.Fatalf("%s gave %v (record %t), want no record and not found from edit on cost_item:p0 at the provider", name, err, recorded)
			}
			c.none(key, len(before), "h16b")
		}

		// A delete of the existing p0 and a create of the same ID in the call write a new row: its record is the
		// record of that create, and the value that the read before the delete gave is never decided by it.
		c.decision.taken()
		if _, err := c.call(cmd, "k-lc59-t16-again", func(ctx context.Context, h *Handle[testProbe]) error {
			o, err := repo(t, h, c.orgs).Get(ctx, "o1")
			if err != nil {
				return err
			}
			old, err := repo(t, h, c.items).GetForDelete(ctx, "p0")
			if err != nil {
				return err
			}
			if err := repo(t, h, c.items).Delete(ctx, old); err != nil {
				return err
			}
			if open(h, "p0") {
				return errors.New("the delete of the existing p0 opened a record")
			}
			if _, err := repo(t, h, c.items).Create(ctx, costItem{ID: "p0", OrgID: "o1"}, o); err != nil {
				return err
			}
			_, err = repo(t, h, c.prices).Create(ctx, priceRow{ID: "h16c", CostItemID: "p0"}, old)
			return err
		}); err == nil || !slices.Contains(c.decided(false), "edit cost_item:p0") && !errors.Is(err, ErrInternal) {
			t.Fatalf("a reference by the value read before the delete gave %v, want no proof by the record of the new p0", err)
		}
		c.none("k-lc59-t16-again", len(before), "h16c")
		if c.count(`SELECT count(*) FROM cost_items WHERE id = 'p0' AND group_id = ''`) != 1 || c.count(`SELECT count(*) FROM price_histories WHERE id = 'h0' AND price = '1'`) != 1 {
			t.Fatal("a call of the test changed p0 or h0")
		}
	})

	t.Run("ST-SUP-RP10-LC59-T16 The record and the decision read the ID of a row in the form that the database stores", func(t *testing.T) {
		c := newCostEnv(t)
		c.exec(`INSERT INTO orgs (tenant_id, id, name) VALUES ('t1', 'o1', 'O1')`,
			`CREATE TABLE uuid_items (tenant_id text NOT NULL, id uuid NOT NULL, org_id text NOT NULL DEFAULT '', billing_org_id text NOT NULL DEFAULT '', group_id text NOT NULL DEFAULT '',
 version bigint NOT NULL DEFAULT 1, authz_token text NOT NULL DEFAULT '', PRIMARY KEY (tenant_id, id))`,
			"ALTER TABLE uuid_items ENABLE ROW LEVEL SECURITY", "ALTER TABLE uuid_items FORCE ROW LEVEL SECURITY",
			"CREATE POLICY uuid_items_tenant ON uuid_items USING (tenant_id = current_setting('app.tenant', true)) WITH CHECK (tenant_id = current_setting('app.tenant', true))",
			"GRANT SELECT, INSERT, UPDATE, DELETE ON uuid_items TO "+c.roles[0])
		c.decision.setRelations(c.grants()...)
		s := spec("CostItem", "cost_item", "uuid_items", []string{"org_id", "billing_org_id", "group_id"}, c.items.spec.Scan, c.items.spec.Values, c.items.spec.State)
		s.Parent = c.items.spec.Parent
		items, err := NewEntity(s)
		if err != nil {
			t.Fatal(err)
		}
		cmd := c.addCostItem(true, []Witness{priceWitness}, nil, c.orgs, items, c.prices)
		before, rows := c.tuples(), 0
		for i, template := range []string{"A0EEBC99-9C0B-4EF8-BB6D-6BB9BD380A%d1", "{a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a%d2}", "a0eebc999c0b4ef8bb6d6bb9bd380a%d3",
			" a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a%d4 ", "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a%d5"} {
			for j, raw := range []bool{false, true} {
				form := fmt.Sprintf(template, j)
				stored := strings.ToLower(strings.Trim(strings.TrimSpace(form), "{}"))
				if len(stored) == 32 {
					stored = stored[:8] + "-" + stored[8:12] + "-" + stored[12:16] + "-" + stored[16:20] + "-" + stored[20:]
				}
				child := stored
				if raw {
					child = form
				}
				key := fmt.Sprintf("k-lc59-t16-id-%d-%d", i, j)
				var keys []string
				var createErr error
				var id string
				c.decision.taken()
				_, err := c.call(cmd, key, func(ctx context.Context, h *Handle[testProbe]) error {
					o, err := repo(t, h, c.orgs).Get(ctx, "o1")
					if err != nil {
						return err
					}
					p, err := repo(t, h, items).Create(ctx, costItem{ID: form, OrgID: "o1"}, o)
					for k := range h.st.created {
						keys = append(keys, k.id)
					}
					if createErr = err; err != nil {
						return err
					}
					id = p.ID()
					_, err = repo(t, h, c.prices).Create(ctx, priceRow{ID: key, CostItemID: child}, p)
					return err
				})
				provider := slices.ContainsFunc(c.decided(false), func(d string) bool { return strings.HasPrefix(d, "edit cost_item:") })
				if createErr != nil {
					// A form that the database does not take writes no row and opens no record.
					if len(keys) != 0 || err == nil {
						t.Fatalf("the create of %q gave %v with the records %v, want no record and no commit", form, createErr, keys)
					}
					c.none(key, len(before), key)
					if c.count(`SELECT count(*) FROM uuid_items`) != rows {
						t.Fatalf("the create of %q left a row", form)
					}
					continue
				}
				if id != stored || !slices.Equal(keys, []string{stored}) {
					t.Fatalf("the create of %q gave the ID %q and the records %v, want the stored form %q in both", form, id, keys, stored)
				}
				if child == stored {
					if err != nil || provider {
						t.Fatalf("a reference by the stored form %q gave %v (provider %t), want a commit proved by the record", stored, err, provider)
					}
					before, rows = c.tuples(), rows+1
					continue
				}
				// A reference that names the row in another form is not the loaded value's row: the create is refused
				// before any decision, so no form other than the stored one reaches the record.
				if !errors.Is(err, ErrInternal) || provider {
					t.Fatalf("a reference by the form %q of %q gave %v (provider %t), want an internal error with no decision", form, stored, err, provider)
				}
				c.none(key, len(before), key)
				if c.count(`SELECT count(*) FROM uuid_items`) != rows {
					t.Fatalf("the refused create of %q left a row", form)
				}
			}
		}
	})

	t.Run("ST-SUP-RP10-LC59-T15 A witness record that differs from its witness in the principal, the relation or the decided permission, and a value of another state, prove nothing", func(t *testing.T) {
		// Security review of PR #30, L2: each part of the match of a witness record is needed. A hook of the test
		// changes one part of the records of the CostItem; the reference of the PriceHistory is then decided at the
		// provider, which denies edit on the new CostItem.
		c := newCostEnv(t)
		c.exec(`INSERT INTO orgs (tenant_id, id, name) VALUES ('t1', 'o1', 'O1')`)
		c.decision.setRelations(c.grants()...)
		var hook func(records []witnessRecord) []witnessRecord
		c.crt.witnessRecordHook = func(entity string, records []witnessRecord) []witnessRecord {
			if entity == "CostItem" && hook != nil {
				return hook(records)
			}
			return records
		}
		change := func(match func(witnessRecord) bool, edit func(*witnessRecord)) func([]witnessRecord) []witnessRecord {
			return func(records []witnessRecord) []witnessRecord {
				for i := range records {
					if match(records[i]) {
						edit(&records[i])
					}
				}
				return records
			}
		}
		relationship := func(r witnessRecord) bool { return r.relationship }
		referenceOf := func(r witnessRecord) bool { return r.field == "org_id" }
		noPermission := func(s *CommandSpec[testProbe]) { s.Permission, s.Resource = "", nil }
		owner := Witness{Entity: "PriceHistory", Field: "cost_item_id", Subject: SubjectCaller, Permission: "edit", Kind: WitnessRelationship, Relation: "owner"}
		byOwner := c.addCostItem(true, []Witness{owner}, noPermission, c.orgs, c.owned, c.prices)
		byOrg := c.addCostItem(true, []Witness{priceWitness}, nil, c.orgs, c.items, c.prices)
		before := c.tuples()
		n := 0
		for _, run := range []struct {
			name  string
			cmd   *Command[testProbe]
			items *Entity[costItem]
			hook  func([]witnessRecord) []witnessRecord
			proof bool
		}{
			{"the relationship witness as made", byOwner, c.owned, nil, true},
			{"a relationship record of another principal", byOwner, c.owned, change(relationship, func(r *witnessRecord) { r.subject = "user:u-9" }), false},
			{"a relationship record of another relation", byOwner, c.owned, change(relationship, func(r *witnessRecord) { r.relation = "admin" }), false},
			{"the reference witness as made", byOrg, c.items, nil, true},
			{"a reference record of another decided permission", byOrg, c.items, change(referenceOf, func(r *witnessRecord) { r.permission = "view" }), false},
			{"a reference record of another subject kind", byOrg, c.items, change(referenceOf, func(r *witnessRecord) { r.subject = SubjectWorker }), false},
		} {
			n++
			hook = run.hook
			c.decision.taken()
			id := strings.Repeat("r", n)
			key := "k-lc59-t15-neg-" + id
			_, err := c.call(run.cmd, key, c.pair(run.items, "p"+id, "h"+id))
			provider := slices.Contains(c.decided(false), "edit cost_item:p"+id)
			if run.proof {
				if err != nil || provider {
					t.Fatalf("%s gave %v (provider %t), want a commit proved by the record", run.name, err, provider)
				}
				before = c.tuples()
				continue
			}
			if !errors.Is(err, ErrNotFound) || !provider {
				t.Fatalf("%s gave %v (provider %t), want not found from edit on cost_item:p%s at the provider", run.name, err, provider, id)
			}
			c.none(key, len(before), "p"+id, "h"+id)
		}
		hook = nil

		// A value whose owner is not the state of the handle is decided at the provider even when its entity, its ID
		// and its write token are those of an open record of the state: the owner is part of the test, not only the
		// parent match of the create.
		c.decision.taken()
		var proved bool
		var decideErr error
		if _, err := c.call(byOrg, "k-lc59-t15-owner", func(ctx context.Context, h *Handle[testProbe]) error {
			o, err := repo(t, h, c.orgs).Get(ctx, "o1")
			if err != nil {
				return err
			}
			p, err := repo(t, h, c.items).Create(ctx, costItem{ID: "p-own", OrgID: "o1"}, o)
			if err != nil {
				return err
			}
			other := refOf(p)
			other.st = &state{}
			ref := reference{entity: "CostItem", resourceType: "cost_item", id: p.ID(), permission: "edit", relation: "cost_item", field: "cost_item_id"}
			c.decision.taken()
			allows, err := h.st.decideCreateReference(ctx, "PriceHistory", ref, other)
			proved, decideErr = err == nil && len(allows) > 0 && !slices.Contains(c.decided(false), "edit cost_item:p-own"), err
			return errors.New("the test ends the call")
		}); err == nil {
			t.Fatal("the call committed")
		}
		if proved || !errors.Is(decideErr, ErrNotFound) {
			t.Fatalf("a value of another state was proved (%t) with %v, want not found from edit on cost_item:p-own at the provider", proved, decideErr)
		}
	})
}

// workerFill adapts a fill of the tests to RunWorker.
func workerFill(fill func(ctx context.Context, h *Handle[testProbe]) error) TickFill[testProbe] {
	return tickFill(fill)
}

// consume runs the consumer w once on a message of the stager place-costs with the principal user:u-7 in t1, in a
// transaction of its own that commits when the run returns nil.
func (c *costEnv) consume(w *Worker[testProbe], fill func(ctx context.Context, h *Handle[testProbe]) error) error {
	c.t.Helper()
	tn := mustTenant(c.t, "t1")
	tx, err := tenant.Begin(c.ctx, c.pool, tn, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		c.t.Fatal(err)
	}
	defer tx.Rollback()
	d := outbox.Delivery{Message: outbox.Message{ID: uuid.New(), Destination: "partner-replies", Kind: "reply-received",
		Payload: []byte(`{}`), Principal: "user:u-7", Origin: "place-costs"}, Tenant: tn, Attempts: 1}
	err = RunConsumer(c.ctx, c.crt, w, tx, d, func([]byte) (struct{}, error) { return struct{}{}, nil },
		func(ctx context.Context, _ struct{}, _ string, h *Handle[testProbe]) error { return fill(ctx, h) })
	if err == nil {
		if err := tx.Commit(); err != nil {
			c.t.Fatal(err)
		}
	}
	return err
}

func TestCreatedRowsWorkers(t *testing.T) {
	t.Run("ST-SUP-RP10-LC59-T07 A worker creates a row and a row that refers to it in one run, and a stager is proved only by stager decisions", func(t *testing.T) {
		c := newCostEnv(t)
		syncW := Witness{Entity: "PriceHistory", Field: "cost_item_id", Subject: SubjectWorker, Permission: "sync", Kind: WitnessTenant, Relation: "tenant", Decided: "sync"}
		stageW := Witness{Entity: "PriceHistory", Field: "cost_item_id", Subject: SubjectStager, Permission: "stage", Kind: WitnessTenant, Relation: "tenant", Decided: "stage"}
		pair := func(item, price string) func(ctx context.Context, h *Handle[testProbe]) error {
			return func(ctx context.Context, h *Handle[testProbe]) error {
				p, err := repo(t, h, c.tenanted).Create(ctx, costItem{ID: item})
				if err != nil {
					return err
				}
				_, err = repo(t, h, c.prices).Create(ctx, priceRow{ID: price, CostItemID: item}, p)
				return err
			}
		}
		c.decision.setRelations(relation(t, "service_account:sync-costs", "sync", "t1", "tenant", "t1"))
		w := c.worker("sync-costs", "sync", func(s *WorkerSpec[testProbe]) { s.CreatesWithChildren, s.Witnesses = true, []Witness{syncW} }, c.tenanted, c.prices)
		c.decision.taken()
		if err := RunWorker(c.ctx, c.crt, w, tick, mustTenant(t, "t1"), nil, workerFill(pair("pw1", "hw1"))); err != nil {
			t.Fatalf("the interval run gave %v, want a commit", err)
		}
		if got := c.decided(true); !slices.Equal(got, []string{"service_account:sync-costs sync tenant:t1"}) {
			t.Fatalf("the interval run was decided as %v, want one sync on the tenant resource and none on the new CostItem", got)
		}
		consumer := func(witnesses []Witness) *Worker[testProbe] {
			return c.worker("apply-costs", "sync", func(s *WorkerSpec[testProbe]) {
				s.Interval, s.Stagers, s.CreatesWithChildren, s.Witnesses = 0, map[string]string{"place-costs": "stage"}, true, witnesses
			}, c.tenanted, c.prices)
		}
		c.decision.setRelations(relation(t, "service_account:apply-costs", "sync", "t1", "tenant", "t1"), relation(t, "user:u-7", "stage", "t1", "tenant", "t1"),
			relation(t, "service_account:apply-costs", "sync", "t1", "cost_item", "pc2"))
		c.decision.taken()
		if err := c.consume(consumer([]Witness{syncW, stageW}), pair("pc1", "hc1")); err != nil {
			t.Fatalf("the consuming run with a stager witness gave %v, want a commit", err)
		}
		if got := c.decided(true); !slices.Equal(got, []string{"service_account:apply-costs sync tenant:t1", "user:u-7 stage tenant:t1"}) {
			t.Fatalf("the consuming run was decided as %v, want one worker and one stager decision on the tenant resource", got)
		}
		// cost_item.stage = owner: checking gives no stager witness, while the worker has its tenant witness.
		c.decision.taken()
		err := c.consume(consumer([]Witness{syncW}), pair("pc2", "hc2"))
		if outcome, cause := consumerOutcome(err); cause != "relations_pending" || outcome != "undecided" ||
			!slices.Contains(c.decided(true), "user:u-7 stage cost_item:pc2") {
			t.Fatalf("the consuming run with no stager witness gave %v (%s %s), want undecided relations_pending from stage on cost_item:pc2", err, outcome, cause)
		}
		if c.count(`SELECT count(*) FROM cost_items WHERE id = 'pc2'`) != 0 {
			t.Fatal("the undecided run left its row")
		}
	})

	t.Run("ST-SUP-RP10-LC59-T07 A tenant witness record of another tenant or of another decided permission proves nothing", func(t *testing.T) {
		// Security review of PR #30, L2: a hook of the test changes one part of the tenant record of the CostItem; the
		// reference of the PriceHistory is then decided at the provider, which denies sync on the new CostItem.
		c := newCostEnv(t)
		syncW := Witness{Entity: "PriceHistory", Field: "cost_item_id", Subject: SubjectWorker, Permission: "sync", Kind: WitnessTenant, Relation: "tenant", Decided: "sync"}
		var hook func(*witnessRecord)
		c.crt.witnessRecordHook = func(entity string, records []witnessRecord) []witnessRecord {
			if entity == "CostItem" && hook != nil {
				for i := range records {
					if records[i].relation == authz.TenantRelation {
						hook(&records[i])
					}
				}
			}
			return records
		}
		c.decision.setRelations(relation(t, "service_account:sync-costs", "sync", "t1", "tenant", "t1"))
		w := c.worker("sync-costs", "sync", func(s *WorkerSpec[testProbe]) { s.CreatesWithChildren, s.Witnesses = true, []Witness{syncW} }, c.tenanted, c.prices)
		at := tick
		for i, run := range []struct {
			name  string
			hook  func(*witnessRecord)
			proof bool
		}{
			{"the tenant record as made", nil, true},
			{"a tenant record of another tenant", func(r *witnessRecord) { r.resourceID = "t2" }, false},
			{"a tenant record of another decided permission", func(r *witnessRecord) { r.permission = "stage" }, false},
			{"a tenant record of another resource type", func(r *witnessRecord) { r.resourceType = "org" }, false},
		} {
			hook = run.hook
			at = at.Add(time.Minute)
			id := strings.Repeat("t", i+1)
			c.decision.taken()
			err := RunWorker(c.ctx, c.crt, w, at, mustTenant(t, "t1"), nil, workerFill(func(ctx context.Context, h *Handle[testProbe]) error {
				p, err := repo(t, h, c.tenanted).Create(ctx, costItem{ID: "pw" + id})
				if err != nil {
					return err
				}
				_, err = repo(t, h, c.prices).Create(ctx, priceRow{ID: "hw" + id, CostItemID: "pw" + id}, p)
				return err
			}))
			provider := slices.Contains(c.decided(true), "service_account:sync-costs sync cost_item:pw"+id)
			if run.proof != (err == nil) || run.proof == provider {
				t.Fatalf("%s gave %v (provider %t), want proof %t", run.name, err, provider, run.proof)
			}
			if !run.proof && c.count(`SELECT count(*) FROM cost_items WHERE id = $1`, "pw"+id) != 0 {
				t.Fatalf("%s left its row", run.name)
			}
		}
	})

	t.Run("ST-SUP-RP10-LC59-T10 A worker's create proves from the worker's own permission, never from the permission that the reference names", func(t *testing.T) {
		c := newCostEnv(t)
		c.exec(`INSERT INTO orgs (tenant_id, id, name) VALUES ('t1', 'o1', 'O1')`)
		var records []witnessRecord
		c.crt.witnessRecordHook = func(entity string, r []witnessRecord) []witnessRecord {
			if entity == "CostItem" {
				records = slices.Clone(r)
			}
			return r
		}
		fill := func(item string) func(ctx context.Context, h *Handle[testProbe]) error {
			return func(ctx context.Context, h *Handle[testProbe]) error {
				o, err := repo(t, h, c.orgs).Get(ctx, "o1")
				if err != nil {
					return err
				}
				c.decision.taken()
				p, err := repo(t, h, c.items).Create(ctx, costItem{ID: item, OrgID: "o1"}, o)
				if err != nil {
					return err
				}
				_, err = repo(t, h, c.prices).Create(ctx, priceRow{ID: "h" + item, CostItemID: item}, p)
				return err
			}
		}
		// org.catalog_edit = owner + sync: the decision of sync on o1 proves sync on the CostItem.
		syncW := Witness{Entity: "PriceHistory", Field: "cost_item_id", Subject: SubjectWorker, Permission: "sync", Kind: WitnessReference, Relation: "org", Decided: "sync"}
		w := c.worker("sync-items", "sync", func(s *WorkerSpec[testProbe]) { s.CreatesWithChildren, s.Witnesses = true, []Witness{syncW} }, c.orgs, c.items, c.prices)
		c.decision.setRelations(relation(t, "service_account:sync-items", "sync", "t1", "org", "o1"))
		if err := RunWorker(c.ctx, c.crt, w, tick, mustTenant(t, "t1"), nil, workerFill(fill("p1"))); err != nil {
			t.Fatalf("the first run gave %v, want a commit", err)
		}
		if got := c.decided(true); !slices.Equal(got, []string{"service_account:sync-items sync org:o1"}) {
			t.Fatalf("the creates were decided as %v, want one sync on org:o1 for the service account", got)
		}
		// org.catalog_edit = owner + admin and org.sync = worker_sync: validate warns new-reference-decided, so the table
		// holds no witness. The provider also allows catalog_edit on o1 for every subject.
		everyone := []string{relation(t, "service_account:sync-items", "sync", "t1", "org", "o1"), relation(t, "service_account:sync-items", "catalog_edit", "t1", "org", "o1"),
			relation(t, "service_account:apply-items", "sync", "t1", "org", "o1"), relation(t, "service_account:apply-items", "catalog_edit", "t1", "org", "o1"),
			relation(t, "user:u-7", "stage", "t1", "org", "o1"), relation(t, "user:u-7", "catalog_edit", "t1", "org", "o1")}
		c.decision.setRelations(everyone...)
		bare := c.worker("sync-items", "sync", func(s *WorkerSpec[testProbe]) { s.CreatesWithChildren = true }, c.orgs, c.items, c.prices)
		var denied *deniedError
		err := RunWorker(c.ctx, c.crt, bare, tick.Add(time.Minute), mustTenant(t, "t1"), nil, workerFill(fill("p2")))
		got := c.decided(true)
		if !errors.As(err, &denied) || denied.id != "p2" || !slices.Contains(got, "service_account:sync-items sync cost_item:p2") ||
			slices.ContainsFunc(got, func(d string) bool { return strings.Contains(d, "catalog_edit") }) {
			t.Fatalf("the interval run with no witness gave %v, decided as %v, want the deny of sync on cost_item:p2 at the provider and no catalog_edit", err, got)
		}
		if len(records) != 1 || records[0] != (witnessRecord{subject: SubjectWorker, permission: "sync", resourceType: "org", resourceID: "o1", field: "org_id", relation: "org"}) {
			t.Fatalf("the record of the create of p2 holds %+v, want only sync for the worker", records)
		}
		// A consuming run of the same fill: the deny of the new row with an empty token is undecided, relations_pending.
		consumer := c.worker("apply-items", "sync", func(s *WorkerSpec[testProbe]) {
			s.Interval, s.Stagers, s.CreatesWithChildren = 0, map[string]string{"place-costs": "stage"}, true
		}, c.orgs, c.items, c.prices)
		err = c.consume(consumer, fill("p3"))
		got = c.decided(true)
		if outcome, cause := consumerOutcome(err); outcome != "undecided" || cause != "relations_pending" || !slices.Contains(got, "service_account:apply-items sync cost_item:p3") ||
			slices.ContainsFunc(got, func(d string) bool { return strings.Contains(d, "catalog_edit") }) {
			t.Fatalf("the consuming run with no witness gave %v (%s %s), decided as %v, want undecided relations_pending from sync on cost_item:p3", err, outcome, cause, got)
		}
		if c.count(`SELECT count(*) FROM cost_items WHERE id IN ('p2', 'p3')`) != 0 {
			t.Fatal("a run with no witness left its row")
		}
	})
}

// costServe is the HTTP handler of the command add-price of T03: it creates a PriceHistory of the CostItem that the
// request names, loaded by Get.
func (c *costEnv) costServe(r *revocationEnv, item, price string) http.Handler {
	c.t.Helper()
	cmd := c.addCostItem(true, []Witness{priceWitness}, nil, c.orgs, c.items, c.prices)
	spec := ServeSpec[*structpb.Struct, *structpb.Struct, testProbe]{
		Command: cmd, ResourceType: "org", SubjectType: "user", Status: http.StatusOK,
		NewRequest:  func() *structpb.Struct { return &structpb.Struct{} },
		NewResponse: func() *structpb.Struct { return &structpb.Struct{} },
		Resource:    func(*structpb.Struct) string { return "o1" },
		Fill: func(ctx context.Context, _ *structpb.Struct, h *Handle[testProbe]) (*structpb.Struct, error) {
			p, err := repo(c.t, h, c.items).Get(ctx, item)
			if err != nil {
				return nil, err
			}
			if _, err := repo(c.t, h, c.prices).Create(ctx, priceRow{ID: price, CostItemID: item}, p); err != nil {
				return nil, err
			}
			return structpb.NewStruct(map[string]any{"done": true})
		},
	}
	h, err := Serve(r.rt, spec)
	if err != nil {
		c.t.Fatal(err)
	}
	return h
}

func TestCreatedRowsOtherCall(t *testing.T) {
	t.Run("ST-SUP-RP10-LC59-T03 A row that another call created, whose tuples the relay has not written, is decided as before", func(t *testing.T) {
		r := newRevocationEnv(t, 5*time.Second, 0)
		c := newCostEnvOn(t, r.env, r.rt)
		c.exec(`INSERT INTO orgs (tenant_id, id, name) VALUES ('t1', 'o1', 'O1')`)
		org, _ := authz.NewResource(mustTenant(t, "t1"), "org", "o1")
		if _, err := r.port.write([]authz.Tuple{{Resource: org, Relation: "owner", Subject: authz.Subject{Type: "user", ID: "u-7"}}}); err != nil {
			t.Fatal(err)
		}
		cmd := c.addCostItem(true, []Witness{priceWitness}, nil, c.orgs, c.items, c.prices)
		b := r.bound("u-7@t1")
		req := r.request(b.Tenant.ID(), b.Principal, "k-lc59-t03-a", `{}`)
		req.Resource = "o1"
		first, err := r.runOn(r.rt, cmd, req, fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
			o, err := repo(t, h, c.orgs).Get(ctx, "o1")
			if err != nil {
				return err
			}
			_, err = repo(t, h, c.items).Create(ctx, costItem{ID: "p4", OrgID: "o1"}, o)
			return err
		}))
		if err != nil {
			t.Fatal(err)
		}
		location := first.Response.Header.Get(authz.RevocationLocationHeader)
		h := c.costServe(r, "p4", "h4")
		send := func(key string, after ...string) *httptest.ResponseRecorder {
			request := httptest.NewRequestWithContext(r.identity["u-7@t1"], http.MethodPost, "/prices", strings.NewReader(`{}`))
			request.Header.Set(idempotency.KeyHeader, `"`+key+`"`)
			for _, value := range after {
				request.Header.Add(authz.RevocationAfterHeader, value)
			}
			answer := httptest.NewRecorder()
			h.ServeHTTP(answer, request)
			return answer
		}
		if answer := send("k-lc59-t03-b"); answer.Code != http.StatusNotFound || c.count(`SELECT count(*) FROM price_histories WHERE id = 'h4'`) != 0 {
			t.Fatalf("the call on p4 with no header answered %d %s, want 404 from the provider", answer.Code, answer.Body)
		}
		go func() { time.Sleep(300 * time.Millisecond); startCostRelay(t, r.env, r.port) }()
		if answer := send("k-lc59-t03-c", location); answer.Code != http.StatusOK || c.count(`SELECT count(*) FROM price_histories WHERE id = 'h4'`) != 1 {
			t.Fatalf("the call on p4 with %s answered %d %s, want 200 after the wait", authz.RevocationAfterHeader, answer.Code, answer.Body)
		}
	})
}
