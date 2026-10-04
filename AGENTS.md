# AGENTS.md

`sard` — a Go CLI (module `sardonyx`) that ingests Markdown files from a
git repo URL or a local directory into Onyx via the Ingestion API.

## Status
- T1–T12 are complete: config, models, transform, local directory
  source, git repository source, Onyx client, CLI wiring, summary &
  polish (progress lines, summary with elapsed time and failure list,
  the source layer's skipped count), test hardening (CRLF, non-UTF-8,
  oversize, monorepo depth, .mdx frontmatter, and the two
  URL-normalization coverage gaps), build & README (the static binary
  recipe, the quickstart, and `--dry-run` running without Onyx
  credentials — a dry run sends nothing, so it needs none), the
  `sard check` pre-flight (three read-only probes, §13), and the
  `sard ls` command (list the documents the API key can see via
  `GET /onyx-api/ingestion`, read-only, §14). T13–T14 are planned and
  not yet implemented: T13 adds the optional `--tag` metadata flag;
  T14 adds the `mime_type` metadata field. No stub packages remain;
  the task list is in `docs/PLAN.md` §10.

## Naming
- Repo/Go module is `sardonyx`; the binary and command are `sard`.
  (Never the old name "onyx-git-ingest".)

## Read on demand
- `docs/PLAN.md` — architecture, CLI spec, document-ID strategy, task list, tests.
- `docs/onyx-ingestion-api.md` — Onyx API auth, payload schema, response semantics.
- `README.md` — user-facing overview.

## Rules
- Go; stdlib-first — the only external dependencies are `godotenv` (`.env`
  loading), `doublestar/v4` (globs), and `spf13/cobra` (CLI argument
  parsing + auto-generated help; used only in `internal/cli`), declared in
  `go.mod`; do not add other dependencies without discussion. Tests via
  `go test` (`net/http/httptest` for HTTP mocking).
- Config precedence: CLI flag > env var > `.env`; `.env` is never committed.
- Licensed AGPL-3.0 (full text in `LICENSE`). Every `.go` file starts with the
  two-line header `// Copyright (C) 2026 Joakim Nohlgård` +
  `// SPDX-License-Identifier: AGPL-3.0` — copy it into any new `.go` file.
- Never log or echo the Onyx API key or git token.

## Git
- Make small, logically contained commits; one per meaningful change, committed as you go.
- Subject lines start with a single emoji that relates to the change, keep subjects under 72 characters. The rest of the message follows normal style (imperative, multi-paragraph).
- The only trailer is a single `Assisted-By: <agent>:<model>` line (e.g. `Assisted-By: goose:qwen3.8-27b`); never `Co-Authored-By`, never a "Generated with" line. This applies to every AI assistant, including Claude Code.
