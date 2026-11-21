package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/declaration"
	"github.com/jsell-rh/stego/internal/fill"
	"gopkg.in/yaml.v3"
)

// runFillSign implements `stego fill sign <name>` (ST-EXT-CLI03,
// ST-EXT-CMD03): it signs the digest of the fill's current evidence with the
// person's own SSH or GPG key, as their git configuration names it, and
// writes the sign-off into the fill manifest. It refuses a fill with no
// evidence or with stale evidence. It keeps a sign-off that already signs the
// current evidence with a key that SIGNERS lists, writes nothing and says so,
// unless --force is given: a second sign-off of the same evidence changes
// fill.yaml for no gain and gives a merge conflict in a fill that no branch
// changed (walkthrough 19, W19-T4). STEGO holds no key: it makes, stores and
// uses none of its own.
func runFillSign(args []string) error {
	fs := flag.NewFlagSet("fill sign", flag.ContinueOnError)
	force := fs.Bool("force", false, "sign the fill again also when its sign-off already signs its current evidence")
	// The flag may come before or after the name.
	var names, flags []string
	for _, arg := range args {
		if arg == "--force" || arg == "-force" {
			flags = append(flags, arg)
		} else {
			names = append(names, arg)
		}
	}
	if err := fs.Parse(append(flags, names...)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return fmt.Errorf("fill name is required: stego fill sign [--force] <name>")
	}
	name := fs.Arg(0)
	if _, err := fillPackageName(name); err != nil {
		return err
	}
	input, err := buildReconcilerInput()
	if err != nil {
		return err
	}
	check, err := compiler.ResolveFillCheck(input)
	if err != nil {
		return err
	}
	dir := filepath.Join(input.ProjectDir, "fills", name)
	manifest := filepath.Join(dir, fill.ManifestFile)
	f, err := fill.ReadManifest(manifest, check)
	if err != nil {
		return err
	}
	if f.Evidence == nil {
		return fmt.Errorf("fill %q has no evidence: run stego test first", name)
	}
	current, err := fill.CurrentState(dir, f, check)
	if err != nil {
		return err
	}
	if f.Evidence.FillDigest != current.FillDigest || f.Evidence.ContractDigest != current.ContractDigest {
		return fmt.Errorf("the evidence of fill %q is stale: its files or its slot contract changed after its tests passed; run stego test again", name)
	}
	digest, err := fill.EvidenceDigest(f)
	if err != nil {
		return err
	}
	signers, err := fill.ReadSigners(input.ProjectDir)
	if err != nil {
		return err
	}
	if f.SignOff != nil && !*force {
		if signer, err := fill.VerifySignOff(f.SignOff, digest, signers); err == nil {
			// A sign-off whose key only the change adds to SIGNERS is not kept: the listed signer signs the fill
			// again with their own key (walkthrough 21, T-D; decision ST-EXT-Q31).
			base, err := readBaseSigners("")
			if err != nil {
				return err
			}
			if _, err := fill.VerifySignOff(f.SignOff, digest, base.signers); !base.changed || !errors.Is(err, fill.ErrUnlistedKey) {
				fmt.Printf("The sign-off of fill %q already signs its current evidence %s as %s, so fill.yaml stays as it is. To sign it again, run stego fill sign --force %s.\n", name, digest, signer, name)
				return nil
			}
			fmt.Printf("The sign-off of fill %q signs its current evidence as %s, whose key SIGNERS at %s does not list: the change adds it. So the fill is signed again with your key.\n", name, signer, base.ref)
		}
	}
	key, err := personalSigningKey(input.ProjectDir)
	if err != nil {
		return err
	}
	signature, fingerprint, err := key.sign(input.ProjectDir, digest)
	if err != nil {
		return err
	}
	signature.Signer = fill.Principal(signature, digest, signers)
	listed := signature.Signer != ""
	var addLine string
	if !listed {
		// The signer of a key that SIGNERS does not list is the fingerprint of
		// the key, never the path of a key file (walkthrough 20, T-B).
		if signature.KeyType == fill.KeySSH {
			var public string
			if fingerprint, public, err = fill.SSHSignatureKey(signature.Armored); err != nil {
				return fmt.Errorf("reading the key of the new SSH signature: %w", err)
			}
			addLine = fmt.Sprintf("the line %q", gitEmail(input.ProjectDir)+" "+public)
		} else {
			if !gpgFingerprint.MatchString(fingerprint) {
				return fmt.Errorf("%s gave no SIG_CREATED status line with a fingerprint of 40 or 64 hexadecimal digits, so the fingerprint of the key is not known, and fill.yaml stays as it is", key.program)
			}
			addLine = fmt.Sprintf("the line %q, and below it the output of gpg --armor --export %s", gitEmail(input.ProjectDir)+" gpg", fingerprint)
		}
		signature.Signer = fingerprint
	}
	signOff := fill.FillSignOff{EvidenceDigest: digest, Signature: signature, SignedAt: time.Now().UTC().Truncate(time.Second)}
	if err := fill.WriteSignOff(manifest, signOff); err != nil {
		return err
	}
	event := fill.FillSignedOff{FillName: name, Signer: signature.Signer, EvidenceDigest: digest}
	out, err := yaml.Marshal(map[string]fill.FillSignOff{"sign_off": signOff})
	if err != nil {
		return err
	}
	fmt.Printf("Signed the evidence %s of fill %q as %s.\n%s", event.EvidenceDigest, event.FillName, event.Signer, out)
	if !listed {
		fmt.Fprintf(os.Stderr, "warning: SIGNERS does not list the key %s that signed fill %q, so validate, plan, apply, deps and build refuse the fill.\n"+
			"  Do one of these:\n"+
			"  - Ask a person whom SIGNERS lists to run stego test and stego fill sign %s on your branch, and commit the fill.yaml that it writes.\n"+
			"  - Add your key to SIGNERS with %s, in a change that a listed signer reviews and merges. When SIGNERS lists the key, this sign-off counts and you need not sign again.\n",
			signature.Signer, name, name, addLine)
	}
	return nil
}

