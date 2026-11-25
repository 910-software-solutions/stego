package compiler

import (
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/generator/cliapplication"
	"github.com/jsell-rh/stego/internal/generator/controller"
	"github.com/jsell-rh/stego/internal/generator/grpcapplication"
	"github.com/jsell-rh/stego/internal/generator/httpapplication"
	"github.com/jsell-rh/stego/internal/generator/jwtauth"
	"github.com/jsell-rh/stego/internal/generator/kafkaproducer"
	"github.com/jsell-rh/stego/internal/generator/keycloakprovider"
	"github.com/jsell-rh/stego/internal/generator/kubernetesclient"
	"github.com/jsell-rh/stego/internal/generator/outbox"
	"github.com/jsell-rh/stego/internal/generator/postgresadapter"
	"github.com/jsell-rh/stego/internal/generator/postgresclient"
	"github.com/jsell-rh/stego/internal/generator/restapi"
	"github.com/jsell-rh/stego/internal/generator/rhssoauth"
	"github.com/jsell-rh/stego/internal/types"
)

func TestLibraryGeneratorsCheckGoPackageNames(t *testing.T) {
	for name, generator := range map[string]gen.Generator{
		"keycloak": new(keycloakprovider.Generator), "controller": new(controller.Generator), "grpc": new(grpcapplication.ProcessGenerator), "http": new(httpapplication.Generator), "jwt": new(jwtauth.Generator), "kafka": new(kafkaproducer.Generator), "kubernetes": new(kubernetesclient.Generator), "outbox": new(outbox.Generator), "storage": new(postgresadapter.Generator), "postgres client": new(postgresclient.Generator), "rest": new(restapi.Generator), "sso": new(rhssoauth.Generator),
	} {
		t.Run(name, func(t *testing.T) {
			ctx := gen.Context{ModuleName: "example.com/names", OutDirName: "out", OutputNamespace: "go-services/worker", AuthPackage: "example.com/names/out/auth", PeerNamespaces: map[string]string{"jwt-auth": "auth", "http-application": "application", "outbox": "queue", "command": "commands", "query": "queries"}, Entities: []types.Entity{{Name: "Record", Fields: []types.Field{{Name: "label", Type: types.FieldTypeString}}}}, Collections: []types.Collection{{Name: "records", Entity: "Record", Operations: []types.Operation{types.OpRead}}}, ComponentConfig: map[string]any{}}
			if name == "http" {
				ctx.ComponentConfig["factory_package"] = "domain"
			}
			if name == "storage" || name == "rest" {
				// The runtime data path: tables from the declaration as SQL, for a pooled tenancy.
				ctx.ComponentConfig["schema_generation"] = "fresh-v1"
				ctx.Tenancy = &gen.Tenancy{Key: "tenant_id"}
				ctx.PeerNamespaces["tenancy"] = "tenancy"
				if name == "rest" {
					delete(ctx.ComponentConfig, "schema_generation")
					ctx.Authorization = &types.Authorization{Provider: "spicedb", SchemaPath: "authz/schema.zed", SubjectFromIdentity: "user"}
					ctx.Entities[0].ResourceType = "record"
					ctx.Entities[0].Permissions = map[types.Operation]string{types.OpRead: "view", types.OpList: "view"}
					// The command and rest-api components get the schema among their inputs (ST-SUP-Q118).
					ctx.Inputs = map[string][]byte{"authz/schema.zed": []byte("definition user {}\n\ndefinition record {\n\trelation viewer: user\n\tpermission view = viewer\n}\n")}
				}
			}
			if name == "grpc" {
				// The protobuf and process generators of grpc-processes: grpc-application serves only declared
				// commands and queries (ST-SUP-RC07-OC19).
				ctx.ComponentConfig["proto_files"] = []any{map[string]any{"path": "api.proto", "import_path": "api.proto"}}
				ctx.ComponentConfig["processes"] = []any{map[string]any{"name": "records", "factory_package": "factory"}}
				ctx.PeerNamespaces["otel-tracing"] = "telemetry"
				ctx.Inputs = map[string][]byte{"api.proto": []byte(`syntax="proto3";package sample;message Record{}`),
					"factory/rpc.go": []byte("package factory\nimport (\n\t\"context\"\n\tprocess \"example.com/names/out/go-services/worker/process\"\n)\nfunc Open(ctx context.Context) (process.Application, error) { return nil, nil }\n")}
			}
			validator, ok := generator.(gen.ContextValidator)
			if !ok {
				t.Fatal("library has no preflight")
			}
			for _, namespace := range []string{"bad-name", "nested/type", "nested/_", "nested/main", "nested/9worker", "nested/café"} {
				ctx.OutputNamespace = namespace
				checked := validator.ValidateContext(ctx)
				if checked == nil {
					t.Fatal("invalid library passed preflight", namespace)
				}
				files, wiring, err := generator.Generate(ctx)
				if err == nil || err.Error() != checked.Error() || files != nil || wiring != nil {
					t.Fatal("direct generation bypassed package check", namespace, err)
				}
			}
			ctx.OutputNamespace = "go-services/worker"
			if err := validator.ValidateContext(ctx); err != nil {
				t.Fatal("nested library rejected", err)
			}
			files, _, err := generator.Generate(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, file := range files {
				if strings.HasSuffix(file.Path, ".go") {
					if _, err := parser.ParseFile(token.NewFileSet(), file.Path, file.Content, parser.AllErrors); err != nil {
						t.Fatal("invalid generated source", file.Path, err)
					}
				}
			}
		})
	}
}

func TestCLIContainerAllowsHyphens(t *testing.T) {
	ctx := gen.Context{ModuleName: "example.com/names", OutDirName: "generated-code", OutputNamespace: "cli-tools", ComponentConfig: map[string]any{"factory_package": "domain"}}
	generator := new(cliapplication.Generator)
	if err := generator.ValidateContext(ctx); err != nil {
		t.Fatal(err)
	}
	files, _, err := generator.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if !strings.HasPrefix(file.Path, "cli-tools/") {
			t.Fatal("wrong CLI container", file.Path)
		}
		if _, err := parser.ParseFile(token.NewFileSet(), file.Path, file.Content, parser.AllErrors); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOutputDirectoryMustBeAGoImportPath(t *testing.T) {
	t.Run("ST-DECL-INV17-T The output folder and each factory package are import paths outside the output", func(t *testing.T) {
		input := snapshotTestInput(t)
		input.OutDir = filepath.Join(input.ProjectDir, "bad directory")
		input.Generators["stub-api"] = rejectedInputGenerator{t}
		result, err := Validate(input)
		if err != nil || !result.HasErrors() || !strings.Contains(FormatValidation(result), "Go import namespace") {
			t.Fatal("invalid output directory passed", result, err)
		}
		if plan, err := Reconcile(input); plan != nil || err == nil {
			t.Fatal("invalid output directory reached rendering", plan, err)
		}
		// The factory package of the cli-application component must be an import path outside the output folder.
		for _, factory := range []string{"out/factory", "out", "bad factory"} {
			input := applicationPreflightInput(t, new(cliapplication.Generator), factory)
			if result, err := Validate(input); err != nil || !result.HasErrors() {
				t.Fatal("a factory package that is not an import path outside the output passed", factory, result, err)
			}
			if plan, err := Reconcile(input); plan != nil || err == nil {
				t.Fatal("a factory package that is not an import path outside the output reached rendering", factory, plan, err)
			}
		}
		if result, err := Validate(applicationPreflightInput(t, new(cliapplication.Generator), "domain-code/factory")); err != nil || result.HasErrors() {
			t.Fatal("a factory package outside the output was refused", result, err)
		}
	})
}

func TestApplicationFactoriesRequireGoImportPaths(t *testing.T) {
	// grpc-application has no factory since its bridge to the GORM store is gone (ST-SUP-RC20-OC39, ST-SUP-RC07-OC18).
	for name, generator := range map[string]gen.Generator{"cli": new(cliapplication.Generator), "http": new(httpapplication.Generator)} {
		t.Run(name, func(t *testing.T) {
			for _, factory := range []string{"bad factory", "café/factory"} {
				input := applicationPreflightInput(t, generator, factory)
				result, err := Validate(input)
				if err != nil || !result.HasErrors() {
					t.Fatal("invalid factory import passed", factory, result, err)
				}
				if plan, err := Reconcile(input); plan != nil || err == nil {
					t.Fatal("invalid factory import rendered", factory, plan, err)
				}
			}
			input := applicationPreflightInput(t, generator, "domain-code/factory")
			if result, err := Validate(input); err != nil || result.HasErrors() {
				t.Fatal("valid factory import rejected", result, err)
			}
		})
	}
}

func TestProtobufGeneratedPackagesMustBeImportable(t *testing.T) {
	// The protobuf compiler of the gRPC generators, through grpc-processes: grpc-application serves only declared
	// commands and queries (ST-SUP-RC07-OC19).
	generator := new(grpcapplication.ProcessGenerator)
	ctx := gen.Context{ModuleName: "example.com/names", OutDirName: "out", OutputNamespace: "grpcapi", AuthPackage: "example.com/names/out/auth", PeerNamespaces: map[string]string{"jwt-auth": "auth", "otel-tracing": "telemetry"}, Inputs: map[string][]byte{"api.proto": []byte(`syntax="proto3";package sample;message Record{}`),
		"factory/rpc.go": []byte("package factory\nimport (\n\t\"context\"\n\tprocess \"example.com/names/out/grpcapi/process\"\n)\nfunc Open(ctx context.Context) (process.Application, error) { return nil, nil }\n")}}
	for _, name := range []string{"main/api.proto", "bad space/api.proto", "café/api.proto"} {
		t.Run(name, func(t *testing.T) {
			ctx.ComponentConfig = map[string]any{"processes": []any{map[string]any{"name": "records", "factory_package": "factory"}}, "proto_files": []any{map[string]any{"path": "api.proto", "import_path": name}}}
			if err := generator.ValidateContext(ctx); err == nil {
				t.Fatal("unimportable generated protobuf passed preflight", name)
			}
			if files, _, err := generator.Generate(ctx); err == nil || files != nil {
				t.Fatal("unimportable protobuf was generated", name, err)
			}
		})
	}
}

// markerGenerator writes a marker file into a folder each time it runs, and plans the file a.go in its namespace,
// so a test sees whether the compiler ran it.
type markerGenerator struct{ marker string }

func (g markerGenerator) Generate(ctx gen.Context) ([]gen.File, *gen.Wiring, error) {
	if err := os.WriteFile(g.marker, []byte("ran\n"), 0o600); err != nil {
		return nil, nil, err
	}
	return []gen.File{{Path: ctx.OutputNamespace + "/a.go", Content: []byte("package " + filepath.Base(ctx.OutputNamespace) + "\n")}}, nil, nil
}

func TestNamespaceIsAGoImportPath(t *testing.T) {
	t.Run("ST-SUP-INV13-T A namespace that is not a Go import path, or a library namespace whose last element is not a usable package name, is refused before rendering and is never rewritten", func(t *testing.T) {
		// namespaced gives a test input whose stub-store states the namespace and runs the generator given, and
		// the marker file of a markerGenerator for stub-api.
		namespaced := func(namespace string, store gen.Generator) (ReconcilerInput, string) {
			input := snapshotTestInput(t)
			path := filepath.Join(input.RegistryDir, "components", "stub-store", "component.yaml")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, path, strings.Replace(string(data), "output_namespace: internal/storage", "output_namespace: "+namespace, 1))
			marker := filepath.Join(t.TempDir(), "marker")
			input.Generators["stub-api"] = markerGenerator{marker}
			input.Generators["stub-store"] = store
			return input, marker
		}
		// A component namespace that is not a valid Go import path is refused when the registry is loaded.
		for _, namespace := range []string{"'internal/store cache'", "internal//store"} {
			input, marker := namespaced(namespace, &stubGenerator{})
			plan, err := Reconcile(input)
			written := strings.Trim(namespace, "'")
			if plan != nil || err == nil || !strings.Contains(err.Error(), "stub-store") || !strings.Contains(err.Error(), `"`+written+`"`) {
				t.Fatalf("the namespace %q was not refused by name: %v", written, err)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("a generator ran for the namespace %q", written)
			}
		}
		// A library namespace whose last element is no package name is refused by the preflight of the library,
		// before any generator runs. my-lib is a valid path element, and is not taken as my_lib or mylib.
		for _, namespace := range []string{"lib/main", "lib/type", "lib/_", "lib/my-lib"} {
			input, marker := namespaced(namespace, new(postgresclient.Generator))
			plan, err := Reconcile(input)
			if plan != nil || err == nil || !strings.Contains(err.Error(), "stub-store") || !strings.Contains(err.Error(), path.Base(namespace)) {
				t.Fatalf("the library namespace %q was not refused by name: %v", namespace, err)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("a generator ran for the library namespace %q", namespace)
			}
		}
		// A component namespace with a hyphen and a library namespace with a package name are accepted, and every
		// planned file is under exactly the namespace as written.
		input, marker := namespaced("lib/worker", new(postgresclient.Generator))
		api := filepath.Join(input.RegistryDir, "components", "stub-api", "component.yaml")
		data, err := os.ReadFile(api)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, api, strings.Replace(string(data), "output_namespace: internal/api", "output_namespace: go-services/worker", 1))
		plan, err := Reconcile(input)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(marker); err != nil {
			t.Fatal("the generator of go-services/worker did not run")
		}
		seen := map[string]bool{}
		for _, file := range plan.GeneratedFiles {
			for _, namespace := range []string{"go-services/worker/", "lib/worker/"} {
				if strings.HasPrefix(file.Path, namespace) {
					seen[namespace] = true
					if strings.HasSuffix(file.Path, ".go") {
						parsed, err := parser.ParseFile(token.NewFileSet(), file.Path, file.Content, parser.PackageClauseOnly)
						if err != nil || parsed.Name.Name != "worker" {
							t.Fatalf("%s does not hold the package worker: %v", file.Path, err)
						}
					}
				}
			}
		}
		if !seen["go-services/worker/"] || !seen["lib/worker/"] {
			t.Fatalf("the plan holds no file under go-services/worker or lib/worker: %v", plan.Files)
		}
	})
}
