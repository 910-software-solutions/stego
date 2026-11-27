package assure

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The proof that each inner go command of a generator test takes the checked module cache of localtest
// (ST-STD-LOCAL-R03, security review of PR #7, L1 to L3 and N1). It is a scan of the syntax of the test files of one
// package at a time, with no type check.
//
// A command is a call of Command or CommandContext of the package os/exec, by any import name. It is in scope unless
// its program is shown not to be go: a string, a constant, a local variable, a parameter (through each call of its
// function in the package), a call of a function of the package, filepath.Join, os.Getenv, exec.LookPath or an index
// of a variadic parameter is read for the program. os.Getenv("STEGO_TEST_PINNED_GO") is go; os.Args[0] is not; any
// other program that the scan cannot read is in scope. A shell with -c and a script with the word go is in scope too.
// An exec.Cmd literal is a fault.
//
// The environment of a command in scope is each assignment of Env on the variable that holds it, after the call and
// before the variable is set again. There must be one, and each must be, as its outermost expression, a call of
// localtest.GoEnv or localtest.FillEnv, a call of a function of the package that returns one, or a variable or a
// parameter whose every value is one. An append to it is a fault. Each entry that a call of GoEnv adds must be read:
// a string, a concatenation that starts with a string, localtest.PostgresEnv, nil, or a variable, field or parameter
// whose every value is one; and it must not set a go setting that GoEnv sets (innerGoSettings), except an entry of
// innerGoAllowed.

// innerGoScope is the folder whose packages innerGoFaults reads: the generator packages, whose tests build and run
// generated modules with the checked module cache of localtest.
const innerGoScope = "internal/generator/"

// innerGoSettings are the go settings that localtest.GoEnv sets, which no entry after it may set again.
var innerGoSettings = map[string]bool{"GOMODCACHE": true, "GOPROXY": true, "GOSUMDB": true, "GONOSUMDB": true, "GONOSUMCHECK": true,
	"GOPRIVATE": true, "GONOPROXY": true, "GOINSECURE": true, "GOVCS": true, "GOAUTH": true, "GIT_TERMINAL_PROMPT": true,
	"GOENV": true, "GOFLAGS": true, "GOWORK": true, "GOTOOLCHAIN": true}

// innerGoAllowed are the only entries of a go setting that may follow GoEnv: they change no module source and no
// proxy. -mod=mod lets a command update go.mod from the checked cache; -modcacherw keeps the folders that a stego
// command extracts writable, so that the test can remove its temporary folders.
var innerGoAllowed = map[string]bool{"GOFLAGS=-mod=mod": true, "GOFLAGS=": true, "GOFLAGS=-modcacherw": true}

// goProgram is what the scan knows of the program of a command.
type goProgram int

const (
	programNotGo goProgram = iota
	programUnknown
	programGo
)

// goDef is one assignment or variable declaration: its left and right sides.
type goDef struct {
	lhs, rhs []ast.Expr
	pos      token.Pos
}

// goScope is one function body: of a declared function or method, or of a function literal.
type goScope struct {
	params   map[string]int
	variadic int
	name     string
	method   bool
	parent   *goScope
	defs     []goDef
	calls    []*ast.CallExpr
	results  []ast.Expr
	file     *ast.File
	bound    *goSite
}

// goSite is one call of a function, in the scope that makes it.
type goSite struct {
	call  *ast.CallExpr
	scope *goScope
}

// goScan is the scan of one package.
type goScan struct {
	fset   *token.FileSet
	scopes []*goScope
	funcs  map[string]*goScope
	consts map[string]ast.Expr
	fields map[string][]goSite
	faults []string
	seen   map[string]bool
}

func (g *goScan) text(node ast.Node) string {
	var b strings.Builder
	_ = printer.Fprint(&b, g.fset, node)
	return b.String()
}

func (g *goScan) where(node ast.Node) string {
	position := g.fset.Position(node.Pos())
	return fmt.Sprintf("%s:%d", position.Filename, position.Line)
}

// execName gives the import name of os/exec in the file, or "" when it does not import it.
func execName(file *ast.File) string {
	for _, spec := range file.Imports {
		if spec.Path.Value == `"os/exec"` {
			if spec.Name != nil {
				return spec.Name.Name
			}
			return "exec"
		}
	}
	return ""
}

