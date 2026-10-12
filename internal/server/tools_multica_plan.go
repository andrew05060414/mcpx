package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/mcpresult"
	"mcpx/internal/plan"
)

const multicaDispatchLease = time.Minute

func (r *Runtime) registerMulticaPlanTaskTool(s *mcp.Server, confirm map[string]any) {
	r.addToolIfAbsent(s, cleanCoreTool("multica_materialize_plan_task",
		"Create and assign exactly one Multica Issue from a ready MCPX Plan Task. The durable link is returned on the Plan Task and repeated calls reuse it. Write-gated: requires an idempotency_key and either user_confirmed=true or an active plan_run_grant for this Plan that covers repository and risk_level; out-of-scope, expired, revoked, or exhausted grants fall back to per-call confirmation.",
		map[string]any{
			"remote_session_id": stringSchema("Persistent Remote Session ID"),
			"purpose":           stringSchema("Why this Plan Task should become a Multica execution now"),
			"plan_id":           stringSchema("Exact Plan ID returned by plan create/read"),
			"plan_task_id":      stringSchema("Exact Plan Task ID returned by plan create/read"),
			"agent_id":          stringSchema("Multica Agent UUID to assign; the Issue is queued for this Agent"),
			"project_id":        stringSchema("Optional Multica Project ID; defaults to MCPX_MULTICA_PROJECT_ID or the active Multica project"),
			"priority":          enumSchema("Optional Multica priority", "low", "medium", "high", "urgent", "none"),
			"repository":        stringSchema("Target repository owner/name; required to use a plan_run_grant and recorded in the Issue dispatch policy"),
			"risk_level":        enumSchema("Task risk level (routine 例行 / standard 标准 / review 需过目 / signoff 需签字); required to use a plan_run_grant", plan.TaskRiskLevels()...),
			"idempotency_key":   stringSchema("Stable key to reuse when retrying this same Plan Task dispatch"),
			"user_confirmed":    confirm,
		}, []string{"remote_session_id", "purpose", "plan_id", "plan_task_id", "agent_id", "idempotency_key"}, arcControlWriteAnnotation), r.toolMulticaMaterializePlanTask)
	r.registerPlanRunGrantTool(s, confirm)
}

func (r *Runtime) toolMulticaMaterializePlanTask(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := mcpresult.Arguments(req)
	if strings.TrimSpace(stringArg(args, "idempotency_key")) == "" {
		return r.controlError(ctx, req, "BAD_REQUEST", errors.New("idempotency_key is required; reuse it when retrying this Plan Task dispatch"))
	}
	authorization := map[string]any{"mode": "user_confirmed"}
	if !boolLike(args, "user_confirmed") {
		decision, fail, err := r.authorizeMaterializeWithRunGrant(ctx, req)
		if fail != nil || err != nil {
			return fail, err
		}
		if !decision.Authorized {
			_, result, err := r.requireWriteConfirm(ctx, req, "Create and assign a Multica Issue for this MCPX Plan Task?",
				map[string]any{"run_grant": runGrantFallback(decision)})
			return result, err
		}
		authorization = runGrantAuthorization(decision)
	}
	return r.withCleanIdempotency(withMaterializeAuthorization(ctx, authorization), req, "multica_materialize_plan_task", args, r.materializePlanTask)
}

