// Copyright (C) 2026 Joakim Nohlgård
// SPDX-License-Identifier: AGPL-3.0

// Package cli wires sard's pipeline together: command-line parsing,
// config resolution, source discovery, transform, ingestion, and the
// final summary (docs/PLAN.md §3, §4; task T7) — plus the `sard check`
// pre-flight subcommand (docs/PLAN.md §13; task T11b), which verifies
// the environment a real run needs without creating any document, and
// the `sard ls` subcommand (docs/PLAN.md §14; task T12b), which lists
// the documents the API key can see without creating, updating, or
// deleting anything.
//
// Run is the single entry point used by cmd/sard. It builds the cobra
// command tree — the root `sard` command and the `ingest`, `check`,
// and `ls` subcommands, "sard ingest <source> [options]", "sard check
// [options]", and "sard ls [options]" — and executes it. The flag
// definitions on the ingest command are the single source of truth for
// the --help output: cobra renders the usage line and the option list
// from the same definitions that bind the parsed values, so the two
// cannot drift apart. pflag parses flags and the positional <source> in
// any order (both "sard ingest ./docs --dry-run" and
// "sard ingest --dry-run ./docs" work), and pflag's StringArray type
// makes --include/--exclude repeatable. Both commands run with
// SilenceErrors/SilenceUsage set — the framework never prints on its
// own: usage and error diagnostics come out of Run (exit 2), and
// everything else through the run's log/slog logger on stderr.
//
// After parsing, the ingest command's RunE (runIngest) builds the
// run's log/slog logger from --log-level, resolves configuration
// (config.Resolve), classifies the input (an existing local directory →
// source.Local, anything else → source.Git), and runs discovery →
// transform → ingest sequentially under signal.NotifyContext so Ctrl-C
// aborts promptly. The check command's RunE (runCheck) does the same
// pre-flight — build logger, validate the flags, resolve configuration
// with DryRun: false (exactly the required-credentials set of a real
// run; that is the point of the command) — and then runs
// onyx.Client.Check under signal.NotifyContext (docs/PLAN.md §13.6).
// The ls command's RunE (runLs) is the same shape minus the cc-pair
// validation: build logger, resolve configuration with CCPairOptional:
// true (ls needs only the API key, §14.3.1), run onyx.Client.ListDocs
// under signal.NotifyContext, print the list to stdout and the summary
// (or the empty-list warning) to stderr (docs/PLAN.md §14.5).
//
// --help prints the auto-generated usage to stdout and exits 0 (cobra's
// convention); the root's --version flag prints the build-stamped
// version string (Version, default "dev") to stdout and exits 0. All
// other output — logging, error diagnostics, usage on errors — goes to
// stderr, so stdout stays clean for --dry-run payloads. cmd/sard stays
// a thin wrapper: it assigns the build-time main.version stamp to
// Version and maps Run's code to os.Exit.
//
// Exit codes (docs/PLAN.md §8):
//
//   - 0 — the run completed and every discovered file was ingested
//     successfully, or 0 files were found (a warning is logged). A
//     successful --dry-run — discovery OK, one valid JSON payload per
//     file on stdout, nothing sent — also exits 0.
//   - 1 — the run completed but at least one file failed to ingest, or a
//     runtime source failure (e.g. a git clone failure, a missing git
//     binary) stopped the run.
//   - 2 — a configuration error before ingestion: a missing/invalid
//     setting (config.ErrConfiguration), a bad flag value (unknown
//     --source, negative --limit / --max-depth / --max-file-size /
//     --cc-pair-id, invalid --log-level, an unknown flag, a wrong number
//     of arguments), an unrecognized input (bad path, invalid git URL
//     form), a missing subcommand, or an Onyx 401/403. The auth case is
//     deliberate: the client fails fast with onyx.ErrAuth because every
//     remaining file would fail identically, and a rejected key is a
//     credentials problem, so the run aborts with 2 rather than 1.
//     Missing Onyx credentials are a configuration error for a real run
//     only; a dry run sends nothing and needs none.
//   - 130 — the run was interrupted (SIGINT via signal.NotifyContext).
//
// sard check (docs/PLAN.md §13) maps its own exit codes:
//
//   - 0 — the server is reachable, the key was accepted (probe 2 or
//     the 2f fallback), and the cc-pair was validated — or, where the
//     best-effort cc-pair probe could not run, that was reported as a
//     warning.
//   - 1 — the server is unreachable (a connection failure/timeout —
//     probe 1, or probes 2/2f after retries) or a persistent 429/5xx.
//   - 2 — the same configuration pre-flight as a real run (missing or
//     invalid ONYX_API_KEY / ONYX_CC_PAIR_ID, a negative --cc-pair-id,
//     an invalid --log-level) — or a rejected key (401/403) — or the
//     configured cc-pair-id not present on the deployment (probe 3,
//     404) — or no Onyx Ingestion API at the configured URL (2f, other
//     4xx).
//   - 130 — interrupted (Ctrl-C).
//
// Like ingest, check prints everything to stderr — one line per probe,
// then a final verdict line on success (plain, no log prefix:
// "OK: all checks passed in …", or, when the pass carried warnings,
// "OK: all required checks passed in … (N warnings above)"), or a
// single actionable error line on failure — and stdout stays empty.
//
// sard ls (docs/PLAN.md §14) maps its own exit codes:
//
//   - 0 — the list was retrieved — including an empty list (a warning
//     is logged instead of a summary).
//   - 1 — the server is unreachable (a connection failure/timeout after
//     retries, or a persistent 429/5xx) — or a 200 whose body is
//     neither a bare array nor a {data: [...]} envelope (malformed
//     response).
//   - 2 — a configuration error (missing/invalid ONYX_API_KEY, an
//     invalid --log-level) — or a rejected key (401/403: the same
//     classification as the ingest fail-fast and check) — or the GET
//     /onyx-api/ingestion endpoint unavailable at the configured URL
//     (404/405/other 4xx).
//   - 130 — interrupted (Ctrl-C).
//
// Unlike ingest and check, ls writes the list itself to stdout — one
// "<document_id>\t<semantic_id>" line per document (a third,
// tab-separated link field only when non-empty), in the order the
// server returned it; the summary (or the empty-list warning) and all
// diagnostics go to stderr (the §8 discipline).
//
// --dry-run never exits 1 (it sends nothing, so no file can fail); a
// discovery error in dry-run still exits 2. A dry run needs no Onyx
// credentials (no API key, no cc-pair id): it prints the would-be
// payloads and sends nothing, so zero configuration is enough
// (docs/PLAN.md §4). --limit caps the number of
// files ingested — and the number of payloads printed in a dry-run — and
// the summary's total (the summary describes the capped set; the
// skipped count comes from discovery and is unaffected by --limit).
//
// --branch and --token are git-only flags: for a local directory input
// they are ignored with a warning (their values are never logged).
//
// The run ends with a summary on stderr (docs/PLAN.md §8): a header
// line — "<verb>: <N> files in <elapsed>" — carrying the created,
// updated, skipped, and failed counts (a dry run instead reports the
// number of payloads printed and the skipped count), followed by one
// "  failed: <file> — <reason>" line per failed file in ingestion
// (RelPath) order. The elapsed time spans the whole run from before
// discovery (the git clone dominates) to the last action. During a real
// run, every file additionally gets a progress line, "[i/N] <file> →
// created|updated" (info) or "→ failed: <reason>" (warn).
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"sardonyx/internal/config"
	"sardonyx/internal/models"
	"sardonyx/internal/onyx"
	"sardonyx/internal/source"
	"sardonyx/internal/transform"
)

