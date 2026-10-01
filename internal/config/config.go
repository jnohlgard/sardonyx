// Copyright (C) 2026 Joakim Nohlgård
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY of any kind. See LICENSE for the full text.

// Package config resolves sard's settings: CLI flag > environment variable
// > .env file (current working directory) > default (docs/PLAN.md §5.1).
//
// Resolve is testable without a real process: callers pass already-parsed
// flag values in Flags, and tests control the rest with t.Setenv and
// t.Chdir (a .env file in a temp directory).
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

// DefaultAPIURL is the Onyx API base URL used when ONYX_API_URL is not set
// in any source.
const DefaultAPIURL = "https://cloud.onyx.app/api"

// Environment variable names.
const (
	EnvAPIURL   = "ONYX_API_URL"
	EnvAPIKey   = "ONYX_API_KEY"
	EnvCCPairID = "ONYX_CC_PAIR_ID"
	EnvGitToken = "GIT_TOKEN"
	EnvIDBase   = "SARD_ID_BASE"
)

// Settings holds the resolved configuration. APIKey and GitToken are
// secrets: never log or echo them.
type Settings struct {
	APIURL   string
	APIKey   string
	CCPairID int
	GitToken string
	IDBase   string // optional; empty = the source's default ID base (§6)
}

// Flags carries CLI flag overrides already parsed by the caller (task T7
// uses the stdlib flag package). A zero or empty field means the flag was
// not set.
type Flags struct {
	APIURL   string
	APIKey   string
	CCPairID int
	GitToken string
	IDBase   string
	// DryRun marks a --dry-run invocation: the run sends nothing, so the
	// Onyx credentials (API key, cc-pair id) are not required for it
	// (docs/PLAN.md §4).
	DryRun bool
}

// ErrConfiguration is the sentinel for configuration failures (a required
// value missing or invalid after all sources are merged). Callers map it to
// exit code 2 (docs/PLAN.md §8); see IsConfigurationError.
var ErrConfiguration = errors.New("configuration error")

// Error lists the specific configuration problems found during pre-flight
// validation. It wraps ErrConfiguration so errors.Is(err, ErrConfiguration)
// holds. Its message names the offending variables but never any secret
// value (API key, git token).
type Error struct {
	Problems []string
}

func (e *Error) Error() string {
	if len(e.Problems) == 0 {
		return ErrConfiguration.Error()
	}
	return fmt.Sprintf("configuration error: %s", strings.Join(e.Problems, "; "))
}

// Unwrap reports ErrConfiguration so the whole error class can be matched.
func (e *Error) Unwrap() error { return ErrConfiguration }

// IsConfigurationError reports whether err is (or wraps) a configuration
// error that should abort the run with exit code 2.
func IsConfigurationError(err error) bool {
	return errors.Is(err, ErrConfiguration)
}

// Resolve merges the configuration sources in priority order — explicit CLI
// flag override, environment variable, .env file in the current working
// directory, built-in default — and validates the required values
// pre-flight. An empty environment variable is treated as unset, so a .env
// value still applies. A missing .env file is not an error. A dry run
// (Flags.DryRun) sends nothing, so the Onyx credentials are not required
// for it. It returns an *Error wrapping ErrConfiguration when a required
// value is missing or invalid.
func Resolve(flags Flags) (*Settings, error) {
	dotEnv, err := loadDotEnv(".env")
	if err != nil {
		return nil, err
	}

	apiURL := pick(flags.APIURL, EnvAPIURL, dotEnv)
	if apiURL == "" {
		apiURL = DefaultAPIURL
	}
	apiKey := pick(flags.APIKey, EnvAPIKey, dotEnv)
	gitToken := pick(flags.GitToken, EnvGitToken, dotEnv)
	idBase := pick(flags.IDBase, EnvIDBase, dotEnv)

	ccPairIDStr := ""
	if flags.CCPairID != 0 {
		ccPairIDStr = strconv.Itoa(flags.CCPairID)
	} else {
		ccPairIDStr = pick("", EnvCCPairID, dotEnv)
	}

	// A dry run sends nothing, so the Onyx credentials are not required
	// for it (docs/PLAN.md §4). A cc-pair id that is set but not an
	// integer is an error in either mode.
	var problems []string
	if !flags.DryRun && apiKey == "" {
		problems = append(problems, requiredProblem(EnvAPIKey, "--api-key"))
	}
	if ccPairIDStr == "" {
		if !flags.DryRun {
			problems = append(problems, requiredProblem(EnvCCPairID, "--cc-pair-id"))
		}
	} else if _, err := strconv.Atoi(ccPairIDStr); err != nil {
		problems = append(problems, EnvCCPairID+" must be an integer")
	}
	if len(problems) > 0 {
		return nil, &Error{Problems: problems}
	}

	ccPairID, _ := strconv.Atoi(ccPairIDStr) // validated above
	return &Settings{
		APIURL:   apiURL,
		APIKey:   apiKey,
		CCPairID: ccPairID,
		GitToken: gitToken,
		IDBase:   idBase,
	}, nil
}

// pick returns the first non-empty value among the flag override, the
// environment variable, and the .env file value.
func pick(flag, envVar string, dotEnv map[string]string) string {
	if flag != "" {
		return flag
	}
	if v := os.Getenv(envVar); v != "" {
		return v
	}
	return strings.TrimSpace(dotEnv[envVar])
}

// loadDotEnv reads the .env file via godotenv. A missing file is not an
// error: it simply yields no values.
func loadDotEnv(path string) (map[string]string, error) {
	vals, err := godotenv.Read(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return vals, nil
}

func requiredProblem(envVar, flagName string) string {
	return fmt.Sprintf("%s is required: set the %s flag, the %s environment variable, or .env", envVar, flagName, envVar)
}
