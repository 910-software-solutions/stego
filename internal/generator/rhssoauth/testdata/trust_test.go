package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func ssoFixture(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	document, err := json.Marshal(map[string]any{"keys": []any{map[string]any{"kid": "test", "kty": "RSA", "alg": "RS256", "use": "sig", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB"}}})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "keys.json")
	if err := os.WriteFile(file, document, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JWK_CERT_FILE", file)
	t.Setenv("JWK_CA_FILE", "")
	t.Setenv("STEGO_AUTH_ISSUER", "https://issuer.example/realm")
	t.Setenv("STEGO_AUTH_AUDIENCE", "service")
	t.Setenv("AUTH_ENABLED", "")
	return key
}

func ssoClaims() jwt.MapClaims {
	return jwt.MapClaims{"iss": "https://issuer.example/realm", "aud": "service", "sub": "caller", "iat": time.Now().Add(-time.Minute).Unix(), "exp": time.Now().Add(time.Minute).Unix()}
}

func ssoToken(t *testing.T, key *rsa.PrivateKey, claims jwt.MapClaims, method jwt.SigningMethod) string {
	t.Helper()
	token := jwt.NewWithClaims(method, claims)
	token.Header["kid"] = "test"
	raw, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestTrustClaims(t *testing.T) {
	key := ssoFixture(t)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewJWTHandler()
	if err != nil {
		t.Fatal(err)
	}
	defer h.Stop()
	called := 0
	next := h.Build()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called++; w.WriteHeader(204) }))
	const (
		claimGroup  = "ST-SUP-RC21-OC02-T01 The emitted SSO handler denies a wrong issuer, a wrong audience and an absent expiry"
		denialGroup = "ST-SUP-RC21-OC05-T01 A denial from the emitted SSO handler hides the token and sets no-store and WWW-Authenticate"
		trustGroup  = "ST-SUP-RC21-OC01-T01 The emitted SSO handler verifies the token before it maps any claim"
	)
	for _, group := range []struct {
		name  string
		cases []string
	}{
		{claimGroup, []string{"wrong-issuer", "wrong-audience", "missing-expiry"}},
		{denialGroup, []string{"wrong-issuer", "wrong-audience", "missing-expiry", "missing-issuer", "missing-audience", "missing-subject", "missing-issue-time", "future-issue-time", "expired", "wrong-signature", "wrong-algorithm", "duplicate-header", "oversize-token"}},
		{trustGroup, []string{"valid", "missing-issuer", "missing-audience", "missing-subject", "missing-issue-time", "future-issue-time", "expired", "wrong-signature", "wrong-algorithm", "duplicate-header", "oversize-token"}},
	} {
		t.Run(group.name, func(t *testing.T) {
			for _, name := range group.cases {
				t.Run(name, func(t *testing.T) {
					claims := ssoClaims()
					method := jwt.SigningMethodRS256
					signingKey := key
					switch name {
					case "wrong-issuer":
						claims["iss"] = "https://other.example/realm"
					case "wrong-audience":
						claims["aud"] = "other-service"
					case "missing-expiry":
						delete(claims, "exp")
					case "missing-issuer":
						delete(claims, "iss")
					case "missing-audience":
						delete(claims, "aud")
					case "missing-subject":
						delete(claims, "sub")
					case "missing-issue-time":
						delete(claims, "iat")
					case "future-issue-time":
						claims["iat"] = time.Now().Add(time.Hour).Unix()
					case "expired":
						claims["exp"] = time.Now().Add(-time.Second).Unix()
					case "wrong-signature":
						signingKey = other
					case "wrong-algorithm":
						method = jwt.SigningMethodRS512
					}
					raw := ssoToken(t, signingKey, claims, method)
					if name == "oversize-token" {
						raw = strings.Repeat("x", maxTokenBytes+1)
					}
					r := httptest.NewRequest("GET", "/private", nil)
					r.Header.Set("Authorization", "Bearer "+raw)
					if name == "duplicate-header" {
						r.Header.Add("Authorization", "Bearer "+raw)
					}
					w := httptest.NewRecorder()
					next.ServeHTTP(w, r)
					want := 401
					if name == "valid" {
						want = 204
					}
					if w.Code != want {
						t.Fatalf("got %d, want %d", w.Code, want)
					}
					if want == 401 && (strings.Contains(w.Body.String(), raw) || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("WWW-Authenticate") == "") {
						t.Fatal("unsafe authentication error response")
					}
				})
			}
			// The trust group runs last, so this count covers every group.
			if group.name != trustGroup {
				return
			}
			if called != 1 {
				t.Fatal("a denied request reached the application")
			}
		})
	}
	t.Run("ST-SUP-RC21-OC03-T01 The emitted SSO handler lets only an exact public path through without a token", func(t *testing.T) {
		for _, p := range []string{"/healthcheck", "/metrics", "/openapi", "/healthcheck/private", "/metrics/private"} {
			w := httptest.NewRecorder()
			next.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
			want := 204
			if strings.HasSuffix(p, "/private") {
				want = 401
			}
			if w.Code != want {
				t.Fatal("public path match was not exact", p, w.Code)
			}
		}
	})
	t.Run("ST-SUP-RC21-OC04-T01 After Stop, the emitted SSO handler denies every request and a repeat Stop is safe", func(t *testing.T) {
		h.Stop()
		h.Stop()
		r := httptest.NewRequest("GET", "/private", nil)
		r.Header.Set("Authorization", "Bearer "+ssoToken(t, key, ssoClaims(), jwt.SigningMethodRS256))
		w := httptest.NewRecorder()
		next.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatal("stopped handler accepted token")
		}
	})
}

func TestSSOPayloadMapping(t *testing.T) {
	key := ssoFixture(t)
	h, err := NewJWTHandler()
	if err != nil {
		t.Fatal(err)
	}
	defer h.Stop()
	for _, group := range []string{"ST-SUP-RC21-OC06-T01 The emitted SSO handler fills each payload field from its declared claim order", "ST-SUP-RC21-OC07-T01 The identity from the emitted SSO handler holds an empty Role"} {
		t.Run(group, func(t *testing.T) {
			for _, mode := range []string{"sso", "oidc", "fallback"} {
				claims := ssoClaims()
				claims["email"] = "person@example.com"
				claims["clientId"] = "service-client"
				username, first, last := "caller", "Example", "Person"
				switch mode {
				case "sso":
					claims["username"] = "account"
					claims["preferred_username"] = "ignored"
					claims["first_name"] = first
					claims["last_name"] = last
					username = "account"
				case "oidc":
					claims["preferred_username"] = "preferred"
					claims["given_name"] = first
					claims["family_name"] = last
					username = "preferred"
				case "fallback":
					claims["name"] = first + " " + last
				}
				called := false
				next := h.Build()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					called = true
					p := GetAuthPayload(r)
					id := IdentityFromContext(r.Context())
					if p.Username != username || p.FirstName != first || p.LastName != last || p.Email != "person@example.com" || p.ClientID != "service-client" || p.Issuer != "https://issuer.example/realm" {
						t.Fatal("SSO mapping changed", mode)
					}
					if id.UserID != "caller" || id.Username != username || id.Role != "" || id.ExpiresAt.IsZero() || id.Attributes["client_id"] != p.ClientID || GetUsernameFromContext(r.Context()) != username || !TokenFromContext(r.Context()).Valid {
						t.Fatal("verified context mapping changed")
					}
					w.WriteHeader(204)
				}))
				r := httptest.NewRequest("GET", "/private", nil)
				r.Header.Set("Authorization", "Bearer "+ssoToken(t, key, claims, jwt.SigningMethodRS256))
				next.ServeHTTP(httptest.NewRecorder(), r)
				if !called {
					t.Fatal("valid payload rejected")
				}
			}
		})
	}
}

