# AGENTS.md

`sard` — a Go CLI (module `sardonyx`) that ingests markdown files from a
git repo URL or a local directory into Onyx via the Ingestion API.

## Status
- T1–T9 are complete (config, models, transform, local directory source,
  git repository source, Onyx client, CLI wiring, summary & polish —
  progress lines, summary with elapsed time and failure list, and the
  source layer's skipped count; plus test hardening — edge-case tests
  for CRLF, non-UTF-8, oversize, monorepo depth, and .mdx frontmatter,
  and the two URL-normalization coverage gaps closed). Next up is
  T10 (build & README), in `docs/PLAN.md` order. No stub packages remain.

## Naming
- Repo/Go module is `sardonyx`; the binary and command are `sard`.
  (Never the old name "onyx-git-ingest".)

## Read on demand
- `docs/PLAN.md` — architecture, CLI spec, document-ID strategy, task list, tests.
- `docs/onyx-ingestion-api.md` — Onyx API auth, payload schema, response semantics.
- `README.md` — user-facing overview.

## Rules
- Go; stdlib-first — the only external dependencies are `godotenv` (`.env`
  loading) and `doublestar/v4` (globs), declared in `go.mod`; do not add other
  dependencies without discussion. Tests via `go test` (`net/http/httptest`
  for HTTP mocking).
- Config precedence: CLI flag > env var > `.env`; `.env` is never committed.
- Never log or echo the Onyx API key or git token.

## Git
- Make small, logically contained commits; one per meaningful change, committed as you go.
- Subject lines start with a single emoji that relates to the change, keep subjects under 72 characters. The rest of the message follows normal style (imperative, multi-paragraph).
- The only trailer is a single `Assisted-By: <agent>:<model>` line (e.g. `Assisted-By: goose:qwen3.8-27b`); never `Co-Authored-By`, never a "Generated with" line. This applies to every AI assistant, including Claude Code.
