package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Distinct sentinel values so a leaked secret is easy to spot in test output.
const (
	apiKeyValue   = "SUPER-SECRET-API-KEY"
	gitTokenValue = "SUPER-SECRET-GIT-TOKEN"
)

// clearConfigEnv empties all recognized environment variables. Empty values
// are treated as unset by Resolve, so this makes tests hermetic.
func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{EnvAPIURL, EnvAPIKey, EnvCCPairID, EnvGitToken} {
		t.Setenv(k, "")
	}
}

func writeDotEnv(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(content), 0o600); err != nil {
		t.Fatalf("writing .env: %v", err)
	}
}

func TestResolvePrecedence(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeDotEnv(t, dir, strings.Join([]string{
		EnvAPIURL + "=https://from-dotenv.example/api",
		EnvAPIKey + "=dotenv-key",
		EnvCCPairID + "=7",
		EnvGitToken + "=dotenv-token",
	}, "\n")+"\n")

	env := map[string]string{
		EnvAPIURL:   "https://from-env.example/api",
		EnvAPIKey:   "env-key",
		EnvCCPairID: "8",
		EnvGitToken: "env-token",
	}

	cases := []struct {
		name  string
		flags Flags
		env   map[string]string
		want  Settings
	}{
		{
			name: ".env values when nothing else is set",
			want: Settings{
				APIURL:   "https://from-dotenv.example/api",
				APIKey:   "dotenv-key",
				CCPairID: 7,
				GitToken: "dotenv-token",
			},
		},
		{
			name: "env beats .env",
			env:  env,
			want: Settings{
				APIURL:   "https://from-env.example/api",
				APIKey:   "env-key",
				CCPairID: 8,
				GitToken: "env-token",
			},
		},
		{
			name:  "flag beats env beats .env",
			flags: Flags{APIURL: "https://from-flag.example/api", APIKey: "flag-key", CCPairID: 9, GitToken: "flag-token"},
			env:   env,
			want: Settings{
				APIURL:   "https://from-flag.example/api",
				APIKey:   "flag-key",
				CCPairID: 9,
				GitToken: "flag-token",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			got, err := Resolve(tc.flags)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if *got != tc.want {
				t.Fatalf("Resolve = %+v, want %+v", *got, tc.want)
			}
		})
	}
}

func TestResolveDefaultAPIURL(t *testing.T) {
	t.Chdir(t.TempDir()) // no .env file
	clearConfigEnv(t)
	t.Setenv(EnvAPIKey, "some-key")
	t.Setenv(EnvCCPairID, "42")

	got, err := Resolve(Flags{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.APIURL != DefaultAPIURL {
		t.Fatalf("APIURL = %q, want default %q", got.APIURL, DefaultAPIURL)
	}
}

func TestMissingDotEnvIsNotAnError(t *testing.T) {
	t.Chdir(t.TempDir()) // deliberately no .env file
	clearConfigEnv(t)
	t.Setenv(EnvAPIKey, "some-key")
	t.Setenv(EnvCCPairID, "42")

	if _, err := Resolve(Flags{}); err != nil {
		t.Fatalf("Resolve without a .env file: %v", err)
	}
}

func TestRequiredValues(t *testing.T) {
	run := func(t *testing.T, env map[string]string, flags Flags) error {
		t.Helper()
		t.Chdir(t.TempDir())
		clearConfigEnv(t)
		for k, v := range env {
			t.Setenv(k, v)
		}
		_, err := Resolve(flags)
		return err
	}

	t.Run("missing API key names it", func(t *testing.T) {
		err := run(t, map[string]string{EnvCCPairID: "42"}, Flags{})
		var cerr *Error
		if !errors.As(err, &cerr) {
			t.Fatalf("err = %v, want *config.Error", err)
		}
		if !IsConfigurationError(err) {
			t.Errorf("IsConfigurationError(%v) = false, want true", err)
		}
		if !strings.Contains(err.Error(), EnvAPIKey) {
			t.Errorf("error %q does not name %s", err, EnvAPIKey)
		}
		if strings.Contains(err.Error(), EnvCCPairID) {
			t.Errorf("error %q unexpectedly names %s", err, EnvCCPairID)
		}
	})

	t.Run("missing cc pair id names it", func(t *testing.T) {
		err := run(t, map[string]string{EnvAPIKey: "some-key"}, Flags{})
		var cerr *Error
		if !errors.As(err, &cerr) {
			t.Fatalf("err = %v, want *config.Error", err)
		}
		if !strings.Contains(err.Error(), EnvCCPairID) {
			t.Errorf("error %q does not name %s", err, EnvCCPairID)
		}
		if strings.Contains(err.Error(), EnvAPIKey) {
			t.Errorf("error %q unexpectedly names %s", err, EnvAPIKey)
		}
	})

	t.Run("both missing names both", func(t *testing.T) {
		err := run(t, nil, Flags{})
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
		if !strings.Contains(err.Error(), EnvAPIKey) || !strings.Contains(err.Error(), EnvCCPairID) {
			t.Errorf("error %q does not name both required variables", err)
		}
	})

	t.Run("non-integer cc pair id", func(t *testing.T) {
		err := run(t, map[string]string{EnvAPIKey: "some-key", EnvCCPairID: "notanint"}, Flags{})
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
		if !strings.Contains(err.Error(), EnvCCPairID) || !strings.Contains(err.Error(), "integer") {
			t.Errorf("error %q does not describe the invalid %s", err, EnvCCPairID)
		}
	})

	t.Run("flag provides cc pair id, none in env", func(t *testing.T) {
		if err := run(t, map[string]string{EnvAPIKey: "some-key"}, Flags{CCPairID: 3}); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
	})

	t.Run("git token absent is not an error", func(t *testing.T) {
		if err := run(t, map[string]string{EnvAPIKey: "some-key", EnvCCPairID: "42"}, Flags{}); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
	})
}

func TestSecretsNeverInErrors(t *testing.T) {
	checkNoLeaks := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
		for _, secret := range []string{apiKeyValue, gitTokenValue} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error message leaks a secret: %v", err)
			}
		}
	}

	t.Run("key and token in .env, cc pair id missing", func(t *testing.T) {
		dir := t.TempDir()
		t.Chdir(dir)
		writeDotEnv(t, dir, EnvAPIKey+"="+apiKeyValue+"\n"+EnvGitToken+"="+gitTokenValue+"\n")
		clearConfigEnv(t)
		_, err := Resolve(Flags{})
		checkNoLeaks(t, err)
	})

	t.Run("key and token in env, cc pair id not an integer", func(t *testing.T) {
		t.Chdir(t.TempDir())
		clearConfigEnv(t)
		t.Setenv(EnvAPIKey, apiKeyValue)
		t.Setenv(EnvGitToken, gitTokenValue)
		t.Setenv(EnvCCPairID, "abc")
		_, err := Resolve(Flags{})
		checkNoLeaks(t, err)
	})
}
