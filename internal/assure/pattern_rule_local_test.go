package assure

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The proofs of the pattern ST-PAT-LOCAL-TEST in one domain: its rules R01 to R06 hold the rules of ST-STD-LOCAL
// for the tests of the domain. Each proof reads what can be read before a run: the test records of the domain in
// the pinned lathe check --json, and the test files that those records name, from the git objects at HEAD. The
// runs themselves, on a workstation and in CI, are the proof run's (ST-ASR-UC01). Each domain's test names each
// subtest with its test record, numbered by the domain's position: ST-ASR 1, ST-BLD 2, ST-DECL 3, ST-DIS 4,
// ST-DLV 5, ST-EXT 6, ST-GEN 7 and ST-SUP 8.

// localTest is one test record of a domain: its ID, what it proves, its fields, and its file when it is automated.
type localTest struct {
	id, parent, file string
	fields           map[string]any
}

// text gives the text of the test record that says what it needs: its given, when, then and notes.
func (test localTest) text() string {
	var parts []string
	for _, field := range []string{"given", "when", "then", "notes"} {
		if value, ok := test.fields[field].(string); ok {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, " ")
}

// external tells if the test record says that it reaches a system outside the product.
func (test localTest) external() bool {
	value, _ := test.fields["external"].(bool)
	return value
}

// localProof holds what the six proofs of one domain read: the domain's automated test records, the test files
// they name, the other test files of the same packages (a test file runs with each file of its package, so a helper
// that starts a service or reads its address is in another file), and the tree at HEAD.
type localProof struct {
	domain string
	tests  []localTest
	files  map[string][]byte
	tree   Tree
	suites []localSuite
}

// packageFiles gives each test file of the tree in the folder of a named test file: the named files and the other
// _test.go files beside them. A Python test file stands alone.
func (p localProof) packageFiles() map[string][]byte {
	folders := map[string]bool{}
	all := map[string][]byte{}
	for file, data := range p.files {
		all[file] = data
		if path.Ext(file) == ".go" {
			folders[path.Dir(file)] = true
		}
	}
	for _, name := range p.tree.paths() {
		if folders[path.Dir(name)] && strings.HasSuffix(name, "_test.go") {
			data, _ := p.tree.File(name)
			all[name] = data
		}
	}
	return all
}

// newLocalProof reads the test records of the domain from the pinned Lathe and their files from the tree at HEAD;
// with no domain, those of the whole product, also of the records that sit under no domain. A record is automated
// when its state is automated, or when its kind has no state (a contract test of the product's own kind) and it
// names a file.
func newLocalProof(t *testing.T, domain string) localProof {
	t.Helper()
	workspace := checkedWorkspace(t)
	testKinds := map[string]bool{}
	for _, kind := range workspace.Kinds {
		if kind.Stage == "test-plan" && kind.Kind != "test_data_set" {
			testKinds[kind.Kind] = true
		}
	}
	_, tree := readHead(t)
	proof := localProof{domain: domain, files: map[string][]byte{}, tree: tree, suites: localSuites(tree)}
	for _, record := range workspace.Records {
		if domain != "" && record.Domain != domain || !testKinds[record.Kind] {
			continue
		}
		file, _ := record.Fields["file"].(string)
		state, stated := record.Fields["state"].(string)
		if file == "" || stated && state != "automated" {
			continue
		}
		proof.tests = append(proof.tests, localTest{id: record.ID, parent: record.Parent, file: file, fields: record.Fields})
		if data, held := tree.File(file); held {
			proof.files[file] = data
		}
	}
	if len(proof.tests) == 0 {
		t.Fatalf("the spec names no automated test of %q", domain)
	}
	return proof
}

// report fails the test with each fault, sorted, or passes when there is none.
func report(t *testing.T, rule string, faults []string) {
	t.Helper()
	slices.Sort(faults)
	faults = slices.Compact(faults)
	for _, fault := range faults {
		t.Errorf("%s: %s", rule, fault)
	}
}

// localTestsFile is the file of these checks.
const localTestsFile = "internal/assure/pattern_rule_local_test.go"

// ownCheck tells if the file is one of Assuring's own files of checks, which write the text that they look for in
// regular expressions (review of PR #504, L3).
func ownCheck(name string) bool {
	return strings.HasPrefix(name, "internal/assure/") && strings.HasSuffix(name, "_test.go")
}

// codeLines gives the lines of a test file that are code: a Go or Python comment line is not, and in Assuring's own
// files of checks neither is a line that compiles a regular expression, where a check writes the text that it looks
// for.
func codeLines(name string, data []byte) []string {
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "*") || ownCheck(name) && strings.Contains(line, "regexp.MustCompile(") {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

// The things a test that runs on a workstation must not need (ST-STD-LOCAL-R01): a privileged container, the
// host's network, process or IPC namespace, an added capability or device, the Docker socket, no seccomp or no
// AppArmor profile, a cluster that a kube configuration names, or a credential of a hosted service read from the
// environment or the home folder. A line that compiles a regular expression is not read (codeLines).
var workstationFaults = []struct {
	pattern *regexp.Regexp
	why     string
}{
	{regexp.MustCompile(`"--privileged(=[^"]*)?"|\bPrivileged:\s*true\b`), "starts a privileged container"},
	{regexp.MustCompile(`"--(network|net|pid|ipc|uts|userns|cgroupns)(=|",\s*")"?host"|\b(NetworkMode|PidMode|IpcMode|UTSMode|UsernsMode):\s*"host"`), "starts a container in a namespace of the host"},
	{regexp.MustCompile(`"--cap-add|\bCapAdd:`), "adds a capability to a container"},
	{regexp.MustCompile(`"--device|\bDevices:`), "gives a container a device of the host"},
	{regexp.MustCompile(`docker\.sock|podman\.sock`), "gives a container the socket of the container engine"},
	{regexp.MustCompile(`(seccomp|apparmor)[=:]unconfined`), "starts a container with no seccomp or AppArmor profile"},
	{regexp.MustCompile(`(Getenv|LookupEnv|environ\.get|environ\[)\(?\s*["'](KUBECONFIG|GH_TOKEN|GITHUB_TOKEN|DOCKER_CONFIG|REGISTRY_AUTH_FILE|AWS_ACCESS_KEY_ID|GOOGLE_APPLICATION_CREDENTIALS)["']`), "reads a cluster configuration or a credential of a hosted service from the environment"},
	{regexp.MustCompile(`(UserHomeDir\(\)|Path\.home\(\)|expanduser\()[^\n]*(\.kube|\.docker|\.config/gh|\.netrc)`), "reads a cluster configuration or a credential from the user's home folder"},
	{regexp.MustCompile(`subprocess\.\w+\(\s*\[\s*["'](\S*/)?(kubectl|oc|helm|argocd|gh|kind)["']`), "runs the client of a cluster or of a hosted service"},
}

// clusterClients are the programs of a cluster or of a hosted service that a test on a workstation must not run.
var clusterClients = map[string]bool{"kubectl": true, "oc": true, "helm": true, "argocd": true, "gh": true, "kind": true}

// commandFaults parses a Go test file and gives each call of exec.Command or exec.CommandContext that runs a client
// of a cluster or of a hosted service: as the program, by any path, or as the first word of a shell's -c script.
func commandFaults(file string, data []byte) []string {
	if path.Ext(file) != ".go" {
		return nil
	}
	parsed, err := parser.ParseFile(token.NewFileSet(), file, data, parser.SkipObjectResolution)
	if err != nil {
		return []string{file + " does not parse: " + err.Error()}
	}
	var faults []string
	literal := func(expr ast.Expr) (string, bool) {
		lit, ok := expr.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return "", false
		}
		text, err := strconv.Unquote(lit.Value)
		return text, err == nil
	}
	ast.Inspect(parsed, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Command" && selector.Sel.Name != "CommandContext" {
			return true
		}
		args := call.Args
		if selector.Sel.Name == "CommandContext" && len(args) > 0 {
			args = args[1:]
		}
		if len(args) == 0 {
			return true
		}
		program, ok := literal(args[0])
		if !ok {
			return true
		}
		if clusterClients[path.Base(program)] {
			faults = append(faults, fmt.Sprintf("%s runs the client %s of a cluster or of a hosted service", file, program))
		}
		if (path.Base(program) == "sh" || path.Base(program) == "bash") && len(args) >= 3 {
			if flag, _ := literal(args[1]); flag == "-c" {
				if script, ok := literal(args[2]); ok {
					if words := strings.Fields(script); len(words) > 0 && clusterClients[path.Base(words[0])] {
						faults = append(faults, fmt.Sprintf("%s runs the client %s of a cluster or of a hosted service through a shell", file, words[0]))
					}
				}
			}
		}
		return true
	})
	return faults
}

