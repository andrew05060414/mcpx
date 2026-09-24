package server

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/audit"
	"mcpx/internal/mcpresult"
	"mcpx/internal/plan"
)

var planRunGrantToolAnnotation = toolAnnotation{
	ReadOnly: false, Destructive: false, Idempotent: true, OpenWorld: false,
	Title: "Plan 运行授权（一次确认，范围内免逐次确认）",
	Meta: mcp.Meta{"mcpx/action_risk": map[string]any{
		"create":       riskDescriptor(false, false, true, false, "authorization_grant_user_confirmed"),
		"get":          riskDescriptor(true, false, true, false, "server_state_read"),
		"revoke":       riskDescriptor(false, false, true, false, "authorization_reduce"),
		"report_usage": riskDescriptor(false, false, true, false, "server_state_write"),
	}},
}

func (r *Runtime) registerPlanRunGrantTool(s *mcp.Server, confirm map[string]any) {
	r.addToolIfAbsent(s, cleanCoreTool("plan_run_grant",
		"Plan 级运行授权（run grant）。create 需要用户确认一次（user_confirmed=true + idempotency_key），写明允许的仓库、自动合并最高风险等级"+
			"（none/routine 例行/standard 标准/review 需过目；signoff 需签字永远要人）、预算（max_tokens、max_runtime_minutes、max_attempts）和过期时间。"+
			"有效期内，该 Plan 的 multica_materialize_plan_task 传入 repository 与 risk_level 即可免逐次确认，每次使用都写审计；"+
			"超出范围、过期、撤销、预算用完时自动退回逐次确认。get 读取当前授权与历史；revoke 立即撤销；report_usage 上报累计 token 用量（单调递增）。",
		map[string]any{
			"remote_session_id":   stringSchema("Persistent Remote Session ID"),
			"purpose":             stringSchema("Why this grant action is needed"),
			"action":              enumSchema("Grant action", "create", "get", "revoke", "report_usage"),
			"plan_id":             stringSchema("Exact Plan ID returned by plan create/read"),
			"repositories":        arraySchema(map[string]any{"type": "string"}, "create: allowed repositories as owner/name (GitHub URLs are normalized); no wildcards"),
			"auto_merge_max_risk": enumSchema("create: highest risk level that may auto-merge; signoff is never allowed", plan.AutoMergeRiskCeilings()...),
			"max_tokens":          numberSchema("create: token budget for the whole run"),
			"max_runtime_minutes": numberSchema("create: wall-clock budget, counted from the first authorized dispatch"),
			"max_attempts":        numberSchema("create: maximum dispatch attempts (each new idempotency_key for an undispatched task counts once)"),
			"expires_in_minutes":  numberSchema("create: grant lifetime from now; at most 43200 (30 days)"),
			"note":                stringSchema("create: optional human note, e.g. the goal or milestone"),
			"reason":              stringSchema("revoke: why the grant is revoked"),
			"tokens_used_total":   numberSchema("report_usage: cumulative tokens spent under the active grant"),
			"idempotency_key":     stringSchema("create: stable key to reuse when retrying this same grant"),
			"user_confirmed":      confirm,
		}, []string{"remote_session_id", "purpose", "action", "plan_id"}, planRunGrantToolAnnotation), r.toolPlanRunGrant)
}

func (r *Runtime) toolPlanRunGrant(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := mcpresult.Arguments(req)
	switch action := strings.TrimSpace(stringArg(args, "action")); action {
	case "create":
		if strings.TrimSpace(stringArg(args, "idempotency_key")) == "" {
			return r.controlError(ctx, req, "BAD_REQUEST", errors.New("idempotency_key is required; reuse it when retrying this grant"))
		}
		if gated, result, err := r.requireWriteConfirm(ctx, req, runGrantConfirmSummary(args)); gated {
			return result, err
		}
		return r.withCleanIdempotency(ctx, req, "plan_run_grant", args, r.planRunGrantAction)
	case "get", "revoke", "report_usage":
		return r.planRunGrantAction(ctx, req)
	default:
		return r.controlError(ctx, req, "INVALID_ACTION", fmt.Errorf("plan_run_grant does not support action %q", action))
	}
}

