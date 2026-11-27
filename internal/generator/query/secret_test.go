package query

import (
	"strings"
	"testing"

	"github.com/910-software-solutions/stego/internal/types"
)

func TestSecretFields(t *testing.T) {
	check := func(t *testing.T) {
		ctx := shopContext(t)
		for i, e := range ctx.Entities {
			if e.Name == "Order" {
				ctx.Entities[i].Fields = append(ctx.Entities[i].Fields,
					types.Field{Name: "api_token", Type: types.FieldTypeString, Sensitivity: "secret", Optional: true},
					types.Field{Name: "pin", Type: types.FieldTypeString, Sensitivity: "secret", CompareOnly: true, Optional: true})
			}
		}
		for path, content := range generated(t, ctx) {
			text := string(content)
			for _, name := range []string{"api_token", "ApiToken", "pin", "Pin "} {
				if strings.Contains(text, name) {
					t.Errorf("%s names the secret field %s", path, strings.TrimSpace(name))
				}
			}
			if strings.Contains(text, "registry/runtime/sealed") {
				t.Errorf("%s imports the sealing runtime", path)
			}
		}
	}
	t.Run("ST-SUP-RC33-OC13-T01 A query never reads, filters on or orders by a secret column", check)
	t.Run("ST-SUP-RP10-LC45-T01 Only the command handle reads a secret field: no query statement or value names one", check)
}
