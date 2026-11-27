// Package localtest gives the tests of one test binary of the root module the local services and the module cache
// that ST-STD-LOCAL asks for. It is test support: only test files import it, and no generated code does.
//
// A package's TestMain calls Main. A test then calls PostgresDSN for the superuser URL of a local PostgreSQL server,
// and GoEnv for the environment of a go command in a generated module.
//
// PostgreSQL (ST-STD-LOCAL-R04): the server is the one that STEGO_TEST_POSTGRES_DSN names, a superuser URL, when the
// variable is set, as CI sets it for its job service; else one container of PostgresImage per test binary, which the
// first test that needs it starts and which Main removes when the tests end. The container runs with podman when
// podman is on PATH and can start it, else with docker: as the image's own unprivileged user, with no capabilities
// and no new privileges, with 2 GiB of memory, two CPUs and at most 1024 processes, its data on a tmpfs, published on
// 127.0.0.1 at a free port only, and with a time limit (the binary's -test.timeout and one minute, else one hour). A
// reaper process in a process group of its own removes it when the test binary is killed (ST-STD-LOCAL-R05), as the
// reaper of the runtime module's pgtest does. When neither podman nor docker is on PATH, each test that needs the
// server fails with the reason: a missing result is not a pass.
//
// SpiceDB (ST-STD-LOCAL-R04): SpiceDB starts one container of SpiceDBImage for each test that calls it, with podman
// first and docker next, as the PostgreSQL container does: as the image's own unprivileged user (65532), with no
// capabilities and no new privileges, with 256 MiB of memory, one CPU and at most 512 processes, published on
// 127.0.0.1 at a free port only. The image holds only the spicedb and grpc_health_probe programs, so it has no timeout
// program for an entrypoint. Its time limit is the --timeout of podman, and for both tools the reaper, which removes
// the container when the time limit ends. The cleanup of the test, Main and the reaper remove it (ST-STD-LOCAL-R05).
// When neither podman nor docker is on PATH, the test fails with the reason.
//
// Modules (ST-STD-LOCAL-R03): localtest keeps a module cache of its own, apart from the GOMODCACHE of the developer,
// which other tools and tests fill with modules that no sum database checked. It is the folder localtest-modules in
// STEGO_TEST_RUNS when that variable names a folder, else in the user cache folder (os.UserCacheDir); never in the
// shared GOMODCACHE and never in the temporary folder. The first call of GoEnv fills it, once per test binary, with each
// module that testdata/modules, testdata/modules-other and the build of ./cmd/stego need: through the module proxy
// of the environment, with the sum database sum.golang.org on and GONOSUMDB, GOPRIVATE, GONOPROXY, GOINSECURE and
// GOSUMDB set, not read from the environment or the go env file, so the go command checks each module against go.sum
// or the sum database before it writes it. Each go command of a generated module then has that cache as its
// GOMODCACHE and its download folder as its only module proxy (GOPROXY=file://...,off), with no sum database
// (GOSUMDB=off, as no host can be reached), no private or insecure module and no version control, so it reaches no
// host. CheckSums is the second check: after a go mod tidy of a generated module, it fails the test when the module's
// go.sum holds a line that the go.sum files of testdata and of the root module do not hold.
package localtest

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/sumdb/dirhash"
)

// PostgresImage is the pinned PostgreSQL server image: postgres:18.6-alpine, by the digest of the image that the CI job
// services of STEGO run.
const PostgresImage = "docker.io/library/postgres@sha256:d3e1620b530c944afa6e887d22eb899824da68e19c52024bf98f5220c88a65b2"

// SpiceDBImage is the pinned SpiceDB image of the local stand-in: version 1.57.0 at its digest, the image of the
// runtime's own SpiceDB tests.
const SpiceDBImage = "authzed/spicedb:v1.57.0@sha256:a170d372c9f6b921bb6849064df6ca3566dd5b174882c1a2c4300c2ae1d60efd"

const (
	// PostgresVariable names an existing server, a superuser URL; when it is set, no container starts.
	PostgresVariable = "STEGO_TEST_POSTGRES_DSN"
	// startLimit is the time the container has to pull its image and become ready.
	startLimit = 3 * time.Minute
	// defaultLife is the time limit of the container when the test binary has no -test.timeout.
	defaultLife = time.Hour
)

var (
	postgresOnce sync.Once
	dsn          string
	failWhy      string

	mu sync.Mutex
	// running holds each container that localtest started and did not remove yet, by its name.
	running = map[string]*started{}

	modulesOnce sync.Once
	modulesWhy  string
	// moduleCache is the module cache of localtest, and cacheProxy its download folder, as a file URL.
	moduleCache, cacheProxy string
)

// checkedSumsFile is the file in the module cache of localtest that holds the go.sum lines that its last fill checked.
const checkedSumsFile = "checked-sums.txt"

// Main runs the tests of m, removes each container that a test started, if any, and gives the exit code
// for os.Exit. It watches no signal: tests of the root module send SIGINT and SIGTERM to their own process, as the
// interrupt tests of stego build download do, and a handler would end the binary. A binary that a signal ends is
// covered by the reaper of the container, in its own process group, and by the time limit of the container.
//
//	func TestMain(m *testing.M) { os.Exit(localtest.Main(m)) }
func Main(m *testing.M) int {
	code := m.Run()
	remove()
	if emptyDir != "" {
		_ = os.RemoveAll(emptyDir)
	}
	return code
}

// MainModules is Main for a package whose tests run go commands with GoEnv: it fills the module cache of localtest
// first, and when that fails it runs no test, names the reason on standard error and gives 1, so no test of the
// binary runs a go command without the checked cache (security review of PR #510, L4).
//
//	func TestMain(m *testing.M) { os.Exit(localtest.MainModules(m)) }
func MainModules(m *testing.M) int {
	modulesOnce.Do(downloadModules)
	if modulesWhy != "" {
		fmt.Fprintln(os.Stderr, "localtest: the module cache cannot be used, so no test runs: "+modulesWhy)
		return 1
	}
	return Main(m)
}

