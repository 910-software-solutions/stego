package assure

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// runtimeTables are the tables of the schema manifests of the runtime library (ST-SUP-RP15-LC01), and
// runtimeFunctions its functions that a statement calls.
var (
	runtimeTables = []string{"messages", "audit_event", "audit_chain_heads", "audit_anchor_boundaries", "audit_retention_checkpoints",
		"idempotency_keys", "authz_floor", "authz_revocations", "authz_revocation_tuples", "leases", "stego_tenants"}
	runtimeFunctions = []string{"claim", "move", "release", "purge_parked", "purge_done", "blocked", "oldest", "resolve",
		"audit_maintain", "idempotency_keys_purge", "authz_revocation_state"}
)

// declaredTable is a table of the schema manifest that a generated service declares.
var declaredTable = regexp.MustCompile(`\{Schema: "[a-z_][a-z0-9_]*", Name: "([a-z_][a-z0-9_]*)"`)

// ownerDDL are the files whose statements only the owner role runs, in the migration job, and which make the
// objects in the schema of the migration (ST-STD-TXN-R19 names the statements of the runtime, worker and relay
// roles).
var ownerDDL = []string{"registry/runtime/idempotency/migration.go"}

// unknown stands for a part of a string expression that is no constant the scanner knows, and argument for an
// argument of a format that it does not know.
const (
	unknown  = "\x00"
	argument = "\x01"
)

// goPackages holds the string constants and package-level string variables of each folder of Go files that the
// scanner reads, the imports of each folder, and the folder of each module by its path, so a selector such as
// audit.EventsTable is folded too, also when its constant is a selector of a third package. A package-level
// variable that a function of its package sets again is not folded: its value is unknown. An unexported field of a
// struct, which only its package can set, stands for the text that each expression it is set to gives, when all of
// them give the same text, such as a schema, a dot and a table.
type goPackages struct {
	values  map[string]map[string]ast.Expr   // folder -> name -> expression
	fields  map[string]map[string][]ast.Expr // folder -> unexported field -> each expression it is set to
	imports map[string]map[string]string     // folder -> import name -> folder
	modules map[string]string                // module path -> folder
}

// index reads the constants and string variables of each Go file of paths, and the module of each go.mod of tree.
func index(tree Tree, paths []string) goPackages {
	g := goPackages{values: map[string]map[string]ast.Expr{}, fields: map[string]map[string][]ast.Expr{}, imports: map[string]map[string]string{}, modules: map[string]string{}}
	module := regexp.MustCompile(`(?m)^module\s+(\S+)`)
	for _, p := range tree.paths() {
		if path.Base(p) == "go.mod" {
			data, _ := tree.File(p)
			if m := module.FindSubmatch(data); m != nil {
				g.modules[string(m[1])] = path.Dir(p)
			}
		}
	}
	set := map[string]map[string]bool{}
	var files []*ast.File
	var dirs []string
	for _, p := range paths {
		if path.Ext(p) != ".go" {
			continue
		}
		data, _ := tree.File(p)
		f, err := parser.ParseFile(token.NewFileSet(), p, data, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		files, dirs = append(files, f), append(dirs, path.Dir(p))
	}
	for i, f := range files {
		dir := dirs[i]
		if g.values[dir] == nil {
			g.values[dir], g.imports[dir], set[dir] = map[string]ast.Expr{}, map[string]string{}, map[string]bool{}
			g.fields[dir] = map[string][]ast.Expr{}
		}
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || (gen.Tok != token.CONST && gen.Tok != token.VAR) {
				continue
			}
			for _, spec := range gen.Specs {
				v := spec.(*ast.ValueSpec)
				for i, n := range v.Names {
					if i < len(v.Values) {
						g.values[dir][n.Name] = v.Values[i]
					}
				}
			}
		}
		unexported := func(name string) bool { return name != "" && name[0] >= 'a' && name[0] <= 'z' }
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				for i, l := range n.Lhs {
					switch l := l.(type) {
					case *ast.Ident:
						if n.Tok != token.DEFINE {
							set[dir][l.Name] = true
						}
					case *ast.SelectorExpr:
						if unexported(l.Sel.Name) {
							var v ast.Expr = &ast.BadExpr{}
							if n.Tok == token.ASSIGN && len(n.Lhs) == len(n.Rhs) {
								v = n.Rhs[i]
							}
							g.fields[dir][l.Sel.Name] = append(g.fields[dir][l.Sel.Name], v)
						}
					}
				}
			case *ast.CompositeLit:
				for _, el := range n.Elts {
					if kv, ok := el.(*ast.KeyValueExpr); ok {
						if key, ok := kv.Key.(*ast.Ident); ok && unexported(key.Name) {
							g.fields[dir][key.Name] = append(g.fields[dir][key.Name], kv.Value)
						}
					}
				}
			}
			return true
		})
	}
	for i, f := range files {
		for _, spec := range f.Imports {
			importPath, _ := strconv.Unquote(spec.Path.Value)
			name := path.Base(importPath)
			if spec.Name != nil {
				name = spec.Name.Name
			}
			if other := g.folder(importPath); other != "" {
				g.imports[dirs[i]][name] = other
			}
		}
	}
	for dir, names := range set {
		for name := range names {
			delete(g.values[dir], name)
		}
	}
	return g
}

