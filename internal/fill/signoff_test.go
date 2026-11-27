package fill

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// testKeys are the keys of ST-EXT-TD02, made new for the test run: two SSH
// keys and two GPG keys in one test keyring, and a SIGNERS file that lists
// listed-ssh and listed-gpg only. No key is committed.
type testKeys struct {
	dir                 string
	gnupgHome           string
	listedSSH           string
	unlistedSSH         string
	listedGPG           string
	keyringGPG          string
	signers             []byte
	listedSSHAllowedRow string
}

var (
	keysOnce sync.Once
	keys     *testKeys
	keysErr  error
)

// makeTestKeys makes the keys once per test run. TestMain stops the
// gpg-agent and removes the folder.
func makeTestKeys(t *testing.T) *testKeys {
	t.Helper()
	keysOnce.Do(func() { keys, keysErr = newTestKeys() })
	if keysErr != nil {
		t.Fatal(keysErr)
	}
	return keys
}

func newTestKeys() (*testKeys, error) {
	dir, err := os.MkdirTemp("", "stego-fill-keys-")
	if err != nil {
		return nil, err
	}
	k := &testKeys{dir: dir, gnupgHome: filepath.Join(dir, "gnupg")}
	if err := os.Mkdir(k.gnupgHome, 0700); err != nil {
		return nil, err
	}
	run := func(name string, args ...string) (string, error) {
		cmd := exec.Command(name, args...)
		cmd.Env = append(os.Environ(), "GNUPGHOME="+k.gnupgHome)
		out, err := cmd.CombinedOutput()
		if err != nil {
			// The error itself, with the output: a tool that does not start, such as a host with no gpg, has no
			// ProcessState, and an exec.ExitError of it printed only <nil> (security review of PR #12).
			return "", fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, out)
		}
		return string(out), nil
	}
	for _, name := range []string{"listed-ssh", "unlisted-ssh"} {
		if _, err := run("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", name, "-f", filepath.Join(dir, name)); err != nil {
			return nil, err
		}
	}
	k.listedSSH, k.unlistedSSH = filepath.Join(dir, "listed-ssh"), filepath.Join(dir, "unlisted-ssh")
	for _, name := range []string{"listed-gpg", "keyring-gpg"} {
		if _, err := run("gpg", "--batch", "--passphrase", "", "--quick-gen-key", name+" <"+name+"@example.com>", "ed25519", "sign", "never"); err != nil {
			return nil, err
		}
	}
	fingerprint := func(name string) (string, error) {
		out, err := run("gpg", "--batch", "--with-colons", "--list-keys", name+"@example.com")
		if err != nil {
			return "", err
		}
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "fpr:") {
				return strings.Split(line, ":")[9], nil
			}
		}
		return "", os.ErrNotExist
	}
	if k.listedGPG, err = fingerprint("listed-gpg"); err != nil {
		return nil, err
	}
	if k.keyringGPG, err = fingerprint("keyring-gpg"); err != nil {
		return nil, err
	}
	public, err := os.ReadFile(k.listedSSH + ".pub")
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(string(public))
	k.listedSSHAllowedRow = "listed@example.com " + fields[0] + " " + fields[1]
	armored, err := run("gpg", "--batch", "--armor", "--export", k.listedGPG)
	if err != nil {
		return nil, err
	}
	k.signers = []byte("# SSH keys, in the allowed signers format\n" + k.listedSSHAllowedRow + "\n\n# GPG keys\nlisted-gpg@example.com gpg\n" + armored)
	_, _ = run("gpgconf", "--kill", "gpg-agent")
	return k, nil
}

// sshSign signs the message with the SSH key through ssh-keygen -Y sign.
func (k *testKeys) sshSign(t *testing.T, key, message string) string {
	t.Helper()
	cmd := exec.Command("ssh-keygen", "-Y", "sign", "-n", SSHNamespace, "-f", key)
	cmd.Stdin = strings.NewReader(message)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ssh-keygen -Y sign: %v", err)
	}
	return string(out)
}

// gpgSign signs the message with the GPG key through gpg --detach-sign
// --armor.
func (k *testKeys) gpgSign(t *testing.T, fingerprint, message string) string {
	t.Helper()
	cmd := exec.Command("gpg", "--batch", "--detach-sign", "--armor", "--local-user", fingerprint)
	cmd.Env = append(os.Environ(), "GNUPGHOME="+k.gnupgHome)
	cmd.Stdin = strings.NewReader(message)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("gpg --detach-sign: %v", err)
	}
	t.Cleanup(func() {
		stop := exec.Command("gpgconf", "--kill", "gpg-agent")
		stop.Env = append(os.Environ(), "GNUPGHOME="+k.gnupgHome)
		_ = stop.Run()
	})
	return string(out)
}