// PostgresDSN gives the superuser URL of the local server, and starts the container the first time a test of the
// binary asks. It fails the test with the reason when the server cannot start.
func PostgresDSN(t testing.TB) string {
	t.Helper()
	postgresOnce.Do(startPostgres)
	if dsn == "" {
		t.Fatal("localtest: " + failWhy)
	}
	return dsn
}

// PostgresEnv gives the environment entries that hand the local server to the tests of a generated module: the
// superuser URL in STEGO_TEST_POSTGRES_DSN, and STEGO_REQUIRE_POSTGRES=1, so that such a test fails, never skips,
// when it finds no server. It starts the server as PostgresDSN does.
func PostgresEnv(t testing.TB) []string {
	t.Helper()
	return []string{PostgresVariable + "=" + PostgresDSN(t), "STEGO_REQUIRE_POSTGRES=1"}
}

// startPostgres finds the server: the variable, else a new container. It sets dsn, or failWhy.
func startPostgres() {
	if v := os.Getenv(PostgresVariable); v != "" {
		if u, err := url.Parse(v); err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
			failWhy = PostgresVariable + " must be a postgres:// URL"
			return
		}
		dsn = v
		return
	}
	found := containerTools()
	if len(found) == 0 {
		failWhy = "neither podman nor docker is on PATH, so the PostgreSQL container of the test cannot start; install one, or set " +
			PostgresVariable + " to a local server"
		return
	}
	// podman comes first. When it is there but cannot start the container, as a rootless podman without the cgroup
	// controllers for the limits cannot, docker is tried next.
	var errs []string
	for _, path := range found {
		got, name, err := runPostgres(path)
		if err == nil {
			dsn = got
			return
		}
		removeContainer(name)
		errs = append(errs, err.Error())
	}
	failWhy = "start the PostgreSQL container: " + strings.Join(errs, "; then: ")
}

// runPostgres starts the container with the tool, waits until the server accepts TCP connections inside it, and
// gives the superuser URL on the published port and the name of the container.
func runPostgres(path string) (string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), startLimit)
	defer cancel()
	password := suffix() + suffix()
	life := lifeLimit()
	name := fmt.Sprintf("stego-localtest-pg-%d-%s", os.Getpid(), suffix())
	// The image's user postgres (uid 70) owns its data on the tmpfs; the tmpfs folders are open to it with the
	// sticky bit, as the image's own folders are.
	args := []string{"run", "--detach", "--rm", "--name", name,
		"--user", "70:70", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--memory", "2g", "--cpus", "2", "--pids-limit", "1024", "--shm-size", "256m",
		"--tmpfs", "/var/lib/postgresql:rw,size=1536m,mode=1777",
		"--tmpfs", "/var/run/postgresql:rw,size=8m,mode=1777",
		"--publish", "127.0.0.1::5432",
		// The password is in the environment of the tool, not in its arguments, which any local user can read.
		"--env", "POSTGRES_PASSWORD",
		"--env", "POSTGRES_INITDB_ARGS=--auth-host=scram-sha-256 --auth-local=trust",
		"--entrypoint", "timeout",
		PostgresImage,
		fmt.Sprintf("%ds", int(life.Seconds())), "docker-entrypoint.sh", "postgres",
		"-c", "fsync=off", "-c", "synchronous_commit=off", "-c", "full_page_writes=off", "-c", "max_connections=300"}
	if err := runContainer(ctx, path, name, life, append(os.Environ(), "POSTGRES_PASSWORD="+password), args); err != nil {
		return "", name, err
	}
	out, err := command(ctx, path, "port", name, "5432/tcp")
	if err != nil {
		return "", name, err
	}
	host, port, err := net.SplitHostPort(strings.TrimSpace(strings.SplitN(out, "\n", 2)[0]))
	if err != nil {
		return "", name, fmt.Errorf("read the published port from %q: %w", out, err)
	}
	// The image's entrypoint first runs a server for initdb that listens only on its socket; a TCP connection
	// inside the container reaches only the final server.
	for {
		if _, err := command(ctx, path, "exec", name, "pg_isready", "--host", "127.0.0.1", "--username", "postgres", "--timeout", "2"); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			logs, _ := command(context.Background(), path, "logs", "--tail", "40", name)
			return "", name, fmt.Errorf("the server is not ready after %v; its last log lines:\n%s", startLimit, logs)
		case <-time.After(200 * time.Millisecond):
		}
	}
	u := url.URL{Scheme: "postgres", User: url.UserPassword("postgres", password), Host: net.JoinHostPort(host, port),
		Path: "/postgres", RawQuery: "sslmode=disable"}
	return u.String(), name, nil
}

// SpiceDB starts a container of SpiceDBImage for the test and gives its gRPC endpoint, host:port on 127.0.0.1. args
// are the arguments of the spicedb program: serve-testing, or serve with a preshared key and the memory datastore. The
// cleanup of the test removes the container. SpiceDB fails the test with the reason when the container cannot start,
// also when neither podman nor docker is on PATH: a missing result is not a pass.
func SpiceDB(t testing.TB, args ...string) string {
	t.Helper()
	endpoint, name, err := startSpiceDB(args)
	if name != "" {
		t.Cleanup(func() { removeContainer(name) })
	}
	if err != nil {
		t.Fatal("localtest: " + err.Error())
	}
	return endpoint
}

// startSpiceDB starts the SpiceDB container with podman, else with docker, and gives its endpoint and its name. On an
// error, no container of it runs.
func startSpiceDB(args []string) (string, string, error) {
	found := containerTools()
	if len(found) == 0 {
		return "", "", errors.New("neither podman nor docker is on PATH, so the SpiceDB container of the test cannot start; install one")
	}
	var errs []string
	for _, path := range found {
		endpoint, name, err := runSpiceDB(path, args)
		if err == nil {
			return endpoint, name, nil
		}
		removeContainer(name)
		errs = append(errs, err.Error())
	}
	return "", "", errors.New("start the SpiceDB container: " + strings.Join(errs, "; then: "))
}

