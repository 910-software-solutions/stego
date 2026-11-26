package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func keyDocument(t *testing.T, id string, key *rsa.PrivateKey) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{"keys": []any{map[string]any{"kid": id, "kty": "RSA", "alg": "RS256", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB"}}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestKeySourceTLSRotationAndFailure(t *testing.T) {
	first, second := keyForTest(t), keyForTest(t)
	var document atomic.Value
	document.Store(keyDocument(t, "first", first))
	var requests atomic.Int32
	var fail atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if fail.Load() {
			http.Error(w, "private-provider-error", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(document.Load().([]byte))
	}))
	defer server.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	config := JWKSConfig{Trust: Config{Issuer: "https://issuer.example", Audience: "example-api"}, URL: server.URL}
	t.Run("ST-SUP-RC11-OC13-T01 The emitted key source refuses an untrusted TLS certificate before it sends a request", func(t *testing.T) {
		if v, err := NewJWKSVerifier(context.Background(), config); err == nil {
			v.Stop()
			t.Fatal("untrusted TLS certificate accepted")
		}
		if requests.Load() != 0 {
			t.Fatal("untrusted server received an HTTP request")
		}
	})
	config.CAFile = ca
	v, err := NewJWKSVerifier(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Stop()
	now := time.Now()
	v.now = func() time.Time { return now }
	a := signed(t, validClaims(), jwt.SigningMethodRS256, first, map[string]any{"kid": "first"})
	b := signed(t, validClaims(), jwt.SigningMethodRS256, second, map[string]any{"kid": "second"})
	if _, err := v.Verify(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	initial := requests.Load()
	t.Run("ST-SUP-RC11-OC15-T01 The emitted key source makes one request for each refresh and holds the cooldown", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			if _, err := v.Verify(context.Background(), b); err == nil {
				t.Fatal("unknown key accepted")
			}
		}
		if requests.Load() != initial {
			t.Fatal("unknown key bypassed the refresh interval")
		}
		now = now.Add(31 * time.Second)
		document.Store(keyDocument(t, "second", second))
		if _, err := v.Verify(context.Background(), b); err != nil {
			t.Fatal("rotated key rejected", err)
		}
		if _, err := v.Verify(context.Background(), a); err == nil {
			t.Fatal("removed key accepted")
		}
		if requests.Load() != initial+1 {
			t.Fatal("rotation did not make one bounded request")
		}
	})
	t.Run("ST-SUP-RC11-OC17-T01 The emitted key source denies a removed key at once", func(t *testing.T) {
		if _, err := v.Verify(context.Background(), a); err == nil {
			t.Fatal("removed key accepted")
		}
	})
	t.Run("ST-SUP-RC11-OC16-T01 The emitted key source denies every token after the outage window until a load succeeds", func(t *testing.T) {
		fail.Store(true)
		now = now.Add(6 * time.Minute)
		if _, err := v.Verify(context.Background(), b); err != nil {
			t.Fatal("known key failed inside the outage limit", err)
		}
		now = now.Add(10 * time.Minute)
		if _, err := v.Verify(context.Background(), b); err == nil {
			t.Fatal("expired cache survived provider failure")
		}
		fail.Store(false)
		now = now.Add(31 * time.Second)
		if _, err := v.Verify(context.Background(), b); err != nil {
			t.Fatal("provider recovery failed", err)
		}
	})
	t.Run("A canceled request does not contact the provider", func(t *testing.T) {
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		count := requests.Load()
		if _, err := v.Verify(canceled, b); err == nil {
			t.Fatal("canceled request accepted")
		}
		if requests.Load() != count {
			t.Fatal("canceled request contacted provider")
		}
	})
	t.Run("ST-SUP-RC11-OC18-T01 After Stop, the emitted key source denies every verification and a repeat Stop is safe", func(t *testing.T) {
		v.Stop()
		v.Stop()
		if _, err := v.Verify(context.Background(), b); err == nil {
			t.Fatal("stopped verifier accepted request")
		}
	})
}

func TestKeySourceInputLimitsAndRedirect(t *testing.T) {
	key := keyForTest(t)
	file := filepath.Join(t.TempDir(), "keys.json")
	config := JWKSConfig{Trust: Config{Issuer: "https://issuer.example", Audience: "example-api"}, File: file}
	t.Run("An invalid key file is refused", func(t *testing.T) {
		for _, data := range [][]byte{nil, []byte(`{"keys":[]}`), []byte(strings.Repeat(" ", maxKeyFileBytes+1)), []byte(`{"keys":[],"keys":[]}`)} {
			if err := os.WriteFile(file, data, 0600); err != nil {
				t.Fatal(err)
			}
			if v, err := NewJWKSVerifier(context.Background(), config); err == nil {
				v.Stop()
				t.Fatal("invalid key file accepted")
			}
		}
	})
	if err := os.WriteFile(file, keyDocument(t, "key", key), 0600); err != nil {
		t.Fatal(err)
	}
	v, err := NewJWKSVerifier(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Stop()
	t.Run("ST-SUP-RC11-OC05-T02 The emitted key source refuses an oversize token", func(t *testing.T) {
		if _, err := v.Verify(context.Background(), strings.Repeat("x", maxTokenBytes+1)); err == nil {
			t.Fatal("oversize token accepted")
		}
	})
	t.Run("A missing audience is refused", func(t *testing.T) {
		config.Trust.Audience = ""
		if bad, err := NewJWKSVerifier(context.Background(), config); err == nil {
			bad.Stop()
			t.Fatal("missing audience accepted")
		}
	})
	var targetRequests atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetRequests.Add(1) }))
	defer target.Close()
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer redirect.Close()
	t.Run("ST-SUP-RC11-OC13-T02 The emitted key source refuses a redirect and a plaintext URL", func(t *testing.T) {
		ca := filepath.Join(t.TempDir(), "ca.pem")
		if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: redirect.Certificate().Raw}), 0600); err != nil {
			t.Fatal(err)
		}
		config = JWKSConfig{Trust: Config{Issuer: "https://issuer.example", Audience: "example-api"}, URL: redirect.URL, CAFile: ca}
		if bad, err := NewJWKSVerifier(context.Background(), config); err == nil {
			bad.Stop()
			t.Fatal("redirect accepted")
		}
		if targetRequests.Load() != 0 {
			t.Fatal("key loader followed redirect")
		}
		config.URL = "http://example.com/keys"
		if bad, err := NewJWKSVerifier(context.Background(), config); err == nil {
			bad.Stop()
			t.Fatal("plaintext key source accepted")
		}
	})
}