func TestMain(m *testing.M) {
	code := m.Run()
	if keys != nil {
		stop := exec.Command("gpgconf", "--kill", "gpg-agent")
		stop.Env = append(os.Environ(), "GNUPGHOME="+keys.gnupgHome)
		_ = stop.Run()
		_ = os.RemoveAll(keys.dir)
	}
	os.Exit(code)
}

// unsignedFill is unsigned-fill of ST-EXT-TD01 in memory: a fill with
// evidence and no sign-off.
func unsignedFill() *Fill {
	return &Fill{Kind: "fill", Name: "unsigned-fill", Implements: "rest-api.before_create", Collection: "org-users", Evidence: &FillEvidence{
		FillDigest: strings.Repeat("1", 64), ContractDigest: strings.Repeat("2", 64), PassedAt: time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC),
		Files: map[string]string{"policy.go": strings.Repeat("3", 64)},
	}}
}

func mustEvidenceDigest(t *testing.T, f *Fill) string {
	t.Helper()
	digest, err := EvidenceDigest(f)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func mustSigners(t *testing.T, data []byte) Signers {
	t.Helper()
	signers, err := ParseSigners(data)
	if err != nil {
		t.Fatal(err)
	}
	return signers
}

func TestVerifySignOff(t *testing.T) {
	k := makeTestKeys(t)
	signers := mustSigners(t, k.signers)
	f := unsignedFill()
	digest := mustEvidenceDigest(t, f)
	sshSignOff := func(key, message string) *FillSignOff {
		return &FillSignOff{EvidenceDigest: digest, Signature: Signature{KeyType: KeySSH, Signer: "listed@example.com", Armored: k.sshSign(t, key, message)}, SignedAt: time.Now()}
	}
	gpgSignOff := func(fingerprint, message string) *FillSignOff {
		return &FillSignOff{EvidenceDigest: digest, Signature: Signature{KeyType: KeyGPG, Signer: "listed-gpg@example.com", Armored: k.gpgSign(t, fingerprint, message)}, SignedAt: time.Now()}
	}

	t.Run("ST-EXT-DS02-T01 A sign-off by the listed SSH key over the current evidence digest is accepted", func(t *testing.T) {
		principal, err := VerifySignOff(sshSignOff(k.listedSSH, digest), digest, signers)
		if err != nil || principal != "listed@example.com" {
			t.Fatalf("refused the sign-off: %q, %v", principal, err)
		}
	})

	t.Run("ST-EXT-DS02-T02 A sign-off by the listed GPG key over the current evidence digest is accepted", func(t *testing.T) {
		principal, err := VerifySignOff(gpgSignOff(k.listedGPG, digest), digest, signers)
		if err != nil || principal != "listed-gpg@example.com" {
			t.Fatalf("refused the sign-off: %q, %v", principal, err)
		}
	})

	t.Run("ST-EXT-DS02-T03 A signature by a listed key over any other digest is refused", func(t *testing.T) {
		earlier := unsignedFill()
		earlier.Evidence.PassedAt = earlier.Evidence.PassedAt.Add(-time.Hour)
		for name, message := range map[string]string{
			"earlier evidence": mustEvidenceDigest(t, earlier),
			"an added newline": digest + "\n",
		} {
			_, err := VerifySignOff(sshSignOff(k.listedSSH, message), digest, signers)
			if err == nil || !strings.Contains(err.Error(), ErrNotCovered.Error()) {
				t.Errorf("%s: got %v, want %v", name, err, ErrNotCovered)
			}
		}
	})

	t.Run("ST-EXT-DS02-T04 A valid signature by a key that SIGNERS does not list is refused", func(t *testing.T) {
		for name, signOff := range map[string]*FillSignOff{
			"unlisted-ssh": sshSignOff(k.unlistedSSH, digest),
			"keyring-gpg":  gpgSignOff(k.keyringGPG, digest),
		} {
			_, err := VerifySignOff(signOff, digest, signers)
			if err == nil || !strings.Contains(err.Error(), ErrUnlistedKey.Error()) {
				t.Errorf("%s: got %v, want %v", name, err, ErrUnlistedKey)
			}
		}
	})

	t.Run("ST-EXT-DS02-T05 A changed signature is refused", func(t *testing.T) {
		for name, signOff := range map[string]*FillSignOff{
			"listed-ssh": sshSignOff(k.listedSSH, digest),
			"listed-gpg": gpgSignOff(k.listedGPG, digest),
		} {
			signOff.Signature.Armored = changeArmoredBody(t, signOff.Signature.Armored)
			if principal, err := VerifySignOff(signOff, digest, signers); err == nil {
				t.Errorf("%s: accepted a changed signature as %s", name, principal)
			}
		}
	})

	t.Run("ST-EXT-DS02-T06 A listed SSH key whose SIGNERS entry has expired is refused, as ssh-keygen refuses it", func(t *testing.T) {
		fields := strings.SplitN(k.listedSSHAllowedRow, " ", 2)
		expired := mustSigners(t, []byte(fields[0]+" valid-before=\"20200101\" "+fields[1]+"\n"))
		if principal, err := VerifySignOff(sshSignOff(k.listedSSH, digest), digest, expired); err == nil {
			t.Fatalf("accepted a sign-off by an expired key as %s", principal)
		}
	})

	t.Run("ST-EXT-DS02-T07 A sign-off by a listed GPG key that is revoked or expired is refused, as gpgv reports it", func(t *testing.T) {
		home := filepath.Join(k.dir, "gnupg-not-valid")
		if err := os.Mkdir(home, 0700); err != nil {
			t.Fatal(err)
		}
		gpg := func(input string, args ...string) string {
			t.Helper()
			cmd := exec.Command("gpg", append([]string{"--batch", "--passphrase", "", "--pinentry-mode", "loopback"}, args...)...)
			cmd.Env = append(os.Environ(), "GNUPGHOME="+home)
			cmd.Stdin = strings.NewReader(input)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("gpg %v: %v", args, err)
			}
			return string(out)
		}
		defer func() {
			stop := exec.Command("gpgconf", "--kill", "gpg-agent")
			stop.Env = append(os.Environ(), "GNUPGHOME="+home)
			_ = stop.Run()
		}()
		fingerprintOf := func(name string) string {
			for _, line := range strings.Split(gpg("", "--with-colons", "--list-keys", name+"@example.com"), "\n") {
				if strings.HasPrefix(line, "fpr:") {
					return strings.Split(line, ":")[9]
				}
			}
			t.Fatalf("no fingerprint of %s", name)
			return ""
		}
		// The expired key is made and signs with a clock of 2020; it expired one day later.
		const then = "--faked-system-time=20200101T000000!"
		gpg("", then, "--quick-gen-key", "expired-gpg <expired-gpg@example.com>", "ed25519", "sign", "1d")
		gpg("", "--quick-gen-key", "revoked-gpg <revoked-gpg@example.com>", "ed25519", "sign", "never")
		gpg("", "--quick-gen-key", "valid-gpg <valid-gpg@example.com>", "ed25519", "sign", "never")
		// A primary key that only certifies, with a signing subkey that is revoked after it signed (security review
		// of PR #487, I1): gpgv gives REVKEYSIG with the subkey and VALIDSIG with the primary key.
		gpg("", "--quick-gen-key", "subkey-gpg <subkey-gpg@example.com>", "ed25519", "cert", "never")
		expired, revoked, valid, subkey := fingerprintOf("expired-gpg"), fingerprintOf("revoked-gpg"), fingerprintOf("valid-gpg"), fingerprintOf("subkey-gpg")
		gpg("", "--quick-add-key", subkey, "ed25519", "sign", "never")
		sign := func(fingerprint string, faked ...string) string {
			return gpg(digest, append(faked, "--detach-sign", "--armor", "--local-user", fingerprint)...)
		}
		signOffs := map[string]string{"expired": sign(expired, then), "revoked": sign(revoked), "valid": sign(valid), "subkey": sign(subkey)}
		gpg("key 1\nrevkey\ny\n0\n\ny\nsave\n", "--command-fd", "0", "--edit-key", subkey)
		// The key is revoked after it signed, with the revocation certificate that gpg made for it.
		certificate, err := os.ReadFile(filepath.Join(home, "openpgp-revocs.d", revoked+".rev"))
		if err != nil {
			t.Fatal(err)
		}
		gpg(strings.Replace(string(certificate), ":-----BEGIN", "-----BEGIN", 1), "--import")
		var listed strings.Builder
		for _, name := range []string{"expired", "revoked", "valid", "subkey"} {
			listed.WriteString(name + "-gpg@example.com gpg\n" + gpg("", "--armor", "--export", name+"-gpg@example.com") + "\n")
		}
		signers := mustSigners(t, []byte(listed.String()))
		for name, armored := range signOffs {
			signOff := &FillSignOff{EvidenceDigest: digest, Signature: Signature{KeyType: KeyGPG, Signer: name + "-gpg@example.com", Armored: armored}, SignedAt: time.Now()}
			principal, err := VerifySignOff(signOff, digest, signers)
			if name == "valid" {
				if err != nil || principal != "valid-gpg@example.com" {
					t.Fatalf("refused the sign-off of the valid key beside the others: %q, %v", principal, err)
				}
				continue
			}
			if !errors.Is(err, ErrKeyNotValid) || principal != "" {
				t.Errorf("%s: got %q, %v; want %v", name, principal, err, ErrKeyNotValid)
			}
			if Principal(signOff.Signature, digest, signers) != "" {
				t.Errorf("%s: Principal names the key", name)
			}
		}
	})
}

