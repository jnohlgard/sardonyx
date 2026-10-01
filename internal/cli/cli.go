// Package cli wires sard's pipeline together: flag parsing, config
// resolution, source discovery, transform, ingestion, and the final
// summary (docs/PLAN.md §3, §4; task T7).
//
// Run is the single entry point used by cmd/sard. It parses the
// "sard ingest <source> [options]" invocation with the stdlib flag
// package (an append-value helper makes --include/--exclude repeatable),
// builds the run's log/slog logger from --log-level, resolves
// configuration (config.Resolve), classifies the input (an existing local
// directory → source.Local, anything else → source.Git), and runs
// discovery → transform → ingest sequentially under
// signal.NotifyContext so Ctrl-C aborts promptly.
//
// Logging goes entirely through that slog logger to stderr — stdout is
// reserved for --dry-run output — so cmd/sard stays a one-line wrapper
// around Run and os.Exit.
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
//     form), or an Onyx 401/403. The auth case is deliberate: the client
//     fails fast with onyx.ErrAuth because every remaining file would
//     fail identically, and a rejected key is a credentials problem, so
//     the run aborts with 2 rather than 1.
//   - 130 — the run was interrupted (SIGINT via signal.NotifyContext).
//
// --dry-run never exits 1 (it sends nothing, so no file can fail); a
// discovery error in dry-run still exits 2. --limit caps the number of
// files ingested — and the number of payloads printed in a dry-run —
// without changing the summary wording.
//
// --branch and --token are git-only flags: for a local directory input
// they are ignored with a warning (their values are never logged).
//
// TODO (T8): polish the summary (elapsed time, cleaner failure list) and
// add a progress line per file.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"

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
	include     stringList
	exclude     stringList
	maxDepth    int
	maxFileSize int
	token       string
	dryRun      bool
	limit       int
	logLevel    string
}

// newFlagSet defines the `sard ingest` flags of docs/PLAN.md §4 on f
// and binds them to p. It returns the names of the boolean flags —
// reorderFlags uses them to decide which flags consume a value token.
func newFlagSet(f *flag.FlagSet, p *ingestFlags) (boolNames []string) {
	f.StringVar(&p.apiURL, "api-url", "", "Onyx API base URL (default: "+config.DefaultAPIURL+"; env: "+config.EnvAPIURL+")")
	f.StringVar(&p.apiKey, "api-key", "", "Onyx API key (env: "+config.EnvAPIKey+")")
	f.IntVar(&p.ccPairID, "cc-pair-id", 0, "Onyx connector-credential-pair id (env: "+config.EnvCCPairID+")")
	f.StringVar(&p.branch, "branch", "", "branch to clone; git sources only")
	f.StringVar(&p.source, "source", "", "override the document source enum: "+strings.Join(validSources, " | "))
	f.StringVar(&p.idBase, "id-base", "", "arbitrary document-ID base for this run (env: "+config.EnvIDBase+")")
	f.Var(&p.include, "include", "include filter, repeatable (e.g. \"docs/**\")")
	f.Var(&p.exclude, "exclude", "exclude filter, repeatable (on top of the default noise-dir exclusions)")
	f.IntVar(&p.maxDepth, "max-depth", 0, "max directory depth below the source root (0 = unlimited)")
	f.IntVar(&p.maxFileSize, "max-file-size", 1024, "skip files larger than N KiB (0 = unlimited)")
	f.StringVar(&p.token, "token", "", "git auth token for private repos (env: "+config.EnvGitToken+")")
	f.BoolVar(&p.dryRun, "dry-run", false, "print the would-be payloads to stdout; send nothing")
	boolNames = append(boolNames, "dry-run")
	f.IntVar(&p.limit, "limit", 0, "ingest at most N files (0 = unlimited)")
	f.StringVar(&p.logLevel, "log-level", "info", "log level: debug | info | warning | error")
	return boolNames
}

// stringList is a flag.Value that appends every occurrence of a flag to
// a slice — how the repeatable --include / --exclude flags work
// (docs/PLAN.md §4).
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ", ") }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
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
		for _, v := range validSources {
			if v == p.source {
				return nil
			}
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
// RelPath), the run's default document-ID base, and the default document
// source for the input.
type discovery struct {
	files         []models.IngestedFile
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
		idBase:        res.IDBase,
		defaultSource: norm.Source,
	}, nil
}

// reorderFlags moves the positional <source> argument(s) to the end of
// the argument list. The stdlib flag package stops parsing at the first
// non-flag token, but the §4 form is "sard ingest <source> [options]" —
// positional first — so the args are re-sorted into flags (with their
// values) followed by positionals before parsing. Both
// "sard ingest ./docs --dry-run" and "sard ingest --dry-run ./docs"
// work. A non-bool flag without an inline "=" consumes the next token
// as its value even when it starts with "-" (e.g. --limit -1); a
// "--" terminator ends flag parsing, as in the stdlib.
func reorderFlags(f *flag.FlagSet, boolNames []string, rest []string) []string {
	boolSet := make(map[string]bool, len(boolNames))
	for _, n := range boolNames {
		boolSet[n] = true
	}
	var flags, positionals []string
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		if a == "--" {
			positionals = append(positionals, rest[i:]...)
			return append(flags, positionals...)
		}
		if len(a) >= 2 && a[0] == '-' {
			flags = append(flags, a)
			if eq := strings.IndexByte(a, '='); eq < 2 {
				name := strings.TrimLeft(a, "-")
				if fl := f.Lookup(name); fl != nil && !boolSet[name] {
					i++
					if i < len(rest) {
						flags = append(flags, rest[i])
					}
				}
			}
			continue
		}
		positionals = append(positionals, a)
	}
	return append(flags, positionals...)
}

