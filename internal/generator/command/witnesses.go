package command

import (
	"fmt"
	"strings"

	authzschema "github.com/910-software-solutions/stego/internal/declaration"
	"github.com/910-software-solutions/stego/internal/gen"
	"github.com/910-software-solutions/stego/internal/types"
)

// optsIn tells whether a command or a worker of the service declares creates_with_children (ST-DECL-INV108).
func optsIn(ctx gen.Context) bool {
	for _, c := range ctx.Commands {
		if c.CreatesWithChildren {
			return true
		}
	}
	for _, w := range ctx.Workers {
		if w.CreatesWithChildren {
			return true
		}
	}
	return false
}

// ChildWitnesses gives the witnesses that checking proves for the command or the worker name of the service, which
// declares creates_with_children, from the schema that the authorization setting names among the inputs of the
// generator (ST-DECL-INV108, ST-SUP-RP10-LC59). It refuses a declaration whose schema proves none, as checking does.
func ChildWitnesses(ctx gen.Context, name string) ([]authzschema.ChildWitness, error) {
	setting := ctx.Authorization
	if setting == nil || setting.SchemaPath == "" {
		return nil, fmt.Errorf("command: %s declares creates_with_children, and the service binds no authorization schema (ST-DECL-INV108)", name)
	}
	source, ok := ctx.Inputs[setting.SchemaPath]
	if !ok {
		return nil, fmt.Errorf("command: the schema %q that the authorization setting names is not among the inputs of the generator", setting.SchemaPath)
	}
	schema, err := authzschema.CompileAuthzSchema(setting.SchemaPath, source)
	if err != nil {
		return nil, fmt.Errorf("command: the schema %q does not compile: %w", setting.SchemaPath, err)
	}
	service := &types.ServiceDeclaration{Authorization: setting, Entities: ctx.Entities, Subscriptions: ctx.Subscriptions}
	for _, c := range ctx.Commands {
		service.Commands = append(service.Commands, c.Command)
	}
	for _, w := range ctx.Workers {
		service.Workers = append(service.Workers, w.Worker)
	}
	witnesses := authzschema.ChildWitnesses(service, schema, name)
	if len(witnesses) == 0 {
		return nil, fmt.Errorf("command: %s declares creates_with_children, and the schema proves no decision of a reference among the entities in its writes (ST-DECL-INV108)", name)
	}
	return witnesses, nil
}

// WitnessesSpec gives the fields CreatesWithChildren and Witnesses of a generated CommandSpec or WorkerSpec, one
// line for each witness, which the runtime freezes when the handle is bound (ST-SUP-RP10-LC59).
func WitnessesSpec(witnesses []authzschema.ChildWitness) string {
	var b strings.Builder
	b.WriteString("\t// A reference to a row that a create of the same attempt wrote is decided by the record of that create when\n" +
		"\t// one of these witnesses, which checking proved from the schema, matches it (ST-DECL-INV108, ST-SUP-RP10-LC59).\n")
	b.WriteString("\tCreatesWithChildren: true,\n\tWitnesses: []command.Witness{\n")
	for _, w := range witnesses {
		fmt.Fprintf(&b, "\t\t{Entity: %q, Field: %q, Subject: %q, Permission: %q, Kind: %q, Relation: %q", w.Entity, w.Field, w.Subject, w.Permission, w.Kind, w.Relation)
		if w.Decided != "" {
			fmt.Fprintf(&b, ", Decided: %q", w.Decided)
		}
		b.WriteString("},\n")
	}
	b.WriteString("\t},\n")
	return b.String()
}
