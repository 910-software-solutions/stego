package main

import (
	"encoding/json"
	"go/scanner"
	"go/token"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
	"github.com/jsell-rh/stego/internal/registry"
)

// The release scan of ST-SUP-UC01-R10 and ST-SUP-INV22: each environment variable that the code a component emits,
// or a runtime package its code calls, reads is declared by that component, and each declared variable is read. The
// scan reads the generator packages (their templates and the code that their Go source emits as text), the code
// that the assembler emits, and the runtime packages, as Go tokens. It follows a name through a constant, a helper
// that takes the name as a parameter, a getenv function that the caller passes in, a field of a struct literal, and
// an enumeration of the environment under a prefix. It matches os and syscall under any import name, a local getenv
// such as getenv := os.Getenv, os.Getenv passed to a runtime function, and os.ExpandEnv of a constant text. A read
// whose name it cannot follow fails the scan, such as os.Expand, a dot import, or os.Getenv passed to another
// function. It also holds each component that owns a library to the variables that the library reads by itself
// (libraryReads).
//
// What the scan does not see: the code of a fill, which the service author writes; files that are not Go source or
// a Go template, such as a script or a container file; a name that reaches os.Getenv through reflection or through
// an assignment to a struct field that is not a literal of the field; and a library that reads a variable through
// a module other than pgx, grpc-go, the OpenTelemetry SDK and the Go standard library.

// scanUnit is a set of files whose reads one or more components own.
type scanUnit struct {
	label  string
	files  []string
	owners []string
}

// compilerOwners are the files of the compiler that emit code which reads the environment, with the component
// whose part the assembler wires (F16, 2026-11-14). compilerOwn are the compiler files that read STEGO's own
// variables, which are no service variable.
var (
	compilerOwners = map[string][]string{
		"database_config.go":     {"postgres-adapter"},
		"http_tls.go":            {"health-check"},
		"assembler.go":           {"health-check"},
		"runtime_wiring.go.tmpl": {"command", "kafka-producer", "kafka-consumer"},
	}
	compilerOwn = map[string][]string{"apply_hook.go": {"STEGO_TEST_APPLY_LOG", "STEGO_TEST_STOP_APPLY"}, "init.go": {"STEGO_REGISTRY"}}
	// runtimeOwners are the runtime packages whose settings a component declares, by their path in the runtime
	// module, with the components that wire them.
	runtimeOwners = map[string][]string{
		"service":            {"postgres-adapter", "spicedb-authz", "query", "command", "jwt-auth", "rh-sso-auth", "oidc-auth"},
		"modes":              {"command"},
		"probe":              {"domain-probe"},
		"otel":               {"otel-tracing"},
		"audit/auditsink":    {"audit-log"},
		"outbox/kafkaoutbox": {"kafka-producer", "kafka-consumer"},
		"outbox/kafkainbox":  {"kafka-consumer"},
	}
	// emitsAlso are the generator packages whose generator emits the code of another generator package too, such as
	// rh-sso-auth, which emits the jwt-auth middleware.
	emitsAlso = map[string][]string{"internal/generator/rhssoauth": {"internal/generator/jwtauth"}}
	// declarationNamed are the components whose variable names come from the declaration, whose fields state the
	// same facts (ST-SUP-RC22-OC09).
	declarationNamed = []string{"runtime-configuration"}
	// clientFixtures are the contexts in which the scan runs the generator of each component that declares a
	// variable read by a client program alone (read_by_client), so that it can scan each emitted file by its path
	// (security review of PR #468, L2).
	clientFixtures = map[string]gen.Context{
		"cli-application": {ModuleName: "example.com/scan", OutDirName: "out", OutputNamespace: "cli", ServiceName: "records",
			ComponentConfig: map[string]any{"factory_package": "app"}, PeerNamespaces: map[string]string{"otel-tracing": "tracing"}},
	}
)

// scanRead is one read that the scan finds: the name, or "" with why it cannot be followed.
type scanRead struct {
	name, file, unresolved string
}

