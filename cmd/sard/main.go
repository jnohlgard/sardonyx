// Copyright (C) 2026 Joakim Nohlgård
// SPDX-License-Identifier: AGPL-3.0

// Command sard ingests Markdown files from a git repository URL or a
// local directory into Onyx via the Ingestion API.
//
// This entry point stays deliberately thin: it hands every argument to
// cli.Run, which owns flag parsing, logging (log/slog to stderr), the
// pipeline, and the exit codes (docs/PLAN.md §8), and exits with the
// returned code. main prints nothing on its own.
package main

import (
	"os"

	"sardonyx/internal/cli"
)

// version is set at build time: go build -ldflags "-X main.version=…"
var version = "dev"

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
