package query

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/910-software-solutions/stego/registry/runtime/audit"
	"github.com/910-software-solutions/stego/registry/runtime/authz"
)

func TestReadAuditCode(t *testing.T) {
	t.Run("ST-SUP-RP11-LC16-T01 A failed read is audited with its code", func(t *testing.T) {
		e := newEnv(t)
		decision := &fakeDecision{decide: allowAll}
		rt := e.runtimeOf(t, decision)
		charges := chargeEntity(t)
		e.insert(t, "INSERT INTO app.charges (tenant_id, id, card_last4, amount) VALUES ('t1', 'ch-1', '4242', 100), ('t1', 'ch-2', '#18#', 200)")
		q := mustQuery(t, "read-charge", charges)
		read := func(id string) error {
			_, err := Run(context.Background(), rt, q, bound(t, "t1", "user:u-7", q.Name()), id,
				func(ctx context.Context, id string, h *Handle[noProbe]) (int64, error) {
					repo, err := Repo(h, charges)
					if err != nil {
						return 0, err
					}
					c, err := repo.Get(ctx, id)
					if err != nil {
						return 0, err
					}
					return c.Value().Amount, nil
				})
			return err
		}
		if err := read("ch-1"); err != nil {
			t.Fatalf("the read that succeeds gave %v", err)
		}
		decision.mu.Lock()
		decision.down = true
		decision.mu.Unlock()
		if err := read("ch-2"); !errors.Is(err, ErrUndecided) {
			t.Fatalf("the read whose decision fails gave %v, want ErrUndecided", err)
		}
		rows, err := e.admin.QueryContext(context.Background(), "SELECT resource_id, outcome, error_code, coalesce(after::text, '') FROM "+
			audit.EventsTable+" WHERE action = 'read' ORDER BY seq")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		type row struct{ id, outcome, code, state string }
		var got []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.outcome, &r.code, &r.state); err != nil {
				t.Fatal(err)
			}
			got = append(got, r)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("the reads wrote %d read rows, want two: %+v", len(got), got)
		}
		if got[0].id != "ch-1" || got[0].outcome != "ok" || got[0].code != "" {
			t.Fatalf("the read that succeeded has the row %+v, want outcome ok and no error code", got[0])
		}
		if got[1].id != "ch-2" || got[1].outcome != "failed" || got[1].code != audit.ErrorCodeUndecided {
			t.Fatalf("the read whose decision failed has the row %+v, want outcome failed and the code %s", got[1], audit.ErrorCodeUndecided)
		}
		if got[1].state != "" && got[1].state != "null" {
			t.Fatalf("the failed read row holds the state %s, want none", got[1].state)
		}
		// The card value holds #, which no hex, base64 or UUID text of a random row value can hold, so only a leak of
		// the value matches.
		if n := e.count(t, "SELECT count(*) FROM "+audit.EventsTable+" WHERE row_to_json("+audit.EventsTable+")::text LIKE '%down%' OR row_to_json("+audit.EventsTable+")::text LIKE '%#18#%'"); n != 0 {
			t.Fatalf("%d audit rows hold the error text or the row data", n)
		}
		// A list page whose batch decision fails writes a failed read row for each row of personal data it read.
		lq := mustListQuery(t, "list-charges", charges, "view")
		if _, err := Run(context.Background(), rt, lq, bound(t, "t1", "user:u-7", lq.Name()), struct{}{},
			func(ctx context.Context, _ struct{}, h *Handle[noProbe]) (int, error) {
				repo, err := Repo(h, charges)
				if err != nil {
					return 0, err
				}
				page, err := repo.List(ctx, ListRequest{})
				return len(page.Items), err
			}); err == nil {
			t.Fatal("the list whose decision fails gave a result")
		}
		if n := e.count(t, "SELECT count(*) FROM "+audit.EventsTable+" WHERE action = 'read' AND operation = 'list-charges' AND outcome = 'failed' AND error_code = $1", audit.ErrorCodeUndecided); n != 2 {
			t.Fatalf("the failed list wrote %d failed read rows, want one for each of the 2 rows", n)
		}
		if _, err := e.writer.RecordRead(context.Background(), e.auditDB, mustTenant(t, "t1"), audit.Event{ActorID: "user:u-7",
			ActorKind: audit.ActorUser, ResourceType: "charge", ResourceID: "ch-3", Service: "shop", ErrorCode: "the provider is down"}); err == nil {
			t.Fatal("a read row with an error text as its code was written")
		}
	})
}

