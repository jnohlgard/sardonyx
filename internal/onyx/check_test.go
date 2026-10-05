// Copyright (C) 2026 Joakim Nohlgård
// SPDX-License-Identifier: AGPL-3.0

// Tests for Client.Check — the sard check pre-flight (docs/PLAN.md
// §10, T11a; §13) — against net/http/httptest. Retry waits are
// injected at millisecond scale through the unexported backoff field
// (the same machinery as the Ingest tests), so the suite stays fast.
package onyx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// checkRecorder records check requests and routes responses by
// method+path; n is the 1-based request number for that (method, path).
type checkRecorder struct {
	mu       sync.Mutex
	requests []requestRec
	respond  func(method, path string, n int) (code int, body string)
}

func (rec *checkRecorder) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		rec.mu.Lock()
		rec.requests = append(rec.requests, requestRec{
			Method:      r.Method,
			Path:        r.URL.Path,
			Auth:        r.Header.Get("Authorization"),
			ContentType: r.Header.Get("Content-Type"),
			Body:        body,
		})
		n := 0
		for _, rr := range rec.requests {
			if rr.Method == r.Method && rr.Path == r.URL.Path {
				n++
			}
		}
		rec.mu.Unlock()

		code, b := rec.respond(r.Method, r.URL.Path, n)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, b)
	}
}

func (rec *checkRecorder) count(method, path string) int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	n := 0
	for _, rr := range rec.requests {
		if rr.Method == method && rr.Path == path {
			n++
		}
	}
	return n
}

func (rec *checkRecorder) at(i int) requestRec {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.requests[i]
}

func (rec *checkRecorder) all() []requestRec {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]requestRec(nil), rec.requests...)
}

func (rec *checkRecorder) first(method string) (requestRec, bool) {
	for _, rr := range rec.all() {
		if rr.Method == method {
			return rr, true
		}
	}
	return requestRec{}, false
}

func startCheckServer(t *testing.T, rec *checkRecorder) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(rec.handler())
	t.Cleanup(ts.Close)
	return ts
}

// checkHappy is the healthy response for all three endpoints: a
// success health, a two-document list, and the cc-pair's full info.
func checkHappy() func(method, path string, n int) (int, string) {
	return func(method, path string, n int) (int, string) {
		switch {
		case method == http.MethodGet && path == "/health":
			return 200, `{"success":true,"message":"ok","data":null}`
		case method == http.MethodGet && path == "/onyx-api/ingestion":
			return 200, `[{"document_id":"d1","semantic_id":"a.md","link":null},{"document_id":"d2","semantic_id":"b.md","link":null}]`
		case method == http.MethodGet && path == "/manage/admin/cc-pair/42":
			return 200, `{"success":true,"data":{"name":"My Docs","status":"ACTIVE","num_docs_indexed":12}}`
		}
		return 404, `{"detail":"not found"}`
	}
}

// TestCheckHappyPath covers the T11a happy path: exactly one request
// per probe (methods, paths, and the Bearer header asserted — health
// carries no auth); the key appears in no returned string; the result
// is fully populated.
func TestCheckHappyPath(t *testing.T) {
	rec := &checkRecorder{respond: checkHappy()}
	ts := startCheckServer(t, rec)

	res, err := fastClient(t, ts).Check(context.Background(), 42)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if got := rec.count(http.MethodGet, "/health"); got != 1 {
		t.Errorf("GET /health requests = %d, want 1", got)
	}
	if got := rec.count(http.MethodGet, "/onyx-api/ingestion"); got != 1 {
		t.Errorf("GET /onyx-api/ingestion requests = %d, want 1", got)
	}
	if got := rec.count(http.MethodGet, "/manage/admin/cc-pair/42"); got != 1 {
		t.Errorf("GET cc-pair requests = %d, want 1", got)
	}
	if got := rec.count(http.MethodPost, "/onyx-api/ingestion"); got != 0 {
		t.Errorf("POST /onyx-api/ingestion requests = %d, want 0 (no fallback on a modern deployment)", got)
	}

	if got := rec.at(0); got.Method != http.MethodGet || got.Path != "/health" {
		t.Errorf("first request = %s %s, want GET /health (probe order)", got.Method, got.Path)
	}
	if rec.at(0).Auth != "" {
		t.Errorf("health probe carries Authorization %q, want none", rec.at(0).Auth)
	}
	for i, path := range []string{"/onyx-api/ingestion", "/manage/admin/cc-pair/42"} {
		if got := rec.at(i + 1).Auth; got != "Bearer "+testKey {
			t.Errorf("request %d (%s) Authorization = %q, want %q", i+1, path, got, "Bearer "+testKey)
		}
	}

	if !res.Healthy {
		t.Error("Healthy = false, want true")
	}
	if !res.KeyOK {
		t.Error("KeyOK = false, want true")
	}
	if res.DocCount != 2 {
		t.Errorf("DocCount = %d, want 2", res.DocCount)
	}
	if res.UsedFallback {
		t.Error("UsedFallback = true, want false")
	}
	if res.CCPair == nil {
		t.Fatal("CCPair = nil, want populated")
	}
	if res.CCPair.ID != 42 || res.CCPair.Name != "My Docs" || res.CCPair.Status != "ACTIVE" || res.CCPair.Docs != 12 {
		t.Errorf("CCPair = %+v, want {42 My Docs ACTIVE 12}", res.CCPair)
	}
	// The key is in no returned string.
	for _, s := range []string{res.CCPair.Name, res.CCPair.Status} {
		if strings.Contains(s, testKey) {
			t.Errorf("API key leaked into a result string: %q", s)
		}
	}
}