func (r *Runtime) planRunGrantAction(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	action := strings.TrimSpace(stringArg(mcpresult.Arguments(req), "action"))
	envReq, principal, session, fail := r.changeRequest(ctx, req, action != "get")
	if fail != nil {
		return fail, nil
	}
	planID := stringPayload(envReq.Payload, "plan_id")
	if planID == "" {
		return r.planError(envReq, session, fmt.Errorf("%w: plan_id is required", plan.ErrInvalidInput))
	}
	var grant plan.RunGrant
	var err error
	switch action {
	case "create":
		repositories, parseErr := stringListValue(envReq.Payload["repositories"])
		if parseErr != nil {
			return r.planError(envReq, session, fmt.Errorf("%w: repositories: %v", plan.ErrInvalidInput, parseErr))
		}
		grant, err = r.plans.CreateRunGrant(ctx, session.ID, planID, principal.ID, plan.RunGrantInput{
			Repositories:      repositories,
			AutoMergeMaxRisk:  stringPayload(envReq.Payload, "auto_merge_max_risk"),
			MaxTokens:         int64(intPayload(envReq.Payload, "max_tokens")),
			MaxRuntimeMinutes: intPayload(envReq.Payload, "max_runtime_minutes"),
			MaxAttempts:       intPayload(envReq.Payload, "max_attempts"),
			ExpiresInMinutes:  intPayload(envReq.Payload, "expires_in_minutes"),
			Note:              stringPayload(envReq.Payload, "note"),
		})
	case "revoke":
		grant, err = r.plans.RevokeRunGrant(ctx, session.ID, planID, principal.ID, stringPayload(envReq.Payload, "reason"))
	case "report_usage":
		if _, present := envReq.Payload["tokens_used_total"]; !present {
			return r.planError(envReq, session, fmt.Errorf("%w: tokens_used_total is required", plan.ErrInvalidInput))
		}
		grant, err = r.plans.ReportRunGrantUsage(ctx, session.ID, planID, principal.ID, int64(intPayload(envReq.Payload, "tokens_used_total")))
	case "get":
		grants, listErr := r.plans.ListRunGrants(ctx, session.ID, planID)
		if listErr != nil {
			return r.planError(envReq, session, listErr)
		}
		data := map[string]any{"plan_id": planID, "grants": grants, "active": nil}
		for i := range grants {
			if grants[i].Status == plan.RunGrantActive {
				data["active"] = grants[i]
				break
			}
		}
		return r.remoteResult(envReq, session.ID, session.WorkspaceName, data)
	}
	if err != nil {
		if errors.Is(err, plan.ErrRunGrantNotFound) {
			return r.terminalError(envReq, session.ID, session.WorkspaceName, "RUN_GRANT_NOT_FOUND", err.Error())
		}
		return r.planError(envReq, session, err)
	}
	r.logAudit(audit.Event{
		RequestID: envReq.RequestID, RemoteSessionID: session.ID, Workspace: session.WorkspaceName,
		Tool: "plan_run_grant", Command: action, Status: "ok", ApprovalID: grant.ID,
		Detail: map[string]any{"plan_id": planID, "principal_id": principal.ID, "effective_status": grant.Effective},
	})
	return r.remoteResult(envReq, session.ID, session.WorkspaceName, map[string]any{"plan_id": planID, "action": action, "grant": grant})
}

func runGrantConfirmSummary(args map[string]any) string {
	repositories, _ := stringListValue(args["repositories"])
	return fmt.Sprintf("Grant a run authorization for Plan %s: repositories=%s, auto-merge up to %s, budget %d tokens / %d minutes runtime / %d attempts, expires in %d minutes. Dispatches inside this scope will skip per-task confirmation but stay audited.",
		stringArg(args, "plan_id"), strings.Join(repositories, ","), stringArg(args, "auto_merge_max_risk"),
		intArg(args, "max_tokens", 0), intArg(args, "max_runtime_minutes", 0), intArg(args, "max_attempts", 0), intArg(args, "expires_in_minutes", 0))
}

type materializeAuthorizationKey struct{}

func stringListValue(value any) ([]string, error) {
	switch items := value.(type) {
	case nil:
		return nil, nil
	case []string:
		return items, nil
	case []any:
		result := make([]string, 0, len(items))
		for _, item := range items {
			text, ok := item.(string)
			if !ok {
				return nil, errors.New("must be an array of strings")
			}
			result = append(result, text)
		}
		return result, nil
	default:
		return nil, errors.New("must be an array of strings")
	}
}

func withMaterializeAuthorization(ctx context.Context, authorization map[string]any) context.Context {
	return context.WithValue(ctx, materializeAuthorizationKey{}, authorization)
}

func materializeAuthorization(ctx context.Context) map[string]any {
	value, _ := ctx.Value(materializeAuthorizationKey{}).(map[string]any)
	return value
}

