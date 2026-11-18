package client

import (
	"context"
	"net/http"
	"testing"
)

// TestIdempotencyKeyHeader runs in a module that holds the client of Render, of RenderProxy or of RenderCLI;
// keyClient, which the harness writes, is true for the clients of RenderProxy and RenderCLI, which send the key.
func TestIdempotencyKeyHeader(t *testing.T) {
	t.Run("ST-SUP-RC02-OC07-T02 Only the clients of the API proxy and of the CLI send Idempotency-Key; the other HTTPS clients refuse it", func(t *testing.T) { checkIdempotencyKey(t) })
}

func checkIdempotencyKey(t *testing.T) {
	var got []string
	c, err := New(tlsOptions(t, func(w http.ResponseWriter, r *http.Request) { got = r.Header.Values("Idempotency-Key") }))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = c.Do(context.Background(), "POST", "/records", http.Header{"Idempotency-Key": {"place-order-1"}}, nil)
	if keyClient && (err != nil || len(got) != 1 || got[0] != "place-order-1") {
		t.Fatalf("the client of the proxy or the CLI did not send Idempotency-Key: %v %v", err, got)
	}
	if !keyClient && (err == nil || got != nil) {
		t.Fatalf("a client that is not the proxy or the CLI client sent Idempotency-Key")
	}
	if _, err = c.Do(context.Background(), "POST", "/records", http.Header{"X-Idempotency-Key": {"k"}}, nil); err == nil {
		t.Fatal("a client sent X-Idempotency-Key")
	}
}
