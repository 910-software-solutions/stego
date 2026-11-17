package compiler

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"

	"github.com/jsell-rh/stego/internal/declaration"
	"github.com/jsell-rh/stego/internal/parser"
	"github.com/jsell-rh/stego/internal/registry"
	"github.com/jsell-rh/stego/internal/types"
	"golang.org/x/mod/modfile"
	"gopkg.in/yaml.v3"
)

type InitOptions struct {
	ProjectDir string
	Archetype  string
	// Empty asks for a scaffold with no example content (ST-DECL-UC01-R05).
	Empty bool
	// Module is the module path of the service: the --module flag of stego init, or STEGO_MODULE. When the project
	// holds no go.mod, init writes a go.mod that declares it, and each later command reads the module from that
	// go.mod only. With no value, init takes the module of the project's go.mod or the default. A value that
	// differs from the module of the project's go.mod is refused (ProjectModuleSettings).
	Module string
	// GoVersion is the Go version of the go.mod that init writes: STEGO_GO_VERSION, or with no value that of the
	// project's go.mod or the default.
	GoVersion string
	Stderr    io.Writer
}

type InitResult struct {
	Name    string
	Created []string
	// Module is the module path of the service, and ModuleFromProject tells that it is the one of the go.mod that the
	// project held, so init wrote no go.mod.
	Module            string
	ModuleFromProject bool
	// Owed names each setting that the archetype's components mark as author supplied, for an empty scaffold
	// (ST-DECL-UC01-R05). A scaffold with example content gives them, so it owes none.
	Owed []string
}

