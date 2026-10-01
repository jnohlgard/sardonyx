// Copyright (C) 2026 Joakim Nohlgård
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY of any kind. See LICENSE for the full text.

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
