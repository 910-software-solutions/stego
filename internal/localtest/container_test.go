package localtest

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The proofs that the containers of localtest are started and removed as ST-STD-LOCAL asks (security review of PR #7,
// L4). They run with stand-ins for podman and docker, shell scripts that write each call to a log, so they run on a
// host with no container engine. The run of the real image is CI's.

// The runs of the stand-in tools: one that creates the container, one that cannot, and one that takes a second and
// writes run-end to the log when it ends.
const (
	runStarts = `echo 0123456789ab`
	runFails  = `echo "cannot start: no cgroup controllers" >&2; exit 125`
	runSlow   = `sleep 1; echo run-end >> "$log"; echo 0123456789ab`
)

// fakeTool writes a stand-in for the tool name into dir. It writes each call, one line of its arguments, to the
// file log. A run runs the shell code run; a port prints the address port.
func fakeTool(t *testing.T, dir, name, log, run, port string) {
	t.Helper()
	script := "#!/bin/sh\n" +
		`log='` + log + "'\n" +
		`echo "$*" >> "$log"` + "\n" +
		`case "$1" in` + "\n" +
		`run) ` + run + ` ;;` + "\n" +
		`port) echo '` + port + `' ;;` + "\n" +
		`esac` + "\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// calls gives the lines of the log, each split into its arguments.
func calls(t *testing.T, log string) [][]string {
	t.Helper()
	data, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var lines [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			lines = append(lines, strings.Fields(line))
		}
	}
	return lines
}

// hasPair tells if args holds the flag followed by the value.
func hasPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// groupAlive tells if a process that is not a zombie is in the process group pgid, from /proc.
func groupAlive(t *testing.T, pgid int) bool {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Skip("no /proc on this host: the process group of the reaper cannot be read")
	}
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if err != nil {
			continue
		}
		// pid (comm) state ppid pgrp ...: comm can hold spaces, so the fields start after the last ')'.
		fields := strings.Fields(string(data[strings.LastIndexByte(string(data), ')')+1:]))
		if len(fields) > 2 && fields[0] != "Z" && fields[2] == strconv.Itoa(pgid) {
			return true
		}
	}
	return false
}

// waitFor waits up to limit for ok to hold.
func waitFor(limit time.Duration, ok func() bool) bool {
	for deadline := time.Now().Add(limit); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if ok() {
			return true
		}
	}
	return ok()
}

