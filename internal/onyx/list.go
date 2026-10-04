// Copyright (C) 2026 Joakim Nohlgård
// SPDX-License-Identifier: AGPL-3.0

// The sard ls data source (docs/PLAN.md §14, task T12a): ListDocs
// issues the single read-only GET /onyx-api/ingestion — the read-only
// sibling of the Ingestion POST a real run uses (docs/
// onyx-ingestion-api.md, "Other endpoints") — and returns the
// documents the API key can see: document_id (the stable ID sard
// generates, §6), semantic_id (the name shown in the Onyx UI), and
// link (the source link, when one was sent).
package onyx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// IngestionDoc is one document the API key can see, as reported by
// GET /onyx-api/ingestion.
type IngestionDoc struct {
	DocumentID string `json:"document_id"`
	SemanticID string `json:"semantic_id"`
	Link       string `json:"link"`
}

// ListDocs lists the documents the API key can see (docs/PLAN.md §14.2):
// one GET /onyx-api/ingestion with Bearer auth under the shared 10 s
// probe deadline (doProbe), in server order.
//
// Outcomes: a 200 is parsed into the document list, tolerating both a
// bare array and a {data: [...]} envelope (schema drift, like
// parseCCPairBody); a 200 whose body matches neither is an error
// carrying the body excerpt — never a silent empty list (the exit-1
// signal, wrapped in ErrUnreachable, so the CLI's mapping is the same
// as for an unreachable server). 401/403 fail fast (no retry): the
// wrapped ErrAuth with the authHint advice, the same classification as
// the ingest fail-fast and check. 404/405 or any other 4xx is not
// retried and carries no POST fallback (unlike check: a POST cannot
// produce a list) — the wrapped ErrNoIngestionAPI (the exit-2 signal:
// the deployment may predate the endpoint, or ONYX_API_URL is
// misconfigured). 429, 5xx, and connection/timeout errors are retried
// up to 3 attempts with the shared injectable backoff (c.sleep);
// exhausted → the wrapped ErrUnreachable. A canceled context returns
// the context's error (the exit-130 signal).
//
// The API key never appears in any returned string.
func (c *Client) ListDocs(ctx context.Context) ([]IngestionDoc, error) {
	endpoint := "GET /onyx-api/ingestion"
	for attempt := 1; ; attempt++ {
		docs, reason, retryable, err := c.attemptListDocs(ctx)
		if err != nil {
			return nil, err // 401/403 (ErrAuth), other 4xx (ErrNoIngestionAPI), or context cancellation
		}
		if docs != nil {
			return docs, nil
		}
		if !retryable || attempt == maxAttempts {
			// A non-retryable failure (a 200 with a body matching
			// neither accepted shape) or an exhausted retry budget:
			// both are the exit-1 class.
			return nil, c.unreachableError(endpoint, reason)
		}
		if !c.sleep(ctx, attempt-1) {
			return nil, ctx.Err()
		}
	}
}

// attemptListDocs performs one GET /onyx-api/ingestion and maps the
// outcome. It reports the parsed documents (nil unless a 200 parsed
// into a list), the last reason (for the exhausted message), whether
// the failure is retryable, and — when non-nil — a fatal error (401/403
// wraps ErrAuth; a canceled context is returned as-is).
func (c *Client) attemptListDocs(ctx context.Context) (docs []IngestionDoc, reason string, retryable bool, err error) {
	res, err := c.doProbe(ctx, http.MethodGet, "/onyx-api/ingestion", nil, true)
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", false, ctx.Err()
		}
		// Connection failure or the 10-second probe deadline: retryable.
		return nil, err.Error(), true, nil
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, maxBodyBytes))

	switch {
	case res.StatusCode == http.StatusOK:
		parsed, ok := parseDocListBody(raw)
		if !ok {
			// A 200 whose body is neither a bare array nor a
			// {data: [...]} envelope: an error, not a silent empty
			// list (docs/PLAN.md §14.2). It is a 200, so it is not
			// retried — the caller wraps it in ErrUnreachable (exit 1).
			return nil, "malformed document list: " + excerpt(raw), false, nil
		}
		return parsed, "", false, nil

	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		// Fail fast: the same classification as the ingest fail-fast
		// and check probe 2.
		return nil, "", false, authError(res.StatusCode)

	case res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= http.StatusInternalServerError:
		return nil, httpReason(res.StatusCode, raw), true, nil

	default:
		// 404/405 (the deployment predates the GET) or any other 4xx
		// (a misconfigured base): no retry, and no 2f-style POST
		// fallback — unlike check, a POST cannot produce a list, so
		// this is terminal (docs/PLAN.md §14.2).
		return nil, "", false, fmt.Errorf(
			"GET /onyx-api/ingestion is not available at %s — the deployment may predate the endpoint, or ONYX_API_URL is misconfigured (HTTP %d: %s): %w",
			c.apiURL, res.StatusCode, excerpt(raw), ErrNoIngestionAPI)
	}
}

// parseDocListBody parses a 200 body from GET /onyx-api/ingestion,
// tolerating both a bare array and a {data: [...]} envelope (schema
// drift, like parseCCPairBody). It reports ok = false for a body
// matching neither — a bare null or other scalar, a JSON object
// without a data array, or malformed JSON — so the caller returns an
// error instead of silently an empty list (docs/PLAN.md §14.2). A
// valid empty array (or {"data": []}) is ok with zero documents.
func parseDocListBody(raw []byte) (docs []IngestionDoc, ok bool) {
	trimmed := bytes.TrimSpace(raw)
	switch {
	case len(trimmed) > 0 && trimmed[0] == '[':
		if err := json.Unmarshal(trimmed, &docs); err == nil {
			return docs, true
		}
	case len(trimmed) > 0 && trimmed[0] == '{':
		var env struct {
			Data []IngestionDoc `json:"data"`
		}
		if json.Unmarshal(trimmed, &env) == nil && env.Data != nil {
			return env.Data, true
		}
	}
	return nil, false
}
