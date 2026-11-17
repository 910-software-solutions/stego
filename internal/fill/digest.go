package fill

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/format"
	goparser "go/parser"
	"go/token"
	"io/fs"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/types"
	"gopkg.in/yaml.v3"
)

// Digest is the fill digest (ST-EXT-DS01): the SHA-256 digest of every file
// in the fill's folder, by its path in the folder and its content, in the
// order of the paths. The fill manifest counts without its evidence and its
// sign-off, so recording either never changes the digest. It refuses a
// folder that holds a symbolic link or another file that is not regular.
func Digest(dir string) (string, error) {
	files, err := FileDigests(dir)
	if err != nil {
		return "", err
	}
	return combine(files), nil
}

// FileDigests gives the digest of each file in the fill's folder, by its
// slash-separated path in the folder, as Digest counts it.
func FileDigests(dir string) (map[string]string, error) {
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(dir, name)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if !entry.Type().IsRegular() {
			return fmt.Errorf("fill file %s is not a regular file", relative)
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		if relative == ManifestFile {
			data = manifestContent(data)
		}
		sum := sha256.Sum256(data)
		files[relative] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// manifestContent is the fill manifest as the digest counts it: the YAML
// document with its evidence and its sign-off left out, written again in one
// form. A manifest that is not a YAML mapping counts as its bytes.
func manifestContent(data []byte) []byte {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return data
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return data
	}
	removeKey(document.Content[0], "evidence")
	removeKey(document.Content[0], "sign_off")
	out, err := encodeNode(&document)
	if err != nil {
		return data
	}
	return out
}

// combine is the digest of a set of named contents: each name and the digest
// of its content, with their lengths, in the order of the names.
func combine(files map[string]string) string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	hash := sha256.New()
	for _, name := range names {
		fmt.Fprintf(hash, "%s:%s\x00%s\n", strconv.Itoa(len(name)), name, files[name])
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// CapabilityContractDigest is the contract digest of the fill of the command,
// query or worker slot ref (ST-EXT-DS01, decision on ST-EXT-Q30): the
// SHA-256 digest of each top-level declaration of the generated contract
// package (compiler.FillCheck.CapabilityContract) that the fill compiles
// against, by its file and its name, printed by gofmt, in the order of the
// paths and names. Those declarations are the ones of the command, query or
// worker that binds the fill, whose names start with its Go name; each
// declaration that the fill's Go files name through the contract package; and
// each declaration that one of them names, with the methods of each type. The
// protobuf descriptor plumbing (file_*) counts through the message types,
// not as itself. So a new command, query or worker leaves the evidence of the
// other fills of its package fresh, and a change to a declaration that the
// fill uses makes it stale.
func CapabilityContractDigest(check compiler.FillCheck, fill string, ref types.SlotRef) (string, error) {
	if check.CapabilityContract == nil {
		return "", fmt.Errorf("the contract of the slot %s is not generated", ref)
	}
	files, err := check.CapabilityContract(ref.Component)
	if err != nil {
		return "", err
	}
	// The Go names of the commands, queries or workers of the slot, and those of the bindings of this fill.
	var all, own []string
	for _, binding := range check.Bindings {
		if binding.Owner == "" || binding.Slot != ref.Slot || binding.GoName == "" {
			continue
		}
		all = append(all, binding.GoName)
		if binding.Fill == fill {
			own = append(own, binding.GoName)
		}
	}
	decls := contractDecls{byName: map[string][]contractDecl{}, methods: map[string][]contractDecl{}}
	fset := token.NewFileSet()
	paths := make([]string, 0, len(files))
	for name := range files {
		paths = append(paths, name)
	}
	sort.Strings(paths)
	for _, name := range paths {
		file, err := goparser.ParseFile(fset, name, files[name], goparser.SkipObjectResolution)
		if err != nil {
			return "", fmt.Errorf("the generated contract %s cannot be read: %w", name, err)
		}
		if err := decls.add(fset, name, file); err != nil {
			return "", err
		}
	}
	var roots []string
	for name := range decls.byName {
		if owner := ownerOf(name, all); owner != "" && slices.Contains(own, owner) {
			roots = append(roots, name)
		}
	}
	used, err := contractNamesUsed(filepath.Join(check.ProjectDir, "fills", fill), slotContracts(check, fill, ref.String())[0])
	if err != nil {
		return "", err
	}
	roots = append(roots, used...)
	contents := map[string]string{}
	seen := map[string]bool{}
	for len(roots) > 0 {
		name := roots[0]
		roots = roots[1:]
		if seen[name] || strings.HasPrefix(name, "file_") {
			continue
		}
		seen[name] = true
		for _, decl := range append(append([]contractDecl{}, decls.byName[name]...), decls.methods[name]...) {
			contents[decl.key] = decl.digest
			for _, ref := range decl.names {
				if _, declared := decls.byName[ref]; declared && !seen[ref] {
					roots = append(roots, ref)
				}
			}
		}
	}
	return combine(contents), nil
}

// contractDecl is one top-level declaration of a contract package: its key (file and name), the digest of its gofmt
// form, and the identifiers it names.
type contractDecl struct {
	key, digest string
	names       []string
}

// contractDecls holds the top-level declarations of a contract package by their names, and the methods by the name
// of their receiver type.
type contractDecls struct {
	byName  map[string][]contractDecl
	methods map[string][]contractDecl
}

// add indexes each top-level declaration of a file of the contract package.
func (d contractDecls) add(fset *token.FileSet, path string, file *ast.File) error {
	record := func(into map[string][]contractDecl, name, key string, node ast.Node) error {
		var buf bytes.Buffer
		if err := format.Node(&buf, fset, node); err != nil {
			return err
		}
		sum := sha256.Sum256(buf.Bytes())
		var names []string
		ast.Inspect(node, func(n ast.Node) bool {
			if ident, ok := n.(*ast.Ident); ok {
				names = append(names, ident.Name)
			}
			return true
		})
		into[name] = append(into[name], contractDecl{key: path + " " + key, digest: hex.EncodeToString(sum[:]), names: names})
		return nil
	}
	for _, decl := range file.Decls {
		switch decl := decl.(type) {
		case *ast.FuncDecl:
			if decl.Recv == nil {
				if err := record(d.byName, decl.Name.Name, decl.Name.Name, decl); err != nil {
					return err
				}
				continue
			}
			receiver := receiverName(decl.Recv.List[0].Type)
			if err := record(d.methods, receiver, receiver+"."+decl.Name.Name, decl); err != nil {
				return err
			}
		case *ast.GenDecl:
			for _, spec := range decl.Specs {
				switch spec := spec.(type) {
				case *ast.TypeSpec:
					if err := record(d.byName, spec.Name.Name, spec.Name.Name, spec); err != nil {
						return err
					}
				case *ast.ValueSpec:
					for _, name := range spec.Names {
						if name.Name == "_" {
							continue
						}
						if err := record(d.byName, name.Name, name.Name, spec); err != nil {
							return err
						}
					}
				}
			}
		}
	}
	return nil
}

// receiverName gives the name of the type of a method receiver, such as T for *T or T[P].
func receiverName(expr ast.Expr) string {
	for {
		switch e := expr.(type) {
		case *ast.StarExpr:
			expr = e.X
		case *ast.IndexExpr:
			expr = e.X
		case *ast.IndexListExpr:
			expr = e.X
		case *ast.Ident:
			return e.Name
		default:
			return ""
		}
	}
}

// ownerOf gives the Go name of the command, query or worker that a declaration of the contract belongs to: the
// longest of the names that the declaration's name starts with, also with a lower-case first letter; "" for a
// declaration that the package shares.
func ownerOf(name string, goNames []string) string {
	owner := ""
	for _, goName := range goNames {
		lower := strings.ToLower(goName[:1]) + goName[1:]
		if (strings.HasPrefix(name, goName) || strings.HasPrefix(name, lower)) && len(goName) > len(owner) {
			owner = goName
		}
	}
	return owner
}

// contractNamesUsed gives the names that the Go files of the fill folder select from the contract package, such as
// PlaceOrderFill for commands.PlaceOrderFill.
func contractNamesUsed(dir, contract string) ([]string, error) {
	var names []string
	fset := token.NewFileSet()
	err := filepath.WalkDir(dir, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(name, ".go") {
			return nil
		}
		file, err := goparser.ParseFile(fset, name, nil, goparser.SkipObjectResolution)
		if err != nil {
			// A file that does not parse names nothing; its tests do not pass, so it gets no evidence.
			return nil
		}
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil || path != contract {
				continue
			}
			alias := pathpkg.Base(path)
			if spec.Name != nil {
				alias = spec.Name.Name
			}
			ast.Inspect(file, func(n ast.Node) bool {
				if selector, ok := n.(*ast.SelectorExpr); ok {
					if x, ok := selector.X.(*ast.Ident); ok && x.Name == alias {
						names = append(names, selector.Sel.Name)
					}
				}
				return true
			})
		}
		return nil
	})
	return names, err
}

