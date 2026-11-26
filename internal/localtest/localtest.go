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
// Modules (ST-STD-LOCAL-R03): the first call of GoEnv downloads, once per test binary, each module that testdata/
// modules pins in its go.sum: the runtime library's modules and each module that a generator of STEGO requires. Its
// go command uses the proxy of the environment, as the go command of the test binary itself does. Each go command of a
// generated module then has as its only module proxy the download folder of the module cache, as a file URL, and a sum
// database at a closed local port, so a go mod tidy or a -mod=mod reaches no host: the proxy answers a query for the
// latest version of a module with the versions that the cache holds, and the checksums come from go.sum and from the
// lookups that the cache keeps. A module or a checksum that the cache does not hold fails the command, which names it.
package localtest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// PostgresImage is the pinned PostgreSQL server image: postgres:18.6-alpine, by the digest of the image that the CI job
// services of STEGO run.
const PostgresImage = "docker.io/library/postgres@sha256:d3e1620b530c944afa6e887d22eb899824da68e19c52024bf98f5220c88a65b2"

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

	mu        sync.Mutex
	tool      string
	container string
	stop      *reaper

	modulesOnce sync.Once
	modulesWhy  string
	// cacheProxy is the download folder of the module cache, as a file URL.
	cacheProxy string
)

// Main runs the tests of m, removes the PostgreSQL container that a test started, if any, and gives the exit code
// for os.Exit. It also removes it when the process gets SIGINT. It does not watch SIGTERM, which tests of a shutdown
// send to their own process; the reaper and the time limit of the container cover a process that is killed.
//
//	func TestMain(m *testing.M) { os.Exit(localtest.Main(m)) }
func Main(m *testing.M) int {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)
	done := make(chan struct{})
	go func() {
		select {
		case s := <-signals:
			remove()
			fmt.Fprintf(os.Stderr, "localtest: %v: the PostgreSQL container is removed\n", s)
			os.Exit(2)
		case <-done:
		}
	}()
	code := m.Run()
	close(done)
	signal.Stop(signals)
	remove()
	return code
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
	var found []string
	for _, name := range []string{"podman", "docker"} {
		if p, err := exec.LookPath(name); err == nil {
			found = append(found, p)
		}
	}
	if len(found) == 0 {
		failWhy = "neither podman nor docker is on PATH, so the PostgreSQL container of the test cannot start; install one, or set " +
			PostgresVariable + " to a local server"
		return
	}
	// podman comes first. When it is there but cannot start the container, as a rootless podman without the cgroup
	// controllers for the limits cannot, docker is tried next.
	var errs []string
	for _, path := range found {
		got, err := runPostgres(path)
		if err == nil {
			dsn = got
			return
		}
		remove()
		errs = append(errs, err.Error())
	}
	failWhy = "start the PostgreSQL container: " + strings.Join(errs, "; then: ")
}

// runPostgres starts the container with the tool, waits until the server accepts TCP connections inside it, and
// gives the superuser URL on the published port.
func runPostgres(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), startLimit)
	defer cancel()
	password := suffix() + suffix()
	life := defaultLife
	if f := flag.Lookup("test.timeout"); f != nil {
		if d, err := time.ParseDuration(f.Value.String()); err == nil && d > 0 {
			life = d + time.Minute
		}
	}
	name := fmt.Sprintf("stego-localtest-pg-%d-%s", os.Getpid(), suffix())
	// The image's user postgres (uid 70) owns its data on the tmpfs; the tmpfs folders are open to it with the
	// sticky bit, as the image's own folders are.
	args := []string{"run", "--detach", "--rm", "--name", name,
		"--user", "70:70", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--memory", "2g", "--cpus", "2", "--pids-limit", "1024", "--shm-size", "256m",
		"--tmpfs", "/var/lib/postgresql:rw,size=1536m,mode=1777",
		"--tmpfs", "/var/run/postgresql:rw,size=8m,mode=1777",
		"--publish", "127.0.0.1::5432",
		"--env", "POSTGRES_PASSWORD=" + password,
		"--env", "POSTGRES_INITDB_ARGS=--auth-host=scram-sha-256 --auth-local=trust",
		"--entrypoint", "timeout",
		PostgresImage,
		fmt.Sprintf("%ds", int(life.Seconds())), "docker-entrypoint.sh", "postgres",
		"-c", "fsync=off", "-c", "synchronous_commit=off", "-c", "full_page_writes=off", "-c", "max_connections=300"}
	r, err := newReaper(path, name)
	if err != nil {
		return "", fmt.Errorf("start the reaper of the container: %w", err)
	}
	mu.Lock()
	tool, container, stop = path, name, r
	mu.Unlock()
	if _, err := command(ctx, path, args...); err != nil {
		return "", err
	}
	out, err := command(ctx, path, "port", name, "5432/tcp")
	if err != nil {
		return "", err
	}
	host, port, err := net.SplitHostPort(strings.TrimSpace(strings.SplitN(out, "\n", 2)[0]))
	if err != nil {
		return "", fmt.Errorf("read the published port from %q: %w", out, err)
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
			return "", fmt.Errorf("the server is not ready after %v; its last log lines:\n%s", startLimit, logs)
		case <-time.After(200 * time.Millisecond):
		}
	}
	u := url.URL{Scheme: "postgres", User: url.UserPassword("postgres", password), Host: net.JoinHostPort(host, port),
		Path: "/postgres", RawQuery: "sslmode=disable"}
	return u.String(), nil
}

