package proxytest

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/module"
)

// The proofs that Start takes from one go mod download -json only what the go command checked for each module
// (security review of PR #16, L2 and L3). A wrapper of the go command changes its output or the module cache after
// the go command checked it, as a go command of a source that is not whole can. The test binary is the wrapper: run
// through a link named go, it reads its mode and the real go command from the file wrapperConfig beside the link,
// runs the real go command and changes its result, and runs no test. The name, not the environment, tells it: Start
// runs the go command also with an environment that is not the one of the source.

const (
	wrapperConfig = "wrapper.json"
	// wrappedModule is the module whose result the wrapper changes; wrappedOther is the module that it leaves.
	wrappedModule = "example.com/stand/a"
	wrappedOther  = "example.com/stand/b"
)

func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) == "go" {
		var config struct{ Mode, Go string }
		data, err := os.ReadFile(filepath.Join(filepath.Dir(os.Args[0]), wrapperConfig))
		if err == nil {
			err = json.Unmarshal(data, &config)
		}
		if err != nil || config.Mode == "" || config.Go == "" {
			os.Stderr.WriteString("wrapper: no mode and go command beside the link\n")
			os.Exit(3)
		}
		os.Exit(wrapGo(config.Mode, config.Go, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// wrapGo runs the go command goTool with args and gives its exit status. For go mod download, it changes the output
// or the cache as mode tells, and writes the output that it changed.
func wrapGo(mode, goTool string, args []string) int {
	command := exec.Command(goTool, args...)
	command.Stdin, command.Stderr = os.Stdin, os.Stderr
	if len(args) < 2 || args[0] != "mod" || args[1] != "download" {
		command.Stdout = os.Stdout
		return exitStatus(command.Run())
	}
	output, err := command.Output()
	status := exitStatus(err)
	var results []map[string]any
	decoder := json.NewDecoder(bytes.NewReader(output))
	for decoder.More() {
		var result map[string]any
		if err := decoder.Decode(&result); err != nil {
			os.Stderr.WriteString("wrapper: " + err.Error() + "\n")
			return 3
		}
		results = append(results, result)
	}
	wrapped := func(change func(map[string]any)) {
		for _, result := range results {
			if result["Path"] == wrappedModule {
				change(result)
			}
		}
	}
	const wrongSum = "h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	suffix := ""
	switch mode {
	case "change-go-mod":
		// The go command checked the go.mod; the wrapper changes it in the cache before Start reads it.
		var failure error
		wrapped(func(result map[string]any) {
			name, _ := result["GoMod"].(string)
			if failure = os.Chmod(name, 0o644); failure == nil {
				failure = appendTo(name, "// planted\n")
			}
		})
		if failure != nil {
			os.Stderr.WriteString("wrapper: " + failure.Error() + "\n")
			return 3
		}
	case "fail-with-no-error":
		status = 1
	case "cut-output", "add-partial-result":
	case "error-of-one":
		wrapped(func(result map[string]any) { result["Error"] = "planted error" })
		status = 1
	case "error-of-one-with-no-go-mod":
		wrapped(func(result map[string]any) {
			result["Error"] = "planted error"
			delete(result, "GoMod")
		})
		status = 1
	case "drop-one":
		var kept []map[string]any
		for _, result := range results {
			if result["Path"] != wrappedModule {
				kept = append(kept, result)
			}
		}
		results = kept
	case "swap-paths":
		for _, result := range results {
			if result["Path"] == wrappedModule {
				result["Path"] = wrappedOther
			} else {
				result["Path"] = wrappedModule
			}
		}
	case "wrong-zip-sum":
		wrapped(func(result map[string]any) { result["Sum"] = wrongSum })
	case "wrong-go-mod-sum":
		wrapped(func(result map[string]any) { result["GoModSum"] = wrongSum })
	default:
		os.Stderr.WriteString("wrapper: unknown mode " + mode + "\n")
		return 3
	}
	var out bytes.Buffer
	for _, result := range results {
		data, err := json.MarshalIndent(result, "", "\t")
		if err != nil {
			return 3
		}
		out.Write(append(data, '\n'))
	}
	written := out.Bytes()
	switch mode {
	case "cut-output":
		written = written[:len(written)*3/4]
	case "add-partial-result":
		suffix = `{"Path": "` + wrappedModule + `"`
	}
	os.Stdout.Write(written)
	os.Stdout.WriteString(suffix)
	return status
}

func exitStatus(err error) int {
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		return exit.ExitCode()
	default:
		os.Stderr.WriteString("wrapper: " + err.Error() + "\n")
		return 3
	}
}

func appendTo(name, text string) error {
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(file, text); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

// startWrapped runs Start for wrappedModule and wrappedOther with the test binary as the go command, which changes the
// result of the real go command as mode tells. It gives the failure of Start, or "" and the servers.
func startWrapped(t *testing.T, mode string) (string, *Servers, *upstream) {
	t.Helper()
	u := newUpstreamOf(t, map[string]bool{wrappedModule: true, wrappedOther: true})
	from := u.source(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	folder := t.TempDir()
	wrapper := filepath.Join(folder, "go")
	if err := os.Symlink(binary, wrapper); err != nil {
		t.Fatal(err)
	}
	config, err := json.Marshal(map[string]string{"Mode": mode, "Go": testGo()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, wrapperConfig), config, 0o644); err != nil {
		t.Fatal(err)
	}
	r := &fatalRecorder{TB: t}
	var servers *Servers
	done := make(chan struct{})
	go func() {
		defer close(done)
		servers = Start(r, wrapper, from, wrappedModule+"@"+upstreamVersion, wrappedOther+"@"+upstreamVersion)
	}()
	<-done
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.message, servers, u
}

func TestStartTakesOnlyCheckedResults(t *testing.T) {
	a := module.Version{Path: wrappedModule, Version: upstreamVersion}
	b := module.Version{Path: wrappedOther, Version: upstreamVersion}
	t.Run("ST-STD-LOCAL-R03-T22 Start refuses a go.mod of the cache whose hash is not the hash that the go command checked", func(t *testing.T) {
		// The go command hashes go.mod again on each read and checks it against the sum database, so a go.mod that
		// changes before Start runs it is refused by the go command itself. Only a change after its check and before
		// Start reads the file reaches the check of Start.
		failure, _, u := startWrapped(t, "change-go-mod")
		want := "the go.mod of " + a.String() + " in the cache has the hash "
		if !strings.Contains(failure, want) || !strings.Contains(failure, "not the checked hash "+u.sums[wrappedModule][1]) {
			t.Fatalf("Start served a go.mod that changed after the check; its failure is %q, want %q", failure, want)
		}
	})
	t.Run("ST-STD-LOCAL-R03-T23 Start refuses a go mod download whose output or exit status does not give each module whole or with its error", func(t *testing.T) {
		for _, refused := range []struct{ mode, want string }{
			{"fail-with-no-error", "and no module gives an error"},
			{"drop-one", "download " + a.String() + ": the go command gave no result for it"},
			{"error-of-one-with-no-go-mod", "download " + a.String() + ": planted error"},
			{"cut-output", "proxytest: download "},
			{"add-partial-result", "proxytest: download "},
			{"swap-paths", "not the file "},
			{"wrong-zip-sum", "not the checked hash "},
			{"wrong-go-mod-sum", "not the checked hash "},
		} {
			failure, _, _ := startWrapped(t, refused.mode)
			if !strings.Contains(failure, refused.want) {
				t.Errorf("%s: the failure of Start is %q, want one that holds %q", refused.mode, failure, refused.want)
			}
		}
		// A module with an error gets its checked go.mod alone; the other module of the run stays whole.
		failure, servers, u := startWrapped(t, "error-of-one")
		if failure != "" {
			t.Fatalf("error-of-one: Start refused: %s", failure)
		}
		if record := string(servers.records[a]); record != a.Path+" "+a.Version+"/go.mod "+u.sums[a.Path][1]+"\n" {
			t.Errorf("error-of-one: the record of %s is %q, want the go.mod line alone", a, record)
		}
		if _, err := os.Stat(servers.ZipPath(t, a.Path, a.Version)); !os.IsNotExist(err) {
			t.Errorf("error-of-one: Start serves a zip of %s, whose result has an error: %v", a, err)
		}
		if record := string(servers.records[b]); record != b.Path+" "+b.Version+" "+u.sums[b.Path][0]+"\n"+b.Path+" "+b.Version+"/go.mod "+u.sums[b.Path][1]+"\n" {
			t.Errorf("error-of-one: the record of %s is %q, want its zip and go.mod lines", b, record)
		}
	})
	t.Run("ST-STD-LOCAL-R03-T24 Start gives each version of one module, and each path with upper-case letters, its own record and zip", func(t *testing.T) {
		mods := []module.Version{
			{Path: "example.com/stand/in", Version: "v1.0.0"},
			{Path: "example.com/stand/in", Version: "v1.1.0"},
			{Path: "example.com/Stand/In", Version: "v1.0.0"},
			{Path: "example.com/Stand/In", Version: "v1.1.0-RC.1"},
		}
		versions := map[module.Version]bool{}
		var args []string
		for _, m := range mods {
			versions[m] = true
			args = append(args, m.String())
		}
		u := newUpstreamOfVersions(t, versions)
		failure, servers := startFails(t, u.source(t), args...)
		if failure != "" {
			t.Fatalf("Start refused the modules of a checked source: %s", failure)
		}
		for _, m := range mods {
			sums := u.versionSums[m]
			want := m.Path + " " + m.Version + " " + sums[0] + "\n" + m.Path + " " + m.Version + "/go.mod " + sums[1] + "\n"
			if record := string(servers.records[m]); record != want {
				t.Errorf("the record of %s is %q, want %q", m, record, want)
			}
			data, err := os.ReadFile(servers.ZipPath(t, m.Path, m.Version))
			if err != nil {
				t.Errorf("Start serves no zip of %s: %v", m, err)
				continue
			}
			escapedPath, _ := module.EscapePath(m.Path)
			escapedVersion, _ := module.EscapeVersion(m.Version)
			upstreamZip, err := os.ReadFile(filepath.Join(u.folder, filepath.FromSlash(escapedPath), "@v", escapedVersion+".zip"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(data, upstreamZip) {
				t.Errorf("the zip that Start serves for %s is not the zip of %s upstream", m, m)
			}
		}
	})
}
