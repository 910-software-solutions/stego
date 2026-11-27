package main

import (
	"fmt"
	"go/format"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/910-software-solutions/stego/internal/compiler"
	"github.com/910-software-solutions/stego/internal/fill"
	"github.com/910-software-solutions/stego/internal/gen"
	"github.com/910-software-solutions/stego/internal/generator/capability"
	"github.com/910-software-solutions/stego/internal/parser"
	"golang.org/x/mod/module"
	"gopkg.in/yaml.v3"
)

// fillPackageName gives the Go package name of the fill name: the name with no hyphen, as Go names a package in
// lower case with no underscore, so the fill place-order is the package placeorder (walkthrough 7, B12). An underscore
// of the fill name stays.
func fillPackageName(name string) (string, error) {
	if err := gen.ValidatePath(name); err != nil || strings.Contains(name, "/") {
		return "", fmt.Errorf("fill name must be one safe path segment: %q", name)
	}
	packageName := strings.ReplaceAll(name, "-", "")
	if !token.IsIdentifier(packageName) || packageName == "_" || packageName == "main" {
		return "", fmt.Errorf("fill name %q does not form a Go package name", name)
	}
	return packageName, nil
}

// scaffoldFile is one file of a scaffolded fill, by its name in the fill folder.
type scaffoldFile struct {
	name string
	data []byte
}

// writeFillScaffold creates each path exclusively. Existing application files
// cannot be overwritten. All source validation must finish before this call.
func writeFillScaffold(projectDir, name string, declaration, source []byte) error {
	return writeFillScaffoldFiles(projectDir, name, []scaffoldFile{{"fill.yaml", declaration}, {"fill.go", source}})
}