// qualified gives the package and the name of a call such as os.Getenv, or "" and "".
func qualified(fun ast.Expr) (string, string) {
	if selector, ok := fun.(*ast.SelectorExpr); ok {
		if x, ok := selector.X.(*ast.Ident); ok {
			return x.Name, selector.Sel.Name
		}
	}
	return "", ""
}

func stringLiteral(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	text, err := strconv.Unquote(lit.Value)
	return text, err == nil
}

// newScope reads one function body into a scope, and each function literal in it into a scope of its own.
func (g *goScan) newScope(file *ast.File, kind *ast.FuncType, body *ast.BlockStmt, name string, method bool, parent *goScope) *goScope {
	sc := &goScope{params: map[string]int{}, variadic: -1, name: name, method: method, parent: parent, file: file}
	index := 0
	if kind.Params != nil {
		for _, field := range kind.Params.List {
			_, variadic := field.Type.(*ast.Ellipsis)
			names := field.Names
			if len(names) == 0 {
				index++
				continue
			}
			for _, id := range names {
				sc.params[id.Name] = index
				if variadic {
					sc.variadic = index
				}
				index++
			}
		}
	}
	g.scopes = append(g.scopes, sc)
	literalNames := map[*ast.FuncLit]string{}
	ast.Inspect(body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.FuncLit:
			g.newScope(file, n.Type, n.Body, literalNames[n], false, sc)
			return false
		case *ast.AssignStmt:
			sc.defs = append(sc.defs, goDef{lhs: n.Lhs, rhs: n.Rhs, pos: n.Pos()})
			for i, right := range n.Rhs {
				if lit, ok := right.(*ast.FuncLit); ok && i < len(n.Lhs) {
					if id, ok := n.Lhs[i].(*ast.Ident); ok {
						literalNames[lit] = id.Name
					}
				}
			}
			for i, left := range n.Lhs {
				if selector, ok := left.(*ast.SelectorExpr); ok && i < len(n.Rhs) && len(n.Lhs) == len(n.Rhs) {
					g.fields[selector.Sel.Name] = append(g.fields[selector.Sel.Name], goSite{call: &ast.CallExpr{Fun: n.Rhs[i]}, scope: sc})
				}
			}
		case *ast.ValueSpec:
			lhs := make([]ast.Expr, len(n.Names))
			for i, id := range n.Names {
				lhs[i] = id
			}
			sc.defs = append(sc.defs, goDef{lhs: lhs, rhs: n.Values, pos: n.Pos()})
		case *ast.CallExpr:
			sc.calls = append(sc.calls, n)
		case *ast.ReturnStmt:
			sc.results = append(sc.results, n.Results...)
		case *ast.CompositeLit:
			// A field set in a composite literal, such as &service{env: ...}.
			for _, elt := range n.Elts {
				if kv, ok := elt.(*ast.KeyValueExpr); ok {
					if key, ok := kv.Key.(*ast.Ident); ok {
						g.fields[key.Name] = append(g.fields[key.Name], goSite{call: &ast.CallExpr{Fun: kv.Value}, scope: sc})
					}
				}
			}
		}
		return true
	})
	return sc
}

// sites gives each call of the function of the scope: the bound call, or each call in the package.
func (g *goScan) sites(sc *goScope) []goSite {
	if sc.bound != nil {
		return []goSite{*sc.bound}
	}
	if sc.name == "" {
		return nil
	}
	var found []goSite
	for _, other := range g.scopes {
		for _, call := range other.calls {
			switch fun := call.Fun.(type) {
			case *ast.Ident:
				if !sc.method && fun.Name == sc.name && (sc.parent == nil || within(other, sc.parent)) {
					found = append(found, goSite{call: call, scope: other})
				}
			case *ast.SelectorExpr:
				if sc.method && fun.Sel.Name == sc.name {
					found = append(found, goSite{call: call, scope: other})
				}
			}
		}
	}
	return found
}

// within tells if the scope inner is the scope outer or inside it.
func within(inner, outer *goScope) bool {
	for s := inner; s != nil; s = s.parent {
		if s == outer {
			return true
		}
	}
	return false
}