func (r *Runtime) materializePlanTask(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, principal, session, fail := r.changeRequest(ctx, req, true)
	if fail != nil {
		return fail, nil
	}
	planID := stringPayload(envReq.Payload, "plan_id")
	taskID := stringPayload(envReq.Payload, "plan_task_id")
	assigneeID := stringPayload(envReq.Payload, "agent_id")
	projectID := firstNonEmpty(stringPayload(envReq.Payload, "project_id"), strings.TrimSpace(os.Getenv("MCPX_MULTICA_PROJECT_ID")))
	priority := stringPayload(envReq.Payload, "priority")
	repository := stringPayload(envReq.Payload, "repository")
	riskLevel := stringPayload(envReq.Payload, "risk_level")
	authorization := materializeAuthorization(ctx)
	if planID == "" || taskID == "" || assigneeID == "" {
		return r.controlError(ctx, req, "BAD_REQUEST", errors.New("plan_id, plan_task_id, and agent_id are required"))
	}

	item, err := r.plans.Get(ctx, session.ID, planID)
	if err != nil {
		return r.controlError(ctx, req, "PLAN_NOT_FOUND", err)
	}
	var task *plan.Task
	for i := range item.Tasks {
		if item.Tasks[i].ID == taskID {
			task = &item.Tasks[i]
			break
		}
	}
	if task == nil {
		return r.controlError(ctx, req, "PLAN_TASK_NOT_FOUND", errors.New("plan_task_id does not belong to this Plan"))
	}

	dispatch, _, err := r.plans.ClaimMulticaDispatch(ctx, session.ID, planID, taskID, principal.ID, assigneeID, projectID, priority, multicaDispatchLease)
	if err != nil {
		switch {
		case errors.Is(err, plan.ErrDependency):
			return r.controlError(ctx, req, "PLAN_DEPENDENCY_NOT_SATISFIED", err)
		case errors.Is(err, plan.ErrDispatchInProgress):
			return r.controlError(ctx, req, "MULTICA_DISPATCH_IN_PROGRESS", err)
		case errors.Is(err, plan.ErrDispatchConflict):
			return r.controlError(ctx, req, "MULTICA_DISPATCH_CONFLICT", err)
		case errors.Is(err, plan.ErrNotFound):
			return r.controlError(ctx, req, "PLAN_TASK_NOT_FOUND", err)
		default:
			return r.controlError(ctx, req, "PLAN_DISPATCH_FAILED", err)
		}
	}
	if dispatch.Status == "dispatched" {
		return r.remoteResult(envReq, session.ID, session.WorkspaceName, withAuthorization(multicaDispatchResult(dispatch, true), authorization))
	}
	marker := multicaPlanTaskMarker(session.ID, planID, taskID)
	if !dispatch.CreatedAt.Equal(dispatch.UpdatedAt) {
		existing, found, err := findMulticaPlanTaskIssue(ctx, projectID, marker, dispatch.CreatedAt.Add(-10*time.Minute))
		if err != nil {
			return r.controlError(ctx, req, "MULTICA_DISPATCH_RECONCILIATION_REQUIRED", err)
		}
		if !found {
			return r.controlError(ctx, req, "MULTICA_DISPATCH_RECONCILIATION_REQUIRED", errors.New("a prior create attempt is recorded but its Issue was not found; refusing to create a duplicate"))
		}
		issueID, identifier := multicaIssueIdentity(existing)
		dispatch, err = r.plans.CompleteMulticaDispatch(ctx, session.ID, principal.ID, dispatch, issueID, identifier)
		if err != nil {
			return r.controlError(ctx, req, "MULTICA_DISPATCH_LINK_FAILED", err)
		}
		return r.remoteResult(envReq, session.ID, session.WorkspaceName, withAuthorization(multicaDispatchResult(dispatch, true), authorization))
	}

	description := strings.TrimSpace(item.Goal + "\n\n" + task.Description)
	if footer := dispatchPolicyFooter(authorization, repository, riskLevel); footer != "" {
		description = strings.TrimSpace(description + "\n\n" + footer)
	}
	description = strings.TrimSpace(description + "\n\n" + marker)
	tmpFile, err := os.CreateTemp("", "mcpx-plan-task-*.md")
	if err != nil {
		return r.controlError(ctx, req, "MULTICA_CLI_FAILED", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)
	if err := tmpFile.Chmod(0600); err != nil {
		tmpFile.Close()
		return r.controlError(ctx, req, "MULTICA_CLI_FAILED", err)
	}
	if _, err := tmpFile.WriteString(description); err != nil {
		tmpFile.Close()
		return r.controlError(ctx, req, "MULTICA_CLI_FAILED", err)
	}
	if err := tmpFile.Close(); err != nil {
		return r.controlError(ctx, req, "MULTICA_CLI_FAILED", err)
	}
	argv := []string{"issue", "create", "--title", task.Title, "--description-file", tmpPath, "--allow-external-file", "--assignee-id", assigneeID, "--output", "json"}
	if projectID != "" {
		argv = append(argv, "--project", projectID)
	}
	if priority != "" {
		argv = append(argv, "--priority", priority)
	}
	payload, cli, err := runMulticaJSON(ctx, [][]string{argv})
	if err != nil {
		return r.controlError(ctx, req, "MULTICA_CLI_FAILED", err)
	}
	issue := asObject(payload)
	if nested, ok := issue["issue"].(map[string]any); ok {
		issue = nested
	}
	issueID, identifier := multicaIssueIdentity(issue)
	if issueID == "" {
		return r.controlError(ctx, req, "MULTICA_DISPATCH_RECONCILIATION_REQUIRED", errors.New("Multica create returned no Issue ID; the stable Plan Task marker was included for reconciliation"))
	}
	dispatch, err = r.plans.CompleteMulticaDispatch(ctx, session.ID, principal.ID, dispatch, issueID, identifier)
	if err != nil {
		return r.controlError(ctx, req, "MULTICA_DISPATCH_LINK_FAILED", err)
	}
	dispatchData := withAuthorization(multicaDispatchResult(dispatch, false), authorization)
	dispatchData["issue"] = issue
	dispatchData["cli"] = safeCLIMeta(cli)
	return r.remoteResult(envReq, session.ID, session.WorkspaceName, dispatchData)
}

func multicaDispatchResult(dispatch plan.MulticaDispatch, reused bool) map[string]any {
	return map[string]any{
		"plan_id": dispatch.PlanID, "plan_task_id": dispatch.PlanTaskID,
		"status": dispatch.Status, "issue_id": dispatch.IssueID, "issue_identifier": dispatch.IssueIdentifier,
		"assignee_id": dispatch.AssigneeID, "project_id": dispatch.ProjectID, "priority": dispatch.Priority,
		"reused": reused,
	}
}

func withAuthorization(data map[string]any, authorization map[string]any) map[string]any {
	if authorization != nil {
		data["authorization"] = authorization
	}
	return data
}

func multicaPlanTaskMarker(remoteSessionID, planID, taskID string) string {
	return fmt.Sprintf("<!-- mcpx-plan-task:v1 session=%s plan=%s task=%s -->", remoteSessionID, planID, taskID)
}

func multicaIssueIdentity(issue map[string]any) (string, string) {
	return firstNonEmpty(stringMap(issue, "id"), stringMap(issue, "issue_id")),
		firstNonEmpty(stringMap(issue, "identifier"), stringMap(issue, "key"))
}

func findMulticaPlanTaskIssue(ctx context.Context, projectID, marker string, createdAfter time.Time) (map[string]any, bool, error) {
	const pageSize = 100
	const maxPages = 100
	for offset := 0; offset < pageSize*maxPages; offset += pageSize {
		argv := []string{"issue", "list", "--output", "json", "--limit", strconv.Itoa(pageSize), "--offset", strconv.Itoa(offset), "--sort", "created_at", "--direction", "desc"}
		if projectID != "" {
			argv = append(argv, "--project", projectID)
		}
		payload, _, err := runMulticaJSON(ctx, [][]string{argv})
		if err != nil {
			return nil, false, err
		}
		issues := extractExecutions(payload)
		if len(issues) == 0 {
			return nil, false, nil
		}
		pageReachedOlderIssue := false
		for _, issue := range issues {
			if strings.Contains(stringMap(issue, "description"), marker) {
				return issue, true, nil
			}
			if created := multicaIssueCreatedAt(issue); !created.IsZero() && created.Before(createdAfter) {
				pageReachedOlderIssue = true
			}
		}
		if pageReachedOlderIssue || len(issues) < pageSize {
			return nil, false, nil
		}
	}
	return nil, false, errors.New("searched 10000 recent Multica Issues without reaching the dispatch start time; refusing to risk a duplicate")
}

func multicaIssueCreatedAt(issue map[string]any) time.Time {
	raw := strings.TrimSpace(fmt.Sprint(issue["created_at"]))
	if raw == "" || raw == "<nil>" {
		return time.Time{}
	}
	if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return parsed.UTC()
	}
	if milliseconds, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return time.UnixMilli(milliseconds).UTC()
	}
	var number json.Number = json.Number(raw)
	if milliseconds, err := number.Int64(); err == nil {
		return time.UnixMilli(milliseconds).UTC()
	}
	return time.Time{}
}