// runsOnAWorkstation proves ST-PAT-LOCAL-TEST-R01 in the domain: no test file of the domain's packages needs a
// privileged container, a namespace of the host, a cluster or a credential of a hosted service, and the file of
// each automated test record is in the tree.
func (p localProof) runsOnAWorkstation(t *testing.T) {
	var faults []string
	for _, test := range p.tests {
		if _, held := p.files[test.file]; !held {
			faults = append(faults, fmt.Sprintf("%s names %s, which the tree at HEAD does not hold", test.id, test.file))
		}
	}
	for file, data := range p.packageFiles() {
		for _, line := range codeLines(file, data) {
			for _, fault := range workstationFaults {
				if fault.pattern.MatchString(line) {
					faults = append(faults, fmt.Sprintf("%s %s: %s", file, fault.why, strings.TrimSpace(line)))
				}
			}
		}
		faults = append(faults, commandFaults(file, data)...)
	}
	report(t, "ST-STD-LOCAL-R01", faults)
}

// localSuite is one documented suite command: the folder of its Go module, or the folder of the Python suites.
type localSuite struct {
	folder, command string
}

// localSuites gives the documented suite commands: go test ./... in the folder of each Go module of the tree,
// which README.md documents for the root module and the proof run (ST-ASR-TOOL02) for each module, and
// scripts/run-python-tests.py (ST-ASR-TOOL01) for the Python suites of scripts.
func localSuites(tree Tree) []localSuite {
	var suites []localSuite
	for _, name := range tree.paths() {
		if path.Base(name) == "go.mod" {
			suites = append(suites, localSuite{folder: path.Dir(name), command: "go test ./..."})
		}
	}
	suites = append(suites, localSuite{folder: "scripts", command: "python3 scripts/run-python-tests.py"})
	return suites
}