// spiceDBArguments gives the arguments of the run of the SpiceDB container name with the tool path, with the time
// limit life, for the arguments args of the spicedb program.
func spiceDBArguments(path, name string, life time.Duration, args []string) []string {
	run := []string{"run", "--detach", "--rm", "--name", name,
		"--user", "65532:65532", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--memory", "256m", "--cpus", "1", "--pids-limit", "512", "--stop-timeout", "1",
		"--publish", "127.0.0.1::50051"}
	// The image has no timeout program; podman ends the container itself at the time limit. For docker, the reaper
	// holds the time limit.
	if filepath.Base(path) == "podman" {
		run = append(run, "--timeout", fmt.Sprintf("%d", int(life.Seconds())))
	}
	return append(append(run, SpiceDBImage), args...)
}

// runSpiceDB starts the SpiceDB container with the tool, and gives the endpoint that it publishes and its name.
func runSpiceDB(path string, args []string) (string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), startLimit)
	defer cancel()
	life := lifeLimit()
	name := fmt.Sprintf("stego-localtest-spicedb-%d-%s", os.Getpid(), suffix())
	if err := runContainer(ctx, path, name, life, nil, spiceDBArguments(path, name, life, args)); err != nil {
		return "", name, err
	}
	for {
		out, err := command(ctx, path, "port", name, "50051/tcp")
		if line := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0]); err == nil && line != "" {
			if _, _, err := net.SplitHostPort(line); err != nil {
				return "", name, fmt.Errorf("read the published port from %q: %w", out, err)
			}
			return line, name, nil
		}
		select {
		case <-ctx.Done():
			logs, _ := command(context.Background(), path, "logs", "--tail", "40", name)
			return "", name, fmt.Errorf("the SpiceDB container publishes no port after %v; its last log lines:\n%s", startLimit, logs)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// containerTools gives the paths of podman and docker on PATH, podman first.
func containerTools() []string {
	var found []string
	for _, name := range []string{"podman", "docker"} {
		if p, err := exec.LookPath(name); err == nil {
			found = append(found, p)
		}
	}
	return found
}

// lifeLimit is the time limit of a container: the -test.timeout of the binary and one minute, else defaultLife.
func lifeLimit() time.Duration {
	if f := flag.Lookup("test.timeout"); f != nil {
		if d, err := time.ParseDuration(f.Value.String()); err == nil && d > 0 {
			return d + time.Minute
		}
	}
	return defaultLife
}

// started is one container that localtest started: its tool, its reaper, and a channel that is closed when the run
// of the tool that creates it has ended.
type started struct {
	tool   string
	reaper *reaper
	ran    chan struct{}
}

// runContainer starts the reaper of the container name, with the time limit life, records the container, and runs
// the tool with args and the environment env (the environment of the test when env is nil).
func runContainer(ctx context.Context, path, name string, life time.Duration, env, args []string) error {
	r, err := newReaper(path, name, life)
	if err != nil {
		return fmt.Errorf("start the reaper of the container: %w", err)
	}
	c := &started{tool: path, reaper: r, ran: make(chan struct{})}
	mu.Lock()
	running[name] = c
	mu.Unlock()
	_, err = commandEnv(ctx, env, path, args...)
	close(c.ran)
	return err
}

// reaperScript removes the container $2 with the tool $1 when the test binary ends, or when the time limit of $3
// seconds ends. It reads one line from its standard input, a pipe whose write end only the test binary holds: the end
// of the binary on any path, also a kill, closes the pipe and ends the read, so no PID is watched. The line done is a
// clean exit, after which localtest removed the container itself. After any other end, it removes the container, and
// again every $4 seconds, $5 more times: a run of the tool that the binary started and that did not end before the
// binary ended can create the container after the first removal. A timer in the background removes the container at
// the time limit, also while the binary runs. At the end, the reaper stops its process group, which holds the timer,
// so it leaves no process.
const reaperScript = `( sleep "$3"; "$1" rm --force "$2" >/dev/null 2>&1 ) &
read -r line
if [ "$line" != done ]; then
	n=0
	while :; do
		"$1" rm --force "$2" >/dev/null 2>&1
		[ "$n" -ge "$5" ] && break
		n=$((n + 1))
		sleep "$4"
	done
fi
trap '' TERM
kill -TERM 0 2>/dev/null
exit 0
`

// reaperEvery and reaperTimes are the removals of the reaper after an end that is not clean: every five seconds for
// the start limit of a container.
var (
	reaperEvery = 5 * time.Second
	reaperTimes = int(startLimit / (5 * time.Second))
)

// reaper is one reaper process: the write end of the pipe that it reads, and its process ID, which is also the ID of
// its process group.
type reaper struct {
	stop *os.File
	pid  int
}

// newReaper starts the reaper of the container name of the tool path, with the time limit life, in a process group
// of its own, so that a kill of the group of the test binary, as an interrupt of the terminal sends, does not take it
// too.
func newReaper(path, name string, life time.Duration) (*reaper, error) {
	read, write, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("sh", "-c", reaperScript, "reaper", path, name, fmt.Sprintf("%d", max(int(life.Seconds()), 1)),
		fmt.Sprintf("%d", max(int(reaperEvery.Seconds()), 1)), fmt.Sprintf("%d", reaperTimes))
	cmd.Stdin = read
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = read.Close()
		_ = write.Close()
		return nil, err
	}
	_ = read.Close()
	go func() { _ = cmd.Wait() }()
	return &reaper{stop: write, pid: cmd.Process.Pid}, nil
}

// remove removes each container that localtest started and did not remove yet. It is safe to call more than once.
func remove() {
	mu.Lock()
	var names []string
	for name := range running {
		names = append(names, name)
	}
	mu.Unlock()
	for _, name := range names {
		removeContainer(name)
	}
}