// printTopLevelUsage prints the top-level usage shown for a wrong
// subcommand or a missing <source> argument (docs/PLAN.md §4).
func printTopLevelUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: sard ingest <source> [options]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  <source>   a git repository URL (https://host/owner/repo,")
	fmt.Fprintln(w, "             git@host:owner/repo, or the owner/repo shorthand")
	fmt.Fprintln(w, "             for github.com) or a local directory path")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "run 'sard ingest --help' for the full option list")
}

// Run executes one `sard ingest` invocation and returns the exit code
// per docs/PLAN.md §8 (see the package doc). It owns everything between
// the command line and the process exit: flag parsing, the slog logger
// (from --log-level, writing to stderr), the signal context for Ctrl-C,
// and the pipeline. cmd/sard only maps the returned code to os.Exit.
func Run(args []string) int {
	if len(args) == 0 || args[0] != "ingest" {
		printTopLevelUsage(os.Stderr)
		return 2
	}

	var p ingestFlags
	f := flag.NewFlagSet("sard ingest", flag.ContinueOnError)
	boolNames := newFlagSet(f, &p)
	msgs := &bytes.Buffer{} // the flag package writes parse diagnostics and usage here
	f.SetOutput(msgs)
	if err := f.Parse(reorderFlags(f, boolNames, args[1:])); err != nil {
		fmt.Fprint(os.Stderr, msgs.String()) // the flag package's own diagnostics + usage
		if errors.Is(err, flag.ErrHelp) {
			return 0 // --help: usage printed, not an error
		}
		return 2
	}

	if f.NArg() != 1 {
		fmt.Fprintf(os.Stderr, "error: exactly one <source> argument is required (got %d)\n", f.NArg())
		f.Usage()
		fmt.Fprint(os.Stderr, msgs.String())
		return 2
	}
	src := f.Arg(0)

	log, err := buildLogger(p.logLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
	}
	if err := validate(&p); err != nil {
		log.Error(err.Error())
		return 2
	}
	settings, err := config.Resolve(config.Flags{
		APIURL:   p.apiURL,
		APIKey:   p.apiKey,
		CCPairID: p.ccPairID,
		GitToken: p.token,
		IDBase:   p.idBase,
	})
	if err != nil {
		log.Error(err.Error())
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	d, err := discover(src, &p, settings, log)
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
		log.Warn("no markdown files found", "source", src)
		return 0
	}

	if p.dryRun {
		return runDry(ctx, files, docSource, settings.CCPairID, idBase, log)
	}
	return ingestAll(ctx, files, docSource, settings, idBase, log)
}

// runDry prints the would-be payload of every file to stdout — one
// compact JSON document per line, in the source's stable RelPath order —
// and sends nothing.
func runDry(ctx context.Context, files []models.IngestedFile, source string, ccPairID int, idBase string, log *slog.Logger) int {
	for _, f := range files {
		if ctx.Err() != nil {
			break
		}
		payload := transform.ToOnyxPayload(f, source, ccPairID, idBase)
		b, err := json.Marshal(payload)
		if err != nil {
			log.Error("marshaling payload", "file", f.RelPath, "error", err)
			return 1
		}
		fmt.Fprintln(os.Stdout, string(b))
	}
	if ctx.Err() != nil {
		log.Warn("run interrupted", "files", len(files))
		return 130
	}
	log.Info("dry run complete", "files", len(files))
	return 0
}

// fileFailure is one failed file for the summary (file + reason).
type fileFailure struct {
	file   string
	reason string
}

// ingestAll POSTs every file's payload to the Onyx Ingestion API
// sequentially (docs/PLAN.md §7, §8) and reports the final counts:
// per-file results are logged during the run, and the summary at the end
// carries the counts plus the failed files with their reasons. T8 will
// polish the formatting.
func ingestAll(ctx context.Context, files []models.IngestedFile, docSource string, settings *config.Settings, idBase string, log *slog.Logger) int {
	client := onyx.NewClient(settings.APIURL, settings.APIKey, settings.CCPairID)

	var created, updated int
	var failures []fileFailure
	for _, f := range files {
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
			log.Info("created", "file", f.RelPath)
		case models.ResultUpdated:
			updated++
			log.Info("updated", "file", f.RelPath)
		default:
			failures = append(failures, fileFailure{f.RelPath, result.Reason})
			log.Warn("ingest failed", "file", f.RelPath, "reason", result.Reason)
		}
	}

	if ctx.Err() != nil {
		log.Warn("run interrupted", "total", len(files), "created", created, "updated", updated, "failed", len(failures))
		return 130
	}

	log.Info("run complete", "total", len(files), "created", created, "updated", updated, "failed", len(failures))
	for _, fl := range failures {
		log.Warn("failed", "file", fl.file, "reason", fl.reason)
	}
	if len(failures) > 0 {
		return 1
	}
	return 0
}
