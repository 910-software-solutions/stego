package compiler

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jsell-rh/stego/internal/gen"
)

func outputRelative(projectDir, outDir string) (string, error) {
	relative, err := filepath.Rel(projectDir, outDir)
	if err != nil || gen.ValidatePath(filepath.ToSlash(relative)) != nil {
		return "", fmt.Errorf("OutDir must be a subdirectory of ProjectDir")
	}
	first := strings.Split(filepath.ToSlash(relative), "/")[0]
	switch strings.ToLower(first) {
	case "fills", ".stego", ".git", "go.mod", "go.sum", "service.yaml", "deploy", "architecture":
		return "", fmt.Errorf("OutDir must not use reserved directory %q", first)
	}
	return relative, nil
}

func projectFilePath(outDir, name string) string {
	if isProjectRootFile(name) || isProjectGeneratedFile(name) {
		return name
	}
	return filepath.Join(outDir, name)
}

func checkFileTarget(root *os.Root, name string) error {
	if err := checkFilePath(root, name); err != nil {
		return err
	}
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("output target %s must be a regular file", name)
	}
	return nil
}

// checkFilePath rejects links and special files before apply starts. Root
// operations also enforce containment during each filesystem operation.
func checkFilePath(root *os.Root, name string) error {
	if err := gen.ValidatePath(filepath.ToSlash(name)); err != nil {
		return err
	}
	parts := strings.Split(filepath.ToSlash(name), "/")
	for i := range parts {
		path := filepath.Join(parts[:i+1]...)
		info, err := root.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("checking %s: %w", name, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symbolic link at %s", path)
		}
		if i < len(parts)-1 && !info.IsDir() {
			return fmt.Errorf("path parent %s must be a directory", path)
		}
		if i == len(parts)-1 && !info.Mode().IsRegular() && !info.IsDir() {
			return fmt.Errorf("path %s must be a regular file or directory", path)
		}
	}
	return nil
}

// writeRootFile replaces a complete file through a rename within the root.
// Readers cannot observe a truncated file. A failed write retains the old file.
// This does not make a multi-file apply atomic.
func writeRootFile(root *os.Root, name string, data []byte) (err error) {
	return writeRootFileMode(root, name, data, 0644)
}

func writeRootFileMode(root *os.Root, name string, data []byte, mode os.FileMode) error {
	_, err := publishRootFile(root, name, data, mode, true)
	return err
}

// createRootFile publishes complete bytes without replacing any existing path.
func createRootFile(root *os.Root, name string, data []byte) error {
	_, err := publishRootFile(root, name, data, 0644, false)
	return err
}

// publishRootFile writes the bytes to a temporary file and then puts it at name. published is the identity of the
// file that this call put at name, also when a later step fails, and nil when the call put no file there: a caller
// that removes its own files after a failure removes name only then, and only while name is still that file
// (os.SameFile), never a file that another process put there first or later (sec-419 LOW-1, sec-420 INFO-5).
func publishRootFile(root *os.Root, name string, data []byte, mode os.FileMode, replace bool) (published os.FileInfo, err error) {
	if err := checkFilePath(root, name); err != nil {
		return nil, err
	}
	directory := filepath.Dir(name)
	if err := root.MkdirAll(directory, 0755); err != nil {
		return nil, err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, fmt.Errorf("creating temporary file name: %w", err)
	}
	temporary := filepath.Join(directory, ".stego-write-"+hex.EncodeToString(nonce[:]))
	file, err := root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	defer func() {
		if removeErr := root.Remove(temporary); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("removing temporary file: %w", removeErr))
		}
	}()
	_, writeErr := file.Write(data)
	if writeErr == nil {
		writeErr = file.Chmod(mode.Perm())
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	// The identity of the file is that of the temporary file, read before the file is at name, so a file that
	// another process puts at name later never takes its place.
	identity, statErr := root.Lstat(temporary)
	if err := errors.Join(writeErr, closeErr, statErr); err != nil {
		return nil, err
	}
	if publishingFault != nil {
		publishingFault(name)
	}
	if replace {
		err = root.Rename(temporary, name)
	} else {
		err = root.Link(temporary, name)
	}
	if err != nil {
		return nil, err
	}
	if publishedFault != nil {
		err = publishedFault(name)
	}
	return identity, err
}

// publishedFault is nil in a product build. A test sets it to make the write of a file fail after the file is
// there, as when the removal of the temporary file fails.
var publishedFault func(name string) error

// publishingFault is nil in a product build. A test sets it to act as another process that puts a file at the name
// after the checks of the caller and before this write puts its own file there.
var publishingFault func(name string)
