// Copyright (C) 2026 Joakim Nohlgård
// SPDX-License-Identifier: AGPL-3.0

// Package models holds the record types shared across the pipeline
// (docs/PLAN.md §5.4, §6, §7).
package models

import "time"

// IngestedFile.Kind values.
const (
	KindGit   = "git"
	KindLocal = "local"
)

// IngestedFile — one discovered file, ready for transform.
type IngestedFile struct {
	Kind         string    // "git" | "local"
	RootLabel    string    // "owner/repo" or basename(abs(dir))
	RelPath      string    // relative to the source root, forward slashes
	Content      string    // Markdown text
	DocUpdatedAt time.Time // commit time (git) or mtime (local), UTC
	CommitSHA    string    // git source only
	BlobURL      string    // github.com source only
}

// IngestResult.Status values.
const (
	ResultCreated = "created"
	ResultUpdated = "updated"
	ResultFailed  = "failed"
)

// IngestResult — per-document outcome from the Onyx client. Reason carries a
// human-readable explanation and is only populated for failures.
type IngestResult struct {
	Status string
	Reason string
}

// OK reports whether the ingestion succeeded (created or updated).
func (r IngestResult) OK() bool {
	return r.Status == ResultCreated || r.Status == ResultUpdated
}

// DocumentSource values for OnyxDocument.Source (docs/onyx-ingestion-api.md).
const (
	DocumentSourceFile         = "file"
	DocumentSourceGitHub       = "github"
	DocumentSourceGitLab       = "gitlab"
	DocumentSourceWeb          = "web"
	DocumentSourceIngestionAPI = "ingestion_api"
)

// OnyxDocument — the "document" object of the Ingestion API request body.
// JSON tags must match the Onyx schema exactly (docs/onyx-ingestion-api.md).
// Intentionally omitted per §6: chunk_count (Onyx computes it),
// primary_owners / secondary_owners, additional_info, image sections.
type OnyxDocument struct {
	ID                 string         `json:"id"`
	SemanticIdentifier string         `json:"semantic_identifier"`
	Title              string         `json:"title"`
	Sections           []Section      `json:"sections"`
	Source             string         `json:"source"`
	Metadata           map[string]any `json:"metadata"`       // string or []string values only
	DocUpdatedAt       string         `json:"doc_updated_at"` // pre-formatted RFC-3339 UTC
	FromIngestionAPI   bool           `json:"from_ingestion_api"`
}

// Section — one section of an OnyxDocument. Link is optional; a nil pointer
// omits the key from the JSON so local documents carry no link at all.
type Section struct {
	Text string  `json:"text"`
	Link *string `json:"link,omitempty"`
}

// OnyxPayload — the top-level request body for POST /onyx-api/ingestion:
// {"document": {...}, "cc_pair_id": n}.
type OnyxPayload struct {
	Document OnyxDocument `json:"document"`
	CCPairID int          `json:"cc_pair_id"`
}
