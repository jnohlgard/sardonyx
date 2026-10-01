package transform

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"sardonyx/internal/models"
)

// wantID mirrors the §6 ID derivation (sha256 over kind + base + relpath) so
// tests can pin the exact digest, including the kind prefix.
func wantID(kind, base, relPath string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + base + "\x00" + relPath))
	return hex.EncodeToString(sum[:])
}

func gitFile() models.IngestedFile {
	return models.IngestedFile{
		Kind:         models.KindGit,
		RootLabel:    "acme/widgets",
		RelPath:      "docs/guide.md",
		Content:      "  # The Guide\n\nBody of the guide.",
		DocUpdatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		CommitSHA:    "abc123def456",
		BlobURL:      "https://github.com/acme/widgets/blob/main/docs/guide.md",
	}
}

func localFile() models.IngestedFile {
	return models.IngestedFile{
		Kind:         models.KindLocal,
		RootLabel:    "mydocs",
		RelPath:      "notes/plan.md",
		Content:      "## Not a title\nJust notes.",
		DocUpdatedAt: time.Date(2025, 6, 7, 8, 9, 10, 0, time.UTC),
	}
}

// TestIDIsDeterministic: identical (kind, idBase, relpath) → identical ID,
// even when volatile fields differ — proving branch, commit, and mtime stay
// out of the ID.
func TestIDIsDeterministic(t *testing.T) {
	base := "https://github.com/acme/widgets"
	a := ToOnyxPayload(gitFile(), models.DocumentSourceGitHub, 42, base)

	f := gitFile()
	f.Content = "different content entirely"
	f.CommitSHA = "deadbeef00"
	f.BlobURL = "https://github.com/acme/widgets/blob/other-branch/docs/guide.md"
	f.DocUpdatedAt = f.DocUpdatedAt.Add(24 * time.Hour)
	b := ToOnyxPayload(f, models.DocumentSourceGitHub, 42, base)

	if a.Document.ID != b.Document.ID {
		t.Errorf("ID = %q, want %q for identical (kind, base, relpath)", b.Document.ID, a.Document.ID)
	}
	if want := wantID(models.KindGit, base, "docs/guide.md"); a.Document.ID != want {
		t.Errorf("ID = %q, want %q", a.Document.ID, want)
	}
}

// TestIDMigration: same base + relpath → same ID regardless of the actual
// origin/root the file came from; a different base mints a different ID.
func TestIDMigration(t *testing.T) {
	base := "https://github.com/acme/widgets"
	want := wantID(models.KindGit, base, "docs/guide.md")

	forked := gitFile()
	forked.RootLabel = "alice/widgets-fork" // actual origin changed
	if got := ToOnyxPayload(forked, models.DocumentSourceGitHub, 42, base).Document.ID; got != want {
		t.Errorf("forked repo with same base: ID = %q, want %q", got, want)
	}

	other := ToOnyxPayload(gitFile(), models.DocumentSourceGitHub, 42, "acme-widgets-canonical")
	if other.Document.ID == want {
		t.Errorf("different base produced identical ID %q", other.Document.ID)
	}
	if wantOther := wantID(models.KindGit, "acme-widgets-canonical", "docs/guide.md"); other.Document.ID != wantOther {
		t.Errorf("other-base ID = %q, want %q", other.Document.ID, wantOther)
	}
}

// TestIDKindPrefix: the kind is part of the hash input, so git and local
// documents with the same base and relpath get disjoint IDs.
func TestIDKindPrefix(t *testing.T) {
	const base = "sard-canonical-base"
	git := gitFile()
	local := localFile()
	local.RelPath = git.RelPath // same relpath, different kind

	g := ToOnyxPayload(git, models.DocumentSourceGitHub, 42, base)
	l := ToOnyxPayload(local, models.DocumentSourceFile, 42, base)

	if want := wantID(models.KindGit, base, git.RelPath); g.Document.ID != want {
		t.Errorf("git ID = %q, want %q", g.Document.ID, want)
	}
	if want := wantID(models.KindLocal, base, git.RelPath); l.Document.ID != want {
		t.Errorf("local ID = %q, want %q", l.Document.ID, want)
	}
	if g.Document.ID == l.Document.ID {
		t.Errorf("git and local IDs collide: %q", g.Document.ID)
	}
}

