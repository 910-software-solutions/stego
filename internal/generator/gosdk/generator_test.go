package gosdk

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/oteltracing"
	"github.com/jsell-rh/stego/internal/localtest"
)

const sample = `openapi: 3.0.3
info: {title: Sample, version: '1'}
paths:
  /widgets/{id}:
    patch:
      operationId: patchWidget
      parameters:
        - {name: id, in: path, required: true, schema: {type: string}}
      requestBody:
        required: true
        content:
          application/json:
            schema: {$ref: '#/components/schemas/Widget'}
      responses:
        '200':
          description: Widget
          content:
            application/json:
              schema: {$ref: '#/components/schemas/Widget'}
    get:
      operationId: getWidget
      parameters:
        - {name: id, in: path, required: true, schema: {type: string}}
      responses:
        '200':
          description: Widget
          content:
            application/json:
              schema: {$ref: '#/components/schemas/Widget'}
        '404': {description: Missing}
  /uuid/{id}:
    get:
      operationId: getWithBodyUUID
      parameters:
        - {name: id, in: path, required: true, schema: {type: string, format: uuid}}
      responses:
        '204': {description: Empty}
components:
  schemas:
    Widget:
      x-sensitive: true
      type: object
      required: [id, name]
      properties:
        id: {type: string}
        name: {type: string}
        description: {type: string, nullable: true}
        enabled: {type: boolean, nullable: true}
        count: {type: integer, nullable: true}
        updated: {type: string, format: date-time, nullable: true}
        tags: {type: array, nullable: true, items: {type: string}}
`

// keptSample is a document as the rest-api component writes it for a patch of an entity with a field that a collection
// write cannot change (ST-SUP-RC20-OC15): a closed patch request with kind and an x-kept member.
const keptSample = `openapi: 3.0.3
info: {title: Kept, version: '1'}
paths:
  /widgets/{id}:
    patch:
      operationId: patchWidgets
      parameters:
        - {name: id, in: path, required: true, schema: {type: string}}
      requestBody:
        required: true
        content:
          application/json:
            schema: {$ref: '#/components/schemas/WidgetsPatchRequest'}
      responses:
        '200':
          description: Patched
          content:
            application/json:
              schema: {$ref: '#/components/schemas/Widget'}
components:
  schemas:
    Widget:
      type: object
      required: [id, name]
      properties:
        id: {type: string}
        name: {type: string}
        code: {type: string, readOnly: true}
    WidgetsPatchRequest:
      type: object
      additionalProperties: false
      properties:
        kind: {type: string, enum: [Widget]}
        name: {type: string}
        code: {type: string, x-kept: true, description: A patch cannot change this field.}
`

// strictSample is a document as the rest-api component writes it for a create (ST-SUP-RC20-OC55): a closed request
// schema with kind, an x-kept member and x-output-only members, which have no type and take null.
const strictSample = `openapi: 3.0.3
info: {title: Strict, version: '1'}
paths:
  /widgets:
    post:
      operationId: createWidget
      requestBody:
        required: true
        content:
          application/json:
            schema: {$ref: '#/components/schemas/WidgetCreateRequest'}
      responses:
        '201':
          description: Created
          content:
            application/json:
              schema: {$ref: '#/components/schemas/Widget'}
components:
  schemas:
    Widget:
      type: object
      required: [id, name]
      properties:
        id: {type: string}
        name: {type: string}
        state: {type: string, readOnly: true}
    WidgetCreateRequest:
      type: object
      additionalProperties: false
      required: [name]
      properties:
        kind: {type: string, enum: [Widget]}
        name: {type: string}
        state: {type: string, x-kept: true, description: The body may leave it out or give its default.}
        id: {nullable: true, x-output-only: true, description: The server sets this member.}
        created_time: {nullable: true, x-output-only: true, description: The server sets this member.}
`