// ContractFiles reads a slot contract and the files it imports from the
// active registry. The registry gives both methods.
type ContractFiles interface {
	ReadFile(name string) ([]byte, error)
	ReadProtoImport(name string) ([]byte, error)
}

// protoImport matches one import statement of a .proto file.
var protoImport = regexp.MustCompile(`(?m)^\s*import\s+(?:public\s+|weak\s+)?"([^"]+)"\s*;`)

// ContractDigest is the slot contract digest (ST-EXT-DS01): the SHA-256
// digest of the slot's .proto file at contract, a path in the registry, and of
// each .proto file that it imports, directly or through another import, in
// the order of their names. No other file of the registry counts.
func ContractDigest(files ContractFiles, contract string) (string, error) {
	data, err := files.ReadFile(contract)
	if err != nil {
		return "", fmt.Errorf("slot contract %s: %w", contract, err)
	}
	contents := map[string]string{}
	add := func(name string, data []byte) {
		sum := sha256.Sum256(data)
		contents[name] = hex.EncodeToString(sum[:])
	}
	add(contract, data)
	seen := map[string]bool{}
	pending := importsOf(data)
	for len(pending) > 0 {
		name := pending[0]
		pending = pending[1:]
		if seen[name] {
			continue
		}
		seen[name] = true
		imported, err := files.ReadProtoImport(name)
		if err != nil {
			return "", fmt.Errorf("slot contract %s imports %s: %w", contract, name, err)
		}
		add("import:"+pathpkg.Clean(name), imported)
		pending = append(pending, importsOf(imported)...)
	}
	return combine(contents), nil
}

