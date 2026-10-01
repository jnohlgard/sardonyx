// Local directory source (docs/PLAN.md §5.3, task T4).
//
// TODO (T4): filepath.WalkDir with the default noise-dir exclusions,
// --include/--exclude doublestar globs matched against the path relative to
// the source root, --max-depth and --max-file-size filtering, and mtime for
// doc_updated_at; prefer `git ls-files` when the directory is inside a git
// repository; read content as UTF-8 (invalid sequences → U+FFFD) and skip
// binary files (NUL bytes) with a warning.
package source
