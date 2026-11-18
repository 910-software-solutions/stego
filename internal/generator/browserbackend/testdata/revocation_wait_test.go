package browser

import (
	"net/http"
	"sync"
	"testing"
)

func TestReadsThatWait(t *testing.T) {
	t.Run("ST-SUP-RC02-OC07-T03 A read that waits for a revocation asks for a wait below the proxy timeout and holds only a slot of its own pool", func(t *testing.T) {
		f := setup(t)
		active, _ := login(t, f)
		const revocation = "/authz/revocations/0b8a9f1e-3c55-4c1e-9d55-6c3a0f6f7a10"
		last := func() http.Header {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.requests[len(f.requests)-1]
		}
		// The proxy sends only wait=N, at most maxWait, and wait=maxWait for a read that waits and gives none.
		for _, test := range []struct{ prefer, after, want string }{
			{"", revocation, "wait=4"},
			{"wait=99", revocation, "wait=4"},
			{"return=minimal, wait=2", revocation, "wait=2"},
			{"wait=abc", revocation, "wait=4"},
			{"respond-async, handling=lenient", "", ""},
			{"wait=7", "", "wait=4"},
		} {
			headers := http.Header{}
			if test.prefer != "" {
				headers.Set("Prefer", test.prefer)
			}
			if test.after != "" {
				headers.Set("Authz-Revocation-After", test.after)
			}
			w := send(f.backend, "GET", apiPrefix+"/records/record-1", "", []*http.Cookie{active}, headers)
			require(t, w.Code == 201, "the read failed")
			sent := last()
			require(t, len(sent.Values("Prefer")) <= 1 && sent.Get("Prefer") == test.want, "Prefer "+test.prefer+" reached the API as "+sent.Get("Prefer")+", want "+test.want)
		}

		// A pending revocation keeps each waiting request at the API. Its share of MaxWaitingReads holds their pool, a
		// session holds at most MaxWaitsPerSession slots, and a request past either limit goes on at once with wait=0,
		// never refused (security review of PR #440, L2); a read that does not wait still passes.
		// The API of api_prefix has its share of MaxWaitingReads (ST-SUP-RC02-OC58).
		waits := cap(f.backend.main.waits)
		require(t, waits >= 1 && waits <= MaxWaitingReads && MaxWaitsPerSession == 1, "the waiting requests of the API have no pool of their own")
		sessions := make([]*http.Cookie, waits)
		for i := range sessions {
			sessions[i], _ = login(t, f)
		}
		f.mu.Lock()
		f.hold, f.held = make(chan struct{}), make(chan struct{}, waits+2)
		f.mu.Unlock()
		var group sync.WaitGroup
		codes := make(chan int, waits)
		for _, session := range sessions {
			group.Add(1)
			go func() {
				defer group.Done()
				codes <- send(f.backend, "GET", apiPrefix+"/records/record-1", "", []*http.Cookie{session}, http.Header{"Authz-Revocation-After": {revocation}}).Code
			}()
		}
		for range waits {
			<-f.held
		}
		// The pool is full: another session goes on at once with wait=0.
		w := send(f.backend, "GET", apiPrefix+"/records/record-1", "", []*http.Cookie{active}, http.Header{"Authz-Revocation-After": {revocation}})
		require(t, w.Code == 201 && last().Get("Prefer") == "wait=0", "a waiting request past the pool was refused or waited")
		close(f.hold)
		group.Wait()
		close(codes)
		for code := range codes {
			require(t, code == 201, "a waiting request did not get its answer")
		}
		// One session that loops on a pending revocation holds one slot: its second request goes on with wait=0, and
		// another session still waits in the pool.
		f.mu.Lock()
		f.hold, f.held = make(chan struct{}), make(chan struct{}, waits+2)
		f.mu.Unlock()
		first := make(chan int, 1)
		go func() {
			first <- send(f.backend, "GET", apiPrefix+"/records/record-1", "", []*http.Cookie{sessions[0]}, http.Header{"Authz-Revocation-After": {revocation}}).Code
		}()
		<-f.held
		w = send(f.backend, "GET", apiPrefix+"/records/record-1", "", []*http.Cookie{sessions[0]}, http.Header{"Authz-Revocation-After": {revocation}})
		require(t, w.Code == 201 && last().Get("Prefer") == "wait=0", "a second waiting request of one session took a second slot or was refused")
		if waits > 1 {
			other := make(chan int, 1)
			go func() {
				other <- send(f.backend, "GET", apiPrefix+"/records/record-1", "", []*http.Cookie{active}, http.Header{"Authz-Revocation-After": {revocation}}).Code
			}()
			<-f.held
			close(f.hold)
			require(t, <-other == 201, "another session did not get its wait")
		} else {
			close(f.hold)
		}
		<-first
		w = send(f.backend, "GET", apiPrefix+"/records/record-1", "", []*http.Cookie{active}, nil)
		require(t, w.Code == 201, "the waiting requests took the slots of a read that does not wait")
		f.mu.Lock()
		f.hold, f.held = nil, nil
		f.mu.Unlock()

		// Authz-Revocation-Location is returned only as the path of a revocation.
		for _, location := range []string{"https://internal.example.test/authz/revocations/0b8a9f1e-3c55-4c1e-9d55-6c3a0f6f7a10", "/authz/revocations/x", "/other"} {
			f.mu.Lock()
			f.revocationLocation = location
			f.mu.Unlock()
			w = send(f.backend, "GET", apiPrefix+"/records/record-1", "", []*http.Cookie{active}, nil)
			require(t, w.Code == 201 && w.Header().Get("Authz-Revocation-Location") == "", "the proxy returned the Authz-Revocation-Location "+location)
		}
	})
}