// writeFillScaffoldFiles creates the fill folder name and each of files in it exclusively.
func writeFillScaffoldFiles(projectDir, name string, files []scaffoldFile) error {
	if _, err := fillPackageName(name); err != nil {
		return err
	}
	root, err := os.OpenRoot(projectDir)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.Mkdir("fills", 0755); err != nil && !os.IsExist(err) {
		return err
	}
	info, err := root.Lstat("fills")
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("fills must be a directory without symbolic links")
	}
	fills, err := root.OpenRoot("fills")
	if err != nil {
		return err
	}
	defer fills.Close()
	if err := fills.Mkdir(name, 0755); err != nil {
		return fmt.Errorf("creating fill directory (it must not already exist): %w", err)
	}
	for _, file := range files {
		f, err := fills.OpenFile(filepath.Join(name, file.name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err != nil {
			return err
		}
		_, writeErr := f.Write(file.data)
		if writeErr == nil {
			writeErr = f.Sync()
		}
		closeErr := f.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

// capabilityName is the form of a command, query or worker name that the scaffold puts into Go names and comments.
var capabilityName = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// capabilitySlots are the slots of the fills of commands, queries and workers, with the declaration part whose fill
// implements each (ST-SUP-RC32-OC01, ST-SUP-RC33-OC01, ST-SUP-RC35-OC01).
var capabilitySlots = map[string]string{"command.command": "command", "query.query": "query", "worker.worker": "worker"}

// createCapabilityFill writes the fill of the command, query or worker of the declaration that names the fill name,
// for the slot slot: fill.yaml, code that compiles against the Go contract that generation writes for it, and a test
// file (ST-EXT-CMD01, ST-EXT-UC01-R01). The contract need not be generated yet, so a command, query or worker that a
// later change declares gets its fill before the apply that writes its contract. It refuses a fill that no command,
// query or worker of the declaration names, and a fill that exists.
func createCapabilityFill(projectDir, name, packageName, slot string) error {
	input, err := buildReconcilerInput()
	if err != nil {
		return err
	}
	check, err := compiler.ResolveFillCheck(input)
	if err != nil {
		return err
	}
	kind := capabilitySlots[slot]
	var owner, contract string
	switch kind {
	case "command":
		for _, f := range check.CommandFills {
			if f.Fill == name {
				owner, contract = f.Command, f.Contract
			}
		}
	case "query":
		for _, f := range check.QueryFills {
			if f.Fill == name {
				owner, contract = f.Query, check.QueryContract
			}
		}
	case "worker":
		for _, f := range check.WorkerFills {
			if f.Fill == name {
				owner, contract = f.Query, check.WorkerContract
			}
		}
	}
	if owner == "" {
		return fmt.Errorf("no %s of the declaration names the fill %q; declare the %s with fill: %s in service.yaml first", kind, name, kind, name)
	}
	if !capabilityName.MatchString(owner) {
		return fmt.Errorf("the %s name %q is not lower case letters, digits, - and _ that start with a letter, so it gives no fill code; correct the name in service.yaml", kind, owner)
	}
	if contract == "" {
		return fmt.Errorf("the %s %s has no contract package: the component %s is not active", kind, owner, kind)
	}
	every := false
	if kind == "worker" {
		service, err := parser.ParseServiceDeclaration(filepath.Join(projectDir, "service.yaml"))
		if err != nil {
			return err
		}
		for _, worker := range service.Workers {
			if worker.Name == owner {
				every = strings.HasPrefix(worker.Trigger, "every ")
			}
		}
	}
	source, test, err := capabilityFillSource(kind, owner, packageName, contract, every)
	if err != nil {
		return err
	}
	manifest, err := yaml.Marshal(fill.Fill{Kind: "fill", Name: name, Implements: slot})
	if err != nil {
		return fmt.Errorf("encoding fill.yaml: %w", err)
	}
	if err := writeFillScaffoldFiles(projectDir, name, []scaffoldFile{{"fill.yaml", manifest}, {"fill.go", source}, {"fill_test.go", test}}); err != nil {
		return err
	}
	fmt.Printf("Created fill %q of the %s %s for slot %q\n", name, kind, owner, slot)
	fmt.Printf("  fills/%s/fill.yaml\n  fills/%s/fill.go\n  fills/%s/fill_test.go\n", name, name, name)
	fmt.Println("Run stego apply to write the contract of the fill, and stego deps to resolve its modules; then write the fill.")
	fmt.Println("Until it is written, the fill returns ErrNotImplemented.")
	return nil
}

// capabilityFillSource gives the code and the test of the unfinished fill of the command, query or worker owner, in
// the package packageName, against the contract package at contract: a command or worker fill exports New, which
// gives the fill, and a query fill exports the function Fill, as the generated code that binds them calls
// (ST-SUP-RC32-OC01, ST-SUP-RC33-OC01, ST-SUP-RC35-OC01). Each returns ErrNotImplemented until it is written.
func capabilityFillSource(kind, owner, packageName, contract string, every bool) ([]byte, []byte, error) {
	if err := module.CheckImportPath(contract); err != nil {
		return nil, nil, fmt.Errorf("invalid contract package path: %w", err)
	}
	alias := path.Base(contract)
	if !token.IsIdentifier(alias) {
		return nil, nil, fmt.Errorf("the contract package %s has no Go name", contract)
	}
	goName := capability.MixedCase(owner)
	// The test calls the unfinished fill, which must refuse; the author changes it when the fill is written.
	var imports, body, call, testImports string
	switch {
	case kind == "command":
		imports = "\"context\"\n\"errors\"\n"
		body = fmt.Sprintf("// New gives the fill of the command %[2]s.\nfunc New() %[1]s.%[3]sFill {\nreturn func(ctx context.Context, req *%[1]s.%[3]sRequest, h *%[1]s.%[3]sHandle) (*%[1]s.%[3]sResponse, error) {\nreturn nil, ErrNotImplemented\n}\n}\n", alias, owner, goName)
		call = "_, err := New()(context.Background(), &" + alias + "." + goName + "Request{}, nil)"
	case kind == "query":
		imports = "\"context\"\n\"errors\"\n"
		body = fmt.Sprintf("var _ %[1]s.%[3]sFill = Fill\n\n// Fill is the fill of the query %[2]s.\nfunc Fill(ctx context.Context, req %[1]s.%[3]sRequest, h *%[1]s.%[3]sHandle) (%[1]s.%[3]sResponse, error) {\nvar response %[1]s.%[3]sResponse\nreturn response, ErrNotImplemented\n}\n", alias, owner, goName)
		call = "_, err := Fill(context.Background(), " + alias + "." + goName + "Request{}, nil)"
	case every:
		imports = "\"context\"\n\"errors\"\n\"time\"\n"
		body = fmt.Sprintf("// New gives the fill of the worker %[2]s, which runs once for each tenant at each tick.\nfunc New() %[1]s.%[3]sFill {\nreturn func(ctx context.Context, tick time.Time, h *%[1]s.%[3]sHandle) error {\nreturn ErrNotImplemented\n}\n}\n", alias, owner, goName)
		call = "err := New()(context.Background(), time.Time{}, nil)"
		testImports = "\"time\"\n"
	default:
		imports = "\"context\"\n\"encoding/json\"\n\"errors\"\n"
		body = fmt.Sprintf("// New gives the fill of the worker %[2]s, which runs once for each message it consumes.\nfunc New() %[1]s.%[3]sFill {\nreturn func(ctx context.Context, message json.RawMessage, effect string, h *%[1]s.%[3]sHandle) error {\nreturn ErrNotImplemented\n}\n}\n", alias, owner, goName)
		call = "err := New()(context.Background(), json.RawMessage(\"{}\"), \"\", nil)"
		testImports = "\"encoding/json\"\n"
	}
	source := fmt.Sprintf("// Package %[1]s is the fill of the %[2]s %[3]s.\npackage %[1]s\n\nimport (\n%[4]s\n%[5]s %[6]q\n)\n\n// ErrNotImplemented prevents use of an unfinished fill.\nvar ErrNotImplemented = errors.New(%[7]q)\n\n%[8]s",
		packageName, kind, owner, imports, alias, contract, owner+": the fill is not implemented", body)
	formatted, err := format.Source([]byte(source))
	if err != nil {
		return nil, nil, fmt.Errorf("formatting the fill: %w", err)
	}
	if kind != "worker" {
		// The test of a command or query fill names the request type of the contract.
		testImports += fmt.Sprintf("\n%s %q\n", alias, contract)
	}
	// The test compares the text of the refusal, not the variable ErrNotImplemented, so it still compiles when the
	// author writes the fill and removes the variable; it then fails with a message that says to replace it
	// (walkthrough 11, W11-B7).
	testSource, err := format.Source([]byte(fmt.Sprintf("package %[1]s\n\nimport (\n\"context\"\n\"testing\"\n%[2]s)\n\n"+
		"// unfinished is the refusal of the fill that stego fill create wrote.\nconst unfinished = %[4]q\n\n"+
		"// TestFill passes while the fill is the one that stego fill create wrote, which refuses each call. When you write\n"+
		"// the fill, this test fails: replace it with the tests of what the fill does.\nfunc TestFill(t *testing.T) {\n%[3]s\n"+
		"if err == nil || err.Error() != unfinished {\nt.Fatalf(\"the fill is written, and this test is the one that stego fill create wrote for the unfinished fill: replace it in fill_test.go with the tests of what the fill does (the fill gave %%v)\", err)\n}\n}\n",
		packageName, testImports, call, owner+": the fill is not implemented")))
	if err != nil {
		return nil, nil, fmt.Errorf("formatting the fill test: %w", err)
	}
	return formatted, testSource, nil
}