// goValue is one value that an expression can have: an expression in a scope, or nil when the scan cannot read it.
type goValue struct {
	expr  ast.Expr
	scope *goScope
}

// values gives the values of the name in the scope: of its assignments in the nearest scope that defines it, and of
// each argument that a call gives it when it is a parameter. spread is true for a variadic parameter.
func (g *goScan) values(name string, sc *goScope) ([]goValue, bool, bool) {
	for s := sc; s != nil; s = s.parent {
		var found []goValue
		defined := false
		for _, def := range s.defs {
			for i, left := range def.lhs {
				id, ok := left.(*ast.Ident)
				if !ok || id.Name != name {
					continue
				}
				defined = true
				switch {
				case len(def.rhs) == len(def.lhs):
					found = append(found, goValue{def.rhs[i], s})
				case len(def.rhs) == 1 && i == 0:
					// p, err := exec.LookPath("go"): the first value of the call.
					found = append(found, goValue{def.rhs[0], s})
				case len(def.rhs) == 0:
				default:
					found = append(found, goValue{nil, s})
				}
			}
		}
		if index, ok := s.params[name]; ok {
			defined = true
			variadic := index == s.variadic
			sites := g.sites(s)
			if len(sites) == 0 {
				found = append(found, goValue{nil, s})
			}
			for _, site := range sites {
				args := site.call.Args
				switch {
				case variadic && site.call.Ellipsis.IsValid() && len(args) > index:
					found = append(found, goValue{args[len(args)-1], site.scope})
				case variadic:
					var parts []ast.Expr
					if len(args) > index {
						parts = args[index:]
					}
					found = append(found, goValue{&ast.CompositeLit{Elts: parts}, site.scope})
				case len(args) > index:
					found = append(found, goValue{args[index], site.scope})
				default:
					found = append(found, goValue{nil, site.scope})
				}
			}
			return found, true, variadic
		}
		if defined {
			return found, true, false
		}
	}
	if value, ok := g.consts[name]; ok {
		return []goValue{{value, nil}}, true, false
	}
	return nil, false, false
}

// program reads what the program expression is.
func (g *goScan) program(expr ast.Expr, sc *goScope, depth int) goProgram {
	if depth > 16 || expr == nil {
		return programUnknown
	}
	switch v := expr.(type) {
	case *ast.BasicLit:
		text, ok := stringLiteral(v)
		if !ok {
			return programUnknown
		}
		if path.Base(text) == "go" {
			return programGo
		}
		return programNotGo
	case *ast.ParenExpr:
		return g.program(v.X, sc, depth+1)
	case *ast.Ident:
		values, found, _ := g.values(v.Name, sc)
		if !found {
			return programUnknown
		}
		result := programNotGo
		for _, value := range values {
			result = max(result, g.program(value.expr, value.scope, depth+1))
		}
		return result
	case *ast.CompositeLit:
		// The variadic arguments of one call: index 0 is read by IndexExpr.
		return programUnknown
	case *ast.IndexExpr:
		if pkg, name := qualified(v.X); pkg == "os" && name == "Args" {
			return programNotGo
		}
		id, ok := v.X.(*ast.Ident)
		if !ok {
			return programUnknown
		}
		values, found, _ := g.values(id.Name, sc)
		if !found {
			return programUnknown
		}
		position := -1
		if lit, ok := v.Index.(*ast.BasicLit); ok && lit.Kind == token.INT {
			position, _ = strconv.Atoi(lit.Value)
		}
		result := programNotGo
		for _, value := range values {
			composite, ok := value.expr.(*ast.CompositeLit)
			if !ok || position < 0 {
				result = max(result, programUnknown)
				continue
			}
			if position < len(composite.Elts) {
				result = max(result, g.program(composite.Elts[position], value.scope, depth+1))
			} else {
				result = max(result, programUnknown)
			}
		}
		return result
	case *ast.CallExpr:
		pkg, name := qualified(v.Fun)
		switch {
		case pkg == "os" && name == "Getenv" && len(v.Args) == 1:
			if text, _ := stringLiteral(v.Args[0]); text == "STEGO_TEST_PINNED_GO" {
				return programGo
			}
			return programUnknown
		case (pkg == "filepath" || pkg == "path") && name == "Join" && len(v.Args) > 0:
			return g.program(v.Args[len(v.Args)-1], sc, depth+1)
		case sc != nil && pkg == execName(sc.file) && name == "LookPath" && len(v.Args) == 1:
			return g.program(v.Args[0], sc, depth+1)
		}
		if id, ok := v.Fun.(*ast.Ident); ok {
			if f := g.funcs[id.Name]; f != nil {
				if len(f.results) == 0 {
					return programUnknown
				}
				result := programNotGo
				for _, r := range f.results {
					result = max(result, g.program(r, f, depth+1))
				}
				return result
			}
		}
	}
	return programUnknown
}

