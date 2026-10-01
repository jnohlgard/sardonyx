# AGENTS.md

`sard` — a CLI (Python package `sardonyx`) that ingests markdown files from a
git repo URL or a local directory into Onyx via the Ingestion API.

## Status
- Plan-first: implement tasks T1–T10 from `docs/PLAN.md` in order; each stub
  module has a TODO naming its task. Stubs are docstring-only — no logic yet.

## Naming
- Package/repo is `sardonyx`; the command is `sard`.
  (Never the old name "onyx-git-ingest".)

## Read on demand
- `docs/PLAN.md` — architecture, CLI spec, document-ID strategy, task list, tests.
- `docs/onyx-ingestion-api.md` — Onyx API auth, payload schema, response semantics.
- `README.md` — user-facing overview.

## Rules
- Python ≥3.10; src layout; dependencies only in `pyproject.toml`; tests via pytest (+respx).
- Config precedence: CLI flag > env var > `.env`; `.env` is never committed.
- Never log or echo the Onyx API key or git token.
- Commit small, as you go, with short conventional messages (docs:/feat:/chore:).
