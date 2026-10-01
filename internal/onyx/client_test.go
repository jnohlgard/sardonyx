// Tests for the Onyx client against net/http/httptest (docs/PLAN.md §10,
// T6). Retry waits are injected at millisecond scale through the
// unexported backoff field, so the tests stay fast; production clients
// keep the 1 s / 4 s / 16 s schedule.
package onyx

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"sardonyx/internal/models"
)

const testKey = "super-secret-onyx-key-12345"

// testPayload builds a minimal valid ingestion payload.
func testPayload() models.OnyxPayload {
	return models.OnyxPayload{
		CCPairID: 42,
		Document: models.OnyxDocument{
			ID:                 "doc-1",
			SemanticIdentifier: "repo/a.md",
			Title:              "A",
			Sections:           []models.Section{{Text: "hello"}},
			Source:             "file",
			Metadata:           map[string]any{"path": "a.md", "ingested_by": "sardonyx"},
			DocUpdatedAt:       "2026-01-02T03:04:05Z",
			FromIngestionAPI:   true,
		},
	}
}

// fastClient points at ts with millisecond backoff (unexported-field
// injection) so retry tests never sleep for real.
func fastClient(t *testing.T, ts *httptest.Server) *Client {
	t.Helper()
	c := NewClient(ts.URL, testKey, 42)
	c.backoff = []time.Duration{time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond}
	return c
}

// requestRec is one request as seen by the test server.
type requestRec struct {
	Path        string
	Auth        string
	ContentType string
	Body        []byte
	Payload     models.OnyxPayload
}

// recorder captures requests; response drives the handler's reply, keyed
// by the 1-based request number.
type recorder struct {
	mu       sync.Mutex
	requests []requestRec

	response func(n int) (code int, body string)
}

func (rec *recorder) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p models.OnyxPayload
		_ = json.Unmarshal(body, &p)

		rec.mu.Lock()
		rec.requests = append(rec.requests, requestRec{
			Path:        r.URL.Path,
			Auth:        r.Header.Get("Authorization"),
			ContentType: r.Header.Get("Content-Type"),
			Body:        body,
			Payload:     p,
		})
		n := len(rec.requests)
		rec.mu.Unlock()

		code, b := rec.response(n)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, b)
	}
}

func (rec *recorder) count() int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return len(rec.requests)
}

func (rec *recorder) at(i int) requestRec {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.requests[i]
}

// captureLogs redirects the standard logger to a buffer for the test's
// duration. The onyx client does not log at all, but the buffer guards
// the "key never in logs" acceptance criterion.
func captureLogs(t *testing.T) *strings.Builder {
	t.Helper()
	buf := &strings.Builder{}
	old := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(old) })
	return buf
}

func startServer(t *testing.T, rec *recorder) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(rec.handler())
	t.Cleanup(ts.Close)
	return ts
}

// TestIngestSuccess covers acceptance 1: 200 maps to created (new id)
// and updated (already_existed: true), with the expected headers and
// body shape.
func TestIngestSuccess(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"created", `{"already_existed": false}`, models.ResultCreated},
		{"updated", `{"already_existed": true}`, models.ResultUpdated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{response: func(int) (int, string) { return 200, tc.body }}
			ts := startServer(t, rec)

			res, err := fastClient(t, ts).Ingest(context.Background(), testPayload())
			if err != nil {
				t.Fatalf("Ingest: %v", err)
			}
			if res.Status != tc.want {
				t.Fatalf("status = %q, want %q (reason %q)", res.Status, tc.want, res.Reason)
			}

			if got := rec.count(); got != 1 {
				t.Fatalf("requests = %d, want 1", got)
			}
			req := rec.at(0)
			if req.Path != "/onyx-api/ingestion" {
				t.Errorf("path = %q, want %q", req.Path, "/onyx-api/ingestion")
			}
			if req.Auth != "Bearer "+testKey {
				t.Errorf("Authorization = %q, want %q", req.Auth, "Bearer "+testKey)
			}
			if req.ContentType != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", req.ContentType)
			}
			if req.Payload.CCPairID != 42 {
				t.Errorf("cc_pair_id = %d, want 42", req.Payload.CCPairID)
			}
			if req.Payload.Document.ID != "doc-1" {
				t.Errorf("document id = %q, want doc-1", req.Payload.Document.ID)
			}
		})
	}
}

