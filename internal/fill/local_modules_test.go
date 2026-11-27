package fill

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/910-software-solutions/stego/internal/compiler"
)

// localFillProject writes a project whose fill gate imports the local fill module example.com/erp/workflow, which
// imports the local fill module example.com/erp/rules, as the layout of the spike gives it (go.mod replace =>
// ./workflow). The module example.com/erp/other is local and listed, and no fill reaches it.
func localFillProject(t *testing.T) string {
	t.Helper()
	project := t.TempDir()
	for name, data := range map[string]string{
		"go.mod": "module github.com/example/service\n\ngo 1.26.11\n\nrequire (\n\texample.com/erp/workflow v0.0.0\n\texample.com/erp/rules v0.0.0\n\texample.com/erp/other v0.0.0\n)\n\n" +
			"replace example.com/erp/workflow => ./workflow\n\nreplace example.com/erp/rules => ./rules\n\nreplace example.com/erp/other => ./other\n",
		"fills/gate/fill.yaml":      "kind: fill\nname: gate\nimplements: rest-api.before_create\ncollection: org-users\n",
		"fills/gate/policy.go":      "package gate\n\nimport \"example.com/erp/workflow\"\n\nfunc allowed(role string) bool { return workflow.MayCreate(role) }\n",
		"fills/gate/policy_test.go": "package gate\n\nimport \"testing\"\n\nfunc TestAllowed(t *testing.T) { _ = allowed(\"admin\") }\n",
		"workflow/go.mod":           "module example.com/erp/workflow\n\ngo 1.26.11\n",
		"workflow/workflow.go":      "package workflow\n\nimport \"example.com/erp/rules/roles\"\n\n// MayCreate tells whether the role may create.\nfunc MayCreate(role string) bool { return roles.Admin(role) }\n",
		"workflow/workflow_test.go": "package workflow\n\nimport \"testing\"\n\nfunc TestMayCreate(t *testing.T) {}\n",
		"rules/go.mod":              "module example.com/erp/rules\n\ngo 1.26.11\n",
		"rules/roles/roles.go":      "package roles\n\n// Admin tells whether the role is admin.\nfunc Admin(role string) bool { return role == \"admin\" }\n",
		"other/go.mod":              "module example.com/erp/other\n\ngo 1.26.11\n",
		"other/other.go":            "package other\n",
	} {
		path := filepath.Join(project, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return project
}

// sshSigner makes an SSH key in a folder of the test and gives its key file and its SIGNERS row.
func sshSigner(t *testing.T) (string, string) {
	t.Helper()
	key := filepath.Join(t.TempDir(), "signer")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "signer", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	public, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(public))
	return key, "listed@example.com " + fields[0] + " " + fields[1]
}

func TestLocalFillModuleDigest(t *testing.T) {
	t.Run("ST-EXT-DS01-T07 A change to a local fill module that a signed fill reaches makes the fill stale, and a change to one that it does not reach does not", func(t *testing.T) {
		// spike-localfill (2026-11-27): the fill digest covered only the fill folder, so MayCreate -> return true in
		// ./workflow left a signed fill qualified.
		project := localFillProject(t)
		check := builtInCheck()
		check.ProjectDir = project
		check.Contracts = dirContracts(filepath.Join("..", "..", "registry"))
		check.FillModules = []string{"example.com/erp/workflow", "example.com/erp/rules", "example.com/erp/other"}
		dir := filepath.Join(project, "fills", "gate")
		f, err := ReadManifest(check, "gate")
		if err != nil {
			t.Fatal(err)
		}
		current, err := CurrentState(dir, f, check)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"policy.go", "module:example.com/erp/workflow/workflow.go", "module:example.com/erp/workflow/go.mod", "module:example.com/erp/rules/roles/roles.go"} {
			if current.Files[name] == "" {
				t.Errorf("the files of the fill digest have no %s: %v", name, current.Files)
			}
		}
		for name := range current.Files {
			if strings.HasPrefix(name, "module:example.com/erp/other/") {
				t.Errorf("the files of the fill digest hold %s of a module that the fill does not reach", name)
			}
		}
		// A signed fill with this evidence is qualified.
		key, row := sshSigner(t)
		signers := mustSigners(t, []byte(row+"\n"))
		f.Evidence = &FillEvidence{FillDigest: current.FillDigest, ContractDigest: current.ContractDigest, PassedAt: time.Now().UTC(), Files: current.Files}
		digest := mustEvidenceDigest(t, f)
		cmd := exec.Command("ssh-keygen", "-Y", "sign", "-n", SSHNamespace, "-f", key)
		cmd.Stdin = strings.NewReader(digest)
		armored, err := cmd.Output()
		if err != nil {
			t.Fatalf("ssh-keygen -Y sign: %v", err)
		}
		f.SignOff = &FillSignOff{EvidenceDigest: digest, Signature: Signature{KeyType: KeySSH, Signer: "listed@example.com", Armored: string(armored)}, SignedAt: time.Now()}
		standing := func() (Standing, string) {
			t.Helper()
			now, err := CurrentState(dir, f, check)
			if err != nil {
				t.Fatal(err)
			}
			return assess(f, now, signers)
		}
		if got, why := standing(); got != Qualified {
			t.Fatalf("the signed fill is %s (%s), want qualified", got, why)
		}
		// A change to a module that no fill reaches leaves it qualified.
		write := func(name, data string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(project, filepath.FromSlash(name)), []byte(data), 0644); err != nil {
				t.Fatal(err)
			}
		}
		write("other/other.go", "package other\n\nfunc Changed() {}\n")
		if got, why := standing(); got != Qualified {
			t.Fatalf("after a change to a module that the fill does not reach, the fill is %s (%s), want qualified", got, why)
		}
		// The change of the spike, with no change to a file of the fill folder.
		original, err := os.ReadFile(filepath.Join(project, "workflow", "workflow.go"))
		if err != nil {
			t.Fatal(err)
		}
		write("workflow/workflow.go", strings.Replace(string(original), "return roles.Admin(role)", "return true", 1))
		if got, why := standing(); got != Stale || !strings.Contains(why, "module:example.com/erp/workflow/workflow.go") {
			t.Fatalf("after MayCreate -> return true in the local module, the fill is %s (%s), want stale, naming the file", got, why)
		}
		// A module that the fill reaches through another module counts too.
		write("workflow/workflow.go", string(original))
		roles, err := os.ReadFile(filepath.Join(project, "rules", "roles", "roles.go"))
		if err != nil {
			t.Fatal(err)
		}
		write("rules/roles/roles.go", "package roles\n\nfunc Admin(role string) bool { return true }\n")
		if got, why := standing(); got != Stale || !strings.Contains(why, "module:example.com/erp/rules/roles/roles.go") {
			t.Fatalf("after a change to a module that the fill reaches through workflow, the fill is %s (%s), want stale", got, why)
		}
		// The revision check of stego build reads the manifest and counts the same files.
		if err := WriteEvidence(filepath.Join(dir, ManifestFile), *f.Evidence); err != nil {
			t.Fatal(err)
		}
		if err := WriteSignOff(filepath.Join(dir, ManifestFile), *f.SignOff); err != nil {
			t.Fatal(err)
		}
		write("SIGNERS", row+"\n")
		revision := compiler.FillCheck{ProjectDir: project, FillModules: check.FillModules}
		write("rules/roles/roles.go", string(roles))
		faults, err := RevisionFaults(project, []string{"gate"}, revision)
		if err != nil || len(faults) != 0 {
			t.Fatalf("the revision check of the signed fill with its modules as signed gave %q, %v, want no fault", faults, err)
		}
		write("rules/roles/roles.go", "package roles\n\nfunc Admin(role string) bool { return true }\n")
		faults, err = RevisionFaults(project, []string{"gate"}, revision)
		if err != nil {
			t.Fatal(err)
		}
		if len(faults) != 1 || !strings.Contains(faults[0], "module:example.com/erp/rules/roles/roles.go") {
			t.Errorf("the revision check gave %q, want the fill stale, naming the file of the module", faults)
		}
	})
}