// changeArmoredBody changes one character in the middle of the armored body
// of a signature.
func changeArmoredBody(t *testing.T, armored string) string {
	t.Helper()
	lines := strings.Split(armored, "\n")
	for i := 2; i < len(lines); i++ {
		line := lines[i]
		if len(line) > 20 && !strings.HasPrefix(line, "-----") && !strings.Contains(line, ":") {
			replacement := byte('A')
			if line[10] == 'A' {
				replacement = 'B'
			}
			lines[i] = line[:10] + string(replacement) + line[11:]
			return strings.Join(lines, "\n")
		}
	}
	t.Fatalf("the signature has no body line to change:\n%s", armored)
	return ""
}

func TestSignatureForm(t *testing.T) {
	t.Run("ST-VO44-C01-T A fill signature is an armored detached SSH or GPG signature of its own key type", func(t *testing.T) {
		k := makeTestKeys(t)
		ssh := k.sshSign(t, k.listedSSH, "message")
		gpg := k.gpgSign(t, k.listedGPG, "message")
		for name, signature := range map[string]Signature{
			"ssh": {KeyType: KeySSH, Armored: ssh},
			"gpg": {KeyType: KeyGPG, Armored: gpg},
		} {
			if err := signature.check(); err != nil {
				t.Errorf("%s: refused %v", name, err)
			}
		}
		body := strings.Join(strings.Split(strings.TrimSpace(ssh), "\n")[1:len(strings.Split(strings.TrimSpace(ssh), "\n"))-1], "")
		binary, err := base64.StdEncoding.DecodeString(body)
		if err != nil {
			t.Fatal(err)
		}
		clear := exec.Command("gpg", "--batch", "--clearsign", "--local-user", k.listedGPG)
		clear.Env = append(os.Environ(), "GNUPGHOME="+k.gnupgHome)
		clear.Stdin = strings.NewReader("message")
		clearSigned, err := clear.Output()
		if err != nil {
			t.Fatal(err)
		}
		cut := strings.TrimSpace(ssh)
		cut = cut[:strings.LastIndex(cut, "-----END")]
		for name, refused := range map[string]struct {
			signature Signature
			field     string
		}{
			"key type SSH in capitals":   {Signature{KeyType: "SSH", Armored: ssh}, "signature.key_type"},
			"key type x509":              {Signature{KeyType: "x509", Armored: ssh}, "signature.key_type"},
			"an empty key type":          {Signature{KeyType: "", Armored: ssh}, "signature.key_type"},
			"ssh with a PGP block":       {Signature{KeyType: KeySSH, Armored: gpg}, "signature.signature"},
			"gpg with an SSH block":      {Signature{KeyType: KeyGPG, Armored: ssh}, "signature.signature"},
			"an SSH signature in binary": {Signature{KeyType: KeySSH, Armored: string(binary)}, "signature.signature"},
			"an SSH block with no END":   {Signature{KeyType: KeySSH, Armored: cut}, "signature.signature"},
			"a clear-signed PGP message": {Signature{KeyType: KeyGPG, Armored: string(clearSigned)}, "signature.signature"},
		} {
			err := refused.signature.check()
			if err == nil || !strings.Contains(err.Error(), refused.field) {
				t.Errorf("%s: got %v, want a fault that names %s", name, err, refused.field)
			}
		}
	})
}
