// Package onyx is the HTTP client for the Onyx Ingestion API
// (docs/PLAN.md §7, task T6).
//
// TODO (T6): NewClient(apiURL, apiKey string, ccPairID int) and
// Ingest(ctx, payload) — Bearer auth, 30 s timeout, up to 3 attempts with
// 1 s / 4 s / 16 s backoff on 429, 5xx, and connection/timeout errors (never
// other 4xx), fail-fast on 401/403, result mapping (200 → created/updated
// from already_existed), and never log the API key.
package onyx
