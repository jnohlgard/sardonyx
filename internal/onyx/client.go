// Copyright (C) 2026 Joakim Nohlgård
// SPDX-License-Identifier: AGPL-3.0

// Package onyx is the HTTP client for the Onyx Ingestion API
// (docs/PLAN.md §7, task T6; docs/onyx-ingestion-api.md).
//
// NewClient builds a Client for an Onyx API base URL — the config layer
// guarantees one (default https://cloud.onyx.app/api; a trailing "/" is
// tolerated and stripped) — and an API key. Ingest POSTs one document per
// call to {apiURL}/onyx-api/ingestion with Bearer auth and a 30-second
// timeout.
//
// Each call makes up to 3 attempts, retrying on 429, 5xx, and
// connection/timeout errors with exponential backoff (1 s, 4 s, 16 s);
// the context is checked before each attempt and between retries, so
// Ctrl-C aborts promptly, and other 4xx statuses fail immediately. A 200
// means the document was *accepted* (indexing is asynchronous): the
// response's already_existed flag maps to updated, its absence to
// created. 401/403 fail fast — the result is failed and the error wraps
// ErrAuth so the caller can stop the run, since every later document
// would fail the same way. Any other failure returns a failed result
// with a nil error (a per-document failure that never aborts the run);
// the reason carries the status code plus a short body excerpt.
//
// cc_pair_id: Ingest sends the payload's own CCPairID — the transform
// stamps it from the same Settings.CCPairID, so the payload is the
// single source of truth on the wire. The Client holds no cc-pair of
// its own; the only cc-pair it consumes is the ccPairID argument to
// Check (probe 3, below).
//
// Backoff: the wait schedule lives in an unexported field (default
// 1 s / 4 s / 16 s per §7) so tests can inject millisecond waits and the
// suite stays fast; production clients keep the full schedule. With the
// 3-attempt cap only the first two waits fire (1 s, 4 s); the third
// entry (16 s) is kept so a raised cap has its slot. Check (check.go,
// task T11a) reuses the same field for its probes.
//
// Check is the sard check pre-flight (docs/PLAN.md §13): three
// read-only probes — GET /health, GET /onyx-api/ingestion (with the
// deliberately-invalid POST fallback for older deployments), and GET
// /manage/admin/cc-pair/{id} — that verify reachability, the key's
// ingestion permission, and the cc-pair's existence without creating,
// updating, or deleting any document. It fails via the ErrUnreachable,
// ErrNoIngestionAPI, and ErrCCPairNotFound sentinels (plus ErrAuth for
// a 401/403) so the CLI can map them to exit codes 1 / 2 (2 / 2); its
// ccPairID argument is what probe 3 queries.
//
// ListDocs is the sard ls data source (docs/PLAN.md §14): a single
// read-only GET /onyx-api/ingestion — the read-only sibling of the
// POST a real run uses — under the same 10 s probe deadline and
// Bearer auth. It returns the documents the key can see
// (document_id, semantic_id, link) in server order; a 200 whose body
// is neither a bare array nor a {data: [...]} envelope is an error
// (carrying the body excerpt), never a silent empty list. 401/403
// fail fast via ErrAuth; 404/405 and any other 4xx are terminal via
// ErrNoIngestionAPI (no POST fallback — a POST cannot produce a
// list); 429, 5xx, and connection/timeout errors retry with the
// shared backoff, exhausted into ErrUnreachable. The exit-code
// signals: ErrUnreachable → 1 (including the malformed-200 case),
// ErrAuth / ErrNoIngestionAPI → 2, the context's error → 130.
//
// The API key is never logged or included in error text or reasons.
package onyx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"sardonyx/internal/models"
)

const (
	// maxAttempts is the total number of POST attempts per document
	// (docs/PLAN.md §7: "up to 3 attempts").
	maxAttempts = 3

	// defaultTimeout bounds each request end-to-end (docs/PLAN.md §7).
	defaultTimeout = 30 * time.Second

	// maxBodyBytes caps how much of a response body is read into memory.
	maxBodyBytes = 1 << 20 // 1 MiB

	// maxExcerptLen truncates response-body excerpts in failure reasons.
	maxExcerptLen = 200
)

// defaultBackoff is the exponential retry schedule (docs/PLAN.md §7).
var defaultBackoff = []time.Duration{time.Second, 4 * time.Second, 16 * time.Second}

// ErrAuth is returned (wrapped) by Ingest when the API rejects the
// credentials (401/403). It is the fail-fast signal that the run should
// stop; callers match it with errors.Is (docs/PLAN.md §7).
var ErrAuth = errors.New("Onyx rejected the API key")

// Client is a client for the Onyx Ingestion API.
type Client struct {
	apiURL  string
	apiKey  string
	http    *http.Client
	backoff []time.Duration // retry waits (NewClient: defaultBackoff; tests override)
}

