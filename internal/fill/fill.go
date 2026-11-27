// Package fill holds Extending's fill as its manifest (fill.yaml) states it,
// and the reading of the manifest through the declaration's strict decoder.
package fill

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/910-software-solutions/stego/internal/parser"
	"gopkg.in/yaml.v3"
)

// ManifestFile is the name of the fill manifest in a fill's folder: the file
// that states the fill and holds its evidence and its sign-off.
const ManifestFile = "fill.yaml"

// Fill represents a human-authored implementation of a slot contract, as its
// fill manifest states it, with the evidence that its tests passed and a
// person's sign-off of that evidence.
type Fill struct {
	Kind       string        `yaml:"kind"`
	Name       string        `yaml:"name"`
	Implements string        `yaml:"implements"`
	Collection string        `yaml:"collection,omitempty"`
	Evidence   *FillEvidence `yaml:"evidence,omitempty"`
	SignOff    *FillSignOff  `yaml:"sign_off,omitempty"`
}

// YAMLRenamedKeys marks Fill as a parser.RenamedKeys: the strict decoder lets
// the renamed entity key through, so that UnmarshalYAML reports its rename.
func (Fill) YAMLRenamedKeys() []string { return []string{"entity"} }

// UnmarshalYAML detects the deprecated `entity:` key (renamed to `collection:`
// in the Collection type system migration) and returns a migration error.
func (f *Fill) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.MappingNode {
		for i := 0; i < len(value.Content)-1; i += 2 {
			if value.Content[i].Value == "entity" {
				return fmt.Errorf("the 'entity' key in fill declarations has been renamed to 'collection' — please update your fill.yaml")
			}
		}
	}
	type rawFill Fill
	var raw rawFill
	if err := value.Decode(&raw); err != nil {
		return err
	}
	*f = Fill(raw)
	return nil
}

// ParseFill reads and parses a fill YAML file. It refuses evidence or a
// sign-off with a field missing, and names the field.
func ParseFill(path string) (*Fill, error) {
	data, err := parser.ReadDocument(path)
	if err != nil {
		return nil, err
	}
	return decodeFill(data, path)
}

// decodeFill decodes data, the fill manifest at path, as ParseFill does. The
// path is for the errors only.
func decodeFill(data []byte, path string) (*Fill, error) {
	f, err := parser.ParseKind(data, path, "fill", func(f *Fill) string { return f.Kind })
	if err != nil {
		return nil, err
	}
	if f.Evidence != nil {
		if err := f.Evidence.check(); err != nil {
			return nil, fmt.Errorf("%s: evidence: %w", path, err)
		}
	}
	if f.SignOff != nil {
		if err := f.SignOff.check(); err != nil {
			return nil, fmt.Errorf("%s: sign_off: %w", path, err)
		}
	}
	return f, nil
}

// DecodeCollection decodes data, the fill manifest that the compiler read at
// path, and gives the collection the fill serves. It is the compiler's
// compiler.FillReader: cmd/stego gives it to the compiler, which may not
// import Extending (ST-STD-CODE-R03). It opens no file: the compiler reads the
// manifest inside the project folder.
func DecodeCollection(data []byte, path string) (string, error) {
	f, err := decodeFill(data, path)
	if err != nil {
		return "", err
	}
	return f.Collection, nil
}

// DecodeImplements decodes data, the fill manifest that the compiler read at
// path, and gives the slot the fill implements, as component.slot. It is the
// compiler's compiler.FillSlotReader.
func DecodeImplements(data []byte, path string) (string, error) {
	f, err := decodeFill(data, path)
	if err != nil {
		return "", err
	}
	return f.Implements, nil
}

// writeSection sets one top-level key of the manifest fills/<name>/fill.yaml to
// value, or removes the key when value is nil, and keeps each other key and its
// comments as they are. It writes a new file and renames it over the old one,
// so a reader sees the old manifest or the new one, never a part of either.
//
// It refuses a name outside the fill-name form before it opens a file, as
// ReadManifest does (security review of PR #9, INFO-1). It opens the fill
// folder once as an os.Root inside an os.Root of the fills folder, and reads,
// writes, renames and removes through that one root, so a link that takes the
// path out of the fills folder is refused also when a process puts it there
// after ReadManifest (security review of PR #518, item b, CWE-367), and a swap
// of the folder during the write cannot send the remove of the temporary file
// to another folder (security review of PR #9, INFO-2).
func writeSection(fills, name, key string, value any) (err error) {
	if err := checkFillName(name); err != nil {
		return err
	}
	path := filepath.Join(fills, name, ManifestFile)
	root, err := os.OpenRoot(fills)
	if err != nil {
		return err
	}
	defer root.Close()
	dir, err := root.OpenRoot(name)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer dir.Close()
	file, err := dir.Open(ManifestFile)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return err
	}
	data, err := io.ReadAll(io.LimitReader(file, parser.MaxDocumentBytes+1))
	file.Close()
	if err != nil {
		return err
	}
	if len(data) > parser.MaxDocumentBytes {
		return fmt.Errorf("%s: the fill manifest exceeds the %d byte limit", path, parser.MaxDocumentBytes)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("%s: the fill manifest is not a mapping", path)
	}
	top := document.Content[0]
	removeKey(top, key)
	if value != nil {
		var encoded yaml.Node
		if err := encoded.Encode(value); err != nil {
			return err
		}
		top.Content = append(top.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, &encoded)
	}
	out, err := encodeNode(&document)
	if err != nil {
		return err
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	temp := ".fill-" + hex.EncodeToString(random[:]) + ".yaml"
	written, err := dir.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	renamed := false
	defer func() {
		if !renamed {
			written.Close()
			if removed := dir.Remove(temp); removed != nil && err == nil {
				err = removed
			}
		}
	}()
	if _, err := written.Write(out); err != nil {
		return err
	}
	if err := written.Chmod(info.Mode().Perm()); err != nil {
		return err
	}
	if err := written.Sync(); err != nil {
		return err
	}
	if err := written.Close(); err != nil {
		return err
	}
	if err := dir.Rename(temp, ManifestFile); err != nil {
		return err
	}
	renamed = true
	return nil
}

// removeKey removes a key and its value from a mapping node.
func removeKey(mapping *yaml.Node, key string) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
			return
		}
	}
}

// encodeNode writes a YAML node with an indent of two spaces.
func encodeNode(node *yaml.Node) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := yaml.NewEncoder(&buffer)
	encoder.SetIndent(2)
	if err := encoder.Encode(node); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}