// removeContainer removes the container name, if localtest started it, and stops its reaper. It first waits, up to
// removeWait, for the end of the run of the tool that creates the container: the SIGINT path of Main can come while a
// test starts it, and a run that ends after the removal creates the container. A removal that fails, or that did not
// wait for the end of the run, leaves the reaper, which removes the container again when the binary ends (security
// review of PR #7, N4). It is safe to call more than once, and with an empty name.
func removeContainer(name string) {
	mu.Lock()
	c := running[name]
	delete(running, name)
	mu.Unlock()
	if c == nil {
		return
	}
	ended := true
	select {
	case <-c.ran:
	case <-time.After(removeWait):
		ended = false
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := command(ctx, c.tool, "rm", "--force", name); err != nil {
		fmt.Fprintf(os.Stderr, "localtest: remove the container %s: %v\n", name, err)
		return
	}
	if !ended {
		fmt.Fprintf(os.Stderr, "localtest: the run of the container %s did not end; its reaper removes it again when the tests end\n", name)
		return
	}
	_, _ = c.reaper.stop.WriteString("done\n")
	_ = c.reaper.stop.Close()
}

// removeWait is the time that removeContainer waits for the end of the run of a container: its start limit.
var removeWait = startLimit

// GoEnv gives the environment of a go command in a generated module, and of a stego command that runs one: the
// environment of the test with no work file, no toolchain switch, GOFLAGS cleared and the go env file not read
// (GOENV=off); the module cache of localtest as GOMODCACHE and its download folder as the only module proxy, no sum
// database, no private, insecure or direct module and no version control, as internal/fill sets them; then the
// entries extra. The first call fills the module cache, once per test binary. It takes no test, so a build in a
// goroutine can call it: a failed fill is written to standard error, and RequireModules fails a test with it.
func GoEnv(extra ...string) []string {
	modulesOnce.Do(downloadModules)
	// When the module cache of localtest could not be made or filled, a go command gets no module at all: an empty
	// module cache of its own and no proxy. It never falls back to the shared GOMODCACHE, which an empty GOMODCACHE
	// would name through GOPATH (security review of PR #510, L4).
	cache, proxy := moduleCache, cacheProxy+",off"
	if modulesWhy != "" || moduleCache == "" {
		cache, proxy = emptyCache(), "off"
	}
	env := []string{}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if innerSettings[key] {
			continue
		}
		env = append(env, entry)
	}
	env = append(env, "GOMODCACHE="+cache, "GOPROXY="+proxy, "GOSUMDB=off", "GONOSUMDB=none", "GONOSUMCHECK=",
		"GOPRIVATE=", "GONOPROXY=none", "GOINSECURE=", "GOVCS=*:off", "GOAUTH=off", "GIT_TERMINAL_PROMPT=0", "GOENV=off",
		"GOFLAGS=", "GOWORK=off", "GOTOOLCHAIN=local")
	return append(env, extra...)
}

// RuntimeWorkspace gives the GOWORK entry of a go command of GoEnv in the module folder that takes the runtime library
// from this checkout's registry/runtime folder: the work file that scripts/runtime-workspace.sh writes for the folder,
// in a temporary folder of the test. Give it after the other entries of GoEnv, whose GOWORK=off it replaces. It
// fails the test unless the work file uses only the module folder and registry/runtime and replaces only the runtime
// module with registry/runtime, so the work file changes no module source but that one (ST-STD-LOCAL-R03).
func RuntimeWorkspace(t testing.TB, module string) string {
	t.Helper()
	root := filepath.Join(filepath.Dir(ModulesFolder()), "..", "..", "..")
	work := filepath.Join(t.TempDir(), "runtime.work")
	if err := runtimeWork(root, module, work); err != nil {
		t.Fatal("localtest: " + err.Error())
	}
	return "GOWORK=" + work
}

// runtimeWork writes the work file with scripts/runtime-workspace.sh of root and checks it with checkRuntimeWork.
func runtimeWork(root, module, work string) error {
	script := exec.Command(filepath.Join(root, "scripts", "runtime-workspace.sh"), module, work)
	if out, err := script.CombinedOutput(); err != nil {
		return fmt.Errorf("runtime-workspace.sh: %v\n%s", err, out)
	}
	if why := checkRuntimeWork(root, module, work); why != "" {
		return errors.New(why)
	}
	return nil
}

// runtimeModule is the module path of the runtime library in registry/runtime.
const runtimeModule = "github.com/910-software-solutions/stego/registry/runtime"

// checkRuntimeWork gives the reason why the work file is not one that uses only the module folder and the
// registry/runtime folder of root and replaces only the runtime module with that folder, or "".
func checkRuntimeWork(root, module, work string) string {
	root, _ = filepath.Abs(root)
	module, _ = filepath.Abs(module)
	real := func(name string) string {
		if !filepath.IsAbs(name) {
			// A relative folder of a work file is relative to the folder of the work file.
			name = filepath.Join(filepath.Dir(work), name)
		}
		if resolved, err := filepath.EvalSymlinks(name); err == nil {
			name = resolved
		}
		absolute, _ := filepath.Abs(name)
		return absolute
	}
	runtimeDir := real(filepath.Join(root, "registry", "runtime"))
	allowed := map[string]bool{real(module): true, runtimeDir: true}
	data, err := os.ReadFile(work)
	if err != nil {
		return err.Error()
	}
	file, err := modfile.ParseWork(work, data, nil)
	if err != nil {
		return err.Error()
	}
	if len(file.Use) == 0 {
		return "the work file " + work + " uses no folder"
	}
	for _, use := range file.Use {
		if !allowed[real(use.Path)] {
			return fmt.Sprintf("the work file %s uses %s, which is not the module folder or registry/runtime", work, use.Path)
		}
	}
	for _, replace := range file.Replace {
		if replace.Old.Path != runtimeModule || real(replace.New.Path) != runtimeDir {
			return fmt.Sprintf("the work file %s replaces %s with %s, not only the runtime module with registry/runtime", work, replace.Old.Path, replace.New.Path)
		}
	}
	return ""
}

