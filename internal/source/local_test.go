package source

import (
	"bytes"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"sardonyx/internal/config"
	"sardonyx/internal/models"
)

// invalidContent is the committed fixture file broken/invalid.md: a valid
// prefix, two invalid UTF-8 bytes (0xFF 0xFE), then a valid suffix.
const invalidContent = "# Broken\n\xff\xfe tail\n"

// invalidRepaired is the expected Content: each invalid byte becomes the
// U+FFFD replacement character.
const invalidRepaired = "# Broken\n\uFFFD\uFFFD tail\n"

// nulContent contains a NUL byte, so the file is treated as binary.
const nulContent = "bin\x00ary\n"

// discard returns a logger that drops all messages.
func discard() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// writeTree writes files (slash-separated relative paths) under root,
// creating parent directories.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", p, err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", p, err)
		}
	}
}

// walkTree is the temp fixture for the walk-mode tests: the committed
// testdata/local layout plus entries that only make sense outside a
// repository — a .md file inside a noise dir, a non-markdown file, and a
// .idea dir (committed fixtures cannot carry either, see
// testdata/local/README.md).
func walkTree() map[string]string {
	files := map[string]string{
		"README.md":                  "# sard local fixture\n\nRoot-level readme.\n",
		"guide.mdx":                  "# Mdx guide\n\nAn MDX file.\n",
		"notes.markdown":             "# Markdown extension\n\nA .markdown file.\n",
		"docs/intro.md":              "# Intro\n\nDocs level one.\n",
		"docs/api/reference.md":      "# Reference\n\nDocs level two.\n",
		"deep/a/b/leaf.md":           "# Deep leaf\n\nFour levels down.\n",
		"broken/invalid.md":          invalidContent,
		"binary/nul.md":              nulContent,
		"empty/zero.md":              "",
		"empty/blank.md":             "  \n\t\n",
		"include/only.mdx":           "# Include me\n",
		"exclude/never.md":           "# Exclude me\n",
		"notmd.txt":                  "not markdown\n",
		"node_modules/dep/readme.md": "noise readme\n",
		".idea/workspace.xml":        "noise config\n",
	}
	for _, d := range defaultNoiseDirs {
		if d == ".git" || d == ".idea" {
			continue // .idea carries the file above; .git is not created here
		}
		files[d+"/.keep"] = "keep\n"
	}
	return files
}

// walkDefault is the exact set discovered from walkTree with no options:
// every markdown file except the binary (nul.md) and empty (zero.md,
// blank.md) skips, the non-markdown notmd.txt, and the noise-dir contents.
var walkDefault = []string{
	"README.md",
	"broken/invalid.md",
	"deep/a/b/leaf.md",
	"docs/api/reference.md",
	"docs/intro.md",
	"exclude/never.md",
	"guide.mdx",
	"include/only.mdx",
	"notes.markdown",
}

// wantPaths asserts that the records' RelPaths equal want exactly (in
// order) and returns the records for further assertions.
func wantPaths(t *testing.T, files []models.IngestedFile, want ...string) []models.IngestedFile {
	t.Helper()
	got := make([]string, len(files))
	for i, f := range files {
		got[i] = f.RelPath
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("discovered %v, want %v", got, want)
	}
	return files
}

// checkRecords asserts the §5.4 record shape for local files: Kind,
// RootLabel, slash-separated RelPath, empty CommitSHA/BlobURL, and
// DocUpdatedAt equal to the file's own mtime in UTC (never a wall-clock
// or commit value).
func checkRecords(t *testing.T, root string, files []models.IngestedFile) {
	t.Helper()
	wantLabel := filepath.Base(root)
	for _, f := range files {
		if f.Kind != models.KindLocal {
			t.Errorf("%s: Kind = %q, want %q", f.RelPath, f.Kind, models.KindLocal)
		}
		if f.RootLabel != wantLabel {
			t.Errorf("%s: RootLabel = %q, want %q", f.RelPath, f.RootLabel, wantLabel)
		}
		if strings.Contains(f.RelPath, "\\") {
			t.Errorf("%s: RelPath contains a backslash, want forward slashes", f.RelPath)
		}
		if f.CommitSHA != "" {
			t.Errorf("%s: CommitSHA = %q, want empty", f.RelPath, f.CommitSHA)
		}
		if f.BlobURL != "" {
			t.Errorf("%s: BlobURL = %q, want empty", f.RelPath, f.BlobURL)
		}
		fi, err := os.Stat(filepath.Join(root, filepath.FromSlash(f.RelPath)))
		if err != nil {
			t.Fatalf("stat %s: %v", f.RelPath, err)
		}
		if f.DocUpdatedAt.Location() != time.UTC {
			t.Errorf("%s: DocUpdatedAt location = %v, want UTC", f.RelPath, f.DocUpdatedAt.Location())
		}
		if !f.DocUpdatedAt.Equal(fi.ModTime().UTC()) {
			t.Errorf("%s: DocUpdatedAt = %v, want file mtime %v", f.RelPath, f.DocUpdatedAt, fi.ModTime().UTC())
		}
	}
}

