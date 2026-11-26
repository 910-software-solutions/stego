package assure

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The tests of the rules of ST-STD-RUNTIME (the runtime library), of ST-STD-TRUST-R05 (delivery to a GitOps tool)
// and of ST-STD-TXN-R14 (rows are owned by the lock that claimed them). Each reads the Go files of the tree at HEAD
// that are not test files and not under a testdata folder, so run them with -count=1 after a commit.

// runtimeModule is the folder of the runtime library's Go module.
const runtimeModule = "registry/runtime"

// connectorPackage is the one package of the runtime module that may import a database driver (ST-STD-RUNTIME-R06).
const connectorPackage = runtimeModule + "/pgconn"

// sourceFile is a Go file that is not a test file, parsed.
type sourceFile struct {
	name string
	data []byte
	file *ast.File
}

// goSources parses each Go file of the tree under the folder that is not a test file and not under a testdata
// folder. A file that does not parse fails the test.
func goSources(t *testing.T, tree Tree, folder string) []sourceFile {
	t.Helper()
	var files []sourceFile
	for _, name := range tree.paths() {
		if !strings.HasPrefix(name, folder+"/") || path.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") || strings.Contains(name, "/testdata/") {
			continue
		}
		data, _ := tree.File(name)
		file, err := parser.ParseFile(token.NewFileSet(), name, data, parser.ImportsOnly|parser.ParseComments)
		if err != nil {
			t.Fatalf("%s does not parse: %v", name, err)
		}
		full, err := parser.ParseFile(token.NewFileSet(), name, data, parser.SkipObjectResolution)
		if err == nil {
			file = full
		}
		files = append(files, sourceFile{name: name, data: data, file: file})
	}
	if len(files) == 0 {
		t.Fatalf("the tree holds no Go source file under %s", folder)
	}
	return files
}

// importsOf gives the import paths of the file.
func importsOf(file *ast.File) []string {
	var paths []string
	for _, spec := range file.Imports {
		if value, err := strconv.Unquote(spec.Path.Value); err == nil {
			paths = append(paths, value)
		}
	}
	return paths
}

// internalRegistration is the form of the one init that ST-STD-RUNTIME-R01 admits: a call of Register of an
// internal package of the runtime module, which gives that package a setter or an accessor of the registering
// package's own type, so a test of the module can reach a field that the code a service generates cannot set. It
// registers nothing by name, struct tag or a service's value.
func internalRegistration(function *ast.FuncDecl, imports map[string]string) bool {
	if function.Body == nil || len(function.Body.List) != 1 {
		return false
	}
	statement, ok := function.Body.List[0].(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := statement.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Register" {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	return ok && strings.HasPrefix(imports[pkg.Name], "github.com/jsell-rh/stego/registry/runtime/internal/")
}

// tableFill is the other form of init that ST-STD-RUNTIME-R01 admits: one loop that fills a package-level map from
// a string constant of the file, such as the reserved words of PostgreSQL. It reads no name of a service.
func tableFill(function *ast.FuncDecl) bool {
	if function.Body == nil || len(function.Body.List) != 1 {
		return false
	}
	loop, ok := function.Body.List[0].(*ast.RangeStmt)
	if !ok {
		return false
	}
	call, ok := loop.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Fields" || len(call.Args) != 1 {
		return false
	}
	_, literal := call.Args[0].(*ast.BasicLit)
	return literal
}

// processGlobals are the registries and the settings of the whole process that a library must not use on its own:
// the default HTTP mux, client and transport, the default logger and its output, the local time zone, the database
// driver and gob registries, the global providers and logger of OpenTelemetry, the default resolver, the
// environment, the working folder, the runtime's limits and signals (review of PR #504, M4). The flag package outside
// a FlagSet of its own is one too.
var processGlobals = map[string]bool{
	"net/http.Handle": true, "net/http.HandleFunc": true, "net/http.DefaultServeMux": true, "net/http.DefaultClient": true,
	"net/http.DefaultTransport": true, "net.DefaultResolver": true,
	"log/slog.SetDefault": true, "log/slog.SetLogLoggerLevel": true, "log.SetOutput": true, "log.SetFlags": true,
	"log.SetPrefix": true, "log.Default": true, "time.Local": true,
	"database/sql.Register": true, "encoding/gob.Register": true, "encoding/gob.RegisterName": true,
	"go.opentelemetry.io/otel.SetTracerProvider": true, "go.opentelemetry.io/otel.SetMeterProvider": true,
	"go.opentelemetry.io/otel.SetTextMapPropagator": true, "go.opentelemetry.io/otel.SetErrorHandler": true,
	"go.opentelemetry.io/otel.SetLogger": true, "go.opentelemetry.io/otel/log/global.SetLoggerProvider": true,
	"os.Setenv": true, "os.Unsetenv": true, "os.Clearenv": true, "os.Chdir": true, "os/signal.Notify": true,
	"os/signal.Ignore": true, "os/signal.Reset": true, "expvar.Publish": true, "runtime.GOMAXPROCS": true,
	"runtime/debug.SetGCPercent": true, "runtime/debug.SetMemoryLimit": true, "runtime/debug.SetMaxThreads": true,
	"runtime/debug.SetMaxStack": true, "runtime/debug.SetPanicOnFault": true, "runtime/debug.SetTraceback": true,
}