// emptyCache gives an empty module cache of this process, made once in the temporary folder, for the go commands of
// GoEnv when the module cache of localtest is not there. Main removes it when the tests end.
func emptyCache() string {
	emptyOnce.Do(func() {
		dir, err := os.MkdirTemp("", "localtest-empty-modules-")
		if err != nil {
			// A path that cannot exist: the go command then finds no module in it either.
			dir = filepath.Join(string(filepath.Separator), "nonexistent", "localtest-empty-modules")
		}
		emptyDir = dir
	})
	return emptyDir
}

var (
	emptyOnce sync.Once
	emptyDir  string
)

// innerSettings are the go settings that GoEnv sets, which it takes from no other source.
var innerSettings = map[string]bool{"GOMODCACHE": true, "GOPROXY": true, "GOSUMDB": true, "GONOSUMDB": true, "GONOSUMCHECK": true,
	"GOPRIVATE": true, "GONOPROXY": true, "GOINSECURE": true, "GOVCS": true, "GOAUTH": true, "GIT_TERMINAL_PROMPT": true,
	"GOENV": true, "GOFLAGS": true, "GOWORK": true, "GOTOOLCHAIN": true}

// CheckSums fails the test when the go.sum of the generated module in dir holds a line that is not checked: no go.sum
// of testdata/modules, testdata/modules-other, the root module or registry/runtime holds it, and the fill of the
// module cache of localtest did not check it against go.sum or sum.golang.org (checkedSumsFile). It is the second check of the module cache, after a go mod tidy in
// dir.
func CheckSums(t testing.TB, dir string) {
	t.Helper()
	if err := SumsChecked(dir); err != nil {
		t.Fatal("localtest: " + err.Error())
	}
}

// SumsChecked is CheckSums as an error, for a step that runs in a goroutine of its own.
func SumsChecked(dir string) error {
	known := map[string]bool{}
	root := filepath.Join(filepath.Dir(ModulesFolder()), "..", "..", "..")
	for _, name := range []string{filepath.Join(ModulesFolder(), "go.sum"), filepath.Join(filepath.Dir(ModulesFolder()), "modules-other", "go.sum"),
		filepath.Join(root, "go.sum"), filepath.Join(root, "registry", "runtime", "go.sum")} {
		data, err := os.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				known[line] = true
			}
		}
	}
	// The runtime pins of the shipped registry: stego deps stages the pin from registry/runtime, whose zip hash the
	// archetype states and whose go.mod hash is that of registry/runtime/go.mod, and a test serves the same folder.
	pins, err := runtimePins(root)
	if err != nil {
		return err
	}
	for _, line := range pins {
		known[line] = true
	}
	if moduleCache != "" {
		data, err := os.ReadFile(filepath.Join(moduleCache, checkedSumsFile))
		if err != nil {
			return fmt.Errorf("read the checked lines of the module cache of localtest: %w", err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				known[line] = true
			}
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "go.sum"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the go.sum of %s: %w", dir, err)
	}
	var unknown []string
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" && !known[line] {
			unknown = append(unknown, line)
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf("the go.sum of %s holds lines that no checked go.sum of the repository holds, so their modules came from the module cache unchecked:\n%s",
			dir, strings.Join(unknown, "\n"))
	}
	return nil
}

// ModuleCache gives the module cache of localtest, and fills it as the first call of GoEnv does. A test that copies
// modules out of a module cache, such as the local proxy of Building, takes them from this one: each module in it is
// checked.
func ModuleCache(t testing.TB) string {
	t.Helper()
	RequireModules(t)
	return moduleCache
}

// FillEnv gives the environment of a go command that downloads modules into the module cache of localtest with the
// sum database on, as its fill does: sum.golang.org, no private, insecure or unchecked module, the go env file not
// read.
func FillEnv(t testing.TB) []string {
	t.Helper()
	RequireModules(t)
	return fillEnvironment()
}

// RequireModules downloads the modules of testdata/modules, as the first call of GoEnv does, and fails the test with
// the reason when the download failed.
func RequireModules(t testing.TB) {
	t.Helper()
	modulesOnce.Do(downloadModules)
	if modulesWhy != "" {
		t.Fatal("localtest: " + modulesWhy)
	}
}

// ModulesFolder is the folder of the module whose go.sum pins each module that the code STEGO generates can import.
func ModulesFolder() string {
	_, source, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(source), "testdata", "modules")
}

