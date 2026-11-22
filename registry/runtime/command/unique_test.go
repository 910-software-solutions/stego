package command

import (
	"context"
	"errors"
	"fmt"
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

	t.Run("ST-SUP-RP10-LC48-T02 A unique index of a newer release that the entity does not declare names the declared fields of its name, and no value", func(t *testing.T) {
		e := newEnv(t)
		_, orders, _ := referenceEntities(e)
		// A unique step of a newer release built these indexes; this release declares none of them in Unique.
		e.exec(`CREATE UNIQUE INDEX placed_orders_customer_id_product_id_key ON placed_orders (tenant_id, customer_id, product_id)`,
			`CREATE UNIQUE INDEX placed_orders_product_id_hand_key ON placed_orders (tenant_id, product_id) WHERE customer_id LIKE 'hand%'`)
		s := orders.spec
		s.Parent, s.ResourceType, s.ReadPermission = nil, "", ""
		s.Unique = nil
		s.Constraints = map[string]string{"customer_id": "customer", "product_id": "product", "placed_orders_product_id_check": "product"}
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
		if err := create("k-n1", placedOrder{ID: "n1", CustomerID: secret, ProductID: "p1"}); err != nil {
			t.Fatal(err)
		}
		err = create("k-n2", placedOrder{ID: "n2", CustomerID: secret, ProductID: "p1"})
		o := OutcomeOf(err)
		if o.Status != http.StatusConflict || o.Field != "customer, product" || o.Reason != ReasonUnique {
			t.Fatalf("a violation of the index of a newer release gave %v (%+v), want 409 that names customer, product", err, o)
		}
		for _, text := range []string{err.Error(), o.Field, o.Reason} {
			if strings.Contains(text, secret) || strings.Contains(text, "placed_orders_customer_id_product_id_key") {
				t.Fatalf("the conflict holds a value or the index name: %q", text)
			}
		}
		// A part of the name that is no column of the entity names no field.
		if err := create("k-n3", placedOrder{ID: "n3", CustomerID: "hand1", ProductID: "p7"}); err != nil {
			t.Fatal(err)
		}
		err = create("k-n4", placedOrder{ID: "n4", CustomerID: "hand2", ProductID: "p7"})
		if o := OutcomeOf(err); o.Status != http.StatusConflict || o.Field != "" {
			t.Fatalf("a violation of an index with a part that is no column gave %+v, want 409 with no field", o)
		}
		for name, want := range map[string]string{"placed_orders_customer_id_key": "customer", "other_customer_id_key": "",
			"placed_orders_customer_id": "", "placed_orders__key": "", "placed_orders_placed_orders_product_id_check_key": "",
			"placed_orders_tenant_id_customer_id_key": ""} {
			if got := entity.uniqueFields("placed_orders", name); got != want {
				t.Errorf("the index %s names %q, want %q", name, got, want)
			}
		}
	})

	t.Run("ST-SUP-RP10-LC48-T03 The fields of an index name are split once for each name, and the memo holds a bounded count of names", func(t *testing.T) {
		e := &Entity[placedOrder]{spec: EntitySpec[placedOrder]{Constraints: map[string]string{"customer_id": "customer", "product_id": "product"}}}
		if got := e.uniqueFields("placed_orders", "placed_orders_customer_id_product_id_key"); got != "customer, product" {
			t.Fatalf("the index names %q, want customer, product", got)
		}
		// The second read of the same name takes the memo: it does not split the name again.
		e.spec.Constraints = nil
		if got := e.uniqueFields("placed_orders", "placed_orders_customer_id_product_id_key"); got != "customer, product" {
			t.Fatalf("the second read of the name gave %q, want the memo customer, product", got)
		}
		for i := 0; i < 2*maxUniqueNames; i++ {
			e.uniqueFields("placed_orders", fmt.Sprintf("placed_orders_c%d_key", i))
		}
		kept := 0
		e.uniqueNames.Range(func(any, any) bool { kept++; return true })
		if kept > maxUniqueNames {
			t.Fatalf("the memo holds %d names, want at most %d", kept, maxUniqueNames)
		}
	})
}