// admittedGlobals are the uses of a setting of the whole process that ST-STD-RUNTIME-R01 admits, by file and name,
// each for its reason. A new use fails the test.
//   - service.UseJSONLog sets the default logger when the generated NewService and Migrate call it.
//   - sealed reads GOMAXPROCS(0), which changes nothing, to size its pool of hash slots.
//   - kafkaoutbox reads the default resolver to look up a broker's addresses, and changes nothing.
var admittedGlobals = map[string][]string{
	runtimeModule + "/service/service.go":                {"log/slog.SetDefault"},
	runtimeModule + "/sealed/compare.go":                 {"runtime.GOMAXPROCS"},
	runtimeModule + "/outbox/kafkaoutbox/destination.go": {"net.DefaultResolver"},
}

// admittedGlobal tells if the use is one of admittedGlobals.
func admittedGlobal(source sourceFile, name string) bool {
	return slices.Contains(admittedGlobals[source.name], name)
}

// assignedSelectors gives each selector pkg.Name of an imported package on the left side of an assignment or an
// increment in the file, and each such selector that a field or an index of the left side starts from: a library
// that writes a variable of another package changes it for the whole process (review of PR #504, M4).
func assignedSelectors(file *ast.File, imports map[string]string) []string {
	var names []string
	var root func(expr ast.Expr) *ast.SelectorExpr
	root = func(expr ast.Expr) *ast.SelectorExpr {
		switch e := expr.(type) {
		case *ast.SelectorExpr:
			if pkg, ok := e.X.(*ast.Ident); ok && imports[pkg.Name] != "" {
				return e
			}
			return root(e.X)
		case *ast.IndexExpr:
			return root(e.X)
		case *ast.StarExpr:
			return root(e.X)
		case *ast.ParenExpr:
			return root(e.X)
		case *ast.TypeAssertExpr:
			return root(e.X)
		case *ast.CallExpr:
			return root(e.Fun)
		}
		return nil
	}
	add := func(expr ast.Expr) {
		if selector := root(expr); selector != nil {
			names = append(names, imports[selector.X.(*ast.Ident).Name]+"."+selector.Sel.Name)
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.AssignStmt:
			if n.Tok != token.DEFINE {
				for _, left := range n.Lhs {
					add(left)
				}
			}
		case *ast.IncDecStmt:
			add(n.X)
		}
		return true
	})
	return names
}