var (
	envNameText = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,127}$`)
	// expansion is one $NAME or ${NAME} of a text that os.ExpandEnv expands.
	expansion = regexp.MustCompile(`\$(?:\{([A-Za-z_][A-Za-z0-9_]*)\}|([A-Za-z_][A-Za-z0-9_]*))`)
)

// passedToRuntime tells whether the value at the position is an argument of a call of a function of a package that
// the text imports from the runtime module, such as modes.Parse(os.Args[1:], os.Getenv).
func passedToRuntime(toks []tok, at int) bool {
	depth := 0
	for i := at - 1; i >= 1; i-- {
		switch toks[i].t {
		case token.RPAREN, token.RBRACK, token.RBRACE:
			depth++
		case token.LBRACK, token.LBRACE:
			depth--
		case token.LPAREN:
			if depth == 0 {
				if i >= 3 && toks[i-1].t == token.IDENT && toks[i-2].t == token.PERIOD && toks[i-3].t == token.IDENT {
					return runtimeImport(toks, toks[i-3].text)
				}
				return false
			}
			depth--
		}
	}
	return false
}

// runtimeImport tells whether the name is the package name of an import of the runtime module in the text.
func runtimeImport(toks []tok, name string) bool {
	for _, t := range toks {
		if t.t != token.STRING {
			continue
		}
		path, err := strconv.Unquote(t.text)
		if err == nil && strings.HasPrefix(path, "github.com/jsell-rh/stego/registry/runtime/") && filepath.Base(path) == name {
			return true
		}
	}
	return false
}

// tok is one Go token of a text.
type tok struct {
	t    token.Token
	text string
}

// tokens scans a text as Go tokens and ignores what is not Go, such as a template action.
func tokens(text string) []tok {
	var s scanner.Scanner
	fset := token.NewFileSet()
	file := fset.AddFile("", fset.Base(), len(text))
	s.Init(file, []byte(text), func(token.Position, string) {}, 0)
	var out []tok
	for {
		_, t, lit := s.Scan()
		if t == token.EOF {
			return out
		}
		if t == token.SEMICOLON && lit == "\n" {
			continue
		}
		if lit == "" {
			lit = t.String()
		}
		out = append(out, tok{t, lit})
	}
}

// codeTexts gives the texts of a file that hold code: the file, and for a Go file of a generator or of the compiler
// each string literal, which may be code that it emits.
func codeTexts(path string, emits bool) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	all := []string{string(data)}
	if emits && strings.HasSuffix(path, ".go") {
		for _, t := range tokens(string(data)) {
			if t.t == token.STRING {
				if value, err := strconv.Unquote(t.text); err == nil && strings.Contains(value, "os.") {
					all = append(all, value)
				}
			}
		}
	}
	return all
}

// unitReads finds every environment read of a unit.
func unitReads(t *testing.T, unit scanUnit, emits bool) []scanRead {
	t.Helper()
	type text struct {
		file string
		toks []tok
	}
	var all []text
	for _, file := range unit.files {
		for _, body := range codeTexts(file, emits) {
			all = append(all, text{file, tokens(body)})
		}
	}
	// The constants of the unit: IDENT = "TEXT" and IDENT := "TEXT", and the struct fields Field: "TEXT".
	consts := map[string][]string{}
	fields := map[string][]string{}
	for _, x := range all {
		for i := 0; i+2 < len(x.toks); i++ {
			a, op, b := x.toks[i], x.toks[i+1], x.toks[i+2]
			if a.t != token.IDENT || b.t != token.STRING {
				continue
			}
			value, err := strconv.Unquote(b.text)
			if err != nil {
				continue
			}
			switch op.t {
			case token.ASSIGN, token.DEFINE:
				consts[a.text] = append(consts[a.text], value)
			case token.COLON:
				fields[a.text] = append(fields[a.text], value)
			}
		}
	}
	var reads []scanRead
	var resolve func(file string, toks []tok, at int, depth int) []scanRead
	// argument resolves the expression that starts at toks[at] as a variable name.
	resolve = func(file string, toks []tok, at int, depth int) []scanRead {
		if at >= len(toks) || depth > 6 {
			return []scanRead{{file: file, unresolved: "the name is not a constant"}}
		}
		first := toks[at]
		switch {
		case first.t == token.STRING:
			value, _ := strconv.Unquote(first.text)
			if at+2 < len(toks) && toks[at+1].t == token.ADD {
				return []scanRead{{file: file, unresolved: "the name is built from parts"}}
			}
			return []scanRead{{name: value, file: file}}
		case first.text == "{":
			return []scanRead{{file: file, unresolved: "declaration"}}
		case first.t == token.IDENT && at+2 < len(toks) && toks[at+1].t == token.PERIOD && toks[at+2].t == token.IDENT:
			// A field of a struct: each text that a struct literal of the unit gives the field.
			var out []scanRead
			for _, value := range fields[toks[at+2].text] {
				out = append(out, scanRead{name: value, file: file})
			}
			if len(out) == 0 {
				return []scanRead{{file: file, unresolved: "the field " + toks[at+2].text + " is given no constant"}}
			}
			return out
		case first.t == token.IDENT:
			suffix := ""
			name := first.text
			// A local name := param + "SUFFIX".
			for i := at - 1; i >= 0 && i > at-400; i-- {
				if toks[i].t == token.IDENT && toks[i].text == name && i+4 < len(toks) && toks[i+1].t == token.DEFINE && toks[i+2].t == token.IDENT && toks[i+3].t == token.ADD && toks[i+4].t == token.STRING {
					value, _ := strconv.Unquote(toks[i+4].text)
					name, suffix = toks[i+2].text, value
					break
				}
			}
			if values, ok := consts[name]; ok && suffix == "" {
				var out []scanRead
				for _, value := range values {
					out = append(out, scanRead{name: value, file: file})
				}
				return out
			}
			// A parameter of the function around the read: follow each call of the function.
			fn, index := enclosingParam(toks, at, name)
			if fn == "" {
				return []scanRead{{file: file, unresolved: "the name " + name + " is no constant and no parameter"}}
			}
			var out []scanRead
			for _, x := range all {
				for i := 0; i+1 < len(x.toks); i++ {
					if x.toks[i].t != token.IDENT || x.toks[i].text != fn || x.toks[i+1].t != token.LPAREN || (i > 0 && x.toks[i-1].t == token.FUNC) {
						continue
					}
					arg := argumentAt(x.toks, i+1, index)
					if arg < 0 {
						continue
					}
					for _, r := range resolve(x.file, x.toks, arg, depth+1) {
						if r.name != "" {
							r.name += suffix
						}
						out = append(out, r)
					}
				}
			}
			if len(out) == 0 {
				return []scanRead{{file: file, unresolved: "no call of " + fn + " gives the name"}}
			}
			return out
		}
		return []scanRead{{file: file, unresolved: "the name is not a constant"}}
	}
	for _, x := range all {
		toks := x.toks
		// The getenv functions of the unit: a parameter of the type func(string) string.
		getters := map[string]bool{}
		for i := 0; i+5 < len(toks); i++ {
			if toks[i].t == token.IDENT && strings.Contains(strings.ToLower(toks[i].text), "env") && toks[i+1].t == token.FUNC && toks[i+2].t == token.LPAREN && toks[i+3].text == "string" && toks[i+4].t == token.RPAREN {
				getters[toks[i].text] = true
			}
		}
		// The names of the package os in the text: os, and each alias of an import of "os" (#344 security review
		// M1). A dot import of os, or of syscall, is a read the scan cannot follow.
		osNames := map[string]bool{"os": true, "syscall": true}
		for i := 0; i+1 < len(toks); i++ {
			if toks[i+1].t == token.STRING && (toks[i+1].text == `"os"` || toks[i+1].text == `"syscall"`) {
				switch {
				case toks[i].t == token.IDENT && toks[i].text != "import":
					osNames[toks[i].text] = true
				case toks[i].t == token.PERIOD:
					reads = append(reads, scanRead{file: x.file, unresolved: "a dot import of os or syscall"})
				}
			}
			// getenv := os.Getenv: a local name of a getenv function.
			if toks[i].t == token.IDENT && toks[i+1].t == token.DEFINE && i+4 < len(toks) && osNames[toks[i+2].text] && toks[i+3].t == token.PERIOD &&
				(toks[i+4].text == "Getenv" || toks[i+4].text == "LookupEnv") && (i+5 >= len(toks) || toks[i+5].t != token.LPAREN) {
				getters[toks[i].text] = true
			}
		}
		for i := 0; i+3 < len(toks); i++ {
			isOS := osNames[toks[i].text] && toks[i+1].t == token.PERIOD && (i == 0 || toks[i-1].t != token.PERIOD)
			call := toks[i+1].t == token.LPAREN && getters[toks[i].text] && (i == 0 || toks[i-1].t != token.PERIOD)
			switch {
			case isOS && (toks[i+2].text == "Getenv" || toks[i+2].text == "LookupEnv"):
				if toks[i+3].t != token.LPAREN {
					// os.Getenv as a value: a local getenv (above), or an argument of a runtime function whose getenv
					// parameter the scan follows in the runtime package. Any other use is a read it cannot follow.
					local := i >= 2 && toks[i-1].t == token.DEFINE && getters[toks[i-2].text]
					argument := i >= 1 && (toks[i-1].t == token.COMMA || toks[i-1].t == token.LPAREN) && passedToRuntime(toks, i)
					if !local && !argument {
						reads = append(reads, scanRead{file: x.file, unresolved: "declaration: os.Getenv or os.LookupEnv used as a value"})
					}
					continue
				}
				reads = append(reads, resolve(x.file, toks, i+4, 0)...)
			case isOS && (toks[i+2].text == "ExpandEnv" || toks[i+2].text == "Expand"):
				// An expansion reads each $NAME of its text; only a constant text can be followed.
				if toks[i+3].t == token.LPAREN && i+4 < len(toks) && toks[i+4].t == token.STRING && toks[i+2].text == "ExpandEnv" {
					text, _ := strconv.Unquote(toks[i+4].text)
					for _, match := range expansion.FindAllStringSubmatch(text, -1) {
						reads = append(reads, scanRead{name: match[1] + match[2], file: x.file})
					}
					continue
				}
				reads = append(reads, scanRead{file: x.file, unresolved: "an expansion of the environment"})
			case call:
				reads = append(reads, resolve(x.file, toks, i+2, 0)...)
			case isOS && toks[i+2].text == "Environ":
				// An enumeration under a prefix: each name of the prefix that the unit names is a read.
				var prefixes []string
				for j := 0; j+4 < len(toks); j++ {
					if toks[j].text == "HasPrefix" && toks[j+1].t == token.LPAREN && toks[j+3].t == token.COMMA && toks[j+4].t == token.STRING {
						prefix, _ := strconv.Unquote(toks[j+4].text)
						prefixes = append(prefixes, prefix)
					}
				}
				if len(prefixes) == 0 {
					reads = append(reads, scanRead{file: x.file, unresolved: "the environment is enumerated with no prefix"})
				}
				for _, y := range all {
					for _, t := range y.toks {
						value, err := strconv.Unquote(t.text)
						if t.t == token.STRING && err == nil && envNameText.MatchString(value) {
							for _, prefix := range prefixes {
								if strings.HasPrefix(value, prefix) && value != prefix {
									reads = append(reads, scanRead{name: value, file: y.file})
								}
							}
						}
					}
				}
				// A constant that the enumeration compares by name.
				for j := 0; j+2 < len(toks); j++ {
					if toks[j].text == "name" && toks[j+1].t == token.EQL && toks[j+2].t == token.IDENT {
						for _, value := range consts[toks[j+2].text] {
							reads = append(reads, scanRead{name: value, file: x.file})
						}
					}
				}
			}
		}
	}
	return reads
}

// enclosingParam gives the function around a position whose parameter has the name, and the index of the
// parameter, or "".
func enclosingParam(toks []tok, at int, name string) (string, int) {
	for i := at - 1; i >= 0; i-- {
		if toks[i].t != token.FUNC {
			continue
		}
		j := i + 1
		if j < len(toks) && toks[j].t == token.LPAREN {
			// A method: skip the receiver.
			for j < len(toks) && toks[j].t != token.RPAREN {
				j++
			}
			j++
		}
		if j+1 >= len(toks) || toks[j].t != token.IDENT || toks[j+1].t != token.LPAREN {
			continue
		}
		fn := toks[j].text
		index, depth := 0, 0
		for k := j + 2; k < len(toks); k++ {
			switch toks[k].t {
			case token.LPAREN, token.LBRACK, token.LBRACE:
				depth++
			case token.RPAREN, token.RBRACK, token.RBRACE:
				if depth == 0 {
					return "", -1
				}
				depth--
			case token.COMMA:
				if depth == 0 {
					index++
				}
			case token.IDENT:
				if depth == 0 && toks[k].text == name && (toks[k-1].t == token.LPAREN || toks[k-1].t == token.COMMA) {
					return fn, index
				}
			}
		}
		return "", -1
	}
	return "", -1
}

// argumentAt gives the position of the argument of the index in the call whose '(' is at open, or -1.
func argumentAt(toks []tok, open, index int) int {
	depth, current := 0, 0
	if index == 0 {
		return open + 1
	}
	for k := open + 1; k < len(toks); k++ {
		switch toks[k].t {
		case token.LPAREN, token.LBRACK, token.LBRACE:
			depth++
		case token.RPAREN, token.RBRACK, token.RBRACE:
			if depth == 0 {
				return -1
			}
			depth--
		case token.COMMA:
			if depth == 0 {
				current++
				if current == index {
					return k + 1
				}
			}
		}
	}
	return -1
}

// sourceFiles gives the non-test Go files and templates of a folder, not those of its sub-folders.
func sourceFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasSuffix(name, "_test.go") || !(strings.HasSuffix(name, ".go") || strings.HasSuffix(name, ".tmpl")) {
			continue
		}
		files = append(files, filepath.Join(dir, name))
	}
	return files
}

// libraryRead is a library that the components use which reads environment variables by itself, the names it reads,
// and the components that own it and declare each name (#344 security review M2). The service refuses most of them
// at start (gen.EnvironmentGuardSource). The OpenTelemetry SDK reads OTEL_ variables too: otel-tracing refuses at
// start each OTEL_ name that it does not declare, so they need no declaration.
type libraryRead struct {
	label  string
	owners []string
	names  []string
}

// libraryReads gives the variables that the pgx driver, grpc-go and the Go runtime and standard library read: for
// a module, each name of its pattern in its source at the version that the runtime module requires.
func libraryReads(t *testing.T, root string) []libraryRead {
	t.Helper()
	moduleDir := func(module string) string {
		command := exec.Command("go", "mod", "download", "-json", module)
		command.Dir = filepath.Join(root, "registry", "runtime")
		output, err := command.Output()
		if err != nil {
			t.Fatalf("go mod download %s: %v", module, err)
		}
		var download struct{ Dir string }
		if err := json.Unmarshal(output, &download); err != nil || download.Dir == "" {
			t.Fatalf("go mod download %s gives no folder: %v", module, err)
		}
		return download.Dir
	}
	moduleNames := func(module, sub, pattern string, skip ...string) []string {
		download := struct{ Dir string }{moduleDir(module)}
		var err error
		literal := regexp.MustCompile(`"(` + pattern + `)"`)
		found := map[string]bool{}
		err = filepath.WalkDir(filepath.Join(download.Dir, sub), func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if slices.ContainsFunc(skip, func(s string) bool { return strings.Contains(path, s) }) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, match := range literal.FindAllStringSubmatch(string(data), -1) {
				found[match[1]] = true
			}
			return nil
		})
		if err != nil || len(found) == 0 {
			t.Fatalf("the module %s holds no name of %s: %v", module, pattern, err)
		}
		return slices.Sorted(maps.Keys(found))
	}
	// pgconn reads HOME through os.UserHomeDir for its default pass file, service file and certificate files
	// (#353 security review L2). The opener and the client give each of those settings themselves.
	homeRead := func(module, file string) []string {
		data, err := os.ReadFile(filepath.Join(moduleDir(module), file))
		if err != nil || !strings.Contains(string(data), "os.UserHomeDir") {
			t.Fatalf("the module %s no longer reads the home folder in %s, or the file is gone: %v", module, file, err)
		}
		return []string{"HOME"}
	}
	return []libraryRead{
		{label: "pgx", owners: []string{"postgres-adapter", "postgres-client"}, names: moduleNames("github.com/jackc/pgx/v5", "pgconn", `PG[A-Z_]+`)},
		{label: "pgx (the home folder)", owners: []string{"postgres-adapter", "postgres-client"}, names: homeRead("github.com/jackc/pgx/v5", "pgconn/defaults.go")},
		{label: "grpc-go", owners: []string{"otel-tracing"}, names: moduleNames("google.golang.org/grpc", "", `GRPC_[A-Z0-9_]+`,
			"grpctest", "testutils", "interop", "examples", "benchmark", "/test/")},
		{label: "the Go runtime and standard library", owners: []string{"command"},
			names: []string{"GODEBUG", "GOGC", "GOMAXPROCS", "GOMEMLIMIT", "GOTRACEBACK", "SSL_CERT_DIR", "SSL_CERT_FILE", "TZ"}},
	}
}

// packageFiles gives the source files of a generator package and of its sub-packages, such as grpcgen, out of
// testdata.
func packageFiles(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || !entry.IsDir() {
			return err
		}
		if entry.Name() == "testdata" {
			return filepath.SkipDir
		}
		files = append(files, sourceFiles(t, path)...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// scanUnits gives the units of the scan: each generator package with the components it serves, each compiler file
// that emits a read, and each runtime package with the components that wire it.
func scanUnits(t *testing.T, root string) ([]scanUnit, []scanUnit, []scanUnit) {
	t.Helper()
	byPackage := map[string][]string{}
	for name, generator := range defaultGenerators() {
		value := reflect.TypeOf(generator)
		for value.Kind() == reflect.Pointer {
			value = value.Elem()
		}
		dir := strings.TrimPrefix(value.PkgPath(), "github.com/jsell-rh/stego/")
		byPackage[dir] = append(byPackage[dir], name)
	}
	var generators []scanUnit
	for dir, owners := range byPackage {
		slices.Sort(owners)
		files := packageFiles(t, filepath.Join(root, dir))
		for _, other := range emitsAlso[dir] {
			files = append(files, packageFiles(t, filepath.Join(root, other))...)
		}
		generators = append(generators, scanUnit{label: dir, files: files, owners: owners})
	}
	var compilerUnits []scanUnit
	for _, file := range sourceFiles(t, filepath.Join(root, "internal", "compiler")) {
		compilerUnits = append(compilerUnits, scanUnit{label: filepath.Base(file), files: []string{file}, owners: compilerOwners[filepath.Base(file)]})
	}
	var runtimeUnits []scanUnit
	runtimeRoot := filepath.Join(root, "registry", "runtime")
	err := filepath.WalkDir(runtimeRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil || !entry.IsDir() {
			return err
		}
		if entry.Name() == "testdata" || strings.HasSuffix(entry.Name(), "test") {
			return filepath.SkipDir
		}
		relative, _ := filepath.Rel(runtimeRoot, path)
		runtimeUnits = append(runtimeUnits, scanUnit{label: "registry/runtime/" + filepath.ToSlash(relative), files: sourceFiles(t, path), owners: runtimeOwners[filepath.ToSlash(relative)]})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return generators, compilerUnits, runtimeUnits
}

// checkScan gives each fault of the scan: a read that no owner declares, a read whose name the scan cannot
// follow, and a declared variable that no unit of its component reads.
func checkScan(t *testing.T, root string, reg *registry.Registry) []string {
	t.Helper()
	generators, compilerUnits, runtimeUnits := scanUnits(t, root)
	declared := map[string]map[string]bool{}
	// client are the variables that only a client program of a component reads (read_by_client), by name: no
	// runtime package, no compiler file, no library and no generator of another component may read one, because
	// the service never gets a value for it (security review of PR #465, I2).
	client := map[string][]string{}
	for name, component := range reg.Components() {
		declared[name] = map[string]bool{}
		for _, variable := range component.Variables() {
			declared[name][variable.Name] = true
			if variable.ReadByClient {
				client[variable.Name] = append(client[variable.Name], name)
			}
		}
	}
	// serverRead gives the fault of a read of a client variable by server code, or nothing. This is the split of
	// client and server code in the scan: a runtime package, a compiler file and a library are server code, so a
	// read of a client variable by one of them is a fault. A generator package holds the code that emits both the
	// service and the client program of its component, and the scan of the package as a whole cannot tell the two
	// apart. So a generator unit (generator true) may read the variable when each of its owners declares it
	// read_by_client, and emittedClientFaults then splits what that generator emits by path: a file under a folder
	// with an emitted cmd/main.go is client code, and each other file is server code (security review of PR #468, L2,
	// and of PR #471, I3).
	serverRead := func(where, name string, owners []string, generator bool) string {
		if readers, only := client[name]; only && (!generator || slices.ContainsFunc(owners, func(o string) bool { return !slices.Contains(readers, o) })) {
			return where + " reads the environment variable " + name + ", which only a client program of " + strings.Join(readers, ", ") + " reads (read_by_client); the service never gets a value for it"
		}
		return ""
	}
	read := map[string]map[string]bool{}
	var faults []string
	for _, group := range []struct {
		units     []scanUnit
		emits     bool
		generator bool
	}{{generators, true, true}, {compilerUnits, true, false}, {runtimeUnits, false, false}} {
		for _, unit := range group.units {
			for _, r := range unitReads(t, unit, group.emits) {
				relative, _ := filepath.Rel(root, r.file)
				switch {
				case slices.Contains(compilerOwn[filepath.Base(r.file)], r.name) && strings.Contains(r.file, "internal/compiler"):
					continue
				case strings.HasPrefix(r.unresolved, "declaration") && slices.ContainsFunc(unit.owners, func(o string) bool { return slices.Contains(declarationNamed, o) }):
					continue
				case r.unresolved != "":
					faults = append(faults, relative+": a read of the environment whose name the scan cannot follow: "+r.unresolved)
					continue
				case len(unit.owners) == 0:
					faults = append(faults, relative+": reads the environment variable "+r.name+", but no component owns the file")
					continue
				}
				if fault := serverRead(relative, r.name, unit.owners, group.generator); fault != "" {
					faults = append(faults, fault)
				}
				found := false
				for _, owner := range unit.owners {
					if read[owner] == nil {
						read[owner] = map[string]bool{}
					}
					read[owner][r.name] = true
					found = found || declared[owner][r.name]
				}
				if !found {
					faults = append(faults, "component "+strings.Join(unit.owners, " or ")+": "+relative+" reads the environment variable "+r.name+", which the component does not declare")
				}
			}
		}
	}
	for _, library := range libraryReads(t, root) {
		for _, owner := range library.owners {
			if read[owner] == nil {
				read[owner] = map[string]bool{}
			}
			for _, name := range library.names {
				if fault := serverRead("the library "+library.label, name, library.owners, false); fault != "" {
					faults = append(faults, fault)
				}
				read[owner][name] = true
				if !declared[owner][name] {
					faults = append(faults, "component "+owner+": the library "+library.label+" reads the environment variable "+name+", which the component does not declare")
				}
			}
		}
	}
	faults = append(faults, emittedClientFaults(t, reg, client)...)
	for component, names := range declared {
		for name := range names {
			if !read[component][name] {
				faults = append(faults, "component "+component+": declares the environment variable "+name+", which none of its code reads")
			}
		}
	}
	slices.Sort(faults)
	return faults
}

// emittedClientFaults runs the generator of each component that declares a variable read by a client program alone
// in its fixture (clientFixtures), and gives the faults of what it emits (emittedFaults). The scan of the generator
// package holds the reads of the package as a whole, so it cannot see server code that the generator of the same
// component emits (security review of PR #468, L2).
func emittedClientFaults(t *testing.T, reg *registry.Registry, client map[string][]string) []string {
	t.Helper()
	var owners []string
	for _, readers := range client {
		owners = append(owners, readers...)
	}
	slices.Sort(owners)
	var faults []string
	for _, name := range slices.Compact(owners) {
		ctx, ok := clientFixtures[name]
		generator := defaultGenerators()[name]
		if !ok || generator == nil {
			faults = append(faults, "component "+name+": declares a variable read by a client program alone, and the scan has no fixture to run its generator in (clientFixtures)")
			continue
		}
		files, wiring, err := generator.Generate(ctx)
		if err != nil {
			faults = append(faults, "component "+name+": its generator fails in the fixture of the scan: "+err.Error())
			continue
		}
		var own []string
		for _, variable := range reg.Component(name).Variables() {
			if variable.ReadByClient {
				own = append(own, variable.Name)
			}
		}
		faults = append(faults, emittedFaults(t, name, files, wiring, client, own)...)
	}
	return faults
}

// emittedFaults scans each file that the generator of the component emits by its path. A file under a folder that
// holds an emitted cmd/main.go is a file of a client program; each other file, and the wiring that the assembler
// puts in the main package of the service, is server code. Server code may read no variable that only a client
// program reads (client), and may read no variable whose name the scan cannot follow; the client programs must read
// each such variable of the component (own), so that the scan sees the read that it holds apart.
func emittedFaults(t *testing.T, component string, files []gen.File, wiring *gen.Wiring, client map[string][]string, own []string) []string {
	t.Helper()
	dir := t.TempDir()
	var roots []string
	for _, f := range files {
		if strings.HasSuffix(f.Path, "/cmd/main.go") {
			roots = append(roots, strings.TrimSuffix(f.Path, "cmd/main.go"))
		}
	}
	unit := scanUnit{label: component, owners: []string{component}}
	byPath := map[string]string{}
	write := func(relative string, content []byte) {
		path := filepath.Join(dir, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
		unit.files = append(unit.files, path)
		byPath[path] = relative
	}
	for _, f := range files {
		if strings.HasSuffix(f.Path, ".go") {
			write(f.Path, f.Content)
		}
	}
	const wired = "the wiring of the main package of the service"
	if wiring != nil {
		var code strings.Builder
		code.WriteString("package main\n\nfunc wired() {\n")
		value := reflect.ValueOf(*wiring)
		for i := range value.NumField() {
			field := value.Field(i)
			switch {
			case field.Kind() == reflect.String:
				code.WriteString(field.String() + "\n")
			case field.Kind() == reflect.Slice && field.Type().Elem().Kind() == reflect.String:
				for j := range field.Len() {
					code.WriteString(field.Index(j).String() + "\n")
				}
			}
		}
		code.WriteString("}\n")
		write(wired, []byte(code.String()))
	}
	clientRead := map[string]bool{}
	var faults []string
	for _, r := range unitReads(t, unit, false) {
		relative := byPath[r.file]
		if relative != wired && slices.ContainsFunc(roots, func(root string) bool { return strings.HasPrefix(relative, root) }) {
			clientRead[r.name] = true
			continue
		}
		where := "component " + component + ": the emitted server file " + relative
		if relative == wired {
			where = "component " + component + ": " + wired
		}
		switch readers, only := client[r.name]; {
		case r.unresolved != "":
			faults = append(faults, where+": a read of the environment whose name the scan cannot follow: "+r.unresolved)
		case only:
			faults = append(faults, where+" reads the environment variable "+r.name+", which only a client program of "+strings.Join(readers, ", ")+" reads (read_by_client); the service never gets a value for it")
		}
	}
	for _, name := range own {
		if !clientRead[name] {
			faults = append(faults, "component "+component+": no emitted client program reads the environment variable "+name+", which the component states as read by a client program alone")
		}
	}
	return faults
}

func TestEnvironmentScan(t *testing.T) {
	root := filepath.Join("..", "..")
	t.Run("ST-SUP-UC01-R10-T01 Each environment variable that a component's code or runtime provider reads is declared by the component, and each declared one is read", func(t *testing.T) {
		reg, err := registry.Load(filepath.Join(root, "registry"))
		if err != nil {
			t.Fatal(err)
		}
		for _, fault := range checkScan(t, root, reg) {
			t.Error(fault)
		}
		// The start guard refuses exactly the names that pgx reads, from the same source (walkthrough 3, item 4).
		for _, library := range libraryReads(t, root) {
			if library.label == "pgx" && !slices.Equal(library.names, slices.Sorted(slices.Values(gen.PGVariables))) {
				t.Errorf("the start guard refuses the PG names %v, and pgx reads %v", gen.PGVariables, library.names)
			}
		}
	})
	t.Run("ST-SUP-UC01-R10-T02 The release scan names the component, the variable and the file of a read that is not declared", func(t *testing.T) {
		copied := shippedRegistryCopy(t)
		// Both components that render the gRPC runtime declare the address: rename it in both.
		for _, component := range []string{"grpc-application", "grpc-processes"} {
			path := filepath.Join(copied, "components", component, "component.yaml")
			data, err := os.ReadFile(path)
			if err != nil || !strings.Contains(string(data), "  - name: STEGO_GRPC_ADDR\n") {
				t.Fatalf("the component %s does not declare STEGO_GRPC_ADDR: %v", component, err)
			}
			renamed := strings.Replace(string(data), "  - name: STEGO_GRPC_ADDR\n", "  - name: STEGO_GRPC_LISTEN\n", 1)
			if err := os.WriteFile(path, []byte(renamed), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		reg, err := registry.Load(copied)
		if err != nil {
			t.Fatal(err)
		}
		faults := strings.Join(checkScan(t, root, reg), "\n")
		for _, part := range []string{"grpc-application", "STEGO_GRPC_ADDR", "internal/generator/grpcapplication/runtime.go.tmpl"} {
			if !strings.Contains(faults, part) {
				t.Errorf("the scan does not name %s:\n%s", part, faults)
			}
		}
	})
}

func TestEnvironmentScanClient(t *testing.T) {
	t.Run("ST-SUP-UC01-R10-T06 The release scan names a server package that reads a variable that only a client program reads", func(t *testing.T) {
		root := filepath.Join("..", "..")
		copied := shippedRegistryCopy(t)
		// The runtime package otel reads OTEL_SERVICE_NAME: state it as read by a client program only.
		path := filepath.Join(copied, "components", "otel-tracing", "component.yaml")
		data, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(data), "  - name: OTEL_SERVICE_NAME\n") {
			t.Fatalf("the component otel-tracing does not declare OTEL_SERVICE_NAME: %v", err)
		}
		marked := strings.Replace(string(data), "  - name: OTEL_SERVICE_NAME\n", "  - name: OTEL_SERVICE_NAME\n    read_by_client: true\n", 1)
		if err := os.WriteFile(path, []byte(marked), 0o644); err != nil {
			t.Fatal(err)
		}
		reg, err := registry.Load(copied)
		if err != nil {
			t.Fatal(err)
		}
		faults := strings.Join(checkScan(t, root, reg), "\n")
		if !strings.Contains(faults, "registry/runtime/otel") || !strings.Contains(faults, "OTEL_SERVICE_NAME, which only a client program of otel-tracing reads") {
			t.Errorf("the scan does not name the runtime package that reads the client variable OTEL_SERVICE_NAME:\n%s", faults)
		}
		// The release scan runs the generator of each component with such a variable, so a component with no
		// fixture for its generator fails it (security review of PR #468, L2).
		if !strings.Contains(faults, "component otel-tracing: declares a variable read by a client program alone, and the scan has no fixture") {
			t.Errorf("the scan does not ask for a fixture of the generator of otel-tracing:\n%s", faults)
		}
	})
	t.Run("ST-SUP-UC01-R10-T07 The release scan names server code that reads a variable of a client program, also when the generator of the same component emits it", func(t *testing.T) {
		const name = "cli-application"
		files, wiring, err := defaultGenerators()[name].Generate(clientFixtures[name])
		if err != nil {
			t.Fatal(err)
		}
		client := map[string][]string{"APPLY_CONFIG": {name}}
		own := []string{"APPLY_CONFIG"}
		if faults := emittedFaults(t, name, files, wiring, client, own); len(faults) != 0 {
			t.Fatalf("the scan of the emitted CLI gives faults:\n%s", strings.Join(faults, "\n"))
		}
		// The same generator also emits a server file and wiring that read the variable of the client program.
		server := append(slices.Clone(files), gen.File{Path: "settings/settings.go", Content: []byte("package settings\n\nimport \"os\"\n\nfunc Path() string { return os.Getenv(\"APPLY_CONFIG\") }\n")})
		more := *wiring
		more.Constructors = append(slices.Clone(more.Constructors), `settings.New(os.Getenv("APPLY_CONFIG"))`)
		faults := strings.Join(emittedFaults(t, name, server, &more, client, own), "\n")
		for _, want := range []string{
			"the emitted server file settings/settings.go reads the environment variable APPLY_CONFIG, which only a client program of cli-application reads",
			"the wiring of the main package of the service reads the environment variable APPLY_CONFIG",
		} {
			if !strings.Contains(faults, want) {
				t.Errorf("the scan does not say %q:\n%s", want, faults)
			}
		}
		// A client program that does not read the variable of the component is a fault too.
		var noRead []gen.File
		for _, f := range files {
			if !strings.Contains(string(f.Content), "ConfigEnv") {
				noRead = append(noRead, f)
			}
		}
		if faults := strings.Join(emittedFaults(t, name, noRead, wiring, client, own), "\n"); !strings.Contains(faults, "no emitted client program reads the environment variable APPLY_CONFIG") {
			t.Errorf("the scan does not see that no client program reads APPLY_CONFIG:\n%s", faults)
		}
	})
}

func TestEnvironmentScanForms(t *testing.T) {
	t.Run("ST-SUP-UC01-R10-T03 The release scan follows each form of an environment read, and fails on a form it cannot follow", func(t *testing.T) {
		dir := t.TempDir()
		source := `package x