// validSources are the accepted values for --source (the Onyx
// DocumentSource enum, docs/onyx-ingestion-api.md).
var validSources = []string{
	models.DocumentSourceFile,
	models.DocumentSourceGitHub,
	models.DocumentSourceGitLab,
	models.DocumentSourceWeb,
	models.DocumentSourceIngestionAPI,
}

// ingestFlags holds the parsed state of one `sard ingest` invocation.
type ingestFlags struct {
	apiURL      string
	apiKey      string
	ccPairID    int
	branch      string
	source      string
	idBase      string
	include     []string
	exclude     []string
	maxDepth    int
	maxFileSize int
	token       string
	dryRun      bool
	limit       int
	logLevel    string
}

// errNoSubcommand is what the root command's RunE returns when `sard` is
// invoked without a subcommand (docs/PLAN.md §4: exit 2).
var errNoSubcommand = errors.New("no subcommand")

// exitCode carries the run's exit code (docs/PLAN.md §8) out of the
// ingest command's RunE back into Run. The pipeline logs its own
// diagnostics, so the command runs with SilenceErrors/SilenceUsage set
// and surfaces the result only this way.
type exitCode int

func (e exitCode) Error() string {
	return fmt.Sprintf("run finished with exit code %d", int(e))
}

// The commands render their usage from cobra's own default template
// rather than a forked copy. cobra's defaultUsageTemplate is fetched at
// runtime (cobraDefaultUsageTemplate), so it stays in sync with the
// cobra version pinned in go.mod by construction; the only
// cobra-version-sensitive text in this file is the small anchor at each
// splice point, and TestUsageTemplateAnchors fails if a cobra bump moves
// it.
//
// withUsageSections splices a command's static sections (an Arguments
// section, an Environment section, or both) into the default template,
// inside its {{if .Runnable}} block, right below the usage line — the
// command's Long text is a summary, and the <source> description reads
// better next to the usage line it documents than at the top of the help
// output. Each subcommand sets its own template this way because a
// command inherits its parent's usage template, and the root uses
// rootUsageTemplate.
func cobraDefaultUsageTemplate() string {
	return (&cobra.Command{}).UsageTemplate()
}

// withUsageSections returns the usage template for a command carrying
// static sections (an Arguments section, an Environment section, or both)
// to be rendered below the usage line and above the Flags section. The
// sections are spliced into the default template where its usage line
// ({{.UseLine}}) closes, inside the {{if .Runnable}} block, so they appear
// only for commands that have a usage line.
func withUsageSections(sections string) string {
	const anchor = "{{.UseLine}}{{end}}"
	return strings.Replace(cobraDefaultUsageTemplate(), anchor,
		"{{.UseLine}}\n\n"+sections+"{{end}}", 1)
}

// rootUsageTemplate is cobra's default usage template with the
// {{if .Runnable}} usage-line block removed: the root takes no arguments
// or flags of its own — it only dispatches to subcommands, so a "sard
// [flags]" line would be misleading.
func rootUsageTemplate() string {
	const anchor = "{{if .Runnable}}\n  {{.UseLine}}{{end}}"
	return strings.Replace(cobraDefaultUsageTemplate(), anchor, "", 1)
}