// downloadModules downloads each module that testdata/modules needs, with the go command of the test binary and the
// proxy of the environment, and checks that its go.mod and go.sum are tidy. It sets modulesWhy on a failure.
func downloadModules() {
	goTool := os.Getenv("STEGO_TEST_PINNED_GO")
	if goTool == "" {
		goTool = filepath.Join(runtime.GOROOT(), "bin", "go")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	folder, err := cacheFolder(goTool)
	if err != nil {
		modulesWhy = err.Error()
		fmt.Fprintln(os.Stderr, "localtest: "+modulesWhy)
		return
	}
	moduleCache = folder
	cacheProxy = (&url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Join(moduleCache, "cache", "download"))}).String()
	fillEnv := fillEnvironment()
	// testdata/modules pins the runtime library's modules and each module that a generator requires; testdata/
	// modules-other pins the modules that a test writes into a generated module at another version.
	//
	// A generated module that requires fewer modules selects older versions of some of them, such as an older
	// golang.org/x/text, and the go.mod of such a version can require a version of another module that the build of
	// these folders never reaches, because the go command prunes the module graph. So the cache takes, for each module
	// of the build list of a folder (go list -m all), each version of it in the module graph, and each version of it
	// that the go.mod of such a version requires.
	// dirs are the module folders that the fill extracted, each with the hash that the go command checked for it.
	var dirs []fillDir
	fail := func(folder string, err error, output []byte) {
		modulesWhy = fmt.Sprintf("download the modules of %s: %v\n%s", folder, err, output)
		fmt.Fprintln(os.Stderr, "localtest: "+modulesWhy)
	}
	// The tests build the stego binary of this commit (./cmd/stego) with GoEnv too, so the cache also takes each module
	// that provides a package of that build, in the root module.
	root := filepath.Join(filepath.Dir(ModulesFolder()), "..", "..", "..")
	list := exec.CommandContext(ctx, goTool, "list", "-mod=readonly", "-deps", "-f", "{{with .Module}}{{if not .Main}}{{.Path}}@{{.Version}}{{end}}{{end}}", "./cmd/stego")
	list.Dir, list.Env = root, append(fillEnv, "GOFLAGS=")
	if out, err := list.Output(); err != nil {
		fail(root, err, out)
		return
	} else if versions := strings.Fields(string(out)); len(versions) > 0 {
		download := exec.CommandContext(ctx, goTool, append([]string{"mod", "download", "-json"}, versions...)...)
		download.Dir, download.Env = root, list.Env
		var stderr bytes.Buffer
		download.Stderr = &stderr
		output, err := download.Output()
		if err != nil {
			fail(root, err, append(output, stderr.Bytes()...))
			return
		}
		decoder := json.NewDecoder(bytes.NewReader(output))
		for decoder.More() {
			var module fillDir
			if err := decoder.Decode(&module); err != nil {
				fail(root, err, output)
				return
			}
			dirs = append(dirs, module)
		}
	}
	var checked []string
	defer func() {
		if modulesWhy != "" {
			return
		}
		// The checked lines of this fill, for SumsChecked. The file is written whole each run, so it holds only the
		// lines of the last fill, whose go command checked each one.
		if err := writeAtomic(filepath.Join(moduleCache, checkedSumsFile), []byte(strings.Join(checked, "\n")+"\n")); err != nil {
			fail(moduleCache, err, nil)
		}
	}()
	for _, folder := range []string{ModulesFolder(), filepath.Join(filepath.Dir(ModulesFolder()), "modules-other")} {
		env := append(append([]string(nil), fillEnv...), "GOFLAGS=-mod=mod")
		output := func(args ...string) ([]byte, bool) {
			cmd := exec.CommandContext(ctx, goTool, args...)
			cmd.Dir, cmd.Env = folder, env
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if err != nil {
				fail(folder, err, append(out, stderr.Bytes()...))
				return nil, false
			}
			return out, true
		}
		// The commands after the check of go.mod and go.sum run on a copy of them (-modfile), so that they add no line
		// to the go.sum of testdata, also with an empty module cache.
		scratch, err := os.MkdirTemp("", "localtest-modules-")
		if err != nil {
			fail(folder, err, nil)
			return
		}
		defer os.RemoveAll(scratch)
		for _, name := range []string{"go.mod", "go.sum"} {
			data, err := os.ReadFile(filepath.Join(folder, name))
			if err != nil {
				fail(folder, err, nil)
				return
			}
			if err := os.WriteFile(filepath.Join(scratch, name), data, 0o600); err != nil {
				fail(folder, err, nil)
				return
			}
		}
		if _, ok := output("mod", "tidy", "-diff"); !ok {
			return
		}
		modFile := "-modfile=" + filepath.Join(scratch, "go.mod")
		list, ok := output("list", modFile, "-m", "-f", "{{if not .Replace}}{{.Path}}{{end}}", "all")
		if !ok {
			return
		}
		paths := map[string]bool{}
		for _, path := range strings.Fields(string(list)) {
			paths[path] = true
		}
		graph, ok := output("mod", "graph", modFile)
		if !ok {
			return
		}
		seen := map[string]bool{}
		var next []string
		take := func(version string) {
			if path, _, found := strings.Cut(version, "@"); found && paths[path] && !seen[version] {
				seen[version] = true
				next = append(next, version)
			}
		}
		for _, line := range strings.Split(string(graph), "\n") {
			if fields := strings.Fields(line); len(fields) == 2 {
				take(fields[1])
			}
		}
		// A version of a module that provides a package that go mod tidy loads (go list -deps -test all) gets its zip: a
		// generated module can build it. Every other version gets only its go.mod and info, which a go command needs to
		// load the module graph. Two rounds: the versions of the module graph, and the versions that their go.mod files
		// require; a further round would take the whole history of each module.
		built, ok := output("list", modFile, "-e", "-deps", "-test", "-f", "{{with .Module}}{{.Path}}{{end}}", "all")
		if !ok {
			return
		}
		zip := map[string]bool{}
		for _, path := range strings.Fields(string(built)) {
			zip[path] = true
		}
		for round := 0; round < 2 && len(next) > 0; round++ {
			var zips, mods []string
			for _, version := range next {
				if path, _, _ := strings.Cut(version, "@"); zip[path] {
					zips = append(zips, version)
				} else {
					mods = append(mods, version)
				}
			}
			next = nil
			var found []byte
			for _, part := range [][]string{
				append([]string{"mod", "download", "-json", modFile}, zips...),
				append([]string{"list", "-json", modFile, "-m"}, mods...),
			} {
				if part[0] == "mod" && len(zips) == 0 || part[0] == "list" && len(mods) == 0 {
					continue
				}
				out, ok := output(part...)
				if !ok {
					return
				}
				found = append(found, out...)
			}
			decoder := json.NewDecoder(bytes.NewReader(found))
			for decoder.More() {
				var module struct{ Path, Version, Sum, GoModSum, GoMod, Dir string }
				if err := decoder.Decode(&module); err != nil {
					fail(folder, err, found)
					return
				}
				dirs = append(dirs, fillDir{Path: module.Path, Version: module.Version, Sum: module.Sum, Dir: module.Dir})
				// The go command gives the hashes that it checked against go.sum or sum.golang.org before it wrote the
				// module; they are the checked lines of a version that no go.sum of the repository names.
				if module.Sum != "" {
					checked = append(checked, module.Path+" "+module.Version+" "+module.Sum)
				}
				if module.GoModSum != "" {
					checked = append(checked, module.Path+" "+module.Version+"/go.mod "+module.GoModSum)
				}
				data, err := os.ReadFile(module.GoMod)
				if err != nil {
					fail(folder, err, nil)
					return
				}
				file, err := modfile.ParseLax(module.GoMod, data, nil)
				if err != nil {
					fail(folder, err, data)
					return
				}
				for _, require := range file.Require {
					take(require.Mod.Path + "@" + require.Mod.Version)
				}
			}
		}
	}
	// The go command does not hash an extracted folder again when it finds it in the cache, so a file that a tool
	// or a writable cache (-modcacherw) put into the folder of a checked module would build with no error. Each folder
	// is hashed again, on each fill and on each reuse of the cache, as go mod verify does (security review of PR #7).
	if err := verifyDirs(dirs, filepath.Join(moduleCache, verifiedFile)); err != nil {
		fail(moduleCache, err, nil)
	}
}

