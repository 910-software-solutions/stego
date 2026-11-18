package pgtest

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
		// waitCall waits for the one call of the tool, with no bound of its own (-test.timeout).
		waitCall := func(calls, want string) {
			t.Helper()
			for {
				data, err := os.ReadFile(calls)
				if err == nil && strings.HasSuffix(string(data), "\n") {
					if got := strings.TrimSpace(string(data)); got != want {
						t.Fatalf("the reaper called the tool with %q, want %s", got, want)
					}
					return
				}
				time.Sleep(100 * time.Millisecond)
			}
		}
		// Main's path: remove removes each container and then each network that Reap and ReapNetwork took, and
		// stops their reapers, so no reaper outlives a clean exit and calls the tool again.
		tool, calls := fakeTool(t)
		Reap(tool, "spicedb-a")
		ReapNetwork(tool, "net-a")
		mu.Lock()
		took := append([]reapedContainer(nil), reaped...)
		mu.Unlock()
		remove()
		for _, r := range took {
			if r.reaper == nil {
				t.Fatalf("no reaper started for the %s %s", r.kind, r.name)
			}
			<-r.reaper.ended
		}
		if data, err := os.ReadFile(calls); err != nil || strings.TrimSpace(string(data)) != "rm --force spicedb-a\nnetwork rm net-a" {
			t.Fatalf("remove and the reapers called the tool with %q, %v; want the container and then the network, once each", data, err)
		}

		// The killed path: the end of the binary closes the write end of the pipe, and only then does the reaper remove
		// the container. It is in a process group of its own, so a kill of the group of the binary does not take it.
		tool, calls = fakeTool(t)
		r, err := reap(kindContainer, tool, "spicedb-b")
		if err != nil {
			t.Fatal(err)
		}
		if group, err := syscall.Getpgid(r.pid); err != nil || group != r.pid || group == syscall.Getpgrp() {
			t.Fatalf("the reaper is in the process group %d (%v), want one of its own, not %d", group, err, syscall.Getpgrp())
		}
		if _, err := os.Stat(calls); !os.IsNotExist(err) {
			t.Fatalf("the reaper called the tool while the binary runs: %v", err)
		}
		_ = r.stop.Close()
		waitCall(calls, "rm --force spicedb-b")
		<-r.ended

		// A network of the harness is reaped too, after its containers.
		tool, calls = fakeTool(t)
		if r, err = reap(kindNetwork, tool, "net-b"); err != nil {
			t.Fatal(err)
		}
		_ = r.stop.Close()
		waitCall(calls, "network rm net-b")
		<-r.ended
	})
}
