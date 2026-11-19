package postgresadapter_test

import (
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/postgresadapter"
	"github.com/jsell-rh/stego/internal/types"
)

// sealEntities are the entities of the release before the seals (ST-SUP-RC01-OC49): the owned entity Vault with the
// title, the optional pii string note of at most 200 characters and the required secret string code of at most 40
// characters, both in clear, as a generator before postgres-adapter 6.13.0 stored code (walkthrough 13, W13-B1).
func sealEntities() []types.Entity {
	two, forty := 200, 40
	return []types.Entity{{Name: "Vault", Fields: []types.Field{{Name: "title", Type: types.FieldTypeString},
		{Name: "note", Type: types.FieldTypeString, Optional: true, MaxLength: &two, Sensitivity: "pii"},
		{Name: "code", Type: types.FieldTypeString, MaxLength: &forty, Sensitivity: "secret", ClearStored: true}}}}
}

func sealCollections() []types.Collection {
	return []types.Collection{{Name: "vaults", Entity: "Vault", Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpList}}}
}

// sealBase is the release before the seals.
func sealBase(ns, tenancyNS string) gen.Context {
	return runtimeService(ns, tenancyNS, sealEntities(), sealCollections())
}

// sealSteps gives the release with the seal of Vault.note from pii and of Vault.code from secret at expand, and with contract
// their contract steps too, with the stored names that the compiler gives (gen.SealedNames).
func sealSteps(ns, tenancyNS string, contract bool) gen.Context {
	ctx := sealBase(ns, tenancyNS)
	entities := sealEntities()
	for i := range entities[0].Fields[1:] {
		entities[0].Fields[i+1].Sensitivity, entities[0].Fields[i+1].ClearStored = "secret", false
	}
	ctx.SchemaChanges = []types.SchemaChange{
		{ID: "vault-note-seal", Kind: "seal", Entity: "Vault", Field: "note", From: "pii"},
		// code was declared secret, and a generator before postgres-adapter 6.13.0 stored it in clear (W13-B1).
		{ID: "vault-code-seal", Kind: "seal", Entity: "Vault", Field: "code", From: "secret"},
	}
	for _, change := range ctx.SchemaChanges {
		ctx.SchemaHistory = append(ctx.SchemaHistory, types.SchemaStep{Change: change.ID, Phase: "expand"})
	}
	if contract {
		for _, change := range ctx.SchemaChanges {
			ctx.SchemaHistory = append(ctx.SchemaHistory, types.SchemaStep{Change: change.ID, Phase: "contract"})
		}
	}
	ctx.Entities = gen.SealedNames(entities, ctx.SchemaChanges, ctx.SchemaHistory)
	return ctx
}

