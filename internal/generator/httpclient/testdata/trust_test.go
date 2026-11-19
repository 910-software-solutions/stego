package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
			"an unknown CA":       {BaseURL: trusted.BaseURL},
			"another leaf digest": {BaseURL: trusted.BaseURL, CAFile: trusted.CAFile, PeerCertificateSHA256: &digest},
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