// authorizeMaterializeWithRunGrant decides whether an unconfirmed
// materialization is covered by the Plan's active run grant. Every decision
// that involves a grant is written to plan_events by the plan service and to
// the audit log here.
func (r *Runtime) authorizeMaterializeWithRunGrant(ctx context.Context, req *mcp.CallToolRequest) (plan.RunGrantDecision, *mcp.CallToolResult, error) {
	envReq, principal, session, fail := r.changeRequest(ctx, req, true)
	if fail != nil {
		return plan.RunGrantDecision{}, fail, nil
	}
	planID := stringPayload(envReq.Payload, "plan_id")
	taskID := stringPayload(envReq.Payload, "plan_task_id")
	if planID == "" || taskID == "" {
		return plan.RunGrantDecision{Code: "bad_request", Reason: "plan_id and plan_task_id are required"}, nil, nil
	}
	decision, err := r.plans.AuthorizeRunGrantUse(ctx, session.ID, planID, plan.RunGrantUse{
		PlanTaskID: taskID, IdempotencyKey: stringPayload(envReq.Payload, "idempotency_key"),
		Repository: stringPayload(envReq.Payload, "repository"), RiskLevel: stringPayload(envReq.Payload, "risk_level"),
		PrincipalID: principal.ID,
	})
	if err != nil {
		if errors.Is(err, plan.ErrNotFound) {
			return plan.RunGrantDecision{Code: "plan_or_task_not_found", Reason: err.Error()}, nil, nil
		}
		return plan.RunGrantDecision{}, nil, err
	}
	if decision.Grant != nil {
		status := "run_grant_fallback"
		if decision.Authorized {
			status = "run_grant_authorized"
		}
		r.logAudit(audit.Event{
			RequestID: envReq.RequestID, RemoteSessionID: session.ID, Workspace: session.WorkspaceName,
			Tool: "multica_materialize_plan_task", Status: status, ApprovalID: decision.Grant.ID,
			Detail: map[string]any{
				"plan_id": planID, "plan_task_id": taskID, "principal_id": principal.ID, "code": decision.Code,
				"repository": decision.Repository, "risk_level": decision.RiskLevel, "auto_merge_allowed": decision.AutoMergeAllowed,
				"attempt_consumed": decision.AttemptConsumed, "replay": decision.Replay,
			},
		})
	}
	return decision, nil, nil
}

func runGrantAuthorization(decision plan.RunGrantDecision) map[string]any {
	grant := decision.Grant
	return map[string]any{
		"mode": "run_grant", "grant_id": grant.ID, "repository": decision.Repository, "risk_level": decision.RiskLevel,
		"auto_merge_allowed": decision.AutoMergeAllowed, "auto_merge_max_risk": grant.AutoMergeMaxRisk,
		"attempt_consumed": decision.AttemptConsumed, "attempts_used": grant.AttemptsUsed, "max_attempts": grant.MaxAttempts,
		"tokens_used": grant.TokensUsed, "max_tokens": grant.MaxTokens, "expires_at": grant.ExpiresAt, "runtime_deadline": grant.RuntimeDeadline,
	}
}

func runGrantFallback(decision plan.RunGrantDecision) map[string]any {
	fallback := map[string]any{"applied": false, "code": decision.Code, "reason": decision.Reason}
	if decision.Grant != nil {
		fallback["grant_id"] = decision.Grant.ID
		fallback["effective_status"] = decision.Grant.Effective
	}
	return fallback
}

// dispatchPolicyFooter tells the executing agent which merge gate applies.
func dispatchPolicyFooter(authorization map[string]any, repository, risk string) string {
	repository = plan.NormalizeRepository(repository)
	if repository == "" && risk == "" && authorization["mode"] != "run_grant" {
		return ""
	}
	mode, _ := authorization["mode"].(string)
	grantID, _ := authorization["grant_id"].(string)
	autoMerge := false
	if value, ok := authorization["auto_merge_allowed"].(bool); ok {
		autoMerge = value
	}
	gate := "human_required"
	if autoMerge {
		gate = "auto_merge_after_ci_and_review"
	}
	return fmt.Sprintf("MCPX dispatch policy: authorization=%s repository=%s risk=%s merge=%s\n<!-- mcpx-dispatch-policy:v1 authorization=%s grant=%s repository=%s risk=%s auto_merge=%t -->",
		mode, firstNonEmpty(repository, "unspecified"), firstNonEmpty(risk, "unspecified"), gate, mode, grantID, repository, risk, autoMerge)
}