// suiteOf gives the suite that runs the test file: the Python suite for scripts/test_*.py, and otherwise the Go
// module whose folder holds the file most closely. A test file under a testdata folder, in Go or in JavaScript, is
// copied into a module that a test generates, and runs in the suite of the module that holds that test: the suite
// holds the file only when a Go test file of the same module names its folder above testdata, or the file itself.
func (p localProof) suiteOf(file string) (localSuite, bool) {
	if path.Dir(file) == "scripts" && strings.HasPrefix(path.Base(file), "test_") && path.Ext(file) == ".py" {
		return p.suites[len(p.suites)-1], true
	}
	inTestdata := strings.Contains(file, "/testdata/")
	if !inTestdata && path.Ext(file) != ".go" {
		return localSuite{}, false
	}
	var best localSuite
	found := false
	for _, suite := range p.suites[:len(p.suites)-1] {
		if (suite.folder == "." || strings.HasPrefix(file, suite.folder+"/")) && (!found || best.folder == "." || len(suite.folder) > len(best.folder)) {
			best, found = suite, true
		}
	}
	if found && inTestdata && !p.namedInSuite(best, file) {
		return localSuite{}, false
	}
	return best, found
}

// namedInSuite tells if a Go test file of the suite's module, outside any testdata folder, names the test file: its
// base name or its path under testdata as the end of a string, the testdata folder of its host folder, or, in the
// host folder, the testdata folder itself.
func (p localProof) namedInSuite(suite localSuite, file string) bool {
	host, inside, _ := strings.Cut(file, "/testdata/")
	words := []string{`"` + path.Base(file) + `"`, "/" + path.Base(file) + `"`, path.Base(host) + "/testdata", "testdata/" + inside}
	for _, name := range p.tree.paths() {
		if !strings.HasSuffix(name, "_test.go") || strings.Contains(name, "/testdata/") || suite.folder != "." && !strings.HasPrefix(name, suite.folder+"/") {
			continue
		}
		if other, ok := p.suiteOf(name); !ok || other.folder != suite.folder {
			continue
		}
		data, _ := p.tree.File(name)
		for _, word := range words {
			if strings.Contains(string(data), word) || path.Dir(name) == host && (strings.Contains(string(data), `"testdata"`) || strings.Contains(string(data), `"testdata/`)) {
				return true
			}
		}
	}
	return false
}

