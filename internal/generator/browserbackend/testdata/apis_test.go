package browser

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// secondPrefix is the prefix of the declared API second of the generated configuration.
const secondPrefix = "/api/second/v1"

func TestDeclaredAPIs(t *testing.T) {
	t.Run("ST-SUP-RC02-OC58-T01 Each declared API prefix reaches one fixed upstream", func(t *testing.T) {
		f := setup(t)
		if f.second == nil {
			t.Fatal("the generated configuration declares no apis")
		}
		active, _ := login(t, f)
		counts := func() (int, int) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return len(f.paths), len(f.secondPaths)
		}
		// Each prefix goes to its own origin, at a whole segment, with a query that names another host.
		w := send(f.backend, "GET", apiPrefix+"/records/record-1", "", []*http.Cookie{active}, nil)
		first, second := counts()
		require(t, w.Code == 201 && first == 1 && second == 0, "api_prefix did not reach its own API alone")
		for _, target := range []string{secondPrefix, secondPrefix + "/things?host=evil.example.test&url=https://evil.example.test"} {
			w = send(f.backend, "GET", target, "", []*http.Cookie{active}, nil)
			require(t, w.Code == 200 && w.Body.String() == `{"api":"second"}`, "the declared API did not answer: "+target)
		}
		first, second = counts()
		require(t, first == 1 && second == 2, "a request of the declared API reached another API")
		f.mu.Lock()
		require(t, f.secondPaths[1] == secondPrefix+"/things?host=evil.example.test&url=https://evil.example.test", "the declared API got another path")
		f.mu.Unlock()
		// A path that is not under a prefix at a whole segment, a dot segment, an empty segment and an encoded
		// slash reach no API.
		for _, target := range []string{"/api/secondary/v1/things", secondPrefix + "/../../records/v1/records", secondPrefix + "/./things", secondPrefix + "//things", secondPrefix + "/a%2Fb"} {
			w = send(f.backend, "GET", target, "", []*http.Cookie{active}, nil)
			require(t, w.Code != 200 && w.Code != 201, "a path that is not a declared API path was served: "+target)
		}
		again, secondAgain := counts()
		require(t, again == first && secondAgain == second, "a path that is not a declared API path reached an API")
		// A Location must be under the prefix of the same API.
		w = send(f.backend, "GET", secondPrefix+"/things?location="+secondPrefix+"/things/1", "", []*http.Cookie{active}, nil)
		require(t, w.Code == 200 && w.Header().Get("Location") == secondPrefix+"/things/1", "a Location under the same prefix was not returned")
		for _, location := range []string{apiPrefix + "/records/1", secondPrefix + "/../../records/v1/records/1", secondPrefix + "/%2e%2e/%2e%2e/records/v1/records/1", secondPrefix + "/./things"} {
			w = send(f.backend, "GET", secondPrefix+"/things?location="+url.QueryEscape(location), "", []*http.Cookie{active}, nil)
			require(t, w.Code == 502 && w.Header().Get("Location") == "", "a Location that leaves the prefix of its API was returned: "+location)
		}

		// Each API has its own pool of permits: with every permit of the API of api_prefix taken, that API answers
		// 503 and the API second still answers.
		for range cap(f.backend.main.permits) {
			f.backend.main.permits <- struct{}{}
		}
		w = send(f.backend, "GET", apiPrefix+"/records/record-1", "", []*http.Cookie{active}, nil)
		require(t, w.Code == 503, "an API with no permit left was served")
		w = send(f.backend, "GET", secondPrefix+"/things", "", []*http.Cookie{active}, nil)
		require(t, w.Code == 200, "a busy API took the permits of another API")
		for range cap(f.backend.main.permits) {
			<-f.backend.main.permits
		}
		require(t, cap(f.backend.main.permits)+cap(f.backend.apis[0].permits) <= cap(f.backend.permits), "the pools of the APIs are more than the bound of the backend")

		// Start refuses a missing, an unknown or a repeated name, and an address that is not an HTTPS origin.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, upstreams := range []string{
			"",
			"second=" + f.second.URL + ",third=" + f.second.URL,
			"second=" + f.second.URL + ",second=" + f.second.URL,
			"second=" + strings.Replace(f.second.URL, "https://", "http://", 1),
			"second=" + f.second.URL + "/path",
			"other=" + f.second.URL,
			"second=" + f.oidc.server.URL,
		} {
			o := f.options
			o.Upstreams = upstreams
			b, err := newBackend(ctx, f.db, o)
			if err == nil {
				b.Close()
				t.Fatalf("start accepted STEGO_BROWSER_API_URLS %q", upstreams)
			}
			// Walkthrough 29, BB-FAIL: the failure record names the fault, the setting and the remedy, and no value.
			var named interface {
				FaultClass() string
				FaultObject() string
				FaultRemedy() string
			}
			require(t, errors.As(err, &named) && named.FaultClass() == "browser.settings" && named.FaultObject() == "stego_browser_api_urls" &&
				named.FaultRemedy() == apiURLsRemedy, "the refusal of STEGO_BROWSER_API_URLS "+upstreams+" names no fault, setting and remedy: "+err.Error())
			require(t, !strings.Contains(err.Error(), f.second.URL) && !strings.Contains(named.FaultRemedy(), f.second.URL), "the refusal shows the value of STEGO_BROWSER_API_URLS")
		}
	})

	t.Run("ST-SUP-RC02-OC58-T04 A path with a ; in a segment, a percent sign after the decode, or bytes that are not UTF-8 reaches no API", func(t *testing.T) {
		f := setup(t)
		active, _ := login(t, f)
		counts := func() (int, int) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return len(f.paths), len(f.secondPaths)
		}
		first, second := counts()
		// A matrix parameter, the ..; of a servlet container, an encoded ;, a double encoding of a dot, a slash and a
		// NUL, and an overlong form of a dot and a byte that is not UTF-8, under each API.
		for _, target := range []string{secondPrefix + "/..;/..;/records/v1/records", secondPrefix + "/things;x=1", secondPrefix + "/a%3Bb",
			secondPrefix + "/%252e%252e/%252e%252e/records/v1/records", secondPrefix + "/a%252Fb", apiPrefix + "/records/record-1%2500",
			secondPrefix + "/%C0%AE%C0%AE/%C0%AE%C0%AE/records/v1/records", secondPrefix + "/a%FFb"} {
			w := send(f.backend, "GET", target, "", []*http.Cookie{active}, nil)
			require(t, w.Code == 400, "a path with a ;, a double encoding or bytes that are not UTF-8 was not refused: "+target)
		}
		again, secondAgain := counts()
		require(t, again == first && secondAgain == second, "a path with a ; or a double encoding reached an API")
		// A path with neither still reaches its API.
		w := send(f.backend, "GET", secondPrefix+"/things", "", []*http.Cookie{active}, nil)
		require(t, w.Code == 200, "a clean path of the declared API was not served")
	})

	t.Run("ST-SUP-RC02-OC58-T06 With every permit of the backend held by API requests, a login, a sign-out page, the session and an asset still answer", func(t *testing.T) {
		f := setup(t)
		active, _ := login(t, f)
		// Slow APIs hold each permit of the backend: each request to an API takes one before it is routed.
		for range cap(f.backend.permits) {
			f.backend.permits <- struct{}{}
		}
		defer func() {
			for range cap(f.backend.permits) {
				<-f.backend.permits
			}
		}()
		w := send(f.backend, "GET", apiPrefix+"/records/record-1", "", []*http.Cookie{active}, nil)
		require(t, w.Code == 503, "an API request was served with no permit of the backend left")
		w = send(f.backend, "GET", "/auth/login?return_to=%2F", "", nil, http.Header{"Sec-Fetch-Site": {"none"}})
		require(t, w.Code == 302, "a login got no permit while slow APIs held the permits of the backend")
		w = send(f.backend, "GET", "/auth/session", "", []*http.Cookie{active}, nil)
		require(t, w.Code == 200, "the session got no permit while slow APIs held the permits of the backend")
		w = send(f.backend, "GET", "/auth/logout", "", []*http.Cookie{active}, nil)
		require(t, w.Code == 200, "the sign-out page got no permit while slow APIs held the permits of the backend")
		for _, path := range []string{"/assets/main.js", "/", "/records/record-1"} {
			w = send(f.backend, "GET", path, "", nil, nil)
			require(t, w.Code == 200, "an asset or a document got no permit while slow APIs held the permits of the backend: "+path)
		}
		// The reserve is bounded too: with it full as well, a login gets 503.
		for range cap(f.backend.reserve) {
			f.backend.reserve <- struct{}{}
		}
		w = send(f.backend, "GET", "/auth/login?return_to=%2F", "", nil, http.Header{"Sec-Fetch-Site": {"none"}})
		for range cap(f.backend.reserve) {
			<-f.backend.reserve
		}
		require(t, w.Code == 503, "a login was served with no permit of the backend and no permit of the reserve left")
		// A telemetry request does not take the reserve: it is not a route that the backend answers alone.
		require(t, !f.backend.ownRoute("/telemetry/v1/traces") && !f.backend.ownRoute(secondPrefix+"/things"), "a request of an API or of telemetry may take the reserve")
	})
}
