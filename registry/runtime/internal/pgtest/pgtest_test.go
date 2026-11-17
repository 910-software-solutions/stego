package pgtest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeTool writes a tool that records its arguments, one call to a line, in the file calls of its folder.
func fakeTool(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	tool, calls := filepath.Join(dir, "tool"), filepath.Join(dir, "calls")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\necho \"$@\" >> \""+calls+"\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return tool, calls
}

func TestReap(t *testing.T) {
	t.Run("ST-STD-LOCAL-R05-T02 A container of an image with no time limit is removed when the tests end and when the test binary is killed", func(t *testing.T) {
		// Main's path: remove removes each container that Reap took.
		tool, calls := fakeTool(t)
		mu.Lock()
		reaped = append(reaped, reapedContainer{path: tool, name: "spicedb-a"})
		mu.Unlock()
		remove()
		if data, err := os.ReadFile(calls); err != nil || strings.TrimSpace(string(data)) != "rm --force spicedb-a" {
			t.Fatalf("remove called the tool with %q, %v; want rm --force spicedb-a", data, err)
		}
		// The killed path: the reaper removes the container once the watched process has ended, and not before.
		tool, calls = fakeTool(t)
		binary := exec.Command("sleep", "3600")
		if err := binary.Start(); err != nil {
			t.Fatal(err)
		}
		if err := reap(binary.Process.Pid, tool, "spicedb-b"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(calls); !os.IsNotExist(err) {
			t.Fatalf("the reaper called the tool while the watched process runs: %v", err)
		}
		if err := binary.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		_ = binary.Wait()
		// The reaper polls once a second; the test waits for its call with no bound of its own (-test.timeout).
		for {
			data, err := os.ReadFile(calls)
			if err == nil && strings.HasSuffix(string(data), "\n") {
				if got := strings.TrimSpace(string(data)); got != "rm --force spicedb-b" {
					t.Fatalf("the reaper called the tool with %q, want rm --force spicedb-b", got)
				}
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	})
}