// oneCommandRunsEachSuite proves ST-PAT-LOCAL-TEST-R02 in the domain: each test file of the domain belongs to
// exactly one documented suite, and README.md documents the command of the root module's suite.
func (p localProof) oneCommandRunsEachSuite(t *testing.T) {
	var faults []string
	readme, _ := p.tree.File("README.md")
	if !strings.Contains(string(readme), "go test ./...") {
		faults = append(faults, "README.md documents no go test ./... command")
	}
	if _, held := p.tree.File("scripts/run-python-tests.py"); !held {
		faults = append(faults, "the tree holds no scripts/run-python-tests.py, the command of the Python suites")
	}
	for file := range p.files {
		if _, ok := p.suiteOf(file); !ok {
			faults = append(faults, file+" belongs to no documented suite")
		}
	}
	faults = append(faults, p.undocumentedEnvironment(string(readme))...)
	report(t, "ST-STD-LOCAL-R02", faults)
}

// A read of an environment variable of the product that the test needs: a STEGO_ variable compared with the empty
// string, or looked up with no value, which the documented command does not set.
var neededVariable = regexp.MustCompile(`(?:Getenv\("(STEGO_[A-Z0-9_]+)"\)\s*==\s*""|""\s*==\s*os\.Getenv\("(STEGO_[A-Z0-9_]+)"\)|LookupEnv\("(STEGO_[A-Z0-9_]+)"\);\s*!ok)`)

// A failure or a skip of a test, which a missing variable leads to within a few lines.
var stopsTest = regexp.MustCompile(`\bt\.(Fatal|Fatalf|Skip|Skipf|SkipNow)\(`)

// A set of a variable by a test of the tree: Setenv, or an entry of the environment of a command it runs.
var setsVariable = regexp.MustCompile(`Setenv\("([A-Z0-9_]+)"|"([A-Z0-9_]+)="\s*\+|"([A-Z0-9_]+)=[^"]*"`)

// undocumentedEnvironment gives each STEGO_ variable that a test file of the domain's packages needs and README.md
// does not name: the test fails or skips when the variable is empty, and no test of the tree sets it for the test
// (as a parent test sets the path of a binary for the child process it runs). The documented command go test ./...
// runs the test with no such variable, so the test does not run from the documented command alone (review of PR
// #502, L4).
func (p localProof) undocumentedEnvironment(readme string) []string {
	set := map[string]bool{}
	for _, name := range p.tree.paths() {
		if strings.HasSuffix(name, "_test.go") || strings.HasPrefix(name, "registry/runtime/internal/pgtest/") || strings.HasPrefix(name, "registry/runtime/kafkatest/") {
			data, _ := p.tree.File(name)
			for _, match := range setsVariable.FindAllStringSubmatch(string(data), -1) {
				set[match[1]+match[2]+match[3]] = true
			}
		}
	}
	var faults []string
	for file, data := range p.packageFiles() {
		lines := codeLines(file, data)
		for i, line := range lines {
			for _, match := range neededVariable.FindAllStringSubmatch(line, -1) {
				name := match[1] + match[2] + match[3]
				end := min(i+4, len(lines))
				if strings.Contains(readme, name) || set[name] || !stopsTest.MatchString(strings.Join(lines[i:end], "\n")) {
					continue
				}
				faults = append(faults, fmt.Sprintf("%s needs %s, which README.md does not name", file, name))
			}
		}
	}
	return faults
}

// The signs in the code of a test file that it reaches a system outside the product (ST-STD-LOCAL-R03): a request
// to a host that is not loopback, a module proxy that is not off, a fetch of modules that falls back to the proxy of
// the environment, and an address of a hosted service read from the environment.
var outsideFaults = []struct {
	pattern *regexp.Regexp
	why     string
}{
	{regexp.MustCompile(`http\.(Get|Head|Post|NewRequest(WithContext)?)\([^\n]*"https?://(proxy\.golang\.org|sum\.golang\.org|github\.com|api\.github\.com|quay\.io|ghcr\.io|registry\.npmjs\.org|pypi\.org)|urlopen\(\s*["']https?://(?:proxy\.golang\.org|github\.com|api\.github\.com|quay\.io|ghcr\.io|pypi\.org)`), "sends a request to a hosted service"},
	{regexp.MustCompile(`"GOPROXY=(https?:|direct)`), "fetches modules from a hosted proxy"},
	{regexp.MustCompile(`"mod",\s*"(download|tidy)"|"go",\s*"get"|"-mod=mod"|GOFLAGS=-mod=mod`), "fetches or resolves modules, which reaches the proxy of the environment unless the test sets GOPROXY off"},
	{regexp.MustCompile(`(Getenv|LookupEnv)\("(GITHUB_API_URL|ACTIONS_ID_TOKEN_REQUEST_URL|SIGSTORE_[A-Z_]*|COSIGN_[A-Z_]*)"\)`), "reads the address of a hosted service from the environment"},
}