// folder gives the folder of the package of an import path, or "" when no module of the tree holds it.
func (g goPackages) folder(importPath string) string {
	for module, dir := range g.modules {
		if importPath == module {
			return dir
		}
		if rest, ok := strings.CutPrefix(importPath, module+"/"); ok {
			return path.Join(dir, rest)
		}
	}
	return ""
}

// locals gives the local variables of a function body that are set once, with the expression they are set to. A
// variable that is set again, with = or +=, in a loop or a branch, is left out, so its value is unknown.
func locals(body ast.Node) map[string]ast.Expr {
	values, count := map[string]ast.Expr{}, map[string]int{}
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			for i, l := range n.Lhs {
				if id, ok := l.(*ast.Ident); ok {
					count[id.Name]++
					if n.Tok == token.DEFINE && len(n.Lhs) == len(n.Rhs) {
						values[id.Name] = n.Rhs[i]
					}
				}
			}
		case *ast.ValueSpec:
			for i, id := range n.Names {
				count[id.Name]++
				if i < len(n.Values) {
					values[id.Name] = n.Values[i]
				}
			}
		}
		return true
	})
	for name, c := range count {
		if c != 1 {
			delete(values, name)
		}
	}
	return values
}

// verb is a verb of a format, with its argument index if it has one.
var verb = regexp.MustCompile(`%[-+# 0]*(?:\[([0-9]+)\])?[0-9]*(?:\.[0-9]+)?(?:\[([0-9]+)\])?[a-zA-Z%]`)

