// Package source discovers the files to ingest from one input: a git
// repository (git.go) or a local directory (local.go). Both produce
// []models.IngestedFile records (docs/PLAN.md §5).
//
// The local source (task T4) is exported as:
//
//	Local(dir string, opts LocalOptions, log *slog.Logger) (*LocalResult, error)
//
// dir is the source directory; opts carries the include/exclude doublestar
// globs, max depth, and max file size; log receives warnings for skipped
// files. LocalResult holds the discovered files (sorted by RelPath) and
// the source's default ID base (the source root's cleaned absolute path as
// given, no symlink resolution), which the pipeline uses when no
// --id-base / SARD_ID_BASE override is set.
package source