func fixture() gen.Context {
	return gen.Context{ModuleName: "example.com/sdkprobe", OutputNamespace: "sdk", ComponentConfig: map[string]any{"document": "api.yaml"}, Inputs: map[string][]byte{"api.yaml": []byte(sample)}}
}
func TestInputBoundary(t *testing.T) {
	for _, mutation := range []struct {
		name   string
		change func(*gen.Context)
	}{
		{"remote", func(c *gen.Context) {
			c.Inputs["api.yaml"] = []byte(strings.Replace(sample, "#/components/schemas/Widget", "https://invalid.example/schema", 1))
		}},
		{"undeclared", func(c *gen.Context) {
			c.Inputs["api.yaml"] = []byte(strings.Replace(sample, "#/components/schemas/Widget", "other.yaml#/components/schemas/Widget", 1))
		}},
		{"duplicate", func(c *gen.Context) { c.Inputs["api.yaml"] = append([]byte(sample), []byte("openapi: 3.0.3\n")...) }},
		{"extension", func(c *gen.Context) { c.Inputs["api.yaml"] = append([]byte(sample), []byte("x-go-type: Unsafe\n")...) }},
		{"missing", func(c *gen.Context) { delete(c.Inputs, "api.yaml") }},
		{"cycle", func(c *gen.Context) {
			c.Inputs["api.yaml"] = []byte(strings.Replace(sample, "name: {type: string}", "name: {$ref: '#/components/schemas/Widget'}", 1))
		}},
		{"unused callback", func(c *gen.Context) { c.Inputs["api.yaml"] = []byte(sample + "  callbacks:\n    Unused: {}\n") }},
		{"missing paths", func(c *gen.Context) {
			c.Inputs["api.yaml"] = []byte("openapi: 3.0.3\ninfo: {title: Empty, version: '1'}\n")
		}},
		{"missing responses", func(c *gen.Context) {
			c.Inputs["api.yaml"] = []byte("openapi: 3.0.3\ninfo: {title: Empty, version: '1'}\npaths:\n  /widgets:\n    get: {operationId: getWidgets}\n")
		}},
		{"unknown setting", func(c *gen.Context) { c.ComponentConfig["documents"] = "api.yaml" }},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			c := fixture()
			mutation.change(&c)
			g := new(Generator)
			if err := g.ValidateContext(c); err == nil {
				t.Fatal("invalid SDK contract passed preflight")
			}
			files, _, err := g.Generate(c)
			if err == nil || len(files) != 0 {
				t.Fatal("invalid SDK contract produced files")
			}
		})
	}
}

func TestGeneratedSDK(t *testing.T) {
	for _, traced := range []bool{false, true} {
		t.Run(fmt.Sprint(traced), func(t *testing.T) { testGeneratedSDK(t, traced) })
	}
}
func testGeneratedSDK(t *testing.T, traced bool) {
	ctx := fixture()
	if traced {
		ctx.PeerNamespaces = map[string]string{"otel-tracing": "tracing"}
	}

	g := new(Generator)
	files, wiring, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	again, _, err := g.Generate(ctx)
	if err != nil || len(again) != len(files) {
		t.Fatal("repeat generation failed", err)
	}
	for i := range files {
		if files[i].Path != again[i].Path || !bytes.Equal(files[i].Bytes(), again[i].Bytes()) {
			t.Fatal("SDK generation is not deterministic")
		}
	}
	requires := wiring.GoModRequires
	if traced {
		traceFiles, traceWiring, err := new(oteltracing.Generator).Generate(gen.Context{OutputNamespace: "tracing", ServiceName: "sdk-probe"})
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, traceFiles...)
		for name, version := range traceWiring.GoModRequires {
			requires[name] = version
		}
	}
	files = append(files, gen.File{Path: "sdk/client_test.go", Content: []byte(runtimeTest)})
	files = append(files, gen.File{Path: "sdk/nullable_test.go", Content: []byte(nullableRuntimeTest)})
	root := t.TempDir()
	for _, file := range files {
		target := filepath.Join(root, file.Path)
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, file.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
	}
	var mod strings.Builder
	mod.WriteString("module example.com/sdkprobe\ngo 1.26.0\nrequire(\n")
	names := make([]string, 0, len(requires))
	for name := range requires {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&mod, "%s %s\n", name, requires[name])
	}
	mod.WriteString(")\n")
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(mod.String()), 0644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"mod", "tidy"}, {"test", "-json", "-race", "-count=1", "-timeout=60s", "./..."}, {"vet", "./..."}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = root
		cmd.Env = localtest.GoEnv()
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		output, err := cmd.Output()
		if err != nil {
			t.Fatalf("SDK compile and runtime: %v %s %s", err, output, stderr.String())
		}
		if args[0] == "test" {
			passed := map[string]bool{}
			decoder := json.NewDecoder(bytes.NewReader(output))
			for {
				var event struct{ Action, Package, Test string }
				if err := decoder.Decode(&event); err == io.EOF {
					break
				} else if err != nil {
					t.Fatal("invalid generated SDK test record:", err)
				}
				// Go reports a package with no tests as a package-level skip.
				// The SDK package and every named test must still pass.
				if event.Action == "fail" || (event.Action == "skip" && (event.Test != "" || event.Package == "example.com/sdkprobe/sdk")) {
					t.Fatal("generated SDK test did not pass:", event.Package, event.Test)
				}
				if event.Action == "pass" && event.Package == "example.com/sdkprobe/sdk" {
					passed[event.Test] = true
				}
			}
			for _, name := range []string{"", "TestHTTPSAndStatus", "TestNullableExchange"} {
				if !passed[name] {
					t.Fatal("missing generated SDK test result:", name)
				}
			}
			t.Log("generated SDK passed HTTPS, status, and nullable exchange checks")
		}
	}
	localtest.CheckSums(t, root)
}