// frameworkFaults gives each fault of ST-STD-RUNTIME-R01 in the runtime module: an init that is not one of the two
// admitted forms, a dot import, which hides what the file uses, a read of a struct tag, a lookup of a type or a
// function by name (plugin, reflect), a use of a registry or a setting of the whole process, and an assignment to a
// variable of another package (review of PR #504, M4).
func frameworkFaults(files []sourceFile) []string {
	var faults []string
	for _, source := range files {
		imports := map[string]string{}
		for _, spec := range source.file.Imports {
			value, _ := strconv.Unquote(spec.Path.Value)
			name := path.Base(value)
			if spec.Name != nil {
				name = spec.Name.Name
			}
			if name == "." {
				faults = append(faults, fmt.Sprintf("%s imports %s with a dot, so the test cannot see what it uses", source.name, value))
				continue
			}
			imports[name] = value
		}
		for _, value := range importsOf(source.file) {
			if value == "plugin" || value == "expvar" {
				faults = append(faults, fmt.Sprintf("%s imports %s, which registers or discovers by name", source.name, value))
			}
		}
		for _, name := range assignedSelectors(source.file, imports) {
			faults = append(faults, fmt.Sprintf("%s assigns to %s, a variable of another package, which changes it for the whole process", source.name, name))
		}
		ast.Inspect(source.file, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.FuncDecl:
				if n.Recv == nil && n.Name.Name == "init" && !internalRegistration(n, imports) && !tableFill(n) {
					faults = append(faults, fmt.Sprintf("%s has an init that registers or discovers something", source.name))
				}
			case *ast.SelectorExpr:
				if pkg, ok := n.X.(*ast.Ident); ok {
					imported := imports[pkg.Name]
					name := imported + "." + n.Sel.Name
					if processGlobals[name] || imported == "flag" && !slices.Contains([]string{"NewFlagSet", "ContinueOnError", "ExitOnError", "PanicOnError", "FlagSet", "ErrHelp", "Value", "Flag"}, n.Sel.Name) {
						if !admittedGlobal(source, name) {
							faults = append(faults, fmt.Sprintf("%s uses %s, a registry or a setting of the whole process", source.name, name))
						}
					}
				}
				if n.Sel.Name == "Tag" || n.Sel.Name == "Lookup" && strings.Contains(fmt.Sprint(n.X), "Tag") {
					faults = append(faults, fmt.Sprintf("%s reads a struct tag", source.name))
				}
			}
			return true
		})
	}
	return faults
}

// driverImport tells if an import path is a database driver or its connection package.
var driverImport = regexp.MustCompile(`^(github\.com/jackc/pgx(/v[0-9]+)?(/|$)|github\.com/lib/pq$|github\.com/go-sql-driver/|github\.com/mattn/go-sqlite3$|modernc\.org/sqlite$)`)

// A store transaction that a function of the runtime module takes: a parameter whose type names Tx, other than
// *sql.Tx, an interface that the package declares, or a type of the module's own packages.
func foreignTransactions(files []sourceFile) []string {
	var faults []string
	for _, source := range files {
		imports := map[string]string{}
		for _, spec := range source.file.Imports {
			value, _ := strconv.Unquote(spec.Path.Value)
			name := path.Base(value)
			if spec.Name != nil {
				name = spec.Name.Name
			}
			imports[name] = value
		}
		for _, decl := range source.file.Decls {
			function, ok := decl.(*ast.FuncDecl)
			if !ok || !function.Name.IsExported() || function.Type.Params == nil {
				continue
			}
			for _, field := range function.Type.Params.List {
				expr := field.Type
				if star, ok := expr.(*ast.StarExpr); ok {
					expr = star.X
				}
				selector, ok := expr.(*ast.SelectorExpr)
				if !ok {
					continue
				}
				pkg, ok := selector.X.(*ast.Ident)
				if ok && strings.HasSuffix(selector.Sel.Name, "Tx") && driverImport.MatchString(imports[pkg.Name]) {
					faults = append(faults, fmt.Sprintf("%s: %s takes the transaction %s.%s of a driver, not *sql.Tx", source.name, function.Name.Name, pkg.Name, selector.Sel.Name))
				}
			}
		}
	}
	return faults
}

// driverFaults gives each fault of ST-STD-RUNTIME-R06 in the runtime module: a file outside the connector package
// that imports a database driver, and an exported function that takes the transaction type of a driver.
func driverFaults(files []sourceFile, admitted func(name, imported string) bool) []string {
	var faults []string
	for _, source := range files {
		for _, imported := range importsOf(source.file) {
			if driverImport.MatchString(imported) && !admitted(source.name, imported) {
				faults = append(faults, fmt.Sprintf("%s imports the database driver package %s outside %s", source.name, imported, connectorPackage))
			}
		}
	}
	return append(faults, foreignTransactions(files)...)
}

// libraryFiles gives the Go source files of the runtime library: those of the runtime module, apart from its test
// harnesses pgtest and kafkatest, which are tooling (ST-SUP-TOOL14, ST-SUP-TOOL15) that only tests import.
func libraryFiles(t *testing.T, tree Tree) []sourceFile {
	t.Helper()
	return slices.DeleteFunc(goSources(t, tree, runtimeModule), func(source sourceFile) bool {
		return strings.HasPrefix(source.name, runtimeModule+"/internal/pgtest/") || strings.HasPrefix(source.name, runtimeModule+"/kafkatest/")
	})
}

