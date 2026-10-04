// Copyright (C) 2026 Joakim Nohlgård
// SPDX-License-Identifier: AGPL-3.0

// Local directory source (docs/PLAN.md §5.3, task T4).
//
// Local discovers the Markdown files (.md, .mdx, .markdown) under a
// directory and returns them as models.IngestedFile records. When the
// directory (or an ancestor) is a git repository, discovery prefers
// `git ls-files` — tracked files only, the repository's own .gitignore
// respected — and falls back to a warned filepath.WalkDir when git is
// unavailable or fails.
//
// A directory input always yields Kind=local records, even when it is
// inside a git repository: the input type decides the kind
// (docs/PLAN.md §5.4). The `git ls-files` preference changes only *which*
// files are discovered; the provenance stays local — DocUpdatedAt is the
// file's mtime (UTC) and CommitSHA/BlobURL are empty — which is what §6's
// local metadata of {path, ingested_by} expects.
package source

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/bmatcuk/doublestar/v4"

	"sardonyx/internal/config"
	"sardonyx/internal/models"
)

// Markdown extensions the local source discovers (docs/PLAN.md §5.3).
var markdownExtensions = []string{".md", ".mdx", ".markdown"}

// Default directory names excluded from the filesystem walk
// (docs/PLAN.md §5.3). The .github workflows dir is deliberately excluded;
// an --include glob can bring specific files back. These exclusions apply
// in walk mode only: in git mode, git ls-files lists tracked files and the
// repository's own .gitignore provides the scoping.
var defaultNoiseDirs = []string{
	".git", "node_modules", "dist", "build", "vendor",
	".venv", "__pycache__", ".idea", ".github",
}

// LocalOptions is the local source's discovery filter. It is the shared
// Filter (collect.go): the local source uses exactly the shared
// include/exclude/depth/size semantics (docs/PLAN.md §5.3), so it reuses
// the type directly rather than duplicating the fields.
type LocalOptions = Filter

// LocalResult is Local's output: the discovered files, sorted by RelPath,
// the source's default ID base — the source root's cleaned absolute
// path as given, with no symlink resolution — and Skipped, the number of
// files dropped by a per-file check (oversize, binary, empty, unreadable;
// each such drop is a warning) and reported in the run summary
// (docs/PLAN.md §8). Files outside the scope of the depth, include, or
// exclude filters are not skipped — that is deliberate scoping. The
// pipeline uses IDBase as the document ID base when no --id-base /
// SARD_ID_BASE override is set (docs/PLAN.md §5.3, §6).
type LocalResult struct {
	IDBase  string
	Files   []models.IngestedFile
	Skipped int
}

// Local discovers the Markdown files (.md, .mdx, .markdown) under dir and
// returns them as models.IngestedFile records (docs/PLAN.md §5.3, §5.4).
//
// dir must exist and be a directory; otherwise Local returns an error
// wrapping config.ErrConfiguration, as do malformed include/exclude globs
// (the CLI maps both to exit code 2, docs/PLAN.md §8).
//
// Record shape: Kind=local, RootLabel=basename(abs(dir)),
// RelPath=the file path relative to the source root with forward slashes,
// Content=UTF-8 text with invalid byte sequences replaced by U+FFFD,
// DocUpdatedAt=file mtime (UTC), CommitSHA and BlobURL empty. Files that
// are oversize, binary (NUL bytes), or empty (zero-length or
// whitespace-only) are skipped with a warning on log and counted in
// LocalResult.Skipped.
//
// See the package comment for the git-repository discovery behavior. A nil
// log uses slog.Default().
func Local(dir string, opts LocalOptions, log *slog.Logger) (*LocalResult, error) {
	if log == nil {
		log = slog.Default()
	}

	// "As given": filepath.Abs cleans the path but does not resolve
	// symlinks (docs/PLAN.md §5.3).
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, configError("invalid source path %q: %v", dir, err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, configError("invalid source path %q: %v", dir, err)
	}
	if !fi.IsDir() {
		return nil, configError("invalid source path %q: not a directory", dir)
	}

	if err := opts.validate(); err != nil {
		return nil, err
	}

	var candidates []string
	if findGitRoot(abs) != "" {
		files, err := gitListMarkdown(abs)
		if err != nil {
			log.Warn("git ls-files failed; falling back to a filesystem walk", "error", err)
			candidates = walkMarkdown(abs, opts, log)
		} else {
			candidates = files
		}
	} else {
		candidates = walkMarkdown(abs, opts, log)
	}

	files, skipped := collectFiles(abs, filepath.Base(abs), candidates, opts, log)
	return &LocalResult{
		IDBase:  abs,
		Files:   files,
		Skipped: skipped,
	}, nil
}

// configError wraps a message with config.ErrConfiguration so the error
// class can be matched with config.IsConfigurationError and mapped to exit
// code 2 by the CLI (docs/PLAN.md §8).
func configError(format string, args ...any) error {
	return errors.Join(fmt.Errorf(format, args...), config.ErrConfiguration)
}

// findGitRoot returns the first ancestor of dir (including dir itself) that
// contains a `.git` entry — a directory, or a file as in worktrees and
// submodules — or "" if none does.
func findGitRoot(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		if parent := filepath.Dir(d); parent == d {
			return ""
		}
	}
}