// sqlTexts gives the texts of the Go file p that a statement can be made of: each string expression that is not
// part of a larger one, with each + folded over the constants and string variables of its package and of the
// packages it imports and over the local variables that are set once; each fmt.Sprintf and fmt.Fprintf with its
// known arguments in place of their verbs, %[n] verbs too, and argument for each other one; strings.ReplaceAll,
// strings.Replace, the Replace of a strings.NewReplacer and strings.Join of a list it knows; each other part is
// unknown.
func sqlTexts(file *ast.File, p string, g goPackages) []string {
	type scope struct {
		dir    string
		locals map[string]ast.Expr
	}
	var eval func(e ast.Expr, s scope, depth int) (string, bool)
	fieldTexts := map[string]string{}
	known := func(e ast.Expr, s scope, depth int) (string, bool) {
		v, ok := eval(e, s, depth)
		return v, ok && !strings.ContainsAny(v, unknown+argument)
	}
	// value gives the expression that an identifier of the scope stands for.
	value := func(e ast.Expr, s scope) (ast.Expr, scope, bool) {
		switch e := e.(type) {
		case *ast.Ident:
			if v, ok := s.locals[e.Name]; ok {
				return v, s, true
			}
			if v, ok := g.values[s.dir][e.Name]; ok {
				return v, scope{dir: s.dir}, true
			}
		case *ast.SelectorExpr:
			if x, ok := e.X.(*ast.Ident); ok {
				if other := g.imports[s.dir][x.Name]; other != "" {
					if v, ok := g.values[other][e.Sel.Name]; ok {
						return v, scope{dir: other}, true
					}
				}
			}
		}
		return nil, s, false
	}
	call := func(e *ast.CallExpr, pkg, name string) bool {
		sel, ok := e.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != name {
			return false
		}
		x, ok := sel.X.(*ast.Ident)
		return ok && x.Name == pkg
	}
	eval = func(e ast.Expr, s scope, depth int) (string, bool) {
		if depth > 32 {
			return unknown, false
		}
		switch e := e.(type) {
		case *ast.BasicLit:
			if e.Kind == token.STRING {
				v, err := strconv.Unquote(e.Value)
				return v, err == nil
			}
		case *ast.ParenExpr:
			return eval(e.X, s, depth+1)
		case *ast.Ident, *ast.SelectorExpr:
			if v, vs, ok := value(e, s); ok {
				return eval(v, vs, depth+1)
			}
			if sel, ok := e.(*ast.SelectorExpr); ok {
				key := s.dir + "\x00" + sel.Sel.Name
				if v, ok := fieldTexts[key]; ok {
					return v, v != unknown
				}
				if sets := g.fields[s.dir][sel.Sel.Name]; len(sets) > 0 {
					// A field that its own expressions name is unknown while they are folded.
					fieldTexts[key] = unknown
					first, _ := eval(sets[0], scope{dir: s.dir}, depth+1)
					for _, set := range sets[1:] {
						if v, _ := eval(set, scope{dir: s.dir}, depth+1); v != first {
							first = unknown
							break
						}
					}
					fieldTexts[key] = first
					return first, first != unknown
				}
			}
		case *ast.BinaryExpr:
			if e.Op == token.ADD {
				x, xok := eval(e.X, s, depth+1)
				y, yok := eval(e.Y, s, depth+1)
				return x + y, xok || yok
			}
		case *ast.CallExpr:
			switch {
			case (call(e, "fmt", "Sprintf") && len(e.Args) > 0) || (call(e, "fmt", "Fprintf") && len(e.Args) > 1):
				args := e.Args
				if call(e, "fmt", "Fprintf") {
					args = args[1:]
				}
				format, ok := eval(args[0], s, depth+1)
				if !ok {
					return unknown, false
				}
				args = args[1:]
				i := 0
				return verb.ReplaceAllStringFunc(format, func(v string) string {
					if v == "%%" {
						return "%"
					}
					for _, n := range verb.FindStringSubmatch(v)[1:] {
						if n != "" {
							i, _ = strconv.Atoi(n)
							i--
						}
					}
					if i < 0 || i >= len(args) {
						return argument
					}
					a, ok := known(args[i], s, depth+1)
					i++
					if !ok {
						return argument
					}
					return a
				}), true
			case (call(e, "strings", "ReplaceAll") && len(e.Args) == 3) || (call(e, "strings", "Replace") && len(e.Args) == 4):
				text, ok := eval(e.Args[0], s, depth+1)
				if !ok {
					return unknown, false
				}
				old, ook := known(e.Args[1], s, depth+1)
				if !ook || old == "" {
					return text, true
				}
				with, wok := known(e.Args[2], s, depth+1)
				if !wok {
					with = argument
				}
				return strings.ReplaceAll(text, old, with), true
			case call(e, "strings", "Join") && len(e.Args) == 2:
				list, ok := e.Args[0].(*ast.CompositeLit)
				sep, sok := known(e.Args[1], s, depth+1)
				if !ok || !sok {
					return unknown, false
				}
				var parts []string
				for _, el := range list.Elts {
					v, _ := eval(el, s, depth+1)
					parts = append(parts, v)
				}
				return strings.Join(parts, sep), true
			}
			// The Replace of a strings.NewReplacer, made in place or kept in a variable.
			if sel, ok := e.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Replace" && len(e.Args) == 1 {
				r, rs := ast.Expr(sel.X), s
				for range 4 {
					v, vs, ok := value(r, rs)
					if !ok {
						break
					}
					r, rs = v, vs
				}
				if made, ok := r.(*ast.CallExpr); ok && call(made, "strings", "NewReplacer") {
					text, ok := eval(e.Args[0], s, depth+1)
					if !ok {
						return unknown, false
					}
					var pairs []string
					for i, a := range made.Args {
						v, ok := known(a, rs, depth+1)
						if !ok {
							if i%2 == 0 {
								return text, true
							}
							v = argument
						}
						pairs = append(pairs, v)
					}
					if len(pairs)%2 == 0 {
						return strings.NewReplacer(pairs...).Replace(text), true
					}
					return text, true
				}
			}
		}
		return unknown, false
	}
	var texts []string
	var inspect func(n ast.Node, s scope)
	inspect = func(root ast.Node, s scope) {
		ast.Inspect(root, func(n ast.Node) bool {
			if n == root {
				return true
			}
			switch n := n.(type) {
			case *ast.FuncDecl:
				if n.Body != nil {
					inspect(n.Body, scope{dir: s.dir, locals: locals(n.Body)})
				}
				return false
			case *ast.FuncLit:
				inner := maps.Clone(s.locals)
				if inner == nil {
					inner = map[string]ast.Expr{}
				}
				maps.Copy(inner, locals(n.Body))
				inspect(n.Body, scope{dir: s.dir, locals: inner})
				return false
			case *ast.BinaryExpr, *ast.CallExpr:
				if v, ok := eval(n.(ast.Expr), s, 0); ok {
					texts = append(texts, v)
					if _, call := n.(*ast.CallExpr); !call {
						return false
					}
				}
			case *ast.BasicLit:
				if v, ok := eval(n, s, 0); ok {
					texts = append(texts, v)
				}
			}
			return true
		})
	}
	inspect(file, scope{dir: path.Dir(p)})
	return texts
}

// templateLiterals gives the Go string literals of a .go.tmpl file, with each action of the template in them as
// argument. It skips the Go comments, the Go code and the actions outside a literal.
func templateLiterals(src string) []string {
	var out []string
	for i := 0; i < len(src); {
		rest := src[i:]
		switch {
		case strings.HasPrefix(rest, "{{"):
			j := strings.Index(rest, "}}")
			if j < 0 {
				return out
			}
			i += j + 2
		case strings.HasPrefix(rest, "//"):
			j := strings.IndexByte(rest, '\n')
			if j < 0 {
				return out
			}
			i += j
		case strings.HasPrefix(rest, "/*"):
			j := strings.Index(rest[2:], "*/")
			if j < 0 {
				return out
			}
			i += j + 4
		case rest[0] == '`':
			j := strings.IndexByte(rest[1:], '`')
			if j < 0 {
				return out
			}
			out = append(out, rest[1:1+j])
			i += j + 2
		case rest[0] == '"' || rest[0] == '\'':
			j := 1
			for j < len(rest) && rest[j] != rest[0] && rest[j] != '\n' {
				if rest[j] == '\\' {
					j++
				}
				j++
			}
			if rest[0] == '"' {
				out = append(out, rest[1:min(j, len(rest))])
			}
			i += j + 1
		default:
			i++
		}
	}
	return out
}

