package assure

import (
	"fmt"
	"path"
	"regexp"
	"slices"
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
// they name, and the tree at HEAD.
type localProof struct {
	domain string
	tests  []localTest
	files  map[string][]byte
	tree   Tree
	suites []localSuite
}

// newLocalProof reads the test records of the domain from the pinned Lathe and their files from the tree at HEAD.
// A record is automated when its state is automated, or when its kind has no state (a contract test of the
// product's own kind) and it names a file.
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
		if record.Domain != domain || !testKinds[record.Kind] {
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
		t.Fatalf("the spec names no automated test of %s", domain)
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

// codeLines gives the lines of a test file that are code: a Go or Python comment line is not.
func codeLines(name string, data []byte) []string {
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "*") {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

// The things a test that runs on a workstation must not need (ST-STD-LOCAL-R01): a privileged container, the
// host's network, process or IPC namespace, an added capability, a cluster that a kube configuration names, or a
// credential of a hosted service read from the environment.
var workstationFaults = []struct {
	pattern *regexp.Regexp
	why     string
}{
	{regexp.MustCompile(`"--privileged(=true)?"`), "starts a privileged container"},
	{regexp.MustCompile(`"--(network|net|pid|ipc|uts|userns)(=|", ")"?host"`), "starts a container in a namespace of the host"},
	{regexp.MustCompile(`"--cap-add`), "adds a capability to a container"},
	{regexp.MustCompile(`"--device`), "gives a container a device of the host"},
	{regexp.MustCompile(`(Getenv|LookupEnv|environ\.get|environ\[)\(?\s*["'](KUBECONFIG|GH_TOKEN|GITHUB_TOKEN|DOCKER_CONFIG|REGISTRY_AUTH_FILE|AWS_ACCESS_KEY_ID|GOOGLE_APPLICATION_CREDENTIALS)["']`), "reads a cluster configuration or a credential of a hosted service from the environment"},
	{regexp.MustCompile(`(UserHomeDir\(\)|Path\.home\(\)|expanduser\()[^\n]*(\.kube|\.docker|\.config/gh|\.netrc)`), "reads a cluster configuration or a credential from the user's home folder"},
	{regexp.MustCompile(`(exec\.Command(Context)?\((ctx, )?|subprocess\.\w+\(\s*\[\s*)["'](kubectl|oc|helm|argocd|gh|kind)["']`), "runs the client of a cluster or of a hosted service"},
}

// runsOnAWorkstation proves ST-PAT-LOCAL-TEST-R01 in the domain: no automated test of the domain needs a privileged
// container, a namespace of the host, a cluster or a credential of a hosted service, and the file of each one is in
// the tree.
func (p localProof) runsOnAWorkstation(t *testing.T) {
	var faults []string
	for _, test := range p.tests {
		if _, held := p.files[test.file]; !held {
			faults = append(faults, fmt.Sprintf("%s names %s, which the tree at HEAD does not hold", test.id, test.file))
		}
	}
	for file, data := range p.files {
		for _, line := range codeLines(file, data) {
			for _, fault := range workstationFaults {
				if fault.pattern.MatchString(line) {
					faults = append(faults, fmt.Sprintf("%s %s: %s", file, fault.why, strings.TrimSpace(line)))
				}
			}
		}
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
	report(t, "ST-STD-LOCAL-R02", faults)
}

// hasLocalStandIn proves ST-PAT-LOCAL-TEST-R03 in the domain: each obligation of the domain that an external test
// proves also has an automated test that is not external, which runs against a local stand-in.
func (p localProof) hasLocalStandIn(t *testing.T) {
	local := map[string]bool{}
	external := map[string][]string{}
	for _, test := range p.tests {
		if test.external() {
			external[test.parent] = append(external[test.parent], test.id)
		} else {
			local[test.parent] = true
		}
	}
	var faults []string
	for parent, tests := range external {
		if !local[parent] {
			faults = append(faults, fmt.Sprintf("%s is proved only by tests that reach an outside system: %s", parent, strings.Join(tests, ", ")))
		}
	}
	report(t, "ST-STD-LOCAL-R03", faults)
}

// An image that a test names: a reference with a registry host or a repository and a tag or a digest.
var imageReference = regexp.MustCompile(`"((?:[a-z0-9.-]+\.[a-z]{2,}(?::[0-9]+)?/)?[a-z0-9][a-z0-9._-]*/[a-z0-9][a-z0-9._/-]*(?::[A-Za-z0-9._-]+)?(?:@sha256:[0-9a-f]*)?)"`)

// A container that a test starts: a docker or podman run, or a start through the harness of the runtime module.
var containerRun = regexp.MustCompile(`"(docker|podman)",\s*"run"|\[\]string\{"run",|args := \[\]string\{"run"`)

// pinnedImage tells if an image reference is pinned by a whole sha256 digest.
var pinnedImage = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)

// startsPinnedServices proves ST-PAT-LOCAL-TEST-R04 in the domain: each test file of the domain that starts a
// container names each image it can start by a whole digest, through a constant of the file or of a harness of the
// tree (pgtest, kafkatest), and no test reads the address of a service that it expects to run already.
func (p localProof) startsPinnedServices(t *testing.T) {
	var faults []string
	for file, data := range p.files {
		text := strings.Join(codeLines(file, data), "\n")
		if containerRun.MatchString(text) {
			for _, match := range imageReference.FindAllStringSubmatch(text, -1) {
				image := match[1]
				if strings.HasPrefix(image, "example.") || strings.Contains(image, "example.com/") || !strings.Contains(image, ":") && !strings.Contains(image, "@") {
					continue
				}
				if containerImage(text, image) && !pinnedImage.MatchString(image) {
					faults = append(faults, fmt.Sprintf("%s starts the image %s, which no whole sha256 digest pins", file, image))
				}
			}
		}
		for _, line := range codeLines(file, data) {
			if fixedService.MatchString(line) {
				faults = append(faults, fmt.Sprintf("%s expects a service that runs already at a fixed address: %s", file, strings.TrimSpace(line)))
			}
		}
	}
	report(t, "ST-STD-LOCAL-R04", faults)
}

// containerImage tells if the text uses the image as the image of a container: in a constant or a variable whose
// name holds Image, or next to a run.
func containerImage(text, image string) bool {
	quoted := regexp.QuoteMeta(`"` + image + `"`)
	return regexp.MustCompile(`(?i)image\w*\s*(:?=|,)\s*` + quoted + `|"run"[^\n]*` + quoted).MatchString(text)
}

// A service address that a test expects to find running: a database, a broker or a server on a fixed local port
// that the test does not start, with no environment variable that names it in its place.
var fixedService = regexp.MustCompile(`(Open|Connect|Dial|DialContext|sql\.Open|pgx\.Connect|NewClient)\([^\n]*"(postgres(ql)?|kafka|redis|grpc|http)://[^"]*(localhost|127\.0\.0\.1):(5432|9092|6379|50051|8080)\b`)

// leavesNoState proves ST-PAT-LOCAL-TEST-R05 in the domain: no test of the domain writes into the user's home folder
// or a fixed folder of the host, and each container that a test starts is removed when it ends (--rm, a cleanup
// that removes it, or the reaper of pgtest).
func (p localProof) leavesNoState(t *testing.T) {
	var faults []string
	for file, data := range p.files {
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

// A test that needs a shared cluster: its record names the OpenShift jshell cluster or a shared cluster, not as a
// second proof that MAY follow.
var sharedCluster = regexp.MustCompile(`(?i)[^.]*\b(jshell|shared cluster|OpenShift cluster)\b[^.]*`)

// needsCluster tells if the test record says that it needs a shared cluster.
func (test localTest) needsCluster() bool {
	for _, sentence := range sharedCluster.FindAllString(test.text(), -1) {
		if !regexp.MustCompile(`(?i)\bMAY follow\b|\bsecond\b|\bnot the only\b|\bnever\b`).MatchString(sentence) {
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
	t.Run("ST-PAT-LOCAL-TEST-R02-T02 Each test file of Building belongs to one documented suite", proof.oneCommandRunsEachSuite)
	t.Run("ST-PAT-LOCAL-TEST-R04-T02 Each container that a test of Building starts is pinned by digest", proof.startsPinnedServices)
	t.Run("ST-PAT-LOCAL-TEST-R05-T02 Building's tests leave no container or file outside a temporary folder", proof.leavesNoState)
	t.Run("ST-PAT-LOCAL-TEST-R06-T02 Each obligation of Building with a cluster or volume test has a workstation test", proof.neverOnlyClusterProof)
}

func TestDeclaringFollowsTheLocalTestPattern(t *testing.T) {
	proof := newLocalProof(t, "ST-DECL")
	t.Run("ST-PAT-LOCAL-TEST-R01-T03 Declaring's tests need no privileged container, cluster or hosted credential", proof.runsOnAWorkstation)
	t.Run("ST-PAT-LOCAL-TEST-R02-T03 Each test file of Declaring belongs to one documented suite", proof.oneCommandRunsEachSuite)
	t.Run("ST-PAT-LOCAL-TEST-R03-T03 Each obligation of Declaring that an external test proves has a local test", proof.hasLocalStandIn)
	t.Run("ST-PAT-LOCAL-TEST-R04-T03 Each container that a test of Declaring starts is pinned by digest", proof.startsPinnedServices)
	t.Run("ST-PAT-LOCAL-TEST-R05-T03 Declaring's tests leave no container or file outside a temporary folder", proof.leavesNoState)
	t.Run("ST-PAT-LOCAL-TEST-R06-T03 Each obligation of Declaring with a cluster or volume test has a workstation test", proof.neverOnlyClusterProof)
}

func TestDistributingFollowsTheLocalTestPattern(t *testing.T) {
	proof := newLocalProof(t, "ST-DIS")
	t.Run("ST-PAT-LOCAL-TEST-R01-T04 Distributing's tests need no privileged container, cluster or hosted credential", proof.runsOnAWorkstation)
	t.Run("ST-PAT-LOCAL-TEST-R02-T04 Each test file of Distributing belongs to one documented suite", proof.oneCommandRunsEachSuite)
	t.Run("ST-PAT-LOCAL-TEST-R03-T04 Each obligation of Distributing that an external test proves has a local test", proof.hasLocalStandIn)
	t.Run("ST-PAT-LOCAL-TEST-R04-T04 Each container that a test of Distributing starts is pinned by digest", proof.startsPinnedServices)
	t.Run("ST-PAT-LOCAL-TEST-R05-T04 Distributing's tests leave no container or file outside a temporary folder", proof.leavesNoState)
	t.Run("ST-PAT-LOCAL-TEST-R06-T04 Each obligation of Distributing with a cluster or volume test has a workstation test", proof.neverOnlyClusterProof)
}

func TestDeliveringFollowsTheLocalTestPattern(t *testing.T) {
	proof := newLocalProof(t, "ST-DLV")
	t.Run("ST-PAT-LOCAL-TEST-R01-T05 Delivering's tests need no privileged container, cluster or hosted credential", proof.runsOnAWorkstation)
	t.Run("ST-PAT-LOCAL-TEST-R02-T05 Each test file of Delivering belongs to one documented suite", proof.oneCommandRunsEachSuite)
	t.Run("ST-PAT-LOCAL-TEST-R03-T05 Each obligation of Delivering that an external test proves has a local test", proof.hasLocalStandIn)
	t.Run("ST-PAT-LOCAL-TEST-R04-T05 Each container that a test of Delivering starts is pinned by digest", proof.startsPinnedServices)
	t.Run("ST-PAT-LOCAL-TEST-R05-T05 Delivering's tests leave no container or file outside a temporary folder", proof.leavesNoState)
	t.Run("ST-PAT-LOCAL-TEST-R06-T05 Each obligation of Delivering with a cluster or volume test has a workstation test", proof.neverOnlyClusterProof)
}

func TestExtendingFollowsTheLocalTestPattern(t *testing.T) {
	proof := newLocalProof(t, "ST-EXT")
	t.Run("ST-PAT-LOCAL-TEST-R01-T06 Extending's tests need no privileged container, cluster or hosted credential", proof.runsOnAWorkstation)
	t.Run("ST-PAT-LOCAL-TEST-R02-T06 Each test file of Extending belongs to one documented suite", proof.oneCommandRunsEachSuite)
	t.Run("ST-PAT-LOCAL-TEST-R03-T06 Each obligation of Extending that an external test proves has a local test", proof.hasLocalStandIn)
	t.Run("ST-PAT-LOCAL-TEST-R04-T06 Each container that a test of Extending starts is pinned by digest", proof.startsPinnedServices)
	t.Run("ST-PAT-LOCAL-TEST-R05-T06 Extending's tests leave no container or file outside a temporary folder", proof.leavesNoState)
	t.Run("ST-PAT-LOCAL-TEST-R06-T06 Each obligation of Extending with a cluster or volume test has a workstation test", proof.neverOnlyClusterProof)
}

func TestGeneratingFollowsTheLocalTestPattern(t *testing.T) {
	proof := newLocalProof(t, "ST-GEN")
	t.Run("ST-PAT-LOCAL-TEST-R01-T07 Generating's tests need no privileged container, cluster or hosted credential", proof.runsOnAWorkstation)
	t.Run("ST-PAT-LOCAL-TEST-R02-T07 Each test file of Generating belongs to one documented suite", proof.oneCommandRunsEachSuite)
	t.Run("ST-PAT-LOCAL-TEST-R03-T07 Each obligation of Generating that an external test proves has a local test", proof.hasLocalStandIn)
	t.Run("ST-PAT-LOCAL-TEST-R04-T07 Each container that a test of Generating starts is pinned by digest", proof.startsPinnedServices)
	t.Run("ST-PAT-LOCAL-TEST-R05-T07 Generating's tests leave no container or file outside a temporary folder", proof.leavesNoState)
	t.Run("ST-PAT-LOCAL-TEST-R06-T07 Each obligation of Generating with a cluster or volume test has a workstation test", proof.neverOnlyClusterProof)
}

func TestSupplyingFollowsTheLocalTestPattern(t *testing.T) {
	proof := newLocalProof(t, "ST-SUP")
	t.Run("ST-PAT-LOCAL-TEST-R01-T08 Supplying's tests need no privileged container, cluster or hosted credential", proof.runsOnAWorkstation)
	t.Run("ST-PAT-LOCAL-TEST-R02-T08 Each test file of Supplying belongs to one documented suite", proof.oneCommandRunsEachSuite)
	t.Run("ST-PAT-LOCAL-TEST-R03-T08 Each obligation of Supplying that an external test proves has a local test", proof.hasLocalStandIn)
	t.Run("ST-PAT-LOCAL-TEST-R04-T08 Each container that a test of Supplying starts is pinned by digest", proof.startsPinnedServices)
	t.Run("ST-PAT-LOCAL-TEST-R05-T08 Supplying's tests leave no container or file outside a temporary folder", proof.leavesNoState)
	t.Run("ST-PAT-LOCAL-TEST-R06-T08 Each obligation of Supplying with a cluster or volume test has a workstation test", proof.neverOnlyClusterProof)
}
