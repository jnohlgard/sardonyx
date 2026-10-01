// Package cli wires the sard pipeline together: config resolution, source
// discovery, transform, ingestion, and the final summary (docs/PLAN.md §3).
//
// TODO (T7): Run(args []string) (exitCode int) — stdlib flag parsing per
// §4 (append-value helper for the repeatable --include/--exclude flags),
// log/slog setup, source selection, pipeline orchestration, and the exit
// codes from §8.
//
// TODO (T8): summary output (counts, elapsed time, failure list), --limit,
// and a progress line per file.
package cli
