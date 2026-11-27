package postgresadapter_test

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/910-software-solutions/stego/internal/gen"
	"github.com/910-software-solutions/stego/internal/generator/postgresadapter"
	"github.com/910-software-solutions/stego/internal/types"
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
			"INSERT INTO stego_schema.seals (change_id, expand) VALUES ('vault-note-seal', 'step_0001_vault-note-seal_expand') ON CONFLICT (change_id) DO NOTHING;",
		} {
			if !strings.Contains(note, want) {
				t.Errorf("the expand of the seal of note does not hold %s:\n%s", want, note)
			}
		}
		// The functions of the seal checkpoints are in no body: the migration job runs them (RegisterSealCheckpoints).
		if strings.Contains(note, "FUNCTION stego_schema.seal_") {
			t.Errorf("the expand of the seal of note holds a function of the seal checkpoints:\n%s", note)
		}
		if migrate := generated(t, files[1], "seal/migrate.go"); !strings.Contains(migrate, "RegisterSealCheckpoints(") ||
			!strings.Contains(migrate, `GRANT EXECUTE ON FUNCTION stego_schema.seal_checkpoint(text, text, uuid, bigint, bigint, boolean, text, text, bigint) TO \"seal_runtime\";`) {
			t.Error("the migrate.go of the release of the seals does not register the seal checkpoints with EXECUTE for the runtime role")
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
			`{Name: "note", Type: "character varying(200)", NotNull: false, Droppable: true}`, `{Name: "note_sealed", Type: "bytea", NotNull: false, Secret: true}`,
			`{Name: "code", Type: "character varying(40)", NotNull: true, Droppable: true}`, `{Name: "code_sealed", Type: "bytea", NotNull: false, Secret: true}`,
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
		for _, want := range []string{`{Name: "note_sealed", Type: "bytea", NotNull: false, Secret: true}`, `{Name: "code_sealed", Type: "bytea", NotNull: true, Secret: true}`} {
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

	t.Run("ST-SUP-RC01-OC12-T11 The manifest marks as droppable the clear column of each open seal and no other column", func(t *testing.T) {
		roles := map[string]map[string]any{gen.TenancyComponent: {"runtime_role": "seal_runtime", "owner_role": "seal_owner"}}
		for _, c := range []struct {
			name      string
			ctx       gen.Context
			droppable int
		}{
			{"the release before the seals", sealBase("dropbase", "dropbasetenancy"), 0},
			{"the release of the expand steps", sealSteps("dropseal", "dropsealtenancy", false), 2},
			{"the release of the contract steps", sealSteps("dropsealc", "dropsealctenancy", true), 0},
		} {
			c.ctx.PeerConfigs = roles
			out, _, err := new(postgresadapter.Generator).Generate(c.ctx)
			if err != nil {
				t.Fatal(err)
			}
			manifest := generated(t, out, c.ctx.OutputNamespace+"/schema_manifest.go")
			if got := strings.Count(manifest, "Droppable: true"); got != c.droppable {
				t.Errorf("%s: the manifest marks %d columns droppable, want %d:\n%s", c.name, got, c.droppable, manifest)
			}
			if c.droppable > 0 {
				for _, want := range []string{`{Name: "note", Type: "character varying(200)", NotNull: false, Droppable: true}`,
					`{Name: "code", Type: "character varying(40)", NotNull: true, Droppable: true}`} {
					if !strings.Contains(manifest, want) {
						t.Errorf("%s: the manifest does not hold %s", c.name, want)
					}
				}
			}
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

func TestSealEarlierDigests(t *testing.T) {
	t.Run("ST-SUP-RC01-OC51-T02 The steps of a seal accept the digests of the bodies that postgres-adapter 6.19.0 and 6.20.0 wrote, and their bodies hold no function of the seal checkpoints", func(t *testing.T) {
		// The digests that the generators of main at postgres-adapter 6.19.0 (8c4bce8c1) and 6.20.0 (5ca603ad2) give
		// these steps, with these roles: the follow-up worker, run 31, generated them with those trees.
		want := map[string][]string{
			"step_0001_vault-note-seal_expand":  {"17bd5249f9112ebc5e517b6d0a5ed93465285405ac06f4411f410cf61fb6a23f", "7634675da1fb0439a74e05c63f70df1086e0e6255d6a6b44a8a9d3dcdc14cfb6"},
			"step_0002_vault-code-seal_expand":  {"a0ec45294f1f5143b8542b735dc3328105a472847f495632fdce80fbd3c3a612", "26aea3a0e490d4a59935b7791666d42ba3a2d8491fe10c95cba25dec41298c48"},
			"step_0003_vault-title-drop_expand": {"3a5e7cffb05a1e2f0434215fa079f470fe2d7721d1857f4491c22e6475fdb513", "adf56403884c4acf7bddba040ce4f09d6cdf9a138456fe4f8b93d96538d60a82"},
		}
		ctx := sealSteps("seal", "sealtenancy", false)
		ctx.SchemaChanges = append(ctx.SchemaChanges, types.SchemaChange{ID: "vault-title-drop", Kind: "drop", Entity: "Vault", Field: "title", Definition: "string, first"})
		ctx.SchemaHistory = append(ctx.SchemaHistory, types.SchemaStep{Change: "vault-title-drop", Phase: "expand"})
		ctx.Entities[0].Fields = ctx.Entities[0].Fields[1:]
		ctx.PeerConfigs = map[string]map[string]any{gen.TenancyComponent: {"runtime_role": "probe_runtime", "owner_role": "probe_owner"}}
		files, _, err := new(postgresadapter.Generator).Generate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		migrate := generated(t, files, "seal/migrate.go")
		for name, digests := range want {
			call := "RegisterEarlierDigests(" + strconv.Quote(name) + ", " + strconv.Quote(digests[0]) + ", " + strconv.Quote(digests[1]) + ")"
			if !strings.Contains(migrate, call) {
				t.Errorf("migrate.go does not hold %s", call)
			}
			body := generated(t, files, "seal/migrations/"+name+".sql")
			if strings.Contains(body, "FUNCTION stego_schema.seal_") {
				t.Errorf("the body of %s holds a function of the seal checkpoints:\n%s", name, body)
			}
			// The digest of the body is none of the earlier digests: the body changed, and the rest of the step is still
			// in it.
			sum := sha256.Sum256([]byte(body))
			if slices.Contains(digests, hex.EncodeToString(sum[:])) {
				t.Errorf("the body of %s has an earlier digest", name)
			}
		}
		for _, want := range []string{"ALTER TABLE public.\"vaults\" ADD COLUMN \"note_sealed\" bytea;", "CREATE TABLE IF NOT EXISTS stego_schema.releases", "CREATE TABLE IF NOT EXISTS stego_schema.seals",
			"INSERT INTO stego_schema.seals (change_id, expand) VALUES ('vault-note-seal', 'step_0001_vault-note-seal_expand')"} {
			if body := generated(t, files, "seal/migrations/step_0001_vault-note-seal_expand.sql"); !strings.Contains(body, want) {
				t.Errorf("the body of the expand of the seal of note does not hold %s", want)
			}
		}
		if strings.Count(migrate, "\tRegisterEarlierDigests(") != len(want) {
			t.Errorf("migrate.go registers earlier digests for %d migrations, want %d", strings.Count(migrate, "\tRegisterEarlierDigests("), len(want))
		}
	})
}

// relateService is the release of ST-SUP-RC01-OC52-T01: the owned Payout of the resource type payout with the field
// approver_id, whose schema gives payout the relation tenant, a relationship approver from the field, and relate steps
// of each, as release 2 of walkthrough 24 (P2-R2) adds them.
func relateService(ns, tenancyNS string, relates ...types.SchemaChange) gen.Context {
	ctx := runtimeService(ns, tenancyNS, []types.Entity{{Name: "Payout",
		EntityAuthorization: types.EntityAuthorization{ResourceType: "payout", Relationships: []types.Relationship{
			{Relation: "approver", SubjectSource: types.SubjectSourceField, SubjectField: "approver_id", SubjectType: "user"},
			{Relation: "owner", SubjectSource: types.SubjectSourceCaller, SubjectType: "user"}}},
		Fields: []types.Field{{Name: "amount", Type: types.FieldTypeInt64}, {Name: "approver_id", Type: types.FieldTypeString, Optional: true}}}},
		[]types.Collection{{Name: "payouts", Entity: "Payout", Operations: []types.Operation{types.OpCreate, types.OpRead}}})
	ctx.PeerConfigs = map[string]map[string]any{gen.TenancyComponent: {"runtime_role": "relate_runtime", "owner_role": "relate_owner"}}
	ctx.Authorization = &types.Authorization{Provider: "spicedb", SchemaPath: "authz/schema.zed"}
	ctx.Inputs = map[string][]byte{"authz/schema.zed": []byte("definition user {}\n\ndefinition tenant {\n\trelation approver: user\n}\n\ndefinition payout {\n\trelation tenant: tenant\n\trelation approver: user\n\trelation owner: user\n\tpermission view = owner + approver + tenant->approver\n}\n")}
	ctx.SchemaChanges = relates
	for _, change := range relates {
		ctx.SchemaHistory = append(ctx.SchemaHistory, types.SchemaStep{Change: change.ID, Phase: "expand"})
	}
	return ctx
}

func TestRelateSteps(t *testing.T) {
	t.Run("ST-SUP-RC01-OC52-T01 The expand of a relate makes its checkpoint and changes no table, and the relate job reads each row with its subject", func(t *testing.T) {
		base := relateService("relatebase", "relatebasetenancy")
		head := relateService("relate", "relatetenancy",
			types.SchemaChange{ID: "payout-tenant-relate", Kind: "relate", Entity: "Payout", Relation: "tenant"},
			types.SchemaChange{ID: "payout-approver-relate", Kind: "relate", Entity: "Payout", Relation: "approver"})
		var files [2][]gen.File
		for i, ctx := range []gen.Context{base, head} {
			out, _, err := new(postgresadapter.Generator).Generate(ctx)
			if err != nil {
				t.Fatal(err)
			}
			files[i] = out
		}
		if got, want := generated(t, files[1], "relate/migrations/001_initial.sql"), generated(t, files[0], "relatebase/migrations/001_initial.sql"); got != want {
			t.Errorf("the 001_initial of the release of the relates is not the one of the release before:\n%s", got)
		}
		// Apart from the name of the release, which is its last migration, the manifest is the one of the release before.
		release := regexp.MustCompile(`(?m)^const SchemaRelease = .*$`)
		if got, want := release.ReplaceAllString(generated(t, files[1], "relate/schema_manifest.go"), ""), release.ReplaceAllString(strings.ReplaceAll(generated(t, files[0], "relatebase/schema_manifest.go"), "relatebase", "relate"), ""); got != want {
			t.Errorf("the manifest of the release of the relates is not the one of the release before:\n%s", got)
		}
		tenant := generated(t, files[1], "relate/migrations/step_0001_payout-tenant-relate_expand.sql")
		for _, want := range []string{
			"CREATE TABLE IF NOT EXISTS stego_schema.releases",
			"CREATE TABLE IF NOT EXISTS stego_schema.seals",
			"INSERT INTO stego_schema.seals (change_id, expand) VALUES ('payout-tenant-relate', 'step_0001_payout-tenant-relate_expand') ON CONFLICT (change_id) DO NOTHING;",
		} {
			if !strings.Contains(tenant, want) {
				t.Errorf("the expand of the relate of tenant does not hold %s:\n%s", want, tenant)
			}
		}
		if strings.Contains(tenant, "ALTER TABLE public.") || strings.Contains(tenant, "FUNCTION stego_schema.seal_") {
			t.Errorf("the expand of the relate changes a table or holds a function of the seal checkpoints:\n%s", tenant)
		}
		migrate := generated(t, files[1], "relate/migrate.go")
		for _, want := range []string{`RegisterSealStep("step_0001_payout-tenant-relate_expand",`, `RegisterSealStep("step_0002_payout-approver-relate_expand",`, "RegisterSealCheckpoints("} {
			if !strings.Contains(migrate, want) {
				t.Errorf("migrate.go of the release of the relates does not hold %s", want)
			}
		}
		relates := generated(t, files[1], "relate/relate_changes.go")
		for _, want := range []string{
			`{Change: "payout-tenant-relate", Entity: "Payout", ResourceType: "payout", Relation: "tenant", Subject: command.RelateTenant, SubjectType: "", Resource: false,`,
			`Select: "SELECT \"id\"::text, NULL::text FROM public.\"payouts\" WHERE \"tenant_id\" = $1 AND ($2::uuid IS NULL OR \"id\" > $2::uuid) ORDER BY \"id\" LIMIT $3 FOR SHARE"`,
			`{Change: "payout-approver-relate", Entity: "Payout", ResourceType: "payout", Relation: "approver", Subject: command.RelateField, SubjectType: "user", Resource: false,`,
			`Select: "SELECT \"id\"::text, \"approver_id\"::text FROM public.\"payouts\" WHERE \"tenant_id\" = $1 AND ($2::uuid IS NULL OR \"id\" > $2::uuid) ORDER BY \"id\" LIMIT $3 FOR SHARE"`,
		} {
			if !strings.Contains(relates, want) {
				t.Errorf("relate_changes.go does not hold %s:\n%s", want, relates)
			}
		}
		if slices.ContainsFunc(files[0], func(f gen.File) bool { return strings.HasSuffix(f.Path, "relate_changes.go") }) {
			t.Error("the release with no relate step has a relate_changes.go")
		}
		// A relationship whose subject is the caller, and a relation that the runtime does not write, are refused.
		for relation, want := range map[string]string{"owner": "caller of the create", "auditor": "writes no tuple of the relation auditor"} {
			bad := relateService("relatebad", "relatebadtenancy", types.SchemaChange{ID: "payout-bad-relate", Kind: "relate", Entity: "Payout", Relation: relation})
			if _, _, err := new(postgresadapter.Generator).Generate(bad); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("a relate of %s gave %v, want a refusal that says %q", relation, err, want)
			}
		}
	})
}