func TestSealSteps(t *testing.T) {
	t.Run("ST-SUP-RC01-OC49-T01 The expand of a seal adds the sealed column beside the clear one, and the contract checks and drops the clear column", func(t *testing.T) {
		roles := map[string]map[string]any{gen.TenancyComponent: {"runtime_role": "seal_runtime", "owner_role": "seal_owner"}}
		base, head, contract := sealBase("sealbase", "sealbasetenancy"), sealSteps("seal", "sealtenancy", false), sealSteps("sealc", "sealctenancy", true)
		for _, ctx := range []*gen.Context{&base, &head, &contract} {
			ctx.PeerConfigs = roles
		}
		var files [3][]gen.File
		for i, ctx := range []gen.Context{base, head, contract} {
			out, _, err := new(postgresadapter.Generator).Generate(ctx)
			if err != nil {
				t.Fatal(err)
			}
			files[i] = out
		}
		// The baseline of each release is the baseline of the release before: the fields come back in clear.
		initial := generated(t, files[0], "sealbase/migrations/001_initial.sql")
		for i, name := range []string{"seal", "sealc"} {
			if got := generated(t, files[i+1], name+"/migrations/001_initial.sql"); got != initial {
				t.Errorf("the 001_initial of %s is not the one of the release before:\n%s\nwant\n%s", name, got, initial)
			}
		}
		note := generated(t, files[1], "seal/migrations/step_0001_vault-note-seal_expand.sql")
		for _, want := range []string{
			"ALTER TABLE public.\"vaults\" ADD COLUMN \"note_sealed\" bytea;\n",
			"CREATE TABLE IF NOT EXISTS stego_schema.releases",
			"CREATE TABLE IF NOT EXISTS stego_schema.seals",
			"GRANT EXECUTE ON FUNCTION stego_schema.seal_checkpoint(text, text, uuid, bigint, bigint, boolean, text, text, bigint) TO \"seal_runtime\";",
			"INSERT INTO stego_schema.seals (change_id, expand) VALUES ('vault-note-seal', 'step_0001_vault-note-seal_expand') ON CONFLICT (change_id) DO NOTHING;",
		} {
			if !strings.Contains(note, want) {
				t.Errorf("the expand of the seal of note does not hold %s:\n%s", want, note)
			}
		}
		for _, gone := range []string{"DROP", "ALTER COLUMN", "CHECK", "TRIGGER"} {
			if strings.Contains(strings.ReplaceAll(note, "DROP OWNED", ""), "ALTER TABLE public.\"vaults\" "+gone) {
				t.Errorf("the expand of the seal changes more than its column: %s", gone)
			}
		}
		// The release of the seals names the clear and the sealed columns, the sealed one nullable, and the optional
		// CHECKs of the contract.
		manifest := generated(t, files[1], "seal/schema_manifest.go")
		for _, want := range []string{
			`{Name: "note", Type: "character varying(200)", NotNull: false}`, `{Name: "note_sealed", Type: "bytea", NotNull: false}`,
			`{Name: "code", Type: "character varying(40)", NotNull: true}`, `{Name: "code_sealed", Type: "bytea", NotNull: false}`,
			`{Name: "vaults_note_sealed_complete", Kind: manifest.KindCheck, Definition: "CHECK (((note_sealed IS NOT NULL) OR (note IS NULL)))", NotValid: true}`,
			`{Name: "vaults_code_sealed_not_null", Kind: manifest.KindCheck, Definition: "CHECK ((code_sealed IS NOT NULL))", NotValid: true}`,
		} {
			if !strings.Contains(manifest, want) {
				t.Errorf("the manifest of the release of the seals does not hold %s:\n%s", want, manifest)
			}
		}
		sealedColumns := generated(t, files[1], "seal/sealed_columns.go")
		for _, want := range []string{
			`{Change: "vault-note-seal", Entity: "Vault", Table: "vaults", Clear: "note", Sealed: "note_sealed",`,
			`Select: "SELECT \"id\"::text, \"note\"::text, \"note_sealed\" FROM public.\"vaults\" WHERE \"tenant_id\" = $1 AND \"id\" > $2::uuid ORDER BY \"id\" LIMIT $3 FOR UPDATE"`,
			`Update: "UPDATE public.\"vaults\" SET \"note_sealed\" = $3 WHERE \"tenant_id\" = $1 AND \"id\" = $2::uuid"`,
			`Column: "note_sealed"`,
		} {
			if !strings.Contains(sealedColumns, want) {
				t.Errorf("sealed_columns.go of the release of the seals does not hold %s:\n%s", want, sealedColumns)
			}
		}
		// After the contract the field is in its sealed column alone, NOT NULL for the required code.
		manifest = generated(t, files[2], "sealc/schema_manifest.go")
		for _, want := range []string{`{Name: "note_sealed", Type: "bytea", NotNull: false}`, `{Name: "code_sealed", Type: "bytea", NotNull: true}`} {
			if !strings.Contains(manifest, want) {
				t.Errorf("the manifest after the contract does not hold %s:\n%s", want, manifest)
			}
		}
		for _, gone := range []string{`{Name: "note",`, `{Name: "code",`, "_complete"} {
			if strings.Contains(manifest, gone) {
				t.Errorf("the manifest after the contract names %s:\n%s", gone, manifest)
			}
		}
		if strings.Contains(generated(t, files[2], "sealc/sealed_columns.go"), "SealColumns") {
			t.Error("sealed_columns.go after the contract still gives the seal job")
		}
		migrate := generated(t, files[2], "sealc/migrate.go")
		for _, want := range []string{
			`{SQL: "ALTER TABLE public.\"vaults\" ADD CONSTRAINT \"vaults_code_sealed_complete\" CHECK ((code_sealed IS NOT NULL) OR (code IS NULL)) NOT VALID"`,
			`{SQL: "ALTER TABLE public.\"vaults\" VALIDATE CONSTRAINT \"vaults_code_sealed_complete\""`,
			`{SQL: "ALTER TABLE public.\"vaults\" ALTER COLUMN \"code_sealed\" SET NOT NULL"`,
			`Also: []string{"ALTER TABLE public.\"vaults\" DROP COLUMN IF EXISTS \"code\"", "DELETE FROM stego_schema.seals WHERE change_id = 'vault-code-seal'"}},`,
			`}, Seal: true})`,
		} {
			if !strings.Contains(migrate, want) {
				t.Errorf("migrate.go of the contract does not hold %s", want)
			}
		}
		if strings.Contains(migrate, `ALTER COLUMN \"note_sealed\" SET NOT NULL`) {
			t.Error("the contract of the optional note sets its sealed column NOT NULL")
		}
	})

	t.Run("ST-SUP-RC01-OC49-T01 A seal from public of a field that a list searches keeps its search index on the clear column until the contract drops the column", func(t *testing.T) {
		roles := map[string]map[string]any{gen.TenancyComponent: {"runtime_role": "seal_runtime", "owner_role": "seal_owner"}}
		clear := []types.Entity{{Name: "Vault", Fields: []types.Field{{Name: "title", Type: types.FieldTypeString}, {Name: "code", Type: types.FieldTypeString}}}}
		release := func(ns string, phases ...string) gen.Context {
			ctx := runtimeService(ns, ns+"tenancy", clear, sealCollections())
			ctx.PeerConfigs = roles
			if len(phases) == 0 {
				return ctx
			}
			entities := []types.Entity{{Name: "Vault", Fields: []types.Field{{Name: "title", Type: types.FieldTypeString}, {Name: "code", Type: types.FieldTypeString, Sensitivity: "secret"}}}}
			// The base declares code with no sensitivity, and the seal names it public (walkthrough 15, W15-B1).
			ctx.SchemaChanges = []types.SchemaChange{{ID: "vault-code-seal", Kind: "seal", Entity: "Vault", Field: "code", From: "public"}}
			for _, phase := range phases {
				ctx.SchemaHistory = append(ctx.SchemaHistory, types.SchemaStep{Change: "vault-code-seal", Phase: phase})
			}
			ctx.Entities = gen.SealedNames(entities, ctx.SchemaChanges, ctx.SchemaHistory)
			return ctx
		}
		var files [3][]gen.File
		for i, ctx := range []gen.Context{release("pubbase"), release("pub", "expand"), release("pubc", "expand", "contract")} {
			out, _, err := new(postgresadapter.Generator).Generate(ctx)
			if err != nil {
				t.Fatal(err)
			}
			files[i] = out
		}
		const index = `CREATE INDEX "vaults_code_search" ON public."vaults" ("tenant_id", "code");`
		initial := generated(t, files[0], "pubbase/migrations/001_initial.sql")
		if !strings.Contains(initial, index) {
			t.Fatalf("the base does not give the search index of code:\n%s", initial)
		}
		for i, name := range []string{"pub", "pubc"} {
			if got := generated(t, files[i+1], name+"/migrations/001_initial.sql"); got != initial {
				t.Errorf("the 001_initial of %s with the seal from public is not the one of the base:\n%s\nwant\n%s", name, got, initial)
			}
		}
		// The release of the expand step keeps the clear column with its search index, and no index of the sealed
		// column; after the contract the index went with the clear column.
		if manifest := generated(t, files[1], "pub/schema_manifest.go"); !strings.Contains(manifest, `"vaults_code_search"`) || strings.Contains(manifest, "code_sealed_search") {
			t.Errorf("the manifest of the expand step does not keep the search index of the clear column alone:\n%s", manifest)
		}
		if manifest := generated(t, files[2], "pubc/schema_manifest.go"); strings.Contains(manifest, "vaults_code_search") || strings.Contains(manifest, "code_sealed_search") {
			t.Errorf("the manifest after the contract names a search index of code:\n%s", manifest)
		}
	})

	t.Run("ST-SUP-RC01-OC49-T01 A seal that generation does not carry is refused with its reason", func(t *testing.T) {
		ctx := sealSteps("sealbad", "sealbadtenancy", false)
		ctx.PeerConfigs = map[string]map[string]any{gen.TenancyComponent: {"runtime_role": "seal_runtime", "owner_role": "seal_owner"}}
		// The entity declares a field note_sealed: the seal cannot add its sealed column.
		entities := sealSteps("sealbad", "sealbadtenancy", false).Entities
		entities[0].Fields = append(entities[0].Fields, types.Field{Name: "note_sealed", Type: types.FieldTypeString, Optional: true})
		ctx.Entities = entities
		if _, _, err := new(postgresadapter.Generator).Generate(ctx); err == nil || !strings.Contains(err.Error(), "note_sealed") {
			t.Errorf("a seal whose sealed column the entity declares gives %v", err)
		}
	})
}