func TestKeySourceStopCancelsRefresh(t *testing.T) {
	t.Run("ST-SUP-RC11-OC25-T01 Stop on the emitted key source cancels a retrieval that is already running", func(t *testing.T) {
		key := keyForTest(t)
		data := keyDocument(t, "key", key)
		var wait atomic.Bool
		entered := make(chan struct{})
		var once sync.Once
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if wait.Load() {
				once.Do(func() { close(entered) })
				<-r.Context().Done()
				return
			}
			_, _ = w.Write(data)
		}))
		defer server.Close()
		ca := filepath.Join(t.TempDir(), "ca.pem")
		if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
			t.Fatal(err)
		}
		v, err := NewJWKSVerifier(context.Background(), JWKSConfig{Trust: Config{Issuer: "https://issuer.example", Audience: "example-api"}, URL: server.URL, CAFile: ca})
		if err != nil {
			t.Fatal(err)
		}
		defer v.Stop()
		future := time.Now().Add(6 * time.Minute)
		v.now = func() time.Time { return future }
		raw := signed(t, validClaims(), jwt.SigningMethodRS256, key, map[string]any{"kid": "key"})
		wait.Store(true)
		done := make(chan error, 1)
		go func() { _, err := v.Verify(context.Background(), raw); done <- err }()
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("refresh did not start")
		}
		v.Stop()
		v.Stop()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("stopped refresh accepted token")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("shutdown did not cancel key retrieval")
		}
	})
}

func TestEmptyKeySourceFailsClosed(t *testing.T) {
	t.Run("ST-SUP-RC11-OC26-T01 A zero emitted key source denies every token and a repeat Stop is safe", func(t *testing.T) {
		var v JWKSVerifier
		if _, err := v.Verify(context.Background(), "token"); err == nil {
			t.Fatal("empty verifier accepted token")
		}
		v.Stop()
		v.Stop()
	})
}

func TestRemoteKeyInputLimits(t *testing.T) {
	t.Run("ST-SUP-RC11-OC19-T01 A startup error from the emitted key source holds no provider detail", func(t *testing.T) {
		for _, mode := range []string{"body", "headers", "status"} {
			t.Run(mode, func(t *testing.T) {
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch mode {
					case "body":
						_, _ = w.Write([]byte(strings.Repeat("x", maxKeyFileBytes+1)))
					case "headers":
						w.Header().Set("X-Excess", strings.Repeat("x", 16384))
						w.WriteHeader(200)
					case "status":
						http.Error(w, "private-provider-error", 503)
					}
				}))
				defer server.Close()
				ca := filepath.Join(t.TempDir(), "ca.pem")
				if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
					t.Fatal(err)
				}
				v, err := NewJWKSVerifier(context.Background(), JWKSConfig{Trust: Config{Issuer: "https://issuer.example", Audience: "example-api"}, URL: server.URL, CAFile: ca})
				if err == nil {
					v.Stop()
					t.Fatal("invalid remote input accepted")
				}
				if strings.Contains(err.Error(), server.URL) || strings.Contains(err.Error(), "private-provider-error") {
					t.Fatal("provider details reached the startup error")
				}
			})
		}
	})
}