const runtimeTest = `package sdk
import("context";"crypto/tls";"encoding/pem";"errors";"net/http";"net/http/httptest";"os";"path/filepath";"strings";"testing";"time")
func TestHTTPSAndStatus(t *testing.T){
 t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT","")
 server:=httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
  if r.Header.Get("Authorization")!="Bearer private-token"{t.Error("token missing")}
  if r.URL.Path!="/widgets/one"&&r.URL.Path!="/prefix/widgets/one"&&!strings.HasPrefix(r.URL.Path,"/widgets/"){t.Error("unexpected API path")}
  if r.URL.Path=="/widgets/missing"{w.WriteHeader(404);return}
  if r.URL.Path=="/widgets/malformed"{w.Header().Set("Content-Type","application/json");w.Write([]byte("private-token"));return}
  if r.URL.Path=="/widgets/redirect"{http.Redirect(w,r,"https://invalid.example",302);return}
  if r.URL.Path=="/widgets/large"{w.Header().Set("Content-Type","application/json");w.Write([]byte(strings.Repeat("x",(4<<20)+1)));return}
  if r.URL.Path=="/widgets/wait"{<-r.Context().Done();return}
  w.Header().Set("Content-Type","application/json");w.Write([]byte("{\"id\":\"one\",\"name\":\"Widget\"}"))
 }));server.TLS=&tls.Config{MinVersion:tls.VersionTLS13};server.StartTLS();defer server.Close()
 ca:=filepath.Join(t.TempDir(),"ca.pem");os.WriteFile(ca,pem.EncodeToMemory(&pem.Block{Type:"CERTIFICATE",Bytes:server.Certificate().Raw}),0600)
 client,err:=NewClient(Options{BaseURL:server.URL,CAFile:ca,Token:"private-token"});if err!=nil{t.Fatal(err)};defer client.Close()
 prefix,err:=NewClient(Options{BaseURL:server.URL+"/prefix",CAFile:ca,Token:"private-token"});if err!=nil{t.Fatal(err)};defer prefix.Close()
 t.Run("ST-SUP-RC06-OC01-T01 The emitted SDK client keeps one verified origin and path prefix",func(t *testing.T){
  if bad,err:=NewClient(Options{BaseURL:strings.Replace(server.URL,"https:","http:",1),Token:"private-token"});err==nil{bad.Close();t.Fatal("SDK accepted plaintext")}
  if bad,err:=NewClient(Options{BaseURL:server.URL,Token:"private-token"});err==nil{defer bad.Close();if _,err:=bad.GetWidgetWithResponse(context.Background(),"one");err==nil{t.Fatal("SDK accepted an untrusted certificate")}}
  if _,err:=prefix.GetWidgetWithResponse(context.Background(),"one");err!=nil{t.Fatal("SDK path prefix failed",err)}
 })
 t.Run("ST-SUP-RC06-OC02-T01 The emitted SDK client follows no redirect",func(t *testing.T){
  for _,id:=range []string{"redirect"}{if _,err:=client.GetWidgetWithResponse(context.Background(),id);err==nil||strings.Contains(err.Error(),"private-token"){t.Fatal("unsafe request or error",err)}}
 })
 t.Run("ST-SUP-RC06-OC03-T01 The emitted SDK client sends its bearer token and hides its transport",func(t *testing.T){
  got,err:=client.GetWidgetWithResponse(context.Background(),"one");if err!=nil||got.JSON200==nil||got.JSON200.Id!="one"{t.Fatal("typed SDK read failed",err)}
 })
 t.Run("ST-SUP-RC06-OC04-T01 A failed SDK call keeps its status and hides its secrets",func(t *testing.T){
  denied,err:=client.GetWidgetWithResponse(context.Background(),"missing");if err!=nil||denied.StatusCode()!=404{t.Fatal("SDK lost status",err)}
  for _,id:=range []string{"large","malformed","../private-token"}{if _,err:=client.GetWidgetWithResponse(context.Background(),id);err==nil||strings.Contains(err.Error(),"private-token"){t.Fatal("unsafe request or error",err)}}
 })
 t.Run("ST-SUP-RC06-OC05-T01 The emitted SDK client honours a shorter caller deadline",func(t *testing.T){
  short,cancel:=context.WithTimeout(context.Background(),30*time.Millisecond);defer cancel()
  if _,err:=client.GetWidgetWithResponse(short,"wait");!errors.Is(err,context.DeadlineExceeded){t.Fatal("SDK lost caller deadline",err)}
 })
 t.Run("ST-SUP-RC06-OC06-T01 The emitted SDK client bounds its active calls",func(t *testing.T){
  held:=0;for held<17&&prefix.acquire(context.Background())==nil{held++}
  for i:=0;i<held;i++{prefix.release()}
  if held!=16{t.Fatal("SDK client did not refuse the seventeenth active call",held)}
 })
 t.Run("ST-SUP-RC06-OC07-T01 Close bounds its wait and ends the SDK client",func(t *testing.T){
  if err:=prefix.acquire(context.Background());err!=nil{t.Fatal(err)}
  start:=time.Now();prefix.Close();if time.Since(start)>2*time.Second{t.Fatal("SDK close did not bound pending caller work")};prefix.release()
  client.Close();if _,err:=client.GetWidgetWithResponse(context.Background(),"one");err==nil{t.Fatal("closed SDK client accepted a request")}
 })
}
`