// TestIngestRetriesOn429 covers acceptance 2: a 429 is retried and the
// follow-up 200 succeeds.
func TestIngestRetriesOn429(t *testing.T) {
	rec := &recorder{response: func(n int) (int, string) {
		if n == 1 {
			return 429, `{"detail": "rate limited"}`
		}
		return 200, `{"already_existed": false}`
	}}
	ts := startServer(t, rec)

	res, err := fastClient(t, ts).Ingest(context.Background(), testPayload())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if res.Status != models.ResultCreated {
		t.Fatalf("status = %q, want created (reason %q)", res.Status, res.Reason)
	}
	if got := rec.count(); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
}

// TestIngestExhausts500s covers acceptance 3: 500 ×3 exhausts the 3
// attempts and fails with the status code and a body excerpt.
func TestIngestExhausts500s(t *testing.T) {
	rec := &recorder{response: func(int) (int, string) {
		return 500, `{"error": "boom: internal onyx error"}`
	}}
	ts := startServer(t, rec)

	res, err := fastClient(t, ts).Ingest(context.Background(), testPayload())
	if err != nil {
		t.Fatalf("Ingest: %v (per-document failures must not abort the run)", err)
	}
	if res.Status != models.ResultFailed {
		t.Fatalf("status = %q, want failed", res.Status)
	}
	if got := rec.count(); got != 3 {
		t.Fatalf("requests = %d, want 3", got)
	}
	for _, want := range []string{"500", "boom: internal onyx error"} {
		if !strings.Contains(res.Reason, want) {
			t.Errorf("reason %q does not contain %q", res.Reason, want)
		}
	}
}

// TestIngestAuthFailsFast covers acceptance 4: 401/403 return after
// exactly one request with an actionable message and an ErrAuth-wrapping
// error so the caller can stop the run.
func TestIngestAuthFailsFast(t *testing.T) {
	for _, code := range []int{401, 403} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			rec := &recorder{response: func(int) (int, string) {
				return code, `{"detail": "unauthorized"}`
			}}
			ts := startServer(t, rec)

			res, err := fastClient(t, ts).Ingest(context.Background(), testPayload())
			if err == nil {
				t.Fatal("Ingest: expected an error for 401/403")
			}
			if !errors.Is(err, ErrAuth) {
				t.Fatalf("error %v does not wrap ErrAuth", err)
			}
			if res.Status != models.ResultFailed {
				t.Fatalf("status = %q, want failed", res.Status)
			}
			if got := rec.count(); got != 1 {
				t.Fatalf("requests = %d, want 1 (must not retry)", got)
			}
			for _, s := range []string{res.Reason, err.Error()} {
				for _, want := range []string{"ONYX_API_KEY", "manage:connectors"} {
					if !strings.Contains(s, want) {
						t.Errorf("%q does not mention %q", s, want)
					}
				}
			}
		})
	}
}

// TestIngestOther4xxNotRetried: 400 (neither 401/403 nor 429/5xx) fails
// on the first attempt with the status code and body excerpt.
func TestIngestOther4xxNotRetried(t *testing.T) {
	rec := &recorder{response: func(int) (int, string) {
		return 400, `{"detail": "bad payload"}`
	}}
	ts := startServer(t, rec)

	res, err := fastClient(t, ts).Ingest(context.Background(), testPayload())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if res.Status != models.ResultFailed {
		t.Fatalf("status = %q, want failed", res.Status)
	}
	if got := rec.count(); got != 1 {
		t.Fatalf("requests = %d, want 1 (must not retry)", got)
	}
	for _, want := range []string{"400", "bad payload"} {
		if !strings.Contains(res.Reason, want) {
			t.Errorf("reason %q does not contain %q", res.Reason, want)
		}
	}
}

