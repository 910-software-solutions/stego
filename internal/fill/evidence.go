package fill

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/910-software-solutions/stego/internal/buildidentity"
	"github.com/910-software-solutions/stego/internal/compiler"
)

// TestRun runs the tests of the fill in the folder and gives an error when
// a test fails or the tests do not run.
type TestRun func(fillDir string) error

// EvidenceResult is what recording test evidence did for one fill: the
// evidence it wrote, or why it wrote none.
type EvidenceResult struct {
	Fill   string
	Passed *FillTestsPassed
	// Kept tells that the fill's evidence already named its current state,
	// so the passing run kept it, and its sign-off with it.
	Kept   bool
	Failed bool
	Reason string
}

// RecordEvidence runs the tests of each fill of the project and, for each
// fill whose tests pass, writes its evidence into its fill manifest, with the
// fill digest and the contract digest taken before the run (ST-EXT-CMD02).
// It writes nothing for a fill whose manifest it cannot read against the
// declaration, for a fill with no test file or with an import it may not
// have (ST-EXT-INV05), and for a fill whose tests fail
// or whose files change during the run, and names why. Evidence that names
// the fill's current state stays as it is, with its sign-off, when the tests
// pass again. The results are in the order of the fill names.
func RecordEvidence(check compiler.FillCheck, run TestRun, build buildidentity.Record, now func() time.Time) ([]EvidenceResult, error) {
	names, err := fillFolders(check.ProjectDir)
	if err != nil {
		return nil, err
	}
	var results []EvidenceResult
	for _, name := range names {
		results = append(results, recordOne(check, name, run, build, now))
	}
	return results, nil
}

// recordOne records the evidence of one fill.
func recordOne(check compiler.FillCheck, name string, run TestRun, build buildidentity.Record, now func() time.Time) EvidenceResult {
	result := EvidenceResult{Fill: name}
	f, err := ReadManifest(check, name)
	dir := filepath.Join(check.ProjectDir, "fills", name)
	if err != nil {
		result.Reason = err.Error()
		return result
	}
	before, err := CurrentState(dir, f, check)
	if err != nil {
		result.Reason = err.Error()
		return result
	}
	if err := requireTests(before); err != nil {
		result.Reason = err.Error()
		return result
	}
	if err := checkImports(dir, check, name, f.Implements); err != nil {
		result.Reason = err.Error()
		return result
	}
	if err := run(dir); err != nil {
		result.Failed = true
		result.Reason = fmt.Sprintf("its tests failed: %v", err) + scaffoldTestCause(dir)
		return result
	}
	after, err := CurrentState(dir, f, check)
	if err != nil {
		result.Reason = err.Error()
		return result
	}
	if after.FillDigest != before.FillDigest || after.ContractDigest != before.ContractDigest {
		result.Reason = "its files or its slot contract changed while its tests ran"
		return result
	}
	if f.Evidence != nil && f.Evidence.FillDigest == before.FillDigest && f.Evidence.ContractDigest == before.ContractDigest {
		result.Kept = true
		result.Passed = &FillTestsPassed{FillName: name, FillDigest: before.FillDigest, ContractDigest: before.ContractDigest}
		return result
	}
	evidence := FillEvidence{
		FillDigest:     before.FillDigest,
		ContractDigest: before.ContractDigest,
		PassedAt:       now().UTC().Truncate(time.Second),
		Compiler:       build,
		Files:          before.Files,
	}
	if err := WriteEvidence(filepath.Join(check.ProjectDir, "fills"), name, evidence); err != nil {
		result.Reason = fmt.Sprintf("writing its evidence: %v", err)
		return result
	}
	result.Passed = &FillTestsPassed{FillName: name, FillDigest: evidence.FillDigest, ContractDigest: evidence.ContractDigest}
	return result
}

// scaffoldTestCause names the likely cause of failed tests of the fill in dir: a test file that still names
// ErrNotImplemented, as the test that an older stego fill create wrote for the unfinished fill does. When the author
// writes the fill and removes ErrNotImplemented, that test no longer compiles; when the fill keeps it, the test fails
// (walkthrough 11, W11-B7). It gives the empty text when no test file of the folder names it.
func scaffoldTestCause(dir string) string {
	tests, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	if err != nil {
		return ""
	}
	var named []string
	for _, test := range tests {
		data, err := os.ReadFile(test)
		if err == nil && strings.Contains(string(data), "ErrNotImplemented") {
			named = append(named, filepath.Base(test))
		}
	}
	if len(named) == 0 {
		return ""
	}
	return fmt.Sprintf("; %s still expects ErrNotImplemented, as the test that an older stego fill create wrote for the unfinished fill: "+
		"replace it with the tests of what the fill does (see docs/upgrade.md#write-the-tests-of-your-fills)", strings.Join(named, ", "))
}

// testEvent is one event of the report of go test -json.
type testEvent struct {
	Action string
	Test   string
	Output string
}

// ReadTestReport reads the report of go test -json of the tests of one fill from r, writes the text of its output to
// w, and gives an error unless at least one test passed and none failed: a run with no test to run, such as -count=0
// or a run filter from the environment, records no evidence (ST-EXT-UC02-R01). A report that cannot be read is an
// error too.
func ReadTestReport(r io.Reader, w io.Writer) error {
	decoder := json.NewDecoder(r)
	passed, failed := 0, 0
	for {
		var event testEvent
		err := decoder.Decode(&event)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			_, _ = io.Copy(io.Discard, r)
			return fmt.Errorf("the report of go test cannot be read: %w", err)
		}
		if event.Output != "" {
			fmt.Fprint(w, event.Output)
		}
		switch {
		case event.Action == "pass" && event.Test != "":
			passed++
		case event.Action == "fail":
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d test or package failed", failed)
	}
	if passed == 0 {
		return errors.New("no test of the fill ran")
	}
	return nil
}
