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
// files. LocalResult holds the discovered files (sorted by RelPath), the
// number of files dropped by a per-file check (Skipped, for the run
// summary), and the source's default ID base (the source root's cleaned
// absolute path as given, no symlink resolution), which the pipeline uses
// when no --id-base / SARD_ID_BASE override is set.
//
// The git source (task T5) is exported as:
//
//	Git(url string, opts GitOptions, log *slog.Logger) (*GitResult, error)
//
// url is a git repository URL — an https URL, a git@ ssh form, or the
// owner/repo shorthand; also file:///path or an existing local path
// for local clones — cloned shallowly into a temporary directory.
// GitResult holds the discovered files (sorted by RelPath) with git
// provenance (the resolved HEAD commit's SHA and committer time — see
// git.go for the shallow-clone attribution semantics — and GitHub blob
// URLs for github.com origins), the run's default ID base — the
// normalized origin URL, lowercased host, no trailing .git, never
// credentials —, the branch the clone resolved to, and the number of
// files dropped by a per-file check (Skipped, for the run summary).
//
// GitOptions mirrors LocalOptions: the same include/exclude globs
// (matched case-sensitively against the slash-separated path relative
// to the repository root), max depth, and max file size apply to the
// git source as well, so the pipeline (task T7) treats both sources
// uniformly; Branch and Token are the git-specific additions.
package source