func TestRuntimeLibraryStandard(t *testing.T) {
	_, tree := readHead(t)
	files := libraryFiles(t, tree)

	t.Run("ST-STD-RUNTIME-R01-T01 The runtime library registers and discovers nothing by name, struct tag or init", func(t *testing.T) {
		report(t, "ST-STD-RUNTIME-R01", frameworkFaults(files))
	})

	t.Run("ST-STD-RUNTIME-R02-T01 No file of the runtime library imports reflect or unsafe", func(t *testing.T) {
		var faults []string
		for _, source := range files {
			for _, imported := range importsOf(source.file) {
				if imported == "reflect" || imported == "unsafe" {
					faults = append(faults, fmt.Sprintf("%s imports %s", source.name, imported))
				}
			}
		}
		report(t, "ST-STD-RUNTIME-R02", faults)
	})

	t.Run("ST-STD-RUNTIME-R06-T01 Only the connector package of the runtime library imports a database driver", func(t *testing.T) {
		report(t, "ST-STD-RUNTIME-R06", driverFaults(files, func(name, imported string) bool {
			return path.Dir(name) == connectorPackage || strings.HasPrefix(path.Dir(name), connectorPackage+"/") || knownDriverImport(name, imported)
		}))
	})
}

// knownDriverImport tells if the import is one that the record of the runtime package names as a gap of
// ST-STD-RUNTIME-R06 not closed yet: ST-SUP-RP04-LF08 (registry/runtime/pgconn/connect.go) is not written, so the
// tenant runtime's connector and session guard still sit in package tenant, and the command runtime reads the
// error type of pgconn. The list holds each file and import by name, so a new driver import fails the test.
func knownDriverImport(name, imported string) bool {
	known := map[string][]string{
		"registry/runtime/tenant/owner.go":     {"github.com/jackc/pgx/v5", "github.com/jackc/pgx/v5/pgconn", "github.com/jackc/pgx/v5/stdlib"},
		"registry/runtime/tenant/provision.go": {"github.com/jackc/pgx/v5"},
		"registry/runtime/tenant/tx.go":        {"github.com/jackc/pgx/v5"},
		"registry/runtime/command/handle.go":   {"github.com/jackc/pgx/v5/pgconn"},
	}
	return slices.Contains(known[name], imported)
}

// importNames gives the name by which the file refers to each import path: its alias, or its last element.
func importNames(file *ast.File) map[string]string {
	names := map[string]string{}
	for _, spec := range file.Imports {
		value, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		name := path.Base(value)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		names[name] = value
	}
	return names
}

// gitopsImports are the imports that a file of the delivery package may have: the standard library's text,
// encoding, data and error packages, the YAML package, and the application build's pins. Each other import fails the
// test, so a network, process, gRPC, Kubernetes, Argo CD or TLS client is never in the package (review of PR #504,
// M2). net and net/url parse and check addresses; neither dials.
var gitopsImports = map[string]bool{
	"bytes": true, "errors": true, "fmt": true, "io": true, "maps": true, "math": true, "net": true, "net/url": true,
	"path": true, "regexp": true, "slices": true, "sort": true, "strconv": true, "strings": true, "time": true,
	"unicode": true, "unicode/utf8": true, "gopkg.in/yaml.v3": true, "github.com/jsell-rh/stego/internal/appbuild": true,
}

// gitopsDials are the functions of the admitted imports that open a connection: a file of the delivery package
// must not refer to one.
var gitopsDials = map[string]bool{"net.Dial": true, "net.DialTimeout": true, "net.DialTCP": true, "net.DialUDP": true,
	"net.DialIP": true, "net.DialUnix": true, "net.Listen": true, "net.ListenPacket": true, "net.LookupHost": true,
	"net.LookupIP": true, "net.LookupAddr": true, "net.DefaultResolver": true, "net.Dialer": true, "net.Resolver": true}

// gitopsReads are the functions of package os and io/ioutil that read the environment or a file. A file of the
// delivery package must not refer to one, as a call or as a value (os.Expand(text, os.Getenv)).
var gitopsReads = map[string]bool{"os.Getenv": true, "os.LookupEnv": true, "os.Environ": true, "os.ExpandEnv": true,
	"os.Expand": true, "os.ReadFile": true, "os.Open": true, "os.OpenFile": true, "os.ReadDir": true, "os.DirFS": true,
	"os.UserHomeDir": true, "os.UserConfigDir": true, "io/ioutil.ReadFile": true, "io/ioutil.ReadDir": true}