// TestCheckProbe2Unauthorized: a 401 on probe 2 → ErrAuth with exactly
// one ingestion request (no retry, no further probes).
func TestCheckProbe2Unauthorized(t *testing.T) {
	rec := &checkRecorder{respond: func(method, path string, n int) (int, string) {
		if path == "/health" {
			return 200, `{"success":true}`
		}
		if method == http.MethodGet && path == "/onyx-api/ingestion" {
			return 401, `{"detail":"invalid token"}`
		}
		return 404, `{}`
	}}
	ts := startCheckServer(t, rec)

	res, err := fastClient(t, ts).Check(context.Background(), 42)
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
	if res.KeyOK {
		t.Error("KeyOK = true after a 401")
	}
	if got := rec.count(http.MethodGet, "/onyx-api/ingestion"); got != 1 {
		t.Errorf("GET ingestion requests = %d, want 1 (no retry)", got)
	}
	if got := rec.count(http.MethodPost, "/onyx-api/ingestion"); got != 0 {
		t.Errorf("POST ingestion requests = %d, want 0 (a 401 is not ambiguous)", got)
	}
	if got := rec.count(http.MethodGet, "/manage/admin/cc-pair/42"); got != 0 {
		t.Errorf("cc-pair requests = %d, want 0 (no further probes)", got)
	}
}

// TestCheckProbe2Forbidden: a 403 on probe 2 falls through to the 2f
// disambiguation — the endpoint a real run uses decides: POST 403 →
// ErrAuth, POST 422 → KeyOK + UsedFallback, POST 200 → KeyOK (the
// warning the CLI surfaces).
func TestCheckProbe2Forbidden(t *testing.T) {
	for name, post := range map[string]int{"403": 403, "422": 422, "200": 200} {
		t.Run(name, func(t *testing.T) {
			rec := &checkRecorder{respond: func(method, path string, n int) (int, string) {
				switch {
				case path == "/health":
					return 200, `{"success":true}`
				case method == http.MethodGet && path == "/onyx-api/ingestion":
					return 403, `{"detail":"forbidden"}`
				case method == http.MethodPost && path == "/onyx-api/ingestion":
					return post, `{}`
				default: // probe 3, fallback flow: 404 is only a warning
					return 404, `{}`
				}
			}}
			ts := startCheckServer(t, rec)

			res, err := fastClient(t, ts).Check(context.Background(), 42)
			if got := rec.count(http.MethodGet, "/onyx-api/ingestion"); got != 1 {
				t.Errorf("GET ingestion requests = %d, want 1", got)
			}
			if got := rec.count(http.MethodPost, "/onyx-api/ingestion"); got != 1 {
				t.Errorf("POST ingestion requests = %d, want 1 (the 2f disambiguation)", got)
			}

			switch post {
			case 403:
				if !errors.Is(err, ErrAuth) {
					t.Fatalf("err = %v, want ErrAuth", err)
				}
				if res.KeyOK {
					t.Error("KeyOK = true after 403/403")
				}
				if got := rec.count(http.MethodGet, "/manage/admin/cc-pair/42"); got != 0 {
					t.Errorf("cc-pair requests = %d, want 0 (no further probes)", got)
				}
			default:
				if err != nil {
					t.Fatalf("Check: %v", err)
				}
				if !res.KeyOK || !res.UsedFallback {
					t.Fatalf("result = %+v, want KeyOK + UsedFallback", res)
				}
				if res.CCPair != nil {
					t.Errorf("CCPair = %+v, want nil (a probe-3 404 in the fallback flow is a warning)", res.CCPair)
				}
			}
		})
	}
}

