package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// yamlBlockPattern is a fenced YAML block of a Markdown doc, with its body.
var yamlBlockPattern = regexp.MustCompile("(?s)```ya?ml\n(.*?)```")

// zedBlockPattern is a fenced SpiceDB schema block of a user doc, and schemaPathPattern the schema path of a
// declaration. protoBlockPattern is a fenced proto file block of a user doc.
var (
	zedBlockPattern   = regexp.MustCompile("(?s)```zed\n(.*?)```")
	schemaPathPattern = regexp.MustCompile(`(?m)^\s+schema_path: (\S+)$`)
	protoBlockPattern = regexp.MustCompile("(?s)```proto\n(.*?)```")
)

// excerptBlockPattern is a fenced block of a language that the line <!-- excerpt --> comes just before, at any
// indentation, with its indentation, its language and its body. The marker is a line of its own, so the info string
// of the fence stays one word, as the docs standard reads fences (ST-STD-DOCS-R02).
var excerptBlockPattern = regexp.MustCompile("(?ms)^([ \t]*)<!-- excerpt -->\n[ \t]*```(\\w+)\n(.*?)^[ \t]*```[ \t]*$")

// docDeclaration is the part of a whole declaration in a user doc that names other files of the project: the fills of
// the commands, queries and workers, and the proto files of the command messages.
type docDeclaration struct {
	Commands  []docOperation `yaml:"commands"`
	Queries   []docOperation `yaml:"queries"`
	Workers   []docOperation `yaml:"workers"`
	Overrides struct {
		Command struct {
			ProtoFiles []struct {
				Path string `yaml:"path"`
			} `yaml:"proto_files"`
		} `yaml:"command"`
	} `yaml:"overrides"`
}

// docOperation is a command, a query or a worker of a declaration in a user doc, with the one fill it names.
type docOperation struct {
	Fill string `yaml:"fill"`
}

// putDocProjectFiles writes the files of a project that a whole declaration of a user doc names besides its schema:
// each proto file of the setting proto_files, from the doc's proto block, and the fill.yaml of each fill of a
// command, query or worker, which implements the slot that stego validate asks for (ST-DECL-INV39, ST-DECL-INV45,
// ST-DECL-INV68).
func putDocProjectFiles(t *testing.T, project, doc, text, declaration string, block int) {
	t.Helper()
	var decl docDeclaration
	if err := yaml.Unmarshal([]byte(declaration), &decl); err != nil {
		t.Fatalf("%s, YAML block %d: the declaration does not parse: %v", doc, block, err)
	}
	for _, file := range decl.Overrides.Command.ProtoFiles {
		proto := protoBlockPattern.FindStringSubmatch(text)
		if proto == nil {
			t.Fatalf("%s, YAML block %d: the declaration names the proto file %s, and the doc holds no proto block", doc, block, file.Path)
		}
		putFile(t, project, file.Path, proto[1])
	}
	for slot, operations := range map[string][]docOperation{"command.command": decl.Commands, "query.query": decl.Queries, "worker.worker": decl.Workers} {
		for _, operation := range operations {
			if operation.Fill != "" {
				putFile(t, project, filepath.Join("fills", operation.Fill, "fill.yaml"), "kind: fill\nname: "+operation.Fill+"\nimplements: "+slot+"\n")
			}
		}
	}
}

// userDocs reads the user doc inventory (ST-ASR-TOOL05): one path from the repository's root per line, a line that
// starts with # being a comment.
func userDocs(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "internal", "assure", "testdata", "user-docs.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var docs []string
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			docs = append(docs, line)
		}
	}
	return docs
}

