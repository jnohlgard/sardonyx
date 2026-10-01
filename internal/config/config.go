// Package config resolves sard's settings: CLI flag > environment variable
// > .env file > default (docs/PLAN.md §5.1).
//
// TODO (T1): Settings struct and resolution logic — env vars ONYX_API_URL,
// ONYX_API_KEY, ONYX_CC_PAIR_ID, GIT_TOKEN; .env loaded via godotenv (a
// missing file is not an error); pre-flight validation that fails with exit
// code 2 when a required value is missing.
package config
