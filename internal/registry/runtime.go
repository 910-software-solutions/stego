package registry

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
	"golang.org/x/mod/sumdb/dirhash"
	modzip "golang.org/x/mod/zip"
)

// RuntimeModule is the module path of the runtime library, the Go module in the folder runtime of the common
// registry (ST-SUP-TOOL11).
const RuntimeModule = "github.com/jsell-rh/stego/registry/runtime"

// RuntimePin is one version of the runtime library (ST-VO58): its module path, a tagged version and the go.sum
// hash of that version's contents. An archetype pins exactly one (ST-SUP-INV10).
type RuntimePin struct {
	Module  string `yaml:"module"`
	Version string `yaml:"version"`
	Sum     string `yaml:"sum"`
}

var moduleSum = regexp.MustCompile(`^h1:[A-Za-z0-9+/]{43}=$`)

// Validate enforces ST-VO58-C01: the pin names a module path, a tagged version and the module checksum of that
// version, never a branch, a pseudo-version or a replace directive. A replace directive has no field in a pin, so
// the strict decoder refuses it by its key. Each error names the field.
func (p RuntimePin) Validate() error {
	if p.Module == "" {
		return errors.New("runtime.module is missing: a runtime pin names the module path of the runtime library")
	}
	if err := module.CheckPath(p.Module); err != nil {
		return fmt.Errorf("runtime.module %q is not a module path: %w", p.Module, err)
	}
	if p.Version == "" {
		return errors.New("runtime.version is missing: a runtime pin names a tagged version")
	}
	if err := CheckTaggedVersion(p.Module, p.Version); err != nil {
		return err
	}
	if p.Sum == "" {
		return errors.New("runtime.sum is missing: a runtime pin names the go.sum hash of its version")
	}
	if !moduleSum.MatchString(p.Sum) {
		return fmt.Errorf("runtime.sum %q is not a go.sum hash h1:<44 base64 characters>", p.Sum)
	}
	return nil
}

// CheckTaggedVersion refuses a version of a module that is not a tagged version: a branch, a version that is not
// canonical, a pseudo-version, or one that does not fit the module path's major version. The error names the
// field runtime.version.
func CheckTaggedVersion(modulePath, version string) error {
	if !semver.IsValid(version) || semver.Canonical(version) != version || semver.Build(version) != "" {
		return fmt.Errorf("runtime.version %q is not a tagged version such as v1.4.2; a branch is not a version", version)
	}
	if module.IsPseudoVersion(version) {
		return fmt.Errorf("runtime.version %q is a pseudo-version; a runtime pin names a tagged version", version)
	}
	if err := module.Check(modulePath, version); err != nil {
		return fmt.Errorf("runtime.version %q does not fit runtime.module %q: %w", version, modulePath, err)
	}
	return nil
}

// checkRuntimePin enforces ST-SUP-INV10 at load: an archetype pins exactly one runtime library, by module path,
// version and module checksum. The YAML decoder refuses a second runtime key, and a runtime value that is a list.
func checkRuntimePin(a *Archetype) error {
	if a.Runtime == nil {
		return fmt.Errorf("archetype %s %s pins no runtime library: add runtime with its module, version and sum", a.Name, a.Version)
	}
	if err := a.Runtime.Validate(); err != nil {
		return fmt.Errorf("archetype %s %s: %w", a.Name, a.Version, err)
	}
	return nil
}

// IsStegoModule says whether a module path is under one of STEGO's own module path, the
// compiler's repository github.com/jsell-rh/stego, which holds the runtime library too. A generated service may require only the runtime pin of these
// (ST-SUP-INV10, ST-GEN-INV13).
func IsStegoModule(path string) bool {
	const root = "github.com/jsell-rh/stego"
	return path == root || strings.HasPrefix(path, root+"/")
}

// InModule says whether an import path is a package of the module at modulePath.
func InModule(importPath, modulePath string) bool {
	return importPath == modulePath || strings.HasPrefix(importPath, modulePath+"/")
}

// RuntimeFolder gives the folder of the runtime library in the common registry, the first source, or "" when that
// source has no runtime folder with a go.mod. The runtime library is released with the common registry
// (ST-STD-RUNTIME-R04).
func (r *Registry) RuntimeFolder() string {
	if len(r.captured) == 0 {
		return ""
	}
	folder := filepath.Join(r.captured[0].dir, "runtime")
	if info, err := os.Stat(filepath.Join(folder, "go.mod")); err != nil || !info.Mode().IsRegular() {
		return ""
	}
	return folder
}

