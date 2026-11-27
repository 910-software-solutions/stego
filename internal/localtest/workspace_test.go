package localtest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The proof that RuntimeWorkspace gives only a work file that changes no module source but the runtime module
// (ST-STD-LOCAL-R03, sec-8 F3): the work file of scripts/runtime-workspace.sh passes checkRuntimeWork, and a work file
// that uses another folder or replaces another module, or the runtime module with another folder, does not.
func TestRuntimeWorkspaceChecksTheWorkFile(t *testing.T) {
	root := filepath.Join(filepath.Dir(ModulesFolder()), "..", "..", "..")
	module := t.TempDir()
	if err := os.WriteFile(filepath.Join(module, "go.mod"), []byte("module example.com/svc\n\ngo 1.26.11\n\nrequire "+runtimeModule+" v0.207.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	entry := RuntimeWorkspace(t, module)
	work, ok := strings.CutPrefix(entry, "GOWORK=")
	if !ok {
		t.Fatalf("RuntimeWorkspace gives %q, not a GOWORK entry", entry)
	}
	data, err := os.ReadFile(work)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "replace "+runtimeModule+" v0.207.0 => ") {
		t.Fatalf("the work file of runtime-workspace.sh replaces no runtime pin:\n%s", data)
	}
	runtimeDir, err := filepath.EvalSymlinks(filepath.Join(root, "registry", "runtime"))
	if err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	for name, text := range map[string]string{
		"a use of another folder":                        "go 1.26.11\n\nuse (\n\t" + module + "\n\t" + other + "\n)\n",
		"a replace of another module":                    "go 1.26.11\n\nuse " + module + "\n\nreplace example.com/lib => " + runtimeDir + "\n",
		"a replace of the runtime with another folder":   "go 1.26.11\n\nuse " + module + "\n\nreplace " + runtimeModule + " => " + other + "\n",
		"a replace of the runtime with a module version": "go 1.26.11\n\nuse " + module + "\n\nreplace " + runtimeModule + " => example.com/fork v1.0.0\n",
		"a relative use of the folder of the work file":  "go 1.26.11\n\nuse (\n\t" + module + "\n\t.\n)\n",
		// From the folder of this test, the runtime folder; from the folder of the work file, another folder.
		"a relative use of a folder that is registry/runtime only from the test folder": "go 1.26.11\n\nuse (\n\t" + module + "\n\t../../registry/runtime\n)\n",
		"no use": "go 1.26.11\n",
	} {
		name, text := name, text
		t.Run(name, func(t *testing.T) {
			planted := filepath.Join(t.TempDir(), "runtime.work")
			if err := os.WriteFile(planted, []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
			if why := checkRuntimeWork(root, module, planted); why == "" {
				t.Fatalf("checkRuntimeWork accepts the work file with %s:\n%s", name, text)
			}
		})
	}
}

// The proof that RuntimeWorkspace takes only a work file that passes checkRuntimeWork: the work file of a script that
// also uses another folder fails runtimeWork.
func TestRuntimeWorkspaceRefusesAnotherWorkFile(t *testing.T) {
	root := t.TempDir()
	module, other := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf 'go 1.26.11\\n\\nuse (\\n\\t%s\\n\\t%s\\n)\\n' \"$1\" " + other + " > \"$2\"\n"
	if err := os.WriteFile(filepath.Join(root, "scripts", "runtime-workspace.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(t.TempDir(), "runtime.work")
	err := runtimeWork(root, module, work)
	if err == nil || !strings.Contains(err.Error(), "which is not the module folder or registry/runtime") {
		data, _ := os.ReadFile(work)
		t.Fatalf("runtimeWork gives %v for a work file that uses another folder:\n%s", err, data)
	}
}