// TestLocalWalk covers discovery outside any git repository: the default
// noise-dir exclusions, include/exclude globs, max depth, max file size,
// the binary/empty skips with warnings, U+FFFD repair, and the record
// shape.
func TestLocalWalk(t *testing.T) {
	t.Run("default discovery", func(t *testing.T) {
		root := t.TempDir()
		writeTree(t, root, walkTree())
		var buf bytes.Buffer
		log := slog.New(slog.NewTextHandler(&buf, nil))

		res, err := Local(root, LocalOptions{}, log)
		if err != nil {
			t.Fatalf("Local: %v", err)
		}
		files := wantPaths(t, res.Files, walkDefault...)
		checkRecords(t, root, files)

		byPath := make(map[string]models.IngestedFile, len(files))
		for _, f := range files {
			byPath[f.RelPath] = f
		}
		if got := byPath["broken/invalid.md"].Content; got != invalidRepaired {
			t.Errorf("repaired content = %q, want %q", got, invalidRepaired)
		}
		// The skips are warned: binary and both empty files.
		for _, want := range []string{
			"skipping binary file (contains NUL bytes)",
			"skipping empty file (zero-length or whitespace-only)",
		} {
			if strings.Count(buf.String(), want) == 0 {
				t.Errorf("expected warning %q in log output:\n%s", want, buf.String())
			}
		}
		if got := strings.Count(buf.String(), "skipping empty file"); got != 2 {
			t.Errorf("expected 2 empty-file warnings, got %d:\n%s", got, buf.String())
		}
	})

	t.Run("max depth", func(t *testing.T) {
		root := t.TempDir()
		writeTree(t, root, walkTree())

		res, err := Local(root, LocalOptions{MaxDepth: 1}, discard())
		if err != nil {
			t.Fatalf("Local: %v", err)
		}
		wantPaths(t, res.Files, "README.md", "guide.mdx", "notes.markdown")

		res, err = Local(root, LocalOptions{MaxDepth: 2}, discard())
		if err != nil {
			t.Fatalf("Local: %v", err)
		}
		wantPaths(t, res.Files,
			"README.md", "broken/invalid.md", "docs/intro.md", "exclude/never.md",
			"guide.mdx", "include/only.mdx", "notes.markdown")
		checkRecords(t, root, res.Files)
	})

	t.Run("include filter", func(t *testing.T) {
		root := t.TempDir()
		writeTree(t, root, walkTree())

		res, err := Local(root, LocalOptions{Include: []string{"docs/**"}}, discard())
		if err != nil {
			t.Fatalf("Local: %v", err)
		}
		wantPaths(t, res.Files, "docs/api/reference.md", "docs/intro.md")

		// Multiple patterns: a file matching any of them is included
		// (single-star stays in its own level; ** crosses levels).
		res, err = Local(root, LocalOptions{Include: []string{"*.mdx", "**/only.mdx"}}, discard())
		if err != nil {
			t.Fatalf("Local: %v", err)
		}
		wantPaths(t, res.Files, "guide.mdx", "include/only.mdx")

		// No match: empty result, no error.
		res, err = Local(root, LocalOptions{Include: []string{"nomatch/**"}}, discard())
		if err != nil {
			t.Fatalf("Local: %v", err)
		}
		if len(res.Files) != 0 {
			t.Errorf("discovered %v, want none", res.Files)
		}
	})

	t.Run("exclude filter", func(t *testing.T) {
		root := t.TempDir()
		writeTree(t, root, walkTree())

		res, err := Local(root, LocalOptions{Exclude: []string{"exclude/**", "*.markdown"}}, discard())
		if err != nil {
			t.Fatalf("Local: %v", err)
		}
		wantPaths(t, res.Files,
			"README.md", "broken/invalid.md", "deep/a/b/leaf.md", "docs/api/reference.md",
			"docs/intro.md", "guide.mdx", "include/only.mdx")
		checkRecords(t, root, res.Files)
	})

	t.Run("max file size", func(t *testing.T) {
		root := t.TempDir()
		files := walkTree()
		files["big/large.md"] = strings.Repeat("x", 2048) + "\n" // 2049 bytes
		writeTree(t, root, files)

		var buf bytes.Buffer
		log := slog.New(slog.NewTextHandler(&buf, nil))
		res, err := Local(root, LocalOptions{MaxFileSizeKiB: 1}, log)
		if err != nil {
			t.Fatalf("Local: %v", err)
		}
		wantPaths(t, res.Files, walkDefault...)
		if !strings.Contains(buf.String(), "skipping file larger than max file size") {
			t.Errorf("expected oversize warning in log output:\n%s", buf.String())
		}

		// 4 KiB admits the 2049-byte file.
		res, err = Local(root, LocalOptions{MaxFileSizeKiB: 4}, discard())
		if err != nil {
			t.Fatalf("Local: %v", err)
		}
		wantPaths(t, res.Files,
			"README.md", "big/large.md", "broken/invalid.md", "deep/a/b/leaf.md",
			"docs/api/reference.md", "docs/intro.md", "exclude/never.md",
			"guide.mdx", "include/only.mdx", "notes.markdown")
	})
}