// withoutComments gives the SQL text with each comment, -- to the end of its line and /* to */, as a space. A
// comment marker in a string literal, '...' or "...", is no comment, so the literal is kept; the body of a
// dollar-quoted string is SQL, such as the body of a function, so its comments are removed too (security review of
// PR #266, L5).
func withoutComments(text string) string {
	var b strings.Builder
	for i := 0; i < len(text); {
		switch c := text[i]; {
		case c == '\'' || c == '"':
			j := i + 1
			for j < len(text) {
				if text[j] == c {
					if j+1 < len(text) && text[j+1] == c {
						j += 2
						continue
					}
					break
				}
				j++
			}
			end := min(j+1, len(text))
			b.WriteString(text[i:end])
			i = end
		case strings.HasPrefix(text[i:], "--"):
			j := strings.IndexByte(text[i:], '\n')
			if j < 0 {
				return b.String()
			}
			b.WriteByte(' ')
			i += j
		case strings.HasPrefix(text[i:], "/*"):
			j := strings.Index(text[i+2:], "*/")
			if j < 0 {
				return b.String()
			}
			b.WriteByte(' ')
			i += j + 4
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// unqualifiedSQL gives each statement text of a file of tree, in the runtime library, in the generated code of an
// example, or in a template of a generator, that names a table or a function of a schema manifest with no schema,
// or a table that a format argument or another value names. It reads the Go files that are not tests, with their
// constants folded, the string literals of the .go.tmpl files, and the other .sql and .tmpl files whole, with each
// action of a template as an argument, and matches across lines. A .sql or .tmpl file is a statement with no
// comments. A text that starts as a statement starts, after SQL comments, or that starts with a word that names a
// table after it, such as the FROM of a part, is matched in any case; in another text, such as prose, only the
// keywords in upper case count, but for a table of a manifest after FROM, JOIN, INTO or UPDATE, in any case. A common table expression with the name of a table of a manifest is that table,
// and the cursor that a FETCH at the start of a statement reads is no table. A text that ends with a table keyword
// is a statement whose table another value names, and the role list of a GRANT or a REVOKE is no table. The files
// of ownerDDL give their constants to the fold, and are not read for statements.
func unqualifiedSQL(tree Tree) []string {
	tables := slices.Clone(runtimeTables)
	var files, read []string
	for _, p := range tree.paths() {
		runtime := strings.HasPrefix(p, "registry/runtime/")
		generated := strings.HasPrefix(p, "examples/") && strings.Contains(p, "/out/")
		template := path.Ext(p) == ".tmpl" && (strings.HasPrefix(p, "internal/generator/") || strings.HasPrefix(p, "internal/compiler/"))
		ext := path.Ext(p)
		if (!runtime && !generated && !template) || strings.HasSuffix(p, "_test.go") || !slices.Contains([]string{".go", ".sql", ".tmpl"}, ext) {
			continue
		}
		read = append(read, p)
		if slices.Contains(ownerDDL, p) {
			continue
		}
		files = append(files, p)
		if strings.HasSuffix(p, "/schema_manifest.go") {
			data, _ := tree.File(p)
			for _, m := range declaredTable.FindAllStringSubmatch(string(data), -1) {
				if !slices.Contains(tables, m[1]) {
					tables = append(tables, m[1])
				}
			}
		}
	}
	g := index(tree, read)
	slices.Sort(tables)
	name := `\\?"?(` + strings.Join(tables, "|") + `)\\?"?`
	end := `(?:[^a-z0-9_.(]|$)`
	keyword := `\b(?:FROM|INTO|UPDATE|JOIN|TRUNCATE(?:\s+TABLE)?|LOCK(?:\s+TABLE)?|COPY|TABLE|USING)\s+(?:ONLY\s+)?`
	type search struct {
		forms             []*regexp.Regexp
		argument, unknown *regexp.Regexp
		ends              *regexp.Regexp
		function          *regexp.Regexp
	}
	searches := map[bool]search{}
	for _, statement := range []bool{true, false} {
		flags := `(?s)`
		if statement {
			flags = `(?is)`
		}
		names := `(?i:` + name + `)`
		// A value in place of a table counts only after a keyword in upper case, so prose is not read as a statement.
		value := func(marker string) *regexp.Regexp {
			return regexp.MustCompile(`(?s)` + keyword + `"?` + marker + `"?(?:[^.\x00\x01"]|$)`)
		}
		before := `(?:\b(?:SELECT|FROM|PERFORM|CALL)\s+(?:\*\s+FROM\s+)?`
		if statement {
			before += `|[,(]\s*`
		}
		searches[statement] = search{
			forms: []*regexp.Regexp{
				// A table after a keyword that names one.
				regexp.MustCompile(flags + keyword + names + end),
				// A table of a manifest after FROM, JOIN, INTO or UPDATE, in any case in any text: prose seldom names
				// one there, and a part of a statement may start with any word (security review of PR #266, L6).
				regexp.MustCompile(`(?is)\b(?:FROM|JOIN|INTO|UPDATE)\s+(?:ONLY\s+)?` + names + end),
				// A table after a comma of a FROM list.
				regexp.MustCompile(flags + `\bFROM\s+[a-z0-9_."]+(?:\s+(?:AS\s+)?[a-z_]+)?(?:\s*,\s*[a-z0-9_."]+(?:\s+(?:AS\s+)?[a-z_]+)?)*\s*,\s*` + names + end),
				// A table in a regclass cast.
				regexp.MustCompile(`(?i)'` + name + `'::(?:pg_catalog\.)?regclass`),
			},
			argument: value(argument),
			unknown:  value(unknown),
			ends:     regexp.MustCompile(`(?s)\b(?:FROM|INTO|UPDATE|JOIN|TRUNCATE|LOCK|COPY|TABLE|USING)\s+"?$`),
			function: regexp.MustCompile(flags + before + `)(?i:(` + strings.Join(runtimeFunctions, "|") + `))\s*\(`),
		}
	}
	// A statement starts with one of these words, after white space, parentheses and SQL comments; a part of a
	// statement starts with a word that names a table after it.
	head := regexp.MustCompile(`(?is)^[\s(]*(?:SELECT|INSERT|UPDATE|DELETE|WITH|LOCK|TRUNCATE|COPY|TABLE|CREATE|ALTER|DROP|GRANT|REVOKE|CALL|PERFORM|DO|VALUES|MERGE|DECLARE|EXPLAIN|FETCH|SET|RESET|BEGIN|START|EXECUTE|PREPARE|REFRESH|VACUUM|ANALYZE|CLUSTER|REINDEX|COMMENT|IMPORT|FROM|INTO|JOIN|USING)\b`)
	leadingComments := regexp.MustCompile(`(?s)^(?:\s*(?:--[^\n]*(?:\n|$)|/\*.*?\*/))+`)
	// The words FROM and UPDATE where they name no table: the FROM of a FETCH at the start of a statement names a
	// cursor, after the direction and the count of the FETCH.
	notTable := regexp.MustCompile(`(?is)(?:^|;)[\s(]*FETCH\s+(?:(?:NEXT|PRIOR|FIRST|LAST|ABSOLUTE|RELATIVE|FORWARD|BACKWARD|ALL|[+-]?[0-9]+|[\x00\x01])\s+){0,2}(?:FROM|IN)\b|\bFOR\s+(?:NO\s+KEY\s+)?UPDATE\b|\bFOR\s+(?:KEY\s+)?SHARE\b|\bDO\s+UPDATE\b|\bDISTINCT\s+FROM\b`)
	// The role list of a GRANT or a REVOKE, after its TO or its FROM: of a statement in any case, and in upper case
	// in other text. The part before it, which names the object, is still read.
	roles := regexp.MustCompile(`(?is)((?:^|;)[\s(]*GRANT\b[^;]*?)\sTO\s[^;]*|((?:^|;)[\s(]*REVOKE\b[^;]*?)\sFROM\s[^;]*|(?-i:(\bGRANT\b[^;]*?)\sTO\s[^;]*|(\bREVOKE\b[^;]*?)\sFROM\s[^;]*)`)
	action := regexp.MustCompile(`(?s)\{\{.*?\}\}`)
	var faults []string
	for _, p := range files {
		data, _ := tree.File(p)
		texts := []string{string(data)}
		switch {
		case strings.HasSuffix(p, ".go.tmpl"):
			texts = templateLiterals(string(data))
		case path.Ext(p) == ".go":
			file, err := parser.ParseFile(token.NewFileSet(), p, data, parser.SkipObjectResolution)
			if err != nil {
				faults = append(faults, fmt.Sprintf("%s: the file does not parse: %v", p, err))
				continue
			}
			texts = sqlTexts(file, p, g)
		}
		seen := map[string]bool{}
		literals := strings.HasSuffix(p, ".go.tmpl")
		// A .sql or .tmpl file is SQL in each part but its comments, so it is read as a statement with no comments.
		whole := path.Ext(p) != ".go" && !literals
		for _, text := range texts {
			text = action.ReplaceAllString(text, argument)
			if whole {
				text = withoutComments(text)
			}
			statement := whole || head.MatchString(leadingComments.ReplaceAllString(text, ""))
			s := searches[statement]
			// A common table expression with the name of a table of a manifest counts as that table: its own body,
			// a query around it and the other statements of the text name the real table.
			for _, form := range s.forms {
				for _, m := range form.FindAllStringSubmatch(text, -1) {
					seen["the table "+strings.ToLower(m[1])] = true
				}
			}
			for _, m := range s.function.FindAllStringSubmatch(text, -1) {
				seen["the function "+strings.ToLower(m[1])] = true
			}
			// The literals of a .go.tmpl file are parts of a statement whose other parts are Go code that the scanner
			// cannot fold; the generated code of the examples is the proof of those parts.
			if literals {
				continue
			}
			text = roles.ReplaceAllString(notTable.ReplaceAllString(text, " "), "$1$2$3$4 ")
			if s.argument.MatchString(text) {
				seen["the table that a format argument names"] = true
			}
			if s.unknown.MatchString(text) || s.ends.MatchString(text) {
				seen["the table that another value names"] = true
			}
		}
		for _, what := range slices.Sorted(maps.Keys(seen)) {
			faults = append(faults, fmt.Sprintf("%s: %s with no schema", p, what))
		}
	}
	return faults
}

func TestQualifiedSQL(t *testing.T) {
	t.Run("ST-STD-TXN-R19-T01 No statement of the runtime library or of a generated service names a table or a function of a schema manifest with no schema", func(t *testing.T) {
		root, err := RepositoryRoot(".")
		if err != nil {
			t.Fatal(err)
		}
		tree, err := ReadCommittedTree(root, "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		if faults := unqualifiedSQL(tree); len(faults) > 0 {
			t.Fatalf("%d statements name a table or a function with no schema:\n%s", len(faults), strings.Join(faults, "\n"))
		}
		// The predicate finds each form that it refuses, in a file of its own, and no qualified form.
		forms := map[string]string{
			"from":       "const a = `SELECT * FROM leases WHERE name = $1`",
			"folded":     "const t = \"leases\"\nconst a = `SELECT x\n FROM ` + t + ` WHERE y`",
			"into":       "const a = `INSERT INTO \"orders\" (id) VALUES ($1)`",
			"update":     "func f() { _ = \"UPDATE \" + \"messages SET x = 1\" }",
			"join":       "const a = `SELECT 1 FROM public.orders o JOIN leases l ON true`",
			"truncate":   "const a = `TRUNCATE leases`",
			"lock":       "const a = `LOCK TABLE leases IN SHARE MODE`",
			"copy":       "const a = `COPY leases TO STDOUT`",
			"table":      "const a = `TABLE leases`",
			"using":      "const a = `DELETE FROM public.orders USING leases WHERE true`",
			"comma":      "const a = `SELECT 1 FROM public.orders o, leases l WHERE true`",
			"regclass":   "const a = `SELECT 'leases'::regclass`",
			"selected":   "const a = `SELECT idempotency_keys_purge($1)`",
			"after":      "const a = `SELECT 1, claim($1)`",
			"paren":      "const a = `SELECT coalesce((oldest($1)), 0)`",
			"call":       "const a = `CALL resolve($1)`",
			"sql":        "",
			"selector":   "import \"github.com/910-software-solutions/stego/registry/runtime/names\"\n\nconst a = `SELECT 1 FROM ` + names.Leases",
			"variable":   "var t = \"leases\"\n\nvar a = `DELETE FROM ` + t",
			"sprintf":    "import \"fmt\"\n\nfunc f() string { return fmt.Sprintf(\"SELECT 1 FROM %s WHERE %s\", \"leases\", \"x\") }",
			"argument":   "import \"fmt\"\n\nfunc f(t string) string { return fmt.Sprintf(\"UPDATE %s SET x = 1\", t) }",
			"owner":      "import \"github.com/910-software-solutions/stego/registry/runtime/idempotency\"\n\nconst a = `SELECT 1 FROM ` + idempotency.Keys",
			"replaceall": "import \"strings\"\n\nconst tmpl = \"SELECT 1 FROM {t} WHERE x\"\n\nfunc f() string { return strings.ReplaceAll(tmpl, \"{t}\", \"leases\") }",
			"replacer":   "import \"strings\"\n\nvar r = strings.NewReplacer(\"{t}\", \"leases\")\n\nfunc f() string { return r.Replace(\"DELETE FROM {t}\") }",
			"reassigned": "var t = \"public.leases\"\n\nfunc init() { t = \"x\" }\n\nfunc f() string { return \"SELECT 1 FROM \" + t }",
			"local":      "func f() string {\n\tt := \"leases\"\n\treturn \"SELECT 1 FROM \" + t\n}",
			"loop":       "func f(ts []string) string {\n\tq := \"SELECT 1\"\n\tfor _, t := range ts {\n\t\tq += \" FROM \" + t\n\t}\n\treturn q\n}",
			"unknown":    "func f(t string) string { return \"SELECT 1 FROM \" + t + \" WHERE x\" }",
			"action":     "const q = \"SELECT 1 FROM {{.Table}} WHERE x\"",
			"joined":     "import \"strings\"\n\nfunc f() string { return strings.Join([]string{\"SELECT 1 FROM\", \"leases\"}, \" \") }",
			"builder":    "import \"strings\"\n\nfunc f(b *strings.Builder, t string) {\n\tb.WriteString(\"DELETE FROM \")\n\tb.WriteString(t)\n}",
			"chain":      "import \"github.com/910-software-solutions/stego/registry/runtime/alias\"\n\nconst a = \"SELECT 1 FROM \" + alias.Leases",
			"indexed":    "import \"fmt\"\n\nfunc f() string { return fmt.Sprintf(\"SELECT %[2]s FROM %[1]s\", \"leases\", \"x\") }",
			"fprintf":    "import (\n\t\"fmt\"\n\t\"io\"\n)\n\nfunc f(w io.Writer) { fmt.Fprintf(w, \"SELECT 1 FROM %s\", \"leases\") }",
			// A common table expression with the name of a table: its own body, a query around it and a later
			// statement name the real table (security review of PR #266, M1).
			"ctebody":      "const a = `WITH leases AS (DELETE FROM leases WHERE x RETURNING *) SELECT * FROM public.y`",
			"cteouter":     "const a = `SELECT * FROM (WITH leases AS (SELECT 1) SELECT * FROM leases) s JOIN leases l ON true`",
			"cterecursive": "const a = `WITH RECURSIVE leases AS (SELECT * FROM leases) SELECT 1`",
			// Lower-case SQL after a comment, after a head word such as SET, and in a part of a statement (M2).
			"lowercomment": "const a = `-- note\nlock table leases`",
			"lowerset":     "const a = `SET LOCAL lock_timeout = 1; truncate leases`",
			"lowerpart":    "func f() string {\n\tq := \"SELECT 1\"\n\tfor i := 0; i < 2; i++ {\n\t\tq += \" using leases\"\n\t}\n\treturn q\n}",
			// The word grant in a value, a FETCH that is an alias, and the object of a REVOKE (L1, L3).
			"grantword":  "func f(t string) string { return \"SELECT 'grant' AS k, x FROM \" + t }",
			"fetchalias": "func f(n, t string) string { return \"SELECT x.fetch \" + n + \" FROM \" + t }",
			"revoked":    "func f(t, r string) string { return \"REVOKE ALL ON TABLE \" + t + \" FROM \" + r }",
			// Lower-case SQL in a part that starts with no head word, and a table after FROM in prose, as main reads
			// them (security review of PR #266, L6).
			"lowwhere": "func f() string {\n\tq := \"SELECT 1 \"\n\tq += \"where x in (select 1 from leases)\"\n\treturn q\n}",
			"lowand":   "func f() string {\n\tq := \"SELECT 1 \"\n\tq += \"and exists (select 1 from leases)\"\n\treturn q\n}",
			"lowcomma": "func f() string {\n\tq := \"SELECT a\"\n\tq += \", b from leases\"\n\treturn q\n}",
			"prose":    "import \"errors\"\n\nvar e = errors.New(\"cannot read from leases\")",
			"fetched":  "func f(n, c, t string) string { return \"FETCH FORWARD \" + n + \" FROM \" + c + \"; DELETE FROM \" + t }",
			"field":    "type e struct{ table string }\n\nfunc f() string {\n\tx := e{table: \"leases\"}\n\treturn \"SELECT 1 FROM \" + x.table\n}",
			// The forms that are no unqualified statement: a common table expression, prose, a function name in prose,
			// a field that is always a schema and a table, FOR UPDATE, the roles of a REVOKE, and the cursor of a FETCH.
			"clean": "import \"errors\"\n\ntype q struct{ table string }\n\nfunc g(schema, name string) q { return q{table: schema + \".\" + name} }\n\n" +
				"func f(v q, role string) []any {\n\tstep := \"lock \" + role\n\treturn []any{step, errors.New(\"cannot read the leases\"), \"move (the key), claim (the row)\",\n" +
				"\t\t\"WITH rows AS (SELECT 1) SELECT * FROM rows\", \"SELECT 1 FROM \" + v.table + \" WHERE x\",\n" +
				"\t\t\"SELECT x FROM public.leases WHERE k = $1 FOR UPDATE \", \"REVOKE ALL ON public.leases FROM \" + role,\n" +
				"\t\t\"FETCH FORWARD \" + role + \" FROM \" + role, \"FETCH NEXT IN \" + role, \"FETCH 10 FROM \" + role}\n}",
			"qualified": "import \"fmt\"\n\nfunc f(t string) string { return fmt.Sprintf(\"UPDATE public.%s SET x = 1\", t) }\n\nconst a = `SELECT public.leases.x FROM public.leases, stego_outbox.messages m WHERE stego_outbox.claim($1) AND 'public.leases'::regclass IS NOT NULL`\n// FROM leases in a comment is no statement\n",
		}
		files := map[string][]byte{"examples/e/out/internal/storage/schema_manifest.go": []byte("package storage\n\nvar m = `{Schema: \"public\", Name: \"orders\",`\n"),
			"registry/runtime/go.mod":                   []byte("module github.com/910-software-solutions/stego/registry/runtime\n"),
			"registry/runtime/names/names.go":           []byte("package names\n\nconst Leases = \"leases\"\n"),
			"registry/runtime/idempotency/migration.go": []byte("package idempotency\n\nconst Keys = \"idempotency_keys\"\n\nconst ddl = `ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY`\n"),
			"internal/generator/g/query.go.tmpl":        []byte("const q = `SELECT 1 FROM leases WHERE {{.Key}}`\n"),
			"internal/generator/g/low.go.tmpl":          []byte("var q = \"SELECT 1\" + \" from leases\"\n"),
			"internal/generator/g/c.sql.tmpl":           []byte("-- header\ndelete from leases;\n"),
			// A comment marker in a string literal is no comment (L5).
			"internal/generator/g/strcomment.sql.tmpl": []byte("SELECT '/*';\nDELETE FROM leases;\nSELECT '*/';\n"),
			"internal/generator/g/strdash.sql.tmpl":    []byte("SELECT 'a\n-- DELETE FROM leases\n';\n"),
			// A file that starts with no head word is still a statement.
			"internal/generator/g/w.sql.tmpl":   []byte("{{.Prelude}}\ntruncate leases;\n"),
			"internal/generator/g/cte.sql.tmpl": []byte("WITH leases AS (SELECT 1) SELECT * FROM public.x;\nDELETE FROM leases;\n"),
			// The comments of a .sql file are no statement.
			"internal/generator/g/clean.sql.tmpl": []byte("-- the rows from leases\n/* and into leases */\nSELECT 1 FROM public.leases WHERE {{.Key}};\n" +
				"DO $do$ BEGIN -- reads from leases\n\tPERFORM 1 FROM public.leases; END $do$;\nSELECT '--', \"a--b\" FROM public.leases;\n"),
			"registry/runtime/alias/alias.go": []byte("package alias\n\nimport \"github.com/910-software-solutions/stego/registry/runtime/names\"\n\nconst Leases = names.Leases\n"),
			// A template whose Go comments and Go code, and its text outside a literal, name a table or a function.
			"internal/generator/g/clean.go.tmpl": []byte("// SELECT 1 FROM leases\nfunc f() { _ = move(1); _ = x(claim(2)) }\n{{if .X}}FROM leases{{end}}\nvar s = \"LOCK TABLE \" + name\n"),
		}
		for name, body := range forms {
			if name == "sql" {
				files["examples/e/out/internal/tenancy/migrations/x.sql"] = []byte("LOCK TABLE leases;\n")
				continue
			}
			files["registry/runtime/"+name+"/"+name+".go"] = []byte("package " + name + "\n\n" + body + "\n")
		}
		got := unqualifiedSQL(NewTree(files))
		var want []string
		for name, what := range map[string]string{"from": "table leases", "folded": "table leases", "into": "table orders", "update": "table messages",
			"join": "table leases", "truncate": "table leases", "lock": "table leases", "copy": "table leases", "table": "table leases",
			"using": "table leases", "comma": "table leases", "regclass": "table leases", "selected": "function idempotency_keys_purge",
			"after": "function claim", "paren": "function oldest", "call": "function resolve", "selector": "table leases", "variable": "table leases",
			"sprintf": "table leases", "argument": "table that a format argument names", "owner": "table idempotency_keys",
			"replaceall": "table leases", "replacer": "table leases", "reassigned": "table that another value names", "local": "table leases",
			"loop": "table that another value names", "unknown": "table that another value names", "action": "table that a format argument names",
			"joined": "table leases", "builder": "table that another value names", "chain": "table leases", "indexed": "table leases",
			"fprintf": "table leases", "field": "table leases", "fetched": "table that another value names",
			"ctebody": "table leases", "cteouter": "table leases", "cterecursive": "table leases", "lowercomment": "table leases",
			"lowerset": "table leases", "lowerpart": "table leases", "grantword": "table that another value names",
			"fetchalias": "table that another value names", "revoked": "table that another value names",
			"lowwhere": "table leases", "lowand": "table leases", "lowcomma": "table leases", "prose": "table leases"} {
			want = append(want, "registry/runtime/"+name+"/"+name+".go: the "+what+" with no schema")
		}
		want = append(want, "examples/e/out/internal/tenancy/migrations/x.sql: the table leases with no schema",
			"internal/generator/g/query.go.tmpl: the table leases with no schema",
			"internal/generator/g/low.go.tmpl: the table leases with no schema",
			"internal/generator/g/c.sql.tmpl: the table leases with no schema",
			"internal/generator/g/w.sql.tmpl: the table leases with no schema",
			"internal/generator/g/strcomment.sql.tmpl: the table leases with no schema",
			"internal/generator/g/strdash.sql.tmpl: the table leases with no schema",
			"internal/generator/g/cte.sql.tmpl: the table leases with no schema")
		slices.Sort(want)
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Fatalf("the predicate gave:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	})
}
