package restapi

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/types"
)

func TestSecretFields(t *testing.T) {
	t.Run("ST-SUP-RC20-OC48-T02 The OpenAPI document marks a secret writeOnly, and the handlers keep a stored secret that a write leaves out", func(t *testing.T) {
		eight := 8
		ctx := gen.Context{
			Conventions: types.Convention{Layout: "flat"},
			Entities: []types.Entity{{Name: "Vault", Fields: []types.Field{
				{Name: "label", Type: types.FieldTypeString},
				{Name: "api_token", Type: types.FieldTypeString, Sensitivity: "secret", MinLength: &eight},
				{Name: "pin", Type: types.FieldTypeString, Sensitivity: "secret", CompareOnly: true, Optional: true},
			}}},
			Collections:     []types.Collection{{Name: "vaults", Entity: "Vault", Operations: []types.Operation{types.OpCreate, types.OpRead, types.OpUpdate, types.OpList}}},
			OutputNamespace: "internal/api",
		}
		files, _, err := (&Generator{}).Generate(onRuntime(ctx))
		if err != nil {
			t.Fatal(err)
		}
		var spec map[string]any
		if err := json.Unmarshal([]byte(findFileContent(t, files, "internal/api/openapi.json")), &spec); err != nil {
			t.Fatal(err)
		}
		schemas := spec["components"].(map[string]any)["schemas"].(map[string]any)
		entity := schemas["Vault"].(map[string]any)
		create := schemas["VaultCreateRequest"].(map[string]any)
		for _, name := range []string{"api_token", "pin"} {
			for _, schema := range []map[string]any{entity, create} {
				field := schema["properties"].(map[string]any)[name].(map[string]any)
				if field["writeOnly"] != true || !strings.Contains(field["description"].(string), "no response gives it back") {
					t.Errorf("the schema of %s is not writeOnly with its description: %v", name, field)
				}
			}
		}
		required := func(schema map[string]any) []string {
			var out []string
			list, _ := schema["required"].([]any)
			for _, v := range list {
				out = append(out, v.(string))
			}
			return out
		}
		if slices.Contains(required(entity), "api_token") || !slices.Contains(required(create), "api_token") {
			t.Errorf("the entity schema requires the secret, or the create request does not: %v %v", required(entity), required(create))
		}
		// An update may leave each secret out, or give it empty, so the update request requires none and holds no
		// min_length of one, which an empty value would break (walkthrough 14, W14-B4).
		update, ok := schemas["VaultUpdateRequest"].(map[string]any)
		if !ok {
			t.Fatal("the entity with a secret has no VaultUpdateRequest")
		}
		token := update["properties"].(map[string]any)["api_token"].(map[string]any)
		if slices.Contains(required(update), "api_token") || slices.Contains(required(update), "pin") || !slices.Contains(required(update), "label") || token["minLength"] != nil || token["writeOnly"] != true {
			t.Errorf("the update request requires %v and holds the secret as %v, want label alone required and the secret writeOnly with no minLength", required(update), token)
		}
		put := spec["paths"].(map[string]any)["/vaults/{id}"].(map[string]any)["put"].(map[string]any)
		if ref := put["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)["$ref"]; ref != "#/components/schemas/VaultUpdateRequest" {
			t.Errorf("the update takes the body %v, want VaultUpdateRequest", ref)
		}
		handler := findFileContent(t, files, "internal/api/vaults_handler.go")
		for _, want := range []string{"func vaultsKeepSecrets(next, stored data.Vault) data.Vault", "if !next.ApiToken.IsSet() {",
			"repo.Update(ctx, from, vaultsKeepSecrets(vaultsToData(vault), from.Value()), parents...)", `ApiToken: sealed.SecretOf(v.ApiToken)`,
			`Pin: sealed.ComparedOf(vaultsText(v.Pin))`, `ApiToken: ""`, `Pin: nil`} {
			if !strings.Contains(handler, want) {
				t.Errorf("the handler has no %s", want)
			}
		}
		data := findFileContent(t, files, "internal/api/data/entities.go")
		for _, want := range []string{"ApiToken sealed.Secret", "Pin      sealed.Compared", `command.SealAt("vaults", "api_token", id, v.ApiToken)`,
			`command.OpenAt("vaults", "api_token", &v.ID, &v.ApiToken)`, "command.HashOf(v.Pin)", "command.ComparedAt(&v.Pin)"} {
			if !strings.Contains(data, want) {
				t.Errorf("the entity declaration has no %s:\n%s", want, data)
			}
		}
		if !regexp.MustCompile(`\n\tSealed: +true,\n`).MatchString(data) {
			t.Error("the entity declaration is not marked Sealed")
		}
		if strings.Contains(data, `"api_token": v.ApiToken`) || strings.Contains(data, `"pin": v.Pin`) {
			t.Error("the audit state of the entity holds a secret field")
		}
	})
}