// envFault gives the reason why expr is not an environment of localtest, or "".
func (g *goScan) envFault(expr ast.Expr, sc *goScope, depth int) string {
	if depth > 16 || expr == nil {
		return "an environment that the scan cannot read"
	}
	switch v := expr.(type) {
	case *ast.ParenExpr:
		return g.envFault(v.X, sc, depth+1)
	case *ast.CallExpr:
		pkg, name := qualified(v.Fun)
		if pkg == "localtest" && name == "FillEnv" {
			return ""
		}
		if pkg == "localtest" && name == "GoEnv" {
			if v.Ellipsis.IsValid() && len(v.Args) > 0 {
				for _, arg := range v.Args[:len(v.Args)-1] {
					if why := g.entryFault(arg, sc, depth+1); why != "" {
						return why
					}
				}
				return g.sliceFault(v.Args[len(v.Args)-1], sc, depth+1)
			}
			for _, arg := range v.Args {
				if why := g.entryFault(arg, sc, depth+1); why != "" {
					return why
				}
			}
			return ""
		}
		if id, ok := v.Fun.(*ast.Ident); ok {
			if f := g.funcs[id.Name]; f != nil && len(f.results) > 0 {
				for _, r := range f.results {
					if why := g.envFault(r, f, depth+1); why != "" {
						return why
					}
				}
				return ""
			}
		}
	case *ast.Ident:
		values, found, _ := g.values(v.Name, sc)
		if found && len(values) > 0 {
			for _, value := range values {
				if why := g.envFault(value.expr, value.scope, depth+1); why != "" {
					return why
				}
			}
			if why := g.mutated(v.Name, sc); why != "" {
				return why
			}
			return ""
		}
	}
	return "an environment that is not of localtest as its outermost expression: " + g.text(expr)
}

// mutated gives a reason when an element of the slice variable name is set in the scope that defines it.
func (g *goScan) mutated(name string, sc *goScope) string {
	for s := sc; s != nil; s = s.parent {
		for _, def := range s.defs {
			for _, left := range def.lhs {
				if index, ok := left.(*ast.IndexExpr); ok {
					if id, ok := index.X.(*ast.Ident); ok && id.Name == name {
						return "an environment of localtest whose entry is set after it: " + g.text(left)
					}
				}
			}
		}
	}
	return ""
}

// sliceFault gives the reason why the entries of the slice expr cannot be read or set a go setting, or "".
func (g *goScan) sliceFault(expr ast.Expr, sc *goScope, depth int) string {
	if depth > 24 || expr == nil {
		return "entries that the scan cannot read"
	}
	key := fmt.Sprintf("slice %p %d", sc, expr.Pos())
	if g.seen[key] {
		return ""
	}
	g.seen[key] = true
	switch v := expr.(type) {
	case *ast.ParenExpr:
		return g.sliceFault(v.X, sc, depth+1)
	case *ast.CompositeLit:
		for _, elt := range v.Elts {
			if why := g.entryFault(elt, sc, depth+1); why != "" {
				return why
			}
		}
		return ""
	case *ast.CallExpr:
		pkg, name := qualified(v.Fun)
		if pkg == "localtest" && name == "PostgresEnv" {
			return ""
		}
		if id, ok := v.Fun.(*ast.Ident); ok && id.Name == "append" && len(v.Args) > 0 {
			if why := g.sliceFault(v.Args[0], sc, depth+1); why != "" {
				return why
			}
			rest := v.Args[1:]
			if v.Ellipsis.IsValid() && len(rest) > 0 {
				if why := g.sliceFault(rest[len(rest)-1], sc, depth+1); why != "" {
					return why
				}
				rest = rest[:len(rest)-1]
			}
			for _, arg := range rest {
				if why := g.entryFault(arg, sc, depth+1); why != "" {
					return why
				}
			}
			return ""
		}
	case *ast.Ident:
		if v.Name == "nil" {
			return ""
		}
		values, found, _ := g.values(v.Name, sc)
		if found {
			for _, value := range values {
				if why := g.sliceFault(value.expr, value.scope, depth+1); why != "" {
					return why
				}
			}
			return ""
		}
	case *ast.SelectorExpr:
		// A field: each value that the package gives it.
		sets := g.fields[v.Sel.Name]
		if len(sets) > 0 {
			for _, set := range sets {
				if why := g.sliceFault(set.call.Fun, set.scope, depth+1); why != "" {
					return why
				}
			}
			return ""
		}
	}
	return "entries that the scan cannot read after localtest: " + g.text(expr)
}