// gitListMarkdown runs `git ls-files` with CWD = root and returns the
// tracked Markdown files as slash-separated paths relative to root. Only
// tracked files are listed, so the repository's .gitignore is respected
// for free (docs/PLAN.md §5.2, §5.3). Default noise-dir exclusions are not
// applied here — that is a walk-mode concern.
func gitListMarkdown(root string) ([]string, error) {
	// -c core.quotePath=off keeps non-ASCII names as UTF-8 in the output.
	cmd := exec.Command("git", "-c", "core.quotePath=off", "-C", root,
		"ls-files", "--", "*.md", "*.mdx", "*.markdown")
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("git ls-files: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}
	var files []string
	for line := range strings.SplitSeq(string(out), "\n") {
		if line == "" {
			continue
		}
		rel := filepath.ToSlash(line)
		if isMarkdown(filepath.Base(rel)) {
			files = append(files, rel)
		}
	}
	return files, nil
}

// walkMarkdown lists the Markdown files under root via filepath.WalkDir,
// returning slash-separated paths relative to root. The default noise-dir
// exclusions and MaxDepth pruning are applied during the walk; the
// include/exclude globs and the per-file filters run in the shared pipeline
// (collect, via collectFiles).
func walkMarkdown(root string, opts LocalOptions, log *slog.Logger) []string {
	// filepath.WalkDir Lstats its root, which would treat a symlinked root
	// as a plain file. Walk the target instead; relative paths are
	// unaffected because the symlink is the root.
	walkRoot := root
	if fi, err := os.Lstat(root); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if target, err := os.Readlink(root); err == nil {
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(root), target)
			}
			walkRoot = target
		}
	}

	var files []string
	_ = filepath.WalkDir(walkRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d == nil {
				log.Warn("skipping unreadable file", "path", path, "error", err)
				return nil
			}
			log.Warn("skipping unreadable directory", "path", path, "error", err)
			return fs.SkipDir
		}
		rel, err := filepath.Rel(walkRoot, path)
		if err != nil {
			log.Warn("cannot relativize path", "path", path, "error", err)
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." {
				if isNoiseDir(d.Name()) {
					return fs.SkipDir
				}
				if opts.MaxDepth > 0 && pathDepth(rel) >= opts.MaxDepth {
					return fs.SkipDir // every deeper file exceeds the limit
				}
			}
			return nil
		}
		if isMarkdown(d.Name()) {
			files = append(files, rel)
		}
		return nil
	})
	return files
}

// collectFiles turns candidate relative paths into local IngestedFile
// records via the shared pipeline (collect). The build closure supplies the
// local provenance: DocUpdatedAt is the file's mtime (UTC), with CommitSHA
// and BlobURL left empty — the input type (a directory) decides the kind,
// even when the directory sits inside a git repository (docs/PLAN.md §5.4).
// See collect for the check order and skip semantics.
func collectFiles(root, rootLabel string, candidates []string, f Filter, log *slog.Logger) ([]models.IngestedFile, int) {
	return collect(root, candidates, f, log, func(rel string, fi os.FileInfo, content string) (models.IngestedFile, error) {
		return models.IngestedFile{
			Kind:         models.KindLocal,
			RootLabel:    rootLabel,
			RelPath:      rel,
			Content:      content,
			DocUpdatedAt: fi.ModTime().UTC(),
		}, nil
	})
}

// passesInclude reports whether rel passes the include filter: it passes
// when there is no filter, or it matches at least one pattern. Patterns
// are validated up front in Local, so a Match error is impossible here.
func passesInclude(rel string, include []string) bool {
	if len(include) == 0 {
		return true
	}
	return matchesAny(rel, include)
}

// matchesAny reports whether rel matches any of the (validated) patterns.
func matchesAny(rel string, patterns []string) bool {
	for _, pattern := range patterns {
		if ok, _ := doublestar.Match(pattern, rel); ok {
			return true
		}
	}
	return false
}

// repairUTF8 decodes b as UTF-8, replacing every invalid byte sequence
// with U+FFFD, the Unicode replacement character (docs/PLAN.md §5.3).
// DecodeRune signals an invalid byte as (RuneError, 1); a genuine U+FFFD
// in the text is a valid 3-byte sequence, so it passes through unchanged.
func repairUTF8(b []byte) string {
	var sb strings.Builder
	sb.Grow(len(b))
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size == 1 {
			sb.WriteString("\uFFFD")
			i++
			continue
		}
		sb.WriteRune(r)
		i += size
	}
	return sb.String()
}

// pathDepth is the number of path components in a slash-separated path
// relative to the source root: a file directly under the root has depth 1.
func pathDepth(rel string) int {
	return strings.Count(rel, "/") + 1
}

// isMarkdown reports whether name has one of the discovered extensions
// (.md, .mdx, .markdown); matching is case-sensitive.
func isMarkdown(name string) bool {
	for _, ext := range markdownExtensions {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}

// isNoiseDir reports whether name is one of the default excluded directory
// names (walk mode only).
func isNoiseDir(name string) bool {
	return slices.Contains(defaultNoiseDirs, name)
}
