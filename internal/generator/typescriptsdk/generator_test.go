package typescriptsdk

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jsell-rh/stego/internal/gen"
)

const sample = `openapi: 3.0.3
info: {title: Records, version: '1'}
paths:
  /api/prototype:
    get:
      operationId: __proto__
      responses:
        '200':
          description: Found
          content:
            application/json:
              schema: {$ref: '#/components/schemas/Record'}
  /api/records:
    post:
      operationId: createRecord
      requestBody:
        required: true
        content:
          application/json:
            schema: {$ref: '#/components/schemas/Record'}
      responses:
        '201':
          description: Created
          content:
            application/json:
              schema: {$ref: '#/components/schemas/Record'}
  /api/records/{id}:
    get:
      operationId: getRecord
      parameters:
        - {name: id, in: path, required: true, schema: {type: string}}
        - {name: search, in: query, schema: {type: string}}
      responses:
        '200':
          description: Found
          content:
            application/json:
              schema: {$ref: '#/components/schemas/Record'}
    delete:
      operationId: deleteRecord
      parameters:
        - {name: id, in: path, required: true, schema: {type: string}}
      responses:
        '204': {description: Deleted}
  /api/records/{id}/archive:
    post:
      operationId: archiveRecord
      parameters:
        - {name: id, in: path, required: true, schema: {type: string}}
        - {name: Idempotency-Key, in: header, required: true, schema: {type: string}}
        - {name: Prefer, in: header, schema: {type: string}}
      responses:
        '204': {description: Archived}
components:
  schemas:
    Record:
      type: object
      additionalProperties: false
      required: [id, name, count, enabled, server_id]
      properties:
        server_id: {type: string, readOnly: true}
        secret: {type: string, writeOnly: true}
        id: {type: string}
        name: {type: string, minLength: 1, maxLength: 200}
        count: {type: integer, minimum: 0}
        enabled: {type: boolean}
        description: {type: string, nullable: true}
        created_at: {type: string, format: date-time}
        mode: {type: string, nullable: true, enum: [ready]}
        endpoint: {type: string, format: uri}
        ref: {type: string, format: uuid}
        blob: {type: string, format: byte}
        big: {type: string, format: int64}
`

func fixture() gen.Context {
	return gen.Context{ModuleName: "example.com/records", OutDirName: "out", OutputNamespace: "sdk", ComponentConfig: map[string]any{"document": "api.yaml", "error_codes": []any{"record_name_exists"}}, Inputs: map[string][]byte{"api.yaml": []byte(sample)}}
}
func TestGeneration(t *testing.T) {
	ctx := fixture()
	g := new(Generator)
	files, w, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	again, other, err := g.Generate(ctx)
	if err != nil || !reflect.DeepEqual(files, again) || !reflect.DeepEqual(w, other) {
		t.Fatal("generation changed", err)
	}
	if len(files) != 3 {
		t.Fatal("unexpected output")
	}
	for _, change := range []struct{ old, new string }{{"type: string, minLength: 1, maxLength: 200", "type: string, pattern: '(a+)+$'"}, {"operationId: createRecord", "operationId: session"}, {"operationId: createRecord", "operationId: login"}, {"in: path", "in: cookie"}, {"format: date-time", "format: unknown"}, {"minLength: 1", "minLenght: 1"}, {"type: string, minLength: 1, maxLength: 200", "minLength: 1"}, {"schema: {type: string}", "schema: {type: string, nullable: true}"}} {
		c := fixture()
		c.Inputs["api.yaml"] = []byte(strings.ReplaceAll(sample, change.old, change.new))
		if g.ValidateContext(c) == nil {
			t.Fatalf("invalid contract accepted: %s", change.new)
		}
	}
}
func TestGeneratedRuntime(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("STEGO_REQUIRE_NODE") == "1" {
			t.Fatal("Node.js is required")
		}
		t.Skip("Node.js is unavailable")
	}
	project := t.TempDir()
	files, _, err := new(Generator).Generate(fixture())
	if err != nil {
		t.Fatal(err)
	}
	sessionFiles, err := BrowserSessionFiles("session", "/api/v1")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, sessionFiles...)
	for _, f := range files {
		name := filepath.Join(project, f.Path)
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, f.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"runtime.test.mjs", "usage.mts", "session.test.mjs", "session-usage.mts"} {
		data, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(project, name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	run := func(args ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, node, args...)
		cmd.Dir = project
		cmd.Env = append(os.Environ(), "NODE_OPTIONS=--max-old-space-size=256")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("generated SDK check: %v\n%s", err, out)
		} else {
			t.Logf("%s", out)
		}
	}
	run("--test", "runtime.test.mjs")
	run("--test", "session.test.mjs")
	tsc := os.Getenv("STEGO_TEST_TSC_FILE")
	if tsc == "" {
		tsc, _ = filepath.Abs("testdata/node_modules/typescript/lib/tsc.js")
	}
	if _, err := os.Stat(tsc); err != nil {
		if os.Getenv("STEGO_REQUIRE_NODE") == "1" {
			t.Fatal("TypeScript compiler fixture is required")
		}
		t.Log("TypeScript compiler check was not run")
		return
	}
	run(tsc, "--strict", "--noEmit", "--target", "ES2022", "--module", "NodeNext", "--moduleResolution", "NodeNext", "usage.mts")
	run(tsc, "--strict", "--noEmit", "--target", "ES2022", "--module", "NodeNext", "--moduleResolution", "NodeNext", "session-usage.mts")
}