import (
	xos "os"
	"syscall"
)

func f(s string) {
	_ = xos.Getenv("ALIASED")
	getenv := xos.Getenv
	_ = getenv("LOCAL")
	_, _ = syscall.Getenv("SYSCALL")
	_ = xos.ExpandEnv("${EXPANDED}/$ALSO")
	_ = xos.Expand(s, xos.Getenv)
	other(xos.Getenv)
}
`
		dotted := "package y\n\nimport . \"os\"\n\nfunc g() { _ = Getenv(\"DOTTED\") }\n"
		for name, text := range map[string]string{"x.go": source, "y.go": dotted} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		reads := unitReads(t, scanUnit{label: "forms", files: []string{filepath.Join(dir, "x.go"), filepath.Join(dir, "y.go")}, owners: []string{"forms"}}, false)
		var names, unresolved []string
		for _, r := range reads {
			if r.name != "" {
				names = append(names, r.name)
			} else {
				unresolved = append(unresolved, r.unresolved)
			}
		}
		slices.Sort(names)
		if strings.Join(names, " ") != "ALIASED ALSO EXPANDED LOCAL SYSCALL" {
			t.Errorf("the scan follows the names %v", names)
		}
		text := strings.Join(unresolved, "; ")
		for _, want := range []string{"an expansion of the environment", "used as a value", "a dot import"} {
			if !strings.Contains(text, want) {
				t.Errorf("the scan does not fail on %s: %s", want, text)
			}
		}
	})
}
