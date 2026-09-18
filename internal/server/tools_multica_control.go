package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/envelope"
	"mcpx/internal/mcpresult"
)

const controlCLITimeout = 20 * time.Second

var (
	controlLookPath = exec.LookPath
	controlRunCLI   = runControlCLI
	githubIssueRef  = regexp.MustCompile(`(?i)(?:github\.com/([^/\s]+)/([^/\s#]+)/(?:issues|pull)/(\d+)|\[GH\s+[^#]*#(\d+)\]|#(\d+))`)
)

type controlCLIResult struct {
	Args     []string
	Stdout   string
	Stderr   string
	ExitCode int
}

func (r *Runtime) registerMulticaControlTools(s *mcp.Server) {
	executionID := stringSchema("Multica execution/issue id or identifier, e.g. TEC-42")
	confirm := booleanSchema("Required true for write tools after the user confirms the action. Tokens never appear in the model-visible result.")

	r.addToolIfAbsent(s, cleanCoreTool("multica_list_executions",
		"List Multica executions/issues via the local `multica` CLI (`issue list --output json`). Filter by project, status, or assignee. Read-only.",
		map[string]any{
			"project":     stringSchema("Multica project id; defaults to MCPX_MULTICA_PROJECT_ID when set"),
			"status":      stringSchema("Optional status filter: todo, in_progress, in_review, blocked, done"),
			"assignee_id": stringSchema("Optional agent id filter"),
			"assignee":    stringSchema("Optional agent name filter"),
			"limit":       numberSchema("Optional list limit, default 100"),
		},
		nil, arcControlReadAnnotation), r.toolMulticaListExecutions)

	r.addToolIfAbsent(s, cleanCoreTool("multica_get_execution",
		"Get one Multica execution via `multica issue get|view|show --output json`, including metadata, GitHub linkage, and latest activity when the CLI returns it.",
		map[string]any{"execution_id": executionID},
		[]string{"execution_id"}, arcControlReadAnnotation), r.toolMulticaGetExecution)

	r.addToolIfAbsent(s, cleanCoreTool("multica_list_agents",
		"List Multica agents/workers via `multica agent list --output json`, including runtime/harness identity fields the CLI exposes.",
		map[string]any{
			"query": stringSchema("Optional name substring filter"),
		},
		nil, arcControlReadAnnotation), r.toolMulticaListAgents)

	r.addToolIfAbsent(s, cleanCoreTool("multica_assign_execution",
		"Assign or reassign a Multica execution to an agent. Write-gated: requires user_confirmed=true. Wraps `multica issue update|assign`.",
		map[string]any{
			"execution_id":   executionID,
			"assignee_id":    stringSchema("Target agent id"),
			"assignee":       stringSchema("Target agent name if id is unknown"),
			"user_confirmed": confirm,
		},
		[]string{"execution_id"}, arcControlWriteAnnotation), r.toolMulticaAssignExecution)

	r.addToolIfAbsent(s, cleanCoreTool("multica_retry_execution",
		"Retry or resume a Multica execution. Write-gated: requires user_confirmed=true. Tries `issue retry|rerun` then `run retry`.",
		map[string]any{
			"execution_id":   executionID,
			"user_confirmed": confirm,
		},
		[]string{"execution_id"}, arcControlWriteAnnotation), r.toolMulticaRetryExecution)

	r.addToolIfAbsent(s, cleanCoreTool("multica_request_review",
		"Request independent review on a Multica execution (typically status in_review). Write-gated: requires user_confirmed=true.",
		map[string]any{
			"execution_id":   executionID,
			"reviewer_id":    stringSchema("Optional reviewer agent id"),
			"reviewer":       stringSchema("Optional reviewer agent name"),
			"user_confirmed": confirm,
		},
		[]string{"execution_id"}, arcControlWriteAnnotation), r.toolMulticaRequestReview)

	r.addToolIfAbsent(s, cleanCoreTool("multica_update_status",
		"Transition a Multica execution status where the CLI allows it. Write-gated: requires user_confirmed=true. Destructive production closes remain separately gated by confirmation.",
		map[string]any{
			"execution_id":   executionID,
			"status":         enumSchema("Target Multica status", "todo", "in_progress", "in_review", "blocked", "done"),
			"user_confirmed": confirm,
		},
		[]string{"execution_id", "status"}, arcControlWriteAnnotation), r.toolMulticaUpdateStatus)

	r.addToolIfAbsent(s, cleanCoreTool("github_comment",
		"Comment on a linked GitHub Issue or PR using the already-logged-in `gh` CLI. Write-gated: requires user_confirmed=true. Pass repo+number, or execution_id to resolve github_repo / github_issue_number metadata. Tokens are never returned.",
		map[string]any{
			"execution_id":   stringSchema("Optional Multica execution used to resolve linked GitHub Issue/PR"),
			"repo":           stringSchema("owner/name repository; required unless execution metadata supplies github_repo"),
			"number":         numberSchema("Issue or PR number"),
			"target":         enumSchema("Comment target; auto uses a PR when target=pr or gh pr view succeeds", "issue", "pr", "auto"),
			"body":           stringSchema("Comment markdown"),
			"user_confirmed": confirm,
		},
		[]string{"body"}, arcControlWriteAnnotation), r.toolGitHubComment)
}

