"""CLI: argparse + pipeline orchestration.

Implements docs/PLAN.md §4 (CLI spec) and §10 task T7:

    sard ingest <url-or-path> [--cc-pair-id …] [--dry-run] …

Pipeline: resolve config → pick source (git repo | local dir) → discover markdown
files → transform each to an Onyx payload → send via OnyxClient → print summary
(created/updated/skipped/failed) → set exit code (§8).
"""

# TODO (T7): implement `main(argv=None) -> int`.
