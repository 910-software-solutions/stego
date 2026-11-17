package browserbackend

import (
	"fmt"
	"strings"
	"testing"
)

func TestDeclaredAPIsSetting(t *testing.T) {
	t.Run("ST-SUP-RC02-OC58-T02 Checking refuses an entry of apis whose name or prefix it cannot take", func(t *testing.T) {
		entry := func(name, prefix string) map[string]any { return map[string]any{"name": name, "prefix": prefix} }
		accepted := fixture()
		accepted.ComponentConfig["apis"] = []any{entry("fulfilment", "/api/fulfilment/v1"), entry("billing-2", "/api/billing")}
		if _, _, err := new(Generator).Generate(accepted); err != nil {
			t.Fatalf("two declared APIs were refused: %v", err)
		}
		many := []any{}
		for i := 0; i <= MaxAPIs; i++ {
			many = append(many, entry(fmt.Sprintf("api-%d", i), fmt.Sprintf("/api/n%d", i)))
		}
		for name, apis := range map[string]any{
			"no entry":                   []any{},
			"too many entries":           many,
			"not a list":                 "fulfilment",
			"an upper-case name":         []any{entry("Fulfilment", "/api/fulfilment")},
			"a name with an underscore":  []any{entry("ful_filment", "/api/fulfilment")},
			"a name of 33 characters":    []any{entry(strings.Repeat("a", 33), "/api/fulfilment")},
			"a repeated name":            []any{entry("a", "/api/a"), entry("a", "/api/b")},
			"a repeated prefix":          []any{entry("a", "/api/a"), entry("b", "/api/a")},
			"the prefix of api_prefix":   []any{entry("a", "/api/records/v1")},
			"a prefix under api_prefix":  []any{entry("a", "/api/records/v1/inner")},
			"a prefix above api_prefix":  []any{entry("a", "/api/records")},
			"a prefix under another":     []any{entry("a", "/api/a"), entry("b", "/api/a/b")},
			"a reserved prefix":          []any{entry("a", "/auth/x")},
			"a prefix with a parameter":  []any{entry("a", "/api/{id}")},
			"a prefix that is not clean": []any{entry("a", "/api/../x")},
			"the root prefix":            []any{entry("a", "/")},
			"an unknown key":             []any{map[string]any{"name": "a", "prefix": "/api/a", "url": "https://evil.example.test"}},
		} {
			ctx := fixture()
			ctx.ComponentConfig["apis"] = apis
			if _, _, err := new(Generator).Generate(ctx); err == nil {
				t.Errorf("%s: the setting apis was accepted", name)
			}
		}
		route := fixture()
		route.ComponentConfig["apis"] = []any{entry("a", "/records")}
		if _, _, err := new(Generator).Generate(route); err == nil {
			t.Error("a browser route under a declared API prefix was accepted")
		}
		local := fixture()
		local.ComponentConfig["apis"] = []any{entry("a", "/api/a")}
		if _, _, err := (&Generator{LocalApplicationPort: 9090}).Generate(local); err == nil {
			t.Error("apis with a local application was accepted")
		}
	})
}
