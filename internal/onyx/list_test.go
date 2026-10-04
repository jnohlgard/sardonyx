// Copyright (C) 2026 Joakim Nohlgård
// SPDX-License-Identifier: AGPL-3.0

// Tests for Client.ListDocs — the sard ls data source (docs/PLAN.md
// §10, T12a; §14) — against net/http/httptest. Retry waits are
// injected at millisecond scale through the unexported backoff field
// (the same machinery as the Ingest and Check tests), so the suite
// stays fast (~1 s).
package onyx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// listDocsBody is the two-document list served by the happy-path
// mock: the first document has no link, the second does (a null link
// decodes to the empty string).
const listDocsBody = `[{"document_id":"d1","semantic_id":"a.md","link":null},{"document_id":"d2","semantic_id":"b.md","link":"https://example.com/b"}]`

var wantDocs = []IngestionDoc{
	{DocumentID: "d1", SemanticID: "a.md"},
	{DocumentID: "d2", SemanticID: "b.md", Link: "https://example.com/b"},
}

// assertOneGet asserts the single request of a happy-path ListDocs:
// the method, the path, and the Bearer header (the key appears in the
// request header only — the request carries no body).
func assertOneGet(t *testing.T, rec *checkRecorder) {
	t.Helper()
	if got := rec.count(http.MethodGet, "/onyx-api/ingestion"); got != 1 {
		t.Fatalf("GET /onyx-api/ingestion requests = %d, want 1", got)
	}
	if got := len(rec.all()); got != 1 {
		t.Fatalf("total requests = %d, want 1", got)
	}
	r := rec.at(0)
	if r.Method != http.MethodGet || r.Path != "/onyx-api/ingestion" {
		t.Fatalf("request = %s %s, want GET /onyx-api/ingestion", r.Method, r.Path)
	}
	if r.Auth != "Bearer "+testKey {
		t.Fatalf("Authorization = %q, want Bearer <key>", r.Auth)
	}
}

// TestListDocsBareArrayAndEnvelope: a 200 with a bare array and a 200
// with the {data: [...]} envelope both parse to the same documents
// (method, path, and Bearer header asserted; the key in no returned
// string); a valid empty array yields zero documents, not an error.
func TestListDocsBareArrayAndEnvelope(t *testing.T) {
	bodies := map[string]string{
		"bare array": listDocsBody,
		"envelope":   `{"data":` + listDocsBody + `}`,
		"empty":      `[]`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			rec := &checkRecorder{respond: func(method, path string, n int) (int, string) {
				if method == http.MethodGet && path == "/onyx-api/ingestion" {
					return 200, body
				}
				return 404, `{"detail":"not found"}`
			}}
			ts := startCheckServer(t, rec)

			docs, err := fastClient(t, ts).ListDocs(context.Background())
			if err != nil {
				t.Fatalf("ListDocs: %v", err)
			}
			want := wantDocs
			if name == "empty" {
				want = []IngestionDoc{}
			}
			if !reflect.DeepEqual(docs, want) {
				t.Fatalf("docs = %+v, want %+v", docs, want)
			}
			assertOneGet(t, rec)

			// The key appears in no returned string.
			for _, d := range docs {
				for _, s := range []string{d.DocumentID, d.SemanticID, d.Link} {
					if strings.Contains(s, testKey) {
						t.Fatalf("the API key leaked into a document field: %q", s)
					}
				}
			}
		})
	}
}

// TestListDocsMalformedBody: a 200 whose body is neither a bare array
// nor a {data: [...]} envelope is an error (the exit-1 class, wrapped
// in ErrUnreachable) carrying the body excerpt — never a silent empty
// list. It is a 200, so it is not retried.
func TestListDocsMalformedBody(t *testing.T) {
	bodies := map[string]string{
		"object without data": `{"foo":"bar"}`,
		"null data":           `{"data":null}`,
		"bare null":           `null`,
		"bare string":         `"not a list"`,
		"malformed json":      `not json at all`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			rec := &checkRecorder{respond: func(method, path string, n int) (int, string) {
				if method == http.MethodGet && path == "/onyx-api/ingestion" {
					return 200, body
				}
				return 404, `{"detail":"not found"}`
			}}
			ts := startCheckServer(t, rec)

			_, err := fastClient(t, ts).ListDocs(context.Background())
			if err == nil {
				t.Fatal("expected an error for a malformed 200 body, got nil (a silent empty list)")
			}
			if !errors.Is(err, ErrUnreachable) {
				t.Fatalf("err = %v, want ErrUnreachable (the exit-1 class)", err)
			}
			if !strings.Contains(err.Error(), "malformed document list") {
				t.Errorf("error does not name the malformed body: %v", err)
			}
			if !strings.Contains(err.Error(), "GET /onyx-api/ingestion") {
				t.Errorf("error does not name the endpoint: %v", err)
			}
			if !strings.Contains(err.Error(), ts.URL) {
				t.Errorf("error does not name the URL: %v", err)
			}
			if got := rec.count(http.MethodGet, "/onyx-api/ingestion"); got != 1 {
				t.Errorf("GET requests = %d, want 1 (a 200 is not retried)", got)
			}
			if strings.Contains(err.Error(), testKey) {
				t.Errorf("the API key leaked into the error: %v", err)
			}
		})
	}
}