// TestIngestContextCanceled: cancellation is honored both before the
// first attempt (zero requests) and in flight (the aborted attempt is
// not retried), returning the context's error.
func TestIngestContextCanceled(t *testing.T) {
	t.Run("before call", func(t *testing.T) {
		rec := &recorder{response: func(int) (int, string) { return 200, `{}` }}
		ts := startServer(t, rec)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		res, err := fastClient(t, ts).Ingest(ctx, testPayload())
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if res.Status != models.ResultFailed || !strings.Contains(res.Reason, "interrupted") {
			t.Fatalf("result = %+v, want failed with interrupted reason", res)
		}
		if got := rec.count(); got != 0 {
			t.Fatalf("requests = %d, want 0", got)
		}
	})

	t.Run("in flight", func(t *testing.T) {
		release := make(chan struct{})
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Hold the request open until the test releases it, so the
			// cancellation lands while the request is in flight.
			<-release
		})
		ts := httptest.NewServer(h)
		// ts.Close is called explicitly after release is closed below:
		// closing first would wait on the still-blocked handler.

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		var res models.IngestResult
		var err error
		go func() {
			res, err = NewClient(ts.URL, testKey, 42).Ingest(ctx, testPayload())
			close(done)
		}()

		time.Sleep(50 * time.Millisecond) // let the request reach the server
		cancel()
		<-done

		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if res.Status != models.ResultFailed || !strings.Contains(res.Reason, "interrupted") {
			t.Fatalf("result = %+v, want failed with interrupted reason", res)
		}
		close(release)
		ts.Close()
	})
}

// TestIngestConnectionError: a refused connection is retryable and
// exhausts to a failed result with a connection-error reason.
func TestIngestConnectionError(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close() // port now refuses connections

	c := NewClient("http://"+addr, testKey, 42)
	c.backoff = []time.Duration{time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond}

	res, err := c.Ingest(context.Background(), testPayload())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if res.Status != models.ResultFailed {
		t.Fatalf("status = %q, want failed", res.Status)
	}
	if !strings.Contains(res.Reason, "connection error") {
		t.Errorf("reason %q does not mention the connection error", res.Reason)
	}
}

// TestNewClientStripsTrailingSlash: the client tolerates a trailing "/"
// in the base URL and still hits /onyx-api/ingestion.
func TestNewClientStripsTrailingSlash(t *testing.T) {
	rec := &recorder{response: func(int) (int, string) {
		return 200, `{"already_existed": false}`
	}}
	ts := startServer(t, rec)

	c := NewClient(ts.URL+"/", testKey, 42)
	c.backoff = []time.Duration{time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond}
	res, err := c.Ingest(context.Background(), testPayload())
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if res.Status != models.ResultCreated {
		t.Fatalf("status = %q, want created", res.Status)
	}
	if got := rec.at(0).Path; got != "/onyx-api/ingestion" {
		t.Fatalf("path = %q, want /onyx-api/ingestion", got)
	}
}

// TestAPIKeyNeverLeaked covers acceptance 5: across a mix of outcomes
// (success, retryable exhaustion, auth rejection) the API key appears in
// no log line, error string, or reason.
func TestAPIKeyNeverLeaked(t *testing.T) {
	logs := captureLogs(t)
	checks := 0
	check := func(what, s string) {
		checks++
		if strings.Contains(s, testKey) {
			t.Errorf("API key leaked into %s: %q", what, s)
		}
	}

	type outcome struct {
		res models.IngestResult
		err error
	}

	run := func(t *testing.T, response func(n int) (int, string)) (*recorder, outcome) {
		t.Helper()
		rec := &recorder{response: response}
		ts := startServer(t, rec)
		res, err := fastClient(t, ts).Ingest(context.Background(), testPayload())
		return rec, outcome{res, err}
	}

	cases := map[string]func(n int) (int, string){
		"success":     func(int) (int, string) { return 200, `{"already_existed": false}` },
		"500 exhaust": func(int) (int, string) { return 500, `{"error": "boom: internal onyx error"}` },
		"429 exhaust": func(int) (int, string) { return 429, `{"detail": "rate limited"}` },
		"401":         func(int) (int, string) { return 401, `{"detail": "unauthorized"}` },
	}

	for name, response := range cases {
		rec, out := run(t, response)
		check("reason: "+name, out.res.Reason)
		if out.err != nil {
			check("error: "+name, out.err.Error())
		}
		for i := 0; i < rec.count(); i++ {
			check("request body: "+name, string(rec.at(i).Body))
		}
	}

	check("log output", logs.String())
	if checks == 0 {
		t.Fatal("no strings were checked")
	}
}