func TestHeaderParameters(t *testing.T) {
	t.Run("ST-SUP-RC24-OC15-T02 Generation takes only the header parameters that the browser backend forwards, each under its own input name", func(t *testing.T) {
		g := new(Generator)
		c, err := compile(fixture())
		if err != nil {
			t.Fatal(err)
		}
		var headers []string
		for _, p := range c.Operations["archiveRecord"].Parameters {
			if p.In == "header" {
				headers = append(headers, p.Name+"="+p.Header)
			}
		}
		if want := []string{"idempotencyKey=Idempotency-Key", "prefer=Prefer"}; !reflect.DeepEqual(headers, want) {
			t.Errorf("the header parameters of archiveRecord are %v, want %v", headers, want)
		}
		for _, change := range []struct{ old, new string }{
			{"name: Prefer, in: header", "name: X-Tenant, in: header"},
			{"name: Prefer, in: header", "name: Authorization, in: header"},
			{"name: Prefer, in: header, schema: {type: string}", "name: Prefer, in: header, schema: {type: integer}"},
			{"- {name: Prefer, in: header, schema: {type: string}}", "- {name: Prefer, in: header, schema: {type: string}}\n        - {name: idempotencyKey, in: query, schema: {type: string}}"},
		} {
			ctx := fixture()
			ctx.Inputs["api.yaml"] = []byte(strings.Replace(sample, change.old, change.new, 1))
			if g.ValidateContext(ctx) == nil {
				t.Errorf("a document with %s is accepted", change.new)
			}
		}
	})
}

func TestServiceDocument(t *testing.T) {
	t.Run("ST-SUP-RC24-OC14-T01 With no document the browser client is made from the service's own OpenAPI document, and with neither it is refused", func(t *testing.T) {
		g := new(Generator)
		want, _, err := g.Generate(fixture())
		if err != nil {
			t.Fatal(err)
		}
		own := fixture()
		own.ComponentConfig = map[string]any{"error_codes": []any{"record_name_exists"}}
		own.Inputs, own.ServiceOpenAPI = nil, []byte(sample)
		if names, err := g.InputFiles(own.ComponentConfig); err != nil || len(names) != 0 {
			t.Fatalf("with no document the SDK names the inputs %v, %v; want none", names, err)
		}
		got, _, err := g.Generate(own)
		if err != nil {
			t.Fatalf("the client of the service's own document: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Error("the client of the service's own document differs from the client of the same document given as a file")
		}
		own.ServiceOpenAPI = nil
		if err := g.ValidateContext(own); err == nil || !strings.Contains(err.Error(), "the setting document") {
			t.Errorf("with no document and no service document the SDK gives %v, want a refusal that names the setting", err)
		}
	})
}

func TestErrorCodeConfiguration(t *testing.T) {
	g := new(Generator)
	for _, value := range []any{"wrong", []any{""}, []any{"Bad-Code"}, []any{"same", "same"}, []any{1}, []any{strings.Repeat("a", 129)}, make([]any, 65)} {
		ctx := fixture()
		ctx.ComponentConfig["error_codes"] = value
		if g.ValidateContext(ctx) == nil {
			t.Fatalf("invalid error_codes accepted: %v", value)
		}
	}
	ctx := fixture()
	delete(ctx.ComponentConfig, "error_codes")
	if _, _, err := g.Generate(ctx); err != nil {
		t.Fatal(err)
	}
}