// localModuleProxy is the text that shows that a test file runs its module fetches with no hosted proxy: GOPROXY off,
// or a file proxy that the test writes.
var localModuleProxy = regexp.MustCompile(`"GOPROXY=off"|"GOPROXY",\s*"off"|GOPROXY=file://|"file://"\s*\+|runtimeProxy\(`)

// reachesOutside gives each sign that the test file reaches a system outside the product, with no local module proxy
// in the file for a module fetch.
func reachesOutside(file string, data []byte) []string {
	lines := codeLines(file, data)
	local := localModuleProxy.MatchString(strings.Join(lines, "\n"))
	var signs []string
	for _, line := range lines {
		for i, fault := range outsideFaults {
			if i == 2 && local {
				continue
			}
			if fault.pattern.MatchString(line) {
				signs = append(signs, fmt.Sprintf("%s %s: %s", file, fault.why, strings.TrimSpace(line)))
			}
		}
	}
	return signs
}

// hasLocalStandIn proves ST-PAT-LOCAL-TEST-R03 in the domain: each automated test whose file reaches a system outside
// the product is marked external, and each obligation of the domain that an external test proves also has an
// automated test that is not external, which runs against a local stand-in.
func (p localProof) hasLocalStandIn(t *testing.T) {
	local := map[string]bool{}
	external := map[string][]string{}
	var faults []string
	for _, test := range p.tests {
		if test.external() {
			external[test.parent] = append(external[test.parent], test.id)
			continue
		}
		local[test.parent] = true
		if data, held := p.files[test.file]; held {
			for _, sign := range reachesOutside(test.file, data) {
				faults = append(faults, fmt.Sprintf("%s is not external, but %s", test.id, sign))
			}
		}
	}
	for parent, tests := range external {
		if !local[parent] {
			faults = append(faults, fmt.Sprintf("%s is proved only by tests that reach an outside system: %s", parent, strings.Join(tests, ", ")))
		}
	}
	report(t, "ST-STD-LOCAL-R03", faults)
}

// An image that a test names: a reference with a tag or a digest, with or without a registry host and a repository
// path, such as postgres:17, authzed/spicedb:v1.57.0 or docker.io/library/postgres@sha256:<digest>.
var imageReference = regexp.MustCompile(`"((?:[a-z0-9.-]+\.[a-z]{2,}(?::[0-9]+)?/)?[a-z0-9][a-z0-9._-]*(?:/[a-z0-9][a-z0-9._-]*)*(?::[A-Za-z0-9._-]+)?(?:@sha256:[0-9a-f]*)?)"`)

// A container that a test starts: a docker or podman run, or a start through the harness of the runtime module.
var containerRun = regexp.MustCompile(`"(docker|podman)",\s*"run"|\[\]string\{"run",|args := \[\]string\{"run"|ContainerCreate\(`)

// pinnedImage tells if an image reference is pinned by a whole sha256 digest.
var pinnedImage = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)

// A service address that a test reads from the environment: a variable whose name ends in _DSN, _ENDPOINT, _ADDRESS
// or _ADDR.
var serviceVariable = regexp.MustCompile(`(?:Getenv|LookupEnv)\("([A-Z0-9_]*(?:_DSN|_ENDPOINT|_ADDRESS|_ADDR))"\)`)