// NewClient builds a Client for an Onyx API base URL and Bearer
// credentials. A trailing "/" on apiURL is stripped before joining the
// /onyx-api/ingestion path. The client holds no cc-pair: Ingest sends
// the payload's own CCPairID, and Check takes the id as an argument.
func NewClient(apiURL, apiKey string) *Client {
	return &Client{
		apiURL:  strings.TrimRight(apiURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: defaultTimeout},
		backoff: defaultBackoff,
	}
}

// Ingest POSTs one document to the Onyx Ingestion API and maps the
// response to a per-document result (docs/PLAN.md §7).
//
// A 200 means the document was accepted (indexing is asynchronous); the
// response's already_existed flag distinguishes updated from created.
// 429, 5xx, and connection/timeout errors are retried — up to 3 attempts
// total, with the backoff waits between them; other 4xx statuses fail
// immediately. 401/403 fail fast: the result is failed and the error
// wraps ErrAuth. Context cancellation returns a failed result with the
// context's error. Any other failure returns a failed result with a nil
// error; the reason carries the status code and a short body excerpt.
// The API key never appears in logs, errors, or reasons.
func (c *Client) Ingest(ctx context.Context, payload models.OnyxPayload) (models.IngestResult, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return models.IngestResult{
			Status: models.ResultFailed,
			Reason: "encoding payload: " + err.Error(),
		}, err
	}

	for attempt := 1; ; attempt++ {
		result, retryable, err := c.attempt(ctx, body)
		if err != nil {
			return result, err // 401/403 (fail fast) or context cancellation
		}
		if !retryable || attempt == maxAttempts {
			return result, nil
		}
		if !c.sleep(ctx, attempt-1) {
			return interrupted(ctx.Err())
		}
	}
}

// attempt performs one POST and maps the outcome. It reports whether the
// failure is retryable and, when non-nil, the error to surface to the
// caller (401/403 wrap ErrAuth; a canceled context is returned as-is).
func (c *Client) attempt(ctx context.Context, body []byte) (models.IngestResult, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL+"/onyx-api/ingestion", bytes.NewReader(body))
	if err != nil {
		if ctx.Err() != nil {
			r, e := interrupted(ctx.Err())
			return r, false, e
		}
		return models.IngestResult{
			Status: models.ResultFailed,
			Reason: "building request: " + err.Error(),
		}, false, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	res, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			r, e := interrupted(ctx.Err())
			return r, false, e
		}
		// Connection failure or the 30-second request timeout: retryable.
		return models.IngestResult{
			Status: models.ResultFailed,
			Reason: "connection error: " + err.Error(),
		}, true, nil
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, maxBodyBytes))

	switch res.StatusCode {
	case http.StatusOK:
		var out struct {
			AlreadyExisted bool `json:"already_existed"`
		}
		_ = json.Unmarshal(raw, &out) // malformed body: falsy → created
		status := models.ResultCreated
		if out.AlreadyExisted {
			status = models.ResultUpdated
		}
		return models.IngestResult{Status: status}, false, nil

	case http.StatusUnauthorized, http.StatusForbidden:
		reason := fmt.Sprintf("Onyx returned %d: %s", res.StatusCode, authHint(res.StatusCode))
		return models.IngestResult{
			Status: models.ResultFailed,
			Reason: reason,
		}, false, fmt.Errorf("%s: %w", reason, ErrAuth)
	}

	reason := fmt.Sprintf("Onyx returned HTTP %d: %s", res.StatusCode, excerpt(raw))
	retryable := res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= http.StatusInternalServerError
	return models.IngestResult{
		Status: models.ResultFailed,
		Reason: reason,
	}, retryable, nil
}

// sleep waits before the next retry, honoring context cancellation; it
// reports whether the wait completed. i indexes the backoff schedule by
// the number of failed attempts so far (first failure → backoff[0]);
// beyond the schedule's end the last entry applies.
func (c *Client) sleep(ctx context.Context, i int) bool {
	if i >= len(c.backoff) {
		i = len(c.backoff) - 1
	}
	t := time.NewTimer(c.backoff[i])
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// interrupted reports a failure caused by context cancellation (e.g.
// Ctrl-C) along with the context's error.
func interrupted(err error) (models.IngestResult, error) {
	return models.IngestResult{
		Status: models.ResultFailed,
		Reason: "interrupted: " + err.Error(),
	}, err
}

// authHint is the actionable advice for a 401/403 (docs/PLAN.md §7): it
// names the variable and the required permission, but never the key.
func authHint(code int) string {
	what := "invalid or expired credentials"
	if code == http.StatusForbidden {
		what = "the key lacks the required permission for this CC-pair"
	}
	return what + "; check ONYX_API_KEY and that it has the `manage:connectors` or `admin` permission"
}

// excerpt trims and truncates a response body for inclusion in a failure
// reason (docs/PLAN.md §7: "status code + body excerpt").
func excerpt(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) <= maxExcerptLen {
		return s
	}
	cut := maxExcerptLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
