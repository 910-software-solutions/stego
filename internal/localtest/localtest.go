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
	"encoding/hex"
	"encoding/json"
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

	"golang.org/x/mod/modfile"
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
	// moduleCache is the module cache of localtest, and cacheProxy its download folder, as a file URL.
	moduleCache, cacheProxy string
)

// checkedSumsFile is the file in the module cache of localtest that holds the go.sum lines that its last fill checked.
const checkedSumsFile = "checked-sums.txt"

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
		// The password is in the environment of the tool, not in its arguments, which any local user can read.
		"--env", "POSTGRES_PASSWORD",
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
	if _, err := commandEnv(ctx, append(os.Environ(), "POSTGRES_PASSWORD="+password), path, args...); err != nil {
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
// environment of the test with no work file, no toolchain switch, GOFLAGS cleared and the go env file not read
// (GOENV=off); the module cache of localtest as GOMODCACHE and its download folder as the only module proxy, no sum
// database, no private, insecure or direct module and no version control, as internal/fill sets them; then the
// entries extra. The first call fills the module cache, once per test binary. It takes no test, so a build in a
// goroutine can call it: a failed fill is written to standard error, and RequireModules fails a test with it.
func GoEnv(extra ...string) []string {
	modulesOnce.Do(downloadModules)
	env := []string{}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if innerSettings[key] {
			continue
		}
		env = append(env, entry)
	}
	env = append(env, "GOMODCACHE="+moduleCache, "GOPROXY="+cacheProxy+",off", "GOSUMDB=off", "GONOSUMDB=none", "GONOSUMCHECK=",
		"GOPRIVATE=", "GONOPROXY=none", "GOINSECURE=", "GOVCS=*:off", "GOAUTH=off", "GIT_TERMINAL_PROMPT=0", "GOENV=off",
		"GOFLAGS=", "GOWORK=off", "GOTOOLCHAIN=local")
	return append(env, extra...)
}

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
		download := exec.CommandContext(ctx, goTool, append([]string{"mod", "download"}, versions...)...)
		download.Dir, download.Env = root, list.Env
		if output, err := download.CombinedOutput(); err != nil {
			fail(root, err, output)
			return
		}
	}
	var checked []string
	defer func() {
		if modulesWhy != "" {
			return
		}
		// The checked lines of this fill, for SumsChecked. The file is written whole each run, so it holds only the
		// lines of the last fill, whose go command checked each one.
		if err := os.WriteFile(filepath.Join(moduleCache, checkedSumsFile), []byte(strings.Join(checked, "\n")+"\n"), 0o644); err != nil {
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
				var module struct{ Path, Version, Sum, GoModSum, GoMod string }
				if err := decoder.Decode(&module); err != nil {
					fail(folder, err, found)
					return
				}
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
	return append(fillEnv, "GOMODCACHE="+moduleCache, "GOPROXY="+proxy, "GOSUMDB=sum.golang.org", "GONOSUMDB=", "GONOSUMCHECK=",
		"GOPRIVATE=", "GONOPROXY=", "GOINSECURE=", "GOVCS=*:off", "GOAUTH=off", "GIT_TERMINAL_PROMPT=0", "GOENV=off", "GOWORK=off",
		"GOTOOLCHAIN=local")
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
	if err := os.MkdirAll(folder, 0o755); err != nil {
		return "", fmt.Errorf("make the module cache of localtest %s: %w", folder, err)
	}
	return folder, nil
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
