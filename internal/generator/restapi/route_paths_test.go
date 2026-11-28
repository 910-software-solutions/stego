package restapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/910-software-solutions/stego/internal/types"
)

func TestRejectUnsafeCollectionPathsBeforeRendering(t *testing.T) {
	for _, prefix := range []string{
		"users", "/", "//users", "/users/", "/a/../users", "/a/./users", "/a//users",
		"/users?admin=true", "/users#fragment", "/users/%2f", "/users/%7Bid%7D", "/users\\next",
		"/users\nnext", "/users\"next", "/café", "/users/{id}", "/users/{tail...}", "/users/{$}",
		"/users/{bad-name}", "/users/{missing", "/users/{same}/{same}", "/users/a{name}",
	} {
		t.Run(strconv.Quote(prefix), func(t *testing.T) {
			ctx := basicContext()
			ctx.Collections[0].PathPrefix = prefix
			files, wiring, err := (&Generator{}).Generate(onRuntime(ctx))
			if err == nil {
				_, failure := basePathMux(wiring)
				t.Fatalf("accepted unsafe collection path; router result: %v", failure)
			}
			if len(files) != 0 || wiring != nil || !strings.Contains(err.Error(), "path") {
				t.Fatal("invalid path produced output or an unrelated error", err)
			}
		})
	}
}

func TestRejectSemanticRouteConflictsBeforeRendering(t *testing.T) {
	for _, pair := range [][2]string{{"/owners/{left}/users", "/owners/{right}/users"}, {"/{owner}/users", "/owners/{user}"}} {
		t.Run(pair[0]+"_"+pair[1], func(t *testing.T) {
			ctx := basicContext()
			ctx.BasePath = "/api/v1"
			ctx.Collections = []types.Collection{
				{Name: "first", Entity: "User", PathPrefix: pair[0], Operations: []types.Operation{types.OpList}},
				{Name: "second", Entity: "User", PathPrefix: pair[1], Operations: []types.Operation{types.OpList}},
			}
			files, wiring, err := (&Generator{}).Generate(onRuntime(ctx))
			if err == nil {
				_, failure := basePathMux(wiring)
				t.Fatalf("accepted conflicting routes %v; router result: %v", pair, failure)
			}
			if len(files) != 0 || wiring != nil || !strings.Contains(err.Error(), "route") {
				t.Fatal("conflicting routes produced output or an unrelated error", err)
			}
		})
	}
}

func TestRejectCollectionPathsHiddenByDiscovery(t *testing.T) {
	for _, prefix := range []string{"/openapi", "/openapi.html", "/{name}"} {
		t.Run(prefix, func(t *testing.T) {
			ctx := basicContext()
			ctx.Collections[0].PathPrefix = prefix
			ctx.Collections[0].Operations = []types.Operation{types.OpList}
			files, wiring, err := (&Generator{}).Generate(onRuntime(ctx))
			if err == nil || len(files) != 0 || wiring != nil {
				t.Fatal("accepted a collection route hidden by public discovery", prefix, err)
			}
		})
	}
}

func TestRejectEquivalentOpenAPIPathsAcrossMethods(t *testing.T) {
	ctx := basicContext()
	ctx.Collections = []types.Collection{
		{Name: "first", Entity: "User", PathPrefix: "/owners/{left}/users", Operations: []types.Operation{types.OpList}},
		{Name: "second", Entity: "User", PathPrefix: "/owners/{right}/users", Operations: []types.Operation{types.OpCreate}},
	}
	files, wiring, err := (&Generator{}).Generate(onRuntime(ctx))
	if err == nil || len(files) != 0 || wiring != nil {
		t.Fatal("accepted equivalent OpenAPI paths with different parameter names", err)
	}
}