// TestCheckGET404And405Fallback: a deployment predating the GET
// endpoint (404/405) falls through to 2f, whose request body is
// asserted to be exactly "{}" with Content-Type application/json; a
// 400/422 response means the key was accepted (UsedFallback).
func TestCheckGET404And405Fallback(t *testing.T) {
	for _, get := range []int{404, 405} {
		for _, post := range []int{400, 422} {
			t.Run(fmt.Sprintf("GET%d/POST%d", get, post), func(t *testing.T) {
				rec := &checkRecorder{respond: func(method, path string, n int) (int, string) {
					switch {
					case path == "/health":
						return 200, `{"success":true}`
					case method == http.MethodGet && path == "/onyx-api/ingestion":
						return get, `{}`
					case method == http.MethodPost && path == "/onyx-api/ingestion":
						return post, `{}`
					case path == "/manage/admin/cc-pair/42":
						return 200, `{"name":"My Docs","status":"ACTIVE","num_docs_indexed":3}`
					}
					return 404, `{}`
				}}
				ts := startCheckServer(t, rec)

				res, err := fastClient(t, ts).Check(context.Background(), 42)
				if err != nil {
					t.Fatalf("Check: %v", err)
				}
				if !res.KeyOK || !res.UsedFallback {
					t.Fatalf("result = %+v, want KeyOK + UsedFallback", res)
				}
				if res.DocCount != 0 {
					t.Errorf("DocCount = %d, want 0 (the GET list was never reached)", res.DocCount)
				}
				if res.CCPair == nil || res.CCPair.Docs != 3 {
					t.Errorf("CCPair = %+v, want a bare-shape pair with 3 docs", res.CCPair)
				}

				postReq, ok := rec.first(http.MethodPost)
				if !ok {
					t.Fatal("no POST request (2f must have run)")
				}
				if string(postReq.Body) != "{}" {
					t.Fatalf("2f body = %q, want byte-exact {}", postReq.Body)
				}
				if postReq.ContentType != "application/json" {
					t.Errorf("2f Content-Type = %q, want application/json", postReq.ContentType)
				}
			})
		}
	}
}

// TestCheckFallbackNoIngestionAPI: a 2f POST answered with a non-auth
// 4xx (404/405/…) → ErrNoIngestionAPI — never an auth error (the URL
// is misconfigured, not the key).
func TestCheckFallbackNoIngestionAPI(t *testing.T) {
	for _, post := range []int{404, 405} {
		t.Run(fmt.Sprintf("POST%d", post), func(t *testing.T) {
			rec := &checkRecorder{respond: func(method, path string, n int) (int, string) {
				if path == "/health" {
					return 200, `{"success":true}`
				}
				if path == "/onyx-api/ingestion" {
					return 404, `{"detail":"not found"}`
				}
				return 404, `{}`
			}}
			ts := startCheckServer(t, rec)

			res, err := fastClient(t, ts).Check(context.Background(), 42)
			if !errors.Is(err, ErrNoIngestionAPI) {
				t.Fatalf("err = %v, want ErrNoIngestionAPI", err)
			}
			if errors.Is(err, ErrAuth) {
				t.Fatal("must not be an auth error")
			}
			if res.KeyOK {
				t.Error("KeyOK = true with no Ingestion API at this URL")
			}
			if got := rec.count(http.MethodGet, "/manage/admin/cc-pair/42"); got != 0 {
				t.Errorf("cc-pair requests = %d, want 0 (no further probes)", got)
			}
		})
	}
}

