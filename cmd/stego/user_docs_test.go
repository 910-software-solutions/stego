package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/jsell-rh/stego/internal/fill"
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

// excerptMarker is the line that comes just before the fence of an excerpt: a part of a checked block of the same
// doc. The marker is a line of its own, so the info string of the fence stays one word, as the docs standard reads
// fences (ST-STD-DOCS-R02).
const excerptMarker = "<!-- excerpt -->"

// wholeCheckedDocs are the user docs in which each yaml, zed and proto block must be a checked block: a whole
// service declaration, the schema or the proto file that ST-STD-DOCS-R02-T02 writes, a fill manifest, or an
// excerpt. A doc that holds an excerpt marker is one of them too.
var wholeCheckedDocs = map[string]bool{"docs/commands-queries-workers.md": true}

// docBlock is one fenced block of a user doc: its first line, its language, whether the excerpt marker comes just
// before it, and its body without the indentation of its fence.
type docBlock struct {
	line     int
	language string
	marked   bool
	body     string
}

// docBlocks reads the fenced blocks of a doc, at any indentation, and counts the excerpt marker lines.
func docBlocks(text string) ([]docBlock, int) {
	lines := strings.Split(text, "\n")
	var blocks []docBlock
	markers := 0
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimLeft(lines[i], " \t")
		if strings.TrimSpace(trimmed) == excerptMarker {
			markers++
			continue
		}
		if !strings.HasPrefix(trimmed, "```") {
			continue
		}
		indent := lines[i][:len(lines[i])-len(trimmed)]
		block := docBlock{line: i + 1, language: strings.TrimSpace(strings.TrimPrefix(trimmed, "```")),
			marked: i > 0 && strings.TrimSpace(lines[i-1]) == excerptMarker}
		var body []string
		for i++; i < len(lines) && !strings.HasPrefix(strings.TrimLeft(lines[i], " \t"), "```"); i++ {
			body = append(body, strings.TrimPrefix(lines[i], indent)+"\n")
		}
		block.body = strings.Join(body, "")
		blocks = append(blocks, block)
	}
	return blocks, markers
}

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

// putDocProtoFiles writes each proto file of the setting proto_files that a whole declaration of a user doc names,
// from the doc's proto block, and gives the declaration's parts that name other files of the project.
func putDocProtoFiles(t *testing.T, project, doc, text, declaration string, block int) docDeclaration {
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
	return decl
}

// createDocFills runs stego fill create, as step 2 of the fill workflow of docs/commands-queries-workers.md gives
// it, for each fill of a command, query or worker of the declaration, with the slot that stego validate asks for
// (ST-DECL-INV39, ST-DECL-INV45, ST-DECL-INV68, ST-EXT-UC01-R01).
func createDocFills(t *testing.T, project, doc string, environment []string, decl docDeclaration, block int) {
	t.Helper()
	for _, slot := range []string{"command.command", "query.query", "worker.worker"} {
		operations := map[string][]docOperation{"command.command": decl.Commands, "query.query": decl.Queries, "worker.worker": decl.Workers}[slot]
		for _, operation := range operations {
			if operation.Fill == "" {
				continue
			}
			run := stegoIn(t, project, environment, "fill", "create", operation.Fill, "--slot", slot)
			if run.code != 0 {
				t.Errorf("%s, YAML block %d: stego fill create %s --slot %s exits %d:\n%s%s", doc, block, operation.Fill, slot, run.code, run.stdout, run.stderr)
			}
		}
	}
}