func TestUserDocDeclarations(t *testing.T) {
	t.Run("ST-STD-DOCS-R02-T02 Each whole service declaration in a user doc passes stego validate against the repository's registry", func(t *testing.T) {
		registry, err := filepath.Abs(filepath.Join("..", "..", "registry"))
		if err != nil {
			t.Fatal(err)
		}
		found := 0
		for _, doc := range userDocs(t) {
			data, err := os.ReadFile(filepath.Join("..", "..", doc))
			if err != nil {
				t.Fatal(err)
			}
			for i, block := range yamlBlockPattern.FindAllStringSubmatch(string(data), -1) {
				if !strings.HasPrefix(block[1], "kind: service\n") {
					continue
				}
				found++
				project := t.TempDir()
				putFile(t, project, "service.yaml", block[1])
				// The declaration's SpiceDB schema is the doc's zed block, at the declared schema path; a
				// project with no record is in a Git repository, whose HEAD commit the compiler reads to choose
				// the data path (ST-SUP-RC01-OC41).
				if path := schemaPathPattern.FindStringSubmatch(block[1]); path != nil {
					schema := zedBlockPattern.FindStringSubmatch(string(data))
					if schema == nil {
						t.Fatalf("%s, YAML block %d: the declaration names the schema %s, and the doc holds no zed block", doc, i+1, path[1])
					}
					putFile(t, project, path[1], schema[1])
				}
				putDocProjectFiles(t, project, doc, string(data), block[1], i+1)
				git(t, project, "init", "-q")
				if err := os.MkdirAll(filepath.Join(project, "fills"), 0o755); err != nil {
					t.Fatal(err)
				}
				run := stegoIn(t, project, []string{"STEGO_REGISTRY=" + registry}, "validate")
				if run.code != 0 {
					t.Errorf("%s, YAML block %d: stego validate exits %d:\n%s%s", doc, i+1, run.code, run.stdout, run.stderr)
				}
			}
		}
		if found == 0 {
			t.Fatal("no user doc holds a whole service declaration, so the rule proves nothing; README.md's quick start holds one")
		}
	})
}

// excerptSources gives the blocks of a user doc that an excerpt of the language may be taken from: the whole
// service declarations for yaml, and the SpiceDB schema and the proto file that the check of the declarations writes
// into the project for zed and proto.
func excerptSources(text, language string) []string {
	var sources []string
	switch language {
	case "yaml":
		for _, block := range yamlBlockPattern.FindAllStringSubmatch(text, -1) {
			if strings.HasPrefix(block[1], "kind: service\n") {
				sources = append(sources, block[1])
			}
		}
	case "zed":
		if block := zedBlockPattern.FindStringSubmatch(text); block != nil {
			sources = append(sources, block[1])
		}
	case "proto":
		if block := protoBlockPattern.FindStringSubmatch(text); block != nil {
			sources = append(sources, block[1])
		}
	}
	return sources
}

func TestUserDocExcerpts(t *testing.T) {
	t.Parallel()
	t.Run("ST-STD-DOCS-R02-T03 Each excerpt block of a user doc is whole lines of a checked block of the same doc", func(t *testing.T) {
		t.Parallel()
		for _, doc := range userDocs(t) {
			data, err := os.ReadFile(filepath.Join("..", "..", doc))
			if err != nil {
				t.Fatal(err)
			}
			text := string(data)
			for i, excerpt := range excerptBlockPattern.FindAllStringSubmatch(text, -1) {
				indent, language := excerpt[1], excerpt[2]
				var lines []string
				for _, line := range strings.SplitAfter(excerpt[3], "\n") {
					if line != "" {
						lines = append(lines, strings.TrimPrefix(line, indent))
					}
				}
				body := strings.Join(lines, "")
				sources := excerptSources(text, language)
				if len(sources) == 0 {
					t.Errorf("%s, excerpt %d: the doc holds no checked %s block that the excerpt can come from", doc, i+1, language)
					continue
				}
				found := false
				for _, source := range sources {
					if strings.Contains("\n"+source, "\n"+body) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("%s, excerpt %d: the %s excerpt is not whole lines of a checked %s block of the doc:\n%s", doc, i+1, language, language, body)
				}
			}
		}
	})
}