// fillDir is a module that the fill downloaded: its path, its version, the hash of its zip that the go command
// checked against go.sum or sum.golang.org, and its extracted folder in the module cache.
type fillDir struct{ Path, Version, Sum, Dir string }

// verifiedDirs is the number of module folders that the last fill checked, and hashedDirs the number of them that it
// hashed again, as their stamp changed.
var verifiedDirs, hashedDirs int

// verifiedFile is the file in the module cache of localtest that holds, for each module folder that a fill hashed with
// the checked hash, a stamp of its files: their names, sizes, modes and modification times.
const verifiedFile = "verified-dirs.json"

// dirStamp gives the stamp of the files of the folder dir: a hash of the name, size, mode and modification time of
// each entry. A file that is added, removed or written changes it.
func dirStamp(dir string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(dir, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, name)
		// The go command extracts only folders and regular files. A link, which Lstat sees and the hash of dirhash
		// follows, would let a stamp stay the same while the file it names changes (security review of PR #7, I2).
		if !entry.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("%s holds %s, which is not a regular file or a folder", dir, rel)
		}
		fmt.Fprintf(h, "%q %d %o %d\n", rel, info.Size(), info.Mode(), info.ModTime().UnixNano())
		return nil
	})
	return hex.EncodeToString(h.Sum(nil)), err
}

// verifyDirs hashes each extracted module folder of dirs, as go mod verify does, and fails with each module whose
// folder does not have the checked hash of its zip: "dir has been modified". A module with no folder or no hash, such
// as a version of which only the go.mod was read, is not hashed. A folder whose stamp (dirStamp) and checked hash are
// those that verifiedFile holds from an earlier hash is not hashed again: its files did not change since then. Only
// a deliberate edit that also sets the size and the modification time of each file back gets past the stamp.
//
// stampsFile is the file of the stamps, verifiedFile in the module cache of localtest.
func verifyDirs(dirs []fillDir, stampsFile string) error {
	seen := map[string]bool{}
	var todo []fillDir
	for _, d := range dirs {
		if d.Dir == "" || d.Sum == "" || seen[d.Dir] {
			continue
		}
		seen[d.Dir] = true
		todo = append(todo, d)
	}
	type record struct{ Sum, Stamp string }
	known := map[string]record{}
	if data, err := os.ReadFile(stampsFile); err == nil {
		_ = json.Unmarshal(data, &known)
	}
	var (
		wg       sync.WaitGroup
		failedMu sync.Mutex
		failed   []string
		hashed   int
		passed   = map[string]record{}
	)
	work := make(chan fillDir)
	for range max(runtime.NumCPU(), 1) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for d := range work {
				stamp, err := dirStamp(d.Dir)
				if err != nil {
					// A folder that cannot be stamped, such as one that holds a link, is refused, not hashed.
					failedMu.Lock()
					failed = append(failed, fmt.Sprintf("%s %s: %v", d.Path, d.Version, err))
					failedMu.Unlock()
					continue
				}
				why := ""
				if known[d.Dir] == (record{d.Sum, stamp}) {
					failedMu.Lock()
					passed[d.Dir] = record{d.Sum, stamp}
					failedMu.Unlock()
					continue
				}
				got, err := dirhash.HashDir(d.Dir, d.Path+"@"+d.Version, dirhash.Hash1)
				switch {
				case err != nil:
					why = fmt.Sprintf("%s %s: hash its folder %s: %v", d.Path, d.Version, d.Dir, err)
				case got != d.Sum:
					why = fmt.Sprintf("%s %s: dir has been modified (%s): its hash is %s, not the checked %s", d.Path, d.Version, d.Dir, got, d.Sum)
				}
				failedMu.Lock()
				hashed++
				if why != "" {
					failed = append(failed, why)
				} else if stamp != "" {
					passed[d.Dir] = record{d.Sum, stamp}
				}
				failedMu.Unlock()
			}
		}()
	}
	for _, d := range todo {
		work <- d
	}
	close(work)
	wg.Wait()
	verifiedDirs, hashedDirs = len(todo), hashed
	if data, err := json.Marshal(passed); err == nil {
		_ = writeAtomic(stampsFile, data)
	}
	if len(failed) > 0 {
		slices.Sort(failed)
		return errors.New("the module cache of localtest holds changed module folders:\n" + strings.Join(failed, "\n"))
	}
	return nil
}

// fillEnvironment is the environment of the fill of the module cache of localtest. It reads modules through the
// module proxy of the environment, and each module is checked: against the go.sum of the folder, else against
// sum.golang.org. No setting of the environment or of the go env file can turn a check off: GOENV=off, and each sum
// and private setting is set here.
func fillEnvironment() []string {
	fillEnv := []string{}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if innerSettings[key] {
			continue
		}
		fillEnv = append(fillEnv, entry)
	}
	proxy := os.Getenv("GOPROXY")
	if proxy == "" || proxy == "off" {
		proxy = "https://proxy.golang.org"
	}
	// GOPATH of its own, beside the module cache: the go command writes the state of the sum database into
	// GOPATH/pkg/sumdb, and never into the GOPATH of the developer (security review of PR #8).
	return append(fillEnv, "GOPATH="+filepath.Join(filepath.Dir(moduleCache), "localtest-gopath"), "GOMODCACHE="+moduleCache, "GOPROXY="+proxy, "GOSUMDB=sum.golang.org", "GONOSUMDB=", "GONOSUMCHECK=",
		"GOPRIVATE=", "GONOPROXY=", "GOINSECURE=", "GOVCS=*:off", "GOAUTH=off", "GIT_TERMINAL_PROMPT=0", "GOENV=off", "GOWORK=off",
		"GOTOOLCHAIN=local")
}