// outboundAddress gives an IP address of this host that is not loopback, which Go's proxy rule does not exempt, or
// "" when the host has none.
func outboundAddress() string {
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, address := range addresses {
		if prefix, ok := address.(*net.IPNet); ok && !prefix.IP.IsLoopback() && prefix.IP.To4() != nil && !prefix.IP.IsLinkLocalUnicast() {
			return prefix.IP.String()
		}
	}
	return ""
}

// proxyChild names the environment variable under which the test runs again as a child process: Go reads the proxy
// settings of the environment once per process, so the child takes them from its start, whatever test of the parent
// read them first.
const proxyChild = "STEGO_TEST_PROXY_CHILD"

// proxyChildRun selects the test in the child process, and proxyChildPass is the line that its subtest prints when it
// passes under -test.v.
var (
	proxyChildRun  = "^TestKeySourceSendsNoRequestThroughAProxy$"
	proxyChildPass = "--- PASS: TestKeySourceSendsNoRequestThroughAProxy/ST-SUP-RC11-OC14-T01_The_key_source_fetches_keys_directly_while_proxy_settings_are_set"
)

// hasLine tells whether a line of the output, without its indent and its time, is the given line.
func hasLine(output, want string) bool {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == want || strings.HasPrefix(line, want+" (") {
			return true
		}
	}
	return false
}

func TestKeySourceSendsNoRequestThroughAProxy(t *testing.T) {
	t.Run("ST-SUP-RC11-OC14-T01 The key source fetches keys directly while proxy settings are set", func(t *testing.T) {
		if os.Getenv(proxyChild) == "" {
			var proxied atomic.Int32
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				proxied.Add(1)
				http.Error(w, "the recording proxy forwards nothing", http.StatusBadGateway)
			}))
			defer proxy.Close()
			child := exec.Command(os.Args[0], "-test.run", proxyChildRun, "-test.count=1", "-test.v")
			child.Env = append(os.Environ(), proxyChild+"=1", "HTTP_PROXY="+proxy.URL, "HTTPS_PROXY="+proxy.URL, "http_proxy="+proxy.URL,
				"https_proxy="+proxy.URL, "NO_PROXY=", "no_proxy=")
			output, err := child.CombinedOutput()
			if err != nil || proxied.Load() != 0 {
				t.Fatalf("the key source with proxy settings: %v, the proxy got %d requests\n%s", err, proxied.Load(), output)
			}
			// A child that matches no test also prints PASS, so the exact line of this subtest must be in its output.
			if !hasLine(string(output), proxyChildPass) || strings.Contains(string(output), "no tests to run") {
				t.Fatalf("the child did not pass the subtest %q:\n%s", proxyChildPass, output)
			}
			return
		}
		host := outboundAddress()
		if host == "" {
			t.Fatal("the host has no address that is not loopback, which the test needs: Go exempts a loopback host from every proxy")
		}
		key := keyForTest(t)
		var direct atomic.Int32
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			direct.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(keyDocument(t, "first", key))
		}))
		listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
		if err != nil {
			t.Fatal(err)
		}
		server.Listener = listener
		// The certificate of httptest names 127.0.0.1 and ::1 only: the server takes one for the address of the host,
		// which the key source trusts through its CA file.
		caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(1), IPAddresses: []net.IP{net.ParseIP(host)}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		der, err := x509.CreateCertificate(rand.Reader, template, template, &caKey.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: caKey}}}
		server.StartTLS()
		defer server.Close()
		ca := filepath.Join(t.TempDir(), "ca.pem")
		if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
			t.Fatal(err)
		}
		config := JWKSConfig{Trust: Config{Issuer: "https://issuer.example", Audience: "example-api"}, URL: server.URL + "/keys", CAFile: ca}
		v, err := NewJWKSVerifier(context.Background(), config)
		if err != nil {
			t.Fatalf("the key source did not fetch the keys directly: %v", err)
		}
		defer v.Stop()
		// A refresh after the cooldown fetches again, directly.
		now := time.Now().Add(time.Hour)
		v.now = func() time.Time { return now }
		_, _ = v.Verify(context.Background(), signed(t, validClaims(), jwt.SigningMethodRS256, keyForTest(t), map[string]any{"kid": "unknown"}))
		if direct.Load() != 2 {
			t.Fatalf("the key server got %d direct requests, want 2", direct.Load())
		}
	})
}
