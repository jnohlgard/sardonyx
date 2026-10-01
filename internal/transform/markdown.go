// Package transform maps discovered files to Onyx Ingestion API payloads
// (docs/PLAN.md §6, task T3).
package transform

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"strings"
	"time"

	"sardonyx/internal/models"
)

// ToOnyxPayload maps a discovered file to an Onyx Ingestion API payload
// (docs/PLAN.md §6). It is pure: the document ID is the deterministic sha256
// of kind + idBase + relpath, so volatile facts (branch, commit, mtime) never
// leak into it, and idBase is the run-level value the pipeline supplies (the
// --id-base / SARD_ID_BASE override or the source's default).
func ToOnyxPayload(f models.IngestedFile, source string, ccPairID int, idBase string) models.OnyxPayload {
	meta := map[string]any{
		"path":        f.RelPath,
		"ingested_by": "sardonyx",
	}
	if f.Kind == models.KindGit {
		meta["repo"] = f.RootLabel
		meta["commit"] = f.CommitSHA
	}

	section := models.Section{Text: f.Content}
	if f.BlobURL != "" {
		section.Link = &f.BlobURL
	}

	return models.OnyxPayload{
		CCPairID: ccPairID,
		Document: models.OnyxDocument{
			ID:                 documentID(f.Kind, idBase, f.RelPath),
			SemanticIdentifier: f.RootLabel + "/" + f.RelPath,
			Title:              headingTitle(f.Content, f.RelPath),
			Sections:           []models.Section{section},
			Source:             source,
			Metadata:           meta,
			DocUpdatedAt:       f.DocUpdatedAt.UTC().Format(time.RFC3339),
			FromIngestionAPI:   true,
		},
	}
}

// documentID returns the stable Onyx document ID: the lowercase hex of sha256
// over "<kind>\x00<idBase>\x00<relpath>" (docs/PLAN.md §6). The kind prefix
// keeps the git and local ID spaces disjoint; the base and relpath are the
// only other inputs, so branch and commit never influence the ID.
func documentID(kind, idBase, relPath string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + idBase + "\x00" + relPath))
	return hex.EncodeToString(sum[:])
}

// headingTitle returns the text of the file's first level-1 ATX heading — a
// line that, after trimming leading whitespace, begins with a single "#"
// followed by a space — or the file's basename when no such heading exists
// (docs/PLAN.md §6).
func headingTitle(content, relPath string) string {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, "# ") {
			if rest := strings.TrimSpace(trimmed[2:]); rest != "" {
				return rest
			}
		}
	}
	return path.Base(relPath)
}
