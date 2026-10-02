// Copyright (C) 2026 Joakim Nohlgård
// SPDX-License-Identifier: AGPL-3.0

// The sard check pre-flight (docs/PLAN.md §13, task T11a): Check
// verifies the environment a real run needs — server reachability, an
// API key that carries the Ingestion API's permission requirement, and
// (best-effort) the configured cc-pair's existence — without creating,
// updating, or deleting any document (docs/PLAN.md §13.3 proves it).
package onyx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// probeTimeout bounds each probe request (docs/PLAN.md §13.2: a 10 s
// deadline per probe), inside the client's 30 s http.Client backstop.
const probeTimeout = 10 * time.Second

// fallbackBody is the deliberately-invalid body of probe 2f: the
// Ingestion API's schema requires the document object, so the body is
// rejected before any handler logic runs and no document can be created
// (docs/PLAN.md §13.3). It must stay byte-exact "{}".
const fallbackBody = "{}"

// ccpairPath is probe 3's endpoint (docs/onyx-ingestion-api.md,
// "Other endpoints").
const ccpairPath = "/manage/admin/cc-pair/%d"

// CCPairInfo is the configured cc-pair as reported by probe 3.
type CCPairInfo struct {
	ID     int
	Name   string
	Status string
	Docs   int
}

// CheckResult is what Check found (docs/PLAN.md §13.6).
type CheckResult struct {
	Healthy      bool        // probe 1: 200 with success=true
	KeyOK        bool        // probe 2 or 2f accepted the key
	DocCount     int         // documents visible to the key (probe 2 only)
	CCPair       *CCPairInfo // nil = not validated (the report warns)
	UsedFallback bool        // true = probe 2f (the invalid POST) was used
}

// ErrUnreachable is returned (wrapped) by Check when the server cannot
// be reached: a connection failure or timeout (probe 1's single attempt,
// or probes 2/2f after 3 attempts) or a persistent 429/5xx. It is the
// exit-1 signal; callers match it with errors.Is (docs/PLAN.md §13.4).
var ErrUnreachable = errors.New("Onyx server unreachable")

// ErrCCPairNotFound is returned (wrapped) by Check when probe 3 reports
// 404 on a deployment where the GET /onyx-api/ingestion endpoint exists
// (the deployment is modern, so the endpoint is known to exist and the
// 404 means the id does not): a real run would fail *silently*, because
// the Ingestion POST accepts a bogus id. Callers match it with
// errors.Is (docs/PLAN.md §13.4, exit 2).
var ErrCCPairNotFound = errors.New("cc-pair-id not found on this deployment")

// ErrNoIngestionAPI is returned (wrapped) by Check when probe 2f's POST
// is answered with a non-auth 4xx: the configured URL has no Onyx
// Ingestion API at all (a misconfigured base, e.g. a missing trailing
// /api). Callers match it with errors.Is (docs/PLAN.md §13.4, exit 2).
var ErrNoIngestionAPI = errors.New("no Onyx Ingestion API at this URL")

// keyVerdict is the outcome of one key probe request (probes 2/2f).
type keyVerdict int

const (
	keyVerdictNone keyVerdict = iota
	keyVerdictAccepted
	keyVerdictToFallback // GET 403/404/405/other-4xx: the POST decides
	keyVerdictNoAPI      // 2f: a non-auth 4xx — no Ingestion API at this URL
)