// runtimePins gives the go.sum lines of each runtime pin of the archetypes of the shipped registry in root: the zip
// hash that the archetype states, and the hash of the go.mod of registry/runtime at that version.
func runtimePins(root string) ([]string, error) {
	archetypes, err := filepath.Glob(filepath.Join(root, "registry", "archetypes", "*", "archetype.yaml"))
	if err != nil {
		return nil, err
	}
	modData, err := os.ReadFile(filepath.Join(root, "registry", "runtime", "go.mod"))
	if err != nil {
		return nil, err
	}
	modSum, err := dirhash.Hash1([]string{"go.mod"}, func(string) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(modData)), nil })
	if err != nil {
		return nil, err
	}
	pin := regexp.MustCompile(`(?m)^runtime:\s*\n\s+module:\s*(\S+)\s*\n\s+version:\s*(\S+)\s*\n\s+sum:\s*(\S+)`)
	var lines []string
	for _, name := range archetypes {
		data, err := os.ReadFile(name)
		if err != nil {
			return nil, err
		}
		for _, match := range pin.FindAllStringSubmatch(string(data), -1) {
			lines = append(lines, match[1]+" "+match[2]+" "+match[3], match[1]+" "+match[2]+"/go.mod "+modSum)
		}
	}
	return lines, nil
}

// writeAtomic writes data to name through a new file in the same folder and a rename, so a reader never sees a part
// of it.
func writeAtomic(name string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(name), filepath.Base(name)+".*")
	if err != nil {
		return err
	}
	_, err = file.Write(data)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Chmod(file.Name(), 0o644)
	}
	if err == nil {
		err = os.Rename(file.Name(), name)
	}
	if err != nil {
		_ = os.Remove(file.Name())
	}
	return err
}

// cacheFolder gives the module cache of localtest, and makes it: localtest-modules in STEGO_TEST_RUNS when that
// variable names a folder, else in the user cache folder. It refuses a folder in the temporary folder, and the
// GOMODCACHE of the go command, which other tools fill unchecked.
func cacheFolder(goTool string) (string, error) {
	base := os.Getenv("STEGO_TEST_RUNS")
	if base == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("find the user cache folder for the module cache of localtest: %w; set STEGO_TEST_RUNS", err)
		}
		base = cache
	}
	folder, err := filepath.Abs(filepath.Join(base, "localtest-modules"))
	if err != nil {
		return "", err
	}
	tmp, _ := filepath.Abs(os.TempDir())
	if tmp != "" && (folder == tmp || strings.HasPrefix(folder, tmp+string(filepath.Separator))) {
		return "", fmt.Errorf("the module cache of localtest %s is in the temporary folder %s; set STEGO_TEST_RUNS to a persistent folder", folder, tmp)
	}
	if shared, err := exec.Command(goTool, "env", "GOMODCACHE").Output(); err == nil {
		if abs, err := filepath.Abs(strings.TrimSpace(string(shared))); err == nil && (abs == folder || strings.HasPrefix(folder, abs+string(filepath.Separator))) {
			return "", fmt.Errorf("the module cache of localtest %s is in the shared GOMODCACHE %s", folder, abs)
		}
	}
	if _, err := os.Stat(folder); errors.Is(err, fs.ErrNotExist) {
		if err := firstFill(folder, os.Stderr, freeSpace); err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(folder, 0o755); err != nil {
		return "", fmt.Errorf("make the module cache of localtest %s: %w", folder, err)
	}
	return folder, nil
}

// cacheSize is about the size of the module cache of localtest when it is full: the modules of testdata/modules,
// testdata/modules-other and the build of ./cmd/stego.
const cacheSize = 3 << 30

// freeSpace gives the bytes that the file system of the path has free for this user.
var freeSpace = func(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}

// firstFill is the step before the first fill of the module cache of localtest in folder, which does not exist yet:
// it writes one notice line with the folder, its size when full and how to keep it elsewhere or remove it, and
// refuses the fill when the file system of the folder has less than two times that size free (owner disk note,
// 2026-11-27). free gives the free bytes of the nearest folder that exists.
func firstFill(folder string, out io.Writer, free func(string) (uint64, error)) error {
	fmt.Fprintf(out, "localtest: the first fill writes the checked module cache, about %d GB, to %s; set STEGO_TEST_RUNS to a folder to keep it there, or remove the folder to free the space\n", cacheSize>>30, folder)
	existing := filepath.Dir(folder)
	for {
		if _, err := os.Stat(existing); err == nil || filepath.Dir(existing) == existing {
			break
		}
		existing = filepath.Dir(existing)
	}
	available, err := free(existing)
	if err != nil {
		return fmt.Errorf("find the free space for the module cache of localtest at %s: %w", existing, err)
	}
	if available < 2*cacheSize {
		return fmt.Errorf("the module cache of localtest needs about %d GB at %s, and its file system has %d MB free, less than %d GB; free space there, or set STEGO_TEST_RUNS to a folder on a file system with more free space", cacheSize>>30, folder, available>>20, 2*cacheSize>>30)
	}
	return nil
}

// command runs the tool and gives its standard output, or an error with its standard error.
func command(ctx context.Context, path string, args ...string) (string, error) {
	return commandEnv(ctx, nil, path, args...)
}

// commandEnv is command with the environment env, or the environment of the test when env is nil.
func commandEnv(ctx context.Context, env []string, path string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) || stderr.Len() > 0 {
			return "", fmt.Errorf("%s %s: %v: %s", path, args[0], err, strings.TrimSpace(stderr.String()))
		}
		return "", fmt.Errorf("%s %s: %w", path, args[0], err)
	}
	return stdout.String(), nil
}

// suffix is 12 random hexadecimal digits.
func suffix() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