// ingestUsageSections is the static text spliced into `sard ingest`'s
// usage, below the usage line: the <source> argument and an Environment
// summary of the env vars behind the flags (the names must stay in sync
// with internal/config; TestIngestUsageTemplateEnvNames guards that).
const ingestUsageSections = `Arguments:
  <source>  A git repository URL (https://host/owner/repo,
            git@host:owner/repo, or the owner/repo shorthand for
            github.com) or a local directory path.

Environment:
  ONYX_API_URL     Onyx API base URL (--api-url)
  ONYX_API_KEY     Onyx API key (--api-key)
  ONYX_CC_PAIR_ID  connector-credential pair id (--cc-pair-id)
  GIT_TOKEN        token for private repos (--token)
  SARD_ID_BASE     document-ID base for the run (--id-base)`

// checkUsageSections is the static Environment section spliced into
// `sard check`'s usage: exactly the three Onyx variables behind its flags
// — no GIT_TOKEN or SARD_ID_BASE, since check has no git or ID-base flags
// (and no positional argument, so no Arguments section).
// TestCheckUsageTemplateEnvNames guards the names.
const checkUsageSections = `Environment:
  ONYX_API_URL     Onyx API base URL (--api-url)
  ONYX_API_KEY     Onyx API key (--api-key)
  ONYX_CC_PAIR_ID  connector-credential pair id (--cc-pair-id)`

// lsUsageSections is the static Environment section spliced into `sard
// ls`'s usage: exactly the two Onyx variables behind its flags — no
// ONYX_CC_PAIR_ID, since ls takes no cc-pair id.
// TestLsUsageTemplateEnvNames guards the names.
const lsUsageSections = `Environment:
  ONYX_API_URL     Onyx API base URL (--api-url)
  ONYX_API_KEY     Onyx API key (--api-key)`

// checkFlags holds the parsed state of one `sard check` invocation
// (docs/PLAN.md §13.1). It is separate from ingestFlags on purpose:
// check has no source, git, include/exclude, depth/size, --limit,
// --dry-run, or --id-base flags — discovery is out of scope for
// check, and git concerns do not apply.
type checkFlags struct {
	apiURL   string
	apiKey   string
	ccPairID int
	logLevel string
}

// newCheckCmd builds the `sard check` subcommand (docs/PLAN.md §13):
// the four Onyx-side flags bound to p and a RunE wired to runCheck.
// The flag definitions are the single source of truth for the --help
// output, exactly as for ingest: cobra renders the usage line and the
// option list from the same definitions that bind the parsed values.
// The Long text is a summary; there is no Arguments section because
// check takes no positional argument (cobra.NoArgs).
func newCheckCmd(p *checkFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Verify the Onyx environment without creating any document",
		Long: `Verifies the environment a real run needs — the Onyx server at
ONYX_API_URL answers, the API key is accepted with the
Ingestion API's permission requirement (manage:connectors or
admin), and the configured cc-pair id exists on the deployment
(best-effort) — without creating, updating, or deleting any
document.

It is the pre-flight for the first sard ingest: after setting up
your credentials, run it once; when it exits 0, the run cannot
fail on an environmental problem. Exit codes: 0 verified; 1
server unreachable; 2 configuration, credentials, or cc-pair
problem; 130 interrupted.`,
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if code := runCheck(p); code != 0 {
				return exitCode(code)
			}
			return nil
		},
	}
	cmd.SetUsageTemplate(withUsageSections(checkUsageSections))
	f := cmd.Flags()
	f.StringVar(&p.apiURL, "api-url", "", "Onyx API base URL (default: "+config.DefaultAPIURL+"; env: "+config.EnvAPIURL+")")
	f.StringVar(&p.apiKey, "api-key", "", "Onyx API key (env: "+config.EnvAPIKey+")")
	f.IntVar(&p.ccPairID, "cc-pair-id", 0, "Onyx connector-credential-pair id (env: "+config.EnvCCPairID+")")
	f.StringVar(&p.logLevel, "log-level", "info", "log level: debug | info | warning | error")
	return cmd
}

// lsFlags holds the parsed state of one `sard ls` invocation
// (docs/PLAN.md §14.1). It is separate from ingestFlags and checkFlags
// on purpose: ls has exactly three flags — no cc-pair id (the endpoint
// is key-scoped, §14.3.1) and no source/git/include/limit/dry-run
// flags (there is no discovery).
type lsFlags struct {
	apiURL   string
	apiKey   string
	logLevel string
}