// Initialize prepares a project for a new declaration (ST-GEN-CMD07). It uses the configured registry, or with no
// configuration the registry of STEGO_REGISTRY or the default local registry folder, writes the
// declaration that declaration.Scaffold makes from the archetype's facts, and publishes service.yaml last.
// Existing files are never replaced. A failed or interrupted attempt can leave
// complete configuration files or empty directories, but no truncated output.
func Initialize(opts InitOptions) (result *InitResult, err error) {
	if opts.Archetype == "" {
		return nil, fmt.Errorf("archetype is required")
	}
	project, err := filepath.Abs(opts.ProjectDir)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(project)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	original, existed, err := inspectInitProject(root)
	if err != nil {
		return nil, err
	}
	config := original
	if !existed {
		directory := os.Getenv("STEGO_REGISTRY")
		if directory == "" {
			directory, err = DefaultRegistryFolder()
			if err != nil {
				return nil, err
			}
		}
		config, err = yaml.Marshal(registry.RegistryConfig{Registry: []registry.RegistrySource{{URL: directory, Ref: "local"}}})
		if err != nil {
			return nil, err
		}
	}
	resolved, err := registry.ResolveRegistry(registry.ResolveOptions{ProjectDir: project, ConfigData: config, Stderr: opts.Stderr})
	if err != nil {
		return nil, err
	}
	reg, err := registry.LoadDirectories(resolved.Dirs...)
	if err != nil {
		return nil, err
	}
	arch := reg.Archetype(opts.Archetype)
	if arch == nil {
		available := make([]string, 0, len(reg.Archetypes()))
		for name := range reg.Archetypes() {
			available = append(available, name)
		}
		sort.Strings(available)
		return nil, fmt.Errorf("archetype %q not found in registry (available: %s)", opts.Archetype, strings.Join(available, ", "))
	}
	name := filepath.Base(project)
	// The example content of the archetype (ST-SUP-EN03 field Example content), unless the author asks for an empty
	// scaffold. Its placeholders take the service name and the module path that generation then uses.
	var example []registry.ExampleFile
	if !opts.Empty {
		example = arch.Example
	}
	var exampleDeclaration []byte
	var exampleFiles []registry.ExampleFile
	for _, file := range example {
		if file.Path == registry.ExampleDeclaration {
			exampleDeclaration = file.Text
		} else {
			exampleFiles = append(exampleFiles, file)
		}
	}
	// The module that the placeholders of the example content take is the one that generation then uses: that
	// of the project's go.mod, or the one that init writes in a new go.mod (walkthrough-7 B12, decision A).
	module, goVersion, err := ProjectModuleSettings(project, opts.Module, opts.GoVersion)
	if err != nil {
		return nil, err
	}
	var goModule []byte
	if _, err := root.Lstat("go.mod"); errors.Is(err, os.ErrNotExist) {
		if goModule, err = minimalModule(module, goVersion); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	document, err := declaration.Scaffold(name, opts.Archetype, arch.Language, exampleDeclaration, module, registry.ExampleService, registry.ExampleModule)
	if err != nil {
		return nil, err
	}
	var owed []string
	if len(example) == 0 {
		// The components the archetype names, and its default auth component, as the registry holds them; checking
		// resolves the ports of the set later.
		schemas := map[string]map[string]types.ConfigField{}
		for _, component := range append(append([]string(nil), arch.Components...), arch.DefaultAuth) {
			if item := reg.Component(component); item != nil {
				schemas[component] = item.Config
			}
		}
		owed = declaration.AuthorSuppliedSettings(schemas)
	}
	// No path of the example content may hold a file, a folder or a link, so a refused scaffold changes no file
	// (ST-DECL-UC01-R02, ST-DECL-UC01-R06).
	for _, file := range exampleFiles {
		if err := checkExampleTarget(root, file.Path); err != nil {
			return nil, err
		}
	}
	if err := reg.Verify(); err != nil {
		return nil, err
	}
	lock, err := lockProject(root)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	current, exists, err := inspectInitProject(root)
	if err != nil {
		return nil, err
	}
	if existed != exists || !bytes.Equal(original, current) {
		return nil, fmt.Errorf("registry configuration changed during initialization")
	}
	if err := reg.Verify(); err != nil {
		return nil, err
	}
	// A write that fails part way removes each file and folder that this run made, so a rerun does not refuse the
	// example content of this run as files that the project already holds (ST-DECL-UC01-R02). A file or folder
	// that was there before the run is never removed.
	var made []string
	for _, file := range append([]string{"fills/.", ".stego/config.yaml", localStateIgnorePath, "go.mod", "service.yaml"}, examplePaths(exampleFiles)...) {
		made = appendAbsentFolders(root, made, path.Dir(file))
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, removeMade(root, made))
		}
	}()
	// create writes a new file and names it in made once this run has put it there, also when a later step of the
	// write fails, so a write that fails after the file is there removes it too. A path that the project held
	// before, or that another process made first, is never named (sec-419 LOW-1).
	create := func(name string, data []byte) error {
		published, err := publishRootFile(root, name, data, 0644, false)
		if published && !slices.Contains(made, name) {
			made = append(made, name)
		}
		return err
	}
	created := []string{}
	if err := root.Mkdir("fills", 0755); err == nil {
		created = append(created, "fills/")
	} else if !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	if err := checkInitDirectory(root, "fills"); err != nil {
		return nil, err
	}
	if !existed {
		if err := create(".stego/config.yaml", config); err != nil {
			return nil, err
		}
		created = append(created, ".stego/config.yaml")
	}
	// The apply lock and a saved plan are local state: they are not committed, so no build takes them as input
	// (ST-DECL-UC01-R07). An ignore file that the author already has is kept as written.
	if err := create(localStateIgnorePath, []byte(localStateIgnore)); err == nil {
		created = append(created, localStateIgnorePath)
	} else if !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	for _, file := range exampleFiles {
		if err := checkExampleTarget(root, file.Path); err != nil {
			return nil, err
		}
		text := strings.NewReplacer(registry.ExampleService, name, registry.ExampleModule, module).Replace(string(file.Text))
		if err := create(file.Path, []byte(text)); err != nil {
			return nil, err
		}
		created = append(created, file.Path)
	}
	// A go.mod that the project holds is kept as written; one that another process makes first fails the write.
	if goModule != nil {
		if err := create("go.mod", goModule); err != nil {
			return nil, err
		}
		created = append(created, "go.mod")
	}
	if err := create("service.yaml", document); err != nil {
		return nil, err
	}
	return &InitResult{Name: name, Created: append(created, "service.yaml"), Owed: owed, Module: module, ModuleFromProject: goModule == nil}, nil
}

