package command

import (
	"bytes"
	"context"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// keyRequest is one request that the key test server got.
type keyRequest struct{ method, path, key, body string }

// keyFixture is a TLS server that answers each request with the next action of its script: "drop" closes the
// connection with no answer, a number is the status of the answer, and an empty script answers 201 to a POST, 204
// to a DELETE and 200 to the rest. A list, which the apply lookup reads, takes no action and has no items.
type keyFixture struct {
	mu        sync.Mutex
	script    []string
	requests  []keyRequest
	directory string
}

func (f *keyFixture) next(actions ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.script, f.requests = actions, nil
}

func (f *keyFixture) got() []keyRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]keyRequest(nil), f.requests...)
}

func keySetup(t *testing.T, app Application) *keyFixture {
	t.Helper()
	f := &keyFixture{directory: privateDir(t)}
	t.Setenv("TEST_CLI_CONFIG", filepath.Join(f.directory, "config.json"))
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method == "GET" && r.URL.RawQuery != "" {
			// The apply lookup finds no resource.
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"items":[],"size":0}`))
			f.mu.Lock()
			f.requests = append(f.requests, keyRequest{r.Method, r.URL.Path, strings.Join(r.Header.Values("Idempotency-Key"), ","), ""})
			f.mu.Unlock()
			return
		}
		f.mu.Lock()
		f.requests = append(f.requests, keyRequest{r.Method, r.URL.Path, strings.Join(r.Header.Values("Idempotency-Key"), ","), string(body)})
		action := ""
		if len(f.script) > 0 {
			action, f.script = f.script[0], f.script[1:]
		}
		f.mu.Unlock()
		if action == "drop" {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case action == "big":
			// An answer body over the limit of the client.
			w.WriteHeader(201)
			w.Write(bytes.Repeat([]byte(" "), 4<<20+1))
			return
		case action == "302":
			w.Header().Set("Location", "/elsewhere")
			w.WriteHeader(302)
			return
		case action != "":
			w.WriteHeader(map[string]int{"400": 400, "409": 409, "500": 500, "503": 503}[action])
		case r.Method == "POST":
			w.WriteHeader(201)
		case r.Method == "DELETE":
			w.WriteHeader(204)
			return
		}
		name := "first"
		if strings.Contains(string(body), "second") {
			name = "second"
		}
		id := "one"
		if r.Method != "POST" {
			id = r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		}
		w.Write([]byte(`{"id":"` + id + `","name":"` + name + `"}`))
	}))
	t.Cleanup(server.Close)
	ca, token := filepath.Join(f.directory, "ca.pem"), filepath.Join(f.directory, "token")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(token, []byte("test-key-token"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), app, []string{"login", "--url", server.URL, "--token-file", token, "--ca-file", ca}, io.Discard); err != nil {
		t.Fatal(err)
	}
	return f
}

// keyedDocument is an OpenAPI document whose create and patch of records carry the Idempotency-Key header parameter,
// the patch through a parameter of its path item, and whose patch of widgets does not.
const keyedDocument = `{"openapi":"3.0.3","paths":{
 "/records":{"get":{},"post":{"parameters":[{"name":"Idempotency-Key","in":"header"}]}},
 "/records/{recordId}":{"parameters":[{"name":"idempotency-key","in":"header"}],"get":{},"patch":{},"delete":{}},
 "/widgets":{"post":{"parameters":[{"name":"Idempotency-Key","in":"header"}]}},
 "/widgets/{id}":{"patch":{"parameters":[{"name":"Idempotency-Key","in":"query"}]}}}}`

// structuredKey is the form of the key that the CLI makes: a structured string that holds a random UUID.
var structuredKey = regexp.MustCompile(`^"[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}"$`)

func TestWriteIdempotencyKeys(t *testing.T) {
	t.Run("ST-SUP-RC04-OC31-T01 The emitted CLI sends a new key with each write and sends an uncertain write again one time with the same key", func(t *testing.T) {
		// The wait before a second request is 1 s; the test makes it short.
		previous := writeRetryWait
		writeRetryWait = time.Millisecond
		defer func() { writeRetryWait = previous }()
		app := applyDefinition()
		app.Commands = append(app.Commands, Command{Name: []string{"patch", "record"}, Method: "PATCH", Path: "/records/{id}", ID: true, Fields: []Field{{Flag: "name", Key: "name", Type: "string"}}, Success: []int{200}})
		f := keySetup(t, app)
		run := func(app Application, args ...string) error {
			t.Helper()
			var output bytes.Buffer
			return Run(context.Background(), app, args, &output)
		}
		// check runs one command under a script and expects its requests: their methods, and whether a key is on
		// each and the same on all of them.
		keys := map[string]bool{}
		check := func(name string, app Application, script []string, wantErr bool, methods string, keyed bool, args ...string) {
			t.Helper()
			f.next(script...)
			err := run(app, args...)
			if (err != nil) != wantErr {
				t.Errorf("%s: the command gave the error %v, want an error %t", name, err, wantErr)
			}
			got := f.got()
			var gotMethods []string
			for _, r := range got {
				gotMethods = append(gotMethods, r.method)
			}
			if strings.Join(gotMethods, " ") != methods {
				t.Errorf("%s: the server got %q, want %q", name, strings.Join(gotMethods, " "), methods)
				return
			}
			for i, r := range got {
				switch {
				case !keyed && r.key != "":
					t.Errorf("%s: a %s request carried the Idempotency-Key %s", name, r.method, r.key)
				case keyed && !structuredKey.MatchString(r.key):
					t.Errorf("%s: a %s request carried the Idempotency-Key %q, want one structured string of a random UUID", name, r.method, r.key)
				case i > 0 && (r.key != got[0].key || r.body != got[0].body || r.path != got[0].path):
					t.Errorf("%s: the second request differs from the first: %+v and %+v", name, got[0], r)
				}
			}
			if keyed && len(got) > 0 {
				if keys[got[0].key] {
					t.Errorf("%s: the key %s was sent for an earlier write too", name, got[0].key)
				}
				keys[got[0].key] = true
			}
		}
		// A service that does not keep the key: each write gets a new key, and nothing is sent again.
		check("create", app, nil, false, "POST", true, "create", "record", "--name", "first")
		check("create again", app, nil, false, "POST", true, "create", "record", "--name", "first")
		check("create with 500", app, []string{"500"}, true, "POST", true, "create", "record", "--name", "first")
		check("create with no answer", app, []string{"drop"}, true, "POST", true, "create", "record", "--name", "first")
		check("patch with 503", app, []string{"503"}, true, "PATCH", true, "patch", "record", "one", "--name", "first")
		// The key is kept per command and per apply resource, as the OpenAPI document of the service states it.
		keeps, err := IdempotentWritesFrom(app, []byte(keyedDocument))
		if err != nil {
			t.Fatal(err)
		}
		var keyedNames []string
		for _, c := range keeps.Commands {
			if c.IdempotentWrites {
				keyedNames = append(keyedNames, strings.Join(c.Name, " "))
			}
		}
		for _, r := range keeps.Resources {
			if r.IdempotentWrites {
				keyedNames = append(keyedNames, r.Kind)
			}
		}
		if strings.Join(keyedNames, ",") != "create record,patch record,Record" {
			t.Errorf("IdempotentWritesFrom states the key kept for %v, want the create and the patch of a record and the resource Record", keyedNames)
		}
		if _, err := IdempotentWritesFrom(app, []byte(`{"paths":`)); err == nil {
			t.Error("IdempotentWritesFrom read an invalid document")
		}
		wrong := keeps
		wrong.Commands = append([]Command(nil), keeps.Commands...)
		wrong.Commands[1].IdempotentWrites = true
		if err := run(wrong, "get", "record", "one"); err == nil || !strings.Contains(err.Error(), "idempotent write") {
			t.Errorf("a GET command with IdempotentWrites gave %v, want the definition refused", err)
		}
		// A service that keeps the key: an uncertain write is sent again one time with the same key and body.
		check("kept create with no answer", keeps, []string{"drop"}, false, "POST POST", true, "create", "record", "--name", "first")
		check("kept create with 500 two times", keeps, []string{"500", "500"}, true, "POST POST", true, "create", "record", "--name", "first")
		check("kept create with no answer two times", keeps, []string{"drop", "drop"}, true, "POST POST", true, "create", "record", "--name", "first")
		check("kept patch with 503", keeps, []string{"503"}, false, "PATCH PATCH", true, "patch", "record", "one", "--name", "first")
		// A second request that gets 409, as when the first one is still in its transaction, leaves the outcome
		// unknown: the first write can still commit.
		f.next("500", "409")
		if err := run(keeps, "create", "record", "--name", "first"); err == nil || !strings.Contains(err.Error(), "outcome is unknown") || len(f.got()) != 2 {
			t.Errorf("a create whose second request got 409 gave %v after %d requests, want the error that the outcome is unknown after 2", err, len(f.got()))
		}
		// A refused write, a read and a delete are never sent again, and a read and a delete carry no key.
		check("kept create with 400", keeps, []string{"400"}, true, "POST", true, "create", "record", "--name", "first")
		check("kept read with 500", keeps, []string{"500"}, true, "GET", false, "get", "record", "one")
		check("kept read with no answer", keeps, []string{"drop"}, true, "GET", false, "get", "record", "one")
		check("kept delete with 500", keeps, []string{"500"}, true, "DELETE", false, "delete", "record", "one", "--yes")
		// Only a transport failure, an expired request and a 5xx answer leave a write uncertain: a redirect and an
		// answer body over the limit are not sent again (security review of PR 447, I-2).
		check("kept create with a redirect", keeps, []string{"302"}, true, "POST", true, "create", "record", "--name", "first")
		check("kept create with a body over the limit", keeps, []string{"big"}, true, "POST", true, "create", "record", "--name", "first")
		// The apply command: a lookup is a GET with no key; each create and patch has a key of its own.
		input := applyFile(t, filepath.Join(f.directory, "input.yaml"), manifest("Record", "first", "{}")+"---\n"+manifest("Record", "second", "{}"))
		f.next("drop")
		results, output, err := runApplyTest(t, keeps, "apply", "-f", input, "-o", "json")
		var writes []keyRequest
		for _, r := range f.got() {
			if r.method == "GET" && r.key != "" {
				t.Errorf("the apply lookup carried the Idempotency-Key %s", r.key)
			}
			if r.method != "GET" {
				writes = append(writes, r)
			}
		}
		if err != nil || len(results) != 2 || results[0].Status != "created" || results[1].Status != "created" {
			t.Errorf("the apply with one dropped write gave %s %v, want two created", output, err)
		}
		if len(writes) != 3 || writes[0].key != writes[1].key || writes[2].key == writes[0].key || !structuredKey.MatchString(writes[2].key) {
			t.Errorf("the apply writes were %+v, want the first create two times with one key and the second with a new key", writes)
		}
		f.next("drop", "409")
		results, _, err = runApplyTest(t, keeps, "apply", "-f", input, "-o", "json")
		if err == nil || len(results) != 2 || results[0].Status != "unknown" || results[1].Status != "not_attempted" {
			t.Errorf("the apply with a write whose second request got 409 gave %v %v, want unknown and not_attempted", results, err)
		}
		f.next("503", "503")
		results, _, err = runApplyTest(t, keeps, "apply", "-f", input, "-o", "json")
		if err == nil || len(results) != 2 || results[0].Status != "unknown" || results[1].Status != "not_attempted" {
			t.Errorf("the apply with a write that is uncertain two times gave %v %v, want unknown and not_attempted", results, err)
		}
		if got := f.got(); len(got) < 2 || got[len(got)-1].method != "POST" || got[len(got)-2].method != "POST" || got[len(got)-1].key != got[len(got)-2].key {
			t.Errorf("the apply with a write that is uncertain two times sent %+v, want one create two times with one key", got)
		}
		// A resource whose patch does not carry the key in the document is not kept: its create is sent one time.
		widgets := applyFile(t, filepath.Join(f.directory, "widgets.yaml"), manifest("Widget", "first", "{}"))
		f.next("drop")
		results, _, err = runApplyTest(t, keeps, "apply", "-f", widgets, "-o", "json")
		var widgetWrites int
		for _, r := range f.got() {
			if r.method == "POST" {
				widgetWrites++
			}
		}
		if err == nil || len(results) != 1 || results[0].Status != "unknown" || widgetWrites != 1 {
			t.Errorf("the apply of a widget with no answer gave %v %v after %d creates, want unknown after one", results, err, widgetWrites)
		}
	})
}

func TestSecretBodyFields(t *testing.T) {
	t.Run("ST-SUP-RC04-OC32-T03 A secret member has no flag, and --body FILE gives it", func(t *testing.T) {
		app := applyDefinition()
		app.Commands = append(app.Commands, Command{Name: []string{"issue", "key"}, Method: "POST", Path: "/keys", Fields: []Field{{Flag: "label", Key: "label", Type: "string"}},
			BodyFields: []Field{{Key: "pin", Type: "string", Required: true}}, Success: []int{201}})
		f := keySetup(t, app)
		run := func(args ...string) error {
			t.Helper()
			var output bytes.Buffer
			return Run(context.Background(), app, args, &output)
		}
		body := filepath.Join(f.directory, "body.json")
		if err := os.WriteFile(body, []byte(`{"label":"a","pin":"482913"}`), 0600); err != nil {
			t.Fatal(err)
		}
		f.next()
		if err := run("issue", "key", "--body", body); err != nil {
			t.Fatalf("the secret member in --body FILE gave %v", err)
		}
		if got := f.got(); len(got) != 1 || !strings.Contains(got[0].body, `"pin":"482913"`) || !strings.Contains(got[0].body, `"label":"a"`) {
			t.Errorf("the server got %+v, want one request with the pin of the body file and the label", got)
		}
		f.next()
		for _, args := range [][]string{{"issue", "key", "--pin", "482913"}, {"issue", "key", "--label", "a"}} {
			if err := run(args...); err == nil {
				t.Errorf("%q was accepted: a secret member has no flag and is required", args)
			}
		}
		if got := f.got(); len(got) != 0 {
			t.Errorf("a refused command reached the server: %+v", got)
		}
		bad := app
		bad.Commands = append([]Command(nil), app.Commands...)
		bad.Commands[len(bad.Commands)-1].BodyFields = []Field{{Flag: "pin", Key: "pin", Type: "string"}}
		if err := run2(bad, "issue", "key", "--body", body); err == nil || !strings.Contains(err.Error(), "invalid CLI body field") {
			t.Errorf("a body field with a flag gave %v, want the refusal of the definition", err)
		}
	})
}

func run2(app Application, args ...string) error {
	var output bytes.Buffer
	return Run(context.Background(), app, args, &output)
}
