package command

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestUniqueKey(t *testing.T) {
	t.Run("ST-SUP-RP10-LC48-T01 A unique violation of a declared key names the fields of the key, and of another index no field", func(t *testing.T) {
		e := newEnv(t)
		_, orders, _ := referenceEntities(e)
		e.exec(`CREATE UNIQUE INDEX placed_orders_customer_id_product_id_key ON placed_orders (tenant_id, customer_id, product_id)`,
			`CREATE UNIQUE INDEX placed_orders_hand_made ON placed_orders (tenant_id, product_id) WHERE customer_id LIKE 'hand%'`)
		s := orders.spec
		s.Parent, s.ResourceType, s.ReadPermission = nil, "", ""
		s.Unique = map[string]string{"placed_orders_customer_id_product_id_key": "customer_id, product_id"}
		entity, err := NewEntity(s)
		if err != nil {
			t.Fatal(err)
		}
		create := func(key string, v placedOrder) error {
			cmd := e.command("place-"+key, nil, entity)
			_, err := e.run(cmd, e.request("t1", u7(t), key, `{}`), fillOf(func(ctx context.Context, h *Handle[testProbe]) error {
				_, err := repo(t, h, entity).Create(ctx, v)
				return err
			}))
			return err
		}
		const secret = "c-jane.doe@example.com"
		if err := create("k-u1", placedOrder{ID: "u1", CustomerID: secret, ProductID: "p1"}); err != nil {
			t.Fatal(err)
		}
		err = create("k-u2", placedOrder{ID: "u2", CustomerID: secret, ProductID: "p1"})
		o := OutcomeOf(err)
		if !errors.Is(err, ErrConflict) || o.Status != http.StatusConflict || o.Code != "conflict" || o.Field != "customer_id, product_id" || o.Reason != ReasonUnique {
			t.Fatalf("a second row with the same key gave %v (%+v), want 409 conflict that names customer_id, product_id", err, o)
		}
		st := status.Convert(o.RPCStatus())
		if st.Code() != codes.Aborted || st.Message() != "conflict" {
			t.Fatalf("the gRPC status of the unique conflict is %v, want Aborted conflict", st)
		}
		for _, text := range []string{err.Error(), o.Field, o.Reason, st.String()} {
			if strings.Contains(text, secret) || strings.Contains(text, "placed_orders_customer_id_product_id_key") {
				t.Fatalf("the conflict holds a value or the index name: %q", text)
			}
		}
		// A unique index that the entity does not declare stays a conflict that names no field.
		if err := create("k-u3", placedOrder{ID: "u3", CustomerID: "hand1", ProductID: "p7"}); err != nil {
			t.Fatal(err)
		}
		err = create("k-u4", placedOrder{ID: "u4", CustomerID: "hand2", ProductID: "p7"})
		if o := OutcomeOf(err); o.Status != http.StatusConflict || o.Field != "" || o.Reason != "" {
			t.Fatalf("a violation of an index that the entity does not declare gave %+v, want 409 with no field", o)
		}
		// Another conflict, such as a moved version, names no field either.
		if o := OutcomeOf(&conflictError{text: "moved"}); o.Status != http.StatusConflict || o.Field != "" {
			t.Fatalf("a moved version gave %+v, want 409 with no field", o)
		}
	})
}