// Check verifies the environment a real run needs, without creating,
// updating, or deleting any document (docs/PLAN.md §13.2, §13.3).
//
// It runs three probes in order:
//
//  1. GET /health — no auth, a single un-retried attempt. A connection
//     failure/timeout stops the check (ErrUnreachable); any other
//     non-200 (incl. 404/405 on older deployments) is not an error —
//     Healthy stays false and probe 2 doubles as the reachability test.
//  2. GET /onyx-api/ingestion — Bearer; the read-only sibling of the
//     POST a real run uses, with the same key and the same
//     manage:connectors/admin requirement, so a 200 is specifically a
//     statement about the key's ingestion ability (DocCount = the
//     document list). 401 → ErrAuth. 403 (the key may lack only this
//     list endpoint's permission), 404/405 (a deployment predating the
//     GET), or any other 4xx → 2f. 429/5xx/connection → up to 3 attempts
//     with the shared injectable backoff; exhausted → ErrUnreachable.
//     2f. POST /onyx-api/ingestion with the deliberately-invalid {} body —
//     the endpoint a real run uses, so its verdict wins: 401/403 →
//     ErrAuth; 400/422 (the schema rejected the body) or an unexpected
//     200 → key accepted (UsedFallback); any other 4xx →
//     ErrNoIngestionAPI; 429/5xx/connection → retried as above.
//  3. GET /manage/admin/cc-pair/{id} — Bearer, best-effort, not retried
//     (a 429 there downgrades to a warning). 200 → CCPair populated;
//     404 → ErrCCPairNotFound only when probe 2 was the successful GET
//     (a modern deployment), otherwise a warning (the caller); 403 or
//     anything else → no error.
//
// Errors: the CLI maps them with errors.Is (docs/PLAN.md §13.4) —
// ErrUnreachable → 1; ErrAuth, ErrNoIngestionAPI, ErrCCPairNotFound →
// 2; the context's error (cancellation) → 130. The API key never
// appears in any returned string.
func (c *Client) Check(ctx context.Context) (CheckResult, error) {
	// Probe 1: reachability (no auth, a single un-retried attempt).
	healthy, err := c.probeHealth(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return CheckResult{}, ctx.Err()
		}
		return CheckResult{}, fmt.Errorf("Onyx at %s: GET /health: %s: %w", c.apiURL, err, ErrUnreachable)
	}
	res := CheckResult{Healthy: healthy}

	// Probe 2 (or the 2f fallback): credentials.
	keyOK, usedFallback, docCount, err := c.probeKey(ctx)
	if err != nil {
		return res, err
	}
	res.KeyOK = keyOK
	res.DocCount = docCount
	res.UsedFallback = usedFallback

	// Probe 3: cc-pair (best-effort, not retried). A 404 is an error
	// only when probe 2 was the GET (a modern deployment where the
	// endpoint is known to exist); in the fallback flow an absent
	// endpoint is indistinguishable from an absent pair.
	pair, err := c.probeCCPair(ctx, !usedFallback)
	if err != nil {
		return res, err
	}
	res.CCPair = pair
	return res, nil
}

// doProbe runs one probe request under a 10 s deadline (inside the
// client's 30 s http.Client backstop). On a transport error it returns
// the caller context's error when it is canceled (the caller maps that
// to exit 130), otherwise a plain error the caller maps to
// ErrUnreachable. A non-nil body gets Content-Type: application/json;
// auth adds the Bearer header.
func (c *Client) doProbe(ctx context.Context, method, path string, body []byte, auth bool) (*http.Response, error) {
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(pctx, method, c.apiURL+path, reader)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("building request: %v", err)
	}
	if auth {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("connection error: %v", err)
	}
	return res, nil
}

// probeHealth is probe 1: GET /health — no auth, a single un-retried
// attempt under a 10 s deadline (docs/PLAN.md §13.2). A connection
// failure/timeout is an error (the caller wraps it in ErrUnreachable);
// any non-200 — incl. 404/405 on older deployments without the
// endpoint — is not an error: Healthy stays false and the caller warns
// and continues, since probe 2 doubles as the reachability test. A 200
// is Healthy only when the body's success field is true.
func (c *Client) probeHealth(ctx context.Context) (healthy bool, err error) {
	res, err := c.doProbe(ctx, http.MethodGet, "/health", nil, false)
	if err != nil {
		return false, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, maxBodyBytes))

	if res.StatusCode != http.StatusOK {
		return false, nil
	}
	var out struct {
		Success bool `json:"success"`
	}
	_ = json.Unmarshal(raw, &out) // malformed body: falsy → not healthy
	return out.Success, nil
}

// probeKey is probe 2 (and its 2f fallback): it verifies that the API
// key carries the Ingestion API's permission requirement (docs/PLAN.md
// §13.2). The primary path is GET /onyx-api/ingestion; a 403, a
// 404/405, or any other 4xx (except 401) falls through to 2f — the
// POST a real run uses — whose verdict wins. 401 (on either probe) →
// ErrAuth; 429/5xx/connection → up to 3 attempts with the shared
// injectable backoff, exhausted → ErrUnreachable; a canceled context is
// returned as the context's error.
func (c *Client) probeKey(ctx context.Context) (keyOK, usedFallback bool, docCount int, err error) {
	endpoint := "GET /onyx-api/ingestion"
	for attempt := 1; ; attempt++ {
		verdict, count, retryable, reason, err := c.attemptKeyGet(ctx)
		if err != nil {
			return false, false, 0, err
		}
		switch verdict {
		case keyVerdictAccepted:
			return true, false, count, nil
		case keyVerdictToFallback:
			return c.probeKeyFallback(ctx)
		}
		if !retryable || attempt == maxAttempts {
			return false, false, 0, c.unreachableError(endpoint, reason)
		}
		if !c.sleep(ctx, attempt-1) {
			return false, false, 0, ctx.Err()
		}
	}
}

