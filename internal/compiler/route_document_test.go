package compiler

import (
	"bytes"
	"testing"

	"github.com/910-software-solutions/stego/internal/gen"
)

// routeDocuments is a rest-api generator that gives fixed documents: rest for the rest-api context and routes for the
// context of the command component, and counts the calls.
type routeDocuments struct {
	rest, routes []byte
	calls        int
}

func (*routeDocuments) Generate(gen.Context) ([]gen.File, *gen.Wiring, error) {
	return nil, &gen.Wiring{}, nil
}
func (r *routeDocuments) OpenAPIDocument(gen.Context) ([]byte, error) { r.calls++; return r.rest, nil }
func (r *routeDocuments) RouteOpenAPIDocument(ctx gen.Context) ([]byte, error) {
	r.calls++
	if ctx.OutputNamespace != "commands" {
		return nil, nil
	}
	return r.routes, nil
}

func TestServiceRouteDocument(t *testing.T) {
	t.Run("ST-SUP-RC06-OC09-T04 With no rest-api the clients and the command component get the document of the command and query routes", func(t *testing.T) {
		provider := &routeDocuments{rest: []byte("rest"), routes: []byte("routes")}
		input := ReconcilerInput{Generators: map[string]gen.Generator{restAPIComponent: provider}}
		resolved := &resolvedCompilation{Contexts: map[string]gen.Context{
			commandComponent:  {OutputNamespace: "commands"},
			"go-sdk":          {ComponentConfig: map[string]any{}},
			"cli-application": {ComponentConfig: map[string]any{}},
			"typescript-sdk":  {ComponentConfig: map[string]any{"error_codes": []any{"x"}}},
			"health-check":    {ComponentConfig: map[string]any{}},
		}}
		if err := serviceOpenAPI(input, resolved); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{commandComponent, "go-sdk", "cli-application", "typescript-sdk"} {
			if got := resolved.Contexts[name].ServiceOpenAPI; !bytes.Equal(got, provider.routes) {
				t.Errorf("with no rest-api, %s gets the document %q, want the route document", name, got)
			}
		}
		if got := resolved.Contexts["health-check"].ServiceOpenAPI; got != nil {
			t.Errorf("a component that is not a client gets the service document %q", got)
		}
		if provider.calls != 1 {
			t.Errorf("the route document is made %d times, want once", provider.calls)
		}
		// With rest-api, the clients get its document and the command component gets none.
		provider.calls = 0
		resolved = &resolvedCompilation{Contexts: map[string]gen.Context{
			restAPIComponent: {}, commandComponent: {OutputNamespace: "commands"}, "go-sdk": {ComponentConfig: map[string]any{}},
		}}
		if err := serviceOpenAPI(input, resolved); err != nil {
			t.Fatal(err)
		}
		if got := resolved.Contexts["go-sdk"].ServiceOpenAPI; !bytes.Equal(got, provider.rest) {
			t.Errorf("with rest-api, go-sdk gets %q, want the rest-api document", got)
		}
		if got := resolved.Contexts[commandComponent].ServiceOpenAPI; got != nil {
			t.Errorf("with rest-api, the command component gets the document %q", got)
		}
		// With no route, no client gets a document, so checking refuses a client that names none (ST-SUP-RC06-OC09).
		resolved = &resolvedCompilation{Contexts: map[string]gen.Context{commandComponent: {OutputNamespace: "other"}, "go-sdk": {ComponentConfig: map[string]any{}}}}
		if err := serviceOpenAPI(input, resolved); err != nil {
			t.Fatal(err)
		}
		if got := resolved.Contexts["go-sdk"].ServiceOpenAPI; got != nil {
			t.Errorf("with no route, go-sdk gets the document %q", got)
		}
	})
}