func TestSSOIdentitySubject(t *testing.T) {
	t.Run("ST-SUP-RC21-OC08-T01 The emitted SSO identity holds the verified issuer and subject, not the username", func(t *testing.T) {
		key := ssoFixture(t)
		h, err := NewJWTHandler()
		if err != nil {
			t.Fatal(err)
		}
		defer h.Stop()
		// The username differs from the subject. Authorization keys on the subject,
		// so the identity must carry the subject, not the username.
		claims := ssoClaims()
		claims["username"] = "account"
		called := false
		next := h.Build()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			id := IdentityFromContext(r.Context())
			if id.UserID != "caller" {
				t.Fatal("identity does not hold the verified subject")
			}
			if id.Issuer != "https://issuer.example/realm" {
				t.Fatal("identity does not hold the verified issuer")
			}
			if id.Username != "account" {
				t.Fatal("identity lost the username")
			}
			w.WriteHeader(204)
		}))
		r := httptest.NewRequest("GET", "/private", nil)
		r.Header.Set("Authorization", "Bearer "+ssoToken(t, key, claims, jwt.SigningMethodRS256))
		next.ServeHTTP(httptest.NewRecorder(), r)
		if !called {
			t.Fatal("valid token rejected")
		}
	})
}

func TestSSOStartupRequiresTrust(t *testing.T) {
	_ = ssoFixture(t)
	for _, group := range []struct {
		name     string
		settings []struct{ name, value string }
	}{
		{"ST-SUP-RC21-OC09-T01 The emitted SSO handler refuses to start without an issuer, an audience or a key", []struct{ name, value string }{{"STEGO_AUTH_ISSUER", ""}, {"STEGO_AUTH_AUDIENCE", ""}, {"JWK_CERT_FILE", "/missing-keys"}}},
		{"ST-SUP-RC21-OC10-T01 The emitted SSO handler refuses to start when AUTH_ENABLED is not empty or true", []struct{ name, value string }{{"AUTH_ENABLED", "false"}, {"AUTH_ENABLED", "FALSE"}, {"AUTH_ENABLED", "invalid"}}},
	} {
		t.Run(group.name, func(t *testing.T) {
			for _, setting := range group.settings {
				t.Run(setting.name+setting.value, func(t *testing.T) {
					t.Setenv(setting.name, setting.value)
					if h, err := NewJWTHandler(); err == nil {
						h.Stop()
						t.Fatal("unsafe startup accepted")
					}
				})
			}
		})
	}
}

