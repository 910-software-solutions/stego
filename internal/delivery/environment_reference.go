package delivery

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
)

// ReferenceFile is the path, in the project folder, of the environment variable reference (ST-DLV-UC01-R07).
const ReferenceFile = "deploy/environment-variables.md"

// renderReference renders the environment variable reference of the service from the derived list alone
// (ST-DLV-UC01-R07, ST-DLV-DS01): one section for each reader in name order, and one table row for each variable it
// reads in name order. A secret variable shows its name, type, required flag, description and secret flag only,
// and never a default, an example or a value (ST-DLV-UC01-R08).
func renderReference(service string, list []AcceptedVariable) []byte {
	var out bytes.Buffer
	fmt.Fprintf(&out, "# Environment variables of the service %s\n\n", cell(service))
	out.WriteString("stego apply writes this file from the declaration and the registry, with the list in\n" +
		"platform-requirements.yaml. Do not edit it: change the declaration or the registry.\n")
	if len(list) == 0 {
		out.WriteString("\nThe service reads no environment variable.\n")
		return out.Bytes()
	}
	out.WriteString("\nA required variable has no default: the service does not start when it has no value. A secret variable\n" +
		"shows its name only: give its value through a secret of the platform.\n")
	byReader := map[string][]AcceptedVariable{}
	for _, variable := range list {
		for _, reader := range variable.ReadBy {
			byReader[reader] = append(byReader[reader], variable)
		}
	}
	readers := make([]string, 0, len(byReader))
	for reader := range byReader {
		readers = append(readers, reader)
	}
	slices.Sort(readers)
	for _, reader := range readers {
		fmt.Fprintf(&out, "\n## %s\n\n", cell(reader))
		out.WriteString("| Variable | Type | Required | Default | Secret | Allowed values | Example | Description |\n")
		out.WriteString("|---|---|---|---|---|---|---|---|\n")
		for _, variable := range byReader[reader] {
			defaultText, example, allowed := "none", "-", "-"
			if variable.Default != nil {
				defaultText = code(*variable.Default)
			}
			if variable.Allowed != "" {
				allowed = cell(variable.Allowed)
			}
			if variable.example != nil {
				example = code(*variable.example)
			}
			if variable.Secret {
				// A secret variable shows no default, no example and no allowed values.
				defaultText, example, allowed = "-", "-", "-"
			}
			fmt.Fprintf(&out, "| %s | %s | %s | %s | %s | %s | %s | %s |\n", code(variable.Name), cell(variable.Type), yesNo(variable.Required),
				defaultText, yesNo(variable.Secret), allowed, example, cell(variable.Description))
		}
	}
	renderSecretSources(&out, list)
	return out.Bytes()
}

// renderSecretSources renders, from the list alone, the Secret and the key of each variable with a secret source,
// names only, and which workload of the Kubernetes base reads it: the migration Job alone for a variable that only
// the migrate mode reads, and else the Deployment (ST-DLV-Q20). It renders nothing when no variable has a source.
func renderSecretSources(out *bytes.Buffer, list []AcceptedVariable) {
	var rows []AcceptedVariable
	for _, variable := range list {
		if variable.SecretSource != nil {
			rows = append(rows, variable)
		}
	}
	if len(rows) == 0 {
		return
	}
	out.WriteString("\n## Secrets\n\nThe Secret and the key that give each secret variable, or the file of a path variable, names only. " +
		"The migration Job\nalone reads a variable of the mode migrate; the serving Deployment never references its Secret.\n\n")
	out.WriteString("| Variable | Secret | Key | Read by |\n|---|---|---|---|\n")
	for _, variable := range rows {
		reader := "the Deployment"
		switch {
		case variable.ReadByMode == MigrateMode:
			reader = "the migration Job only (mode migrate)"
		case slices.Contains(MigrationKeyFiles, variable.Name):
			reader = "the Deployment and the migration Job"
		}
		fmt.Fprintf(out, "| %s | %s | %s | %s |\n", code(variable.Name), code(variable.SecretSource.Name), code(variable.SecretSource.Key), reader)
	}
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

// cell escapes text for one cell of a Markdown table: a pipe, a backslash, markup characters and any control
// character cannot end the cell or change what the reader sees.
func cell(text string) string {
	var out strings.Builder
	for _, r := range text {
		switch {
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || (r >= 0x200b && r <= 0x200f) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069):
			out.WriteRune('�')
		case strings.ContainsRune("\\|`*[]<>#", r):
			out.WriteRune('\\')
			out.WriteRune(r)
		default:
			out.WriteRune(r)
		}
	}
	return out.String()
}

// code shows a value as inline code, with its pipes escaped and a fence longer than any run of backticks in it.
func code(text string) string {
	fence := "`"
	for strings.Contains(text, fence) {
		fence += "`"
	}
	var out strings.Builder
	for _, r := range text {
		switch {
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || (r >= 0x200b && r <= 0x200f) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069):
			out.WriteRune('�')
		case r == '|':
			out.WriteString("\\|")
		default:
			out.WriteRune(r)
		}
	}
	padding := ""
	if strings.HasPrefix(text, "`") || strings.HasSuffix(text, "`") {
		padding = " "
	}
	return fence + padding + out.String() + padding + fence
}