// importsOf gives the import paths of a .proto file, with its comments left
// out.
func importsOf(data []byte) []string {
	var imports []string
	for _, match := range protoImport.FindAllSubmatch(stripProtoComments(data), -1) {
		imports = append(imports, string(match[1]))
	}
	return imports
}

// stripProtoComments removes the line and block comments of a .proto file,
// outside its string literals.
func stripProtoComments(data []byte) []byte {
	out := make([]byte, 0, len(data))
	for i := 0; i < len(data); i++ {
		switch {
		case data[i] == '"' || data[i] == '\'':
			quote := data[i]
			out = append(out, data[i])
			for i++; i < len(data); i++ {
				out = append(out, data[i])
				if data[i] == '\\' && i+1 < len(data) {
					i++
					out = append(out, data[i])
					continue
				}
				if data[i] == quote {
					break
				}
			}
		case data[i] == '/' && i+1 < len(data) && data[i+1] == '/':
			for i < len(data) && data[i] != '\n' {
				i++
			}
			if i < len(data) {
				out = append(out, '\n')
			}
		case data[i] == '/' && i+1 < len(data) && data[i+1] == '*':
			for i += 2; i+1 < len(data) && !(data[i] == '*' && data[i+1] == '/'); i++ {
				if data[i] == '\n' {
					out = append(out, '\n')
				}
			}
			i++
		default:
			out = append(out, data[i])
		}
	}
	return out
}