func (r *Runtime) toolMulticaListExecutions(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := mcpresult.Arguments(req)
	argv := []string{"issue", "list", "--output", "json"}
	if project := firstNonEmpty(stringArg(args, "project"), strings.TrimSpace(os.Getenv("MCPX_MULTICA_PROJECT_ID"))); project != "" {
		argv = append(argv, "--project", project)
	}
	if status := stringArg(args, "status"); status != "" {
		argv = append(argv, "--status", status)
	}
	if assigneeID := stringArg(args, "assignee_id"); assigneeID != "" {
		argv = append(argv, "--assignee-id", assigneeID)
	}
	if assignee := stringArg(args, "assignee"); assignee != "" {
		argv = append(argv, "--assignee", assignee)
	}
	argv = append(argv, "--limit", fmt.Sprintf("%d", intArg(args, "limit", 100)))
	payload, cli, err := runMulticaJSON(ctx, [][]string{argv})
	if err != nil {
		return r.controlError(ctx, req, "MULTICA_CLI_FAILED", err)
	}
	executions := extractExecutions(payload)
	data := map[string]any{
		"executions": executions,
		"count":      len(executions),
		"cli":        safeCLIMeta(cli),
	}
	return compactToolResult(data, fmt.Sprintf("Listed %d Multica executions.", len(executions))), nil
}

func (r *Runtime) toolMulticaGetExecution(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := mcpresult.Arguments(req)
	id := stringArg(args, "execution_id")
	if id == "" {
		return r.controlError(ctx, req, "BAD_REQUEST", errors.New("execution_id is required"))
	}
	payload, cli, err := runMulticaJSON(ctx, [][]string{
		{"issue", "get", id, "--output", "json"},
		{"issue", "view", id, "--output", "json"},
		{"issue", "show", id, "--output", "json"},
	})
	if err != nil {
		return r.controlError(ctx, req, "MULTICA_CLI_FAILED", err)
	}
	execution := asObject(payload)
	data := map[string]any{
		"execution":     execution,
		"github":        githubLinkFromExecution(execution),
		"cli":           safeCLIMeta(cli),
		"stuck_in_view": stuckReason(execution),
	}
	identifier := firstNonEmpty(stringMap(execution, "identifier"), id)
	return compactToolResult(data, fmt.Sprintf("Loaded Multica execution %s.", identifier)), nil
}