// newLsCmd builds the `sard ls` subcommand (docs/PLAN.md §14): the
// three flags bound to p and a RunE wired to runLs. The flag
// definitions are the single source of truth for the --help output,
// exactly as for ingest and check: cobra renders the usage line and
// the option list from the same definitions that bind the parsed
// values. The Long text is a summary; there is no Arguments section
// because ls takes no positional argument (cobra.NoArgs).
func newLsCmd(p *lsFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List the documents the API key can see (read-only)",
		Long: `Lists the documents the configured API key can see on the Onyx
deployment — the documents previous sard ingest runs created or
updated — via the read-only GET /onyx-api/ingestion sibling of the
Ingestion POST.

It is read-only: it creates, updates, and deletes nothing, and it
needs only the API key (no cc-pair id). The list goes to stdout — one
line per document, "<document_id><TAB><semantic_id>" and a third
tab-separated link field only when the document has one — in the
order the server returns it; the summary goes to stderr. Exit codes:
0 list retrieved (an empty list exits 0 with a warning); 1 server
unreachable or a malformed list response; 2 configuration,
credentials, or endpoint problem; 130 interrupted.`,
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if code := runLs(p); code != 0 {
				return exitCode(code)
			}
			return nil
		},
	}
	cmd.SetUsageTemplate(withUsageSections(lsUsageSections))
	f := cmd.Flags()
	f.StringVar(&p.apiURL, "api-url", "", "Onyx API base URL (default: "+config.DefaultAPIURL+"; env: "+config.EnvAPIURL+")")
	f.StringVar(&p.apiKey, "api-key", "", "Onyx API key (env: "+config.EnvAPIKey+")")
	f.StringVar(&p.logLevel, "log-level", "info", "log level: debug | info | warning | error")
	return cmd
}

// Version is the running binary's version, printed by the root
// command's --version flag (`sard version <Version>` on stdout, exit
// 0). cmd/sard assigns it from the build-time main.version stamp
// (README "Building"); the default "dev" applies to plain builds.
var Version = "dev"

// newRootCmd builds the root `sard` command. Because Version is
// non-empty, cobra's execute() registers the built-in -v/--version
// flag on it (the root's only flag); invoked without a subcommand its
// RunE returns errNoSubcommand and Run prints the auto-generated
// top-level usage.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:     "sard",
		Version: Version,
		Short:   "Ingest Markdown files from a git repository or a local directory into Onyx",
		Long: `sard is a bulk-ingestion utility for Onyx (https://onyx.app): point
it at a git repository or a local directory and every .md, .mdx, and
.markdown file inside is converted into an Onyx document and sent to
the Onyx Ingestion API, so the documentation becomes part of Onyx's
knowledge base — searchable and usable by its AI features. It is a
one-shot sync: run it whenever your docs change. Re-running is safe;
document IDs are deterministic, so a second run updates existing
documents instead of duplicating them.

sard check is the pre-flight for the first real run: it verifies the
Onyx environment (server reachability, the API key's ingestion
permission, and the configured cc-pair's existence) without creating,
updating, or deleting any document — when it exits 0, the run cannot
fail on an environmental problem.

sard ls is read-only: it lists the documents the API key can see on
the deployment — the documents previous sard ingest runs created or
updated — one per line on stdout, and it needs only the API key.`,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE:          func(cmd *cobra.Command, args []string) error { return errNoSubcommand },
	}
	root.SetUsageTemplate(rootUsageTemplate())
	return root
}

// newIngestCmd builds the `sard ingest` subcommand (docs/PLAN.md §4):
// the flags bound to p and a RunE wired to runIngest. The flag
// definitions are the single source of truth for the flag list in
// --help — cobra renders the usage line and the option list from the
// same definitions that bind the parsed values. The Long text is a
// summary; the <source> description lives in the Arguments section of
// ingestUsageSections, below the usage line.
func newIngestCmd(p *ingestFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ingest <source>",
		Short: "Ingest a git repository or a local directory into Onyx",
		Long: `Ingests every Markdown file (.md, .mdx, .markdown) found under
<source> into Onyx via the Ingestion API: each file becomes one
document with a stable ID, so re-running the same command updates
documents instead of duplicating them.

Use --dry-run to print the would-be payloads to stdout — one JSON
document per file — and send nothing; a dry run needs no Onyx
credentials.`,
		Args:          exactlyOneSource,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if code := runIngest(args[0], p); code != 0 {
				return exitCode(code)
			}
			return nil
		},
	}
	cmd.SetUsageTemplate(withUsageSections(ingestUsageSections))
	f := cmd.Flags()
	f.StringVar(&p.apiURL, "api-url", "", "Onyx API base URL (default: "+config.DefaultAPIURL+"; env: "+config.EnvAPIURL+")")
	f.StringVar(&p.apiKey, "api-key", "", "Onyx API key (env: "+config.EnvAPIKey+")")
	f.IntVar(&p.ccPairID, "cc-pair-id", 0, "Onyx connector-credential-pair id (env: "+config.EnvCCPairID+")")
	f.StringVar(&p.branch, "branch", "", "branch to clone; git sources only")
	f.StringVar(&p.source, "source", "", "override the document source enum: "+strings.Join(validSources, " | "))
	f.StringVar(&p.idBase, "id-base", "", "arbitrary document-ID base for this run (env: "+config.EnvIDBase+")")
	f.StringArrayVar(&p.include, "include", nil, "include filter, repeatable (e.g. \"docs/**\")")
	f.StringArrayVar(&p.exclude, "exclude", nil, "exclude filter, repeatable (on top of the default noise-dir exclusions)")
	f.IntVar(&p.maxDepth, "max-depth", 0, "max directory depth below the source root (0 = unlimited)")
	f.IntVar(&p.maxFileSize, "max-file-size", 1024, "skip files larger than N KiB (0 = unlimited)")
	f.StringVar(&p.token, "token", "", "git auth token for private repos (env: "+config.EnvGitToken+")")
	f.BoolVar(&p.dryRun, "dry-run", false, "print the would-be payloads to stdout; send nothing")
	f.IntVar(&p.limit, "limit", 0, "ingest at most N files (0 = unlimited)")
	f.StringVar(&p.logLevel, "log-level", "info", "log level: debug | info | warning | error")
	return cmd
}

