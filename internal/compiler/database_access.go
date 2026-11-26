package compiler

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/format"
	"go/token"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"text/template"

	"github.com/910-software-solutions/stego/internal/gen"
)

//go:embed database_access.go.tmpl
var databaseAccessSource string

type databaseAccessObject struct {
	Component string `json:"component"`
	gen.DatabaseObject
}

var databaseIdentifier = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

func generateDatabaseAccess(wirings []ComponentWiring) ([]gen.File, error) {
	var objects []databaseAccessObject
	seen := map[string]bool{}
	schemas := map[string]bool{}
	for _, component := range wirings {
		if component.Wiring == nil {
			continue
		}
		if component.Wiring.VerifyDatabaseAccess && (!component.Wiring.NeedsDB || len(component.Wiring.DatabaseAccess) == 0) {
			return nil, fmt.Errorf("component %q requires database objects and a database resource for runtime access checks", component.Name)
		}
		for _, input := range component.Wiring.DatabaseAccess {
			object := input
			object.Privileges = append([]string{}, input.Privileges...)
			key := object.Schema + "." + object.Name
			if component.Name == "" || !databaseIdentifier.MatchString(object.Schema) || !databaseIdentifier.MatchString(object.Name) || strings.HasPrefix(object.Schema, "pg_") || object.Schema == "information_schema" || seen[key] || len(objects) >= 4096 {
				return nil, fmt.Errorf("component %q has an invalid or duplicate database object", component.Name)
			}
			if object.Kind != "table" && object.Kind != "sequence" {
				return nil, fmt.Errorf("component %q has an unsupported database object kind", component.Name)
			}
			if (len(object.Privileges) == 0 && object.Kind != "sequence") || len(object.Privileges) > 4 {
				return nil, fmt.Errorf("component %q has an invalid database privilege set", component.Name)
			}
			sort.Strings(object.Privileges)
			if object.Kind == "table" && !containsDatabasePrivilege(object.Privileges, "SELECT") {
				return nil, fmt.Errorf("component %q requires SELECT for table inspection", component.Name)
			}
			for i, privilege := range object.Privileges {
				valid := (privilege == "USAGE" || privilege == "SELECT") && object.Kind == "sequence"
				if object.Kind == "table" {
					valid = privilege == "SELECT" || privilege == "INSERT" || privilege == "UPDATE" || privilege == "DELETE"
				}
				if !valid || i > 0 && privilege == object.Privileges[i-1] {
					return nil, fmt.Errorf("component %q has an unsafe or duplicate database privilege", component.Name)
				}
			}
			seen[key], schemas[object.Schema] = true, true
			objects = append(objects, databaseAccessObject{Component: component.Name, DatabaseObject: object})
		}
	}
	if len(objects) == 0 {
		return nil, nil
	}
	sort.Slice(objects, func(i, j int) bool {
		return objects[i].Schema+"."+objects[i].Name < objects[j].Schema+"."+objects[j].Name
	})
	var names []string
	for name := range schemas {
		names = append(names, name)
	}
	sort.Strings(names)
	data := struct {
		Objects []databaseAccessObject
		Schemas []string
	}{objects, names}
	tmpl, err := template.New("database-access").Funcs(template.FuncMap{"join": strings.Join}).Parse(databaseAccessSource)
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	if err := tmpl.Execute(&output, data); err != nil {
		return nil, err
	}
	source, err := format.Source(output.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format database access: %w", err)
	}
	manifest, err := json.MarshalIndent(struct {
		Format  int                    `json:"format"`
		Objects []databaseAccessObject `json:"objects"`
	}{1, objects}, "", "  ")
	if err != nil {
		return nil, err
	}
	return []gen.File{
		{Path: gen.DatabaseAccessNamespace + "/access.go", Content: source},
		{Path: gen.DatabaseAccessNamespace + "/access.json", Content: append(manifest, '\n')},
	}, nil
}

// generateBackupContract merges the backup object declarations of all
// components into one operator manifest. A backup that skips any listed
// object leaves the restored database unusable or unverifiable.
func generateBackupContract(wirings []ComponentWiring) ([]gen.File, error) {
	type backupEntry struct {
		Component string           `json:"component"`
		Object    gen.BackupObject `json:"object"`
	}
	var entries []backupEntry
	seen := map[string]bool{}
	for _, component := range wirings {
		if component.Wiring == nil {
			continue
		}
		for _, object := range component.Wiring.BackupObjects {
			key := object.Schema + "." + object.Name
			if component.Name == "" || !databaseIdentifier.MatchString(object.Schema) || !databaseIdentifier.MatchString(object.Name) || strings.HasPrefix(object.Schema, "pg_") || object.Schema == "information_schema" || seen[key] || len(entries) >= 4096 {
				return nil, fmt.Errorf("component %q has an invalid or duplicate backup object", component.Name)
			}
			if object.Kind != "table" && object.Kind != "sequence" && object.Kind != "function" && object.Kind != "trigger" {
				return nil, fmt.Errorf("component %q has an unsupported backup object kind", component.Name)
			}
			seen[key] = true
			entries = append(entries, backupEntry{Component: component.Name, Object: object})
		}
	}
	if len(entries) == 0 {
		return nil, nil
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Object.Schema+"."+entries[i].Object.Name < entries[j].Object.Schema+"."+entries[j].Object.Name
	})
	manifest, err := json.MarshalIndent(struct {
		Format  int           `json:"format"`
		Objects []backupEntry `json:"objects"`
	}{1, entries}, "", "  ")
	if err != nil {
		return nil, err
	}
	return []gen.File{
		{Path: gen.BackupContractNamespace + "/backup.json", Content: append(manifest, '\n')},
	}, nil
}

func needsDatabaseAccessCheck(input AssemblerInput, consumed map[int]bool) bool {
	for i, component := range input.Wirings {
		if consumed[i] && component.Wiring != nil && component.Wiring.VerifyDatabaseAccess {
			return true
		}
	}
	return false
}

func containsDatabasePrivilege(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// schemaCheckCall gives the statements of main.go that run the schema check of a consumed wiring after the database
// ping and before the access check: the stage database.schema, and on a refusal the stage that the Stage function of
// the same package gives for the error, such as database.schema for a schema fault or database.access for a connection
// that did not open (ST-SUP-RC02-OC57). It gives no statement when no wiring declares a schema check.
func schemaCheckCall(input AssemblerInput, imports importResult, consumed map[int]bool, parent string) (string, error) {
	var out string
	for i, component := range input.Wirings {
		if component.Wiring == nil || component.Wiring.SchemaCheck == nil {
			continue
		}
		check := component.Wiring.SchemaCheck
		if out != "" {
			return "", fmt.Errorf("multiple components declare a schema check")
		}
		valid := func(name string) bool { return token.IsIdentifier(name) && ast.IsExported(name) }
		if !consumed[i] || !slices.Contains(component.Wiring.Imports, check.Namespace) || !valid(check.Function) || !valid(check.Stage) {
			return "", fmt.Errorf("component %q has an invalid schema check", component.Name)
		}
		name := path.Base(check.Namespace)
		if renamed := imports.Renames[i][name]; renamed != "" {
			name = renamed
		}
		out = fmt.Sprintf("\tstegoStage = \"database.schema\"\n\tif err := %s.%s(%s, db); err != nil {\n\t\tstegoStage = %s.%s(err)\n\t\treturn err\n\t}\n", name, check.Function, parent, name, check.Stage)
	}
	return out, nil
}