// deepTree is the T9 monorepo fixture: one markdown file at every depth
// from 1 to 12 below the root (d/, d/d/, …).
func deepTree() map[string]string {
	files := map[string]string{}
	for d := 1; d <= 12; d++ {
		files[strings.Repeat("d/", d-1)+"leaf.md"] = "# Depth " + strconv.Itoa(d) + "\n"
	}
	return files
}

// deepSorted returns the deepTree paths in the sorted order collectFiles
// produces: the deeper paths sort first ("d/d/…" < "d/leaf.md" because
// "/" sorts before "l"), the root-level file last.
func deepSorted() []string {
	var want []string
	for d := 12; d >= 2; d-- {
		want = append(want, strings.Repeat("d/", d-1)+"leaf.md")
	}
	return append(want, "leaf.md")
}

// TestLocalWalkDepthBoundary: the depth boundary in walk mode (T9 edge
// case: monorepo depth) — a file at exactly MaxDepth is included, one at
// MaxDepth+1 is excluded, MaxDepth 0 is unlimited. In walk mode a
// directory at depth >= MaxDepth is pruned whole during the walk
// (local.go:244), so its entire subtree never reaches collectFiles.
func TestLocalWalkDepthBoundary(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, deepTree())

	res, err := Local(root, LocalOptions{}, discard())
	if err != nil {
		t.Fatalf("Local: %v", err)
	}
	wantPaths(t, res.Files, deepSorted()...)

	res, err = Local(root, LocalOptions{MaxDepth: 3}, discard())
	if err != nil {
		t.Fatalf("Local(MaxDepth): %v", err)
	}
	// The depth-3 file is included; the depth-4+ files sit inside the
	// pruned depth-3 directory and are excluded with it.
	wantPaths(t, res.Files, "d/d/leaf.md", "d/leaf.md", "leaf.md")
}

// TestLocalGitModeDepth: in git mode (the input is a repository, so
// candidates come from git ls-files) there is no walk-time directory
// pruning — the depth filter is collectFiles' per-file check
// (local.go:269): a file at exactly MaxDepth is included, one at
// MaxDepth+1 is excluded (T9 edge case: monorepo depth). Skipped when
// git is not on PATH.
func TestLocalGitModeDepth(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	root := t.TempDir()
	runGit(t, root, "init", "-q", "-b", "main", ".")
	writeTree(t, root, deepTree())
	runGit(t, root, "add", "-A")
	runGit(t, root, "-c", "user.name=test", "-c", "user.email=test@example.com",
		"commit", "-q", "-m", "deep tree")

	res, err := Local(root, LocalOptions{MaxDepth: 4}, discard())
	if err != nil {
		t.Fatalf("Local: %v", err)
	}
	wantPaths(t, res.Files, "d/d/d/leaf.md", "d/d/leaf.md", "d/leaf.md", "leaf.md")
}