// TestCheckProbe2Retries: a 429 is retried and the follow-up 200
// succeeds (2 requests); 500 ×3 exhausts the attempts → ErrUnreachable
// (a non-auth error), via millisecond backoff injection.
func TestCheckProbe2Retries(t *testing.T) {
	t.Run("429 then 200", func(t *testing.T) {
		rec := &checkRecorder{respond: func(method, path string, n int) (int, string) {
			if path == "/health" {
				return 200, `{"success":true}`
			}
			if method == http.MethodGet && path == "/onyx-api/ingestion" {
				if n == 1 {
					return 429, `{"detail":"rate limited"}`
				}
				return 200, `[{"document_id":"d1"}]`
			}
			if path == "/manage/admin/cc-pair/42" {
				return 200, `{"name":"My Docs","status":"ACTIVE","num_docs_indexed":1}`
			}
			return 404, `{}`
		}}
		ts := startCheckServer(t, rec)

		res, err := fastClient(t, ts).Check(context.Background(), 42)
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		if !res.KeyOK || res.DocCount != 1 {
			t.Fatalf("result = %+v, want KeyOK with 1 doc", res)
		}
		if got := rec.count(http.MethodGet, "/onyx-api/ingestion"); got != 2 {
			t.Fatalf("GET ingestion requests = %d, want 2 (429 retried)", got)
		}
	})
	t.Run("500 x3", func(t *testing.T) {
		rec := &checkRecorder{respond: func(method, path string, n int) (int, string) {
			if path == "/health" {
				return 200, `{"success":true}`
			}
			if method == http.MethodGet && path == "/onyx-api/ingestion" {
				return 500, `{"error":"boom: internal onyx error"}`
			}
			return 404, `{}`
		}}
		ts := startCheckServer(t, rec)

		res, err := fastClient(t, ts).Check(context.Background(), 42)
		if !errors.Is(err, ErrUnreachable) {
			t.Fatalf("err = %v, want ErrUnreachable", err)
		}
		if errors.Is(err, ErrAuth) {
			t.Fatal("must not be an auth error")
		}
		if res.KeyOK {
			t.Error("KeyOK = true after 500 ×3")
		}
		if got := rec.count(http.MethodGet, "/onyx-api/ingestion"); got != 3 {
			t.Fatalf("GET ingestion requests = %d, want 3 (the attempt cap)", got)
		}
		if !strings.Contains(err.Error(), "500") {
			t.Errorf("error %q does not carry the status code", err)
		}
	})
}

// TestCheckProbe1ConnectionFailure: a connection failure on probe 1 →
// ErrUnreachable; the check stops, so no request reaches any other
// endpoint.
func TestCheckProbe1ConnectionFailure(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close() // port now refuses connections

	c := NewClient("http://"+addr, testKey)
	c.backoff = []time.Duration{time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond}

	res, err := c.Check(context.Background(), 42)
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
	if res.Healthy || res.KeyOK || res.CCPair != nil {
		t.Fatalf("result = %+v, want the zero result (the check stopped on probe 1)", res)
	}
	// There is no server to record against: the check stopped on probe
	// 1, so nothing could have been sent to the other endpoints.
}