// entryFault gives the reason why the entry expr cannot be read or sets a go setting, or "".
func (g *goScan) entryFault(expr ast.Expr, sc *goScope, depth int) string {
	if depth > 24 || expr == nil {
		return "an entry that the scan cannot read"
	}
	switch v := expr.(type) {
	case *ast.ParenExpr:
		return g.entryFault(v.X, sc, depth+1)
	case *ast.BasicLit:
		text, ok := stringLiteral(v)
		if !ok {
			break
		}
		name, _, found := strings.Cut(text, "=")
		if !found {
			return "an entry with no = after localtest: " + text
		}
		if innerGoSettings[name] && !innerGoAllowed[text] {
			return "an entry after localtest that sets the go setting " + name + ": " + text
		}
		return ""
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			break
		}
		left := v.X
		for {
			inner, ok := left.(*ast.BinaryExpr)
			if !ok || inner.Op != token.ADD {
				break
			}
			left = inner.X
		}
		text, ok := stringLiteral(left)
		if !ok {
			break
		}
		name, _, found := strings.Cut(text, "=")
		if !found {
			break
		}
		if innerGoSettings[name] {
			return "an entry after localtest that sets the go setting " + name + ": " + g.text(expr)
		}
		return ""
	case *ast.Ident:
		values, found, _ := g.values(v.Name, sc)
		if found && len(values) > 0 {
			for _, value := range values {
				if why := g.entryFault(value.expr, value.scope, depth+1); why != "" {
					return why
				}
			}
			return ""
		}
	}
	return "an entry that the scan cannot read after localtest: " + g.text(expr)
}

// commandFault checks one command in the scope.
func (g *goScan) commandFault(call *ast.CallExpr, sc *goScope, args []ast.Expr) {
	where := g.where(call)
	kind := g.program(args[0], sc, 0)
	if kind == programNotGo {
		if text, _ := stringLiteral(args[0]); (path.Base(text) == "sh" || path.Base(text) == "bash") && len(args) >= 3 {
			flag, _ := stringLiteral(args[1])
			script, ok := stringLiteral(args[2])
			if flag == "-c" && (!ok || slices.Contains(strings.Fields(script), "go")) {
				kind = programGo
			}
		}
	}
	if kind == programNotGo {
		return
	}
	variable := ""
	end := token.Pos(0)
	for _, def := range sc.defs {
		for i, right := range def.rhs {
			if right == call && i < len(def.lhs) {
				if id, ok := def.lhs[i].(*ast.Ident); ok {
					variable = id.Name
				}
			}
		}
	}
	if variable != "" {
		for _, def := range sc.defs {
			for _, left := range def.lhs {
				if id, ok := left.(*ast.Ident); ok && id.Name == variable && def.pos > call.End() && (end == 0 || def.pos < end) {
					end = def.pos
				}
			}
		}
	}
	envs := 0
	for _, def := range sc.defs {
		if variable == "" || def.pos < call.End() || end != 0 && def.pos >= end {
			continue
		}
		for i, left := range def.lhs {
			target := left
			if index, ok := left.(*ast.IndexExpr); ok {
				target = index.X
			}
			selector, ok := target.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "Env" {
				continue
			}
			if id, ok := selector.X.(*ast.Ident); !ok || id.Name != variable {
				continue
			}
			if target != left {
				g.faults = append(g.faults, fmt.Sprintf("%s runs an inner go command whose environment entry is set after localtest: %s", where, g.text(left)))
				continue
			}
			envs++
			var right ast.Expr
			if i < len(def.rhs) && len(def.rhs) == len(def.lhs) {
				right = def.rhs[i]
			}
			if why := g.envFault(right, sc, 0); why != "" {
				g.faults = append(g.faults, fmt.Sprintf("%s runs an inner go command with %s", where, why))
			}
		}
	}
	if envs == 0 {
		g.faults = append(g.faults, where+" runs an inner go command with the environment of the test, not of localtest")
	}
}

