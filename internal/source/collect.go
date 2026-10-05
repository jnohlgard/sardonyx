// Copyright (C) 2026 Joakim Nohlgård
// SPDX-License-Identifier: AGPL-3.0

// The shared discovery/skip pipeline (docs/PLAN.md §5).
//
// Both sources — a local directory (local.go) and a git repository
// (git.go) — discover Markdown candidates and then run the exact same
// per-file pipeline over them. Filter holds the four shared filter knobs
// (include/exclude globs, max depth, max file size) and collect() is the
// single implementation of the pipeline. Each source supplies a build
// closure that turns a surviving file into its models.IngestedFile record
// (local: mtime provenance; git: HEAD-commit provenance, with the
// per-file lastCommit as fallback).
//
// Keeping the pipeline in one place means a future change to the skip
// semantics applies to both sources at once instead of silently
// diverging across two copies.
package source

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"sardonyx/internal/models"
)

// Filter holds the discovery filters shared by both sources: the
// include/exclude doublestar globs, the max depth, and the max file size.
// Every field is optional; zero values mean "no filter / no limit":
//
//   - Include: doublestar globs. When non-empty, only files matching at
//     least one pattern are returned.
//   - Exclude: doublestar globs, on top of the default noise-dir
//     exclusions (walk mode only).
//   - MaxDepth: max number of path components in the file's path relative
//     to the source root — depth 1 is a file directly under the root;
//     0 = unlimited.
//   - MaxFileSizeKiB: files strictly larger than this size in KiB are
//     skipped with a warning; 0 = unlimited.
//
// Globs are matched case-sensitively against the file path relative to the
// source root with forward slashes: `*` does not cross path separators,
// `**` does (docs/PLAN.md §5.3).
type Filter struct {
	Include        []string
	Exclude        []string
	MaxDepth       int
	MaxFileSizeKiB int
}

// validate checks that every include and exclude glob is a well-formed
// doublestar pattern, returning a configuration error (config.ErrConfiguration,
// exit 2) that names the first invalid pattern. Include patterns are
// checked before exclude patterns, matching the order the CLI builds the
// flag-derived filter.
func (f Filter) validate() error {
	for _, pattern := range f.Include {
		if !doublestar.ValidatePattern(pattern) {
			return configError("invalid glob %q", pattern)
		}
	}
	for _, pattern := range f.Exclude {
		if !doublestar.ValidatePattern(pattern) {
			return configError("invalid glob %q", pattern)
		}
	}
	return nil
}

// collect applies the shared filter and per-file checks to the candidates
// under root and builds one record per surviving file via build. It is the
// single implementation of the pipeline both sources share.
//
// Check order, per candidate, is identical to the pre-refactor per-source
// loops: depth, include, exclude, stat, size, read, NUL, empty, then build.
// Files out of scope for depth/include/exclude are dropped silently — that
// is deliberate scoping and is not counted in the returned skip total.
// Every other drop (an unreadable file, an oversize file, a binary file, an
// empty file) is a warning on log and increments the skip count. For each
// file that survives all the checks, build produces the record: it receives
// the file's os.FileInfo and its content already repaired to valid UTF-8,
// so a source that needs no extra I/O (local: the stat's mtime) adds none,
// while a source that does (git: headCommit / lastCommit) can call into
// its own state.
// A non-nil return from build counts as one more skip; build is responsible
// for emitting its own warning (the git source redacts secrets from it).
//
// The returned records are sorted by RelPath (ascending).
func collect(root string, candidates []string, f Filter, log *slog.Logger,
	build func(rel string, fi os.FileInfo, content string) (models.IngestedFile, error)) ([]models.IngestedFile, int) {
	var files []models.IngestedFile
	var skipped int
	maxBytes := int64(f.MaxFileSizeKiB) * 1024
	for _, rel := range candidates {
		if f.MaxDepth > 0 && pathDepth(rel) > f.MaxDepth {
			continue
		}
		if !passesInclude(rel, f.Include) {
			continue
		}
		if matchesAny(rel, f.Exclude) {
			continue
		}

		path := filepath.Join(root, filepath.FromSlash(rel))
		fi, err := os.Stat(path)
		if err != nil {
			skipped++
			log.Warn("skipping unreadable file", "path", rel, "error", err)
			continue
		}
		if maxBytes > 0 && fi.Size() > maxBytes {
			skipped++
			log.Warn("skipping file larger than max file size", "path", rel,
				"sizeKiB", fi.Size()/1024, "maxKiB", f.MaxFileSizeKiB)
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			skipped++
			log.Warn("skipping unreadable file", "path", rel, "error", err)
			continue
		}
		if bytes.IndexByte(raw, 0) >= 0 {
			skipped++
			log.Warn("skipping binary file (contains NUL bytes)", "path", rel)
			continue
		}
		content := repairUTF8(raw)
		if strings.TrimSpace(content) == "" {
			// "Empty" means zero-length or whitespace-only content.
			skipped++
			log.Warn("skipping empty file (zero-length or whitespace-only)", "path", rel)
			continue
		}

		rec, err := build(rel, fi, content)
		if err != nil {
			// build already warned (e.g. git's provenance failure, redacted);
			// just count the drop.
			skipped++
			continue
		}
		files = append(files, rec)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].RelPath < files[j].RelPath })
	return files, skipped
}