// TestSectionLink: a git document carries the blob URL; a local document's
// section omits the link key entirely in the JSON.
func TestSectionLink(t *testing.T) {
	src := gitFile()
	git := ToOnyxPayload(src, models.DocumentSourceGitHub, 42, "base")
	wantSec := models.Section{Text: src.Content, Link: &src.BlobURL}
	if got := git.Document.Sections; !reflect.DeepEqual(got, []models.Section{wantSec}) {
		t.Errorf("git sections = %v, want %v", got, []models.Section{wantSec})
	}

	raw, err := json.Marshal(git)
	if err != nil {
		t.Fatalf("json.Marshal(git): %v", err)
	}
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("json.Unmarshal(git): %v", err)
	}
	sec := top["document"].(map[string]any)["sections"].([]any)[0].(map[string]any)
	if _, has := sec["link"]; !has {
		t.Errorf("git section should carry its link: %v", sec)
	}
	if sec["link"] != gitFile().BlobURL {
		t.Errorf("git section link = %v, want %q", sec["link"], gitFile().BlobURL)
	}

	local := ToOnyxPayload(localFile(), models.DocumentSourceFile, 42, "base")
	if local.Document.Sections[0].Link != nil {
		t.Errorf("local section link = %v, want nil", *local.Document.Sections[0].Link)
	}
	rawLocal, err := json.Marshal(local)
	if err != nil {
		t.Fatalf("json.Marshal(local): %v", err)
	}
	var topLocal map[string]any
	if err := json.Unmarshal(rawLocal, &topLocal); err != nil {
		t.Fatalf("json.Unmarshal(local): %v", err)
	}
	secLocal := topLocal["document"].(map[string]any)["sections"].([]any)[0].(map[string]any)
	if _, has := secLocal["link"]; has {
		t.Errorf("local section must omit the link key entirely: %v", secLocal)
	}
}

