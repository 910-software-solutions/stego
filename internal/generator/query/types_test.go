package query

import (
	"strings"
	"testing"

	"github.com/910-software-solutions/stego/internal/generator/capability"
)

func TestColumnTypes(t *testing.T) {
	t.Run("ST-SUP-RC33-OC14-T01 The entity of a query with a pii field declares the type of each column, and an entity without one declares none", func(t *testing.T) {
		source := strings.Join(strings.Fields(string(generated(t, shopContext(t))["queries/queries.go"])), " ")
		want := `Types: map[string]query.ColumnType{"id": query.ColumnText, "order_id": query.ColumnText, ` +
			`"card_last4": query.ColumnText, "amount": query.ColumnInt, "created_time": query.ColumnTime, ` +
			`"updated_time": query.ColumnTime},`
		if n := strings.Count(source, "Types: map[string]query.ColumnType{"); n != 1 || !strings.Contains(source, want) {
			t.Fatalf("queries.go has %d Types declarations, want one for Charge only: %s", n, want)
		}
		for read, want := range map[string]string{
			capability.ReadText: "ColumnText", capability.ReadInt: "ColumnInt", capability.ReadInt32: "ColumnInt",
			capability.ReadBool: "ColumnBool", capability.ReadTime: "ColumnTime", capability.ReadFloat: "ColumnFloat",
			capability.ReadBytes: "ColumnBytes", capability.ReadJSON: "ColumnJSON",
		} {
			if got := columnType(capability.Field{Read: read}); got != want {
				t.Errorf("the read %s has the column type %s, want %s", read, got, want)
			}
		}
		// A read with no column type gives a type that the query runtime refuses at NewEntity (fail closed).
		if got := columnType(capability.Field{Read: "decimal"}); got != `ColumnType("decimal")` {
			t.Errorf("the read decimal has the column type %s, want ColumnType(\"decimal\")", got)
		}
	})
}