// startsPinnedServices proves ST-PAT-LOCAL-TEST-R04 in the domain: each test file of the domain's packages that
// starts a container names each image it can start by a whole digest; no test connects to a service at a fixed
// local port; and no test needs a service that runs already: one that reads a service address from the environment
// and fails or skips without it, when no file of its package starts the service and sets the variable.
func (p localProof) startsPinnedServices(t *testing.T) {
	var faults []string
	files := p.packageFiles()
	sets := map[string]map[string]bool{} // folder -> variables that a file of the folder sets
	for file, data := range files {
		text := strings.Join(codeLines(file, data), "\n")
		for _, match := range regexp.MustCompile(`Setenv\("([A-Z0-9_]+)"|"([A-Z0-9_]+)="\s*\+`).FindAllStringSubmatch(text, -1) {
			if sets[path.Dir(file)] == nil {
				sets[path.Dir(file)] = map[string]bool{}
			}
			sets[path.Dir(file)][match[1]+match[2]] = true
		}
	}
	for file, data := range files {
		lines := codeLines(file, data)
		text := strings.Join(lines, "\n")
		if containerRun.MatchString(text) {
			for _, match := range imageReference.FindAllStringSubmatch(text, -1) {
				image := match[1]
				if strings.Contains(image, "example.") || !strings.ContainsAny(image, ":@") {
					continue
				}
				if containerImage(text, image) && !pinnedImage.MatchString(image) {
					faults = append(faults, fmt.Sprintf("%s starts the image %s, which no whole sha256 digest pins", file, image))
				}
			}
		}
		for i, line := range lines {
			if fixedService.MatchString(line) {
				faults = append(faults, fmt.Sprintf("%s expects a service that runs already at a fixed address: %s", file, strings.TrimSpace(line)))
			}
			for _, match := range serviceVariable.FindAllStringSubmatch(line, -1) {
				end := min(i+4, len(lines))
				if !regexp.MustCompile(`\bt\.(Fatal|Fatalf|Skip|Skipf|SkipNow)\(`).MatchString(strings.Join(lines[i:end], "\n")) || sets[path.Dir(file)][match[1]] {
					continue
				}
				faults = append(faults, fmt.Sprintf("%s needs a service that runs already: it reads %s and fails or skips without it, and no file of its package starts the service", file, match[1]))
			}
		}
	}
	report(t, "ST-STD-LOCAL-R04", faults)
}

// containerImage tells if the text uses the image as the image of a container: in a constant or a variable whose
// name holds Image, in an Image field, or next to a run.
func containerImage(text, image string) bool {
	quoted := regexp.QuoteMeta(`"` + image + `"`)
	return regexp.MustCompile(`(?i)image\w*\s*(:?=|,|:)\s*` + quoted + `|"run"[^\n]*` + quoted).MatchString(text)
}

// A service address that a test expects to find running: a database, a broker or a server on a fixed local port
// that the test does not start, with no environment variable that names it in its place.
var fixedService = regexp.MustCompile(`(Open|Connect|Dial|DialContext|sql\.Open|pgx\.Connect|NewClient)\([^\n]*"(postgres(ql)?|kafka|redis|grpc|http)://[^"]*(localhost|127\.0\.0\.1):(5432|9092|6379|50051|8080)\b`)

// leavesNoState proves ST-PAT-LOCAL-TEST-R05 in the domain: no test of the domain writes into the user's home folder
// or a fixed folder of the host, and each container that a test starts is removed when it ends (--rm, a cleanup
// that removes it, or the reaper of pgtest).
func (p localProof) leavesNoState(t *testing.T) {
	var faults []string
	for file, data := range p.packageFiles() {
		lines := codeLines(file, data)
		text := strings.Join(lines, "\n")
		for _, line := range lines {
			if hostWrite.MatchString(line) {
				faults = append(faults, fmt.Sprintf("%s writes outside a temporary folder: %s", file, strings.TrimSpace(line)))
			}
		}
		if regexp.MustCompile(`"(docker|podman)",\s*"run"|dockerCommand\([^\n]*"run"|args := \[\]string\{"run"`).MatchString(text) &&
			!regexp.MustCompile(`"--rm"|"rm",\s*"(-f|--force)"|"rm", "--force"|Reap\(`).MatchString(text) {
			faults = append(faults, file+" starts a container that it does not remove when the test ends")
		}
	}
	report(t, "ST-STD-LOCAL-R05", faults)
}

// A write outside a temporary folder: a create, a write or a mkdir of a path in the user's home folder (as the
// process finds it, not a folder that the test names home) or under a fixed folder of the host.
var hostWrite = regexp.MustCompile(`(os\.(WriteFile|Create|Mkdir|MkdirAll|OpenFile)|open)\(\s*(filepath\.Join\(\s*)?(os\.UserHomeDir\(\)|os\.Getenv\("HOME"\)|"/(tmp|var|etc|usr|opt|home|root)/|Path\.home\(\)|os\.path\.expanduser\()`)

// A test that needs a shared cluster: its record names the OpenShift jshell cluster or a shared cluster, in a sentence
// that is not one of the explicit forms of a second proof: "MAY follow", "a second, platform-level proof" or "as a
// second proof".
var sharedCluster = regexp.MustCompile(`(?i)[^.]*\b(jshell|shared cluster|OpenShift cluster)\b[^.]*`)

