package gosdk

import (
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/openapicontract"
)

func (*Generator) InputFiles(config map[string]any) ([]gen.InputFile, error) {
	names, err := openapicontract.InputFiles(config)
	return gen.SourceInputs(names), err
}

// loadDocument loads the document of the client, marks each required secret member of a request as a plain Go field
// (requiredSecretsPlain), and tags each secret member so its struct prints with the member redacted (secretTag).
func loadDocument(ctx gen.Context) (*openapi3.T, error) {
	doc, err := openapicontract.Load(ctx)
	if err != nil {
		return nil, err
	}
	requiredSecretsPlain(doc)
	return doc, nil
}

// skipOptionalPointer is the extension of the pinned OpenAPI backend that gives a property a Go type with no pointer,
// and extraTags the one that adds struct tags to its Go field.
const (
	skipOptionalPointer = "x-go-type-skip-optional-pointer"
	extraTags           = "x-oapi-codegen-extra-tags"
)

// secretTag is the struct tag that marks the Go field of a secret (writeOnly) member: the generated String, GoString
// and LogValue of its struct redact the field (redactionFile, security review of PR #467, L1).
const secretTag = "sdk"

// requiredSecretsPlain marks each required, writeOnly and not nullable member of an object schema of doc, in the
// components and in the request bodies, so that the backend gives it a plain Go field. The backend gives each writeOnly
// member a pointer, also a required one, so a caller could leave out a required secret such as a create's PIN, and the
// service then refuses the request (walkthrough 17). The field keeps omitempty, so an empty value is left out and the
// service refuses it, as it refuses an absent one. Only the loaded copy of the document changes.
func requiredSecretsPlain(doc *openapi3.T) {
	seen := map[*openapi3.Schema]bool{}
	var visit func(ref *openapi3.SchemaRef)
	visit = func(ref *openapi3.SchemaRef) {
		if ref == nil || ref.Value == nil || seen[ref.Value] {
			return
		}
		schema := ref.Value
		seen[schema] = true
		for _, property := range schema.Properties {
			if property == nil || property.Value == nil || property.Ref != "" || !property.Value.WriteOnly {
				continue
			}
			if property.Extensions == nil {
				property.Extensions = map[string]any{}
			}
			property.Extensions[extraTags] = map[string]any{secretTag: "secret"}
		}
		for _, name := range schema.Required {
			property := schema.Properties[name]
			if property == nil || property.Value == nil || property.Ref != "" || !property.Value.WriteOnly || property.Value.Nullable {
				continue
			}
			if property.Extensions == nil {
				property.Extensions = map[string]any{}
			}
			property.Extensions[skipOptionalPointer] = true
		}
		for _, property := range schema.Properties {
			visit(property)
		}
		visit(schema.Items)
	}
	if doc.Components != nil {
		for _, ref := range doc.Components.Schemas {
			visit(ref)
		}
	}
	if doc.Paths == nil {
		return
	}
	for _, item := range doc.Paths.Map() {
		for _, op := range item.Operations() {
			if op.RequestBody == nil || op.RequestBody.Value == nil {
				continue
			}
			for _, media := range op.RequestBody.Value.Content {
				visit(media.Schema)
			}
		}
	}
}
