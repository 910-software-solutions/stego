package allocation

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	kube "example.com/widget/out/kubernetes"
)

type api struct {
	mu                    sync.Mutex
	objects               map[string]kube.Object
	writes                []string
	requests              int
	failQuota             bool
	failNetwork           bool
	failNetworkRead       bool
	failNetworkList       bool
	failAccountRead       bool
	failAccountReadPrefix string
	mutateNetwork         func(kube.Object)
	incomplete            bool
	networkList           func(kube.Object)
	networkPatch          func(kube.Object, kube.Object) int
}

func fixture(t *testing.T) (*Allocator, *api) {
	t.Helper()
	state := &api{objects: map[string]kube.Object{}}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state.mu.Lock()
		defer state.mu.Unlock()
		state.requests++
		if r.Header.Get("Authorization") != "Bearer one" {
			t.Error("missing token")
		}

		if r.Method == "GET" && r.URL.Query().Get("limit") != "" {
			if state.failNetworkList && strings.HasSuffix(r.URL.Path, "/networkpolicies") {
				w.WriteHeader(403)
				return
			}
			entries := []kube.Object{}
			for key, object := range state.objects {
				if !strings.HasPrefix(key, r.URL.Path+"/") || strings.Contains(strings.TrimPrefix(key, r.URL.Path+"/"), "/") {
					continue
				}
				match := true
				for _, label := range strings.Split(r.URL.Query().Get("labelSelector"), ",") {
					if label == "" {
						continue
					}
					key, value, ok := strings.Cut(label, "=")
					if !ok || kube.String(object, "metadata", "labels", key) != value {
						match = false
					}
				}
				if match {
					entry := kube.Object{}
					for key, value := range object {
						if key != "apiVersion" && key != "kind" {
							entry[key] = value
						}
					}
					entries = append(entries, entry)
				}
			}
			sort.Slice(entries, func(i, j int) bool {
				return kube.String(entries[i], "metadata", "name") < kube.String(entries[j], "metadata", "name")
			})
			meta := kube.Object{"resourceVersion": "1"}
			if state.incomplete {
				meta["continue"] = "more"
			}
			page := kube.Object{"apiVersion": "v1", "kind": "List", "metadata": meta, "items": entries}
			if strings.HasSuffix(r.URL.Path, "/networkpolicies") {
				if r.URL.RawQuery != "limit=2" {
					t.Error("network snapshot must be bounded and unfiltered")
				}
				if state.networkList != nil {
					// Change this response, not the stored object used by the next GET.
					encoded, err := json.Marshal(entries)
					if err != nil {
						t.Error(err)
						w.WriteHeader(500)
						return
					}
					var snapshot []kube.Object
					if err = json.Unmarshal(encoded, &snapshot); err != nil {
						t.Error(err)
						w.WriteHeader(500)
						return
					}
					page["items"] = snapshot
					state.networkList(page)
				}
			}
			_ = json.NewEncoder(w).Encode(page)
			return
		}
		current, exists := state.objects[r.URL.Path]
		if r.Method == "GET" {
			if (state.failAccountRead || (state.failAccountReadPrefix != "" && strings.HasPrefix(r.URL.Path, state.failAccountReadPrefix))) && strings.Contains(r.URL.Path, "/serviceaccounts/") {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			if state.failNetworkRead && strings.Contains(r.URL.Path, "/networkpolicies/") {
				w.WriteHeader(403)
				return
			}
			if !exists {
				w.WriteHeader(404)
				return
			}
			_ = json.NewEncoder(w).Encode(current)
			return
		}
		state.writes = append(state.writes, r.Method+" "+r.URL.Path)
		var body kube.Object
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid write")
			w.WriteHeader(400)
			return
		}
		switch r.Method {
		case "POST":
			if r.URL.Query().Get("fieldValidation") != "Strict" {
				t.Error("missing strict validation")
			}
			if (state.failQuota && strings.HasSuffix(r.URL.Path, "/resourcequotas")) || (state.failNetwork && strings.HasSuffix(r.URL.Path, "/networkpolicies")) {
				w.WriteHeader(403)
				return
			}
			name := kube.String(body, "metadata", "name")
			key := r.URL.Path + "/" + name
			if _, ok := state.objects[key]; ok {
				w.WriteHeader(409)
				return
			}
			meta := body["metadata"].(map[string]any)
			meta["uid"] = fmt.Sprintf("uid-%d", len(state.writes))
			meta["resourceVersion"] = "1"
			if state.mutateNetwork != nil && strings.HasSuffix(r.URL.Path, "/networkpolicies") {
				state.mutateNetwork(body)
			}
			state.objects[key] = body
			_ = json.NewEncoder(w).Encode(body)
		case "PATCH":
			if strings.Contains(r.URL.Path, "/networkpolicies/") && state.networkPatch != nil {
				if status := state.networkPatch(current, body); status != 0 {
					w.WriteHeader(status)
					return
				}
			}
			if !exists || kube.String(body, "metadata", "uid") != kube.String(current, "metadata", "uid") || kube.String(body, "metadata", "resourceVersion") != kube.String(current, "metadata", "resourceVersion") {
				w.WriteHeader(409)
				return
			}
			meta := current["metadata"].(map[string]any)
			for _, field := range []string{"labels", "annotations"} {
				if values, ok := kube.Nested(body, "metadata", field).(map[string]any); ok {
					old, ok := meta[field].(map[string]any)
					if !ok {
						old = map[string]any{}
						meta[field] = old
					}
					for key, value := range values {
						old[key] = value
					}
				}
			}
			if strings.Contains(r.URL.Path, "/networkpolicies/") {
				if r.URL.Query().Get("fieldValidation") != "Strict" {
					t.Error("network patch requires strict validation")
				}
				if r.Header.Get("Content-Type") != "application/merge-patch+json" {
					t.Error("network patch must use merge semantics")
				}
				if patch, ok := body["spec"].(map[string]any); ok {
					mergeNetworkFields(current["spec"].(map[string]any), patch)
				}
				var version int
				fmt.Sscan(kube.String(current, "metadata", "resourceVersion"), &version)
				meta["resourceVersion"] = fmt.Sprint(version + 1)
			}
			_ = json.NewEncoder(w).Encode(current)
		case "DELETE":
			if !exists {
				w.WriteHeader(404)
				return
			}
			if kube.String(body, "preconditions", "uid") != kube.String(current, "metadata", "uid") || kube.String(body, "preconditions", "resourceVersion") != "1" {
				t.Error("missing delete precondition")
				w.WriteHeader(409)
				return
			}
			// The allocation policy blocks other actors from updating a live
			// namespace's bindings. The garbage collector cannot clear this
			// finalizer. This behavior was reproduced against Kubernetes.
			if strings.Contains(r.URL.Path, "/rolebindings/") && kube.String(body, "propagationPolicy") == "Foreground" {
				meta := current["metadata"].(map[string]any)
				meta["deletionTimestamp"] = "2026-09-14T00:00:00Z"
				meta["finalizers"] = []any{"foregroundDeletion"}
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(current)
				return
			}
			if kube.String(body, "propagationPolicy") == "Background" {
				meta := current["metadata"].(map[string]any)
				remaining := []any{}
				if values, ok := meta["finalizers"].([]any); ok {
					for _, value := range values {
						if value != "foregroundDeletion" {
							remaining = append(remaining, value)
						}
					}
				}
				if len(remaining) > 0 {
					meta["finalizers"] = remaining
					meta["deletionTimestamp"] = "2026-09-14T00:00:00Z"
					w.WriteHeader(http.StatusAccepted)
					_ = json.NewEncoder(w).Encode(current)
					return
				}
			}
			delete(state.objects, r.URL.Path)
			_ = json.NewEncoder(w).Encode(kube.Object{"kind": "Status"})
		default:
			t.Error("unexpected mutation")
			w.WriteHeader(500)
		}
	}))
	t.Cleanup(server.Close)
	dir := t.TempDir()
	ca, token := filepath.Join(dir, "ca"), filepath.Join(dir, "token")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(token, []byte("one"), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := kube.New(kube.Options{ServerURL: server.URL, CAFile: ca, TokenFile: token})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	allocator, err := New(client, "control")
	if err != nil {
		t.Fatal(err)
	}
	return allocator, state
}
func TestAllocationLifecycle(t *testing.T) {
	a, s := fixture(t)
	ctx := context.Background()
	if err := a.Ensure(ctx, "tenant", "tenant-12345678", "owner-1"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	if len(s.writes) != 5 || s.writes[0] != "POST /api/v1/namespaces" || s.writes[1] != "POST /api/v1/namespaces/tenant-12345678/resourcequotas" {
		t.Fatal("permissions preceded limits")
	}
	if !strings.HasSuffix(s.writes[2], "/rolebindings") || !strings.HasSuffix(s.writes[4], "/clusterrolebindings") {
		t.Fatal("proof must precede cluster access")
	}
	namespace := s.objects["/api/v1/namespaces/tenant-12345678"]
	if kube.String(namespace, "metadata", "labels", "pod-security.kubernetes.io/enforce") != "restricted" {
		t.Fatal("weak Pod security")
	}
	count := len(s.writes)
	s.mu.Unlock()
	if err := a.Ensure(ctx, "tenant", "tenant-12345678", "owner-1"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	if len(s.writes) != count {
		t.Fatal("converged resources changed")
	}
	s.mu.Unlock()
	if ready, err := a.Delete(ctx, "tenant", "tenant-12345678", "owner-1"); err != nil || ready {
		t.Fatalf("submitted binding delete is not complete: %v", err)
	}
	s.mu.Lock()
	if _, ok := s.objects["/api/v1/namespaces/tenant-12345678"]; !ok {
		t.Fatal("namespace removed before cluster binding")
	}
	s.mu.Unlock()
	if ready, err := a.Delete(ctx, "tenant", "tenant-12345678", "owner-1"); err != nil || ready {
		t.Fatalf("submitted namespace delete is not complete: %v", err)
	}
	if ready, err := a.Delete(ctx, "tenant", "tenant-12345678", "owner-1"); err != nil || !ready {
		t.Fatalf("deletion did not converge: %v", err)
	}
}
func TestAllocationRejectsInvalidRequestsBeforeIO(t *testing.T) {
	a, s := fixture(t)
	for _, request := range [][3]string{{"unknown", "tenant-12345678", "owner"}, {"tenant", "foreign-12345678", "owner"}, {"tenant", "tenant-short", "owner"}, {"tenant", "tenant-12345678", "bad/owner"}, {"tenant", "tenant-12345678", ""}, {"tenant", "tenant-1234567-", "owner"}} {
		if err := a.Ensure(context.Background(), request[0], request[1], request[2]); err == nil {
			t.Fatal("invalid ensure accepted")
		}
		if _, err := a.Delete(context.Background(), request[0], request[1], request[2]); err == nil {
			t.Fatal("invalid delete accepted")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.requests != 0 {
		t.Fatal("invalid request reached API")
	}
}
func TestAllocationRefusesForeignOwner(t *testing.T) {
	a, s := fixture(t)
	ctx := context.Background()
	if err := a.Ensure(ctx, "tenant", "tenant-12345678", "owner-1"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	count := len(s.writes)
	s.mu.Unlock()
	if err := a.Ensure(ctx, "tenant", "tenant-12345678", "owner-2"); err == nil {
		t.Fatal("foreign namespace adopted")
	}
	if _, err := a.Delete(ctx, "tenant", "tenant-12345678", "owner-2"); err == nil {
		t.Fatal("foreign namespace deleted")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.writes) != count {
		t.Fatal("foreign request made a write")
	}
}
func TestAllocationQuotaFailureDoesNotGrantAccess(t *testing.T) {
	a, s := fixture(t)
	s.mu.Lock()
	s.failQuota = true
	s.mu.Unlock()
	if err := a.Ensure(context.Background(), "tenant", "tenant-12345678", "owner-1"); err == nil {
		t.Fatal("quota failure ignored")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, write := range s.writes {
		if strings.Contains(write, "rolebindings") {
			t.Fatal("access granted without quota")
		}
	}
}
func TestAllocationCleansBindingsAfterNamespaceLoss(t *testing.T) {
	a, s := fixture(t)
	ctx := context.Background()
	if err := a.Ensure(ctx, "tenant", "tenant-12345678", "owner-1"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	delete(s.objects, "/api/v1/namespaces/tenant-12345678")
	s.mu.Unlock()
	if ready, err := a.Delete(ctx, "tenant", "tenant-12345678", "owner-1"); err != nil || ready {
		t.Fatalf("binding not observed before completion: %v", err)
	}
	if ready, err := a.Delete(ctx, "tenant", "tenant-12345678", "owner-1"); err != nil || !ready {
		t.Fatalf("orphan binding not removed: %v", err)
	}
}

// The cluster job has one CPU, bounded memory, and a projected allocator token.
// This test creates no Pod in an allocated namespace.
func TestClusterAllocationLifecycle(t *testing.T) {
	if os.Getenv("STEGO_ALLOCATION_LIVE") != "1" {
		t.Skip("cluster allocation check is disabled")
	}
	control := os.Getenv("STEGO_ALLOCATION_NAMESPACE")
	suffix := os.Getenv("STEGO_ALLOCATION_SUFFIX")
	if len(suffix) != 8 || !dnsLabel.MatchString(suffix) {
		t.Fatal("invalid cluster check suffix")
	}
	client, err := kube.New(kube.Options{ServerURL: "https://kubernetes.default.svc:443", CAFile: "/var/run/stego-kubernetes/ca.crt", TokenFile: "/var/run/stego-kubernetes/token"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	a, err := New(client, control)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	name, owner := "tenant-"+suffix, "owner-"+suffix
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 60*time.Second)
		defer stop()
		for cleanup.Err() == nil {
			done, err := a.Delete(cleanup, "tenant", name, owner)
			if err != nil {
				t.Errorf("cluster cleanup: %v", err)
				return
			}
			if done {
				return
			}
			select {
			case <-cleanup.Done():
			case <-time.After(time.Second):
			}
		}
		t.Error("cluster cleanup did not finish")
	}()
	for i := 0; i < 2; i++ {
		if err := a.Ensure(ctx, "tenant", name, owner); err != nil {
			t.Fatalf("ensure %d: %v", i, err)
		}
	}
	if err := a.Ensure(ctx, "tenant", name, "foreign-owner"); err == nil {
		t.Fatal("foreign owner accepted")
	}
	// Apply the next profile with cluster access removed. Cleanup must not
	// depend on the removed binding still being in the declaration.
	a.config.Profiles[0].Bindings = a.config.Profiles[0].Bindings[:1]
	for {
		err := a.Ensure(ctx, "tenant", name, owner)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrPending) {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatal("binding cleanup timed out")
		case <-time.After(time.Second):
		}
	}
	if _, code, err := client.Request(ctx, http.MethodGet, "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings/"+control+".widget-queue."+name+".1", nil); err != nil || code != 404 {
		t.Fatalf("removed binding still exists: %v", err)
	}
	current, code, err := client.Request(ctx, http.MethodGet, "/api/v1/namespaces/"+name, nil)
	if err != nil || code != 200 || kube.String(current, "metadata", "labels", MarkerLabel) != a.Marker() {
		t.Fatalf("allocated namespace is not observed: %v", err)
	}
	if _, _, err := client.Request(ctx, http.MethodGet, "/api/v1/namespaces/"+name+"/secrets", nil); err == nil {
		t.Fatal("allocator can read workload Secrets")
	}
	for ctx.Err() == nil {
		done, err := a.Delete(ctx, "tenant", name, owner)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	t.Fatal("allocation cleanup timed out")
}

func TestAllocationRemovesOldBindingsAfterRegeneration(t *testing.T) {
	a, s := fixture(t)
	ctx := context.Background()
	if err := a.Ensure(ctx, "tenant", "tenant-12345678", "owner-1"); err != nil {
		t.Fatal(err)
	}
	// The next declaration removes cluster access and changes the data worker.
	a.config.Profiles[0].Bindings = a.config.Profiles[0].Bindings[:1]
	a.config.Profiles[0].Bindings[0].ServiceAccount = "replacement"
	if err := a.Ensure(ctx, "tenant", "tenant-12345678", "owner-1"); !errors.Is(err, ErrPending) {
		t.Fatalf("binding removal must be observed: %v", err)
	}
	if err := a.Ensure(ctx, "tenant", "tenant-12345678", "owner-1"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for path, object := range s.objects {
		if strings.Contains(path, "/clusterrolebindings/") {
			t.Fatal("removed cluster binding survived regeneration")
		}
		if strings.HasSuffix(path, "-0") {
			subjects := object["subjects"].([]any)
			if subjects[0].(map[string]any)["name"] != "replacement" {
				t.Fatal("old data subject survived regeneration")
			}
		}
	}
}

func TestAllocationRecoversForegroundBindingDeletion(t *testing.T) {
	for _, mode := range []string{"changed", "same", "proof"} {
		t.Run(mode, func(t *testing.T) {
			a, s := fixture(t)
			ctx := context.Background()
			if err := a.Ensure(ctx, "tenant", "tenant-12345678", "owner-1"); err != nil {
				t.Fatal(err)
			}
			suffix := "0"
			if mode == "proof" {
				suffix = "proof"
			}
			path := "/apis/rbac.authorization.k8s.io/v1/namespaces/tenant-12345678/rolebindings/stego-" + a.marker + "-" + suffix
			s.mu.Lock()
			oldUID := kube.String(s.objects[path], "metadata", "uid")
			expectedSubject := s.objects[path]["subjects"].([]any)[0].(map[string]any)["name"]
			meta := s.objects[path]["metadata"].(map[string]any)
			meta["deletionTimestamp"] = "2026-09-14T00:00:00Z"
			meta["finalizers"] = []any{"foregroundDeletion"}
			s.mu.Unlock()
			if mode == "changed" {
				a.config.Profiles[0].Bindings[0].ServiceAccount = "replacement"
				expectedSubject = "replacement"
			}
			if err := a.Ensure(ctx, "tenant", "tenant-12345678", "owner-1"); !errors.Is(err, ErrPending) {
				t.Fatal("binding removal must be observed", err)
			}
			if err := a.Ensure(ctx, "tenant", "tenant-12345678", "owner-1"); err != nil {
				t.Fatal("foreground binding did not recover", err)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if kube.String(s.objects[path], "metadata", "uid") == oldUID || kube.String(s.objects[path], "metadata", "deletionTimestamp") != "" {
				t.Fatal("old binding still exists")
			}
			if subjects := s.objects[path]["subjects"].([]any); subjects[0].(map[string]any)["name"] != expectedSubject {
				t.Fatal("replacement binding missing")
			}
		})
	}
}

func TestAllocationPreservesCustomBindingFinalizer(t *testing.T) {
	for _, foreground := range []bool{false, true} {
		t.Run(fmt.Sprint(foreground), func(t *testing.T) {
			a, s := fixture(t)
			ctx := context.Background()
			if err := a.Ensure(ctx, "tenant", "tenant-12345678", "owner-1"); err != nil {
				t.Fatal(err)
			}
			path := "/apis/rbac.authorization.k8s.io/v1/namespaces/tenant-12345678/rolebindings/stego-" + a.marker + "-0"
			s.mu.Lock()
			meta := s.objects[path]["metadata"].(map[string]any)
			meta["deletionTimestamp"] = "2026-09-14T00:00:00Z"
			meta["finalizers"] = []any{"example.test/cleanup"}
			if foreground {
				meta["finalizers"] = []any{"foregroundDeletion", "example.test/cleanup"}
			}
			before := len(s.writes)
			s.mu.Unlock()
			a.config.Profiles[0].Bindings[0].ServiceAccount = "replacement"
			if err := a.Ensure(ctx, "tenant", "tenant-12345678", "owner-1"); !errors.Is(err, ErrPending) {
				t.Fatal("custom cleanup must remain pending", err)
			}
			if err := a.Ensure(ctx, "tenant", "tenant-12345678", "owner-1"); !errors.Is(err, ErrPending) {
				t.Fatal("custom cleanup must remain pending on another pass", err)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if foreground {
				before++
			}
			if len(s.writes) != before {
				t.Fatal("allocator tried to bypass custom cleanup")
			}
			if values := kube.Nested(s.objects[path], "metadata", "finalizers").([]any); len(values) != 1 || values[0] != "example.test/cleanup" {
				t.Fatal("custom finalizer changed")
			}
		})
	}
}
func TestAllocationDoesNotPruneAnIncompleteSnapshot(t *testing.T) {
	a, s := fixture(t)
	ctx := context.Background()
	if err := a.Ensure(ctx, "tenant", "tenant-12345678", "owner-1"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.incomplete = true
	count := len(s.writes)
	s.mu.Unlock()
	a.config.Profiles[0].Bindings = a.config.Profiles[0].Bindings[:1]
	if err := a.Ensure(ctx, "tenant", "tenant-12345678", "owner-1"); err == nil {
		t.Fatal("incomplete snapshot accepted")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.writes) != count {
		t.Fatal("incomplete snapshot caused a write")
	}
}

func TestNamespaceIdentityIsSealedWithoutSecretAccess(t *testing.T) {
	a, s := fixture(t)
	ctx := context.Background()
	name := "tenant-12345678"
	if err := a.RequireNamespace(ctx, "tenant", name, "owner-1"); !errors.Is(err, ErrPending) {
		t.Fatal("missing allocation was not pending", err)
	}
	if err := a.Ensure(ctx, "tenant", name, "owner-1"); err != nil {
		t.Fatal(err)
	}
	if err := a.RequireNamespace(ctx, "tenant", name, "owner-1"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	ns := s.objects["/api/v1/namespaces/"+name]
	record := kube.Object{"apiVersion": "v1", "kind": "ConfigMap", "immutable": true, "metadata": map[string]any{"name": "key-identity", "uid": "record", "resourceVersion": "1", "labels": kube.Nested(ns, "metadata", "labels")}, "data": map[string]any{"linked_id": "gateway-1", "fingerprint": "gateway-1:public-hash"}}
	s.objects["/api/v1/namespaces/"+name+"/configmaps/key-identity"] = record
	s.mu.Unlock()
	if err := a.Ensure(ctx, "tenant", name, "owner-1"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	if kube.String(ns, "metadata", "annotations", "example.test/fingerprint") != "gateway-1:public-hash" {
		t.Fatal("identity was not sealed")
	}
	count := len(s.writes)
	record["data"].(map[string]any)["fingerprint"] = "replacement"
	s.mu.Unlock()
	if err := a.Ensure(ctx, "tenant", name, "owner-1"); err == nil {
		t.Fatal("sealed identity changed")
	}
	s.mu.Lock()
	if len(s.writes) != count {
		t.Fatal("conflicting record caused a write")
	}
	delete(s.objects, "/api/v1/namespaces/"+name+"/configmaps/key-identity")
	s.mu.Unlock()
	if err := a.Ensure(ctx, "tenant", name, "owner-1"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if kube.String(ns, "metadata", "annotations", "example.test/fingerprint") != "gateway-1:public-hash" {
		t.Fatal("lost record removed the seal")
	}
}

func TestExternalSubjectNamespace(t *testing.T) {
	a, s := fixture(t)
	b := &a.config.Profiles[0].Bindings[0]
	b.Namespace = "external"
	b.ExternalNamespace = "operator-system"
	if err := a.Ensure(context.Background(), "tenant", "tenant-12345678", "owner-1"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	object := s.objects["/apis/rbac.authorization.k8s.io/v1/namespaces/tenant-12345678/rolebindings/stego-"+a.marker+"-0"]
	subjects, ok := object["subjects"].([]any)
	if !ok || len(subjects) != 1 {
		t.Fatal("subject missing")
	}
	subject := subjects[0].(map[string]any)
	if kube.String(subject, "namespace") != "operator-system" || kube.String(subject, "name") != "widget-data" {
		t.Fatal("external subject was rebound to control or allocated namespace")
	}
}

// mergeNetworkFields follows JSON Merge Patch deletion and replacement rules.
func mergeNetworkFields(current, patch map[string]any) {
	for key, value := range patch {
		if value == nil {
			delete(current, key)
			continue
		}
		if fields, ok := value.(map[string]any); ok {
			target, ok := current[key].(map[string]any)
			if !ok {
				target = map[string]any{}
				current[key] = target
			}
			mergeNetworkFields(target, fields)
		} else {
			current[key] = value
		}
	}
}

func TestAllocationEnsureWritesTheDeclaredProfile(t *testing.T) {
	t.Run("ST-SUP-RC26-OC01-T01 Ensure for a declared profile writes that profile's namespace, quota, accounts, bindings and network rules", func(t *testing.T) {
		a, s := fixture(t)
		// The declaration of allocationContext, with two managed accounts and one network rule added in the configuration.
		a.config.Profiles[0].ServiceAccounts = []string{"gateway", "reader"}
		a.config.Profiles[0].NetworkIsolation = true
		a.config.Profiles[0].NetworkPeers = []networkPeer{{Direction: "ingress", Namespace: "control", PodLabel: "app", PodValue: "widget", Protocol: "TCP", Port: 8443}}
		p := a.config.Profiles[0]
		const name = "tenant-12345678"
		if err := a.Ensure(context.Background(), "tenant", name, "owner-1"); err != nil {
			t.Fatal(err)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		namespace, ok := s.objects["/api/v1/namespaces/"+name]
		if !ok || !strings.HasPrefix(kube.String(namespace, "metadata", "name"), "tenant-") {
			t.Fatal("no namespace with the profile prefix")
		}
		if kube.String(namespace, "metadata", "labels", "example.test/owner") != "owner-1" || kube.String(namespace, "metadata", "labels", ProfileLabel) != "tenant" {
			t.Fatal("namespace lacks the owner or the profile")
		}
		quota := s.objects["/api/v1/namespaces/"+name+"/resourcequotas/stego-allocation"]
		hard, _ := kube.Nested(quota, "spec", "hard").(map[string]any)
		want := map[string]string{"pods": "1", "limits.cpu": "1", "limits.memory": "256Mi", "limits.ephemeral-storage": "128Mi", "requests.storage": "1Gi"}
		if len(hard) != len(want) {
			t.Fatalf("quota %v, want %v", hard, want)
		}
		for key, value := range want {
			if hard[key] != value {
				t.Fatalf("quota %v, want %v", hard, want)
			}
		}
		for _, alias := range []string{"gateway", "reader"} {
			account := s.objects["/api/v1/namespaces/"+name+"/serviceaccounts/"+a.serviceAccountName(p, "owner-1", alias)]
			if account == nil || account["automountServiceAccountToken"] != false {
				t.Fatalf("managed account %s missing or mounts its token", alias)
			}
		}
		subject := func(binding kube.Object) (string, string, string) {
			subjects, _ := binding["subjects"].([]any)
			if len(subjects) != 1 {
				return "", "", ""
			}
			entry, _ := subjects[0].(map[string]any)
			return kube.String(binding, "roleRef", "name"), kube.String(entry, "name"), kube.String(entry, "namespace")
		}
		data := s.objects["/apis/rbac.authorization.k8s.io/v1/namespaces/"+name+"/rolebindings/stego-"+a.marker+"-0"]
		if role, account, ns := subject(data); role != "control.widget-queue.data" || account != "widget-data" || ns != "control" {
			t.Fatalf("data binding is %s to %s/%s", role, ns, account)
		}
		review := s.objects["/apis/rbac.authorization.k8s.io/v1/clusterrolebindings/control.widget-queue."+name+".1"]
		if role, account, ns := subject(review); role != "control.widget-queue.review" || account != a.serviceAccountName(p, "owner-1", "gateway") || ns != name {
			t.Fatalf("review binding is %s to %s/%s", role, ns, account)
		}
		policy := s.objects["/apis/networking.k8s.io/v1/namespaces/"+name+"/networkpolicies/stego-allocation"]
		ingress, _ := kube.Nested(policy, "spec", "ingress").([]any)
		if len(ingress) != 1 {
			t.Fatalf("network rules %v", kube.Nested(policy, "spec"))
		}
		rule, _ := ingress[0].(map[string]any)
		from, _ := rule["from"].([]any)
		ports, _ := rule["ports"].([]any)
		if len(from) != 1 || len(ports) != 1 {
			t.Fatalf("network rule %v", rule)
		}
		target, _ := from[0].(map[string]any)
		port, _ := ports[0].(map[string]any)
		if kube.String(target, "namespaceSelector", "matchLabels", "kubernetes.io/metadata.name") != "control" || kube.String(target, "podSelector", "matchLabels", "app") != "widget" || port["protocol"] != "TCP" || fmt.Sprint(port["port"]) != "8443" {
			t.Fatalf("network rule %v", rule)
		}
	})
}

// seedNamespace puts a live namespace with these labels in the fake API, as another writer would leave it.
func seedNamespace(s *api, name string, labels map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects["/api/v1/namespaces/"+name] = kube.Object{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": name, "uid": "uid-seed", "resourceVersion": "1", "labels": labels}}
}

func TestAllocationNamespaceOwnership(t *testing.T) {
	const name = "tenant-12345678"
	t.Run("ST-SUP-RC26-OC02-T01 Every namespace the allocator writes carries its owner and the installation marker", func(t *testing.T) {
		a, s := fixture(t)
		batch := a.config.Profiles[0]
		batch.Name, batch.Prefix = "batch", "batch-"
		a.config.Profiles = append(a.config.Profiles, batch)
		want := map[string]string{"tenant-12345678": "owner-1", "tenant-87654321": "owner-2", "batch-12345678": "owner-1", "batch-87654321": "owner-2"}
		for namespace, owner := range want {
			if err := a.Ensure(context.Background(), strings.TrimSuffix(strings.SplitAfter(namespace, "-")[0], "-"), namespace, owner); err != nil {
				t.Fatal(namespace, err)
			}
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		seen := 0
		for key, object := range s.objects {
			namespace := strings.TrimPrefix(key, "/api/v1/namespaces/")
			if namespace == key || strings.Contains(namespace, "/") {
				continue
			}
			seen++
			if owner, ok := want[namespace]; !ok || kube.String(object, "metadata", "labels", "example.test/owner") != owner {
				t.Errorf("namespace %s has owner %q", namespace, kube.String(object, "metadata", "labels", "example.test/owner"))
			}
			if a.Marker() == "" || kube.String(object, "metadata", "labels", MarkerLabel) != a.Marker() {
				t.Errorf("namespace %s has marker %q, want %q", namespace, kube.String(object, "metadata", "labels", MarkerLabel), a.Marker())
			}
		}
		if seen != len(want) {
			t.Fatalf("%d namespaces, want %d", seen, len(want))
		}
	})
	t.Run("ST-SUP-RC26-OC02-T02 A write to a namespace of another owner is refused", func(t *testing.T) {
		a, s := fixture(t)
		seedNamespace(s, name, map[string]any{MarkerLabel: a.Marker(), ProfileLabel: "tenant", "example.test/owner": "owner-a", "app.kubernetes.io/managed-by": "widget"})
		err := a.Ensure(context.Background(), "tenant", name, "owner-b")
		if err == nil || !strings.Contains(err.Error(), "different owner") {
			t.Fatalf("Ensure for another owner: %v", err)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.writes) != 0 {
			t.Fatalf("writes %v", s.writes)
		}
	})
	t.Run("ST-SUP-RC26-OC02-T03 A deletion of a namespace of another owner is refused", func(t *testing.T) {
		a, s := fixture(t)
		seedNamespace(s, name, map[string]any{MarkerLabel: a.Marker(), ProfileLabel: "tenant", "example.test/owner": "owner-a", "app.kubernetes.io/managed-by": "widget"})
		if _, err := a.Delete(context.Background(), "tenant", name, "owner-b"); err == nil {
			t.Fatal("deletion for another owner accepted")
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, write := range s.writes {
			if strings.HasPrefix(write, "DELETE ") {
				t.Fatalf("delete request %s", write)
			}
		}
		if _, ok := s.objects["/api/v1/namespaces/"+name]; !ok {
			t.Fatal("namespace removed")
		}
	})
	t.Run("ST-SUP-RC26-OC02-T04 A namespace of the same name under another owner is an error, never a success", func(t *testing.T) {
		a, s := fixture(t)
		seedNamespace(s, name, map[string]any{"example.test/owner": "owner-a"})
		if err := a.Ensure(context.Background(), "tenant", name, "owner-b"); err == nil || errors.Is(err, ErrPending) {
			t.Fatalf("Ensure: %v", err)
		}
		if err := a.RequireNamespace(context.Background(), "tenant", name, "owner-b"); err == nil || errors.Is(err, ErrPending) {
			t.Fatalf("RequireNamespace: %v", err)
		}
	})
}