func (r *Runtime) toolMulticaListAgents(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := mcpresult.Arguments(req)
	payload, cli, err := runMulticaJSON(ctx, [][]string{
		{"agent", "list", "--output", "json"},
		{"agents", "list", "--output", "json"},
	})
	if err != nil {
		return r.controlError(ctx, req, "MULTICA_CLI_FAILED", err)
	}
	agents := extractAgents(payload)
	query := strings.ToLower(stringArg(args, "query"))
	if query != "" {
		filtered := agents[:0]
		for _, agent := range agents {
			blob := strings.ToLower(fmt.Sprintf("%v %v %v %v", agent["id"], agent["name"], agent["runtime"], agent["harness"]))
			if strings.Contains(blob, query) {
				filtered = append(filtered, agent)
			}
		}
		agents = filtered
	}
	data := map[string]any{"agents": agents, "count": len(agents), "cli": safeCLIMeta(cli)}
	return compactToolResult(data, fmt.Sprintf("Listed %d Multica agents.", len(agents))), nil
}

func (r *Runtime) toolMulticaAssignExecution(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if gated, result, err := r.requireWriteConfirm(ctx, req, "Assign this Multica execution?"); gated {
		return result, err
	}
	args := mcpresult.Arguments(req)
	id := stringArg(args, "execution_id")
	assigneeID := stringArg(args, "assignee_id")
	assignee := stringArg(args, "assignee")
	if id == "" || (assigneeID == "" && assignee == "") {
		return r.controlError(ctx, req, "BAD_REQUEST", errors.New("execution_id and assignee_id or assignee are required"))
	}
	attempts := [][]string{}
	if assigneeID != "" {
		attempts = append(attempts,
			[]string{"issue", "update", id, "--assignee-id", assigneeID, "--output", "json"},
			[]string{"issue", "assign", id, "--assignee-id", assigneeID, "--output", "json"},
		)
	}
	if assignee != "" {
		attempts = append(attempts,
			[]string{"issue", "update", id, "--assignee", assignee, "--output", "json"},
			[]string{"issue", "assign", id, "--assignee", assignee, "--output", "json"},
		)
	}
	payload, cli, err := runMulticaJSON(ctx, attempts)
	if err != nil {
		return r.controlError(ctx, req, "MULTICA_CLI_FAILED", err)
	}
	return compactToolResult(map[string]any{"execution": payload, "cli": safeCLIMeta(cli)}, "Assigned Multica execution."), nil
}

func (r *Runtime) toolMulticaRetryExecution(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if gated, result, err := r.requireWriteConfirm(ctx, req, "Retry this Multica execution?"); gated {
		return result, err
	}
	id := stringArg(mcpresult.Arguments(req), "execution_id")
	payload, cli, err := runMulticaJSON(ctx, [][]string{
		{"issue", "retry", id, "--output", "json"},
		{"issue", "rerun", id, "--output", "json"},
		{"run", "retry", id, "--output", "json"},
		{"issue", "resume", id, "--output", "json"},
	})
	if err != nil {
		return r.controlError(ctx, req, "MULTICA_CLI_FAILED", err)
	}
	return compactToolResult(map[string]any{"execution": payload, "cli": safeCLIMeta(cli)}, "Retried Multica execution."), nil
}

func (r *Runtime) toolMulticaRequestReview(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if gated, result, err := r.requireWriteConfirm(ctx, req, "Request review on this Multica execution?"); gated {
		return result, err
	}
	args := mcpresult.Arguments(req)
	id := stringArg(args, "execution_id")
	attempts := [][]string{
		{"issue", "review", id, "--output", "json"},
		{"issue", "request-review", id, "--output", "json"},
		{"issue", "update", id, "--status", "in_review", "--output", "json"},
	}
	if reviewerID := stringArg(args, "reviewer_id"); reviewerID != "" {
		attempts = append([][]string{
			{"issue", "review", id, "--assignee-id", reviewerID, "--output", "json"},
			{"issue", "update", id, "--status", "in_review", "--assignee-id", reviewerID, "--output", "json"},
		}, attempts...)
	}
	if reviewer := stringArg(args, "reviewer"); reviewer != "" {
		attempts = append([][]string{
			{"issue", "review", id, "--assignee", reviewer, "--output", "json"},
			{"issue", "update", id, "--status", "in_review", "--assignee", reviewer, "--output", "json"},
		}, attempts...)
	}
	payload, cli, err := runMulticaJSON(ctx, attempts)
	if err != nil {
		return r.controlError(ctx, req, "MULTICA_CLI_FAILED", err)
	}
	return compactToolResult(map[string]any{"execution": payload, "cli": safeCLIMeta(cli)}, "Requested Multica review."), nil
}