// TestLocalCRLF: CRLF line endings round-trip through discovery — the
// \r bytes are valid UTF-8, so repairUTF8 must leave them alone, and the
// record's Content is the file's exact bytes (T9 edge case: CRLF).
func TestLocalCRLF(t *testing.T) {
	root := t.TempDir()
	const crlf = "# Crlf\r\nline one\r\nline two\r\n"
	writeTree(t, root, map[string]string{"crlf/win.md": crlf})

	res, err := Local(root, LocalOptions{}, discard())
	if err != nil {
		t.Fatalf("Local: %v", err)
	}
	files := wantPaths(t, res.Files, "crlf/win.md")
	if got := files[0].Content; got != crlf {
		t.Errorf("Content = %q, want the CRLF bytes preserved exactly %q", got, crlf)
	}
}

// TestMaxFileSizeBoundary: the oversize check is "strictly larger" — a
// file exactly at the limit is kept, one byte over is skipped with a
// warning and counted in Skipped (T9 edge case: the boundary; the
// existing max-file-size subtest of TestLocalWalk covers the clearly
// over case).
func TestMaxFileSizeBoundary(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"at/limit.md":   strings.Repeat("a", 1024), // exactly 1 KiB
		"over/limit.md": strings.Repeat("b", 1025), // one byte over
	})

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	res, err := Local(root, LocalOptions{MaxFileSizeKiB: 1}, log)
	if err != nil {
		t.Fatalf("Local: %v", err)
	}
	files := wantPaths(t, res.Files, "at/limit.md")
	if got := files[0].Content; got != strings.Repeat("a", 1024) {
		t.Errorf("at-limit Content = %d bytes, want 1024", len(got))
	}
	if res.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1 (the one-byte-over file)", res.Skipped)
	}
	if !strings.Contains(buf.String(), "skipping file larger than max file size") {
		t.Errorf("expected the oversize warning in log output:\n%s", buf.String())
	}
}

// TestLargeFileInLimit: a large (512 KiB) single-line file within
// MaxFileSizeKiB is ingested with its content intact and nothing
// skipped (T9 edge case: very long single file).
func TestLargeFileInLimit(t *testing.T) {
	root := t.TempDir()
	const big = 512 * 1024
	writeTree(t, root, map[string]string{"big/single.md": strings.Repeat("x", big)})

	res, err := Local(root, LocalOptions{MaxFileSizeKiB: 1024}, discard()) // the CLI default
	if err != nil {
		t.Fatalf("Local: %v", err)
	}
	files := wantPaths(t, res.Files, "big/single.md")
	if got := files[0].Content; got != strings.Repeat("x", big) {
		t.Errorf("Content = %d bytes, want the full %d-byte single line", len(got), big)
	}
	if res.Skipped != 0 {
		t.Errorf("Skipped = %d, want 0", res.Skipped)
	}
}

// TestRepairUTF8: the shared UTF-8 repair (docs/PLAN.md §5.3, used by
// both sources) — one U+FFFD per invalid byte including multi-byte
// garbage runs, a genuine U+FFFD (a valid 3-byte sequence) passes
// through as a single rune, and valid multi-byte characters are
// untouched (T9 edge case: non-UTF-8).
func TestRepairUTF8(t *testing.T) {
	// A genuine U+FFFD in the source text: one rune, encoded EF BF BD.
	const fffd = "\uFFFD"
	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"valid ASCII unchanged", []byte("plain text\n"), "plain text\n"},
		{"valid multi-byte unchanged", []byte("café naïve — em dash\n"), "café naïve — em dash\n"},
		{"single invalid byte", []byte("a\xffb"), "a" + fffd + "b"},
		{"multi-byte garbage run, one replacement per byte", []byte{'h', 0x80, 0x81, 0x82, 'i'},
			"h" + fffd + fffd + fffd + "i"},
		{"truncated multi-byte sequence at end", []byte("end\xc3"), "end" + fffd},
		{"genuine U+FFFD passes through unchanged", []byte("x\uFFFD y"), "x\uFFFD y"},
		{"mixed valid UTF-8 and invalid bytes", []byte("hé\x80llö\xc3"), "hé" + fffd + "llö" + fffd},
		{"empty", []byte{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := repairUTF8(tc.in)
			if got != tc.want {
				t.Errorf("repairUTF8(%q) = %q (runes: %v), want %q (runes: %v)",
					string(tc.in), got, []rune(got), tc.want, []rune(tc.want))
			}
		})
	}
	// The genuine-U+FFFD case in runes: it must stay one rune, not be
	// re-replaced byte by byte into three.
	if got := repairUTF8([]byte("\uFFFD")); len([]rune(got)) != 1 || got != fffd {
		t.Errorf("repairUTF8(genuine U+FFFD) = %q, want exactly %q", got, fffd)
	}
}