// innerGoFaults gives each inner go command of the test files of one package that does not take its environment
// from localtest, and each exec.Cmd literal. files are the _test.go files of the package, outside testdata, by name.
func innerGoFaults(files map[string][]byte) []string {
	g := &goScan{fset: token.NewFileSet(), funcs: map[string]*goScope{}, consts: map[string]ast.Expr{}, fields: map[string][]goSite{}, seen: map[string]bool{}}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	var parsed []*ast.File
	for _, name := range names {
		file, err := parser.ParseFile(g.fset, name, files[name], parser.SkipObjectResolution)
		if err != nil {
			g.faults = append(g.faults, name+" does not parse: "+err.Error())
			continue
		}
		parsed = append(parsed, file)
	}
	for _, file := range parsed {
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Body == nil {
					continue
				}
				sc := g.newScope(file, d.Type, d.Body, d.Name.Name, d.Recv != nil, nil)
				if d.Recv == nil {
					g.funcs[d.Name.Name] = sc
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if value, ok := spec.(*ast.ValueSpec); ok {
						for i, id := range value.Names {
							if i < len(value.Values) {
								g.consts[id.Name] = value.Values[i]
							}
						}
					}
				}
			}
		}
	}
	for _, sc := range slices.Clone(g.scopes) {
		exec := execName(sc.file)
		if exec == "" {
			continue
		}
		for _, call := range sc.calls {
			pkg, name := qualified(call.Fun)
			if pkg != exec || name != "Command" && name != "CommandContext" {
				continue
			}
			args := call.Args
			if name == "CommandContext" && len(args) > 0 {
				args = args[1:]
			}
			if len(args) == 0 {
				continue
			}
			// A command whose program is a parameter of a helper is checked once for each call of the helper, with the
			// arguments of that call, so a helper that runs git with os.Environ() and go with GoEnv is read call by call.
			if id, ok := args[0].(*ast.Ident); ok {
				if _, param := sc.params[id.Name]; param && sc.bound == nil {
					sites := g.sites(sc)
					for _, site := range sites {
						bound := *sc
						bound.bound = &goSite{call: site.call, scope: site.scope}
						g.commandFault(call, &bound, args)
					}
					if len(sites) > 0 {
						continue
					}
				}
			}
			g.commandFault(call, sc, args)
		}
	}
	for _, file := range parsed {
		exec := execName(file)
		ast.Inspect(file, func(node ast.Node) bool {
			if lit, ok := node.(*ast.CompositeLit); ok && exec != "" {
				if pkg, name := qualified(lit.Type); pkg == exec && name == "Cmd" {
					g.faults = append(g.faults, g.where(lit)+" builds an exec.Cmd by hand, which the scan cannot read")
				}
			}
			return true
		})
	}
	slices.Sort(g.faults)
	return slices.Compact(g.faults)
}

