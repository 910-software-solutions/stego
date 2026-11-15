package compiler

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// MaxArchitectureBytes bounds the architecture model that ReadArchitecture reads (security review of #341, L4).
const MaxArchitectureBytes = 8 << 20

// ArchitectureReadError is a model that cannot be read: the file and the cause (ST-GEN-UC16-R06).
type ArchitectureReadError struct {
	File  string
	Cause string
}

func (e *ArchitectureReadError) Error() string {
	return fmt.Sprintf("%s: %s", e.File, e.Cause)
}

// ReadArchitecture is the one way the queries of the architecture model read it (ST-GEN-RM04): the file
// architecture/architecture.json that the last apply wrote in the project folder, checked against the record's hash
// and against the schema and its rules. It refuses a missing model, a model that does not match the record, and a
// model that is not valid, naming the file and the cause.
func ReadArchitecture(projectDir string) ([]byte, error) {
	file := ArchitectureFile
	data, err := readArchitectureFile(projectDir)
	if err != nil {
		return nil, err
	}
	state, err := LoadState(filepath.Join(projectDir, ".stego", "state.yaml"))
	if err != nil {
		return nil, &ArchitectureReadError{File: ".stego/state.yaml", Cause: "the record cannot be read: " + err.Error()}
	}
	if state.LastApplied == nil || state.LastApplied.ProjectFiles[file] == "" {
		return nil, &ArchitectureReadError{File: file, Cause: "the record does not list the architecture model; run stego apply"}
	}
	if HashBytes(data) != state.LastApplied.ProjectFiles[file] {
		return nil, &ArchitectureReadError{File: file, Cause: "the architecture model does not match the record; it was changed after apply, so run stego apply"}
	}
	if err := ValidateArchitecture(data); err != nil {
		return nil, &ArchitectureReadError{File: file, Cause: err.Error()}
	}
	return data, nil
}

// readArchitectureFile reads the model through the project folder as a root, so that no part of the path leaves it;
// it refuses a symbolic link, a file that is not a regular file and a file beyond MaxArchitectureBytes before it
// reads more.
func readArchitectureFile(projectDir string) ([]byte, error) {
	file := ArchitectureFile
	root, err := os.OpenRoot(projectDir)
	if err != nil {
		return nil, &ArchitectureReadError{File: file, Cause: err.Error()}
	}
	defer root.Close()
	info, err := root.Lstat(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, &ArchitectureReadError{File: file, Cause: "the architecture model is missing; run stego apply"}
	}
	if err != nil {
		return nil, &ArchitectureReadError{File: file, Cause: err.Error()}
	}
	if !info.Mode().IsRegular() {
		return nil, &ArchitectureReadError{File: file, Cause: "the architecture model is not a regular file"}
	}
	// The size of the file stops a large one before any read; the bounded read below holds for a file that grows.
	if info.Size() > MaxArchitectureBytes {
		return nil, &ArchitectureReadError{File: file, Cause: fmt.Sprintf("the architecture model is larger than %d bytes", MaxArchitectureBytes)}
	}
	handle, err := root.Open(file)
	if err != nil {
		return nil, &ArchitectureReadError{File: file, Cause: err.Error()}
	}
	defer handle.Close()
	data, err := io.ReadAll(io.LimitReader(handle, MaxArchitectureBytes+1))
	if err != nil {
		return nil, &ArchitectureReadError{File: file, Cause: err.Error()}
	}
	if len(data) > MaxArchitectureBytes {
		return nil, &ArchitectureReadError{File: file, Cause: fmt.Sprintf("the architecture model is larger than %d bytes", MaxArchitectureBytes)}
	}
	return data, nil
}