func TestScopedCollectionRoutesKeepParameterNames(t *testing.T) {
	for _, prefix := range []string{"/people", "/orgs/{org_id}/people", "/orgs/{tenant}/people"} {
		t.Run(prefix, func(t *testing.T) {
			ctx := basicContext()
			ctx.BasePath = "/api/v1"
			ctx.Entities = append(ctx.Entities, types.Entity{Name: "Organization", Fields: []types.Field{{Name: "name", Type: types.FieldTypeString}}})
			ctx.Collections[0].Scope = map[string]string{"org_id": "Organization"}
			ctx.Collections[0].PathPrefix = prefix
			ctx.Collections = append(ctx.Collections, types.Collection{Name: "organizations", Entity: "Organization", PathPrefix: "/orgs", Operations: []types.Operation{types.OpRead}})
			files, wiring, err := (&Generator{}).Generate(onRuntime(ctx))
			if strings.Contains(prefix, "{tenant}") {
				// The runtime data path checks only the scope field as a path parameter.
				if err == nil || !strings.Contains(err.Error(), "path parameter tenant") {
					t.Fatal("accepted a path parameter that is not the scope field", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			mux, failure := basePathMux(wiring)
			if failure != nil {
				t.Fatal("valid scoped routes failed registration", failure)
			}
			handler := findFileContent(t, files, "internal/api/users_handler.go")
			if !strings.Contains(handler, `r.PathValue("org_id")`) {
				t.Fatal("handler lost the declared parameter")
			}
			var spec struct {
				Paths map[string]json.RawMessage `json:"paths"`
			}
			if err := json.Unmarshal([]byte(findFileContent(t, files, "internal/api/openapi.json")), &spec); err != nil {
				t.Fatal(err)
			}
			pattern := "/api/v1/orgs/{org_id}/people"
			if _, ok := spec.Paths[pattern]; !ok {
				t.Fatal("OpenAPI lost the declared scoped path", pattern)
			}
			for _, methodPath := range [][2]string{{"GET", "/api/v1/orgs/one/people"}, {"POST", "/api/v1/orgs/one/people"}, {"GET", "/api/v1/orgs/one/people/two"}, {"PUT", "/api/v1/orgs/one/people/two"}, {"DELETE", "/api/v1/orgs/one/people/two"}} {
				response := httptest.NewRecorder()
				mux.ServeHTTP(response, httptest.NewRequest(methodPath[0], methodPath[1], nil))
				if response.Code != http.StatusNoContent {
					t.Fatal("scoped route did not match", methodPath, response.Code)
				}
			}
		})
	}
}

// legacyMuxMarker is set to 1 in the environment of the child process of ST-SUP-RC20-OC33-T02, which runs with
// GODEBUG=httpmuxgo121=1. TestMain does not fill the module cache of localtest for that child.
const legacyMuxMarker = "STEGO_TEST_LEGACY_MUX"

// legacyMuxRun is the -test.run argument of that child process. TestMain skips the fill only when the binary also has
// this argument, so a marker that is set for a whole run still fills the cache.
const legacyMuxRun = "-test.run=^TestRouteValidationRejectsLegacyMuxMode$"

func TestRouteValidationRejectsLegacyMuxMode(t *testing.T) {
	t.Run("ST-SUP-RC20-OC33-T02 Route validation requires the current HTTP pattern rules", func(t *testing.T) {
		const marker = legacyMuxMarker
		if os.Getenv(marker) == "1" {
			files, wiring, err := (&Generator{}).Generate(basicContext())
			if err == nil || len(files) != 0 || wiring != nil || !strings.Contains(err.Error(), "HTTP pattern semantics") {
				t.Fatal("legacy mux mode bypassed route validation", err)
			}
			return
		}
		// The child is a new process of the test binary; a busy runner can take more than 10 seconds to start it and
		// run it, so it gets two minutes (still bounded).
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		command := exec.CommandContext(ctx, os.Args[0], legacyMuxRun, "-test.count=1")
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "GODEBUG=") && !strings.HasPrefix(entry, marker+"=") {
				command.Env = append(command.Env, entry)
			}
		}
		command.Env = append(command.Env, "GODEBUG=httpmuxgo121=1", marker+"=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("legacy mux regression failed: %v\n%s", err, output)
		}
	})
}