// runGit runs git with dir as CWD, failing the test on non-zero exit.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// TestLocalGitMode covers discovery inside a real git repository: git
// ls-files lists tracked files only (ignored and untracked files absent),
// default noise-dir exclusions do not apply in git mode, a subdirectory of
// the repo is discovered relative to itself, and DocUpdatedAt stays the
// file mtime (the input type decides the kind — see the note in
// local.go). Skipped when git is not on PATH (docs/PLAN.md §12).
func TestLocalGitMode(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	root := t.TempDir()
	runGit(t, root, "init", "-q", "-b", "main", ".")
	fixedTime := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	writeTree(t, root, map[string]string{
		"tracked.md":    "# Tracked\n",
		"ignored.md":    "# Ignored\n",
		"untracked.md":  "# Untracked\n",
		"sub/nested.md": "# Nested\n",
		"vendor/lib.md": "# Tracked inside a noise dir\n",
		".gitignore":    "ignored.md\n",
	})
	// Backdate the mtimes: if the implementation (wrongly) used the commit
	// time for DocUpdatedAt, this test cannot pass.
	for _, rel := range []string{"tracked.md", "sub/nested.md", "vendor/lib.md"} {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.Chtimes(p, fixedTime, fixedTime); err != nil {
			t.Fatalf("Chtimes(%s): %v", rel, err)
		}
	}
	// Explicit paths: untracked.md stays genuinely untracked (not ignored,
	// not added), while ignored.md is excluded by .gitignore.
	runGit(t, root, "add", "tracked.md", "sub/nested.md", "vendor/lib.md", ".gitignore")
	runGit(t, root, "-c", "user.name=test", "-c", "user.email=test@example.com",
		"commit", "-q", "-m", "init")

	res, err := Local(root, LocalOptions{}, discard())
	if err != nil {
		t.Fatalf("Local: %v", err)
	}
	// git ls-files lists tracked files only; vendor/lib.md is present
	// because the default noise-dir exclusions are walk-mode only.
	files := wantPaths(t, res.Files, "sub/nested.md", "tracked.md", "vendor/lib.md")
	checkRecords(t, root, files)
	for _, f := range files {
		if !f.DocUpdatedAt.Equal(fixedTime) {
			t.Errorf("%s: DocUpdatedAt = %v, want the (backdated) file mtime %v",
				f.RelPath, f.DocUpdatedAt, fixedTime)
		}
	}
	if want, _ := filepath.Abs(root); res.IDBase != want {
		t.Errorf("IDBase = %q, want %q", res.IDBase, want)
	}

	// A subdirectory of the repo: the ancestor repo is found by git itself
	// and discovery is relative to the subdirectory.
	sub := filepath.Join(root, "sub")
	res2, err := Local(sub, LocalOptions{}, discard())
	if err != nil {
		t.Fatalf("Local(sub): %v", err)
	}
	files2 := wantPaths(t, res2.Files, "nested.md")
	checkRecords(t, sub, files2)
	if want, _ := filepath.Abs(sub); res2.IDBase != want {
		t.Errorf("IDBase = %q, want %q", res2.IDBase, want)
	}
}

// TestLocalFakeGitFallback: a stray `.git` entry that is not a real
// repository must not break discovery — git fails, Local warns, falls back
// to the walk, and the default noise-dir exclusions still apply.
func TestLocalFakeGitFallback(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		".git/keep":                "keep\n",
		"readme.md":                "# Readme\n",
		"node_modules/dep/junk.md": "noise\n",
	})

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	res, err := Local(root, LocalOptions{}, log)
	if err != nil {
		t.Fatalf("Local: %v", err)
	}
	checkRecords(t, root, wantPaths(t, res.Files, "readme.md"))
	if !strings.Contains(buf.String(), "falling back to a filesystem walk") {
		t.Errorf("expected the walk-fallback warning in log output:\n%s", buf.String())
	}
}

