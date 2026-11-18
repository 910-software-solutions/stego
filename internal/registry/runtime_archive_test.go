package registry

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/sumdb/dirhash"
)

func TestRuntimeArchiveOneCopy(t *testing.T) {
	t.Run("ST-STD-RUNTIME-R04-T04 The runtime archive hashes the zip and reads its go.mod from the one copy that it writes", func(t *testing.T) {
		const version = "v0.1.0"
		goMod := "module " + RuntimeModule + "\n\ngo 1.26\n"
		prefix := RuntimeModule + "@" + version + "/"
		// zipOf gives a zip of the entries, in their order; two entries can have the same name.
		zipOf := func(entries ...[2]string) []byte {
			var data bytes.Buffer
			w := zip.NewWriter(&data)
			for _, entry := range entries {
				f, err := w.Create(entry[0])
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.Write([]byte(entry[1])); err != nil {
					t.Fatal(err)
				}
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			return data.Bytes()
		}

		// The archive of a folder: the sum is the hash of the file that it wrote, and the go.mod is the one of the zip.
		folder := filepath.Join(t.TempDir(), "runtime")
		if err := os.MkdirAll(filepath.Join(folder, "money"), 0o755); err != nil {
			t.Fatal(err)
		}
		for name, text := range map[string]string{"go.mod": goMod, "money/money.go": "package money\n"} {
			if err := os.WriteFile(filepath.Join(folder, name), []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		zipPath := filepath.Join(t.TempDir(), "runtime.zip")
		archive, err := WriteRuntimeArchive(folder, RuntimeModule, version, zipPath)
		if err != nil {
			t.Fatal(err)
		}
		written, err := dirhash.HashZip(zipPath, dirhash.Hash1)
		if err != nil || archive.Sum != written || string(archive.Mod) != goMod {
			t.Fatalf("the archive gives the sum %s and the go.mod %q; the written zip has the sum %s (%v), want the same and %q", archive.Sum, archive.Mod, written, err, goMod)
		}

		// The same bytes give the same hashes as the file of those bytes.
		data, err := os.ReadFile(zipPath)
		if err != nil {
			t.Fatal(err)
		}
		again, err := runtimeArchiveOf(data, prefix+"go.mod")
		if err != nil || again.Sum != archive.Sum || again.ModSum != archive.ModSum || string(again.Mod) != goMod {
			t.Fatalf("the archive of the written bytes is %+v, %v; want %+v", again, err, archive)
		}

		// A zip with two go.mod entries is refused: the archive never picks one of two.
		two := zipOf([2]string{prefix + "go.mod", goMod}, [2]string{prefix + "go.mod", "module example.com/other\n"})
		if _, err := runtimeArchiveOf(two, prefix+"go.mod"); err == nil || !strings.Contains(err.Error(), "more than one go.mod") {
			t.Fatalf("a zip with two go.mod entries gave %v, want a refusal", err)
		}
		// A zip with no go.mod is refused.
		if _, err := runtimeArchiveOf(zipOf([2]string{prefix + "money/money.go", "package money\n"}), prefix+"go.mod"); err == nil || !strings.Contains(err.Error(), "no go.mod") {
			t.Fatalf("a zip with no go.mod gave %v, want a refusal", err)
		}
	})
}