// needsCluster tells if the test record says that it needs a shared cluster. A unit test of this file reads records
// that name a cluster; it needs none itself (review of PR #504, L2).
func (test localTest) needsCluster() bool {
	if test.file == localTestsFile && test.fields["level"] == "unit" {
		return false
	}
	for _, sentence := range sharedCluster.FindAllString(test.text(), -1) {
		if !regexp.MustCompile(`(?i)\bMAY follow\b|\bsecond(,| and)? platform-level proof\b|\bas a second proof\b`).MatchString(sentence) {
			return true
		}
	}
	return false
}

// neverOnlyClusterProof proves ST-PAT-LOCAL-TEST-R06 in the domain: each obligation of the domain whose automated
// tests need a shared cluster, or are of size volume or worst case at a level that runs the product (service,
// system or browser), also has an automated test that is neither. A test of size worst case at level unit or
// integration runs in process on a workstation, so it is itself a test that runs on a workstation.
func (p localProof) neverOnlyClusterProof(t *testing.T) {
	heavy := map[string][]string{}
	light := map[string]bool{}
	for _, test := range p.tests {
		size, _ := test.fields["size"].(string)
		level, _ := test.fields["level"].(string)
		if size != "" && level != "unit" && level != "integration" || test.needsCluster() {
			heavy[test.parent] = append(heavy[test.parent], test.id)
		} else {
			light[test.parent] = true
		}
	}
	var faults []string
	for parent, tests := range heavy {
		if !light[parent] {
			faults = append(faults, fmt.Sprintf("%s is proved only by tests of volume, worst case or a shared cluster: %s", parent, strings.Join(tests, ", ")))
		}
	}
	report(t, "ST-STD-LOCAL-R06", faults)
}

func TestAssuringFollowsTheLocalTestPattern(t *testing.T) {
	proof := newLocalProof(t, "ST-ASR")
	t.Run("ST-PAT-LOCAL-TEST-R01-T01 Assuring's tests need no privileged container, cluster or hosted credential", proof.runsOnAWorkstation)
	t.Run("ST-PAT-LOCAL-TEST-R02-T01 Each test file of Assuring belongs to one documented suite", proof.oneCommandRunsEachSuite)
	t.Run("ST-PAT-LOCAL-TEST-R03-T01 Each obligation of Assuring that an external test proves has a local test", proof.hasLocalStandIn)
	t.Run("ST-PAT-LOCAL-TEST-R04-T01 Each container that a test of Assuring starts is pinned by digest", proof.startsPinnedServices)
	t.Run("ST-PAT-LOCAL-TEST-R05-T01 Assuring's tests leave no container or file outside a temporary folder", proof.leavesNoState)
	t.Run("ST-PAT-LOCAL-TEST-R06-T01 Each obligation of Assuring with a cluster or volume test has a workstation test", proof.neverOnlyClusterProof)
}

func TestBuildingFollowsTheLocalTestPattern(t *testing.T) {
	proof := newLocalProof(t, "ST-BLD")
	t.Run("ST-PAT-LOCAL-TEST-R01-T02 Building's tests need no privileged container, cluster or hosted credential", proof.runsOnAWorkstation)
	t.Run("ST-PAT-LOCAL-TEST-R05-T02 Building's tests leave no container or file outside a temporary folder", proof.leavesNoState)
	t.Run("ST-PAT-LOCAL-TEST-R06-T02 Each obligation of Building with a cluster or volume test has a workstation test", proof.neverOnlyClusterProof)
}

func TestDeclaringFollowsTheLocalTestPattern(t *testing.T) {
	proof := newLocalProof(t, "ST-DECL")
	t.Run("ST-PAT-LOCAL-TEST-R01-T03 Declaring's tests need no privileged container, cluster or hosted credential", proof.runsOnAWorkstation)
	t.Run("ST-PAT-LOCAL-TEST-R05-T03 Declaring's tests leave no container or file outside a temporary folder", proof.leavesNoState)
	t.Run("ST-PAT-LOCAL-TEST-R06-T03 Each obligation of Declaring with a cluster or volume test has a workstation test", proof.neverOnlyClusterProof)
}