// gitopsFaults gives each fault of ST-STD-TRUST-R05 in a file of the package that delivers to a GitOps tool: an
// import that gitopsImports does not admit, a dot import, a reference to a function that dials, and a reference to a
// function that reads the environment or a file, where a credential of the tool would be (review of PR #504, L6 and
// M2). The adapter writes its files; the caller reads the declaration and passes its values.
func gitopsFaults(source sourceFile) []string {
	var faults []string
	for _, spec := range source.file.Imports {
		imported, _ := strconv.Unquote(spec.Path.Value)
		if spec.Name != nil && spec.Name.Name == "." {
			faults = append(faults, fmt.Sprintf("%s imports %s with a dot, so the test cannot see what it calls", source.name, imported))
		}
		if !gitopsImports[imported] {
			faults = append(faults, fmt.Sprintf("%s imports %s, which the delivery package may not import: it can call the tool or the cluster, or read a credential", source.name, imported))
		}
	}
	names := importNames(source.file)
	ast.Inspect(source.file, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if pkg, ok := selector.X.(*ast.Ident); ok {
			name := names[pkg.Name] + "." + selector.Sel.Name
			if gitopsReads[name] {
				faults = append(faults, fmt.Sprintf("%s refers to %s, which reads the environment or a file where a credential of the tool would be", source.name, name))
			}
			if gitopsDials[name] {
				faults = append(faults, fmt.Sprintf("%s refers to %s, which opens a connection", source.name, name))
			}
		}
		return true
	})
	return faults
}

// gitopsAdapter is the technical record of the adapter that delivers to Argo CD (ST-INT03).
const gitopsAdapter = "ST-INT03-AD"

func TestGitOpsDeliveryWritesFilesOnly(t *testing.T) {
	spec, tree := readHead(t)
	var adapter string
	for _, record := range spec.Technical {
		if record.ID == gitopsAdapter {
			adapter = record.File
		}
	}
	if adapter == "" {
		t.Fatalf("the spec at HEAD has no adapter %s with a file", gitopsAdapter)
	}
	t.Run("ST-STD-TRUST-R05-T01 The Argo CD adapter writes files and calls no API, runs no command and reads no credential", func(t *testing.T) {
		// The adapter's file can call each function of its package, so the test reads each Go file of the package
		// that is not a test file (review of PR #504, M2).
		var faults []string
		if _, held := tree.File(adapter); !held {
			faults = append(faults, adapter+" is not in the tree")
		}
		read := 0
		for _, source := range goSources(t, tree, path.Dir(adapter)) {
			if path.Dir(source.name) != path.Dir(adapter) {
				continue
			}
			read++
			faults = append(faults, gitopsFaults(source)...)
		}
		if read < 2 {
			faults = append(faults, fmt.Sprintf("the test reads %d file of the package of %s, so it does not read the whole package", read, adapter))
		}
		report(t, "ST-STD-TRUST-R05", faults)
	})
}

// consoleSink is the variable that turns the console sink of the domain probes on (ST-SUP-RP12-LC16).
const consoleSink = "STEGO_PROBE_CONSOLE"

// deliveryStanzaFor is a delivery stanza for Argo CD that the test adds to the declaration of an example, so that
// stego apply writes the GitOps object as well as the Kubernetes base.
func deliveryStanzaFor(service string) string {
	return "\ndelivery:\n  argocd:\n    project: " + service + "\n" +
		"    source: {repo: https://git.example/" + service + ".git, path: deploy/base}\n" +
		"    environments: {repo: https://git.example/platform.git, revision: main, path: environments/" + service + "/*.yaml}\n"
}

// runIn runs the program in the folder with the environment added, and fails the test when it fails.
func runIn(t *testing.T, dir string, env []string, program string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, program, args...)
	command.Dir = dir
	command.Env = append(os.Environ(), env...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("%s %s in %s: %v\n%s", program, strings.Join(args, " "), dir, err, output)
	}
}

