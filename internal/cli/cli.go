// Copyright (C) 2026 Joakim Nohlgård
// SPDX-License-Identifier: AGPL-3.0

// Package cli wires sard's pipeline together: command-line parsing,
// config resolution, source discovery, transform, ingestion, and the
// final summary (docs/PLAN.md §3, §4; task T7).
//
// Run is the single entry point used by cmd/sard. It builds the cobra
// command tree — the root `sard` command and the `ingest` subcommand,
// "sard ingest <source> [options]" — and executes it. The flag
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
// aborts promptly.
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

// rootUsageTemplate is the root command's usage template: the v1.10
// default minus the Runnable line (root itself takes no arguments or
// flags — it only dispatches to subcommands, so a "sard [flags]" line
// would be misleading).
const rootUsageTemplate = `Usage:{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]{{end}}{{if gt (len .Aliases) 0}}

Aliases:
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

Examples:
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}{{$cmds := .Commands}}{{if eq (len .Groups) 0}}

Available Commands:{{range $cmds}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{else}}{{range $group := .Groups}}

{{.Title}}{{range $cmds}}{{if (and (eq .GroupID $group.ID) (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{if not .AllChildCommandsHaveGroup}}

Additional Commands:{{range $cmds}}{{if (and (eq .GroupID "") (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

Flags:
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableInheritedFlags}}

Global Flags:
{{.InheritedFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasHelpSubCommands}}

Additional help topics:{{range .Commands}}{{if .IsAdditionalHelpTopicCommand}}
  {{rpad .CommandPath .CommandPathPadding}} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableSubCommands}}

Use "{{.CommandPath}} [command] --help" for more information about a command.{{end}}
`

// ingestUsageTemplate is cobra v1.10.2's defaultUsageTemplate plus two
// hand-written sections below the Usage line: an Arguments section
// (the command's Long text is a summary, and the <source> description
// reads better next to the usage line it documents than at the top of
// the help output) and a static Environment section summarizing the
// env vars behind the flags (the names must stay in sync with
// internal/config; TestIngestUsageTemplateEnvNames guards that). It is
// set explicitly on the ingest command because a command inherits its
// parent's usage template, and the root uses rootUsageTemplate —
// without this, the root's trimmed template (no UseLine) would render
// an empty Usage line for "sard ingest --help". Keep the default part
// in sync with the cobra version pinned in go.mod.
const ingestUsageTemplate = `Usage:{{if .Runnable}}
  {{.UseLine}}

Arguments:
  <source>  A git repository URL (https://host/owner/repo,
            git@host:owner/repo, or the owner/repo shorthand for
            github.com) or a local directory path.

Environment:
  ONYX_API_URL     Onyx API base URL (--api-url)
  ONYX_API_KEY     Onyx API key (--api-key)
  ONYX_CC_PAIR_ID  connector-credential pair id (--cc-pair-id)
  GIT_TOKEN        token for private repos (--token)
  SARD_ID_BASE     document-ID base for the run (--id-base){{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]{{end}}{{if gt (len .Aliases) 0}}

Aliases:
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

Examples:
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}{{$cmds := .Commands}}{{if eq (len .Groups) 0}}

Available Commands:{{range $cmds}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{else}}{{range $group := .Groups}}

{{.Title}}{{range $cmds}}{{if (and (eq .GroupID $group.ID) (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{if not .AllChildCommandsHaveGroup}}

Additional Commands:{{range $cmds}}{{if (and (eq .GroupID "") (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

Flags:
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableInheritedFlags}}

Global Flags:
{{.InheritedFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasHelpSubCommands}}

Additional help topics:{{range .Commands}}{{if .IsAdditionalHelpTopicCommand}}
  {{rpad .CommandPath .CommandPathPadding}} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableSubCommands}}

Use "{{.CommandPath}} [command] --help" for more information about a command.{{end}}
`

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
documents instead of duplicating them.`,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE:          func(cmd *cobra.Command, args []string) error { return errNoSubcommand },
	}
	root.SetUsageTemplate(rootUsageTemplate)
	return root
}

// newIngestCmd builds the `sard ingest` subcommand (docs/PLAN.md §4):
// the flags bound to p and a RunE wired to runIngest. The flag
// definitions are the single source of truth for the flag list in
// --help — cobra renders the usage line and the option list from the
// same definitions that bind the parsed values. The Long text is a
// summary; the <source> description lives in the Arguments section of
// ingestUsageTemplate, below the usage line.
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
	cmd.SetUsageTemplate(ingestUsageTemplate)
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
// owner/repo shorthand. A non-existent path-like input is a bad path
// (exit 2); a bare two-segment string is the owner/repo shorthand and
// fails, if at all, at clone time (exit 1).
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
	if fi, err := os.Stat(src); err == nil && fi.IsDir() {
		if p.branch != "" {
			log.Warn("--branch applies to git sources only; ignoring it for a local directory")
		}
		if p.token != "" {
			log.Warn("--token applies to git sources only; ignoring it for a local directory")
		}
		res, err := source.Local(src, source.LocalOptions{
			Include:        p.include,
			Exclude:        p.exclude,
			MaxDepth:       p.maxDepth,
			MaxFileSizeKiB: p.maxFileSize,
		}, log)
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

	// Not a directory: a git repository input. A non-existent explicit
	// path is a bad path (exit 2), not a repository shorthand.
	if _, err := os.Stat(src); err != nil && pathLike(src) {
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
		Include:        p.include,
		Exclude:        p.exclude,
		MaxDepth:       p.maxDepth,
		MaxFileSizeKiB: p.maxFileSize,
		Branch:         p.branch,
		Token:          settings.GitToken,
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
// tree (root `sard` + the `ingest` subcommand), executes it, and maps
// the outcome: --help (and the built-in help command) printed usage and
// exit 0; the pipeline's own exit code (0/1/2/130, all diagnostics
// logged by the pipeline itself); a missing or unknown subcommand, and
// any flag/argument error on `sard ingest`, exit 2 with the relevant
// usage on stderr (the auto-generated top-level usage for root-level
// problems, the ingest usage for ingest-level ones). cmd/sard only maps
// the returned code to os.Exit.
func Run(args []string) int {
	root := newRootCmd()
	root.AddCommand(newIngestCmd(&ingestFlags{}))
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
		// A flag or argument error on `sard ingest`: the standard
		// diagnostic plus the full ingest usage, exit 2.
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