// innerGoPlants are the bypasses of the first form of this check (security review of PR #7, N1), each as a test
// file of a package with one inner go command; the check must name a fault in each. clean is a package that it must
// pass.
var innerGoPlants = map[string]string{
	"helper with the pinned Go in a variable": `
func (s *service) goTool(args ...string) ([]byte, error) {
	name := os.Getenv("STEGO_TEST_PINNED_GO")
	if name == "" {
		name = filepath.Join(runtime.GOROOT(), "bin", "go")
	}
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "GOWORK=off")
	return cmd.CombinedOutput()
}`,
	"environment variable from os.Environ given to a helper": `
func command(t *testing.T, dir string, env []string, name string, args ...string) string {
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Env = dir, env
	out, _ := cmd.CombinedOutput()
	return string(out)
}
func pinnedGo() string { return filepath.Join(runtime.GOROOT(), "bin", "go") }
func TestX(t *testing.T) {
	command(t, ".", os.Environ(), "git", "init")
	env := append(os.Environ(), "GOFLAGS=-mod=mod")
	command(t, ".", env, pinnedGo(), "build")
}`,
	"go settings appended after GoEnv": `
func TestX(t *testing.T) {
	build := exec.Command("go", "build", "-race", "./out/cli/cmd")
	build.Env = append(localtest.GoEnv(), "GOMODCACHE=/home/u/go/pkg/mod", "GOPROXY=direct", "GOFLAGS=-mod=mod")
	_ = build.Run()
}`,
	"go setting given to GoEnv": `
func TestX(t *testing.T) {
	build := exec.Command("go", "build")
	build.Env = localtest.GoEnv("GOPROXY=direct")
	_ = build.Run()
}`,
	"go setting given to GoEnv through a concatenation": `
func TestX(t *testing.T) {
	build := exec.Command("go", "build")
	build.Env = localtest.GoEnv("GOMODCACHE=" + os.Getenv("HOME"))
	_ = build.Run()
}`,
	"Env appended to after GoEnv": `
func TestX(t *testing.T) {
	download := exec.Command("go", "mod", "download")
	download.Env = localtest.GoEnv()
	download.Env = append(download.Env, "GOMODCACHE=/shared", "GOPROXY=direct")
	_ = download.Run()
}`,
	"Env entry set after GoEnv": `
func TestX(t *testing.T) {
	download := exec.Command("go", "mod", "download")
	download.Env = localtest.GoEnv()
	download.Env[0] = "GOPROXY=direct"
	_ = download.Run()
}`,
	"held environment changed after GoEnv": `
func TestX(t *testing.T) {
	env := localtest.GoEnv()
	env = append(env, "GOPROXY=direct")
	cmd := exec.Command("go", "build")
	cmd.Env = env
	_ = cmd.Run()
}`,
	"no Env": `
func TestX(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "go", "build")
	_ = cmd.Run()
}`,
	"direct run with no variable": `
func TestX(t *testing.T) {
	_ = exec.Command("go", "build").Run()
}`,
	"constant program": `
const goBinary = "go"
func TestX(t *testing.T) {
	cmd := exec.Command(goBinary, "build")
	cmd.Env = os.Environ()
	_ = cmd.Run()
}`,
	"program from LookPath": `
func TestX(t *testing.T) {
	tool, _ := exec.LookPath("go")
	cmd := exec.Command(tool, "build")
	_ = cmd.Run()
}`,
	"program that the scan cannot read": `
func TestX(t *testing.T) {
	cmd := exec.Command(strings.TrimSpace(os.Getenv("GO")), "build")
	_ = cmd.Run()
}`,
	"aliased import of os/exec": `
func TestX(t *testing.T) {
	cmd := run.Command("go", "build")
	_ = cmd.Run()
}`,
	"go through a shell": `
func TestX(t *testing.T) {
	cmd := exec.Command("sh", "-c", "cd out && go build ./...")
	_ = cmd.Run()
}`,
	"exec.Cmd literal": `
func TestX(t *testing.T) {
	cmd := &exec.Cmd{Path: "/usr/local/go/bin/go", Args: []string{"go", "build"}}
	_ = cmd.Run()
}`,
	"closure with the program as its first argument": `
func TestX(t *testing.T) {
	run := func(args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Env = append(os.Environ(), "GOWORK=off")
		_ = cmd.Run()
	}
	run("git", "init")
	run("go", "build")
}`,
}