// reaperScript removes the container $2 with the tool $1 when the test binary ends. It reads one line from its
// standard input, a pipe whose write end only the test binary holds: the end of the binary on any path, also a kill,
// closes the pipe and ends the read, so no PID is watched. The line done is a clean exit, after which Main removed
// the container itself: the reaper then ends at once.
const reaperScript = `read -r line
[ "$line" = done ] && exit 0
"$1" rm --force "$2" >/dev/null 2>&1
`

// reaper is one reaper process: the write end of the pipe that it reads.
type reaper struct {
	stop *os.File
}

// newReaper starts the reaper of the container name of the tool path, in a process group of its own, so that a kill
// of the group of the test binary, as an interrupt of the terminal sends, does not take it too.
func newReaper(path, name string) (*reaper, error) {
	read, write, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("sh", "-c", reaperScript, "reaper", path, name)
	cmd.Stdin = read
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = read.Close()
		_ = write.Close()
		return nil, err
	}
	_ = read.Close()
	go func() { _ = cmd.Wait() }()
	return &reaper{stop: write}, nil
}

// remove removes the container, if one started, and stops its reaper. A removal that fails leaves the reaper, which
// removes the container when the binary ends. It is safe to call more than once.
func remove() {
	mu.Lock()
	path, name, r := tool, container, stop
	container, stop = "", nil
	mu.Unlock()
	if name == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := command(ctx, path, "rm", "--force", name); err != nil {
		fmt.Fprintf(os.Stderr, "localtest: remove the PostgreSQL container %s: %v\n", name, err)
		return
	}
	if r != nil {
		_, _ = r.stop.WriteString("done\n")
		_ = r.stop.Close()
	}
}

// GoEnv gives the environment of a go command in a generated module, and of a stego command that runs one: the
// environment of the test with no work file, no toolchain switch and GOFLAGS cleared, the download folder of the
// module cache as the only module proxy (GOPROXY=file://...,off) and a sum database at a closed local port, then the
// entries extra. The first call downloads the modules of testdata/modules into the module cache, once per test binary.
// It takes no test, so a build in a goroutine can call it: a failed download is written to standard error, and
// RequireModules fails a test with it.
func GoEnv(extra ...string) []string {
	modulesOnce.Do(downloadModules)
	env := []string{}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "GOPROXY", "GOSUMDB", "GONOSUMDB", "GONOSUMCHECK", "GOPRIVATE", "GONOPROXY", "GOFLAGS", "GOWORK", "GOTOOLCHAIN":
			continue
		}
		env = append(env, entry)
	}
	env = append(env, "GOPROXY="+cacheProxy+",off", "GOSUMDB=sum.golang.org http://127.0.0.1:9", "GOFLAGS=", "GOWORK=off", "GOTOOLCHAIN=local")
	return append(env, extra...)
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
	cache, err := exec.CommandContext(ctx, goTool, "env", "GOMODCACHE").Output()
	if err != nil || strings.TrimSpace(string(cache)) == "" {
		modulesWhy = fmt.Sprintf("read GOMODCACHE with %s: %v", goTool, err)
		fmt.Fprintln(os.Stderr, "localtest: "+modulesWhy)
		return
	}
	cacheProxy = (&url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Join(strings.TrimSpace(string(cache)), "cache", "download"))}).String()
	cmd := exec.CommandContext(ctx, goTool, "mod", "tidy", "-diff")
	cmd.Dir = ModulesFolder()
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local", "GOFLAGS=-mod=mod")
	if output, err := cmd.CombinedOutput(); err != nil {
		modulesWhy = fmt.Sprintf("download the modules of %s: %v\n%s", cmd.Dir, err, output)
		fmt.Fprintln(os.Stderr, "localtest: "+modulesWhy)
	}
}

// command runs the tool and gives its standard output, or an error with its standard error.
func command(ctx context.Context, path string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, path, args...)
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
