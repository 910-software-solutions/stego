package check

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/google/uuid"

	"github.com/910-software-solutions/stego/registry/runtime/authz"
	"github.com/910-software-solutions/stego/registry/runtime/sealed"
)

// relate writes the tuple of the subject with the relation on the resource of the type and the row ID in the tenant
// of the caller.
func (e *env) relate(c caller, resourceType, id, relation, subject string) {
	t := e.t
	t.Helper()
	resourceID, err := authz.ResourceID(c.tenant, id)
	must(t, err)
	_, err = v1.NewPermissionsServiceClient(e.spice).WriteRelationships(e.ctx, &v1.WriteRelationshipsRequest{Updates: []*v1.RelationshipUpdate{{
		Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
		Relationship: &v1.Relationship{Resource: &v1.ObjectReference{ObjectType: resourceType, ObjectId: resourceID}, Relation: relation,
			Subject: &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: subject}}}}}})
	must(t, err)
}

// release sends release-hold on the hold with the token: the order, the PIN and whether the fill reads the order
// locked.
func (e *env) release(token, hold, body string) reply {
	t := e.t
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, e.server.URL+"/api/v1/holds/"+hold+"/releases", strings.NewReader(body))
	must(t, err)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Idempotency-Key", `"`+uuid.NewString()+`"`)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	must(t, err)
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	must(t, err)
	return reply{status: response.StatusCode, header: response.Header, body: data}
}

func TestCounterReset(t *testing.T) {
	t.Run("ST-SUP-RC32-OC24-T03 A generated service answers 500 and charges nothing for a check of a counted PIN that a command resets on a row on which the command did not decide its permission, and checks it on a row that it locked", func(t *testing.T) {
		// Decision ST-SUP-Q209 (d), at run time (walkthrough 25, "still to check"): the command release-hold is
		// decided on the resource type hold. Its fill reads an order with no lock, through the read permission view,
		// and checks the PIN of the order, which unlock-pin resets. The check gives no match, charges nothing and
		// fails the call. A locked read decides release on the order, so only a caller who holds release there, and
		// thus a reset guard, checks the PIN.
		e := service(t)
		desk, payer := e.as("u-6", "t1", time.Now().Add(time.Hour)), e.as("u-7", "t1", time.Now().Add(time.Hour))
		const right = "4711"
		hash, err := sealed.Hash(right)
		must(t, err)
		order, hold := uuid.NewString(), uuid.NewString()
		_, err = e.admin.ExecContext(e.ctx, `INSERT INTO orders (tenant_id, id, sku, pin) VALUES ($1, $2, 'sku-1', $3)`, desk.tenant.ID(), order, hash)
		must(t, err)
		_, err = e.admin.ExecContext(e.ctx, `INSERT INTO holds (tenant_id, id, order_id, owner_id) VALUES ($1, $2, $3, 'u-6')`, desk.tenant.ID(), hold, order)
		must(t, err)
		// u-6 views the order and owns the hold; u-7 also owns the hold and may release on the order.
		e.relate(desk, "order", order, "viewer", desk.subject())
		e.relate(desk, "hold", hold, "owner", desk.subject())
		e.relate(desk, "order", order, "viewer", payer.subject())
		e.relate(desk, "order", order, "releaser", payer.subject())
		e.relate(desk, "hold", hold, "owner", payer.subject())
		checks := func() int {
			t.Helper()
			return e.count(`SELECT coalesce(sum(checks), 0) FROM attempt_counts WHERE counter = 'Order.pin' AND row_id = $1`, order)
		}
		body := func(pin string, locked bool) string {
			lock := "false"
			if locked {
				lock = "true"
			}
			return `{"orderId":"` + order + `","pin":"` + pin + `","locked":` + lock + `}`
		}
		// The positive control: the same read with no lock and no PIN check passes, so the 500 below comes from the
		// check of the PIN, not from the read (security review of PR #12, L2).
		if got := e.release(desk.token, hold, body("", false)); got.status != http.StatusCreated || checks() != 0 {
			t.Fatalf("a release of an order read with no lock and no PIN check gave %d %s and %d checks, want 201 and none", got.status, got.body, checks())
		}
		// The right PIN of an order read with no lock: 500 internal, and no charge.
		if got := e.release(desk.token, hold, body(right, false)); got.status != http.StatusInternalServerError || !strings.Contains(string(got.body), "internal") || checks() != 0 {
			t.Fatalf("a check of the PIN of an order read with no lock gave %d %s and %d checks, want 500 internal and none", got.status, got.body, checks())
		}
		if got := e.release(payer.token, hold, body("0000", false)); got.status != http.StatusInternalServerError || checks() != 0 {
			t.Fatalf("a check of a wrong PIN of an order read with no lock gave %d %s and %d checks, want 500 and none", got.status, got.body, checks())
		}
		// Locked, the read decides release on the order, which u-6 does not hold.
		if got := e.release(desk.token, hold, body(right, true)); got.status != http.StatusNotFound || checks() != 0 {
			t.Fatalf("a locked read of the order by a caller with no release on it gave %d %s and %d checks, want 404 and none", got.status, got.body, checks())
		}
		// u-7 holds release on the order: a wrong PIN is the declared refusal and one check, the right PIN passes.
		if got := e.release(payer.token, hold, body("0000", true)); got.status != http.StatusUnprocessableEntity || !strings.Contains(string(got.body), "hold.pin_wrong") || checks() != 1 {
			t.Fatalf("a wrong PIN of a locked order gave %d %s and %d checks, want 422 hold.pin_wrong and one", got.status, got.body, checks())
		}
		if got := e.release(payer.token, hold, body(right, true)); got.status != http.StatusCreated {
			t.Fatalf("the right PIN of a locked order gave %d %s, want 201", got.status, got.body)
		}
	})
}