// exactlyOneSource is the ingest command's argument validator: the form
// is "sard ingest <source>" — exactly one positional argument.
func exactlyOneSource(cmd *cobra.Command, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("exactly one <source> argument is required (got %d)", len(args))
	}
	return nil
}

// buildLogger builds the run's slog logger from the --log-level value
// (debug | info | warning | error). All log output goes to stderr so
// stdout stays clean for --dry-run payloads.
func buildLogger(level string) (*slog.Logger, error) {
	var l slog.Level
	switch strings.ToLower(level) {
	case "debug":
		l = slog.LevelDebug
	case "info":
		l = slog.LevelInfo
	case "warn", "warning":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		return nil, fmt.Errorf("invalid --log-level %q (want debug, info, warning, or error)", level)
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l})), nil
}

// validate pre-flights the parsed flag values (docs/PLAN.md §8: bad flag
// values are a configuration error, exit 2). It runs before
// config.Resolve; both failure kinds exit 2 anyway, so the order only
// decides which problem is reported first — a bad flag beats a missing
// environment value.
func validate(p *ingestFlags) error {
	if p.source != "" {
		if slices.Contains(validSources, p.source) {
			return nil
		}
		return fmt.Errorf("invalid --source %q (want %s)", p.source, strings.Join(validSources, " | "))
	}
	if p.ccPairID < 0 {
		return fmt.Errorf("invalid --cc-pair-id %d (must be >= 0)", p.ccPairID)
	}
	if p.limit < 0 {
		return fmt.Errorf("invalid --limit %d (must be >= 0)", p.limit)
	}
	if p.maxDepth < 0 {
		return fmt.Errorf("invalid --max-depth %d (must be >= 0)", p.maxDepth)
	}
	if p.maxFileSize < 0 {
		return fmt.Errorf("invalid --max-file-size %d (must be >= 0)", p.maxFileSize)
	}
	return nil
}

// pathLike reports whether s is written as an explicit filesystem path
// (absolute, or with a ./ or ../ prefix) rather than a repository URL or
// owner/repo shorthand. A path-like input that doesn't name an existing
// directory — a missing path, or an existing file such as ./notes.md —
// is a configuration error (exit 2), never a clone; a bare two-segment
// string without a path prefix is the owner/repo shorthand and fails,
// if at all, at clone time (exit 1).
func pathLike(s string) bool {
	if strings.Contains(s, "://") || strings.Contains(s, "@") {
		return false
	}
	return strings.HasPrefix(s, "/") || strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../")
}

// configError wraps msg as a configuration error (exit 2). The typed
// error unwraps config.ErrConfiguration without duplicating the
// sentinel's text in Error().
type configError struct{ msg string }

func (e *configError) Error() string { return e.msg }
func (e *configError) Unwrap() error { return config.ErrConfiguration }

func configErrorf(format string, args ...any) error {
	return &configError{msg: fmt.Sprintf(format, args...)}
}

// discovery is what the source layer produced: the files (sorted by
// RelPath), the number of files the source dropped in a per-file check
// (oversize, binary, empty, unreadable; the summary's "skipped"), the
// run's default document-ID base, and the default document source for
// the input.
type discovery struct {
	files         []models.IngestedFile
	skipped       int
	idBase        string
	defaultSource string
}

// discover runs the source that matches src (an existing directory →
// Local, anything else → Git) and returns its files plus the run's
// default ID base and document source (docs/PLAN.md §5).
//
// Error classes (mapped by Run per §8): an invalid path, an unrecognized
// git URL form, or a malformed glob is a configuration error
// (config.ErrConfiguration, exit 2); a clone failure, a missing git
// binary, and the like are runtime errors (exit 1).
func discover(src string, p *ingestFlags, settings *config.Settings, log *slog.Logger) (*discovery, error) {
	// The one shared filter both sources apply to their candidates
	// (docs/PLAN.md §5): built once here and used in both branches below.
	filter := source.Filter{
		Include:        p.include,
		Exclude:        p.exclude,
		MaxDepth:       p.maxDepth,
		MaxFileSizeKiB: p.maxFileSize,
	}

	fi, statErr := os.Stat(src)
	if statErr == nil && fi.IsDir() {
		if p.branch != "" {
			log.Warn("--branch applies to git sources only; ignoring it for a local directory")
		}
		if p.token != "" {
			log.Warn("--token applies to git sources only; ignoring it for a local directory")
		}
		res, err := source.Local(src, filter, log)
		if err != nil {
			return nil, err
		}
		return &discovery{
			files:         res.Files,
			skipped:       res.Skipped,
			idBase:        res.IDBase,
			defaultSource: models.DocumentSourceFile,
		}, nil
	}

	// Not a directory: a git repository input, unless the source is
	// written as an explicit filesystem path — absolute, or with a ./
	// or ../ prefix. A path-like input is never a repository shorthand:
	// an existing file (e.g. ./notes.md) is a mis-pointed source, a
	// missing one a bad path — both are configuration errors (exit 2),
	// not a clone attempt.
	if pathLike(src) {
		if statErr == nil {
			return nil, configErrorf("invalid source %q: not a directory — point sard at the directory containing the Markdown files", src)
		}
		return nil, configErrorf("invalid source %q: not an existing directory and not a git repository URL", src)
	}

	norm, err := source.NormalizeURL(src)
	if err != nil {
		return nil, configErrorf("invalid git source %q: %v", src, err)
	}
	if norm.Local {
		// file:// points at a local path; a missing one is a bad path.
		if _, err := os.Stat(norm.Path); err != nil {
			return nil, configErrorf("invalid source %q: %v", src, err)
		}
	}

	res, err := source.Git(src, source.GitOptions{
		Filter: filter,
		Branch: p.branch,
		Token:  settings.GitToken,
	}, log)
	if err != nil {
		return nil, err
	}
	return &discovery{
		files:         res.Files,
		skipped:       res.Skipped,
		idBase:        res.IDBase,
		defaultSource: norm.Source,
	}, nil
}