func TestCapturedReferenceNames(t *testing.T) {
	ctx := fixture()
	ctx.ComponentConfig["references"] = []any{"schemas/api.a.yaml", "schemas/api.b.yaml"}
	ctx.Inputs["api.yaml"] = []byte(strings.Replace(sample, "name: {type: string}", "name: {$ref: 'schemas/api.a.yaml#/components/schemas/Value'}\n        other: {$ref: 'schemas/api.b.yaml#/components/schemas/Value'}", 1))
	ctx.Inputs["schemas/api.a.yaml"] = []byte("components:\n  schemas:\n    Value: {type: string}\n")
	ctx.Inputs["schemas/api.b.yaml"] = []byte("components:\n  schemas:\n    Value: {type: integer}\n")
	g := new(Generator)
	files, _, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		next, _, err := g.Generate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for j := range files {
			if !bytes.Equal(files[j].Bytes(), next[j].Bytes()) {
				t.Fatal("reference naming is not deterministic")
			}
		}
	}
	doc, err := loadDocument(ctx)
	if err != nil {
		t.Fatal(err)
	}
	props := doc.Components.Schemas["Widget"].Value.Properties
	if props["name"].Ref == props["other"].Ref || !props["name"].Value.Type.Is("string") || !props["other"].Value.Type.Is("integer") {
		t.Fatal("captured schemas were merged")
	}
}

func TestGeneratedNameConflicts(t *testing.T) {
	for _, source := range []string{
		strings.ReplaceAll(sample, "Widget", "Options"),
		strings.Replace(sample, "name: {type: string}", "foo-bar: {type: string}\n        foo_bar: {type: string}\n        name: {type: string}", 1),
	} {
		ctx := fixture()
		ctx.Inputs["api.yaml"] = []byte(source)
		files, _, err := new(Generator).Generate(ctx)
		if err == nil || len(files) != 0 {
			t.Fatal("conflicting SDK names produced files")
		}
	}
}