func (r *Runtime) toolMulticaUpdateStatus(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if gated, result, err := r.requireWriteConfirm(ctx, req, "Change this Multica execution status?"); gated {
		return result, err
	}
	args := mcpresult.Arguments(req)
	id := stringArg(args, "execution_id")
	status := stringArg(args, "status")
	payload, cli, err := runMulticaJSON(ctx, [][]string{
		{"issue", "update", id, "--status", status, "--output", "json"},
		{"issue", "status", id, status, "--output", "json"},
	})
	if err != nil {
		return r.controlError(ctx, req, "MULTICA_CLI_FAILED", err)
	}
	return compactToolResult(map[string]any{"execution": payload, "cli": safeCLIMeta(cli)}, "Updated Multica status."), nil
}

func (r *Runtime) toolGitHubComment(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if gated, result, err := r.requireWriteConfirm(ctx, req, "Post this GitHub comment?"); gated {
		return result, err
	}
	args := mcpresult.Arguments(req)
	body := stringArg(args, "body")
	if body == "" {
		return r.controlError(ctx, req, "BAD_REQUEST", errors.New("body is required"))
	}
	repo := stringArg(args, "repo")
	number := intArg(args, "number", 0)
	target := firstNonEmpty(stringArg(args, "target"), "auto")
	if execID := stringArg(args, "execution_id"); execID != "" && (repo == "" || number == 0) {
		payload, _, err := runMulticaJSON(ctx, [][]string{
			{"issue", "get", execID, "--output", "json"},
			{"issue", "view", execID, "--output", "json"},
			{"issue", "show", execID, "--output", "json"},
		})
		if err != nil {
			return r.controlError(ctx, req, "MULTICA_CLI_FAILED", err)
		}
		link := githubLinkFromExecution(asObject(payload))
		if repo == "" {
			repo = stringMap(link, "repo")
		}
		if number == 0 {
			number = firstNumber(link, "number", 0)
		}
		if target == "auto" {
			if kind := stringMap(link, "kind"); kind != "" {
				target = kind
			}
		}
	}
	if repo == "" || number == 0 {
		return r.controlError(ctx, req, "BAD_REQUEST", errors.New("repo and number are required, or pass execution_id with GitHub metadata"))
	}
	kind := target
	if kind == "auto" {
		kind = "issue"
	}
	cli, err := runGHComment(ctx, kind, repo, number, body)
	if err != nil && target == "auto" {
		cli, err = runGHComment(ctx, "pr", repo, number, body)
	}
	if err != nil {
		return r.controlError(ctx, req, "GITHUB_CLI_FAILED", err)
	}
	data := map[string]any{
		"repo":   repo,
		"number": number,
		"kind":   kind,
		"cli":    safeCLIMeta(cli),
	}
	return compactToolResult(data, fmt.Sprintf("Commented on %s %s#%d.", kind, repo, number)), nil
}

func (r *Runtime) requireWriteConfirm(ctx context.Context, req *mcp.CallToolRequest, summary string) (bool, *mcp.CallToolResult, error) {
	args := mcpresult.Arguments(req)
	if boolLike(args, "user_confirmed") {
		return false, nil, nil
	}
	envReq, _ := r.parseEnv(ctx, req)
	retry := map[string]any{}
	for key, value := range args {
		if envLooksSecret(key) {
			continue
		}
		retry[key] = value
	}
	retry["user_confirmed"] = true
	response := envelope.Fail(envelope.StatusNeedConfirmation, envReq.RequestID, "", map[string]any{
		"confirmation_required":   true,
		"user_confirmed_required": true,
		"summary":                 summary,
		"user_confirmed":          true,
		"retry_arguments":         retry,
	}, "USER_CONFIRMATION_REQUIRED", summary+" Retry the same tool with user_confirmed=true. Tokens are never shown.")
	result, err := r.resultJSON(response)
	return true, result, err
}