// Run executes one `sard` invocation and returns the exit code per
// docs/PLAN.md §8 (see the package doc). It builds the cobra command
// tree (root `sard` + the `ingest`, `check`, and `ls` subcommands),
// executes it, and maps the outcome: --help (and the built-in help
// command) printed usage and exit 0; the pipeline's own exit code
// (0/1/2/130, all diagnostics logged by the pipeline itself); a missing
// or unknown subcommand, and any flag/argument error on `sard ingest`,
// `sard check`, or `sard ls`, exit 2 with the relevant usage on stderr
// (the auto-generated top-level usage for root-level problems, the
// subcommand usage for subcommand-level ones). cmd/sard only maps the
// returned code to os.Exit.
func Run(args []string) int {
	root := newRootCmd()
	root.AddCommand(newIngestCmd(&ingestFlags{}))
	root.AddCommand(newCheckCmd(&checkFlags{}))
	root.AddCommand(newLsCmd(&lsFlags{}))
	root.SetArgs(args)

	cmd, err := root.ExecuteC()
	if err == nil {
		return 0 // --help: usage printed, no error
	}
	var ec exitCode
	switch {
	case errors.As(err, &ec):
		return int(ec)
	case cmd == root:
		// `sard` without a subcommand (the root's RunE returned
		// errNoSubcommand) or `sard <bogus>` (cobra's unknown-command
		// error): the auto-generated top-level usage on stderr, exit 2.
		// InitDefaultHelpFlag makes the -h line render identically on
		// both paths (it is normally initialized only once execute runs).
		root.InitDefaultHelpFlag()
		fmt.Fprintln(os.Stderr, root.UsageString())
		return 2
	default:
		// A flag or argument error on `sard ingest` or
		// `sard check`: the standard diagnostic plus the full
		// subcommand usage, exit 2.
		w := cmd.ErrOrStderr()
		fmt.Fprintln(w, cmd.ErrPrefix(), err)
		fmt.Fprintln(w, cmd.UsageString())
		return 2
	}
}