// minimalModule gives the go.mod that init writes in a project that holds none: the module and the Go version only.
// Apply adds the requirements of the generated code to it (mergeProjectModule).
func minimalModule(module, goVersion string) ([]byte, error) {
	file := &modfile.File{}
	if err := file.AddModuleStmt(module); err != nil {
		return nil, err
	}
	if err := file.AddGoStmt(goVersion); err != nil {
		return nil, err
	}
	return file.Format()
}

// examplePaths gives the path of each file of the example content.
func examplePaths(files []registry.ExampleFile) []string {
	paths := make([]string, len(files))
	for i, file := range files {
		paths[i] = file.Path
	}
	return paths
}

// appendAbsentFolders appends to made each folder of the path, from the top down, that the project does not hold
// and that made does not name yet.
func appendAbsentFolders(root *os.Root, made []string, folder string) []string {
	if folder == "." || folder == "/" || folder == "" {
		return made
	}
	made = appendAbsentFolders(root, made, path.Dir(folder))
	if slices.Contains(made, folder) {
		return made
	}
	if _, err := root.Lstat(folder); errors.Is(err, os.ErrNotExist) {
		made = append(made, folder)
	}
	return made
}

// removeMade removes each path of made that is there, the last first, so a folder goes after the files in it. A
// folder that holds another file is kept.
func removeMade(root *os.Root, made []string) error {
	var faults []error
	for i := len(made) - 1; i >= 0; i-- {
		if err := root.Remove(made[i]); err != nil && !errors.Is(err, os.ErrNotExist) && !isNotEmpty(err) {
			faults = append(faults, fmt.Errorf("removing %s after the failed scaffold: %w", made[i], err))
		}
	}
	return errors.Join(faults...)
}

// isNotEmpty tells whether the error is that of the removal of a folder that holds a file.
func isNotEmpty(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST)
}

// checkExampleTarget refuses a path of the example content at which the project already holds anything, or whose
// folders hold a symbolic link, and names the path.
func checkExampleTarget(root *os.Root, name string) error {
	if err := checkFilePath(root, name); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if _, err := root.Lstat(name); err == nil {
		return fmt.Errorf("%s already exists; stego init writes the example content of the archetype there", name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// localStateIgnorePath is the Git ignore file that init writes for the local state of the service folder.
const localStateIgnorePath = ".stego/.gitignore"

// localStateIgnore names the local state files of .stego: the apply lock and the plan that stego plan saves. The
// record (state.yaml) and the registry pin (config.yaml) are committed.
const localStateIgnore = "# Local state of stego: the apply lock and the saved plan. Do not commit them.\n/apply.lock\n/plan.yaml\n"

// DefaultRegistryFolder is the default local registry folder: stego/registry in the user's configuration folder.
// A project with no configuration names it as a local source when STEGO_REGISTRY is not set.
func DefaultRegistryFolder() (string, error) {
	folder, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("finding the default local registry folder: %w", err)
	}
	return filepath.Join(folder, "stego", "registry"), nil
}

func checkInitDirectory(root *os.Root, name string) error {
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s must be a directory without symbolic links", name)
	}
	return nil
}

func inspectInitProject(root *os.Root) ([]byte, bool, error) {
	if _, err := root.Lstat("service.yaml"); err == nil {
		return nil, false, fmt.Errorf("service.yaml already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	for _, name := range []string{".stego", "fills"} {
		if err := checkInitDirectory(root, name); err != nil {
			return nil, false, err
		}
	}
	// The ignore file of the local state is checked here too, so that a symbolic link there is refused before any
	// write (ST-DECL-UC01-R06).
	if err := checkFileTarget(root, localStateIgnorePath); err != nil {
		return nil, false, err
	}
	const name = ".stego/config.yaml"
	if err := checkFileTarget(root, name); err != nil {
		return nil, false, err
	}
	file, err := root.Open(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("registry config must be a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, parser.MaxDocumentBytes+1))
	if err != nil {
		return nil, false, err
	}
	if len(data) > parser.MaxDocumentBytes {
		return nil, false, fmt.Errorf("registry config exceeds its byte limit")
	}
	return data, true, nil
}
