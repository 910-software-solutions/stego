package fill

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	goparser "go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/mod/modfile"
)

// localModulePrefix starts the name of a file of a local fill module in the
// files of a fill digest: "module:<module path>/<path in the module folder>".
// A file of the fill folder has no such name: its names are paths in the
// folder, and a folder name has no colon.
const localModulePrefix = "module:"

// localModule is a fill module that the go.mod of the project replaces with a
// folder of the project (replace <module> => ./dir).
type localModule struct {
	path, dir string
}

// fillFiles gives the digest of each file that a fill digest counts: each file
// of the fill folder dir (FileDigests), and each file of each local fill
// module that the fill reaches (localModuleFiles). hasTests tells whether the
// fill folder holds a test file; a test file of a module does not count.
//
// A local fill module is code of the project that the fill runs, as its own
// files are, so a change to it must make the evidence of the fill stale and
// its sign-off void (spike-localfill, 2026-11-27).
func fillFiles(projectDir, dir string, fillModules []string) (map[string]string, bool, error) {
	files, err := FileDigests(dir)
	if err != nil {
		return nil, false, err
	}
	hasTests := false
	for name := range files {
		if strings.HasSuffix(name, "_test.go") {
			hasTests = true
		}
	}
	modules, err := localModuleFiles(projectDir, dir, fillModules)
	if err != nil {
		return nil, false, err
	}
	for name, digest := range modules {
		files[name] = digest
	}
	return files, hasTests, nil
}

// localModuleFiles gives the digest of each file of each local fill module that
// the Go files of the fill folder dir reach, by its name localModulePrefix +
// module path + "/" + its slash-separated path in the module folder. A local
// fill module is a module of fillModules that the go.mod of the project in
// projectDir replaces with a folder. The fill reaches a module when a Go file
// of the fill imports a package of it, or a Go file of a module that it reaches
// does. The module counts whole: each file of its folder, outside a sub-folder
// that holds a go.mod of its own. It refuses a module folder that holds a
// symbolic link or another file that is not regular, as FileDigests refuses it
// in the fill folder.
func localModuleFiles(projectDir, dir string, fillModules []string) (map[string]string, error) {
	modules, err := localModules(projectDir, fillModules)
	if err != nil || len(modules) == 0 {
		return map[string]string{}, err
	}
	imports, err := goImports(dir, true)
	if err != nil {
		return nil, err
	}
	reached := map[string]bool{}
	var queue []localModule
	reach := func(paths []string) {
		for _, path := range paths {
			for _, module := range modules {
				if (path == module.path || strings.HasPrefix(path, module.path+"/")) && !reached[module.path] {
					reached[module.path] = true
					queue = append(queue, module)
				}
			}
		}
	}
	reach(imports)
	files := map[string]string{}
	for len(queue) > 0 {
		module := queue[0]
		queue = queue[1:]
		found, err := moduleFolderFiles(module)
		if err != nil {
			return nil, err
		}
		for name, digest := range found {
			files[name] = digest
		}
		next, err := goImports(module.dir, true)
		if err != nil {
			return nil, err
		}
		reach(next)
	}
	return files, nil
}

// localModules gives each module of fillModules that the go.mod of the project
// replaces with a folder, with its folder. A project with no go.mod has none.
func localModules(projectDir string, fillModules []string) ([]localModule, error) {
	if len(fillModules) == 0 {
		return nil, nil
	}
	name := filepath.Join(projectDir, "go.mod")
	data, err := os.ReadFile(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// Parse, not ParseLax: ParseLax leaves out the replace directives.
	file, err := modfile.Parse(name, data, nil)
	if err != nil {
		return nil, err
	}
	listed := map[string]bool{}
	for _, module := range fillModules {
		listed[module] = true
	}
	var modules []localModule
	seen := map[string]bool{}
	for _, replace := range file.Replace {
		if !listed[replace.Old.Path] || replace.New.Version != "" || seen[replace.Old.Path] {
			continue
		}
		seen[replace.Old.Path] = true
		folder := filepath.FromSlash(replace.New.Path)
		if !filepath.IsAbs(folder) {
			folder = filepath.Join(projectDir, folder)
		}
		modules = append(modules, localModule{path: replace.Old.Path, dir: folder})
	}
	sort.Slice(modules, func(i, j int) bool { return modules[i].path < modules[j].path })
	return modules, nil
}

// moduleFolderFiles gives the digest of each file of the folder of the local
// module, by its name in the files of a fill digest.
func moduleFolderFiles(module localModule) (map[string]string, error) {
	files := map[string]string{}
	err := filepath.WalkDir(module.dir, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(module.dir, name)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			if relative != "." && nestedModule(name) {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("file %s of the fill module %s is not a regular file", relative, module.path)
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		files[localModulePrefix+module.path+"/"+relative] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("the fill module %s: %w", module.path, err)
	}
	return files, nil
}

// nestedModule tells whether the folder holds a go.mod: a module of its own.
func nestedModule(dir string) bool {
	info, err := os.Lstat(filepath.Join(dir, "go.mod"))
	return err == nil && info.Mode().IsRegular()
}

// goImports gives the import paths of the Go files of dir, and of its
// sub-folders when deep is true, outside a sub-folder that holds a go.mod of
// its own. A file that does not parse gives no imports: the closure check of
// the fill names it.
func goImports(dir string, deep bool) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(dir, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if name != dir && (!deep || nestedModule(name)) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || !entry.Type().IsRegular() {
			return nil
		}
		file, err := goparser.ParseFile(token.NewFileSet(), name, nil, goparser.ImportsOnly)
		if err != nil {
			return nil
		}
		for _, spec := range file.Imports {
			if path, err := strconv.Unquote(spec.Path.Value); err == nil {
				paths = append(paths, path)
			}
		}
		return nil
	})
	return paths, err
}