// innerGoClean is a package that the check must pass: each form that the generator tests use.
const innerGoClean = `
type service struct{ env []string }
func pinnedGo() string {
	if name := os.Getenv("STEGO_TEST_PINNED_GO"); name != "" {
		return name
	}
	return filepath.Join(runtime.GOROOT(), "bin", "go")
}
func goEnv(extra ...string) []string {
	return localtest.GoEnv(append([]string{"PATH=" + os.Getenv("PATH")}, extra...)...)
}
func command(t *testing.T, dir string, env []string, name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Env = dir, env
	_ = cmd.Run()
}
func (s *service) goTool(args ...string) {
	name := os.Getenv("STEGO_TEST_PINNED_GO")
	cmd := exec.Command(name, args...)
	cmd.Env = localtest.GoEnv(append([]string{"GOFLAGS=-mod=mod"}, append(localtest.PostgresEnv(t), s.env...)...)...)
	_ = cmd.Run()
}
func TestX(t *testing.T) {
	s := &service{}
	s.env = append(s.env, "STEGO_TEST_SPICEDB_ENDPOINT="+os.Getenv("X"))
	command(t, ".", os.Environ(), "git", "init")
	command(t, ".", goEnv("STEGO_MODULE=example.com/x"), pinnedGo(), "build")
	postgres := "STEGO_TEST_POSTGRES_DSN=" + os.Getenv("DSN")
	build := exec.Command("go", "build")
	build.Env = localtest.GoEnv(postgres, "GOFLAGS=")
	_ = build.Run()
	vuln := exec.Command("go", "run", "golang.org/x/vuln/cmd/govulncheck@v1.4.0")
	vuln.Env = localtest.FillEnv(t)
	_ = vuln.Run()
	binary := filepath.Join(t.TempDir(), "cli")
	run := exec.Command(binary, "version")
	run.Env = append(os.Environ(), "X=1")
	_ = run.Run()
	again := exec.Command(os.Args[0], "-test.run=^TestX$")
	_ = again.Run()
}`

// plantPackage gives the planted test file body as a package of one file.
func plantPackage(body string) map[string][]byte {
	imports := "import (\n\t\"context\"\n\t\"os\"\n\t\"os/exec\"\n\t\"path/filepath\"\n\t\"runtime\"\n\t\"strings\"\n\t\"testing\"\n\n\t\"github.com/910-software-solutions/stego/internal/localtest\"\n)\n"
	if strings.Contains(body, "run.Command") {
		imports = strings.Replace(imports, "\t\"os/exec\"\n", "\trun \"os/exec\"\n", 1)
	}
	return map[string][]byte{"internal/generator/plant/plant_test.go": []byte("package plant\n\n" + imports + body + "\n")}
}

// TestInnerGoCommandsTakeTheCheckedCache reads the test files of each generator package at HEAD whose TestMain calls
// localtest.MainModules, and fails on each inner go command of them that does not take the environment of localtest.
// It first shows that the scan names a fault in each planted bypass and none in the clean package.
func TestInnerGoCommandsTakeTheCheckedCache(t *testing.T) {
	t.Run("ST-STD-LOCAL-R03-T12 Each inner go command of a generator test takes the checked module cache of localtest", func(t *testing.T) {
		for name, body := range innerGoPlants {
			if faults := innerGoFaults(plantPackage(body)); len(faults) == 0 {
				t.Errorf("the scan names no fault in the plant %q", name)
			}
		}
		if faults := innerGoFaults(plantPackage(innerGoClean)); len(faults) > 0 {
			t.Errorf("the scan names faults in the clean package:\n%s", strings.Join(faults, "\n"))
		}
		_, tree := readHead(t)
		packages := map[string]map[string][]byte{}
		for _, name := range tree.paths() {
			if !strings.HasPrefix(name, innerGoScope) || !strings.HasSuffix(name, "_test.go") || strings.Contains(name, "/testdata/") {
				continue
			}
			data, _ := tree.File(name)
			if packages[path.Dir(name)] == nil {
				packages[path.Dir(name)] = map[string][]byte{}
			}
			packages[path.Dir(name)][name] = data
		}
		var faults []string
		checked := 0
		for _, files := range packages {
			main := false
			for _, data := range files {
				main = main || strings.Contains(strings.Join(codeLines("", data), "\n"), "localtest.MainModules(")
			}
			if !main {
				continue
			}
			checked++
			faults = append(faults, innerGoFaults(files)...)
		}
		if checked == 0 {
			t.Fatal("no generator package at HEAD calls localtest.MainModules: the scan read nothing")
		}
		report(t, "ST-STD-LOCAL-R03", faults)
	})
}
