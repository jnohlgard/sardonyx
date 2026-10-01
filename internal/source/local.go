// Local directory source (docs/PLAN.md §5.3, task T4).
//
// Local discovers the markdown files (.md, .mdx, .markdown) under a
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
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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

// LocalOptions configures Local. Every field is optional; zero values mean
// "no filter / no limit":
//
//   - Include: doublestar globs. When non-empty, only files matching at
//     least one pattern are returned.
//   - Exclude: doublestar globs, on top of the default noise-dir
//     exclusions (walk mode).
//   - MaxDepth: max number of path components in the file's path relative
//     to the source root — depth 1 is a file directly under the root;
//     0 = unlimited.
//   - MaxFileSizeKiB: files strictly larger than this size in KiB are
//     skipped with a warning; 0 = unlimited.
//
// Globs are matched case-sensitively against the file path relative to the
// source root with forward slashes: `*` does not cross path separators,
// `**` does (docs/PLAN.md §5.3).
type LocalOptions struct {
	Include        []string
	Exclude        []string
	MaxDepth       int
	MaxFileSizeKiB int
}

// LocalResult is Local's output: the discovered files, sorted by RelPath,
// and the source's default ID base — the source root's cleaned absolute
// path as given, with no symlink resolution. The pipeline uses IDBase as
// the document ID base when no --id-base / SARD_ID_BASE override is set
// (docs/PLAN.md §5.3, §6).
type LocalResult struct {
	IDBase string
	Files  []models.IngestedFile
}

// Local discovers the markdown files (.md, .mdx, .markdown) under dir and
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
// whitespace-only) are skipped with a warning on log.
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

	patterns := make([]string, 0, len(opts.Include)+len(opts.Exclude))
	patterns = append(patterns, opts.Include...)
	patterns = append(patterns, opts.Exclude...)
	for _, pattern := range patterns {
		if !doublestar.ValidatePattern(pattern) {
			return nil, configError("invalid glob %q", pattern)
		}
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

	return &LocalResult{
		IDBase: abs,
		Files:  collectFiles(abs, filepath.Base(abs), candidates, opts, log),
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
// tracked markdown files as slash-separated paths relative to root. Only
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
	for _, line := range strings.Split(string(out), "\n") {
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

// walkMarkdown lists the markdown files under root via filepath.WalkDir,
// returning slash-separated paths relative to root. The default noise-dir
// exclusions and MaxDepth pruning are applied during the walk; the
// include/exclude globs and the per-file filters run in collectFiles.
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

// collectFiles turns candidate relative paths into IngestedFile records.
// Files out of scope for MaxDepth, Include, or Exclude are dropped
// silently — they are deliberate scoping — while oversize, binary, and
// empty skips are warnings. The result is sorted by RelPath.
func collectFiles(root, rootLabel string, candidates []string, opts LocalOptions, log *slog.Logger) []models.IngestedFile {
	var files []models.IngestedFile
	maxBytes := int64(opts.MaxFileSizeKiB) * 1024
	for _, rel := range candidates {
		if opts.MaxDepth > 0 && pathDepth(rel) > opts.MaxDepth {
			continue
		}
		if !passesInclude(rel, opts.Include) {
			continue
		}
		if matchesAny(rel, opts.Exclude) {
			continue
		}

		path := filepath.Join(root, filepath.FromSlash(rel))
		fi, err := os.Stat(path)
		if err != nil {
			log.Warn("skipping unreadable file", "path", rel, "error", err)
			continue
		}
		if maxBytes > 0 && fi.Size() > maxBytes {
			log.Warn("skipping file larger than max file size", "path", rel,
				"sizeKiB", fi.Size()/1024, "maxKiB", opts.MaxFileSizeKiB)
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			log.Warn("skipping unreadable file", "path", rel, "error", err)
			continue
		}
		if bytes.IndexByte(raw, 0) >= 0 {
			log.Warn("skipping binary file (contains NUL bytes)", "path", rel)
			continue
		}
		content := repairUTF8(raw)
		if strings.TrimSpace(content) == "" {
			// "Empty" means zero-length or whitespace-only content.
			log.Warn("skipping empty file (zero-length or whitespace-only)", "path", rel)
			continue
		}
		files = append(files, models.IngestedFile{
			Kind:         models.KindLocal,
			RootLabel:    rootLabel,
			RelPath:      rel,
			Content:      content,
			DocUpdatedAt: fi.ModTime().UTC(),
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].RelPath < files[j].RelPath })
	return files
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
	for _, n := range defaultNoiseDirs {
		if name == n {
			return true
		}
	}
	return false
}
