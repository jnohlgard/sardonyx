// Command sard ingests markdown files from a git repository URL or a local
// directory into Onyx via the Ingestion API.
//
// TODO (T7): parse flags per docs/PLAN.md §4 and run the pipeline via
// internal/cli; map the result to the exit codes in §8.
package main

import (
	"fmt"
	"os"
)

// version is set at build time: go build -ldflags "-X main.version=…"
var version = "dev"

func main() {
	fmt.Fprintf(os.Stderr, "sard %s: not implemented yet — see docs/PLAN.md §10 (T7)\n", version)
	os.Exit(2)
}