// attemptKeyGet is one GET /onyx-api/ingestion. It reports the verdict
// (accepted / to-fallback / none-retryable), the document count (200
// only), whether the failure is retryable, the last reason (for the
// exhausted message), and — when non-nil — a fatal error (401 wraps
// ErrAuth; a canceled context is returned as-is).
func (c *Client) attemptKeyGet(ctx context.Context) (verdict keyVerdict, docCount int, retryable bool, reason string, err error) {
	res, err := c.doProbe(ctx, http.MethodGet, "/onyx-api/ingestion", nil, true)
	if err != nil {
		if ctx.Err() != nil {
			return 0, 0, false, "", ctx.Err()
		}
		return 0, 0, true, err.Error(), nil
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, maxBodyBytes))

	switch {
	case res.StatusCode == http.StatusOK:
		var docs []struct {
			DocumentID string `json:"document_id"`
		}
		_ = json.Unmarshal(raw, &docs) // malformed body: 0 documents
		return keyVerdictAccepted, len(docs), false, "", nil

	case res.StatusCode == http.StatusUnauthorized:
		return 0, 0, false, "", authError(res.StatusCode)

	case res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= http.StatusInternalServerError:
		return 0, 0, true, httpReason(res.StatusCode, raw), nil

	default:
		// 403 (the key may lack only this list endpoint's permission —
		// a Group Manager), 404/405 (the deployment predates the GET),
		// or any other 4xx: disambiguate via 2f — the endpoint a real
		// run uses, whose verdict wins.
		return keyVerdictToFallback, 0, false, httpReason(res.StatusCode, raw), nil
	}
}

// probeKeyFallback is probe 2f: POST /onyx-api/ingestion with the
// deliberately-invalid {} body (docs/PLAN.md §13.2, §13.3). It cannot
// create a document: the schema requires the document object, so the
// body is rejected before any handler logic runs (and Onyx evaluates
// Bearer auth before parsing the body). 401/403 → ErrAuth; 400/422
// (the endpoint exists, auth passed, the body was rejected) or an
// unexpected 200 → key accepted (UsedFallback); any other 4xx (404,
// 405, …) → ErrNoIngestionAPI; 429/5xx/connection → up to 3 attempts
// with the shared backoff, exhausted → ErrUnreachable.
func (c *Client) probeKeyFallback(ctx context.Context) (keyOK, usedFallback bool, docCount int, err error) {
	endpoint := "POST /onyx-api/ingestion"
	for attempt := 1; ; attempt++ {
		verdict, reason, fatal, retryable := c.attemptKeyPost(ctx)
		if fatal != nil {
			return false, false, 0, fatal
		}
		switch verdict {
		case keyVerdictAccepted:
			return true, true, 0, nil
		case keyVerdictNoAPI:
			return false, false, 0, fmt.Errorf("no Onyx Ingestion API at %s (%s: %s) — check ONYX_API_URL: %w",
				c.apiURL, endpoint, reason, ErrNoIngestionAPI)
		}
		if !retryable || attempt == maxAttempts {
			return false, false, 0, c.unreachableError(endpoint, reason)
		}
		if !c.sleep(ctx, attempt-1) {
			return false, false, 0, ctx.Err()
		}
	}
}

