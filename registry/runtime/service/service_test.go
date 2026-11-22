package service

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"

	"github.com/jsell-rh/stego/registry/runtime/audit"
	"github.com/jsell-rh/stego/registry/runtime/audit/auditoutbox"
	"github.com/jsell-rh/stego/registry/runtime/authn"
	"github.com/jsell-rh/stego/registry/runtime/authn/authntest"
	"github.com/jsell-rh/stego/registry/runtime/authz"
	"github.com/jsell-rh/stego/registry/runtime/command"
	"github.com/jsell-rh/stego/registry/runtime/idempotency"
	"github.com/jsell-rh/stego/registry/runtime/internal/seal"
	"github.com/jsell-rh/stego/registry/runtime/internal/staging"
	"github.com/jsell-rh/stego/registry/runtime/manifest"
	"github.com/jsell-rh/stego/registry/runtime/modes"
	"github.com/jsell-rh/stego/registry/runtime/outbox"
	"github.com/jsell-rh/stego/registry/runtime/probe"
	"github.com/jsell-rh/stego/registry/runtime/query"
	"github.com/jsell-rh/stego/registry/runtime/tenant"
)

// tenantDecision is a decided Provision request, the one source of a tenant that a test may build.
type tenantDecision struct{ seal.DecisionMark }

func mustTenant(t *testing.T, id string) tenant.Tenant {
	t.Helper()
	tn, err := tenant.New(tenant.Requested(tenant.ProvisionAction, id, tenantDecision{DecisionMark: seal.NewDecisionMark(id, "provision")}))
	if err != nil {
		t.Fatal(err)
	}
	return tn
}

