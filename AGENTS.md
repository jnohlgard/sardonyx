# AGENTS.md

`sard` — a Go CLI (module `sardonyx`) that ingests markdown files from a
git repo URL or a local directory into Onyx via the Ingestion API.

## Status
- T1–T4 are complete (config, models, transform, local directory source).
  Next up is T5 (`internal/source/git.go`), then T6–T10 in `docs/PLAN.md`
  order. The remaining stubs (`source/git.go`, `onyx/client.go`,
  `cli/cli.go`, `cmd/sard/main.go`) are comment-only — no logic yet.

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