// TestCheckProbe2ConnectionFailure: health answers but the ingestion
// endpoint's connection is torn down → probe 2 exhausts its 3 attempts
// → ErrUnreachable.
func TestCheckProbe2ConnectionFailure(t *testing.T) {
	var attempts atomic.Int64
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"success":true}`)
			return
		}
		attempts.Add(1)
		hj, ok := w.(http.Hijacker)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		conn, _, err := hj.Hijack()
		if err == nil {
			_ = conn.Close() // no response: the client sees a connection error
		}
	})
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	c := NewClient(ts.URL, testKey)
	c.backoff = []time.Duration{time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond}

	res, err := c.Check(context.Background(), 42)
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
	if got := attempts.Load(); got != maxAttempts {
		t.Fatalf("ingestion attempts = %d, want %d", got, maxAttempts)
	}
	if !res.Healthy {
		t.Error("Healthy = false, want true (health answered)")
	}
}

// TestCheckHealthDegraded: a health 200 with success:false, and a
// health 404 (an older deployment without the endpoint), are not
// errors: Healthy=false and the check continues (probe 2 doubles as
// the reachability test).
func TestCheckHealthDegraded(t *testing.T) {
	happy := checkHappy()
	for name, health := range map[string]func() (int, string){
		"200 success:false": func() (int, string) { return 200, `{"success":false,"message":"degraded"}` },
		"404":               func() (int, string) { return 404, `{"detail":"not found"}` },
	} {
		t.Run(name, func(t *testing.T) {
			rec := &checkRecorder{respond: func(method, path string, n int) (int, string) {
				if path == "/health" {
					return health()
				}
				return happy(method, path, n)
			}}
			ts := startCheckServer(t, rec)

			res, err := fastClient(t, ts).Check(context.Background(), 42)
			if err != nil {
				t.Fatalf("Check: %v (a non-200 health is a warning, not an error)", err)
			}
			if res.Healthy {
				t.Error("Healthy = true, want false")
			}
			if !res.KeyOK || res.DocCount != 2 || res.CCPair == nil {
				t.Fatalf("result = %+v, want KeyOK + 2 docs + the cc-pair (the check continued)", res)
			}
		})
	}
}

// TestCheckCCPair200: probe 3's 200 populates the CCPair — both the
// {success, data} envelope and a bare object parse.
func TestCheckCCPair200(t *testing.T) {
	for name, body := range map[string]string{
		"envelope": `{"success":true,"data":{"name":"My Docs","status":"ACTIVE","num_docs_indexed":12}}`,
		"bare":     `{"name":"My Docs","status":"ACTIVE","num_docs_indexed":12}`,
	} {
		t.Run(name, func(t *testing.T) {
			happy := checkHappy()
			rec := &checkRecorder{respond: func(method, path string, n int) (int, string) {
				if path == "/manage/admin/cc-pair/42" {
					return 200, body
				}
				return happy(method, path, n)
			}}
			ts := startCheckServer(t, rec)

			res, err := fastClient(t, ts).Check(context.Background(), 42)
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if res.CCPair == nil {
				t.Fatal("CCPair = nil, want populated")
			}
			if res.CCPair.ID != 42 || res.CCPair.Name != "My Docs" || res.CCPair.Status != "ACTIVE" || res.CCPair.Docs != 12 {
				t.Fatalf("CCPair = %+v, want {42 My Docs ACTIVE 12}", res.CCPair)
			}
		})
	}
}

// TestCheckCCPairNotFound: a probe 3 404 after a successful GET (a
// known modern deployment) → ErrCCPairNotFound; the message names the
// configured id and never the key.
func TestCheckCCPairNotFound(t *testing.T) {
	rec := &checkRecorder{respond: func(method, path string, n int) (int, string) {
		if path == "/health" {
			return 200, `{"success":true}`
		}
		if method == http.MethodGet && path == "/onyx-api/ingestion" {
			return 200, `[{"document_id":"d1"}]`
		}
		if path == "/manage/admin/cc-pair/42" {
			return 404, `{"detail":"not found"}`
		}
		return 404, `{}`
	}}
	ts := startCheckServer(t, rec)

	res, err := fastClient(t, ts).Check(context.Background(), 42)
	if !errors.Is(err, ErrCCPairNotFound) {
		t.Fatalf("err = %v, want ErrCCPairNotFound", err)
	}
	if res.CCPair != nil {
		t.Errorf("CCPair = %+v, want nil", res.CCPair)
	}
	if !strings.Contains(err.Error(), "cc-pair-id 42") {
		t.Errorf("error %q does not name the configured id", err)
	}
	if strings.Contains(err.Error(), testKey) {
		t.Fatalf("API key leaked into the error: %q", err)
	}
}

// TestCheckCCPairFallbackWarnings: a probe 3 403 or 404 in the
// fallback flow (an absent endpoint is indistinguishable from an
// absent pair) → no error, CCPair nil (the caller warns).
func TestCheckCCPairFallbackWarnings(t *testing.T) {
	for _, code := range []int{403, 404} {
		t.Run(fmt.Sprintf("%d", code), func(t *testing.T) {
			rec := &checkRecorder{respond: func(method, path string, n int) (int, string) {
				switch {
				case path == "/health":
					return 200, `{"success":true}`
				case method == http.MethodGet && path == "/onyx-api/ingestion":
					return 403, `{"detail":"forbidden"}`
				case method == http.MethodPost && path == "/onyx-api/ingestion":
					return 422, `{}`
				default:
					return code, `{}`
				}
			}}
			ts := startCheckServer(t, rec)

			res, err := fastClient(t, ts).Check(context.Background(), 42)
			if err != nil {
				t.Fatalf("Check: %v (a probe-3 failure in the fallback flow is only a warning)", err)
			}
			if !res.KeyOK || !res.UsedFallback {
				t.Fatalf("result = %+v, want KeyOK + UsedFallback", res)
			}
			if res.CCPair != nil {
				t.Errorf("CCPair = %+v, want nil", res.CCPair)
			}
		})
	}
}

// TestCheckContextCanceled: cancellation mid-flight (a /health request
// held open) returns the context's error — no sentinel wrap.
func TestCheckContextCanceled(t *testing.T) {
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hold the request open until the test releases it, so the
		// cancellation lands while the request is in flight.
		<-release
	}))
	// ts.Close is called explicitly after release is closed: closing
	// first would wait on the still-blocked handler.

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_, err := NewClient(ts.URL, testKey).Check(ctx, 42)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
		close(done)
	}()

	time.Sleep(50 * time.Millisecond) // let the request reach the server
	cancel()
	<-done
	close(release)
	ts.Close()
}

// TestCheckNeverLeaksKey covers the T11a leak criterion extended to
// Check: across a mix of outcomes (happy path, 401, 403/403, no
// Ingestion API, 429/500 exhaustion, cc-pair 404, unreachable) the API
// key appears in no error string, no result field, no log line, and no
// request body.
func TestCheckNeverLeaksKey(t *testing.T) {
	logs := captureLogs(t)
	checks := 0
	check := func(what, s string) {
		checks++
		if strings.Contains(s, testKey) {
			t.Errorf("API key leaked into %s: %q", what, s)
		}
	}

	run := func(t *testing.T, respond func(method, path string, n int) (int, string)) (*checkRecorder, CheckResult, error) {
		t.Helper()
		rec := &checkRecorder{respond: respond}
		ts := startCheckServer(t, rec)
		res, err := fastClient(t, ts).Check(context.Background(), 42)
		return rec, res, err
	}

	cases := map[string]func(method, path string, n int) (int, string){
		"happy": checkHappy(),
		"401": func(method, path string, n int) (int, string) {
			if path == "/health" {
				return 200, `{"success":true}`
			}
			return 401, `{"detail":"unauthorized"}`
		},
		"403/403": func(method, path string, n int) (int, string) {
			if path == "/health" {
				return 200, `{"success":true}`
			}
			if path == "/onyx-api/ingestion" {
				return 403, `{"detail":"forbidden"}`
			}
			return 404, `{}`
		},
		"no-api": func(method, path string, n int) (int, string) {
			if path == "/health" {
				return 200, `{"success":true}`
			}
			return 404, `{"detail":"not found"}`
		},
		"429 exhaust": func(method, path string, n int) (int, string) {
			if path == "/health" {
				return 200, `{"success":true}`
			}
			if method == http.MethodGet && path == "/onyx-api/ingestion" {
				return 429, `{"detail":"rate limited"}`
			}
			return 404, `{}`
		},
		"500 exhaust": func(method, path string, n int) (int, string) {
			if path == "/health" {
				return 200, `{"success":true}`
			}
			if method == http.MethodGet && path == "/onyx-api/ingestion" {
				return 500, `{"error":"boom"}`
			}
			return 404, `{}`
		},
		"cc-pair 404": func(method, path string, n int) (int, string) {
			if path == "/health" {
				return 200, `{"success":true}`
			}
			if method == http.MethodGet && path == "/onyx-api/ingestion" {
				return 200, `[{"document_id":"d1"}]`
			}
			if path == "/manage/admin/cc-pair/42" {
				return 404, `{"detail":"not found"}`
			}
			return 404, `{}`
		},
	}

	for name, respond := range cases {
		rec, res, err := run(t, respond)
		if err != nil {
			check("error: "+name, err.Error())
		}
		if res.CCPair != nil {
			check("result.CCPair.Name: "+name, res.CCPair.Name)
			check("result.CCPair.Status: "+name, res.CCPair.Status)
		}
		for _, rr := range rec.all() {
			check(fmt.Sprintf("request body: %s %s %s", name, rr.Method, rr.Path), string(rr.Body))
		}
	}

	// Unreachable (dead port): the error must not carry the key.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	c := NewClient("http://"+addr, testKey)
	c.backoff = []time.Duration{time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond}
	if _, err := c.Check(context.Background(), 42); err != nil {
		check("error: unreachable", err.Error())
	} else {
		t.Fatal("expected an error for a dead port")
	}

	check("log output", logs.String())
	if checks == 0 {
		t.Fatal("no strings were checked")
	}
}