// RuntimeArchive is the module zip of a runtime folder at one version, with the go.sum hashes of the zip and of
// its go.mod, and the go.mod that the zip holds.
type RuntimeArchive struct {
	Sum    string
	ModSum string
	// Mod is the go.mod of the zip. A proxy serves these bytes as the .mod file of the version, so the go.mod that
	// the go command reads is the one that the zip, and thus the pin's sum, holds; a second read of the folder could
	// give other bytes (security re-review of PR #400, L1).
	Mod []byte
}

// WriteRuntimeArchive writes the module zip of the folder as module@version to zipPath, as the Go module proxy
// serves it for the tag registry/runtime/<version>, and gives its go.sum hashes and its go.mod. The folder holds its
// own LICENSE, so the zip is the same as the one the proxy makes from the tag. It refuses a zip that holds no go.mod,
// such as one of a folder whose go.mod is a symbolic link, which the zip leaves out. It makes the zip in memory, and
// hashes it and reads its go.mod from those bytes before it writes them, so no other process can change the zip
// between the hash and the read (security review of PR #436, L1).
func WriteRuntimeArchive(folder, modulePath, version, zipPath string) (RuntimeArchive, error) {
	var zipped bytes.Buffer
	if err := modzip.CreateFromDir(&zipped, module.Version{Path: modulePath, Version: version}, folder); err != nil {
		return RuntimeArchive{}, fmt.Errorf("module zip of %s: %w", folder, err)
	}
	archive, err := runtimeArchiveOf(zipped.Bytes(), modulePath+"@"+version+"/go.mod")
	if err != nil {
		return RuntimeArchive{}, fmt.Errorf("module zip of %s: %w", folder, err)
	}
	file, err := os.OpenFile(zipPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return RuntimeArchive{}, err
	}
	_, err = file.Write(zipped.Bytes())
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return RuntimeArchive{}, err
	}
	return archive, nil
}

// runtimeArchiveOf gives the go.sum hashes and the go.mod named goModName of the module zip data, all from the same
// bytes. The hash of the zip is the one dirhash.HashZip gives of a file with these bytes.
func runtimeArchiveOf(data []byte, goModName string) (RuntimeArchive, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return RuntimeArchive{}, err
	}
	var names []string
	entries := map[string]*zip.File{}
	for _, entry := range reader.File {
		names = append(names, entry.Name)
		entries[entry.Name] = entry
	}
	sum, err := dirhash.Hash1(names, func(name string) (io.ReadCloser, error) { return entries[name].Open() })
	if err != nil {
		return RuntimeArchive{}, err
	}
	mod, err := zipGoMod(reader, goModName)
	if err != nil {
		return RuntimeArchive{}, err
	}
	modSum, err := dirhash.Hash1([]string{"go.mod"}, func(string) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(mod)), nil
	})
	if err != nil {
		return RuntimeArchive{}, err
	}
	return RuntimeArchive{Sum: sum, ModSum: modSum, Mod: mod}, nil
}

// maxGoModBytes bounds the go.mod that zipGoMod reads, as the go command bounds a go.mod file.
const maxGoModBytes = 16 << 20

// zipGoMod gives the file name of the module zip, at most maxGoModBytes long. It refuses a zip with two entries of
// that name, which a module zip never holds, so it never picks one of two (security review of PR #436, I1).
func zipGoMod(archive *zip.Reader, name string) ([]byte, error) {
	var found *zip.File
	for _, entry := range archive.File {
		if entry.Name != name {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("it holds more than one go.mod file")
		}
		found = entry
	}
	if found == nil {
		return nil, fmt.Errorf("it holds no go.mod file; a go.mod that is a symbolic link is left out of a module zip")
	}
	reader, err := found.Open()
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(reader, maxGoModBytes+1))
	if closeErr := reader.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	if len(data) > maxGoModBytes {
		return nil, fmt.Errorf("its go.mod is larger than %d bytes", maxGoModBytes)
	}
	return data, nil
}

// RuntimeSum gives the go.sum hash of the folder as module@version: the sum an archetype's runtime pin must hold
// for the folder to serve that version.
func RuntimeSum(folder, modulePath, version, scratch string) (string, error) {
	archive, err := WriteRuntimeArchive(folder, modulePath, version, filepath.Join(scratch, "runtime.zip"))
	if err != nil {
		return "", err
	}
	return archive.Sum, nil
}
