package command

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	client "example.com/cli-test/out/cli/client"
)

func applyDefinition() Application {
	app := definition()
	fields := []Field{{Flag: "name", Key: "name", Type: "string", Required: true}, {Flag: "count", Key: "count", Type: "integer"}, {Flag: "tags", Key: "tags", Type: "string-list", Nullable: true}}
	patch := append([]Field(nil), fields...)
	patch[0].Required = false
	for _, kind := range []string{"Record", "Widget"} {
		app.Resources = append(app.Resources, ApplyResource{Kind: kind, APIVersion: "example/v1", Path: "/" + strings.ToLower(kind) + "s", CreateFields: fields, PatchFields: patch})
	}
	return app
}

type applyFixture struct {
	mu            sync.Mutex
	calls, writes int
	// pages counts the list pages that the server answered.
	pages     int
	mode      string
	rows      map[string]map[string]any
	order     []string
	directory string
	// last is the body of the last write.
	last map[string]any
	// lag hides a created row from the list until a list request names its revocation in Authz-Revocation-After, as
	// a list hides a row until its owner relationship is written; each write then answers Authz-Revocation-Location.
	// unique answers 409 to a create of a name that a row has. hide hides a created row from that many lists.
	// conditional gives each row a generation, and a patch needs If-Match with it.
	lag, unique, conditional bool
	hide                     int
	revocations              map[string]string
	settled                  map[string]bool
	hidden                   map[string]int
	// after, prefer and ifMatch are the Authz-Revocation-After and Prefer of each list request and the If-Match of
	// each patch.
	after, prefer, ifMatch []string
	conflicts              int
	// state is the Authz-Revocation that a list answers to a request with Authz-Revocation-After, or none.
	state string
	// afterList runs, under the lock, after each list answer.
	afterList func()
}