func TestSpiceDBContainer(t *testing.T) {
	t.Run("ST-STD-LOCAL-R04-T03 The SpiceDB container runs pinned, unprivileged and limited, podman first, and is removed", func(t *testing.T) {
		dir := t.TempDir()
		podmanLog, dockerLog := filepath.Join(dir, "podman.log"), filepath.Join(dir, "docker.log")
		// podman is on PATH but cannot start the container, as a rootless podman without cgroup controllers cannot;
		// docker is tried next.
		fakeTool(t, dir, "podman", podmanLog, runFails, "")
		fakeTool(t, dir, "docker", dockerLog, runStarts, "127.0.0.1:43123")
		t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		endpoint, name, err := startSpiceDB([]string{"serve-testing"})
		if err != nil {
			t.Fatal(err)
		}
		if endpoint != "127.0.0.1:43123" || !strings.HasPrefix(name, "stego-localtest-spicedb-") {
			t.Fatalf("got the endpoint %q and the name %q", endpoint, name)
		}
		podman := calls(t, podmanLog)
		if len(podman) == 0 || podman[0][0] != "run" {
			t.Fatalf("podman was not tried first: %v", podman)
		}
		if !slices.Contains(podman[0], "--timeout") {
			t.Errorf("the podman run has no time limit: %v", podman[0])
		}
		docker := calls(t, dockerLog)
		if len(docker) == 0 || docker[0][0] != "run" {
			t.Fatalf("docker did not run the container after podman failed: %v", docker)
		}
		run := docker[0]
		for _, pair := range [][2]string{{"--user", "65532:65532"}, {"--cap-drop", "ALL"}, {"--security-opt", "no-new-privileges"},
			{"--memory", "256m"}, {"--cpus", "1"}, {"--pids-limit", "512"}, {"--publish", "127.0.0.1::50051"}, {"--name", name}} {
			if !hasPair(run, pair[0], pair[1]) {
				t.Errorf("the run has no %s %s: %v", pair[0], pair[1], run)
			}
		}
		if !slices.Contains(run, "--rm") || len(run) < 2 || run[len(run)-2] != SpiceDBImage || run[len(run)-1] != "serve-testing" {
			t.Errorf("the run does not start the pinned image %s with serve-testing and --rm: %v", SpiceDBImage, run)
		}
		// The failed podman container is removed before docker starts; the docker one is removed with the test.
		if !slices.ContainsFunc(podman, func(c []string) bool { return c[0] == "rm" }) {
			t.Errorf("the failed podman container was not removed: %v", podman)
		}
		mu.Lock()
		r := running[name]
		mu.Unlock()
		if r == nil {
			t.Fatal("localtest does not hold the container for its removal")
		}
		removeContainer(name)
		if !slices.ContainsFunc(calls(t, dockerLog), func(c []string) bool { return slices.Equal(c, []string{"rm", "--force", name}) }) {
			t.Errorf("the container was not removed: %v", calls(t, dockerLog))
		}
		if !waitFor(5*time.Second, func() bool { return !groupAlive(t, r.reaper.pid) }) {
			t.Error("the reaper and its timer still run after the removal")
		}
	})
	t.Run("ST-STD-LOCAL-R04-T04 With neither podman nor docker on PATH, the SpiceDB container fails with the reason", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		_, name, err := startSpiceDB([]string{"serve-testing"})
		if err == nil || name != "" || !strings.Contains(err.Error(), "neither podman nor docker") {
			t.Fatalf("got the name %q and the error %v", name, err)
		}
	})
}

// fastReaper makes the removals of a reaper after an end that is not clean one second apart, and two in all, for the
// test.
func fastReaper(t *testing.T) {
	every, times := reaperEvery, reaperTimes
	reaperEvery, reaperTimes = time.Second, 1
	t.Cleanup(func() { reaperEvery, reaperTimes = every, times })
}

// removals counts the calls rm --force name in the log.
func removals(t *testing.T, log, name string) int {
	count := 0
	for _, c := range calls(t, log) {
		if slices.Equal(c, []string{"rm", "--force", name}) {
			count++
		}
	}
	return count
}

func TestContainerReaper(t *testing.T) {
	fastReaper(t)
	start := func(t *testing.T, life time.Duration) (*reaper, string) {
		t.Helper()
		dir := t.TempDir()
		log := filepath.Join(dir, "tool.log")
		fakeTool(t, dir, "docker", log, runStarts, "")
		r, err := newReaper(filepath.Join(dir, "docker"), "stego-localtest-c1", life)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = r.stop.Close() })
		return r, log
	}
	removed := func(t *testing.T, log string) bool {
		return slices.ContainsFunc(calls(t, log), func(c []string) bool { return slices.Equal(c, []string{"rm", "--force", "stego-localtest-c1"}) })
	}
	t.Run("ST-STD-LOCAL-R05-T05 The reaper removes the container when the test binary ends without a clean exit", func(t *testing.T) {
		r, log := start(t, time.Hour)
		// A killed binary closes the pipe with no line done.
		_ = r.stop.Close()
		if !waitFor(5*time.Second, func() bool { return removed(t, log) }) {
			t.Fatal("the reaper did not remove the container")
		}
		if !waitFor(5*time.Second, func() bool { return !groupAlive(t, r.pid) }) {
			t.Error("the reaper or its timer still runs")
		}
		// It removes the container again, for a run of the tool that ended after the first removal.
		if got := removals(t, log, "stego-localtest-c1"); got != 2 {
			t.Errorf("the reaper removed the container %d times, not 2", got)
		}
	})
	t.Run("ST-STD-LOCAL-R05-T06 The reaper removes the container at the time limit while the test binary runs", func(t *testing.T) {
		r, log := start(t, time.Second)
		if !waitFor(10*time.Second, func() bool { return removed(t, log) }) {
			t.Fatal("the reaper did not remove the container at the time limit")
		}
		_, _ = r.stop.WriteString("done\n")
		_ = r.stop.Close()
		if !waitFor(5*time.Second, func() bool { return !groupAlive(t, r.pid) }) {
			t.Error("the reaper or its timer still runs")
		}
	})
	t.Run("ST-STD-LOCAL-R05-T07 After a clean exit the reaper ends at once and leaves no process", func(t *testing.T) {
		r, log := start(t, time.Hour)
		_, _ = r.stop.WriteString("done\n")
		_ = r.stop.Close()
		if !waitFor(5*time.Second, func() bool { return !groupAlive(t, r.pid) }) {
			t.Fatal("the reaper or its timer of one hour still runs")
		}
		if removed(t, log) {
			t.Error("the reaper removed the container after a clean exit")
		}
	})
}