// gpgFingerprint is the form of the fingerprint of the status line SIG_CREATED: 40 hexadecimal digits (a version 4
// key) or 64 (a version 5 or 6 key), in upper case. fill sign refuses another value, so the program that
// gpg.program names cannot put any other text into fill.yaml (security review of PR #480, I1). It is the fingerprint
// of the key that signed, which can be a signing subkey; the signer is a label only, and stego validate decides the
// trust from the signature and SIGNERS.
var gpgFingerprint = regexp.MustCompile(`^[0-9A-F]{40}([0-9A-F]{24})?$`)

// gitEmail gives user.email of the git configuration, or <your email> when
// none is set, for the SIGNERS line that the warning of an unlisted key shows.
func gitEmail(projectDir string) string {
	cmd := exec.Command("git", "config", "--get", "user.email")
	cmd.Dir = projectDir
	out, err := cmd.Output()
	if email := strings.TrimSpace(string(out)); err == nil && email != "" {
		return email
	}
	return "<your email>"
}

// signingKey is the key that the person's git configuration names for
// signing: its format (ssh or openpgp), the key, and the program that signs.
type signingKey struct {
	format  string
	key     string
	program string
}

// personalSigningKey reads the person's signing key from git config:
// gpg.format and user.signingkey, with gpg.ssh.program or gpg.program. It
// refuses when no signing key is set up, and makes none.
func personalSigningKey(projectDir string) (signingKey, error) {
	config := func(name string) string {
		cmd := exec.Command("git", "config", "--get", name)
		cmd.Dir = projectDir
		out, err := cmd.Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	key := signingKey{format: config("gpg.format"), key: config("user.signingkey")}
	if key.format == "" {
		key.format = "openpgp"
	}
	if key.key == "" {
		return signingKey{}, errors.New("no signing key is set up: set user.signingkey (and gpg.format ssh for an SSH key) in your git config; STEGO never makes or uses a key of its own")
	}
	switch key.format {
	case "ssh":
		key.program = config("gpg.ssh.program")
		if key.program == "" {
			key.program = "ssh-keygen"
		}
	case "openpgp":
		key.program = config("gpg.program")
		if key.program == "" {
			key.program = "gpg"
		}
	default:
		return signingKey{}, fmt.Errorf("gpg.format %q is not supported: use ssh or openpgp", key.format)
	}
	return key, nil
}

// sign signs the digest with the person's key through ssh-keygen or gpg. For
// gpg it also gives the fingerprint of the key that signed, from the status
// line SIG_CREATED, which gpg writes with --status-fd as git asks it to.
func (k signingKey) sign(projectDir, digest string) (fill.Signature, string, error) {
	var cmd *exec.Cmd
	var keyType string
	switch k.format {
	case "ssh":
		keyType = fill.KeySSH
		keyFile := k.key
		if strings.HasPrefix(keyFile, "key::") {
			return fill.Signature{}, "", errors.New("user.signingkey names a literal key (key::); name the key file instead, so ssh-keygen signs with it")
		}
		if strings.HasPrefix(keyFile, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				keyFile = filepath.Join(home, keyFile[2:])
			}
		}
		cmd = exec.Command(k.program, "-Y", "sign", "-n", fill.SSHNamespace, "-f", keyFile)
	default:
		keyType = fill.KeyGPG
		cmd = exec.Command(k.program, "--batch", "--yes", "--status-fd=2", "--detach-sign", "--armor", "--local-user", k.key)
	}
	cmd.Dir = projectDir
	cmd.Stdin = strings.NewReader(digest)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return fill.Signature{}, "", fmt.Errorf("signing with %s: %v: %s", k.program, err, strings.TrimSpace(stderr.String()))
	}
	if stdout.Len() == 0 {
		return fill.Signature{}, "", fmt.Errorf("signing with %s gave no signature", k.program)
	}
	var fingerprint string
	for _, line := range strings.Split(stderr.String(), "\n") {
		// [GNUPG:] SIG_CREATED <type> <pk algo> <hash algo> <class> <timestamp> <fingerprint>
		if fields := strings.Fields(line); len(fields) == 8 && fields[0] == "[GNUPG:]" && fields[1] == "SIG_CREATED" {
			fingerprint = strings.ToUpper(fields[7])
		}
	}
	return fill.Signature{KeyType: keyType, Armored: stdout.String()}, fingerprint, nil
}