// generatedDeployFiles writes the tree at HEAD to a temporary folder, builds stego from it, and runs stego apply on
// a copy of each example with a delivery stanza for Argo CD. It gives each file that apply writes under deploy/base,
// deploy/components and deploy/argocd, whatever its extension, by its path under examples (review of PR #504, M3).
func generatedDeployFiles(t *testing.T) map[string][]byte {
	t.Helper()
	root, err := RepositoryRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "stego")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "head.tar")
	runIn(t, root, nil, "git", "archive", "--output", archive, "HEAD")
	runIn(t, source, nil, "tar", "-xf", archive)
	stego := filepath.Join(t.TempDir(), "stego")
	runIn(t, source, []string{"GOWORK=off", "GOFLAGS=-mod=readonly", "GOTOOLCHAIN=local"}, "go", "build", "-trimpath", "-o", stego, "./cmd/stego")
	entries, err := os.ReadDir(filepath.Join(source, "examples"))
	if err != nil {
		t.Fatal(err)
	}
	identity := []string{"GIT_AUTHOR_NAME=assure", "GIT_AUTHOR_EMAIL=assure@invalid", "GIT_COMMITTER_NAME=assure", "GIT_COMMITTER_EMAIL=assure@invalid"}
	files := map[string][]byte{}
	for _, entry := range entries {
		example := filepath.Join(source, "examples", entry.Name())
		declaration, err := os.ReadFile(filepath.Join(example, "service.yaml"))
		if !entry.IsDir() || err != nil {
			continue
		}
		// apply checks the declaration against the version on main, so the example as committed is that version.
		runIn(t, example, identity, "git", "init", "-q", "-b", "main")
		runIn(t, example, identity, "git", "add", "-A")
		runIn(t, example, identity, "git", "commit", "-q", "-m", "the example at HEAD")
		if !strings.Contains(string(declaration), "\ndelivery:") {
			if err := os.WriteFile(filepath.Join(example, "service.yaml"), append(declaration, deliveryStanzaFor(entry.Name())...), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		runIn(t, example, []string{"STEGO_REGISTRY=" + filepath.Join(source, "registry"), "GOWORK=off", "GOTOOLCHAIN=local"}, stego, "apply")
		for _, folder := range []string{"base", "components", "argocd"} {
			err := filepath.WalkDir(filepath.Join(example, "deploy", folder), func(name string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				data, err := os.ReadFile(name)
				if err != nil {
					return err
				}
				relative, _ := filepath.Rel(filepath.Join(source, "examples"), name)
				files[filepath.ToSlash(relative)] = data
				return nil
			})
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				t.Fatal(err)
			}
		}
	}
	return files
}

// consoleSinkFaults gives each file that names the console sink, and a fault when no file of the base or no file of
// the GitOps object was read, so that the test never passes on nothing.
func consoleSinkFaults(origin string, files map[string][]byte) []string {
	var faults []string
	base, gitops := 0, 0
	for name, data := range files {
		parts := strings.Split(name, "/")
		if len(parts) < 3 || parts[1] != "deploy" {
			continue
		}
		switch parts[2] {
		case "base", "components":
			base++
		case "argocd":
			gitops++
		default:
			continue
		}
		if strings.Contains(string(data), consoleSink) {
			faults = append(faults, fmt.Sprintf("%s %s names %s", origin, name, consoleSink))
		}
	}
	if base == 0 {
		faults = append(faults, origin+": no file of deploy/base or deploy/components was read, so the test reads nothing of the base")
	}
	if gitops == 0 {
		faults = append(faults, origin+": no file of deploy/argocd was read, so the test reads nothing of the GitOps object")
	}
	return faults
}

func TestDeploymentLeavesTheConsoleSinkOff(t *testing.T) {
	_, tree := readHead(t)
	t.Run("ST-STD-OBS-R03-T01 No Kubernetes base or GitOps object that stego apply writes for an example sets the console sink", func(t *testing.T) {
		// The committed deploy files of the examples, each file whatever its extension (a configMapGenerator can load
		// a .env file). No example states a delivery stanza, so these hold no GitOps object.
		committed := map[string][]byte{}
		for _, name := range tree.paths() {
			parts := strings.Split(name, "/")
			if len(parts) < 4 || parts[0] != "examples" || parts[2] != "deploy" || parts[3] != "base" && parts[3] != "components" && parts[3] != "argocd" {
				continue
			}
			committed[strings.TrimPrefix(name, "examples/")], _ = tree.File(name)
		}
		// The committed files are reported first, so a fault in them is named even when apply refuses the example.
		report(t, "ST-STD-OBS-R03", slices.DeleteFunc(consoleSinkFaults("committed", committed), func(fault string) bool {
			return strings.Contains(fault, "nothing of the GitOps object")
		}))
		// A fresh generation of each example with a delivery stanza: the base and the ApplicationSet that the
		// stego of HEAD writes (review of PR #504, M3).
		report(t, "ST-STD-OBS-R03", consoleSinkFaults("generated", generatedDeployFiles(t)))
	})
}
