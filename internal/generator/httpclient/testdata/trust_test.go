package client

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestUntrustedServiceCertificate runs in a module that holds the client of RenderCLI.
func TestUntrustedServiceCertificate(t *testing.T) {
	t.Run("ST-SUP-RC04-OC31-T03 A refused service certificate is ErrUntrusted and not a transport failure, and a server that does not answer is ErrTransport", func(t *testing.T) {
		sent := 0
		trusted := tlsOptions(t, func(w http.ResponseWriter, r *http.Request) { sent++; w.WriteHeader(http.StatusNoContent) })
		digest := [32]byte{1}
		for name, options := range map[string]Options{
			// The test certificate names 127.0.0.1 and not localhost.
			"another host name": {BaseURL: strings.Replace(trusted.BaseURL, "127.0.0.1", "localhost", 1), CAFile: trusted.CAFile},
			// The system CAs do not sign the test certificate.
			"an unknown CA":          {BaseURL: trusted.BaseURL},
			"another leaf digest":    {BaseURL: trusted.BaseURL, CAFile: trusted.CAFile, PeerCertificateSHA256: &digest},
			"an expired certificate": expiredOptions(t, func(w http.ResponseWriter, r *http.Request) { sent++ }),
		} {
			c, err := New(options)
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.Do(context.Background(), http.MethodPost, "/records", nil, nil)
			c.Close()
			if !errors.Is(err, ErrUntrusted) || errors.Is(err, ErrTransport) || errors.Is(err, ErrDeadline) {
				t.Errorf("%s gives %v, want ErrUntrusted and not ErrTransport", name, err)
			}
		}
		if sent != 0 {
			t.Errorf("the server got %d requests over a refused certificate, want none", sent)
		}
		c, err := New(trusted)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if response, err := c.Do(context.Background(), http.MethodPost, "/records", nil, nil); err != nil || response.StatusCode != http.StatusNoContent {
			t.Fatalf("the trusted service answered %d %v, want 204", response.StatusCode, err)
		}
		closed := httptest.NewTLSServer(http.NotFoundHandler())
		closed.Close()
		gone, err := New(Options{BaseURL: closed.URL, CAFile: trusted.CAFile})
		if err != nil {
			t.Fatal(err)
		}
		defer gone.Close()
		if _, err := gone.Do(context.Background(), http.MethodPost, "/records", nil, nil); !errors.Is(err, ErrTransport) || errors.Is(err, ErrUntrusted) {
			t.Errorf("a server that does not answer gives %v, want ErrTransport", err)
		}
	})
}

// expiredOptions gives the options of a TLS test server whose certificate, which signs itself and names 127.0.0.1,
// expired an hour ago; the CA file holds that certificate.
func expiredOptions(t *testing.T, handler http.HandlerFunc) Options {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-48 * time.Hour), NotAfter: time.Now().Add(-time.Hour),
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	ca := filepath.Join(t.TempDir(), "expired.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	return Options{BaseURL: server.URL, CAFile: ca}
}

func TestServiceTLSAlert(t *testing.T) {
	t.Run("ST-SUP-RC04-OC31-T04 A TLS alert from the service, such as for a client certificate that it requires, is ErrUntrusted and not a transport failure", func(t *testing.T) {
		sent := 0
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sent++ }))
		server.Config.ErrorLog = log.New(io.Discard, "", 0)
		server.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert}
		server.StartTLS()
		defer server.Close()
		ca := filepath.Join(t.TempDir(), "ca.pem")
		if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
			t.Fatal(err)
		}
		c, err := New(Options{BaseURL: server.URL, CAFile: ca})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_, err = c.Do(context.Background(), http.MethodPost, "/records", nil, []byte(`{}`))
		if !errors.Is(err, ErrUntrusted) || errors.Is(err, ErrTransport) || errors.Is(err, ErrDeadline) {
			t.Errorf("a service that requires a client certificate gives %v, want ErrUntrusted and not ErrTransport", err)
		}
		if sent != 0 {
			t.Errorf("the service handled %d requests in a refused TLS session, want none", sent)
		}
	})
}