func TestHeadingTitle(t *testing.T) {
	withContent := func(content string) models.IngestedFile {
		f := localFile()
		f.Content = content
		return f
	}
	tests := []struct {
		name string
		f    models.IngestedFile
		want string
	}{
		{"first level-1 heading wins", withContent("Preamble line\n# First\n# Second\n## Deeper"), "First"},
		{"deeper headings are skipped", withContent("## Sub\n   # Real title\n### Also deep"), "Real title"},
		{"leading whitespace before heading", withContent("\n   # Spaced"), "Spaced"},
		{"hash without space is not a heading", withContent("#NoSpace\n## Still not"), "plan.md"},
		{"no heading falls back to basename", withContent("Just prose, no heading."), "plan.md"},
		// CRLF (T9 edge case): the \r at the end of a heading line is
		// trimmed away — the title never carries a trailing \r.
		{"CRLF right after the heading line", withContent("# Title\r\n"), "Title"},
		{"CRLF heading mid-file", withContent("Preamble line\r\n## Sub\r\n# Real title\r\nmore"), "Real title"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ToOnyxPayload(tc.f, models.DocumentSourceFile, 1, "base").Document.Title; got != tc.want {
				t.Errorf("Title = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLargeSingleLineFile: a large (512 KiB) single-line file with no
// heading maps cleanly — the title falls back to the basename, the
// section carries the full content, and the ID is stable and
// deterministic across identical calls (T9 edge case: very long single
// file).
func TestLargeSingleLineFile(t *testing.T) {
	content := strings.Repeat("x", 512*1024)
	f := localFile()
	f.RelPath = "logs/big.md"
	f.Content = content

	p1 := ToOnyxPayload(f, models.DocumentSourceFile, 42, "base")
	p2 := ToOnyxPayload(f, models.DocumentSourceFile, 42, "base")
	if p1.Document.ID != p2.Document.ID {
		t.Errorf("two identical calls produced different IDs: %q vs %q", p1.Document.ID, p2.Document.ID)
	}
	if want := wantID(models.KindLocal, "base", "logs/big.md"); p1.Document.ID != want {
		t.Errorf("ID = %q, want %q", p1.Document.ID, want)
	}
	if p1.Document.Title != "big.md" {
		t.Errorf("Title = %q, want the basename %q (no heading in the file)", p1.Document.Title, "big.md")
	}
	if p1.Document.Sections[0].Text != content {
		t.Errorf("section text = %d bytes, want the full %d-byte content",
			len(p1.Document.Sections[0].Text), len(content))
	}
}

// TestCRLFPreserved: CRLF content passes through the transform byte for
// byte — the section text is the file's content verbatim, \r and all
// (T9 edge case: CRLF).
func TestCRLFPreserved(t *testing.T) {
	f := localFile()
	const crlf = "# Title\r\nline one\r\nline two\r\n"
	f.Content = crlf

	got := ToOnyxPayload(f, models.DocumentSourceFile, 42, "base")
	if got.Document.Sections[0].Text != crlf {
		t.Errorf("section text = %q, want the CRLF content verbatim %q", got.Document.Sections[0].Text, crlf)
	}
	if got.Document.Title != "Title" {
		t.Errorf("Title = %q, want %q (no trailing \\r)", got.Document.Title, "Title")
	}
}

// TestDocUpdatedAt: a non-UTC input is converted to RFC-3339 UTC ("…Z").
func TestDocUpdatedAt(t *testing.T) {
	f := gitFile()
	f.DocUpdatedAt = time.Date(2026, 3, 4, 15, 6, 7, 0, time.FixedZone("UTC+02:00", 2*3600))
	got := ToOnyxPayload(f, models.DocumentSourceGitHub, 42, "base").Document.DocUpdatedAt
	if want := "2026-03-04T13:06:07Z"; got != want {
		t.Errorf("DocUpdatedAt = %q, want %q", got, want)
	}
}

func TestMetadataGit(t *testing.T) {
	got := ToOnyxPayload(gitFile(), models.DocumentSourceGitHub, 42, "base").Document.Metadata
	want := map[string]any{
		"repo":        "acme/widgets",
		"path":        "docs/guide.md",
		"commit":      "abc123def456",
		"ingested_by": "sardonyx",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("metadata = %v, want %v", got, want)
	}
}

func TestMetadataLocal(t *testing.T) {
	got := ToOnyxPayload(localFile(), models.DocumentSourceFile, 42, "base").Document.Metadata
	want := map[string]any{
		"path":        "notes/plan.md",
		"ingested_by": "sardonyx",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("metadata = %v, want %v", got, want)
	}
}

// TestDocumentShape: the remaining per-document fields map straight from the
// inputs (semantic identifier, source, CC-pair ID, ingestion flag).
func TestDocumentShape(t *testing.T) {
	git := ToOnyxPayload(gitFile(), models.DocumentSourceGitHub, 243, "base")
	if got, want := git.Document.SemanticIdentifier, "acme/widgets/docs/guide.md"; got != want {
		t.Errorf("SemanticIdentifier = %q, want %q", got, want)
	}
	if got, want := git.Document.Source, models.DocumentSourceGitHub; got != want {
		t.Errorf("Source = %q, want %q", got, want)
	}
	if got, want := git.CCPairID, 243; got != want {
		t.Errorf("CCPairID = %d, want %d", got, want)
	}
	if !git.Document.FromIngestionAPI {
		t.Error("FromIngestionAPI = false, want true")
	}
	if len(git.Document.Sections) != 1 || git.Document.Sections[0].Text != gitFile().Content {
		t.Errorf("sections = %v, want one section with the file content", git.Document.Sections)
	}
}