func TestServiceDocument(t *testing.T) {
	t.Run("ST-SUP-RC06-OC09-T01 With no document the client is made from the service's own OpenAPI document, and with neither it is refused", func(t *testing.T) {
		given := fixture()
		want, _, err := new(Generator).Generate(given)
		if err != nil {
			t.Fatal(err)
		}
		own := gen.Context{ModuleName: given.ModuleName, OutputNamespace: given.OutputNamespace, ComponentConfig: map[string]any{}, ServiceOpenAPI: []byte(sample)}
		if names, err := new(Generator).InputFiles(own.ComponentConfig); err != nil || len(names) != 0 {
			t.Fatalf("with no document the SDK names the inputs %v, %v; want none", names, err)
		}
		got, wiring, err := new(Generator).Generate(own)
		if err != nil {
			t.Fatalf("the client of the service's own document: %v", err)
		}
		if len(got) != len(want) {
			t.Fatalf("the client of the service's own document has %d files, want %d", len(got), len(want))
		}
		for i := range got {
			if got[i].Path != want[i].Path || !bytes.Equal(got[i].Bytes(), want[i].Bytes()) {
				t.Errorf("the file %s differs from the client of the same document given as a file", got[i].Path)
			}
		}
		// A module is required only when a generated file imports it, so go mod tidy keeps each requirement.
		for module := range wiring.GoModRequires {
			found := false
			for _, f := range got {
				found = found || bytes.Contains(f.Bytes(), []byte(`"`+module))
			}
			if !found {
				t.Errorf("the SDK requires %s, which no generated file imports", module)
			}
		}
		own.ServiceOpenAPI = nil
		if err := new(Generator).ValidateContext(own); err == nil || !strings.Contains(err.Error(), "the setting document, or the rest-api component") {
			t.Errorf("with no document and no service document the SDK gives %v, want a refusal that names the setting", err)
		}
	})
	t.Run("ST-SUP-RC20-OC15-T05 The Go client is made from a service document whose patch request marks a member x-kept", func(t *testing.T) {
		own := gen.Context{ModuleName: "example.com/sdkprobe", OutputNamespace: "sdk", ComponentConfig: map[string]any{}, ServiceOpenAPI: []byte(keptSample)}
		if err := new(Generator).ValidateContext(own); err != nil {
			t.Fatalf("the document of a rest-api service with a kept field is refused: %v", err)
		}
		if files, _, err := new(Generator).Generate(own); err != nil || len(files) == 0 {
			t.Fatalf("the client of a document with an x-kept member: %d files, %v", len(files), err)
		}
		// Only the marks of the rest-api document are taken: another extension is still refused.
		own.ServiceOpenAPI = []byte(strings.Replace(keptSample, "x-kept: true", "x-go-type: Unsafe", 1))
		if err := new(Generator).ValidateContext(own); err == nil {
			t.Error("a document with the extension x-go-type passed preflight")
		}
	})
	t.Run("ST-SUP-RC20-OC55-T03 The Go client is made from a service document whose closed request schemas hold x-kept and x-output-only members", func(t *testing.T) {
		own := gen.Context{ModuleName: "example.com/sdkprobe", OutputNamespace: "sdk", ComponentConfig: map[string]any{}, ServiceOpenAPI: []byte(strictSample)}
		if err := new(Generator).ValidateContext(own); err != nil {
			t.Fatalf("the document of a rest-api service with closed request schemas is refused: %v", err)
		}
		files, _, err := new(Generator).Generate(own)
		if err != nil || len(files) == 0 {
			t.Fatalf("the client of a document with closed request schemas: %d files, %v", len(files), err)
		}
		// Only the marks of the rest-api document are taken: another extension is still refused.
		own.ServiceOpenAPI = []byte(strings.Replace(strictSample, "x-kept: true", "x-go-type: Unsafe", 1))
		if err := new(Generator).ValidateContext(own); err == nil {
			t.Error("a document with the extension x-go-type passed preflight")
		}
	})
	t.Run("ST-SUP-RC06-OC10-T01 The client sends the idempotency key that its caller gives", func(t *testing.T) {
		files, _, err := new(Generator).Generate(fixture())
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if strings.HasSuffix(f.Path, "internal/transport/client.go") && strings.Contains(string(f.Bytes()), `"idempotency-key",`) {
				t.Errorf("the transport of the SDK refuses the Idempotency-Key header that its caller gives")
			}
		}
	})
}

