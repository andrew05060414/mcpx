package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBotCommentFailClosedAndEnvironmentIsolation(t *testing.T) {
	for _, tc := range []struct {
		name, login           string
		tokenError, postError bool
		wantCalls             int
		wantOK                bool
	}{
		{"success", "NakhimovBot", false, false, 3, true},
		{"wrong_identity", "andrew05060414", false, false, 2, false},
		{"wrong_case", "nakhimovbot", false, false, 2, false},
		{"missing_keyring", "", true, false, 1, false},
		{"post_failure_no_retry", "NakhimovBot", false, true, 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restoreControlCLI(t)
			t.Setenv("GH_TOKEN", "caller-credential")
			t.Setenv("GITHUB_TOKEN", "other-caller-credential")
			t.Setenv("GH_HOST", "other.example")
			t.Setenv("GH_DEBUG", "api")
			controlLookPath = func(string) (string, error) { return "/fake/gh", nil }
			orig := githubPrivateCLI
			t.Cleanup(func() { githubPrivateCLI = orig })
			calls := 0
			var lastEnv []string
			githubPrivateCLI = func(_ context.Context, _ string, argv, env []string, input string) (string, error) {
				calls++
				for _, e := range env {
					if strings.Contains(e, "caller-credential") || strings.HasPrefix(e, "GH_DEBUG=") {
						t.Fatal("ambient credential/debug inherited")
					}
				}
				if strings.Contains(strings.Join(argv, " "), "fake-bot") {
					t.Fatal("credential in argv")
				}
				if calls == 1 {
					if strings.Join(argv, " ") != "auth token --hostname github.com --user NakhimovBot" {
						t.Fatal("account not explicit")
					}
					if tc.tokenError {
						return "", errors.New("secret diagnostic")
					}
					return "fake-bot", nil
				}
				if !strings.Contains(strings.Join(env, "\n"), "GH_TOKEN=fake-bot") {
					t.Fatal("missing bot environment")
				}
				lastEnv = env
				if calls == 2 {
					return tc.login, nil
				}
				if !strings.Contains(input, "test body") {
					t.Fatal("body missing from stdin")
				}
				if tc.postError {
					return "", errors.New("secret diagnostic")
				}
				return `{"html_url":"https://github.com/owner/repo/issues/1#issuecomment-1","user":{"login":"NakhimovBot"}}`, nil
			}
			rt := &Runtime{}
			response := callEnvelope(t, rt.toolGitHubComment, context.Background(), map[string]any{"repo": "owner/repo", "number": 1, "body": "test body", "target": "auto", "user_confirmed": true})
			if statusOK(response) != tc.wantOK || calls != tc.wantCalls {
				t.Fatalf("ok=%v calls=%d", statusOK(response), calls)
			}
			if os.Getenv("GH_TOKEN") != "caller-credential" {
				t.Fatal("parent environment mutated")
			}
			for _, e := range lastEnv {
				if e != "" {
					t.Fatal("temporary child environment not cleared")
				}
			}
		})
	}
}

func TestGitHubCLIResolvesConfiguredScoopRoot(t *testing.T) {
	restoreControlCLI(t)
	t.Setenv("MCPX_GH_BIN", "")
	root := t.TempDir()
	t.Setenv("SCOOP", root)
	expected := filepath.Join(root, "shims", "gh.exe")
	controlLookPath = func(p string) (string, error) {
		if p == expected {
			return p, nil
		}
		return "", errors.New("missing")
	}
	got, err := resolveGitHubCLI()
	if err != nil || got != expected {
		t.Fatalf("got %q error %v", got, err)
	}
	t.Setenv("MCPX_GH_BIN", "missing-explicit")
	if _, err := resolveGitHubCLI(); err == nil {
		t.Fatal("invalid explicit override fell back")
	}
}