// Person is an entity with a pii field, a jsonb field, an optional jsonb field and a bytes field (erp-demo G11).
type Person struct {
	ID, Email    string
	Roles, Prefs json.RawMessage
	Avatar       []byte
}

// personTypes are the declared column types of Person.
func personTypes() map[string]ColumnType {
	return map[string]ColumnType{"id": ColumnText, "email": ColumnText, "roles": ColumnJSON, "prefs": ColumnJSON, "avatar": ColumnBytes}
}

// personEntity is the Person entity with the column types types.
func personEntity(t *testing.T, types map[string]ColumnType) *Entity[Person] {
	t.Helper()
	e, err := personEntityOf(types)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func personEntityOf(types map[string]ColumnType) (*Entity[Person], error) {
	return NewEntity(Spec{Name: "person", Schema: "app", Table: "people", Key: "id", Columns: []string{"id", "email", "roles", "prefs", "avatar"},
		ResourceType: "person", ReadPermission: "view", ListPermission: "view", Fields: audit.Fields{"email": audit.PII}, Types: types},
		func(r Row) (Person, error) {
			var p Person
			var err error
			if p.ID, err = r.Text("id"); err != nil {
				return Person{}, err
			}
			if p.Email, err = r.Text("email"); err != nil {
				return Person{}, err
			}
			if p.Roles, err = r.JSON("roles"); err != nil {
				return Person{}, err
			}
			if p.Prefs, err = r.JSON("prefs"); err != nil {
				return Person{}, err
			}
			p.Avatar, err = r.Bytes("avatar")
			return p, err
		})
}

// The rows of people. The e-mail values hold #, which no hex, base64 or UUID text can hold, so only a leak of the
// value matches. p-1 has no prefs and an avatar of bytes that are no JSON text; p-2 has prefs and an avatar of bytes
// that are JSON text, which the audit must still give as base64.
const insertPeople = `INSERT INTO app.people (tenant_id, id, email, roles, prefs, avatar) VALUES
	('t1', 'p-1', 'ann#11@x.example', '["admin", {"scope": "north"}]', NULL, '\x00ff7b'),
	('t1', 'p-2', 'bo#22@x.example', '{"tier": 2}', '{"lang": "de"}', convert_to('{"a":1}', 'UTF8'))`

// readPeople runs the query name: a Get of p-1 when get is true, else a List of every person, and gives the result.
func readPeople(t *testing.T, rt *Runtime, people *Entity[Person], name string, get bool) ([]Person, error) {
	t.Helper()
	var q *Query
	if get {
		q = mustQuery(t, name, people)
	} else {
		q = mustListQuery(t, name, people, "view")
	}
	return Run(context.Background(), rt, q, bound(t, "t1", "user:u-7", q.Name()), struct{}{},
		func(ctx context.Context, _ struct{}, h *Handle[noProbe]) ([]Person, error) {
			repo, err := Repo(h, people)
			if err != nil {
				return nil, err
			}
			if get {
				p, err := repo.Get(ctx, "p-1")
				if err != nil {
					return nil, err
				}
				return []Person{p.Value()}, nil
			}
			page, err := repo.List(ctx, ListRequest{Limit: 10})
			if err != nil {
				return nil, err
			}
			var out []Person
			for _, p := range page.Items {
				out = append(out, p.Value())
			}
			return out, nil
		})
}

// readStates gives the after state of each read audit row of the operation, by resource ID.
func readStates(t *testing.T, e *env, operation string) map[string]map[string]any {
	t.Helper()
	rows, err := e.admin.QueryContext(context.Background(), "SELECT resource_id, after::text FROM "+audit.EventsTable+
		" WHERE action = 'read' AND operation = $1 ORDER BY seq", operation)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]map[string]any{}
	for rows.Next() {
		var id string
		var state sql.NullString
		if err := rows.Scan(&id, &state); err != nil {
			t.Fatal(err)
		}
		m := map[string]any{}
		if err := json.Unmarshal([]byte(state.String), &m); err != nil {
			t.Fatalf("the read row of %s has the state %q: %v", id, state.String, err)
		}
		out[id] = m
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// member gives the JSON text of the member name of state, so a test compares values and not Go types.
func member(t *testing.T, state map[string]any, name string) string {
	t.Helper()
	v, ok := state[name]
	if !ok {
		t.Fatalf("the state %v has no member %s", state, name)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestReadAuditTypes(t *testing.T) {
	t.Run("ST-SUP-RP11-LC08-T03 A pii read of an entity with a jsonb field answers and is audited with the JSON value", func(t *testing.T) {
		e := newEnv(t)
		rt := e.runtimeOf(t, &fakeDecision{decide: allowAll})
		people := personEntity(t, personTypes())
		e.insert(t, insertPeople)
		for _, get := range []bool{true, false} {
			name := map[bool]string{true: "person-detail", false: "person-list"}[get]
			got, err := readPeople(t, rt, people, name, get)
			if err != nil {
				t.Fatalf("%s gave %v, want the result", name, err)
			}
			if len(got) == 0 || got[0].ID != "p-1" || string(got[0].Roles) != `["admin", {"scope": "north"}]` {
				t.Fatalf("%s gave %+v, want p-1 first with its roles", name, got)
			}
			states := readStates(t, e, name)
			if want := map[bool]int{true: 1, false: 2}[get]; len(states) != want {
				t.Fatalf("%s wrote %d read rows, want %d: %v", name, len(states), want, states)
			}
			if r := member(t, states["p-1"], "roles"); r != `["admin",{"scope":"north"}]` {
				t.Errorf("%s: the read row of p-1 holds roles %s, want the JSON value", name, r)
			}
			if r := member(t, states["p-1"], "email"); r != `"`+audit.RedactionMarker+`"` {
				t.Errorf("%s: the read row of p-1 holds email %s, want the redaction marker", name, r)
			}
			if !get {
				if r := member(t, states["p-2"], "roles"); r != `{"tier":2}` {
					t.Errorf("the list read row of p-2 holds roles %s, want the JSON value", r)
				}
			}
		}
		if n := e.count(t, "SELECT count(*) FROM "+audit.EventsTable+" WHERE row_to_json("+audit.EventsTable+")::text LIKE '%#11@%' OR row_to_json("+audit.EventsTable+")::text LIKE '%#22@%'"); n != 0 {
			t.Fatalf("%d audit rows hold a pii value", n)
		}
	})

	t.Run("ST-SUP-RP11-LC08-T04 A bytes field of a pii entity is audited as its base64 text, also when it holds JSON text", func(t *testing.T) {
		e := newEnv(t)
		rt := e.runtimeOf(t, &fakeDecision{decide: allowAll})
		people := personEntity(t, personTypes())
		e.insert(t, insertPeople)
		got, err := readPeople(t, rt, people, "person-list", false)
		if err != nil {
			t.Fatalf("the list gave %v, want the result", err)
		}
		if len(got) != 2 || !bytes.Equal(got[0].Avatar, []byte{0x00, 0xff, 0x7b}) || string(got[1].Avatar) != `{"a":1}` {
			t.Fatalf("the list gave %+v, want both people with their avatar bytes", got)
		}
		states := readStates(t, e, "person-list")
		if a := member(t, states["p-1"], "avatar"); a != `"AP97"` {
			t.Errorf("the read row of p-1 holds avatar %s, want the base64 text \"AP97\"", a)
		}
		if a := member(t, states["p-2"], "avatar"); a != `"eyJhIjoxfQ=="` {
			t.Errorf("the read row of p-2 holds avatar %s, want the base64 text of its bytes, not their JSON value", a)
		}
	})

	t.Run("ST-SUP-RP11-LC08-T05 A NULL jsonb field of a pii entity answers and is audited as null", func(t *testing.T) {
		e := newEnv(t)
		rt := e.runtimeOf(t, &fakeDecision{decide: allowAll})
		people := personEntity(t, personTypes())
		e.insert(t, insertPeople)
		got, err := readPeople(t, rt, people, "person-detail", true)
		if err != nil {
			t.Fatalf("the read gave %v, want the result", err)
		}
		if len(got) != 1 || got[0].Prefs != nil {
			t.Fatalf("the read gave %+v, want p-1 with no prefs", got)
		}
		states := readStates(t, e, "person-detail")
		if p := member(t, states["p-1"], "prefs"); p != "null" {
			t.Errorf("the read row of p-1 holds prefs %s, want null", p)
		}
		if _, err := readPeople(t, rt, people, "person-list", false); err != nil {
			t.Fatalf("the list gave %v, want the result", err)
		}
		if p := member(t, readStates(t, e, "person-list")["p-2"], "prefs"); p != `{"lang":"de"}` {
			t.Errorf("the list read row of p-2 holds prefs %s, want the JSON value", p)
		}
	})

	t.Run("ST-SUP-RP11-LC08-T06 A pii read whose column has no declared type, an unknown type or a value of another type fails closed", func(t *testing.T) {
		missing := personTypes()
		delete(missing, "roles")
		unknown := personTypes()
		unknown["roles"] = "jsonish"
		extra := personTypes()
		extra["nickname"] = ColumnText
		for name, types := range map[string]map[string]ColumnType{"no type of roles": missing, "an unknown type": unknown,
			"a type of no column": extra, "no types": nil} {
			if _, err := personEntityOf(types); err == nil {
				t.Errorf("NewEntity took a pii entity with %s", name)
			}
		}
		// Each error is compared with its full fixed text: a []byte value that leaks with %v is decimal bytes, which
		// hold no #, so a check for a part of the value does not find the leak (sec-35 L1).
		for _, c := range []struct {
			column string
			typ    ColumnType
			value  any
			want   string
		}{
			{"roles", "jsonish", []byte(`"ann#11@x.example"`), `query: the column roles has the type "jsonish", which is not a column type`},
			{"roles", "jsonish", "ann#11@x.example", `query: the column roles has the type "jsonish", which is not a column type`},
			{"email", ColumnText, []byte("ann#11@x.example"), "query: the column email holds a value that is not of its declared type text"},
			{"email", ColumnInt, "ann#11@x.example", "query: the column email holds a value that is not of its declared type int"},
		} {
			if got, err := auditValue(c.column, c.typ, c.value); got != nil || err == nil || err.Error() != c.want {
				t.Errorf("auditValue(%s, %s) of a %T gave %v, %v, want the error %q", c.column, c.typ, c.value, got, err, c.want)
			}
		}
		// A row of a column with no declared type gives no state, also when NewEntity did not see the column.
		row := Row{columns: []string{"id", "email"}, values: []any{"p-1", "ann#11@x.example"}}
		if state, err := row.auditState(map[string]ColumnType{"id": ColumnText}); err == nil || state != nil ||
			err.Error() != "query: the column email has no declared type" {
			t.Errorf("auditState of a column with no declared type gave %v, %v, want an error with no value", state, err)
		}
		e := newEnv(t)
		rt := e.runtimeOf(t, &fakeDecision{decide: allowAll})
		wrong := personTypes()
		wrong["email"] = ColumnInt
		people := personEntity(t, wrong)
		e.insert(t, insertPeople)
		got, err := readPeople(t, rt, people, "person-detail", true)
		if err == nil || got != nil {
			t.Fatalf("a read of a value of another type than its declared one gave %+v, %v, want no result", got, err)
		}
		if !errors.Is(err, errAudit) || strings.Contains(err.Error(), "#11") || strings.Contains(err.Error(), "p-1") {
			t.Fatalf("the read gave the error %q, want a read audit error that holds no value and no ID", err)
		}
		if n := e.count(t, "SELECT count(*) FROM "+audit.EventsTable+" WHERE action = 'read'"); n != 0 {
			t.Fatalf("the failed read wrote %d read rows, want none", n)
		}
		// A fill that does not return the error of its Get or List still gives no result: the failed conversion
		// fails the read by itself (sec-35 L2).
		for _, get := range []bool{true, false} {
			name := "person-detail"
			q := mustQuery(t, name, people)
			if !get {
				name = "person-list"
				q = mustListQuery(t, name, people, "view")
			}
			got, err := Run(context.Background(), rt, q, bound(t, "t1", "user:u-7", q.Name()), struct{}{},
				func(ctx context.Context, _ struct{}, h *Handle[noProbe]) ([]Person, error) {
					repo, err := Repo(h, people)
					if err != nil {
						return nil, err
					}
					if get {
						_, _ = repo.Get(ctx, "p-1")
					} else {
						_, _ = repo.List(ctx, ListRequest{Limit: 10})
					}
					return []Person{{ID: "made-up"}}, nil
				})
			if got != nil || !errors.Is(err, errAudit) || strings.Contains(err.Error(), "#11") {
				t.Fatalf("%s with a fill that drops the read error gave %+v, %v, want no result and a read audit error with no value",
					name, got, err)
			}
		}
		if n := e.count(t, "SELECT count(*) FROM "+audit.EventsTable+" WHERE action = 'read'"); n != 0 {
			t.Fatalf("the failed reads wrote %d read rows, want none", n)
		}
	})
}

func TestQueryFillFailed(t *testing.T) {
	t.Run("ST-SUP-RP11-LC10-T03 A query fill that ends with 500 logs query.fill.failed with its cause class and no value", func(t *testing.T) {
		var logs bytes.Buffer
		previous, held := slog.Default(), fillLog
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
		// The clock of the log stays at one time, so each later failure of the same class is in the same ten seconds.
		now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
		fillLog = authz.NewCauseLog(func() time.Time { return now })
		t.Cleanup(func() { slog.SetDefault(previous); fillLog = held })

		e := newEnv(t)
		rt := e.runtimeOf(t, &fakeDecision{decide: allowAll})
		e.insert(t, insertPeople)
		wrong := personTypes()
		wrong["email"] = ColumnInt
		people, mistyped := personEntity(t, personTypes()), personEntity(t, wrong)
		refused := NewRefusal("person-detail", "blocked", http.StatusUnprocessableEntity, "person.blocked")
		q := mustQuery(t, "person-detail", people)
		run := func(read *Entity[Person], fail func(ctx context.Context, repo *Repository[Person]) error) error {
			rq := q
			if read != people {
				rq = mustQuery(t, "person-detail", read)
			}
			_, err := Run(context.Background(), rt, rq, bound(t, "t1", "user:u-7", rq.Name()), struct{}{},
				func(ctx context.Context, _ struct{}, h *Handle[noProbe]) (struct{}, error) {
					repo, err := Repo(h, read)
					if err != nil {
						return struct{}{}, err
					}
					return struct{}{}, fail(ctx, repo)
				})
			return err
		}
		lines := func() []string {
			var out []string
			for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
				if strings.Contains(line, "query.fill.failed") {
					out = append(out, line)
				}
			}
			return out
		}
		// A missing row, a declared refusal and a bad request write no line.
		for name, fail := range map[string]func(context.Context, *Repository[Person]) error{
			"a missing row": func(ctx context.Context, repo *Repository[Person]) error {
				_, err := repo.Get(ctx, "p-404")
				return err
			},
			"a declared refusal": func(context.Context, *Repository[Person]) error { return refused },
			"a bad request":      func(context.Context, *Repository[Person]) error { return ErrBadRequest },
		} {
			if err := run(people, fail); err == nil {
				t.Fatalf("the fill of %s gave no error", name)
			}
			if l := lines(); len(l) != 0 {
				t.Fatalf("the fill of %s wrote query.fill.failed: %v", name, l)
			}
		}
		// A fill error with a value and an ID in its text is logged with its class only.
		valueErr := func(ctx context.Context, repo *Repository[Person]) error {
			p, err := repo.Get(ctx, "p-1")
			if err != nil {
				return err
			}
			return fmt.Errorf("the fill failed for %s of %s", p.Value().Email, p.Value().ID)
		}
		if err := run(people, valueErr); err == nil {
			t.Fatal("the failed fill gave no error")
		}
		// A read whose read audit cannot convert a value, a panic and a second fill error in the same ten seconds.
		if err := run(mistyped, func(ctx context.Context, repo *Repository[Person]) error {
			_, err := repo.Get(ctx, "p-1")
			return err
		}); !errors.Is(err, errAudit) {
			t.Fatalf("the read of a mistyped value gave %v, want the read audit error", err)
		}
		if err := run(people, func(context.Context, *Repository[Person]) error { panic("ann#11@x.example") }); err == nil {
			t.Fatal("the panicking fill gave no error")
		}
		if err := run(people, valueErr); err == nil {
			t.Fatal("the second failed fill gave no error")
		}
		want := []string{
			`level=ERROR msg="query: the fill failed" event.name=query.fill.failed query=person-detail cause=fill_error held_back=0`,
			`level=ERROR msg="query: the fill failed" event.name=query.fill.failed query=person-detail cause=audit held_back=0`,
			`level=ERROR msg="query: the fill failed" event.name=query.fill.failed query=person-detail cause=panic held_back=0`,
		}
		got := lines()
		if len(got) != len(want) {
			t.Fatalf("the log has the lines %q, want %d lines of query.fill.failed", got, len(want))
		}
		for i, line := range got {
			if !strings.HasSuffix(line, strings.TrimPrefix(want[i], "level=ERROR ")) || !strings.Contains(line, "level=ERROR") {
				t.Errorf("line %d is %q, want %q", i, line, want[i])
			}
		}
		if strings.Contains(logs.String(), "#11") || strings.Contains(logs.String(), "p-1") || strings.Contains(logs.String(), "fill failed for") {
			t.Fatalf("the log holds a value, an ID or the error text:\n%s", logs.String())
		}
		// After ten seconds the next line of the class gives the number of lines that it held back.
		now = now.Add(11 * time.Second)
		if err := run(people, valueErr); err == nil {
			t.Fatal("the third failed fill gave no error")
		}
		if got := lines(); len(got) != 4 || !strings.HasSuffix(got[3], "cause=fill_error held_back=1") {
			t.Fatalf("the log has the lines %q, want a fourth line of fill_error with held_back=1", got)
		}
	})
}