// TestListDocsAuth: a 401 or 403 fails fast — the wrapped ErrAuth with
// the authHint advice (naming the variable and permission, never the
// key) — with exactly one request (no retry).
func TestListDocsAuth(t *testing.T) {
	for name, code := range map[string]int{"401": 401, "403": 403} {
		t.Run(name, func(t *testing.T) {
			rec := &checkRecorder{respond: func(method, path string, n int) (int, string) {
				return code, `{"detail":"denied"}`
			}}
			ts := startCheckServer(t, rec)

			_, err := fastClient(t, ts).ListDocs(context.Background())
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !errors.Is(err, ErrAuth) {
				t.Fatalf("err = %v, want ErrAuth", err)
			}
			for _, want := range []string{"ONYX_API_KEY", "manage:connectors"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not mention %q: %v", want, err)
				}
			}
			if strings.Contains(err.Error(), testKey) {
				t.Fatalf("the API key leaked into the error: %v", err)
			}
			if got := rec.count(http.MethodGet, "/onyx-api/ingestion"); got != 1 {
				t.Fatalf("GET requests = %d, want 1 (a 401/403 is not retried)", got)
			}
		})
	}
}

// TestListDocsNoAPI: a 404, a 405, or any other 4xx is a terminal
// ErrNoIngestionAPI (the exit-2 class) with exactly one request — no
// retry, and (unlike check) no POST fallback, since a POST cannot
// produce a list.
func TestListDocsNoAPI(t *testing.T) {
	for name, code := range map[string]int{"404": 404, "405": 405, "400": 400, "418": 418} {
		t.Run(name, func(t *testing.T) {
			rec := &checkRecorder{respond: func(method, path string, n int) (int, string) {
				return code, `{"detail":"no such endpoint"}`
			}}
			ts := startCheckServer(t, rec)

			_, err := fastClient(t, ts).ListDocs(context.Background())
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !errors.Is(err, ErrNoIngestionAPI) {
				t.Fatalf("err = %v, want ErrNoIngestionAPI", err)
			}
			for _, want := range []string{
				"GET /onyx-api/ingestion is not available at " + ts.URL,
				"ONYX_API_URL",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not mention %q: %v", want, err)
				}
			}
			if got := rec.count(http.MethodGet, "/onyx-api/ingestion"); got != 1 {
				t.Errorf("GET requests = %d, want 1 (other 4xx is not retried)", got)
			}
			if got := rec.count(http.MethodPost, "/onyx-api/ingestion"); got != 0 {
				t.Errorf("POST requests = %d, want 0 (ls has no 2f-style fallback)", got)
			}
			if strings.Contains(err.Error(), testKey) {
				t.Errorf("the API key leaked into the error: %v", err)
			}
		})
	}
}

// TestListDocsRetries: a 429 followed by a 200 succeeds on the second
// attempt (2 requests, millisecond backoff injection); a persistent
// 429 and a 500 ×3 exhaust the 3 attempts → the wrapped
// ErrUnreachable (the exit-1 class).
func TestListDocsRetries(t *testing.T) {
	t.Run("429 then 200", func(t *testing.T) {
		rec := &checkRecorder{respond: func(method, path string, n int) (int, string) {
			if n == 1 {
				return 429, `{"detail":"rate limited"}`
			}
			return 200, listDocsBody
		}}
		ts := startCheckServer(t, rec)

		docs, err := fastClient(t, ts).ListDocs(context.Background())
		if err != nil {
			t.Fatalf("ListDocs: %v", err)
		}
		if !reflect.DeepEqual(docs, wantDocs) {
			t.Fatalf("docs = %+v, want the two documents", docs)
		}
		if got := rec.count(http.MethodGet, "/onyx-api/ingestion"); got != 2 {
			t.Fatalf("GET requests = %d, want 2 (429 then 200)", got)
		}
	})

	for name, code := range map[string]int{"persistent 429": 429, "500 exhaust": 500} {
		t.Run(name, func(t *testing.T) {
			rec := &checkRecorder{respond: func(method, path string, n int) (int, string) {
				return code, `{"error":"boom"}`
			}}
			ts := startCheckServer(t, rec)

			_, err := fastClient(t, ts).ListDocs(context.Background())
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !errors.Is(err, ErrUnreachable) {
				t.Fatalf("err = %v, want ErrUnreachable", err)
			}
			if got := rec.count(http.MethodGet, "/onyx-api/ingestion"); got != maxAttempts {
				t.Fatalf("GET requests = %d, want %d (3 attempts)", got, maxAttempts)
			}
			if strings.Contains(err.Error(), testKey) {
				t.Errorf("the API key leaked into the error: %v", err)
			}
		})
	}
}

