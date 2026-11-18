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

		// A pending revocation keeps each waiting read at the API. Its share of MaxWaitingReads holds their pool, the next
		// is refused at once, and a read that does not wait still passes.
		// The API of api_prefix has its share of MaxWaitingReads (ST-SUP-RC02-OC58): all of it with one API.
		waits := cap(f.backend.main.waits)
		require(t, waits >= 1 && waits <= MaxWaitingReads, "the waiting reads of the API have no pool of their own")
		f.mu.Lock()
		f.hold, f.held = make(chan struct{}), make(chan struct{}, waits+1)
		f.mu.Unlock()
		var group sync.WaitGroup
		codes := make(chan int, waits)
		for range waits {
			group.Add(1)
			go func() {
				defer group.Done()
				codes <- send(f.backend, "GET", apiPrefix+"/records/record-1", "", []*http.Cookie{active}, http.Header{"Authz-Revocation-After": {revocation}}).Code
			}()
		}
		for range waits {
			<-f.held
		}
		w := send(f.backend, "GET", apiPrefix+"/records/record-1", "", []*http.Cookie{active}, http.Header{"Authz-Revocation-After": {revocation}})
		require(t, w.Code == 503 && w.Header().Get("Retry-After") == "1", "a waiting read past the pool was not refused")
		w = send(f.backend, "GET", apiPrefix+"/records/record-1", "", []*http.Cookie{active}, nil)
		require(t, w.Code == 201, "the waiting reads took the slots of a read that does not wait")
		close(f.hold)
		group.Wait()
		close(codes)
		for code := range codes {
			require(t, code == 201, "a waiting read did not get its answer")
		}
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