// attemptKeyPost is one 2f POST with the {} body. It reports the
// verdict (accepted / no-API / retryable), the last reason, and — when
// non-nil — a fatal error (401/403 wraps ErrAuth; a canceled context is
// returned as-is).
func (c *Client) attemptKeyPost(ctx context.Context) (verdict keyVerdict, reason string, fatal error, retryable bool) {
	res, err := c.doProbe(ctx, http.MethodPost, "/onyx-api/ingestion", []byte(fallbackBody), true)
	if err != nil {
		if ctx.Err() != nil {
			return 0, "", ctx.Err(), false
		}
		return 0, err.Error(), nil, true
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, maxBodyBytes))

	switch {
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		return 0, "", authError(res.StatusCode), false

	case res.StatusCode == http.StatusOK ||
		res.StatusCode == http.StatusBadRequest ||
		res.StatusCode == http.StatusUnprocessableEntity:
		// 400/422: the endpoint exists, auth passed, the body was
		// rejected → key accepted. 200: the key is accepted (the
		// verifiable fact) — the caller warns that a 200 for an
		// invalid body is unexpected.
		return keyVerdictAccepted, "", nil, false

	case res.StatusCode >= http.StatusBadRequest && res.StatusCode < http.StatusInternalServerError:
		// 404/405/other: no Ingestion API at this URL (a misconfigured
		// base, e.g. a missing trailing /api).
		return keyVerdictNoAPI, httpReason(res.StatusCode, raw), nil, false

	default: // 429 or 5xx
		return 0, httpReason(res.StatusCode, raw), nil, true
	}
}

// probeCCPair is probe 3: GET /manage/admin/cc-pair/{id} — Bearer auth,
// best-effort, not retried (a 429 there downgrades to a warning; the
// verdict is best-effort by design). It runs whenever the key was
// accepted (docs/PLAN.md §13.2). 200 → the pair's name, status, and
// document count (tolerating both the {success, data} envelope and a
// bare object). 404 → ErrCCPairNotFound only when modern is true (probe
// 2 was the successful GET, so the endpoint is known to exist and the
// 404 means the id does not); in the fallback flow an absent endpoint
// is indistinguishable from an absent pair → no error (the caller
// warns). 403 or anything else → no error (the key may lack the scope
// for this endpoint while having passed the one that matters for
// ingestion). A canceled context is returned as the context's error.
func (c *Client) probeCCPair(ctx context.Context, modern bool) (*CCPairInfo, error) {
	res, err := c.doProbe(ctx, http.MethodGet, fmt.Sprintf(ccpairPath, c.ccPairID), nil, true)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, nil // transport failure: best-effort → the caller warns
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, maxBodyBytes))

	switch {
	case res.StatusCode == http.StatusOK:
		body := parseCCPairBody(raw)
		info := CCPairInfo{ID: c.ccPairID, Name: body.Name, Status: body.Status, Docs: body.NumDocsIndexed}
		return &info, nil

	case res.StatusCode == http.StatusNotFound && modern:
		return nil, fmt.Errorf("%s: cc-pair-id %d not found on this deployment — copy it from the connector's Admin Panel URL: %w",
			c.apiURL, c.ccPairID, ErrCCPairNotFound)

	default:
		// 403 (the key lacks the scope for this endpoint), 404 (the
		// fallback flow), 429, 5xx, …: best-effort probe — the caller
		// warns, the check continues.
		return nil, nil
	}
}

// ccpairBody is the pair's core fields as returned by probe 3 (either
// the {success, data: {…}} envelope's data object or a bare object —
// the reference documents the fields but the envelope shape can drift).
type ccpairBody struct {
	Name           string `json:"name"`
	Status         string `json:"status"`
	NumDocsIndexed int    `json:"num_docs_indexed"`
}

// parseCCPairBody tolerates both the {success, data: {…}} envelope and
// a bare object.
func parseCCPairBody(raw []byte) (out ccpairBody) {
	var env struct {
		Data *ccpairBody `json:"data"`
	}
	if json.Unmarshal(raw, &env) == nil && env.Data != nil {
		return *env.Data
	}
	_ = json.Unmarshal(raw, &out) // bare shape (or malformed → zero)
	return out
}

// authError is the wrapped ErrAuth for a 401/403 on a key probe,
// carrying the status code and the authHint advice (never the key).
func authError(code int) error {
	return fmt.Errorf("Onyx returned %d: %s: %w", code, authHint(code), ErrAuth)
}

// unreachableError is the wrapped ErrUnreachable for probes 2/2f after
// retries are exhausted (or for a non-retryable failure of the same
// class), carrying the endpoint and the last reason.
func (c *Client) unreachableError(endpoint, reason string) error {
	return fmt.Errorf("Onyx at %s: %s: %s: %w", c.apiURL, endpoint, reason, ErrUnreachable)
}

// httpReason renders a failure reason from a non-200 response: the
// status code plus a short body excerpt (the existing excerpt
// discipline, truncated at maxExcerptLen).
func httpReason(code int, body []byte) string {
	return fmt.Sprintf("HTTP %d: %s", code, excerpt(body))
}
