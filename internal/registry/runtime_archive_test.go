package registry

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
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
	t.Run("ST-STD-RUNTIME-R04-T05 The runtime archive refuses a folder whose module zip is larger than its limit", func(t *testing.T) {
		const version = "v0.1.0"
		folder := filepath.Join(t.TempDir(), "runtime")
		if err := os.MkdirAll(folder, 0o755); err != nil {
			t.Fatal(err)
		}
		// Random bytes do not compress, so the zip is larger than 8 KiB.
		noise := make([]byte, 8<<10)
		if _, err := rand.Read(noise); err != nil {
			t.Fatal(err)
		}
		for name, data := range map[string][]byte{"go.mod": []byte("module " + RuntimeModule + "\n\ngo 1.26\n"), "noise.go": []byte("package runtime\n\nconst noise = " + strconv.Quote(string(noise)) + "\n")} {
			if err := os.WriteFile(filepath.Join(folder, name), data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		// The limit of 64 MiB takes the zip; its size is the smallest limit that takes it.
		whole := filepath.Join(t.TempDir(), "whole.zip")
		if _, err := WriteRuntimeArchive(folder, RuntimeModule, version, whole); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(whole)
		if err != nil {
			t.Fatal(err)
		}
		size := int(info.Size())
		if MaxRuntimeArchive != 64<<20 || size <= 8<<10 {
			t.Fatalf("the limit is %d and the zip %d bytes, want 64 MiB and more than 8 KiB", MaxRuntimeArchive, size)
		}
		exact := filepath.Join(t.TempDir(), "exact.zip")
		if _, err := writeRuntimeArchive(folder, RuntimeModule, version, exact, size); err != nil {
			t.Fatalf("a limit of the size of the zip refused it: %v", err)
		}
		// One byte less is refused, and no zip file is written.
		short := filepath.Join(t.TempDir(), "short.zip")
		if _, err := writeRuntimeArchive(folder, RuntimeModule, version, short, size-1); !errors.Is(err, ErrRuntimeArchiveSize) {
			t.Fatalf("a zip of %d bytes with a limit of %d gave %v, want ErrRuntimeArchiveSize", size, size-1, err)
		}
		if _, err := os.Lstat(short); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a refused zip left the file %s (%v)", short, err)
		}
		// The buffer of the zip has a fixed capacity, and no method other than Write puts bytes in it.
		b := newCappedBuffer(10)
		if _, ok := any(b).(io.ReaderFrom); ok {
			t.Error("the capped buffer has a ReadFrom method, which could write past its capacity")
		}
		if _, ok := any(b).(io.StringWriter); ok {
			t.Error("the capped buffer has a WriteString method, which could write past its capacity")
		}
		for _, w := range []struct {
			size int
			ok   bool
		}{{6, true}, {5, false}, {4, true}, {1, false}} {
			if n, err := b.Write(make([]byte, w.size)); (err == nil) != w.ok || (w.ok && n != w.size) || (!w.ok && !errors.Is(err, ErrRuntimeArchiveSize)) {
				t.Errorf("a write of %d bytes gave %d %v, want accepted %t", w.size, n, err, w.ok)
			}
		}
		if len(b.Bytes()) != 10 || cap(b.data) != 10 {
			t.Errorf("the buffer holds %d bytes in a capacity of %d, want 10 in 10", len(b.Bytes()), cap(b.data))
		}
	})
}