// TestListDocsUnreachable: a connection failure to a dead port retries
// (3 attempts, millisecond backoff) → the wrapped ErrUnreachable.
func TestListDocsUnreachable(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close() // port now refuses connections

	c := NewClient("http://"+addr, testKey, 0)
	c.backoff = []time.Duration{time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond}
	_, err = c.ListDocs(context.Background())
	if err == nil {
		t.Fatal("expected an error for a dead port, got nil")
	}
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
	if strings.Contains(err.Error(), testKey) {
		t.Errorf("the API key leaked into the error: %v", err)
	}
}

// TestListDocsContextCanceled: cancellation mid-flight (a request held
// open) returns the context's error — no sentinel wrap.
func TestListDocsContextCanceled(t *testing.T) {
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
		_, err := fastClient(t, ts).ListDocs(ctx)
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

// TestListDocsNeverLeaksKey covers the T12a leak criterion: across a
// mix of outcomes (bare array, envelope, empty, malformed, 401, 403,
// 404, 429/500 exhaustion, a dead port) the API key appears in no
// error string, no returned document field, and no request body.
func TestListDocsNeverLeaksKey(t *testing.T) {
	logs := captureLogs(t)
	checks := 0
	check := func(what, s string) {
		checks++
		if strings.Contains(s, testKey) {
			t.Errorf("API key leaked into %s: %q", what, s)
		}
	}

	run := func(t *testing.T, respond func(n int) (int, string)) (*checkRecorder, []IngestionDoc, error) {
		t.Helper()
		rec := &checkRecorder{respond: func(method, path string, n int) (int, string) {
			if method == http.MethodGet && path == "/onyx-api/ingestion" {
				return respond(n)
			}
			return 404, `{"detail":"not found"}`
		}}
		ts := startCheckServer(t, rec)
		docs, err := fastClient(t, ts).ListDocs(context.Background())
		return rec, docs, err
	}

	cases := map[string]func(n int) (int, string){
		"bare array":    func(n int) (int, string) { return 200, listDocsBody },
		"envelope":      func(n int) (int, string) { return 200, `{"data":` + listDocsBody + `}` },
		"empty":         func(n int) (int, string) { return 200, `[]` },
		"malformed":     func(n int) (int, string) { return 200, `{"foo":"bar"}` },
		"401":           func(n int) (int, string) { return 401, `{"detail":"unauthorized"}` },
		"403":           func(n int) (int, string) { return 403, `{"detail":"forbidden"}` },
		"404":           func(n int) (int, string) { return 404, `{"detail":"not found"}` },
		"429 exhaust":   func(n int) (int, string) { return 429, `{"detail":"rate limited"}` },
		"500 exhaust":   func(n int) (int, string) { return 500, `{"error":"boom"}` },
		"retry then ok": func(n int) (int, string) {
			if n == 1 {
				return 429, `{"detail":"rate limited"}`
			}
			return 200, listDocsBody
		},
	}

	for name, respond := range cases {
		rec, docs, err := run(t, respond)
		if err != nil {
			check("error: "+name, err.Error())
		}
		for _, d := range docs {
			check(fmt.Sprintf("document fields: %s", name), d.DocumentID+d.SemanticID+d.Link)
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
	c := NewClient("http://"+addr, testKey, 0)
	c.backoff = []time.Duration{time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond}
	if _, err := c.ListDocs(context.Background()); err != nil {
		check("error: unreachable", err.Error())
	} else {
		t.Fatal("expected an error for a dead port")
	}

	check("log output", logs.String())
	if checks == 0 {
		t.Fatal("no strings were checked")
	}
}