func (r *Runtime) controlError(ctx context.Context, req *mcp.CallToolRequest, code string, err error) (*mcp.CallToolResult, error) {
	envReq, _ := r.parseEnv(ctx, req)
	message := redactSecretText(err.Error())
	response := envelope.Fail(envelope.StatusError, envReq.RequestID, "", nil, code, message)
	return r.resultJSON(response)
}

func runMulticaJSON(ctx context.Context, attempts [][]string) (any, controlCLIResult, error) {
	bin, err := controlLookPath(firstNonEmpty(os.Getenv("MCPX_MULTICA_BIN"), "multica"))
	if err != nil {
		return nil, controlCLIResult{}, fmt.Errorf("multica CLI not found on PATH")
	}
	var last controlCLIResult
	var lastErr error
	for _, argv := range attempts {
		result, runErr := controlRunCLI(ctx, bin, argv)
		last = result
		if runErr != nil {
			lastErr = runErr
			if isUnknownCLICommand(result) {
				continue
			}
			return nil, result, runErr
		}
		payload, parseErr := parseCLIJSON(result.Stdout)
		if parseErr != nil {
			lastErr = parseErr
			if isUnknownCLICommand(result) {
				continue
			}
			return nil, result, parseErr
		}
		return payload, result, nil
	}
	if lastErr == nil {
		lastErr = errors.New("multica CLI produced no JSON")
	}
	return nil, last, lastErr
}

func runGHComment(ctx context.Context, kind, repo string, number int, body string) (controlCLIResult, error) {
	bin, err := controlLookPath(firstNonEmpty(os.Getenv("MCPX_GH_BIN"), "gh"))
	if err != nil {
		return controlCLIResult{}, fmt.Errorf("gh CLI not found on PATH")
	}
	verb := "issue"
	if kind == "pr" {
		verb = "pr"
	}
	argv := []string{verb, "comment", fmt.Sprintf("%d", number), "--repo", repo, "--body", body}
	result, runErr := controlRunCLI(ctx, bin, argv)
	return result, runErr
}

func runControlCLI(ctx context.Context, bin string, argv []string) (controlCLIResult, error) {
	runCtx, cancel := context.WithTimeout(ctx, controlCLITimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, bin, argv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = sanitizedCLIEnv(os.Environ())
	cmd.Stdin = nil
	err := cmd.Run()
	result := controlCLIResult{
		Args:     append([]string{bin}, argv...),
		Stdout:   stdout.String(),
		Stderr:   redactSecretText(stderr.String()),
		ExitCode: exitCodeFrom(err),
	}
	if err != nil {
		message := strings.TrimSpace(result.Stderr)
		if message == "" {
			message = redactSecretText(err.Error())
		}
		return result, fmt.Errorf("%s: %s", strings.Join(safeArgv(result.Args), " "), message)
	}
	return result, nil
}

func sanitizedCLIEnv(env []string) []string {
	// Keep the process environment so gh/multica can use existing logins, but
	// never copy secret names into a structure that might be serialized.
	out := make([]string, 0, len(env))
	for _, item := range env {
		name, _, _ := strings.Cut(item, "=")
		if envLooksSecret(name) {
			// Preserve the live credential for the child process only.
			out = append(out, item)
			continue
		}
		out = append(out, item)
	}
	return out
}

func parseCLIJSON(stdout string) (any, error) {
	trimmed := strings.TrimSpace(redactSecretText(stdout))
	if trimmed == "" {
		return nil, errors.New("empty CLI JSON")
	}
	decoder := json.NewDecoder(strings.NewReader(trimmed))
	decoder.UseNumber()
	var payload any
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("CLI output is not JSON")
	}
	return payload, nil
}

