package compiler

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

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
	data, err := os.ReadFile(filepath.Join(projectDir, filepath.FromSlash(file)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, &ArchitectureReadError{File: file, Cause: "the architecture model is missing; run stego apply"}
	}
	if err != nil {
		return nil, &ArchitectureReadError{File: file, Cause: err.Error()}
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
