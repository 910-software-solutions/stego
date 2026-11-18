package service

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jsell-rh/stego/registry/runtime/command"
	"github.com/jsell-rh/stego/registry/runtime/sealed"
)

func TestKeyRing(t *testing.T) {
	t.Run("ST-SUP-RP14-LC12-T01 The key ring of the secret fields is read from its file, and a missing or bad one is refused with no key", func(t *testing.T) {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			t.Fatal(err)
		}
		// A throwaway key, in a file of the test folder that the test removes.
		text := base64.StdEncoding.EncodeToString(key)
		good := writeFile(t, "key-ring", []byte("# rotation\n3 "+text+"\n"))
		t.Setenv(KeyRingFileSetting, good)
		ring, err := ReadKeyRing()
		if err != nil || ring.First() != 3 {
			t.Fatalf("ReadKeyRing gave %v", err)
		}
		bad := map[string]string{
			"no setting":           "",
			"a relative path":      "key-ring",
			"a missing file":       good + ".missing",
			"no key":               writeFile(t, "empty", []byte("# none\n")),
			"a short key":          writeFile(t, "short", []byte("1 "+base64.StdEncoding.EncodeToString(key[:16])+"\n")),
			"a key ID twice":       writeFile(t, "twice", []byte("1 "+text+"\n1 "+text+"\n")),
			"a file others read":   writeFile(t, "open", []byte("1 "+text+"\n")),
			"a line of other form": writeFile(t, "form", []byte("key="+text+"\n")),
		}
		if err := os.Chmod(bad["a file others read"], 0o644); err != nil {
			t.Fatal(err)
		}
		for name, path := range bad {
			t.Setenv(KeyRingFileSetting, path)
			_, err := ReadKeyRing()
			var setting *SettingError
			if !errors.As(err, &setting) || setting.Component != "command" || setting.Variable != KeyRingFileSetting ||
				strings.Contains(err.Error(), text) || strings.Contains(err.Error(), base64.StdEncoding.EncodeToString(key[:16])) {
				t.Errorf("%s gave %v, want a setting error of command that names %s and no key", name, err, KeyRingFileSetting)
			}
			if StartStage(err) != "service.start.setting.command."+KeyRingFileSetting {
				t.Errorf("%s gave the stage %s", name, StartStage(err))
			}
		}
	})
}

func TestKeyRingNeeded(t *testing.T) {
	t.Run("ST-SUP-RP14-LC12-T03 A service with a compare-only secret or a pii field and no key ring does not start", func(t *testing.T) {
		ring, err := sealed.ParseRing([]byte("1 " + base64.StdEncoding.EncodeToString(make([]byte, 32)) + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		for name, p := range map[string]Parts{"a secret or pii field": {KeyRing: true}, "a readable secret": {Reseal: []command.ResealColumn{{}}}, "an open seal": {Seal: []command.SealColumn{{}}}} {
			err := ringNeeded(p, nil)
			var setting *SettingError
			if !errors.As(err, &setting) || setting.Variable != KeyRingFileSetting || StartStage(err) != "service.start.setting.command."+KeyRingFileSetting {
				t.Errorf("%s with no key ring gives %v, want a setting error of %s", name, err, KeyRingFileSetting)
			}
			if err := ringNeeded(p, ring); err != nil {
				t.Errorf("%s with a key ring gives %v", name, err)
			}
		}
		if err := ringNeeded(Parts{}, nil); err != nil {
			t.Errorf("a service with no secret and no pii field and no key ring gives %v", err)
		}
	})
}