func TestDistributingFollowsTheLocalTestPattern(t *testing.T) {
	proof := newLocalProof(t, "ST-DIS")
	t.Run("ST-PAT-LOCAL-TEST-R01-T04 Distributing's tests need no privileged container, cluster or hosted credential", proof.runsOnAWorkstation)
	t.Run("ST-PAT-LOCAL-TEST-R02-T04 Each test file of Distributing belongs to one documented suite", proof.oneCommandRunsEachSuite)
	t.Run("ST-PAT-LOCAL-TEST-R03-T04 Each obligation of Distributing that an external test proves has a local test", proof.hasLocalStandIn)
	t.Run("ST-PAT-LOCAL-TEST-R05-T04 Distributing's tests leave no container or file outside a temporary folder", proof.leavesNoState)
	t.Run("ST-PAT-LOCAL-TEST-R06-T04 Each obligation of Distributing with a cluster or volume test has a workstation test", proof.neverOnlyClusterProof)
}

func TestDeliveringFollowsTheLocalTestPattern(t *testing.T) {
	proof := newLocalProof(t, "ST-DLV")
	t.Run("ST-PAT-LOCAL-TEST-R01-T05 Delivering's tests need no privileged container, cluster or hosted credential", proof.runsOnAWorkstation)
	t.Run("ST-PAT-LOCAL-TEST-R03-T05 Each obligation of Delivering that an external test proves has a local test", proof.hasLocalStandIn)
	t.Run("ST-PAT-LOCAL-TEST-R05-T05 Delivering's tests leave no container or file outside a temporary folder", proof.leavesNoState)
	t.Run("ST-PAT-LOCAL-TEST-R06-T05 Each obligation of Delivering with a cluster or volume test has a workstation test", proof.neverOnlyClusterProof)
}

func TestExtendingFollowsTheLocalTestPattern(t *testing.T) {
	proof := newLocalProof(t, "ST-EXT")
	t.Run("ST-PAT-LOCAL-TEST-R01-T06 Extending's tests need no privileged container, cluster or hosted credential", proof.runsOnAWorkstation)
	t.Run("ST-PAT-LOCAL-TEST-R05-T06 Extending's tests leave no container or file outside a temporary folder", proof.leavesNoState)
	t.Run("ST-PAT-LOCAL-TEST-R06-T06 Each obligation of Extending with a cluster or volume test has a workstation test", proof.neverOnlyClusterProof)
}

func TestGeneratingFollowsTheLocalTestPattern(t *testing.T) {
	proof := newLocalProof(t, "ST-GEN")
	t.Run("ST-PAT-LOCAL-TEST-R01-T07 Generating's tests need no privileged container, cluster or hosted credential", proof.runsOnAWorkstation)
	t.Run("ST-PAT-LOCAL-TEST-R05-T07 Generating's tests leave no container or file outside a temporary folder", proof.leavesNoState)
	t.Run("ST-PAT-LOCAL-TEST-R06-T07 Each obligation of Generating with a cluster or volume test has a workstation test", proof.neverOnlyClusterProof)
}

func TestSupplyingFollowsTheLocalTestPattern(t *testing.T) {
	proof := newLocalProof(t, "ST-SUP")
	t.Run("ST-PAT-LOCAL-TEST-R01-T08 Supplying's tests need no privileged container, cluster or hosted credential", proof.runsOnAWorkstation)
	t.Run("ST-PAT-LOCAL-TEST-R05-T08 Supplying's tests leave no container or file outside a temporary folder", proof.leavesNoState)
	t.Run("ST-PAT-LOCAL-TEST-R06-T08 Each obligation of Supplying with a cluster or volume test has a workstation test", proof.neverOnlyClusterProof)
}

// TestLocalDevelopmentStandard runs the checks of the pattern ST-PAT-LOCAL-TEST on every automated test of the
// product at once, so a test that no domain owns, such as a test of a shared generator library, is also held.
func TestLocalDevelopmentStandard(t *testing.T) {
	proof := newLocalProof(t, "")
	t.Run("ST-STD-LOCAL-R01-T02 No automated test of the product needs a privileged container, a cluster or a hosted credential", proof.runsOnAWorkstation)
	t.Run("ST-STD-LOCAL-R05-T03 No test of the product leaves a container or a file outside a temporary folder", proof.leavesNoState)
	t.Run("ST-STD-LOCAL-R06-T02 Each obligation with a cluster or volume test also has a test that runs on a workstation", proof.neverOnlyClusterProof)
}