// TestLocalValidation: a missing path, a file (not a directory), and
// malformed globs all produce errors wrapping config.ErrConfiguration,
// which the CLI (T7) maps to exit code 2 via config.IsConfigurationError.
func TestLocalValidation(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"a.md": "# A\n"})

	for name, dir := range map[string]string{
		"missing path":          filepath.Join(root, "does-not-exist"),
		"file, not a directory": filepath.Join(root, "a.md"),
		"missing path, deep":    filepath.Join(root, "no", "deeper", "path"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Local(dir, LocalOptions{}, discard())
			if err == nil {
				t.Fatalf("Local(%s) succeeded, want an error", dir)
			}
			if !config.IsConfigurationError(err) {
				t.Errorf("err = %v, want it to wrap config.ErrConfiguration", err)
			}
		})
	}

	for name, opts := range map[string]LocalOptions{
		"invalid include glob": {Include: []string{"[.md"}},
		"invalid exclude glob": {Exclude: []string{"a{b"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Local(root, opts, discard())
			if err == nil {
				t.Fatalf("Local with %v succeeded, want an error", opts)
			}
			if !config.IsConfigurationError(err) {
				t.Errorf("err = %v, want it to wrap config.ErrConfiguration", err)
			}
		})
	}
}

// TestLocalIDBase: the default ID base is the source root's cleaned
// absolute path as given — redundant path components are cleaned and
// symlinks are not resolved.
func TestLocalIDBase(t *testing.T) {
	fixture := filepath.Join("testdata", "local")
	for _, input := range []string{
		fixture,
		"./" + fixture,
		filepath.Join(fixture, "."),
	} {
		res, err := Local(input, LocalOptions{}, discard())
		if err != nil {
			t.Fatalf("Local(%s): %v", input, err)
		}
		want, _ := filepath.Abs(fixture) // already cleaned
		if res.IDBase != want {
			t.Errorf("IDBase for %q = %q, want %q", input, res.IDBase, want)
		}
	}

	// A symlinked input keeps the symlink path in the ID base.
	real := t.TempDir()
	writeTree(t, real, map[string]string{"a.md": "# A\n"})
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	res, err := Local(link, LocalOptions{}, discard())
	if err != nil {
		t.Fatalf("Local(%s): %v", link, err)
	}
	if res.IDBase != link {
		t.Errorf("IDBase = %q, want the symlink path %q (no resolution)", res.IDBase, link)
	}
	// The walk follows the symlinked root, so the target's files are found.
	wantPaths(t, res.Files, "a.md")
}

// TestLocalFixture: the committed fixture tree under testdata/local. It is
// deliberately mode-independent: it holds no markdown file inside a noise
// dir and no untracked or gitignored markdown, so `git ls-files` (when
// this tree sits inside the sardonyx repo) and the filesystem walk
// discover the same set. See testdata/local/README.md.
func TestLocalFixture(t *testing.T) {
	const fixture = "testdata/local"
	res, err := Local(fixture, LocalOptions{}, discard())
	if err != nil {
		t.Fatalf("Local: %v", err)
	}

	want := []string{
		"README.md",
		"broken/invalid.md",
		"deep/a/b/leaf.md",
		"docs/api/reference.md",
		"docs/intro.md",
		"exclude/never.md",
		"guide.mdx",
		"include/only.mdx",
		"notes.markdown",
	}
	// binary/nul.md (NUL byte), empty/zero.md, and empty/blank.md (empty)
	// are skipped; the noise dirs contain no markdown.
	root, _ := filepath.Abs(fixture)
	files := wantPaths(t, res.Files, want...)
	checkRecords(t, root, files)
	if res.IDBase != root {
		t.Errorf("IDBase = %q, want %q", res.IDBase, root)
	}

	byPath := make(map[string]models.IngestedFile, len(files))
	for _, f := range files {
		byPath[f.RelPath] = f
	}
	if got := byPath["broken/invalid.md"].Content; got != invalidRepaired {
		t.Errorf("repaired content = %q, want %q", got, invalidRepaired)
	}

	// The filters work against the fixture in both discovery modes.
	res2, err := Local(fixture, LocalOptions{MaxDepth: 1}, discard())
	if err != nil {
		t.Fatalf("Local(MaxDepth): %v", err)
	}
	wantPaths(t, res2.Files, "README.md", "guide.mdx", "notes.markdown")

	res3, err := Local(fixture, LocalOptions{Include: []string{"include/**"}}, discard())
	if err != nil {
		t.Fatalf("Local(Include): %v", err)
	}
	wantPaths(t, res3.Files, "include/only.mdx")

	res4, err := Local(fixture, LocalOptions{Exclude: []string{"exclude/**"}}, discard())
	if err != nil {
		t.Fatalf("Local(Exclude): %v", err)
	}
	wantPaths(t, res4.Files,
		"README.md", "broken/invalid.md", "deep/a/b/leaf.md", "docs/api/reference.md",
		"docs/intro.md", "guide.mdx", "include/only.mdx", "notes.markdown")
}
