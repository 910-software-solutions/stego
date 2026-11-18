package postgresadapter

import (
	"bytes"
	"fmt"
	"go/format"
	"path"

	"github.com/jsell-rh/stego/internal/gen"
)

// generateSealedColumns gives the file sealed_columns.go of a service with a readable secret field: ResealColumns,
// the columns that the re-seal job of the controller moves to the first key of the key ring, with the statements of
// the job, which take the tenant as $1 (ST-SUP-RP10-LC46, ST-SUP-RC01-OC48). A service with none gets no file, so its
// generated code does not import the sealing runtime and is not asked for the key ring.
func generateSealedColumns(ctx gen.Context) (gen.File, bool, error) {
	key := tenantColumn(ctx)
	var b bytes.Buffer
	count := 0
	for _, entity := range ctx.Entities {
		for _, field := range entity.Fields {
			if !field.Sealed() {
				continue
			}
			if key == "" || !ownedEntity(entity) {
				return gen.File{}, false, fmt.Errorf("postgres-adapter: the entity %s has the secret field %s, and a tenant does not own it; the command runtime seals a secret at the place of its tenant, so a secret field is only on an owned entity", entity.Name, field.Name)
			}
			table := "public." + sqlIdentifier(tableName(entity.Name))
			column := sqlIdentifier(field.ColumnName())
			tenant := sqlIdentifier(key)
			// A value of another key does not start with the version and the key ID of the first key ($2).
			older := fmt.Sprintf("%s = $1 AND %s IS NOT NULL AND substring(%s from 1 for 5) <> $2", tenant, column, column)
			fmt.Fprintf(&b, "\t{Entity: %q, Table: %q, Column: %q,\n", entity.Name, tableName(entity.Name), field.ColumnName())
			// The job pages by the key after $4, so each run reads each row once (security review of PR #441, L4).
			key := sqlIdentifier(keyColumn)
			fmt.Fprintf(&b, "\t\tSelect: %q,\n", fmt.Sprintf("SELECT %s::text, %s FROM %s WHERE %s AND %s > $4::uuid ORDER BY %s LIMIT $3 FOR UPDATE SKIP LOCKED", key, column, table, older, key, key))
			fmt.Fprintf(&b, "\t\tUpdate: %q},\n", fmt.Sprintf("UPDATE %s SET %s = $3 WHERE %s = $1 AND %s = $2::uuid AND %s = $4", table, column, tenant, key, column))
			count++
		}
	}
	if count == 0 {
		return gen.File{}, false, nil
	}
	var out bytes.Buffer
	fmt.Fprintf(&out, "package %s\n\nimport \"github.com/jsell-rh/stego/registry/runtime/command\"\n\n", path.Base(ctx.OutputNamespace))
	out.WriteString("// ResealColumns are the readable secret fields of the service: the re-seal job of the controller moves each\n// sealed value to the first key of the key ring (ST-SUP-RP10-LC46).\nvar ResealColumns = []command.ResealColumn{\n")
	out.Write(b.Bytes())
	out.WriteString("}\n")
	code, err := format.Source(out.Bytes())
	if err != nil {
		return gen.File{}, false, fmt.Errorf("formatting sealed_columns.go: %w", err)
	}
	return gen.File{Path: path.Join(ctx.OutputNamespace, "sealed_columns.go"), Content: code}, true, nil
}