// baseSigners is the SIGNERS file of a base of the change, which validate, plan and fill sign compare with the
// SIGNERS file of the project (walkthrough 21, T-D; decision ST-EXT-Q31): ref and commit name the base, and changed
// tells whether the two files differ. With no base, changed is false.
type baseSigners struct {
	ref, commit string
	signers     fill.Signers
	changed     bool
}

// readBaseSigners reads the SIGNERS file of the service folder at the base: at the ref that named gives, else at the
// merge base of HEAD and the default branch of the repository (defaultBaseRef). A base that holds no SIGNERS file
// lists no key. With no Git repository, no commit or no default branch, there is no base, and changed is false.
func readBaseSigners(named string) (baseSigners, error) {
	var base baseSigners
	if strings.HasPrefix(named, "-") {
		return base, fmt.Errorf("the base ref %q starts with -, which git reads as an option", named)
	}
	base.ref = named
	if named == "" {
		ref, found, err := defaultBaseRef()
		if errors.Is(err, errNoCommit) || (err == nil && !found) {
			return base, nil
		}
		if err != nil {
			return base, err
		}
		base.ref = ref
		// The merge base is the version of the default branch that the change starts from: a key that the
		// change adds is not in its SIGNERS file.
		if commit, err := compatGit("merge-base", "HEAD", ref); err == nil {
			base.commit = strings.TrimSpace(string(commit))
		}
	}
	if base.commit == "" {
		commit, err := compatGit("rev-parse", "--verify", "--quiet", "--end-of-options", base.ref+"^{commit}")
		if err != nil {
			return base, fmt.Errorf("the base ref %s names no commit of the service's repository", base.ref)
		}
		base.commit = strings.TrimSpace(string(commit))
	}
	prefix, err := compatGit("rev-parse", "--show-prefix")
	if err != nil {
		return base, err
	}
	data, _, err := readBaseFile(base.commit, string(bytes.TrimSpace(prefix))+fill.SignersFile)
	if err != nil {
		return base, fmt.Errorf("read %s at the base ref %s (commit %s): %w", fill.SignersFile, base.ref, base.commit, err)
	}
	if base.signers, err = fill.ParseSigners(data); err != nil {
		return base, fmt.Errorf("%s at the base ref %s (commit %s): %w", fill.SignersFile, base.ref, base.commit, err)
	}
	current, err := os.ReadFile(fill.SignersFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return base, err
	}
	base.changed = !bytes.Equal(current, data)
	return base, nil
}

// addedKeyFaults gives one fault for each fill whose sign-off counts only because of a key that the SIGNERS file of
// the project adds to the SIGNERS file of the base (ST-EXT-UC02-R02, decision ST-EXT-Q31): validate and plan give
// each as a warning, and as a fault with --strict. With no base, or the same SIGNERS file as the base, there is none.
func addedKeyFaults(input compiler.ReconcilerInput, named string) ([]declaration.ValidationError, error) {
	base, err := readBaseSigners(named)
	if err != nil || !base.changed {
		return nil, err
	}
	check, err := compiler.ResolveFillCheck(input)
	if err != nil {
		return nil, err
	}
	added, err := fill.AddedKeySignOffs(check, base.signers)
	if err != nil {
		return nil, err
	}
	var faults []declaration.ValidationError
	for _, a := range added {
		faults = append(faults, declaration.ValidationError{Rule: "ST-EXT-UC02-R02", Path: "fills/" + a.Fill + "/" + fill.ManifestFile, Found: a.Signer,
			Message: fmt.Sprintf("the sign-off of fill %q counts only because of the key of %s, which SIGNERS at %s (commit %s) does not list: the change adds it. "+
				"Review the change of SIGNERS on its own and merge it first, or have a person whom SIGNERS at %s lists run stego fill sign --force %s",
				a.Fill, a.Signer, base.ref, shortCommit(base.commit), base.ref, a.Fill)})
	}
	return faults, nil
}

// shortCommit gives the first 12 characters of a commit ID.
func shortCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

// signersBaseUsage is the help of the flag --base of stego validate.
const signersBaseUsage = "the commit or branch whose SIGNERS a sign-off is checked against; the merge base of HEAD and the default branch when left out"

// checkAddedKeys writes a warning for each fill whose sign-off counts only because of a key that the change adds to
// SIGNERS, and with --strict refuses the plan (walkthrough 21, T-D; decision ST-EXT-Q31).
func checkAddedKeys(input compiler.ReconcilerInput, named string, stdout io.Writer) error {
	added, err := addedKeyFaults(input, named)
	if err != nil || len(added) == 0 {
		return err
	}
	for _, fault := range added {
		fmt.Fprintf(stdout, "WARNING: %s: %s\n", fault.Path, fault.Message)
	}
	if strictSecretSources {
		return fmt.Errorf("--strict refuses %d fill(s) whose sign-off counts only because of a key that the change adds to SIGNERS, so plan saved no plan", len(added))
	}
	return nil
}
