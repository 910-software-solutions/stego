package compiler

import (
	"slices"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/registry"
)

func TestReadWithVariables(t *testing.T) {
	t.Run("ST-DLV-UC01-R06-T06 The platform requirements leave out a variable whose runtime package the generated service does not import", func(t *testing.T) {
		pin := registry.RuntimePin{Module: "github.com/jsell-rh/stego/registry/runtime", Version: "v0.125.0", Sum: "h1:a"}
		variables := []registry.EnvironmentVariable{{Name: "DATABASE_URL"}, {Name: "STEGO_WORKER_DATABASE_URL", ReadWith: "service"}, {Name: "STEGO_KAFKA_TOPIC", ReadWith: "outbox/kafkaoutbox"}}
		names := func(files []gen.File) []string {
			var out []string
			for _, variable := range readVariables(variables, runtimePackages(files, pin)) {
				out = append(out, variable.Name)
			}
			return out
		}
		// A browser-service imports no package of the runtime: only the variable with no Read with stays.
		browser := []gen.File{{Path: "main.go", Content: []byte("package main\n\nimport \"example.com/svc/out/browser\"\n\nvar _ = browser.New\n")}, {Path: "browser/schema.sql", Content: []byte("import \"x\"\n")}}
		if got := names(browser); !slices.Equal(got, []string{"DATABASE_URL"}) {
			t.Errorf("a service that imports no runtime package lists %v, want only DATABASE_URL", got)
		}
		// A service that imports the runtime package service lists its variables, and not those of kafkaoutbox.
		service := append(slices.Clone(browser), gen.File{Path: "internal/stegoruntime/runtime.go", Content: []byte("package stegoruntime\n\nimport (\n\t\"github.com/jsell-rh/stego/registry/runtime/service\"\n\tother \"github.com/jsell-rh/stego/registry/runtime/servicex\"\n)\n")})
		if got := names(service); !slices.Equal(got, []string{"DATABASE_URL", "STEGO_WORKER_DATABASE_URL"}) {
			t.Errorf("a service that imports runtime/service lists %v, want DATABASE_URL and STEGO_WORKER_DATABASE_URL", got)
		}
		kafka := append(slices.Clone(service), gen.File{Path: "internal/outbox/runtime.go", Content: []byte("package outbox\n\nimport \"github.com/jsell-rh/stego/registry/runtime/outbox/kafkaoutbox\"\n")})
		if got := names(kafka); len(got) != 3 {
			t.Errorf("a service that imports both packages lists %v, want all three variables", got)
		}
	})
}