func TestContainerRemovalDuringItsRun(t *testing.T) {
	fastReaper(t)
	// begin starts the container c1 with a docker whose run takes a second, in a goroutine, and waits until the run is
	// in flight.
	begin := func(t *testing.T) (string, chan error) {
		t.Helper()
		dir := t.TempDir()
		log := filepath.Join(dir, "docker.log")
		fakeTool(t, dir, "docker", log, runSlow, "")
		result := make(chan error, 1)
		go func() {
			result <- runContainer(context.Background(), filepath.Join(dir, "docker"), "stego-localtest-c1", time.Hour, nil, []string{"run", "--rm", "--name", "stego-localtest-c1"})
		}()
		if !waitFor(5*time.Second, func() bool { return len(calls(t, log)) > 0 }) {
			t.Fatal("the run did not start")
		}
		return log, result
	}
	// order gives the first word of each call in the log, and run-end.
	order := func(t *testing.T, log string) []string {
		var words []string
		for _, c := range calls(t, log) {
			words = append(words, c[0])
		}
		return words
	}
	t.Run("ST-STD-LOCAL-R05-T08 A removal during the run of the container waits for the end of the run", func(t *testing.T) {
		log, result := begin(t)
		mu.Lock()
		r := running["stego-localtest-c1"].reaper
		mu.Unlock()
		// As the SIGINT path of Main does while a test starts the container.
		removeContainer("stego-localtest-c1")
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		if got := order(t, log); !slices.Equal(got, []string{"run", "run-end", "rm"}) {
			t.Fatalf("the calls are %v, not run, run-end and rm", got)
		}
		if !waitFor(5*time.Second, func() bool { return !groupAlive(t, r.pid) }) {
			t.Error("the reaper was not told done after the removal")
		}
	})
	t.Run("ST-STD-LOCAL-R05-T09 When the removal cannot wait for the end of the run, the reaper removes the container after it", func(t *testing.T) {
		wait := removeWait
		removeWait = 100 * time.Millisecond
		t.Cleanup(func() { removeWait = wait })
		log, result := begin(t)
		mu.Lock()
		r := running["stego-localtest-c1"].reaper
		mu.Unlock()
		removeContainer("stego-localtest-c1")
		if !groupAlive(t, r.pid) {
			t.Fatal("the reaper was told done before the run ended")
		}
		// The end of the binary closes the pipe of the reaper.
		_ = r.stop.Close()
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		if !waitFor(10*time.Second, func() bool { return !groupAlive(t, r.pid) }) {
			t.Fatal("the reaper still runs")
		}
		got := order(t, log)
		end := slices.Index(got, "run-end")
		if end < 0 || !slices.Contains(got[end:], "rm") {
			t.Fatalf("no removal after the end of the run: %v", got)
		}
	})
}
