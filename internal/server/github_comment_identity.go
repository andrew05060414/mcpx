package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const githubCommentLogin = "NakhimovBot"

// Private runner: token stdout and child environments must never reach CLI metadata.
var githubPrivateCLI = runGitHubPrivateCLI

func githubCLIEnv(token string) []string {
	out := make([]string, 0, len(os.Environ())+1)
	for _, item := range os.Environ() {
		name, _, _ := strings.Cut(item, "=")
		switch strings.ToUpper(name) {
		case "GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GH_HOST", "GH_DEBUG":
			continue
		}
		out = append(out, item)
	}
	out = append(out, "GH_HOST=github.com", "GH_PROMPT_DISABLED=1")
	if token != "" {
		out = append(out, "GH_TOKEN="+token)
	}
	return out
}

func resolveGitHubCLI() (string, error) {
	if configured := os.Getenv("MCPX_GH_BIN"); configured != "" {
		bin, err := controlLookPath(configured)
		if err != nil {
			return "", errors.New("configured MCPX_GH_BIN is unavailable")
		}
		return bin, nil
	}
	if bin, err := controlLookPath("gh"); err == nil {
		return bin, nil
	}
	// Scoop's configured root may differ from USERPROFILE and a service's stale PATH.
	if root := os.Getenv("SCOOP"); filepath.IsAbs(root) {
		if bin, err := controlLookPath(filepath.Join(root, "shims", "gh.exe")); err == nil {
			return bin, nil
		}
	}
	return "", errors.New("gh CLI unavailable; set MCPX_GH_BIN or repair service PATH")
}

func runGitHubPrivateCLI(ctx context.Context, bin string, argv, env []string, input string) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, controlCLITimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, bin, argv...)
	cmd.Env = env
	cmd.Stdin = strings.NewReader(input)
	// Do not collect stderr, include argv in errors, or expose credential stdout.
	stdout, err := cmd.Output()
	if err != nil {
		return "", errors.New("GitHub CLI operation failed")
	}
	return string(stdout), nil
}

func runBotGitHubComment(ctx context.Context, kind, repo string, number int, body string) (controlCLIResult, error) {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || number <= 0 {
		return controlCLIResult{}, errors.New("invalid GitHub comment target")
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "?#\\ \t\r\n") {
			return controlCLIResult{}, errors.New("invalid GitHub repository")
		}
	}
	bin, err := resolveGitHubCLI()
	if err != nil {
		return controlCLIResult{}, err
	}
	token, err := githubPrivateCLI(ctx, bin, []string{"auth", "token", "--hostname", "github.com", "--user", githubCommentLogin}, githubCLIEnv(""), "")
	token = strings.TrimSpace(token)
	if err != nil || token == "" {
		return controlCLIResult{}, errors.New("NakhimovBot keyring credential unavailable")
	}
	// Only a child-local environment contains the credential. os.Setenv is never used.
	env := githubCLIEnv(token)
	defer func() {
		for i := range env {
			env[i] = ""
		}
		token = ""
	}()
	login, err := githubPrivateCLI(ctx, bin, []string{"api", "--hostname", "github.com", "user", "--jq", ".login"}, env, "")
	if err != nil || strings.TrimSpace(login) != githubCommentLogin {
		return controlCLIResult{}, errors.New("GitHub identity preflight failed; expected NakhimovBot")
	}
	input, _ := json.Marshal(map[string]string{"body": body})
	endpoint := fmt.Sprintf("repos/%s/issues/%d/comments", repo, number)
	argv := []string{"api", "--hostname", "github.com", "--method", "POST", endpoint, "--input", "-"}
	raw, err := githubPrivateCLI(ctx, bin, argv, env, string(input))
	if err != nil {
		return controlCLIResult{}, errors.New("GitHub comment request failed; check GitHub before retrying")
	}
	var comment struct {
		URL  string `json:"html_url"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if json.Unmarshal([]byte(raw), &comment) != nil || comment.User.Login != githubCommentLogin || comment.URL == "" {
		return controlCLIResult{}, errors.New("GitHub comment author verification failed; stop and inspect GitHub")
	}
	return controlCLIResult{Args: append([]string{bin}, argv...), Stdout: comment.URL}, nil
}