func mustPrincipal(t *testing.T, subject string) authz.Principal {
	t.Helper()
	p, err := authz.StagingPrincipal(staging.NewStaging(), subject)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

type order struct{ ID, Status string }

const orderColumns = "id, status, version, authz_token"

// orderEntity is the command entity of the orders table, as the generated code declares it.
func orderEntity(t *testing.T) *command.Entity[order] {
	t.Helper()
	selectRow := "SELECT " + orderColumns + " FROM public.orders WHERE tenant_id = $1 AND id = $2"
	e, err := command.NewEntity(command.EntitySpec[order]{
		Name: "Order", ResourceType: "order", ReadPermission: "view", ListPermission: "view",
		Select: selectRow, SelectForUpdate: selectRow + " FOR NO KEY UPDATE", SelectForDelete: selectRow + " FOR UPDATE",
		Insert: "INSERT INTO public.orders (tenant_id, id, status) VALUES ($1, $2, $3) RETURNING " + orderColumns,
		Update: "UPDATE public.orders SET status = $5, version = version + 1 WHERE tenant_id = $1 AND id = $2 AND version = $3 AND $4::text = id RETURNING " + orderColumns,
		Delete: "DELETE FROM public.orders WHERE tenant_id = $1 AND id = $2 AND version = $3",
		Scan: func(s command.Scanner) (order, command.Meta, error) {
			var v order
			var m command.Meta
			err := s.Scan(&v.ID, &v.Status, &m.Version, &m.Token)
			m.ID = v.ID
			return v, m, err
		},
		Values:     func(v order) []any { return []any{v.ID, v.Status} },
		State:      func(v order) map[string]any { return map[string]any{"id": v.ID, "status": v.Status} },
		KeyChanged: func(order, order) bool { return false },
		Clone:      func(v order) order { return v },
		ValidID:    func(id string) bool { return id != "" },
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// parts are the declared parts of the test service.
func parts(entities []command.Declared, calls *int) Parts {
	return Parts{
		Service: "todo-svc", DeclarationVersion: "d1", SubjectType: "user", TenantClaim: "tenant", ProblemCode: "TODO",
		TenantConfig: tenantConfig(calls), OwnerRole: fixtureOwner, WorkerRole: fixtureWorker, OutboxRelayRole: fixtureOutboxRelay, RelayRole: fixtureRelay, AuthzSchema: "public", PurgeRole: fixturePurge,
		NewQueue: func(db *sql.DB) (*outbox.Queue, error) {
			return outbox.New(db, outbox.Options{PayloadLimits: auditoutbox.PayloadLimits()})
		},
		AuditQueue: func(q *outbox.Queue) (audit.Enqueuer, error) { return auditoutbox.NewQueue(q) },
		NewAuthz: func(db *sql.DB, provider authz.Decision, q *outbox.Queue) (*authz.Runtime, error) {
			return authz.New(authz.Config{Provider: provider, DB: db, Schema: "public", Outbox: q})
		},
		AuditSettings: audit.Settings{Level: audit.LevelChange},
		// The grants of the runtime role that the migration job of the fixture gives (ST-SUP-RP14-LC06).
		Grants:   fixtureGrants,
		Commands: entities,
		// The anchor period of the audit DDL of the migration job of the fixture.
		Workers: WorkerParts{AnchorPeriod: time.Hour, RetentionMonths: 12},
	}
}

// startCaptured runs start with the default logger writing JSON into a buffer and standard output, which the
// console sink of the probe writes, into a pipe, and gives both texts.
func startCaptured(t *testing.T, start func()) (string, string) {
	t.Helper()
	var logs bytes.Buffer
	before := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(before)
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = write
	console := make(chan string, 1)
	go func() {
		text, _ := io.ReadAll(read)
		console <- string(text)
	}()
	func() {
		defer func() { os.Stdout = stdout; _ = write.Close() }()
		start()
	}()
	return logs.String(), <-console
}

// relate writes a relationship of the tenant t1 to SpiceDB: the subject holds relation on the resource key.
func relate(t *testing.T, endpoint, key, resourceType, resourceKey, relation, subject string) {
	t.Helper()
	conn, err := DialSpiceDB(SpiceDB{Endpoint: endpoint, Key: key, InsecureLoopback: true})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	id, err := authz.ResourceID(mustTenant(t, "t1"), resourceKey)
	if err != nil {
		t.Fatal(err)
	}
	subjectType, subjectID, _ := strings.Cut(subject, ":")
	if _, err := v1.NewPermissionsServiceClient(conn).WriteRelationships(context.Background(), &v1.WriteRelationshipsRequest{
		Updates: []*v1.RelationshipUpdate{{Operation: v1.RelationshipUpdate_OPERATION_TOUCH, Relationship: &v1.Relationship{
			Resource: &v1.ObjectReference{ObjectType: resourceType, ObjectId: id}, Relation: relation,
			Subject: &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: subjectType, ObjectId: subjectID}}}}}}); err != nil {
		t.Fatal(err)
	}
}

func TestNew(t *testing.T) {
	t.Run("ST-SUP-RP14-LC02-T01 New wires the runtimes of a service from its settings", func(t *testing.T) {
		endpoint, key := startSpiceDB(t)
		f := newFixture(t)
		spice := SpiceDB{Endpoint: endpoint, Key: key, InsecureLoopback: true}
		if err := Migrate(context.Background(), MigrationSettings{DatabaseURL: f.database, SpiceDB: spice}, f.migration(nil)); err != nil {
			t.Fatal(err)
		}
		relate(t, endpoint, key, "service", "todo-svc", "operator", "user:u-7")
		relate(t, endpoint, key, "order", "o1", "owner", "user:u-7")
		// The tenant t1 is in the tenant list, as its tenant key needs (ST-SUP-Q124 (a)).
		if _, err := f.admin.Exec(`INSERT INTO public.stego_tenants (tenant_id) VALUES ('t1'); INSERT INTO public.orders (tenant_id, id, status) VALUES ('t1', 'o1', 'open')`); err != nil {
			t.Fatal(err)
		}
		signing := authntest.NewKey(t, "k1")
		settings := Settings{DatabaseURL: f.runtimeURL(), Issuer: "https://issuer.example", Audience: "todo",
			KeysFile: writeFile(t, "keys.json", authntest.Document(signing.JWK())), SpiceDB: spice,
			CursorKey: []byte(strings.Repeat("k", 32)), ServiceVersion: "1.0.0"}
		orders := orderEntity(t)
		calls := 0
		svc, err := New(context.Background(), settings, parts([]command.Declared{orders}, &calls))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = svc.Close() })
		if !tenant.Opened(svc.pool) || !tenant.Opened(svc.auditDB) || svc.pool == svc.auditDB || calls != 1 {
			t.Fatalf("New opened its pools %v %v from %d adapter configurations, want two pools of tenant.Open from one", tenant.Opened(svc.pool), tenant.Opened(svc.auditDB), calls)
		}
		if svc.Verifier() == nil || svc.Commands() == nil || svc.Collections() != nil || svc.Queries() == nil || svc.Probes() == nil ||
			svc.Authz() == nil || svc.Writer() == nil || svc.Queue() == nil || svc.Telemetry() == nil {
			t.Fatal("New did not give each runtime")
		}

		// One command through the command runtime: decided on the service-level resource, written in the tenant.
		cmd, err := command.NewCommand(command.CommandSpec[struct{}]{Name: "create-order", Writes: []command.Declared{orders},
			Probe: func(*probe.Probe) struct{} { return struct{}{} }, Permission: "create_order",
			Resource: &command.Resource{Type: "service", Service: "todo-svc"}})
		if err != nil {
			t.Fatal(err)
		}
		create := func(subject, id string) error {
			fingerprint, err := idempotency.Fingerprint("POST", "/orders", []byte(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			_, err = command.Run(context.Background(), svc.Commands(), cmd, command.Request{Tenant: mustTenant(t, "t1"),
				Principal: mustPrincipal(t, subject), Key: "k-" + id, Fingerprint: fingerprint, Body: []byte(`{}`)},
				func([]byte) (struct{}, error) { return struct{}{}, nil },
				func(ctx context.Context, _ struct{}, h *command.Handle[struct{}]) (idempotency.Response, error) {
					repo, err := command.Repo(h, orders)
					if err != nil {
						return idempotency.Response{}, err
					}
					_, err = repo.Create(ctx, order{ID: id, Status: "new"}, nil)
					return idempotency.Response{Status: 201, Body: []byte(`{}`)}, err
				})
			return err
		}
		if err := create("user:u-7", "o2"); err != nil {
			t.Fatalf("the command gave %v", err)
		}
		var tenantOf string
		if err := f.admin.QueryRow(`SELECT tenant_id FROM public.orders WHERE id = 'o2'`).Scan(&tenantOf); err != nil || tenantOf != "t1" {
			t.Fatalf("the command wrote its row in the tenant %q: %v", tenantOf, err)
		}
		if err := create("user:u-9", "o3"); !errors.Is(err, command.ErrDenied) {
			t.Fatalf("the command of a user with no permission gave %v, want ErrDenied", err)
		}

		// One query through the query runtime: decided at SpiceDB in the tenant.
		entity, err := query.NewEntity(query.Spec{Name: "order", Table: "orders", Key: "id", Columns: []string{"id", "status"},
			ResourceType: "order", ReadPermission: "view", ListPermission: "view"},
			func(r query.Row) (order, error) {
				id, err := r.Text("id")
				if err != nil {
					return order{}, err
				}
				status, err := r.Text("status")
				return order{ID: id, Status: status}, err
			})
		if err != nil {
			t.Fatal(err)
		}
		q, err := query.NewQuery("get-order", entity)
		if err != nil {
			t.Fatal(err)
		}
		get := func(subject, id string) (order, error) {
			tn, p := mustTenant(t, "t1"), mustPrincipal(t, subject)
			b := query.Bound[struct{}]{Tenant: tn, Principal: p, Call: svc.Probes().Bind(context.Background(), tn.ID(), p.Subject().String(), q.Name()),
				Probe: func(*probe.Probe) struct{} { return struct{}{} }}
			return query.Run(context.Background(), svc.Queries(), q, b, id, func(ctx context.Context, id string, h *query.Handle[struct{}]) (order, error) {
				repo, err := query.Repo(h, entity)
				if err != nil {
					return order{}, err
				}
				o, err := repo.Get(ctx, id)
				if err != nil {
					return order{}, err
				}
				return o.Value(), nil
			})
		}
		if o, err := get("user:u-7", "o1"); err != nil || o.Status != "open" {
			t.Fatalf("the query gave %+v, %v", o, err)
		}
		if _, err := get("user:u-9", "o1"); !errors.Is(err, query.ErrNotFound) {
			t.Fatalf("the query of a user with no permission gave %v, want not found", err)
		}

		// New is refused when SpiceDB cannot be reached, and when a part is missing.
		unreachable := settings
		unreachable.SpiceDB.Endpoint = "127.0.0.1:1"
		if _, err := New(context.Background(), unreachable, parts([]command.Declared{orderEntity(t)}, nil)); err == nil || !strings.Contains(err.Error(), "SpiceDB") {
			t.Fatalf("New with an unreachable SpiceDB gave %v", err)
		}
		missing := parts(nil, nil)
		missing.TenantConfig = nil
		if _, err := New(context.Background(), settings, missing); err == nil {
			t.Fatal("New took parts with no connection configuration")
		}

		t.Run("ST-SUP-RP14-LC05-T01 The verifier of a declared issuer comes from its discovery document, and a refused start names its stage", func(t *testing.T) {
			provider := authntest.NewServer(t)
			es := authntest.NewECKey(t, "e1")
			provider.Publish(es)
			issuer := provider.Issuer("/realms/todo")
			oidc := settings
			oidc.KeysFile, oidc.Issuer, oidc.Discovery, oidc.CAFile = "", issuer, true, provider.CAFile(t)
			oidc.Algorithms, oidc.SubjectClaim = []string{authn.AlgorithmES256}, "oid"
			svc, err := New(context.Background(), oidc, parts(nil, nil))
			if err != nil {
				t.Fatalf("New with the discovery of the issuer gave %v", err)
			}
			defer func() { _ = svc.Close() }()
			claims := authntest.Claims(issuer, "todo", "user-1", time.Now())
			claims["oid"], claims["tenant"] = "object-7", "t1"
			id, err := svc.Verifier().Verify(context.Background(), authntest.Mint(t, authntest.HeaderOf("ES256", "e1"), claims, es))
			if err != nil || id.UserID() != "object-7" || provider.Discoveries() != 1 {
				t.Fatalf("the verifier gave %q, %v after %d discovery requests, want object-7 after one", id.UserID(), err, provider.Discoveries())
			}
			_, refused := svc.Verifier().Verify(context.Background(), authntest.Mint(t, authntest.Header("e1"), claims, authntest.NewKey(t, "e1")))
			if refused == nil {
				t.Fatal("the verifier with the declared algorithm ES256 accepted an RS256 header")
			}
			if StartStage(refused) != "" || StartStage(errors.New("other")) != "" {
				t.Fatal("StartStage named a stage for a failure that is not of the key source")
			}

			provider.SetDiscovery(authntest.Discovery("https://other.authn.test/realms/todo", provider.URL()))
			_, err = New(context.Background(), oidc, parts(nil, nil))
			if stage := StartStage(err); err == nil || stage != "service.start.authn.discovery" {
				t.Fatalf("New with a discovery document of another issuer gave %v at the stage %q", err, stage)
			}
			keyURL := settings
			keyURL.KeysFile, keyURL.KeysURL = "", provider.URL()
			_, err = New(context.Background(), keyURL, parts(nil, nil))
			if stage := StartStage(err); err == nil || stage != "service.start.authn.keys" {
				t.Fatalf("New with a key URL that the system roots do not trust gave %v at the stage %q", err, stage)
			}
		})

		t.Run("ST-SUP-RP14-LC06-T01 New starts on a database that does not hold the schema manifest, and logs and alerts each fault with its class and no row value", func(t *testing.T) {
			orders := manifest.Table{Schema: "public", Name: "orders", RowSecurity: true,
				Indexes: []manifest.Index{{Name: "orders_pkey", Columns: []string{"tenant_id", "id"}}},
				// The tenant key that the migration job made (ST-SUP-Q124 (a)).
				Constraints: ordersManifest().Tables[0].Constraints, TenantKey: ordersManifest().Tables[0].TenantKey,
				Columns: []manifest.Column{{Name: "tenant_id", Type: "text", NotNull: true}, {Name: "status", Type: "text", NotNull: true},
					{Name: "version", Type: "bigint", Default: "1"}, {Name: "authz_token", Type: "text", Default: "''::text"}},
				Policies: []manifest.Policy{{Name: "orders_tenant", Command: "*", Using: "(tenant_id = current_setting('app.tenant'::text, true))",
					WithCheck: "(tenant_id = current_setting('app.tenant'::text, true))"}}}
			declared := parts(nil, nil)
			declared.Schema = manifest.Manifest{Tables: []manifest.Table{orders}}
			// A row whose value no log line and no observation may hold.
			const rowValue = "row-value-f10-secret"
			if _, err := f.admin.Exec(`INSERT INTO public.orders (tenant_id, id, status) VALUES ('t1', 'o-f10', '` + rowValue + `')`); err != nil {
				t.Fatal(err)
			}
			// The database of the migration job holds the manifest, so New starts.
			svc, err := New(context.Background(), settings, declared)
			if err != nil {
				t.Fatalf("New refused the database that the migration job made: %v", err)
			}
			_ = svc.Close()
			// alerted makes the change, starts the service with the console of the probe and the log captured, and
			// expects it to start, with a log line that names the fault and its class and the observation
			// schema.manifest.mismatch of that class, neither with the row value (decision on finding F10, owner,
			// 2026-11-13).
			alerted := func(change, restore, fault, class string) {
				t.Helper()
				if _, err := f.admin.Exec(change); err != nil {
					t.Fatal(err)
				}
				defer func() {
					if _, err := f.admin.Exec(restore); err != nil {
						t.Fatal(err)
					}
				}()
				logs, console := startCaptured(t, func() {
					svc, err := New(context.Background(), settings, declared)
					if err != nil {
						t.Fatalf("New after %q gave %v, want a start with an alert", change, err)
					}
					if err := svc.Close(); err != nil {
						t.Fatal(err)
					}
				})
				logged := false
				for _, line := range strings.Split(logs, "\n") {
					var record map[string]any
					if json.Unmarshal([]byte(line), &record) == nil && record["level"] == "ERROR" && record["class"] == class &&
						strings.Contains(fmt.Sprint(record["fault"]), fault) {
						logged = true
					}
				}
				if !logged {
					t.Fatalf("after %q the log holds no error line with the class %s and the fault %q:\n%s", change, class, fault, logs)
				}
				alert := false
				for _, line := range strings.Split(console, "\n") {
					var record map[string]any
					if json.Unmarshal([]byte(line), &record) != nil || record["event.name"] != "schema.manifest.mismatch" {
						continue
					}
					fields, _ := record["fields"].(map[string]any)
					if record["severity"] == "ERROR" && fields["class"] == class {
						alert = true
					}
				}
				if !alert {
					t.Fatalf("after %q the console holds no ERROR schema.manifest.mismatch of the class %s:\n%s", change, class, console)
				}
				if strings.Contains(logs, rowValue) || strings.Contains(console, rowValue) {
					t.Fatalf("after %q the log or the console holds the row value:\n%s\n%s", change, logs, console)
				}
			}
			alerted("ALTER TABLE public.orders ALTER COLUMN status TYPE character varying(20)", "ALTER TABLE public.orders ALTER COLUMN status TYPE text",
				"table public.orders: column status has the type character varying(20)", "column")
			alerted("ALTER TABLE public.orders NO FORCE ROW LEVEL SECURITY", "ALTER TABLE public.orders FORCE ROW LEVEL SECURITY",
				"table public.orders: row-level security is not enabled and forced", "rls")
			// Each table of the manifest belongs to the owner role of the service.
			alerted("ALTER TABLE public.orders OWNER TO "+f.relay, "ALTER TABLE public.orders OWNER TO "+f.owner,
				"table public.orders: the table belongs to the role "+f.relay+", and the manifest needs "+f.owner, "owner")
			// The audit record is in the manifest of every service, with the triggers that make it append-only.
			alerted("DROP TRIGGER audit_event_reject_row ON public.audit_event",
				"CREATE TRIGGER audit_event_reject_row BEFORE UPDATE OR DELETE ON public.audit_event FOR EACH ROW EXECUTE FUNCTION public.audit_reject_change()",
				"table public.audit_event: trigger audit_event_reject_row is missing", "trigger")
			// audit_maintain, the SECURITY DEFINER function of the audit partitions, is in the manifest too, with the
			// source of the declared retention (#218 L5).
			alerted("ALTER FUNCTION public.audit_maintain() SECURITY INVOKER", "ALTER FUNCTION public.audit_maintain() SECURITY DEFINER",
				"function public.audit_maintain is not the function of the manifest", "function")
			// The outbox is in the manifest too, with the exact grants of its table.
			alerted("GRANT SELECT ON stego_outbox.messages TO PUBLIC", "REVOKE SELECT ON stego_outbox.messages FROM PUBLIC",
				"table stego_outbox.messages: the privilege SELECT to PUBLIC is granted, and the manifest does not grant it", "grant")
			// A schema named after the runtime role comes first on its default search path "$user", public, so a
			// table there of the name of a table of the manifest, which has no row-level security, would stand in
			// for it for a name with no schema (#243 S1).
			alerted("CREATE SCHEMA "+f.runtime+"; GRANT USAGE ON SCHEMA "+f.runtime+" TO "+f.runtime+"; CREATE TABLE "+f.runtime+".orders (tenant_id text, id uuid)", "DROP SCHEMA "+f.runtime+" CASCADE",
				"the relation "+f.runtime+".orders comes before public.orders on the search path of the session", "owner")
			// A default that the manifest does not state is refused: an INSERT that leaves the column out takes it.
			alerted("ALTER TABLE public.orders ALTER COLUMN status SET DEFAULT 'paid'", "ALTER TABLE public.orders ALTER COLUMN status DROP DEFAULT",
				"table public.orders: column status has a default that the manifest does not state", "column")
			// The key table is in the manifest too: the runtime role may not delete a key row of another tenant's key.
			alerted("GRANT DELETE ON public.idempotency_keys TO "+f.runtime, "REVOKE DELETE ON public.idempotency_keys FROM "+f.runtime,
				"table public.idempotency_keys: the privilege DELETE to "+f.runtime+" is granted, and the manifest does not grant it", "grant")
			// Only the runtime role may run it: EXECUTE for PUBLIC is refused (#239 L2).
			alerted("GRANT EXECUTE ON FUNCTION public.audit_maintain() TO PUBLIC", "REVOKE EXECUTE ON FUNCTION public.audit_maintain() FROM PUBLIC",
				"function public.audit_maintain may be run by roles that the manifest does not name", "grant")
			// A policy that is not the policy of the manifest alerts with the class policy.
			alerted("ALTER POLICY orders_tenant ON public.orders USING (true)",
				"ALTER POLICY orders_tenant ON public.orders USING (tenant_id = current_setting('app.tenant', true))",
				"table public.orders: policy orders_tenant is not the policy of the manifest", "policy")
			// A check that cannot run is still a refusal: parts with no anchor period give no manifest of the audit
			// record.
			broken := declared
			broken.Workers.AnchorPeriod = 0
			if _, err := New(context.Background(), unreachable, broken); err == nil || !strings.Contains(err.Error(), "the manifest of the audit record") || strings.Contains(err.Error(), "SpiceDB") {
				t.Fatalf("New with parts that give no manifest gave %v, want a refusal of the manifest check before SpiceDB", err)
			}
			// A function of the name of a relay function in public, a schema of the path that the relay function's own
			// schema stego_outbox is not on, stands in for no call, as each call names stego_outbox (#245 Q3).
			if _, err := f.admin.Exec("CREATE FUNCTION public.claim() RETURNS integer LANGUAGE sql AS 'SELECT 1'"); err != nil {
				t.Fatal(err)
			}
			if svc, err := New(context.Background(), settings, declared); err != nil {
				t.Fatalf("New refused the restored database with an unrelated public.claim(): %v", err)
			} else {
				_ = svc.Close()
			}
			if _, err := f.admin.Exec("DROP FUNCTION public.claim()"); err != nil {
				t.Fatal(err)
			}
		})

		t.Run("ST-SUP-RP14-LC06-T04 A start on a database that lacks a column of the manifest is not ready and names the column, and a droppable column is ready", func(t *testing.T) {
			ctx := context.Background()
			// start starts the service with a manifest whose orders table needs the column gone, which the database
			// does not hold, and gives the log and the readiness of its api mode; blocked is what Blocked gives.
			var blocked error
			start := func(droppable bool) (string, error) {
				t.Helper()
				declared := parts(nil, nil)
				declared.Schema = ordersManifest()
				declared.Schema.Tables[0].Columns = append(declared.Schema.Tables[0].Columns, manifest.Column{Name: "gone", Type: "text", Droppable: droppable})
				var ready error
				blocked = nil
				logs, _ := startCaptured(t, func() {
					svc, err := New(ctx, settings, declared)
					if err != nil {
						t.Fatalf("New gave %v, want a start", err)
					}
					defer func() { _ = svc.Close() }()
					api, err := svc.Start(ctx, StartConfig{Selection: modes.Selection{modes.API}})
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = api.Shutdown(ctx) }()
					ready = api.Readiness().Ready(ctx, modes.API)
					// The generated readiness of a process of the api mode alone, which starts no worker, reads it.
					blocked = svc.Blocked(modes.Selection{modes.API})
				})
				return logs, ready
			}
			const fault = "table public.orders: column gone is missing"
			logs, ready := start(false)
			// The readiness answers network callers, so it names no table or column; the log does (security review of
			// PR #457, L3).
			if !errors.Is(ready, modes.ErrNotReady) || strings.Contains(ready.Error(), "gone") || !strings.Contains(ready.Error(), "roll forward") {
				t.Fatalf("the api mode gave %v, want not ready with the remedy and no column", ready)
			}
			if !errors.Is(blocked, modes.ErrNotReady) || !strings.Contains(blocked.Error(), "api: not ready") || strings.Contains(blocked.Error(), "gone") {
				t.Fatalf("Blocked of the api mode gave %v, want not ready and no column", blocked)
			}
			if !strings.Contains(logs, "the service starts not ready") || !strings.Contains(logs, fault) {
				t.Fatalf("the log does not say that the service starts not ready:\n%s", logs)
			}
			// A droppable column alerts and blocks nothing: the api mode waits only for its HTTP server.
			logs, ready = start(true)
			if ready == nil || strings.Contains(ready.Error(), fault) || blocked != nil || !strings.Contains(logs, fault) || strings.Contains(logs, "starts not ready") {
				t.Fatalf("with the column droppable, the api mode gave %v and the log %s, want the alert and no block", ready, logs)
			}
		})

		t.Run("ST-SUP-RP09-LC05-T08 A process of the api mode alone is not ready while its database does not answer, with a fixed reason", func(t *testing.T) {
			ctx := context.Background()
			declared := parts(nil, nil)
			declared.Schema = ordersManifest()
			svc, err := New(ctx, settings, declared)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = svc.Close() }()
			if err := svc.Ping(ctx); err != nil {
				t.Fatalf("Ping with the database up gave %v, want ready", err)
			}
			// The pool of the process no longer answers, as when the database is down.
			_ = svc.pool.Close()
			var got error
			logs, _ := startCaptured(t, func() { got = svc.Ping(ctx) })
			if !errors.Is(got, modes.ErrNotReady) || !strings.Contains(got.Error(), "api: not ready: "+modes.DatabaseUnreachable) {
				t.Fatalf("Ping with the pool closed gave %v, want the fixed reason", got)
			}
			// The reason holds no text of the error, which can name the host, the user or the database.
			if strings.Contains(got.Error(), "closed") || strings.Contains(got.Error(), f.runtime) || !strings.Contains(logs, "closed") {
				t.Fatalf("the reason %q or the log %q is not as wanted: the error belongs in the log only", got, logs)
			}
		})

		t.Run("ST-SUP-RP14-LC06-T03 The start check holds the grants of the entity tables and the audit tables, and New refuses parts with no runtime grants", func(t *testing.T) {
			// The table of a declared entity with the authorization token, as the PostgreSQL adapter gives it.
			orders := manifest.Table{Schema: "public", Name: "orders", RowSecurity: true, Entity: true,
				Indexes:     []manifest.Index{{Name: "orders_pkey", Columns: []string{"tenant_id", "id"}}},
				Constraints: ordersManifest().Tables[0].Constraints, TenantKey: ordersManifest().Tables[0].TenantKey,
				Columns: []manifest.Column{{Name: "tenant_id", Type: "text", NotNull: true}, {Name: "status", Type: "text", NotNull: true},
					{Name: "version", Type: "bigint", Default: "1"}, {Name: "authz_token", Type: "text", Default: "''::text"}},
				Policies: []manifest.Policy{{Name: "orders_tenant", Command: "*", Using: "(tenant_id = current_setting('app.tenant'::text, true))",
					WithCheck: "(tenant_id = current_setting('app.tenant'::text, true))"}}}
			declared := parts(nil, nil)
			declared.Schema = manifest.Manifest{Tables: []manifest.Table{orders}}
			exec := func(statement string) {
				t.Helper()
				if _, err := f.admin.Exec(statement); err != nil {
					t.Fatalf("%s: %v", statement, err)
				}
			}
			// grantFaults starts the service and gives the faults of the class grant that it logs: a fault alerts, and
			// the service starts (decision on finding F10, owner, 2026-11-13).
			grantFaults := func() []string {
				t.Helper()
				logs, _ := startCaptured(t, func() {
					svc, err := New(context.Background(), settings, declared)
					if err != nil {
						t.Fatalf("New refused a database whose grants do not hold the manifest: %v", err)
					}
					_ = svc.Close()
				})
				var faults []string
				for _, line := range strings.Split(logs, "\n") {
					var record map[string]any
					if json.Unmarshal([]byte(line), &record) == nil && record["level"] == "ERROR" && record["class"] == "grant" {
						faults = append(faults, fmt.Sprint(record["fault"]))
					}
				}
				return faults
			}
			// The authorization migration of a generated service gives the worker its grants on the token.
			exec("GRANT SELECT (tenant_id, id), UPDATE (authz_token) ON public.orders TO " + f.worker)
			defer exec("REVOKE SELECT (tenant_id, id), UPDATE (authz_token) ON public.orders FROM " + f.worker)
			if got := grantFaults(); len(got) != 0 {
				t.Fatalf("the database that the migration job made has the grant faults %v, want none", got)
			}
			for _, c := range []struct{ change, restore, fault string }{
				{"GRANT TRUNCATE ON public.orders TO " + f.runtime, "REVOKE TRUNCATE ON public.orders FROM " + f.runtime,
					"table public.orders: the privilege TRUNCATE to " + f.runtime + " is granted, and the manifest does not grant it"},
				{"REVOKE DELETE ON public.orders FROM " + f.runtime, "GRANT DELETE ON public.orders TO " + f.runtime,
					"table public.orders: the privilege DELETE to " + f.runtime + " of the manifest is not granted"},
				{"REVOKE UPDATE (authz_token) ON public.orders FROM " + f.worker, "GRANT UPDATE (authz_token) ON public.orders TO " + f.worker,
					"table public.orders: the privilege UPDATE (authz_token) to " + f.worker + " of the manifest is not granted"},
				// The worker gets no grant on the audit record when the service has no consuming worker (ST-SUP-Q141).
				{"GRANT INSERT ON public.audit_event TO " + f.worker, "REVOKE INSERT ON public.audit_event FROM " + f.worker,
					"table public.audit_event: the privilege INSERT to " + f.worker + " is granted, and the manifest does not grant it"},
				{"REVOKE UPDATE ON public.audit_chain_heads FROM " + f.runtime, "GRANT UPDATE ON public.audit_chain_heads TO " + f.runtime,
					"table public.audit_chain_heads: the privilege UPDATE to " + f.runtime + " of the manifest is not granted"},
				{"GRANT SELECT ON public.audit_retention_checkpoints TO PUBLIC", "REVOKE SELECT ON public.audit_retention_checkpoints FROM PUBLIC",
					"table public.audit_retention_checkpoints: the privilege SELECT to PUBLIC is granted, and the manifest does not grant it"},
			} {
				exec(c.change)
				got := grantFaults()
				exec(c.restore)
				if !slices.Contains(got, c.fault) {
					t.Errorf("after %q the start logged the grant faults %v, want %q", c.change, got, c.fault)
				}
			}
			// Parts with no GRANT statement of the runtime role, or with one that is not of the generated form, are
			// refused before the database is read, with an error that names Parts.Grants.
			none := declared
			none.Grants = nil
			if _, err := New(context.Background(), settings, none); err == nil || !strings.Contains(err.Error(), "Parts.Grants") {
				t.Errorf("New with no runtime grants gave %v, want a refusal that names Parts.Grants", err)
			}
			odd := declared
			odd.Grants = []string{`GRANT ALL ON TABLE public.orders`}
			if _, err := New(context.Background(), settings, odd); err == nil || !strings.Contains(err.Error(), "Parts.Grants") {
				t.Errorf("New with a runtime grant of another form gave %v, want a refusal that names Parts.Grants", err)
			}
		})
		t.Run("ST-SUP-RP14-LC06-T02 The start check gives the class of a fault of a partition and of a member of a confined role, and logs at most 50 faults", func(t *testing.T) {
			declared := parts(nil, nil)
			declared.Schema = manifest.Manifest{Tables: []manifest.Table{{Schema: "public", Name: "orders", RowSecurity: true,
				Indexes: []manifest.Index{{Name: "orders_pkey", Columns: []string{"tenant_id", "id"}}},
				// The tenant key that the migration job made (ST-SUP-Q124 (a)).
				Constraints: ordersManifest().Tables[0].Constraints, TenantKey: ordersManifest().Tables[0].TenantKey,
				Columns: []manifest.Column{{Name: "tenant_id", Type: "text", NotNull: true}, {Name: "status", Type: "text", NotNull: true},
					{Name: "version", Type: "bigint", Default: "1"}, {Name: "authz_token", Type: "text", Default: "''::text"}},
				Policies: []manifest.Policy{{Name: "orders_tenant", Command: "*", Using: "(tenant_id = current_setting('app.tenant'::text, true))",
					WithCheck: "(tenant_id = current_setting('app.tenant'::text, true))"}}}}}
			rt, err := sql.Open("pgx", f.runtimeURL())
			if err != nil {
				t.Fatal(err)
			}
			defer rt.Close()
			var partition string
			if err := f.admin.QueryRow("SELECT inhrelid::regclass::text FROM pg_inherits WHERE inhparent = 'public.audit_event'::regclass ORDER BY 1 LIMIT 1").Scan(&partition); err != nil {
				t.Fatal(err)
			}
			// classes makes the change, runs the start check with the log captured, and gives the classes and the error
			// lines of the log.
			classes := func(change, restore string) ([]string, []map[string]any) {
				t.Helper()
				if _, err := f.admin.Exec(change); err != nil {
					t.Fatal(err)
				}
				defer func() {
					if _, err := f.admin.Exec(restore); err != nil {
						t.Fatal(err)
					}
				}()
				var got []string
				logs, _ := startCaptured(t, func() {
					if got, _, err = checkSchema(context.Background(), rt, declared); err != nil {
						t.Fatalf("the check after %q gave %v, want an alert", change, err)
					}
				})
				var lines []map[string]any
				for _, line := range strings.Split(logs, "\n") {
					var record map[string]any
					if json.Unmarshal([]byte(line), &record) == nil && record["level"] == "ERROR" {
						lines = append(lines, record)
					}
				}
				return got, lines
			}
			// Review of PR #305, M3: each of these faults had the class other, which an operator does not page on.
			for _, c := range []struct{ what, change, restore, class string }{
				{"a trigger on an audit partition",
					"CREATE FUNCTION public.lc06t02() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RETURN NEW; END$$; ALTER FUNCTION public.lc06t02() OWNER TO " + f.owner +
						"; CREATE TRIGGER lc06t02 BEFORE INSERT ON " + partition + " FOR EACH ROW EXECUTE FUNCTION public.lc06t02()",
					"DROP TRIGGER lc06t02 ON " + partition + "; DROP FUNCTION public.lc06t02()", "trigger"},
				{"a grant on an audit partition", "GRANT UPDATE ON " + partition + " TO " + f.runtime, "REVOKE UPDATE ON " + partition + " FROM " + f.runtime, "grant"},
				{"a rule on an audit partition", "CREATE RULE lc06t02 AS ON UPDATE TO " + partition + " DO INSTEAD NOTHING", "DROP RULE lc06t02 ON " + partition, "rule"},
				{"the runtime role a member of the outbox relay role", "GRANT " + f.outboxRelay + " TO " + f.runtime, "REVOKE " + f.outboxRelay + " FROM " + f.runtime, "owner"},
			} {
				got, lines := classes(c.change, c.restore)
				if !slices.Equal(got, []string{c.class}) || len(lines) != 1 || lines[0]["class"] != c.class {
					t.Errorf("%s: the classes %v and the log %v, want the class %s once", c.what, got, lines, c.class)
				}
			}
			// Review of PR #305, L2: 2000 extra policies gave 2000 log lines at each start.
			var flood, unflood strings.Builder
			for i := range MaxLoggedFaults + 10 {
				fmt.Fprintf(&flood, "CREATE POLICY lc06t02_%03d ON public.orders AS RESTRICTIVE USING (true);", i)
				fmt.Fprintf(&unflood, "DROP POLICY lc06t02_%03d ON public.orders;", i)
			}
			got, lines := classes(flood.String(), unflood.String())
			if !slices.Equal(got, []string{"policy"}) || len(lines) != MaxLoggedFaults+1 {
				t.Fatalf("%d faults gave the classes %v and %d error lines, want the class policy and %d lines", MaxLoggedFaults+10, got, len(lines), MaxLoggedFaults+1)
			}
			if last := lines[MaxLoggedFaults]; fmt.Sprint(last["faults"]) != "10" || last["fault"] != nil {
				t.Fatalf("the last line is %v, want the count of the 10 faults that are not logged", last)
			}
		})

		t.Run("ST-SUP-RC01-OC21-T04 The start check refuses a runtime role that is the owner role or a member of it", func(t *testing.T) {
			rt, err := sql.Open("pgx", f.runtimeURL())
			if err != nil {
				t.Fatal(err)
			}
			defer rt.Close()
			ctx := context.Background()
			declared := parts(nil, nil)
			if _, _, err := checkSchema(ctx, rt, declared); err != nil {
				t.Fatalf("the start check as a runtime role that is not the owner role gave %v", err)
			}
			refused := func(what string, p Parts) {
				t.Helper()
				if _, _, err := checkSchema(ctx, rt, p); err == nil || !strings.Contains(err.Error(), "is the owner role "+p.OwnerRole+" or a member of it") {
					t.Errorf("the start check with %s gave %v, want a refusal of the runtime role", what, err)
				}
			}
			same := declared
			same.OwnerRole = f.runtime
			refused("the runtime role as the owner role", same)
			// A membership of the owner role, direct or through a role between them, with or without its rights.
			between := f.runtime + "_between"
			for _, c := range []struct{ what, grant, revoke string }{
				{"a membership with no inherit and no set", "GRANT " + f.owner + " TO " + f.runtime + " WITH INHERIT FALSE, SET FALSE", "REVOKE " + f.owner + " FROM " + f.runtime},
				{"a membership with inherit", "GRANT " + f.owner + " TO " + f.runtime + " WITH INHERIT TRUE", "REVOKE " + f.owner + " FROM " + f.runtime},
				{"a membership through a role between them", "CREATE ROLE " + between + " NOLOGIN; GRANT " + f.owner + " TO " + between + "; GRANT " + between + " TO " + f.runtime,
					"REVOKE " + between + " FROM " + f.runtime + "; REVOKE " + f.owner + " FROM " + between + "; DROP ROLE " + between},
			} {
				if _, err := f.admin.Exec(c.grant); err != nil {
					t.Fatal(err)
				}
				refused(c.what, declared)
				if _, err := f.admin.Exec(c.revoke); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := checkSchema(ctx, rt, declared); err != nil {
				t.Fatalf("the start check after the memberships are revoked gave %v", err)
			}
		})
	})
}

func TestStartRelease(t *testing.T) {
	t.Run("ST-SUP-RP14-LC07-T01 New refuses a database whose migration ledger does not hold the last migration of the release, until the migration job ran", func(t *testing.T) {
		endpoint, key := startSpiceDB(t)
		f := newFixture(t)
		spice := SpiceDB{Endpoint: endpoint, Key: key, InsecureLoopback: true}
		ctx := context.Background()
		signing := authntest.NewKey(t, "k1")
		settings := Settings{DatabaseURL: f.runtimeURL(), Issuer: "https://issuer.example", Audience: "todo",
			KeysFile: writeFile(t, "keys.json", authntest.Document(signing.JWK())), SpiceDB: spice,
			CursorKey: []byte(strings.Repeat("k", 32)), ServiceVersion: "1.0.0"}
		declared := parts(nil, nil)
		declared.Release = "002_release"
		// start runs New with the log captured, and gives the refusal and the log.
		start := func() (string, error) {
			var err error
			logs, _ := startCaptured(t, func() {
				var svc *Service
				if svc, err = New(ctx, settings, declared); err == nil {
					_ = svc.Close()
				}
			})
			return logs, err
		}
		refused := func(what, state string) {
			t.Helper()
			logs, err := start()
			if !errors.Is(err, ErrMigrationPending) || StartStage(err) != "service.start.migration" {
				t.Fatalf("%s: New gave %v at the stage %q, want the refusal of a pending migration at service.start.migration", what, err, StartStage(err))
			}
			for _, text := range []string{err.Error(), logs} {
				for _, want := range []string{LedgerTable, state, "the argument migrate", "todo-svc-migrate-<release>", "wait until it completes"} {
					if !strings.Contains(text, want) {
						t.Errorf("%s: the refusal does not say %q:\n%s", what, want, text)
					}
				}
			}
			if !strings.Contains(logs, `"migration":"002_release"`) || strings.Contains(logs, rolePassword) {
				t.Errorf("%s: the log line does not name the migration, or holds the password:\n%s", what, logs)
			}
		}
		// A database on which no migration job ran.
		refused("a database with no ledger", "is not in the database")

		// A catalog that cannot be read is an error of the check, not a pending migration (#373 security review L5).
		closed, err := sql.Open("pgx", f.runtimeURL())
		if err != nil {
			t.Fatal(err)
		}
		_ = closed.Close()
		if err := checkRelease(ctx, closed, "todo-svc", "002_release", nil, nil); err == nil || errors.Is(err, ErrMigrationPending) ||
			!strings.Contains(err.Error(), "read the migration ledger "+LedgerTable+" in the catalog") {
			t.Errorf("the check on a closed pool gave %v, want an error of the catalog read that is not ErrMigrationPending", err)
		}

		// Parts with schema steps and no release fail closed before the database opens (#373 security review L3).
		stepped := parts(nil, nil)
		stepped.SchemaSteps = []string{"orders-status-rename"}
		if _, err := New(ctx, settings, stepped); err == nil || !strings.Contains(err.Error(), "no release") || !strings.Contains(err.Error(), LedgerTable) {
			t.Errorf("New with schema steps and no release gave %v, want a refusal that names the release and the ledger", err)
		}

		// The migration job of a release before this one: it makes the ledger, as the adapter does, with its own last
		// migration, and grants nothing on it.
		ledger := func(versions ...string) func(context.Context, *sql.Conn) error {
			return func(ctx context.Context, conn *sql.Conn) error {
				if _, err := conn.ExecContext(ctx, ordersDDL+`;
CREATE SCHEMA IF NOT EXISTS stego_schema;
CREATE TABLE IF NOT EXISTS stego_schema.migrations (version text PRIMARY KEY, digest text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
					return err
				}
				for _, version := range versions {
					if _, err := conn.ExecContext(ctx, "INSERT INTO stego_schema.migrations (version, digest) VALUES ($1, 'd') ON CONFLICT DO NOTHING", version); err != nil {
						return err
					}
				}
				return nil
			}
		}
		job := MigrationSettings{DatabaseURL: f.database, SpiceDB: spice}
		older := f.migration(nil)
		older.Ledger = ledger("001_initial")
		if err := Migrate(ctx, job, older); err != nil {
			t.Fatal(err)
		}
		refused("a ledger that the runtime role cannot read", "is not readable by the runtime role")

		// The same job with the grant of this release on the ledger, which the adapter's database access gives.
		older.Grants = append(older.Grants, `GRANT USAGE ON SCHEMA "stego_schema"`, `GRANT SELECT ON TABLE "stego_schema"."migrations"`)
		if err := Migrate(ctx, job, older); err != nil {
			t.Fatal(err)
		}
		refused("a ledger without the migration of the release", "does not hold the migration 002_release")

		// The migration job of this release: the start passes the check.
		current := f.migration(nil)
		current.Ledger = ledger("001_initial", "002_release")
		current.Grants = older.Grants
		if err := Migrate(ctx, job, current); err != nil {
			t.Fatal(err)
		}
		if logs, err := start(); err != nil {
			t.Fatalf("New after the migration job of the release gave %v:\n%s", err, logs)
		}

		t.Run("ST-SUP-RP14-LC13-T01 New refuses a database whose ledger holds another body of a migration of the release", func(t *testing.T) {
			// The ledger rows of the job above hold the digest d.
			declared.Migrations = map[string]string{"001_initial": "d", "002_release": "d"}
			if logs, err := start(); err != nil {
				t.Fatalf("New with the digests of the ledger gave %v:\n%s", err, logs)
			}
			// A ledger row of a name that the release does not give, such as a step of a later release, passes.
			declared.Migrations = map[string]string{"001_initial": "d"}
			if logs, err := start(); err != nil {
				t.Fatalf("New with a ledger row that the release does not give gave %v:\n%s", err, logs)
			}
			// A migration of the release that the ledger has no row of is not applied (security review of PR 448, I1).
			declared.Migrations = map[string]string{"001_initial": "d", "050_orders_step": "d", "002_release": "d"}
			if logs, err := start(); !errors.Is(err, ErrMigrationPending) || StartStage(err) != "service.start.migration" ||
				!strings.Contains(err.Error(), "no row of the migration 050_orders_step") || !strings.Contains(logs, `"migration":"050_orders_step"`) {
				t.Fatalf("New with a migration of the release that the ledger has no row of gave %v at the stage %q, want ErrMigrationPending at service.start.migration that names 050_orders_step:\n%s", err, StartStage(err), logs)
			}
			const body = "CREATE TABLE secret_body_text (id int)"
			declared.Migrations = map[string]string{"001_initial": "e3b0c4", "002_release": "d"}
			logs, err := start()
			if !errors.Is(err, ErrMigrationChanged) || StartStage(err) != "service.start.migration" {
				t.Fatalf("New with another digest of 001_initial gave %v at the stage %q, want ErrMigrationChanged at service.start.migration", err, StartStage(err))
			}
			for _, text := range []string{err.Error(), logs} {
				for _, want := range []string{"001_initial", "holds the digest", "e3b0c4", "never changes", "new schema step", "nothing repairs the digest"} {
					if !strings.Contains(text, want) {
						t.Errorf("the refusal does not say %q:\n%s", want, text)
					}
				}
				if strings.Contains(text, body) || strings.Contains(text, rolePassword) {
					t.Errorf("the refusal holds a body or the password:\n%s", text)
				}
			}
			if !strings.Contains(logs, `"migration":"001_initial"`) {
				t.Errorf("the log line does not name the migration:\n%s", logs)
			}
			var digest string
			if err := f.admin.QueryRow("SELECT digest FROM stego_schema.migrations WHERE version = '001_initial'").Scan(&digest); err != nil || digest != "d" {
				t.Fatalf("the refused start changed the ledger row to %q (%v)", digest, err)
			}
		})
		t.Run("ST-SUP-RP14-LC13-T02 New accepts a ledger row that holds an earlier digest of a migration of the release, and no other digest", func(t *testing.T) {
			defer func() { declared.Migrations, declared.EarlierMigrations = nil, nil }()
			// The ledger rows hold the digest d, which an earlier version of the adapter gave the body of 002_release.
			declared.Migrations = map[string]string{"001_initial": "d", "002_release": "e3b0c4"}
			declared.EarlierMigrations = map[string][]string{"002_release": {"c0ffee", "d"}}
			if logs, err := start(); err != nil {
				t.Fatalf("New with an earlier digest of 002_release in the ledger gave %v:\n%s", err, logs)
			}
			// An earlier digest of another migration does not count.
			declared.EarlierMigrations = map[string][]string{"001_initial": {"d"}}
			if _, err := start(); !errors.Is(err, ErrMigrationChanged) || !strings.Contains(err.Error(), "002_release") {
				t.Fatalf("New with the earlier digest of another migration gave %v, want ErrMigrationChanged that names 002_release", err)
			}
		})
	})
}

// TestKeySourceFaults proves the fault class, the object and the remedy of a key source that the key set refuses at
// start, which the failure record of the process names (walkthrough 23, small items).
func TestKeySourceFaults(t *testing.T) {
	t.Run("ST-SUP-RP14-LC05-T06 A key source that the key set refuses names its fault class, its object and a remedy, with no path and no URL", func(t *testing.T) {
		dir := t.TempDir()
		write := func(name, text string) string {
			path := dir + "/" + name
			if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}
		untrusted := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{}")) }))
		defer untrusted.Close()
		for _, c := range []struct {
			name   string
			source authn.KeySource
			class  string
			object string
			stage  string
		}{
			{"a key file that is not there", authn.KeySource{File: dir + "/secret-keys.json"}, authn.KeysFileFault, "key_document_file", "service.start.authn.keys"},
			{"a key file that is not a key set", authn.KeySource{File: write("keys.json", "secret-not-json")}, authn.KeysDocumentFault, "key_document", "service.start.authn.keys"},
			{"a PEM file with no key", authn.KeySource{PEMFile: write("key.pem", "secret-not-pem")}, authn.KeysPEMFault, "key_pem_file", "service.start.authn.keys"},
			{"a key URL that the system roots do not trust", authn.KeySource{URL: untrusted.URL + "/keys"}, authn.KeysLoadFault, "key_document_url", "service.start.authn.keys"},
			{"an issuer whose discovery document cannot be loaded", authn.KeySource{Issuer: untrusted.URL + "/realms/secret"}, authn.KeysDiscoveryFault, "discovery_document", "service.start.authn.discovery"},
		} {
			_, err := authn.NewKeySet(context.Background(), c.source)
			var named interface {
				FaultClass() string
				FaultObject() string
				FaultRemedy() string
			}
			if !errors.As(err, &named) {
				t.Errorf("%s gave %v, which names no fault", c.name, err)
				continue
			}
			if named.FaultClass() != c.class || named.FaultObject() != c.object || named.FaultRemedy() == "" || StartStage(err) != c.stage {
				t.Errorf("%s gave the fault %q, the object %q and the stage %q, want %q, %q and %q with a remedy", c.name,
					named.FaultClass(), named.FaultObject(), StartStage(err), c.class, c.object, c.stage)
			}
			if remedy := named.FaultRemedy(); strings.Contains(remedy, "secret") || strings.Contains(remedy, dir) || strings.Contains(remedy, "127.0.0.1") {
				t.Errorf("%s gave a remedy with a path, a URL or a value: %q", c.name, remedy)
			}
		}
	})
}