func applySetup(t *testing.T) *applyFixture {
	t.Helper()
	f := &applyFixture{directory: privateDir(t), rows: map[string]map[string]any{}, revocations: map[string]string{}, hidden: map[string]int{}, settled: map[string]bool{}}
	t.Setenv("TEST_CLI_CONFIG", filepath.Join(f.directory, "config.json"))
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls++
		if r.Header.Get("Authorization") != "Bearer test-apply-token" {
			t.Error("apply token changed")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			if f.mode == "lookup-failure" && strings.Contains(r.URL.RawQuery, "second") {
				w.WriteHeader(403)
				fmt.Fprint(w, `{"private":"do-not-print"}`)
				return
			}
			if r.URL.RawQuery == "" {
				if row, ok := f.rows[r.URL.Path]; ok {
					json.NewEncoder(w).Encode(row)
				} else {
					w.WriteHeader(404)
				}
				return
			}
			// The list of a generated service refuses a parameter it does not read, and it can give a page with fewer
			// values than its size, or none, with a next cursor. So this list answers one value a page, after an
			// empty first page.
			query := r.URL.Query()
			for name := range query {
				if name != "pageSize" && name != "search" && name != "cursor" {
					w.WriteHeader(400)
					return
				}
			}
			if query.Get("pageSize") != "2" {
				t.Error("lookup page size changed", query)
			}
			switch f.mode {
			case "null-next":
				fmt.Fprint(w, `{"items":[],"size":0,"next":null}`)
				return
			case "wrong-size":
				fmt.Fprint(w, `{"items":[],"size":1}`)
				return
			case "endless":
				f.pages++
				fmt.Fprint(w, `{"items":[],"size":0,"next":"again"}`)
				return
			case "invalid-list":
				fmt.Fprint(w, `{"items":null,"size":0}`)
				return
			case "hidden-total":
				// The end of a list that counts a value it does not give.
				fmt.Fprint(w, `{"items":[],"size":0,"total":1}`)
				return
			case "true-total":
				fmt.Fprint(w, `{"items":[],"size":0,"total":0}`)
				return
			case "stalled":
				// The list answers no page until the client gives up.
				f.pages++
				<-r.Context().Done()
				return
			}
			if f.lag || f.unique {
				f.after, f.prefer = append(f.after, r.Header.Get("Authz-Revocation-After")), append(f.prefer, r.Header.Get("Prefer"))
			}
			// A wait for a revocation completes it, as the list then reads at least as fresh as its token.
			f.settled[r.Header.Get("Authz-Revocation-After")] = true
			if f.state != "" && r.Header.Get("Authz-Revocation-After") != "" {
				w.Header().Set("Authz-Revocation", f.state)
			}
			var items []map[string]any
			for _, path := range slices.Sorted(maps.Keys(f.rows)) {
				row := f.rows[path]
				if strings.HasPrefix(path, r.URL.Path+"/") && query.Get("search") == "name = '"+strings.ReplaceAll(row["name"].(string), "'", "''")+"'" {
					if f.lag && f.revocations[path] != "" && !f.settled[f.revocations[path]] {
						continue
					}
					if f.hidden[path] > 0 {
						f.hidden[path]--
						continue
					}
					items = append(items, row)
				}
			}
			f.pages++
			at := 0
			if cursor := query.Get("cursor"); cursor != "" {
				var err error
				if at, err = strconv.Atoi(strings.TrimPrefix(cursor, "c")); err != nil || at < 1 {
					w.WriteHeader(400)
					return
				}
			}
			page := map[string]any{"kind": "RecordList", "size": 0, "items": []any{}}
			if at > 0 {
				page["size"], page["items"] = 1, items[at-1:at]
			}
			if at < len(items) {
				page["next"] = "c" + strconv.Itoa(at+1)
			}
			json.NewEncoder(w).Encode(page)
			if f.afterList != nil {
				f.afterList()
			}
			return
		}
		f.writes++
		var body map[string]any
		decoder := json.NewDecoder(r.Body)
		decoder.UseNumber()
		if decoder.Decode(&body) != nil {
			t.Error("invalid apply request")
		}
		if body["name"] == "second" && f.mode == "write-failure" {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"private":"do-not-print"}`)
			return
		}
		if f.unique && r.Method == "POST" {
			for path, row := range f.rows {
				if strings.HasPrefix(path, r.URL.Path+"/") && row["name"] == body["name"] {
					f.conflicts++
					w.Header().Set("Content-Type", "application/problem+json")
					w.WriteHeader(409)
					fmt.Fprint(w, `{"type":"https://errors.example.test/conflict","status":409,"detail":"private-detail"}`)
					return
				}
			}
		}
		path, id := r.URL.Path, ""
		if f.conditional && r.Method == "PATCH" {
			f.ifMatch = append(f.ifMatch, r.Header.Get("If-Match"))
			if stored, ok := f.rows[r.URL.Path]; ok && r.Header.Get("If-Match") != fmt.Sprintf("%q", fmt.Sprint(stored["generation"])) {
				w.WriteHeader(412)
				return
			}
		}
		if r.Method == "POST" {
			id = fmt.Sprintf("id-%d", f.writes)
			path += "/" + id
		} else {
			id = path[strings.LastIndex(path, "/")+1:]
		}
		f.last = maps.Clone(body)
		if stored, ok := f.rows[path]; ok && r.Method == "PATCH" {
			// A patch changes only the fields that its body gives.
			merged := maps.Clone(stored)
			maps.Copy(merged, body)
			body = merged
		}
		body["id"] = id
		if f.conditional {
			generation := 1
			if stored, ok := f.rows[path]; ok {
				generation = stored["generation"].(int) + 1
			}
			body["generation"] = generation
		}
		f.rows[path] = body
		if f.lag || f.unique {
			revocation := fmt.Sprintf("/authz/revocations/%08x-0000-4000-8000-%012x", f.writes, f.writes)
			f.revocations[path] = revocation
			w.Header().Set("Authz-Revocation", "pending")
			w.Header().Set("Authz-Revocation-Location", revocation)
		}
		if f.hide > 0 && r.Method == "POST" {
			f.hidden[path] = f.hide
		}
		f.order = append(f.order, body["name"].(string))
		if body["name"] == "second" && f.mode == "unknown-write" {
			w.WriteHeader(500)
			fmt.Fprint(w, `{"private":"do-not-print"}`)
			return
		}
		if f.mode == "wrong-write-id" {
			body = map[string]any{"id": "wrong", "name": "wrong"}
		}
		if r.Method == "POST" {
			w.WriteHeader(201)
		}
		json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(server.Close)
	ca, token := filepath.Join(f.directory, "ca.pem"), filepath.Join(f.directory, "token")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(token, []byte("test-apply-token"), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := Run(context.Background(), applyDefinition(), []string{"login", "--url", server.URL, "--token-file", token, "--ca-file", ca}, &output); err != nil {
		t.Fatal(err)
	}
	return f
}
func applyFile(t *testing.T, path, data string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func runApplyTest(t *testing.T, app Application, args ...string) ([]ApplyResult, string, error) {
	t.Helper()
	var output bytes.Buffer
	err := Run(context.Background(), app, args, &output)
	var results []ApplyResult
	if output.Len() > 0 && json.Unmarshal(output.Bytes(), &results) != nil {
		t.Fatalf("invalid apply result: %s", output.Bytes())
	}
	return results, output.String(), err
}
func manifest(kind, name, spec string) string {
	return fmt.Sprintf("apiVersion: example/v1\nkind: %s\nmetadata:\n  name: %q\nspec: %s\n", kind, name, spec)
}

func TestApplyCreatePatchAndDryRun(t *testing.T) {
	f := applySetup(t)
	app := applyDefinition()
	path := applyFile(t, filepath.Join(f.directory, "input.yaml"), manifest("Record", "a' OR name = 'b", `{"count":9007199254740993,"tags":["a","b"]}`)+"---\n"+manifest("Widget", "other", `{"count":0}`))
	// Dry run needs neither credentials nor a network connection.
	if !t.Run("ST-SUP-RC04-OC24-T01 The emitted apply command keeps a dry run local", func(t *testing.T) {
		t.Setenv("TEST_CLI_CONFIG", filepath.Join(f.directory, "absent.json"))
		result, _, err := runApplyTest(t, app, "apply", "-f", path, "--dry-run", "-o", "json")
		if err != nil || len(result) != 2 || result[0].Status != "validated" || f.calls != 0 {
			t.Fatal("dry run contacted API or credentials", err)
		}
	}) {
		return
	}
	t.Run("ST-SUP-RC04-OC26-T01 The emitted apply command keeps exact integers from its input", func(t *testing.T) {
		t.Setenv("TEST_CLI_CONFIG", filepath.Join(f.directory, "config.json"))
		first, _, err := runApplyTest(t, app, "apply", "-f", path, "-o", "json")
		if err != nil || len(first) != 2 || first[0].Status != "created" || f.writes != 2 {
			t.Fatal("apply creation", first, err)
		}
		f.mu.Lock()
		number := f.rows["/records/"+first[0].ID]["count"].(json.Number).String()
		f.mu.Unlock()
		if number != "9007199254740993" {
			t.Fatal("apply lost integer precision")
		}
		changed := applyFile(t, filepath.Join(f.directory, "changed.yaml"), manifest("Record", "a' OR name = 'b", `{"count":9007199254740995,"tags":["a","b"]}`)+"---\n"+manifest("Widget", "other", `{"count":1}`))
		second, _, err := runApplyTest(t, app, "apply", "--filename="+changed, "--output=json")
		if err != nil || second[0].ID != first[0].ID || second[0].Status != "configured" || f.writes != 4 || len(f.rows) != 2 {
			t.Fatal("repeat apply did not patch", second, err)
		}
	})
}

func TestApplyRejectsAllInputBeforeRequests(t *testing.T) {
	f := applySetup(t)
	app := applyDefinition()
	good := manifest("Record", "first", "{}")
	invalid := []string{
		"kind: Unknown\nmetadata: {name: x}", "kind: Record\nmetadata: {}", "kind: Record\nmetadata: {name: x}\nspec: {count: '1'}",
		"kind: Record\nmetadata: {name: x}\nspec: {count: 9223372036854775808}", "kind: Record\nmetadata: {name: x}\nspec: {unknown: 1}",
		"kind: Record\nmetadata: {name: x}\nspec: {name: x}", "kind: Record\nmetadata: {name: x, id: '../escape'}", "kind: Record\nmetadata: {name: x}\nextra: true",
		"kind: Record\nkind: Widget\nmetadata: {name: x}", "kind: Record\nmetadata: &a {name: x}\nspec: *a", "kind: Record\nmetadata: {name: !private secret}",
		"kind: Record\napiVersion: wrong/v1\nmetadata: {name: x}", "kind: Record\nmetadata: {name: x}\nspec: {count: 0x12}", "kind: Record\nmetadata: {name: x}\nspec: {count: .nan}",
		"kind: Record\nmetadata: {name: x}\nspec: {tags: {nested: value}}", `{"kind":"Record","metadata":{"name":"x"},"spec":{"tags":["\ud800"]}}`,
		"kind: Record\nmetadata: {name: x}\nspec: {tags: [a], tags: [b]}", good + "---\n" + good,
	}
	if !t.Run("ST-SUP-RC04-OC26-T02 The emitted apply command accepts only the JSON subset of YAML", func(t *testing.T) {
		for i, bad := range invalid {
			path := applyFile(t, filepath.Join(f.directory, "invalid.yaml"), good+"---\n"+bad)
			if _, _, err := runApplyTest(t, app, "apply", "-f", path, "-o", "json"); err == nil {
				t.Fatalf("accepted invalid document %d", i)
			}
		}
	}) {
		return
	}
	t.Run("ST-SUP-RC04-OC23-T02 The emitted apply command refuses every invalid document before any request", func(t *testing.T) {
		if f.calls != 0 || f.writes != 0 {
			t.Fatal("invalid document reached API")
		}
	})
}

func TestApplyPreflightAndPartialResults(t *testing.T) {
	check := func(t *testing.T, modes []string) {
		for _, mode := range modes {
			t.Run(mode, func(t *testing.T) {
				f := applySetup(t)
				f.mode = mode
				path := applyFile(t, filepath.Join(f.directory, "input.yaml"), manifest("Record", "first", "{}")+"---\n"+manifest("Record", "second", "{}")+"---\n"+manifest("Record", "third", "{}"))
				results, output, err := runApplyTest(t, applyDefinition(), "apply", "-f", path, "-o", "json")
				if err == nil || strings.Contains(output, "do-not-print") || strings.Contains(err.Error(), "do-not-print") {
					t.Fatal("apply failure or private response handling", err)
				}
				if mode == "lookup-failure" || mode == "invalid-list" {
					if f.writes != 0 {
						t.Fatal("lookup failure allowed writes")
					}
					return
				}
				want := "failed"
				if mode == "unknown-write" {
					want = "unknown"
				}
				if f.writes != 2 || len(results) != 3 || results[0].Status != "created" || results[1].Status != want || results[2].Status != "not_attempted" {
					t.Fatal("partial results are wrong", results, f.writes)
				}
			})
		}
	}
	t.Run("ST-SUP-RC04-OC23-T01 The emitted apply command stops before writes", func(t *testing.T) {
		check(t, []string{"lookup-failure", "invalid-list"})
		t.Run("required-create-field", func(t *testing.T) {
			f := applySetup(t)
			app := applyDefinition()
			app.Resources[0].CreateFields[1].Required = true
			path := applyFile(t, filepath.Join(f.directory, "input.yaml"), manifest("Record", "first", "{count: 1}")+"---\n"+manifest("Record", "second", "{}"))
			if _, _, err := runApplyTest(t, app, "apply", "-f", path, "-o", "json"); err == nil || f.writes != 0 {
				t.Fatal("late required field allowed writes", err)
			}
		})
	})
	t.Run("ST-SUP-RC04-OC25-T01 The emitted apply command reports partial results", func(t *testing.T) {
		check(t, []string{"write-failure", "unknown-write"})
	})
}

func TestApplyAmbiguityAndExplicitIDs(t *testing.T) {
	t.Run("ST-SUP-RC04-OC23-T03 The emitted apply command refuses an ambiguous name before any write", func(t *testing.T) {
		f := applySetup(t)
		for _, id := range []string{"one", "two"} {
			f.rows["/records/"+id] = map[string]any{"id": id, "name": "same"}
		}
		path := applyFile(t, filepath.Join(f.directory, "input.yaml"), manifest("Record", "same", "{}"))
		if _, _, err := runApplyTest(t, applyDefinition(), "apply", "-f", path, "-o", "json"); err == nil || f.writes != 0 {
			t.Fatal("ambiguous name was applied")
		}
		applyFile(t, path, "kind: Record\nmetadata: {name: same, id: one}\nspec: {count: 2}\n---\nkind: Record\nmetadata: {name: same, id: two}\nspec: {count: 3}\n")
		result, _, err := runApplyTest(t, applyDefinition(), "apply", "-f", path, "-o", "json")
		if err != nil || len(result) != 2 || result[0].ID != "one" || result[1].ID != "two" {
			t.Fatal("explicit IDs did not resolve ambiguity", result, err)
		}
		applyFile(t, path, "kind: Record\nmetadata: {name: same, id: one}\n---\nkind: Record\nmetadata: {name: changed, id: one}\n")
		if _, _, err := runApplyTest(t, applyDefinition(), "apply", "-f", path, "-o", "json"); err == nil {
			t.Fatal("duplicate explicit target accepted")
		}
	})
}

func TestApplyDirectoriesAndInputBounds(t *testing.T) {
	t.Run("ST-SUP-RC04-OC26-T03 The emitted apply command reads directories in order and holds its input bounds", func(t *testing.T) {
		f := applySetup(t)
		directory := filepath.Join(f.directory, "documents")
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(directory, "nested"), 0700); err != nil {
			t.Fatal(err)
		}
		applyFile(t, filepath.Join(directory, "a.yaml"), manifest("Record", "first", "{}"))
		applyFile(t, filepath.Join(directory, "nested", "b.json"), `{"kind":"Record","metadata":{"name":"second"}}`)
		applyFile(t, filepath.Join(directory, "z.yml"), manifest("Record", "third", "{}"))
		if _, _, err := runApplyTest(t, applyDefinition(), "apply", "-f", directory, "-o", "json"); err != nil || !slices.Equal(f.order, []string{"first", "second", "third"}) {
			t.Fatal("directory order", f.order, err)
		}
		before := f.calls
		if err := os.Symlink(filepath.Join(directory, "a.yaml"), filepath.Join(directory, "link.yaml")); err != nil {
			t.Fatal(err)
		}
		if _, _, err := runApplyTest(t, applyDefinition(), "apply", "-f", directory, "-o", "json"); err == nil || f.calls != before {
			t.Fatal("directory link reached API")
		}
		if err := os.Remove(filepath.Join(directory, "link.yaml")); err != nil {
			t.Fatal(err)
		}
		fifo := filepath.Join(directory, "fifo.yaml")
		if err := syscall.Mkfifo(fifo, 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := runApplyTest(t, applyDefinition(), "apply", "-f", fifo, "-o", "json"); err == nil {
			t.Fatal("FIFO accepted as a regular input")
		}
		if _, err := decodeApply([]byte(strings.Repeat("---\nkind: Record\nmetadata: {name: x}\n", 129))); err == nil {
			t.Fatal("document bound ignored")
		}
		if _, err := decodeApply([]byte("kind: Record\nmetadata: {name: x}\nspec: " + strings.Repeat("[", 40) + "0" + strings.Repeat("]", 40))); err == nil {
			t.Fatal("depth bound ignored")
		}
	})
}
func TestApplyStdinAndCancellation(t *testing.T) {
	original := os.Stdin
	defer func() { os.Stdin = original }()
	file, err := os.CreateTemp(privateDir(t), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(manifest("Record", "stdin", "{}")); err != nil {
		t.Fatal(err)
	}
	file.Seek(0, 0)
	os.Stdin = file
	result, _, err := runApplyTest(t, applyDefinition(), "apply", "-f", "-", "--dry-run", "-o", "json")
	if err != nil || len(result) != 1 || result[0].Name != "stdin" {
		t.Fatal("stdin apply", err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	defer reader.Close()
	os.Stdin = reader
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		var out bytes.Buffer
		done <- Run(ctx, applyDefinition(), []string{"apply", "-f", "-", "--dry-run"}, &out)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled stdin succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled stdin remained blocked")
	}
}

func TestApplyDefinitionAndOutputFailures(t *testing.T) {
	for _, change := range []func(*Application){
		func(a *Application) { a.Resources[0].Path = "//outside.invalid/records" },
		func(a *Application) { a.Resources[0].Path = "/records/{id}" },
		func(a *Application) { a.Resources[1].Kind = a.Resources[0].Kind },
		func(a *Application) { a.Resources[0].APIVersion = "bad\xff" },
		func(a *Application) {
			a.Resources[0].CreateFields = []Field{{Flag: "value", Key: "value", Type: "string"}}
		},
		func(a *Application) {
			a.Resources[0].CreateFields = append(a.Resources[0].CreateFields, Field{Flag: "id", Key: "id", Type: "string"})
		},
		func(a *Application) {
			a.Resources[0].PatchFields = append([]Field(nil), a.Resources[0].PatchFields...)
			a.Resources[0].PatchFields[1].Type = "string"
		},
	} {
		a := applyDefinition()
		change(&a)
		if validate(a) == nil {
			t.Fatal("invalid apply definition accepted")
		}
	}
	f := applySetup(t)
	path := applyFile(t, filepath.Join(f.directory, "input.yaml"), manifest("Record", "first", "{}"))
	for _, args := range [][]string{{"-f", path, "--filename", path}, {"-f", path, "--dry-run=maybe"}, {"-f", path, "--output", "xml"}, {"-f", path, "-k", "other"}, {"-k", "other"}} {
		if _, _, err := runApplyTest(t, applyDefinition(), append([]string{"apply"}, args...)...); err == nil || f.calls != 0 {
			t.Fatal("invalid apply options reached API")
		}
	}
	if !t.Run("ST-SUP-RC04-OC13-T03 The emitted apply command refuses an output file that is its input", func(t *testing.T) {
		if _, _, err := runApplyTest(t, applyDefinition(), "apply", "-f", path, "--output-file", path, "-o", "json"); err == nil || f.calls != 0 {
			t.Fatal("output overwrote apply input")
		}
	}) {
		return
	}
	t.Run("ST-SUP-RC04-OC25-T02 The emitted apply command reports a wrong write answer as unknown", func(t *testing.T) {
		f.mode = "wrong-write-id"
		results, _, err := runApplyTest(t, applyDefinition(), "apply", "-f", path, "-o", "json")
		if err == nil || len(results) != 1 || results[0].Status != "unknown" || f.writes != 1 {
			t.Fatal("invalid write response allowed success or retry", results, err)
		}
	})
}

func TestApplyAliasesCannotDuplicateTarget(t *testing.T) {
	t.Run("ST-SUP-RC04-OC23-T04 Two aliased kinds cannot select one create target", func(t *testing.T) {
		f := applySetup(t)
		app := applyDefinition()
		app.Resources[1].Path = app.Resources[0].Path
		path := applyFile(t, filepath.Join(f.directory, "input.yaml"), manifest("Record", "same", "{}")+"---\n"+manifest("Widget", "same", "{}"))
		if _, _, err := runApplyTest(t, app, "apply", "-f", path, "-o", "json"); err == nil || f.writes != 0 {
			t.Fatal("aliased kinds selected the same create target")
		}
	})
}

func TestApplyRejectsMalformedListPage(t *testing.T) {
	t.Run("ST-SUP-RC04-OC23-T05 A malformed list page causes no create", func(t *testing.T) {
		for _, mode := range []string{"null-next", "wrong-size", "endless", "hidden-total"} {
			f := applySetup(t)
			f.mode = mode
			path := applyFile(t, filepath.Join(f.directory, "input.yaml"), manifest("Record", "example", "{}"))
			if _, _, err := runApplyTest(t, applyDefinition(), "apply", "-f", path, "-o", "json"); err == nil || f.writes != 0 {
				t.Fatalf("%s: the list page caused a create (%v)", mode, err)
			}
			if mode == "endless" && f.pages != 32 {
				t.Errorf("the lookup read %d pages of a list that never ends, want 32", f.pages)
			}
		}
		// A total that is the number of values found is taken.
		f := applySetup(t)
		f.mode = "true-total"
		path := applyFile(t, filepath.Join(f.directory, "input.yaml"), manifest("Record", "example", "{}"))
		if results, _, err := runApplyTest(t, applyDefinition(), "apply", "-f", path, "-o", "json"); err != nil || f.writes != 1 || len(results) != 1 || results[0].Status != "created" {
			t.Errorf("a list with a true total gave %v %v and %d writes, want one create", results, err, f.writes)
		}
	})
	t.Run("ST-SUP-RC04-OC23-T07 An apply lookup that does not end in its time limit causes no write", func(t *testing.T) {
		f := applySetup(t)
		f.mode = "stalled"
		// The time limit of a lookup is 60 s; the test makes it short, so that it does not wait for the limit of a
		// request, 15 s.
		previous := applyLookupTimeout
		applyLookupTimeout = 100 * time.Millisecond
		defer func() { applyLookupTimeout = previous }()
		path := applyFile(t, filepath.Join(f.directory, "input.yaml"), manifest("Record", "example", "{}"))
		_, _, err := runApplyTest(t, applyDefinition(), "apply", "-f", path, "-o", "json")
		// The handler of the stalled page ends when the client goes; the lock waits for it.
		f.mu.Lock()
		writes, pages := f.writes, f.pages
		f.mu.Unlock()
		if err == nil || !strings.Contains(err.Error(), "apply lookup exceeded its time limit") || writes != 0 || pages != 1 {
			t.Errorf("a lookup past its time limit gave %v with %d writes and %d pages, want the time limit error, no write and one page", err, writes, pages)
		}
	})
}

func applyCreateOnlyDefinition() Application {
	app := definition()
	create := []Field{{Flag: "name", Key: "name", Type: "string", Required: true}, {Flag: "code", Key: "code", Type: "string", Required: true}, {Flag: "count", Key: "count", Type: "integer"}}
	patch := []Field{{Flag: "name", Key: "name", Type: "string"}, {Flag: "count", Key: "count", Type: "integer"}}
	app.Resources = append(app.Resources, ApplyResource{Kind: "Record", APIVersion: "example/v1", Path: "/records", CreateFields: create, PatchFields: patch})
	return app
}

func TestApplyCreateOnlyFields(t *testing.T) {
	t.Run("ST-SUP-RC04-OC35-T01 An apply of the same documents again leaves out a create-only field, and refuses another value before any write", func(t *testing.T) {
		f := applySetup(t)
		app := applyCreateOnlyDefinition()
		same := applyFile(t, filepath.Join(f.directory, "same.yaml"), manifest("Record", "first", `{"code":"UTR1","count":9007199254740993}`))
		first, _, err := runApplyTest(t, app, "apply", "-f", same, "-o", "json")
		if err != nil || len(first) != 1 || first[0].Status != "created" || f.writes != 1 {
			t.Fatal("apply creation", first, err)
		}
		// The same code with another count: the patch takes the count and leaves out the code.
		changed := applyFile(t, filepath.Join(f.directory, "changed.yaml"), manifest("Record", "first", `{"code":"UTR1","count":2}`))
		second, _, err := runApplyTest(t, app, "apply", "-f", changed, "-o", "json")
		f.mu.Lock()
		_, sent := f.last["code"]
		f.mu.Unlock()
		if err != nil || len(second) != 1 || second[0].Status != "configured" || second[0].ID != first[0].ID || f.writes != 2 || sent {
			t.Fatal("a second apply of the same document did not patch without the create-only field", second, err, sent)
		}
		other := applyFile(t, filepath.Join(f.directory, "other.yaml"), manifest("Record", "first", `{"code":"SECRETVALUE","count":2}`))
		_, output, err := runApplyTest(t, app, "apply", "-f", other, "-o", "json")
		if err == nil || !strings.Contains(err.Error(), "field code of Record \"first\"") || !strings.Contains(err.Error(), "another value") ||
			strings.Contains(err.Error()+output, "SECRETVALUE") || f.writes != 2 {
			t.Fatal("apply did not refuse another value of a create-only field before any write", err, output)
		}
		f.mu.Lock()
		delete(f.rows["/records/"+first[0].ID], "code")
		f.mu.Unlock()
		_, _, err = runApplyTest(t, app, "apply", "-f", same, "-o", "json")
		if err == nil || !strings.Contains(err.Error(), "field code of Record \"first\"") || !strings.Contains(err.Error(), "does not return it") || f.writes != 2 {
			t.Fatal("apply did not refuse a create-only field that the answer does not hold", err)
		}
	})
}

func TestApplyAgainUnchanged(t *testing.T) {
	t.Run("ST-SUP-RC04-OC35-T02 An apply of the same documents again sends no patch and reports unchanged, and a changed field is configured", func(t *testing.T) {
		f := applySetup(t)
		app := applyCreateOnlyDefinition()
		same := applyFile(t, filepath.Join(f.directory, "same.yaml"), manifest("Record", "first", `{"code":"UTR1","count":9007199254740993}`)+"---\n"+manifest("Record", "second", `{"code":"UTR2"}`))
		first, _, err := runApplyTest(t, app, "apply", "-f", same, "-o", "json")
		if err != nil || len(first) != 2 || first[0].Status != "created" || first[1].Status != "created" || f.writes != 2 {
			t.Fatal("apply creation", first, err)
		}
		again, output, err := runApplyTest(t, app, "apply", "-f", same, "-o", "json")
		if err != nil || len(again) != 2 || again[0].Status != "unchanged" || again[1].Status != "unchanged" || again[0].ID != first[0].ID || again[1].ID != first[1].ID || f.writes != 2 {
			t.Fatal("a second apply of the same documents did not report unchanged without a write", again, err, f.writes)
		}
		if strings.Contains(output, "UTR1") || strings.Contains(output, "9007199254740993") {
			t.Fatal("the results give a spec value", output)
		}
		// The count is one more than the stored count, past the exact range of a float64.
		changed := applyFile(t, filepath.Join(f.directory, "changed.yaml"), manifest("Record", "first", `{"code":"UTR1","count":9007199254740994}`)+"---\n"+manifest("Record", "second", `{"code":"UTR2"}`))
		results, _, err := runApplyTest(t, app, "apply", "-f", changed, "-o", "json")
		if err != nil || len(results) != 2 || results[0].Status != "configured" || results[1].Status != "unchanged" || f.writes != 3 {
			t.Fatal("a changed field was not patched, or an unchanged document was", results, err, f.writes)
		}
		// A field that the answer does not give cannot be compared, so apply sends the patch.
		f.mu.Lock()
		delete(f.rows["/records/"+first[1].ID], "count")
		f.mu.Unlock()
		counted := applyFile(t, filepath.Join(f.directory, "counted.yaml"), manifest("Record", "second", `{"code":"UTR2","count":0}`))
		results, _, err = runApplyTest(t, app, "apply", "-f", counted, "-o", "json")
		if err != nil || len(results) != 1 || results[0].Status != "configured" || f.writes != 4 {
			t.Fatal("a field that the answer does not give was not patched", results, err, f.writes)
		}
	})
}

func TestApplyNamesMissingField(t *testing.T) {
	t.Run("ST-SUP-RC04-OC23-T08 A missing required create field is refused before any write with its name, its kind and its document name, and no value", func(t *testing.T) {
		f := applySetup(t)
		app := applyDefinition()
		app.Resources[0].CreateFields[1].Required = true
		path := applyFile(t, filepath.Join(f.directory, "input.yaml"), manifest("Record", "first", `{"count": 1}`)+"---\n"+manifest("Record", "second", `{"tags": ["PRIVATEVALUE"]}`))
		_, output, err := runApplyTest(t, app, "apply", "-f", path, "-o", "json")
		if err == nil || err.Error() != `Record "second": the required apply field count is missing` || strings.Contains(output, "PRIVATEVALUE") || f.writes != 0 {
			t.Fatal("the refusal does not name the missing field, or a write was sent", err, output, f.writes)
		}
	})
}

// applyCount counts the rows of the fixture.
func (f *applyFixture) applyCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

func TestApplyWaitsForLastWrite(t *testing.T) {
	t.Run("ST-SUP-RC04-OC37-T01 Apply keeps the revocation of its last write in the configuration and sends it on each later lookup, so quick applies make one row", func(t *testing.T) {
		f := applySetup(t)
		f.lag = true
		app := applyDefinition()
		path := applyFile(t, filepath.Join(f.directory, "input.yaml"), manifest("Record", "first", `{"count": 1}`))
		for i := 0; i < 4; i++ {
			results, _, err := runApplyTest(t, app, "apply", "-f", path, "-o", "json")
			want := "unchanged"
			if i == 0 {
				want = "created"
			}
			if err != nil || len(results) != 1 || results[0].Status != want {
				t.Fatal("apply", i, results, err)
			}
		}
		if f.applyCount() != 1 {
			t.Fatalf("four quick applies made %d rows", f.applyCount())
		}
		f.mu.Lock()
		after, prefer := slices.Clone(f.after), slices.Clone(f.prefer)
		f.mu.Unlock()
		if after[0] != "" || after[1] != "/authz/revocations/00000001-0000-4000-8000-000000000001" || prefer[1] != "wait=10" {
			t.Fatal("the lookups did not send the revocation of the last write", after, prefer)
		}
		config, err := os.ReadFile(filepath.Join(f.directory, "config.json"))
		if err != nil || !strings.Contains(string(config), `"revocation":"/authz/revocations/00000001-0000-4000-8000-000000000001"`) {
			t.Fatal("the configuration does not keep the revocation", string(config), err)
		}
		if info, err := os.Stat(filepath.Join(f.directory, "config.json")); err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("the configuration is not private", err)
		}
		// A command write keeps its revocation too, and the next apply lookup sends it.
		var output bytes.Buffer
		if err := Run(context.Background(), app, []string{"create", "record", "--name", "other"}, &output); err != nil {
			t.Fatal(err)
		}
		if _, _, err := runApplyTest(t, app, "apply", "-f", path, "-o", "json"); err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		last := f.after[len(f.after)-1]
		f.mu.Unlock()
		if last != "/authz/revocations/00000002-0000-4000-8000-000000000002" {
			t.Fatal("the lookup did not send the revocation of the command write", last)
		}
		// A configuration that holds a value of another form is refused.
		changed := strings.Replace(string(config), "/authz/revocations/", "/other/", 1)
		if err := os.WriteFile(filepath.Join(f.directory, "config.json"), []byte(changed), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := runApplyTest(t, app, "apply", "-f", path, "-o", "json"); err == nil || !strings.Contains(err.Error(), "invalid configuration file") {
			t.Fatal("a configuration with another revocation form was accepted", err)
		}
	})
}

func TestApplyCreateConflict(t *testing.T) {
	t.Run("ST-SUP-RC04-OC36-T02 A create that the unique name refuses is looked up again with no other write: unchanged for the same values, failed for other values or a resource that the lookup does not show, and apply --help warns about a name that is not unique", func(t *testing.T) {
		f := applySetup(t)
		f.unique, f.hide = true, 1
		app := applyDefinition()
		path := applyFile(t, filepath.Join(f.directory, "input.yaml"), manifest("Record", "first", `{"count": 1}`))
		if results, _, err := runApplyTest(t, app, "apply", "-f", path, "-o", "json"); err != nil || results[0].Status != "created" {
			t.Fatal("apply creation", results, err)
		}
		// The list hides the new row from the next lookup, so apply creates, gets 409 and looks it up again.
		f.mu.Lock()
		f.revocations = map[string]string{}
		f.mu.Unlock()
		results, _, err := runApplyTest(t, app, "apply", "-f", path, "-o", "json")
		if err != nil || len(results) != 1 || results[0].Status != "unchanged" || results[0].ID != "id-1" || f.conflicts != 1 || f.applyCount() != 1 {
			t.Fatal("a create that 409 refused was not found as unchanged", results, err, f.conflicts)
		}
		writes := f.writes
		f.mu.Lock()
		f.hidden["/records/id-1"] = 1
		f.mu.Unlock()
		other := applyFile(t, filepath.Join(f.directory, "other.yaml"), manifest("Record", "first", `{"count": 2}`))
		results, output, err := runApplyTest(t, app, "apply", "-f", other, "-o", "json")
		if err == nil || !strings.Contains(err.Error(), `Record "first": a resource with a unique value of this document exists, and the resource of this name has other values; apply again to patch it (HTTP 409)`) || results[0].Status != "failed" || f.writes != writes+1 || strings.Contains(err.Error()+output, "private-detail") {
			t.Fatal("a create that 409 refused for other values did not fail with no other write", results, err, f.writes)
		}
		f.mu.Lock()
		f.hidden["/records/id-1"] = 2
		f.mu.Unlock()
		results, _, err = runApplyTest(t, app, "apply", "-f", path, "-o", "json")
		if err == nil || !strings.Contains(err.Error(), "a resource with a unique value of this document exists, and the lookup does not find it by its name") || results[0].Status != "failed" || f.applyCount() != 1 {
			t.Fatal("a create that 409 refused for a hidden resource did not fail", results, err)
		}
		// Apply keeps a kind whose name the service does not hold unique, and its help says what that leaves open.
		var help bytes.Buffer
		if err := Run(context.Background(), app, []string{"apply", "--help"}, &help); err != nil || !strings.Contains(help.String(), "Two clients that apply one new name at the same time can make two resources, unless the service holds the name unique (unique: true on the field name).") {
			t.Fatal("apply --help does not warn about a name that is not unique", help.String(), err)
		}
	})
}

func TestApplyConditionalPatch(t *testing.T) {
	t.Run("ST-SUP-RC04-OC38-T02 A conditional patch sends If-Match with the generation that apply read, fails on 412, and is refused before any write with no generation", func(t *testing.T) {
		f := applySetup(t)
		f.conditional = true
		app := applyDefinition()
		app.Resources[0].ConditionalPatch = true
		first := applyFile(t, filepath.Join(f.directory, "first.yaml"), manifest("Record", "first", `{"count": 1}`))
		second := applyFile(t, filepath.Join(f.directory, "second.yaml"), manifest("Record", "first", `{"count": 2}`))
		if results, _, err := runApplyTest(t, app, "apply", "-f", first, "-o", "json"); err != nil || results[0].Status != "created" {
			t.Fatal("apply creation", results, err)
		}
		if results, _, err := runApplyTest(t, app, "apply", "-f", second, "-o", "json"); err != nil || results[0].Status != "configured" || len(f.ifMatch) != 1 || f.ifMatch[0] != `"1"` {
			t.Fatal("the patch did not send If-Match with the read generation", results, err, f.ifMatch)
		}
		// Another writer changes the resource after the lookup read it: the stored generation is no longer the one read.
		stale := applyFile(t, filepath.Join(f.directory, "stale.yaml"), manifest("Record", "first", `{"count": 3}`))
		f.mu.Lock()
		// The list answers an empty first page, then the page with the resource.
		pages := 0
		f.afterList = func() {
			if pages++; pages == 2 {
				f.rows["/records/id-1"]["generation"] = 5
			}
		}
		f.mu.Unlock()
		results, _, err := runApplyTest(t, app, "apply", "-f", stale, "-o", "json")
		if err == nil || !strings.Contains(err.Error(), `Record "first": the resource changed after apply read it; apply again (HTTP 412)`) || results[0].Status != "failed" {
			t.Fatal("a stale patch did not fail", results, err)
		}
		writes := f.writes
		f.mu.Lock()
		f.afterList = nil
		delete(f.rows["/records/id-1"], "generation")
		f.mu.Unlock()
		if _, _, err := runApplyTest(t, app, "apply", "-f", stale, "-o", "json"); err == nil || !strings.Contains(err.Error(), "does not return the generation") || f.writes != writes {
			t.Fatal("a resource with no generation was not refused before any write", err)
		}
	})
}

func TestApplyRevocationEnds(t *testing.T) {
	t.Run("ST-SUP-RC04-OC37-T02 A lookup answer that ends the revocation clears it, an old kept revocation is not sent, and a write of another configuration is not kept", func(t *testing.T) {
		config := func(f *applyFixture) configuration {
			t.Helper()
			data, err := os.ReadFile(filepath.Join(f.directory, "config.json"))
			var cfg configuration
			if err != nil || json.Unmarshal(data, &cfg) != nil {
				t.Fatal("cannot read the configuration", err)
			}
			return cfg
		}
		// sent applies once and gives the Authz-Revocation-After values that its lookups sent, the empty ones left out.
		sent := func(f *applyFixture, app Application, path string) []string {
			t.Helper()
			f.mu.Lock()
			from := len(f.after)
			f.mu.Unlock()
			if _, _, err := runApplyTest(t, app, "apply", "-f", path, "-o", "json"); err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			var values []string
			for _, value := range f.after[from:] {
				if value != "" {
					values = append(values, value)
				}
			}
			return values
		}
		for _, state := range []string{"complete", "failed", "parked", "expired", "unknown"} {
			f := applySetup(t)
			f.lag, f.state = true, state
			app := applyDefinition()
			path := applyFile(t, filepath.Join(f.directory, "input.yaml"), manifest("Record", "first", `{"count": 1}`))
			if _, _, err := runApplyTest(t, app, "apply", "-f", path, "-o", "json"); err != nil || config(f).Revocation == "" {
				t.Fatal(state, "the create kept no revocation", err)
			}
			if values := sent(f, app, path); len(values) != 1 {
				t.Fatal(state, "the second apply did not wait once, then stop", values)
			}
			if cfg := config(f); cfg.Revocation != "" || !cfg.RevocationAt.IsZero() {
				t.Fatal(state, "a revocation that the answer ended stays kept", cfg.Revocation)
			}
			if values := sent(f, app, path); len(values) != 0 {
				t.Fatal(state, "a lookup after the end sent the revocation", values)
			}
		}
		// A pending answer keeps the revocation; one kept longer ago than revocationAge is not sent.
		f := applySetup(t)
		f.lag, f.state = true, "pending"
		app := applyDefinition()
		path := applyFile(t, filepath.Join(f.directory, "input.yaml"), manifest("Record", "first", `{"count": 1}`))
		if _, _, err := runApplyTest(t, app, "apply", "-f", path, "-o", "json"); err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		f.settled[f.revocations["/records/id-1"]] = true
		f.mu.Unlock()
		if values := sent(f, app, path); len(values) == 0 || config(f).Revocation == "" {
			t.Fatal("a pending revocation was not kept", values)
		}
		revocationNow = func() time.Time { return time.Now().Add(revocationAge + time.Minute) }
		t.Cleanup(func() { revocationNow = time.Now })
		if values := sent(f, app, path); len(values) != 0 {
			t.Fatal("an old kept revocation was sent", values)
		}
		revocationNow = time.Now
		// A write whose configuration was replaced, as by a login to another service, keeps nothing.
		other := config(f)
		other.URL = "https://other.example.test"
		response := client.Response{Header: http.Header{"Authz-Revocation-Location": {"/authz/revocations/00000009-0000-4000-8000-000000000009"}}}
		if got := keepRevocation(context.Background(), app, other, response); got == "" || config(f).Revocation == "/authz/revocations/00000009-0000-4000-8000-000000000009" {
			t.Fatal("a revocation of another configuration was kept", got, config(f).Revocation)
		}
		if got := keepRevocation(context.Background(), app, config(f), response); got == "" || config(f).Revocation != "/authz/revocations/00000009-0000-4000-8000-000000000009" {
			t.Fatal("a revocation of the same configuration was not kept", got, config(f).Revocation)
		}
	})
}