func TestRequiredSecretMember(t *testing.T) {
	t.Run("ST-SUP-RC06-OC09-T03 A required secret member of a request is a plain field, and an optional one stays a pointer", func(t *testing.T) {
		document := secretDocument()
		ctx := gen.Context{ModuleName: "example.com/sdkprobe", OutputNamespace: "sdk", ComponentConfig: map[string]any{}, ServiceOpenAPI: []byte(document)}
		files, _, err := new(Generator).Generate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var wire string
		for _, f := range files {
			if strings.HasSuffix(f.Path, "internal/wire/api.go") {
				wire = string(f.Bytes())
			}
		}
		create := wire[strings.Index(wire, "type PersonCreateRequest struct"):]
		create = create[:strings.Index(create, "\n}\n")]
		if !strings.Contains(create, " string `json:\"pin,omitempty\"") || strings.Contains(create, "*string `json:\"pin") {
			t.Errorf("the required secret pin of the create is not a plain string:\n%s", create)
		}
		update := wire[strings.Index(wire, "type UpdatePersonJSONBody struct"):]
		update = update[:strings.Index(update, "\n}\n")]
		if !strings.Contains(update, "*string `json:\"pin,omitempty\"") {
			t.Errorf("the optional secret pin of the update is not a pointer:\n%s", update)
		}
	})
}

// secretDocument is the sample document with a create whose request schema requires the writeOnly member pin, and an
// update whose inline body has pin optional.
func secretDocument() string {
	document := strings.Replace(sample, "  /uuid/{id}:\n", `  /people:
    post:
      operationId: createPerson
      requestBody:
        required: true
        content:
          application/json:
            schema: {$ref: '#/components/schemas/PersonCreateRequest'}
      responses:
        '201': {description: Created}
  /people/{id}:
    put:
      operationId: updatePerson
      parameters:
        - {name: id, in: path, required: true, schema: {type: string}}
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [name]
              properties:
                name: {type: string}
                pin: {type: string, writeOnly: true}
      responses:
        '200': {description: Updated}
  /uuid/{id}:
`, 1)
	document = strings.Replace(document, "components:\n  schemas:\n", `components:
  schemas:
    PersonCreateRequest:
      type: object
      required: [name, pin]
      properties:
        name: {type: string}
        pin: {type: string, writeOnly: true}
`, 1)
	return document
}

const redactionTest = `package sdk
import ("bytes";"encoding/json";"fmt";"log/slog";"strings";"testing")
func TestRedaction(t *testing.T) {
	pin := "7310"
	create := PersonCreateRequest{Name: "Ann", Pin: pin}
	update := UpdatePersonJSONBody{Name: "Ann", Pin: &pin}
	var logged bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logged, nil))
	logger.Info("request", "create", create, "update", update, "pointer", &create)
	for _, text := range []string{fmt.Sprint(create), fmt.Sprintf("%+v", create), fmt.Sprintf("%#v", create), fmt.Sprintf("%v", &create), fmt.Sprintf("%+v", update), fmt.Sprintf("%#v", &update), logged.String()} {
		if strings.Contains(text, pin) || !strings.Contains(text, "[redacted]") || !strings.Contains(text, "Ann") {
			t.Errorf("a printed request shows the secret or hides the rest: %s", text)
		}
	}
	body, err := json.Marshal(create)
	if err != nil || !strings.Contains(string(body), pin) {
		t.Errorf("the JSON body of the request lost the secret: %s %v", body, err)
	}
	if create.Pin != pin || *update.Pin != pin {
		t.Error("printing changed the request")
	}
}
`

func TestPrintedSecretMember(t *testing.T) {
	t.Run("ST-SUP-RC06-OC09-T05 A printed or logged request redacts each secret member, and its JSON body keeps it", func(t *testing.T) {
		files, wiring, err := new(Generator).Generate(gen.Context{ModuleName: "example.com/sdkprobe", OutputNamespace: "sdk", ComponentConfig: map[string]any{}, ServiceOpenAPI: []byte(secretDocument())})
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, gen.File{Path: "sdk/redaction_test.go", Content: []byte(redactionTest)})
		root := t.TempDir()
		for _, file := range files {
			target := filepath.Join(root, file.Path)
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, file.Bytes(), 0644); err != nil {
				t.Fatal(err)
			}
		}
		var mod strings.Builder
		mod.WriteString("module example.com/sdkprobe\ngo 1.26.0\nrequire(\n")
		names := make([]string, 0, len(wiring.GoModRequires))
		for name := range wiring.GoModRequires {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(&mod, "%s %s\n", name, wiring.GoModRequires[name])
		}
		mod.WriteString(")\n")
		if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(mod.String()), 0644); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"mod", "tidy"}, {"vet", "./..."}, {"test", "-count=1", "-run", "TestRedaction", "./sdk/"}} {
			cmd := exec.Command("go", args...)
			cmd.Dir = root
			cmd.Env = localtest.GoEnv()
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("go %s: %v\n%s", args[0], err, output)
			}
			localtest.CheckSums(t, root)
		}
	})
}
