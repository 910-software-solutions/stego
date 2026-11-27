package postgresadapter

import (
	"strings"
	"testing"

	"github.com/910-software-solutions/stego/internal/gen"
	"github.com/910-software-solutions/stego/internal/types"
)

func TestSealedColumns(t *testing.T) {
	t.Run("ST-SUP-RC01-OC48-T01 A secret column is bytea or text with no CHECK and no index, and the re-seal statements take the tenant", func(t *testing.T) {
		eight, four := 8, 4
		ctx := basicContext()
		ctx.Tenancy = &gen.Tenancy{Key: "tenant_id"}
		ctx.PeerNamespaces[gen.TenancyComponent] = "internal/tenancy"
		ctx.ModuleName = "example.com/vault"
		ctx.Entities = append(ctx.Entities, types.Entity{Name: "Vault", Fields: []types.Field{
			{Name: "label", Type: types.FieldTypeString},
			{Name: "api_token", Type: types.FieldTypeString, Sensitivity: "secret", Optional: true, MinLength: &eight, MaxLength: &eight, Pattern: "^[a-z]+$"},
			{Name: "pin", Type: types.FieldTypeString, Sensitivity: "secret", CompareOnly: true, MinLength: &four},
		}})
		ctx.Collections = []types.Collection{{Name: "vaults", Entity: "Vault", Operations: []types.Operation{types.OpCreate, types.OpList}}}
		files, _, err := new(Generator).Generate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		sql := findFileContent(t, files, "internal/storage/migrations/001_initial.sql")
		manifest := findFileContent(t, files, "internal/storage/schema_manifest.go")
		for _, want := range []string{`"api_token" bytea,`, `"pin" text NOT NULL,`} {
			if !strings.Contains(sql, want) {
				t.Errorf("001_initial.sql has no column %s:\n%s", want, sql)
			}
		}
		if strings.Contains(sql, "vaults_api_token_check") || strings.Contains(sql, "vaults_pin_check") || strings.Contains(sql, "api_token_search") ||
			strings.Contains(sql, "pin_search") {
			t.Errorf("001_initial.sql has a CHECK or an index on a secret column:\n%s", sql)
		}
		if !strings.Contains(manifest, `{Name: "api_token", Type: "bytea", NotNull: false, Secret: true}`) || !strings.Contains(manifest, `{Name: "pin", Type: "text", NotNull: true, Secret: true}`) {
			t.Errorf("the schema manifest does not name the secret columns with their types:\n%s", manifest)
		}
		sealed := findFileContent(t, files, "internal/storage/sealed_columns.go")
		for _, want := range []string{`Entity: "Vault", Table: "vaults", Column: "api_token"`, `\"tenant_id\" = $1`, `FOR UPDATE SKIP LOCKED`,
			`substring(\"api_token\" from 1 for 5) <> $2`, `\"id\" > $4::uuid ORDER BY \"id\"`, `AND \"api_token\" = $4`, `SELECT count(*) FROM public.`} {
			if !strings.Contains(sealed, want) {
				t.Errorf("sealed_columns.go has no %s:\n%s", want, sealed)
			}
		}
		if strings.Contains(sealed, `Column: "pin"`) {
			t.Error("sealed_columns.go names the compare-only column, which no key seals")
		}
		plain := basicContext()
		plain.Tenancy = ctx.Tenancy
		plain.PeerNamespaces[gen.TenancyComponent] = "internal/tenancy"
		plain.ModuleName = "example.com/vault"
		files, _, err = new(Generator).Generate(plain)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if strings.HasSuffix(f.Path, "sealed_columns.go") {
				t.Error("a service with no secret field gets sealed_columns.go")
			}
		}
	})
}
