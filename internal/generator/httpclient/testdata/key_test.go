package client

import (
	"context"
	"net/http"
	"testing"
)

// TestIdempotencyKeyHeader runs in a module that holds either the client of Render or that of RenderProxy;
// proxyClient, which the harness writes, tells which.
func TestIdempotencyKeyHeader(t *testing.T) {
	var got []string
	c, err := New(tlsOptions(t, func(w http.ResponseWriter, r *http.Request) { got = r.Header.Values("Idempotency-Key") }))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = c.Do(context.Background(), "POST", "/records", http.Header{"Idempotency-Key": {"place-order-1"}}, nil)
	if proxyClient && (err != nil || len(got) != 1 || got[0] != "place-order-1") {
		t.Fatalf("the client of the proxy did not send Idempotency-Key: %v %v", err, got)
	}
	if !proxyClient && (err == nil || got != nil) {
		t.Fatalf("a client that is not the proxy client sent Idempotency-Key")
	}
	if _, err = c.Do(context.Background(), "POST", "/records", http.Header{"X-Idempotency-Key": {"k"}}, nil); err == nil {
		t.Fatal("a client sent X-Idempotency-Key")
	}
}