// runIngest runs the pipeline for one `sard ingest <source>`
// invocation (the ingest command's RunE body) and returns the exit code
// per docs/PLAN.md §8 (see the package doc): build the logger from
// --log-level, validate the flag values, resolve configuration, run
// discovery under a signal-aware context, then the dry-run printer or
// the ingest loop.
func runIngest(src string, p *ingestFlags) int {
	log, err := buildLogger(p.logLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
	}
	if err := validate(p); err != nil {
		log.Error(err.Error())
		return 2
	}
	settings, err := config.Resolve(config.Flags{
		APIURL:   p.apiURL,
		APIKey:   p.apiKey,
		CCPairID: p.ccPairID,
		GitToken: p.token,
		IDBase:   p.idBase,
		DryRun:   p.dryRun,
	})
	if err != nil {
		log.Error(err.Error())
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// The summary's elapsed time spans the whole run (docs/PLAN.md §8):
	// for a git source the clone dominates, so the clock starts before
	// discovery, not before the ingest loop.
	start := time.Now()
	d, err := discover(src, p, settings, log)
	if err != nil {
		log.Error(err.Error())
		if config.IsConfigurationError(err) {
			return 2
		}
		return 1
	}

	idBase := settings.IDBase
	if idBase == "" {
		idBase = d.idBase
	}
	docSource := p.source
	if docSource == "" {
		docSource = d.defaultSource
	}

	files := d.files
	if p.limit > 0 && len(files) > p.limit {
		files = files[:p.limit]
	}

	if len(files) == 0 {
		log.Warn("no Markdown files found", "source", src, "skipped", d.skipped)
		return 0
	}

	elapsed := time.Since(start)
	if p.dryRun {
		return runDry(ctx, files, docSource, settings.CCPairID, idBase, d.skipped, elapsed, log)
	}
	return ingestAll(ctx, files, docSource, settings, idBase, d.skipped, elapsed, log)
}

// runDry prints the would-be payload of every file to stdout — one
// compact JSON document per line, in the source's stable RelPath order —
// and sends nothing. It ends with a summary on stderr (docs/PLAN.md §8):
// the total, the number of payloads printed, the skipped count from
// discovery, and the whole-run elapsed time. stdout stays strictly "one
// JSON payload per file."
func runDry(ctx context.Context, files []models.IngestedFile, docSource string, ccPairID int, idBase string, skipped int, elapsed time.Duration, log *slog.Logger) int {
	var printed int
	for _, f := range files {
		if ctx.Err() != nil {
			break
		}
		payload := transform.ToOnyxPayload(f, docSource, ccPairID, idBase)
		b, err := json.Marshal(payload)
		if err != nil {
			log.Error("marshaling payload", "file", f.RelPath, "error", err)
			return 1
		}
		fmt.Fprintln(os.Stdout, string(b))
		printed++
	}

	if ctx.Err() != nil {
		log.Warn(summaryHeader("run interrupted", elapsed, len(files),
			fmt.Sprintf("printed %d %s, skipped %d", printed, plural(printed, "payload"), skipped)))
		return 130
	}
	log.Info(summaryHeader("dry run complete", elapsed, len(files),
		fmt.Sprintf("printed %d %s, skipped %d", printed, plural(printed, "payload"), skipped)))
	return 0
}

// runCheck runs the environment pre-flight for one `sard check`
// invocation and returns the exit code per docs/PLAN.md §13.4 (see the
// package doc): build the logger from --log-level, validate the flag
// values (--cc-pair-id >= 0; 0 = unset, as in ingest), resolve
// configuration exactly as a real run does (config.Resolve with
// DryRun: false — the point of the command), run the three probes
// under a signal-aware context, and print the report. A failed check
// prints no verdict line: a single actionable error line, then the
// exit code (the same shape as the ingest auth fail-fast).
func runCheck(p *checkFlags) int {
	log, err := buildLogger(p.logLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
	}
	if p.ccPairID < 0 {
		log.Error(fmt.Sprintf("invalid --cc-pair-id %d (must be >= 0)", p.ccPairID))
		return 2
	}
	settings, err := config.Resolve(config.Flags{
		APIURL:   p.apiURL,
		APIKey:   p.apiKey,
		CCPairID: p.ccPairID,
		DryRun:   false,
	})
	if err != nil {
		log.Error(err.Error())
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	start := time.Now()
	client := onyx.NewClient(settings.APIURL, settings.APIKey, settings.CCPairID)
	res, err := client.Check(ctx)
	if err != nil {
		if ctx.Err() != nil {
			log.Warn("check interrupted (Ctrl-C)")
			return 130
		}
		log.Error(err.Error())
		if errors.Is(err, onyx.ErrUnreachable) {
			return 1
		}
		return 2 // ErrAuth, ErrNoIngestionAPI, or ErrCCPairNotFound
	}

	printCheckReport(res, settings, time.Since(start), log)
	return 0
}

// printCheckReport renders the check's report (docs/PLAN.md §13.5):
// one line per probe — all on stderr through the run's logger — then
// a final verdict line. The verdict is plain (no time=/level= log
// prefix, so it reads at a glance at the end of the report):
// "OK: all checks passed in …" when every probe passed, or
// "OK: all required checks passed in … (N warnings above)" when the
// exit-0 pass carried warnings (a degraded health, a key verified via
// the 2f fallback, an unvalidated cc-pair) — the warning count matches
// the warning lines the report printed. It is written straight to
// stderr (not through the logger) so it appears at every log level.
func printCheckReport(res onyx.CheckResult, settings *config.Settings, elapsed time.Duration, log *slog.Logger) {
	warnings := 0
	if res.Healthy {
		log.Info(fmt.Sprintf("Onyx at %s: health ok", settings.APIURL))
	} else {
		log.Warn(fmt.Sprintf("Onyx at %s: health check inconclusive (no 200 with success=true); continuing — the key probe doubles as the reachability test", settings.APIURL))
		warnings++
	}

	if res.UsedFallback {
		log.Info("API key accepted via the POST fallback (the GET /onyx-api/ingestion endpoint is unavailable on this deployment)")
		log.Warn("caveat: the key was verified with a deliberately-invalid body; a deployment that validates the body before auth could have misreported a bad key (docs/PLAN.md §13.3)")
		warnings++
	} else {
		log.Info(fmt.Sprintf("API key accepted — %d %s visible via the ingestion API", res.DocCount, plural(res.DocCount, "document")))
	}

	if cp := res.CCPair; cp != nil {
		log.Info(ccPairLine(cp))
	} else {
		log.Warn(fmt.Sprintf("cc-pair %d not validated (best-effort probe); verify the id in the Admin Panel — a real run with a wrong id would fail silently", settings.CCPairID))
		warnings++
	}

	if warnings == 0 {
		fmt.Fprintf(os.Stderr, "OK: all checks passed in %s\n", elapsed)
	} else {
		fmt.Fprintf(os.Stderr, "OK: all required checks passed in %s (%d %s above)\n", elapsed, warnings, plural(warnings, "warning"))
	}
}

// runLs runs the list for one `sard ls` invocation and returns the
// exit code per docs/PLAN.md §14.4 (see the package doc): build the
// logger from --log-level, resolve configuration with CCPairOptional:
// true (ls needs only the API key — a set cc-pair value is still
// validated and picked up, §14.3.1), run the single GET under a
// signal-aware context, then print the list to stdout and the summary
// (or the empty-list warning) to stderr. A failed list prints no
// summary: a single actionable error line, then the exit code (the
// same shape as the ingest auth fail-fast and the failed check).
func runLs(p *lsFlags) int {
	log, err := buildLogger(p.logLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
	}
	settings, err := config.Resolve(config.Flags{
		APIURL:         p.apiURL,
		APIKey:         p.apiKey,
		CCPairOptional: true,
	})
	if err != nil {
		log.Error(err.Error())
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	start := time.Now()
	client := onyx.NewClient(settings.APIURL, settings.APIKey, 0) // the GET carries no cc-pair
	docs, err := client.ListDocs(ctx)
	if err != nil {
		if ctx.Err() != nil {
			log.Warn("ls interrupted (Ctrl-C)")
			return 130
		}
		log.Error(err.Error())
		if errors.Is(err, onyx.ErrUnreachable) {
			return 1 // unreachable, or a malformed 200 body
		}
		return 2 // ErrAuth or ErrNoIngestionAPI
	}

	// The list itself goes to stdout — one line per document, in the
	// order the server returned it; the link field only when non-empty
	// (docs/PLAN.md §14.3).
	for _, d := range docs {
		if d.Link != "" {
			fmt.Fprintf(os.Stdout, "%s\t%s\t%s\n", d.DocumentID, d.SemanticID, d.Link)
		} else {
			fmt.Fprintf(os.Stdout, "%s\t%s\n", d.DocumentID, d.SemanticID)
		}
	}

	if len(docs) == 0 {
		log.Warn("no documents visible to this API key — nothing has been ingested yet (or the key's scope is empty)")
		return 0
	}
	log.Info(fmt.Sprintf("ls: %d %s in %s", len(docs), plural(len(docs), "document"), time.Since(start)))
	return 0
}

// ccPairLine renders the cc-pair probe's report line
// (docs/PLAN.md §13.5): the id, the connector name and the pair's
// status, and the number of indexed documents (an unknown field
// reports as such rather than silently).
func ccPairLine(cp *onyx.CCPairInfo) string {
	name, status := cp.Name, cp.Status
	if name == "" {
		name = "(unnamed)"
	}
	if status == "" {
		status = "(status unknown)"
	}
	return fmt.Sprintf("cc-pair %d: %s — %s, %d %s indexed", cp.ID, name, status, cp.Docs, plural(cp.Docs, "document"))
}

// summaryHeader renders the header line of the end-of-run summary
// (docs/PLAN.md §8): the verb, the total number of files processed in
// this run (--limit already applied), the whole-run elapsed time (the
// timer starts in runIngest, before discovery — the git clone dominates),
// and the outcome counts (created/updated/skipped/failed for a real run,
// payloads printed plus skipped for a dry run). Each failed file
// additionally gets a follow-up line (ingestAll).
func summaryHeader(verb string, elapsed time.Duration, total int, counts string) string {
	return fmt.Sprintf("%s: %d %s in %s — %s", verb, total, plural(total, "file"), elapsed, counts)
}

// plural renders n as the singular or plural of word ("1 file",
// "2 files", "1 payload", "2 payloads").
func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// fileFailure is one failed file for the summary (file + reason).
type fileFailure struct {
	file   string
	reason string
}

// ingestAll POSTs every file's payload to the Onyx Ingestion API
// sequentially (docs/PLAN.md §7, §8). Every file gets a progress line
// on stderr — "[i/N] <file> → created|updated" (info) or
// "→ failed: <reason>" (warn) — and the run ends with the summary: a
// header with the counts and the whole-run elapsed time, followed by
// one "  failed: <file> — <reason>" line per failed file in ingestion
// (RelPath) order. An Onyx 401/403 aborts with an error line and exit 2
// before any summary (see the package doc).
func ingestAll(ctx context.Context, files []models.IngestedFile, docSource string, settings *config.Settings, idBase string, skipped int, elapsed time.Duration, log *slog.Logger) int {
	client := onyx.NewClient(settings.APIURL, settings.APIKey, settings.CCPairID)
	total := len(files)

	var created, updated int
	var failures []fileFailure
	for i, f := range files {
		if ctx.Err() != nil {
			break
		}
		log.Debug("ingesting", "file", f.RelPath)
		payload := transform.ToOnyxPayload(f, docSource, settings.CCPairID, idBase)
		result, err := client.Ingest(ctx, payload)
		if err != nil {
			if errors.Is(err, onyx.ErrAuth) {
				// Fail fast: every remaining file would fail the same
				// way. A rejected key is a credentials problem → exit 2.
				log.Error("Onyx rejected the API key; aborting the run", "file", f.RelPath, "reason", result.Reason)
				return 2
			}
			// Context cancellation (Ctrl-C): record and stop.
			failures = append(failures, fileFailure{f.RelPath, result.Reason})
			break
		}
		switch result.Status {
		case models.ResultCreated:
			created++
			log.Info(fmt.Sprintf("[%d/%d] %s → created", i+1, total, f.RelPath))
		case models.ResultUpdated:
			updated++
			log.Info(fmt.Sprintf("[%d/%d] %s → updated", i+1, total, f.RelPath))
		default:
			failures = append(failures, fileFailure{f.RelPath, result.Reason})
			log.Warn(fmt.Sprintf("[%d/%d] %s → failed: %s", i+1, total, f.RelPath, result.Reason))
		}
	}

	counts := fmt.Sprintf("created %d, updated %d, skipped %d, failed %d",
		created, updated, skipped, len(failures))
	if ctx.Err() != nil {
		log.Warn(summaryHeader("run interrupted", elapsed, total, counts))
		for _, fl := range failures {
			log.Warn(fmt.Sprintf("  failed: %s — %s", fl.file, fl.reason))
		}
		return 130
	}
	log.Info(summaryHeader("run complete", elapsed, total, counts))
	for _, fl := range failures {
		log.Warn(fmt.Sprintf("  failed: %s — %s", fl.file, fl.reason))
	}
	if len(failures) > 0 {
		return 1
	}
	return 0
}