func TestSSOStartupContext(t *testing.T) {
	t.Run("ST-SUP-RC21-OC12-T01 The emitted SSO handler refuses a nil or canceled startup context", func(t *testing.T) {
		ssoFixture(t)
		if h, err := NewJWTHandlerWithContext(nil); err == nil {
			h.Stop()
			t.Fatal("nil startup context accepted")
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if h, err := NewJWTHandlerWithContext(ctx); err == nil {
			h.Stop()
			t.Fatal("canceled startup context accepted")
		}
		h, err := NewJWTHandlerWithContext(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		h.Stop()
	})
}

func TestSSODeclaredTrustWins(t *testing.T) {
	t.Run("ST-SUP-RC21-OC09-T02 The emitted SSO handler takes its issuer and audience through the declared-value rule, which refuses another value", func(t *testing.T) {
		_ = ssoFixture(t)
		t.Setenv("STEGO_AUTH_ISSUER", "https://other.example/realm")
		if _, err := declaredValue("STEGO_AUTH_ISSUER", "https://issuer.example/realm"); err == nil || !strings.Contains(err.Error(), "STEGO_AUTH_ISSUER") {
			t.Fatalf("an issuer override gives %v, want a refusal that names the variable", err)
		}
		source, err := os.ReadFile("sso.go")
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"STEGO_AUTH_ISSUER", "STEGO_AUTH_AUDIENCE"} {
			if !strings.Contains(string(source), `declaredValue("`+name+`"`) || strings.Contains(string(source), `envOrDefault("`+name+`"`) {
				t.Errorf("the emitted SSO handler does not take %s through declaredValue", name)
			}
		}
		// With no declared issuer the variable is taken, so the handler starts on the issuer of the fixture.
		t.Setenv("STEGO_AUTH_ISSUER", "https://issuer.example/realm")
		h, err := NewJWTHandler()
		if err != nil {
			t.Fatal(err)
		}
		h.Stop()
	})
}

func TestSSOOneKeyRefreshTask(t *testing.T) {
	t.Run("ST-SUP-RC21-OC11-T03 The service runs one key refresh task", func(t *testing.T) {
		key := ssoFixture(t)
		document, err := json.Marshal(map[string]any{"keys": []any{map[string]any{"kid": "test", "kty": "RSA", "alg": "RS256", "use": "sig", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB"}}})
		if err != nil {
			t.Fatal(err)
		}
		var requests atomic.Int64
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(document)
		}))
		defer server.Close()
		ca := filepath.Join(t.TempDir(), "ca.pem")
		if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("JWK_CERT_FILE", "")
		t.Setenv("JWK_CERT_URL", server.URL+"/keys")
		t.Setenv("JWK_CA_FILE", ca)
		h, err := NewJWTHandler()
		if err != nil {
			t.Fatal(err)
		}
		defer h.Stop()
		if got := requests.Load(); got != 1 {
			t.Fatalf("startup made %d key requests, want one", got)
		}
		// The refresh interval of the common verifier is 30 seconds: a clock the test moves stands in for it, so the
		// test needs no wall-clock wait. Each interval, ten requests with a key the cache does not hold ask for a
		// refresh; the one task gives one key request per interval, not one per request or two per interval.
		clock := time.Now()
		h.verifier.now = func() time.Time { return clock }
		unknown := ssoToken(t, key, ssoClaims(), jwt.SigningMethodRS256)
		parts := strings.SplitN(unknown, ".", 2)
		header, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "unknown"})
		unknown = base64.RawURLEncoding.EncodeToString(header) + "." + parts[1]
		next := h.Build()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
		for interval := 1; interval <= 3; interval++ {
			clock = clock.Add(31 * time.Second)
			for range 10 {
				request := httptest.NewRequest(http.MethodGet, "/api/v1/records", nil)
				request.Header.Set("Authorization", "Bearer "+unknown)
				next.ServeHTTP(httptest.NewRecorder(), request)
			}
			if got := requests.Load(); got != int64(1+interval) {
				t.Fatalf("after interval %d the server got %d key requests, want %d: one per interval", interval, got, 1+interval)
			}
		}
	})
}