// qualifyDocFills qualifies the fills that fill create wrote for a doc's declaration, as its author does
// (ST-GEN-INV10): stego test, then stego fill sign with listed-ssh of ST-EXT-TD02, a key that the test run makes,
// never a real one.
func qualifyDocFills(t *testing.T, project string, environment []string, where string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(project, "fills"))
	if err != nil || len(entries) == 0 {
		return
	}
	keys := testFillKeys(t)
	putFile(t, project, "SIGNERS", keys.signers)
	signing := append(append(slices.Clone(environment), keys.goEnv...), "GOFLAGS=-modcacherw",
		"HOME="+keys.homes["listed-ssh"], "XDG_CONFIG_HOME="+filepath.Join(keys.homes["listed-ssh"], ".config"))
	commands := [][]string{{"test"}}
	for _, entry := range entries {
		commands = append(commands, []string{"fill", "sign", entry.Name()})
	}
	for _, command := range commands {
		if run := stegoIn(t, project, signing, command...); run.code != 0 {
			t.Errorf("%s: stego %v exits %d:\n%s%s", where, command, run.code, run.stdout, run.stderr)
			return
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
	t.Parallel()
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
				decl := putDocProtoFiles(t, project, doc, string(data), block[1], i+1)
				git(t, project, "init", "-q")
				if err := os.MkdirAll(filepath.Join(project, "fills"), 0o755); err != nil {
					t.Fatal(err)
				}
				environment := []string{"STEGO_REGISTRY=" + registry}
				createDocFills(t, project, doc, environment, decl, i+1)
				qualifyDocFills(t, project, environment, fmt.Sprintf("%s, YAML block %d", doc, i+1))
				run := stegoIn(t, project, environment, "validate")
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

// checkedSources gives the blocks of a user doc that ST-STD-DOCS-R02-T02 checks, by language: the whole service
// declarations for yaml, and the first zed block and the first proto block, which it writes into the project.
func checkedSources(text string) map[string][]string {
	sources := map[string][]string{}
	for _, block := range yamlBlockPattern.FindAllStringSubmatch(text, -1) {
		if strings.HasPrefix(block[1], "kind: service\n") {
			sources["yaml"] = append(sources["yaml"], block[1])
		}
	}
	if block := zedBlockPattern.FindStringSubmatch(text); block != nil {
		sources["zed"] = append(sources["zed"], block[1])
	}
	if block := protoBlockPattern.FindStringSubmatch(text); block != nil {
		sources["proto"] = append(sources["proto"], block[1])
	}
	return sources
}

// docFillSlots gives the slot of each fill that a command, a query or a worker of a whole declaration names.
func docFillSlots(t *testing.T, declarations []string) map[string]string {
	t.Helper()
	slots := map[string]string{}
	for _, declaration := range declarations {
		var decl docDeclaration
		if err := yaml.Unmarshal([]byte(declaration), &decl); err != nil {
			t.Fatalf("a declaration does not parse: %v", err)
		}
		for slot, operations := range map[string][]docOperation{"command.command": decl.Commands, "query.query": decl.Queries, "worker.worker": decl.Workers} {
			for _, operation := range operations {
				slots[operation.Fill] = slot
			}
		}
	}
	return slots
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
			sources := checkedSources(text)
			fills := docFillSlots(t, sources["yaml"])
			blocks, markers := docBlocks(text)
			marked := 0
			whole := wholeCheckedDocs[doc] || markers > 0
			if wholeCheckedDocs[doc] && markers == 0 {
				t.Errorf("%s: the doc holds no excerpt marker, and its parts of a declaration must be marked", doc)
			}
			firstOf := map[string]bool{}
			for _, block := range blocks {
				where := doc + ", the block at line " + strconv.Itoa(block.line)
				if block.marked {
					marked++
					if strings.TrimSpace(block.body) == "" {
						t.Errorf("%s: the excerpt is empty", where)
						continue
					}
					found := false
					for _, source := range sources[block.language] {
						if strings.Contains("\n"+source, "\n"+block.body) {
							found = true
						}
					}
					if !found {
						t.Errorf("%s: the %s excerpt is not whole lines of a checked %s block of the doc:\n%s", where, block.language, block.language, block.body)
					}
					continue
				}
				if !whole {
					continue
				}
				switch language := block.language; {
				case (language == "yaml" || language == "yml") && strings.HasPrefix(block.body, "kind: service\n"):
				case (language == "yaml" || language == "yml") && strings.HasPrefix(block.body, "kind: fill\n"):
					path := filepath.Join(t.TempDir(), "fill.yaml")
					if err := os.WriteFile(path, []byte(block.body), 0o644); err != nil {
						t.Fatal(err)
					}
					manifest, err := fill.ParseFill(path)
					if err != nil {
						t.Errorf("%s: the fill manifest does not parse: %v", where, err)
						continue
					}
					if slot, ok := fills[manifest.Name]; !ok || slot != manifest.Implements {
						t.Errorf("%s: the fill manifest names the fill %q of the slot %q, which no command, query or worker of the doc's declaration binds", where, manifest.Name, manifest.Implements)
					}
				case language == "zed" || language == "proto":
					if firstOf[language] {
						t.Errorf("%s: a second %s block is not checked; mark it as an excerpt of the first", where, language)
					}
					firstOf[language] = true
				case language == "yaml" || language == "yml":
					t.Errorf("%s: the yaml block is not a whole declaration, a fill manifest or a marked excerpt, so nothing checks it", where)
				}
			}
			if marked != markers {
				t.Errorf("%s: the doc holds %d excerpt markers, and %d of them come just before a fence", doc, markers, marked)
			}
		}
	})
}