func extractExecutions(payload any) []map[string]any {
	switch typed := payload.(type) {
	case []any:
		return objectsFromAny(typed)
	case map[string]any:
		for _, key := range []string{"issues", "executions", "items", "data"} {
			if items, ok := typed[key].([]any); ok {
				return objectsFromAny(items)
			}
		}
		return []map[string]any{typed}
	default:
		return nil
	}
}

func extractAgents(payload any) []map[string]any {
	switch typed := payload.(type) {
	case []any:
		return objectsFromAny(typed)
	case map[string]any:
		for _, key := range []string{"agents", "items", "data"} {
			if items, ok := typed[key].([]any); ok {
				return objectsFromAny(items)
			}
		}
		return []map[string]any{typed}
	default:
		return nil
	}
}

func objectsFromAny(items []any) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if asMap, ok := item.(map[string]any); ok {
			out = append(out, asMap)
		}
	}
	return out
}

func asObject(payload any) map[string]any {
	if asMap, ok := payload.(map[string]any); ok {
		return asMap
	}
	return map[string]any{"value": payload}
}

func githubLinkFromExecution(execution map[string]any) map[string]any {
	meta := map[string]any{}
	if nested, ok := execution["metadata"].(map[string]any); ok {
		meta = nested
	}
	repo := firstNonEmpty(stringMap(meta, "github_repo"), stringMap(execution, "github_repo"))
	issue := firstNumber(meta, "github_issue_number", firstNumber(execution, "github_issue_number", 0))
	url := firstNonEmpty(stringMap(meta, "github_issue_url"), stringMap(execution, "github_issue_url"), stringMap(meta, "github_pr_url"))
	kind := "issue"
	if strings.Contains(url, "/pull/") || firstNumber(meta, "github_pr_number", 0) > 0 {
		kind = "pr"
		if issue == 0 {
			issue = firstNumber(meta, "github_pr_number", 0)
		}
	}
	if repo == "" || issue == 0 {
		title := firstNonEmpty(stringMap(execution, "title"), stringMap(execution, "description"), url)
		if match := githubIssueRef.FindStringSubmatch(title); len(match) > 0 {
			if match[1] != "" && match[2] != "" {
				repo = match[1] + "/" + match[2]
			}
			for _, group := range match[3:] {
				if group != "" {
					issue = firstNumber(map[string]any{"n": json.Number(group)}, "n", issue)
					break
				}
			}
		}
	}
	out := map[string]any{}
	if repo != "" {
		out["repo"] = repo
	}
	if issue > 0 {
		out["number"] = issue
	}
	if url != "" {
		out["url"] = url
	}
	if len(out) > 0 {
		out["kind"] = kind
	}
	return out
}

func stuckReason(execution map[string]any) string {
	status := strings.ToLower(stringMap(execution, "status"))
	switch status {
	case "in_review":
		return "in_review"
	case "blocked":
		return "blocked"
	case "in_progress":
		return "in_progress"
	default:
		return ""
	}
}

func safeCLIMeta(result controlCLIResult) map[string]any {
	return map[string]any{
		"argv":      safeArgv(result.Args),
		"exit_code": result.ExitCode,
	}
}

func safeArgv(argv []string) []string {
	out := make([]string, 0, len(argv))
	skipValue := false
	for i, arg := range argv {
		if skipValue {
			skipValue = false
			out = append(out, "[REDACTED]")
			continue
		}
		lower := strings.ToLower(arg)
		if strings.HasPrefix(lower, "--token") || strings.HasPrefix(lower, "--password") || strings.Contains(lower, "secret") {
			if !strings.Contains(arg, "=") && i+1 < len(argv) {
				skipValue = true
			}
			out = append(out, redactSecretText(arg))
			continue
		}
		out = append(out, redactSecretText(arg))
	}
	return out
}

func isUnknownCLICommand(result controlCLIResult) bool {
	blob := strings.ToLower(result.Stderr + " " + result.Stdout)
	return strings.Contains(blob, "unknown command") ||
		strings.Contains(blob, "unknown flag") ||
		strings.Contains(blob, "unrecognized") ||
		strings.Contains(blob, "invalid command")
}

func exitCodeFrom(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return 1
}
