package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"mcpx/internal/plan"
)

type fakeMulticaIssues struct {
	creates      int
	descriptions []string
}

func installFakeMulticaCreate(t *testing.T) *fakeMulticaIssues {
	t.Helper()
	restoreControlCLI(t)
	fake := &fakeMulticaIssues{}
	controlLookPath = func(file string) (string, error) { return "/bin/" + file, nil }
	controlRunCLI = func(_ context.Context, _ string, argv []string) (controlCLIResult, error) {
		if len(argv) < 2 || argv[0] != "issue" || argv[1] != "create" {
			return controlCLIResult{}, fmt.Errorf("unexpected multica call %v", argv)
		}
		for i := range argv {
			if argv[i] == "--description-file" && i+1 < len(argv) {
				raw, err := os.ReadFile(argv[i+1])
				if err != nil {
					return controlCLIResult{}, err
				}
				fake.descriptions = append(fake.descriptions, string(raw))
			}
		}
		fake.creates++
		return controlCLIResult{Stdout: fmt.Sprintf(`{"id":"iss-%d","identifier":"TEC-%d"}`, fake.creates, fake.creates)}, nil
	}
	return fake
}

func waitOperation(t *testing.T, rt *Runtime, sessionID string, accepted map[string]any) map[string]any {
	t.Helper()
	if accepted["status"] != "accepted" {
		t.Fatalf("batch not accepted: %+v", accepted)
	}
	operationID := accepted["data"].(map[string]any)["operation_id"].(string)
	return callOperationTool(t, rt, "operation_manage", map[string]any{
		"remote_session_id": sessionID, "operation_id": operationID, "action": "wait", "timeout_ms": 10000,
	})
}

func TestPlanRunGrantSkipsPerTaskConfirmationThroughOperationBatch(t *testing.T) {
	fake := installFakeMulticaCreate(t)
	rt := newWorkspaceRuntime(t, "demo")
	session := operationTestSession(t, rt, "demo")
	principal, err := rt.principalFromContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	created, err := rt.plans.Create(context.Background(), session.ID, principal.ID, plan.CreateInput{
		Goal: "agent factory run", Tasks: []plan.TaskInput{{ID: "one", Title: "One"}, {ID: "two", Title: "Two"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	materialize := func(taskID, key, repository string) map[string]any {
		return map[string]any{
			"remote_session_id": session.ID, "purpose": "dispatch plan task", "plan_id": created.ID, "plan_task_id": taskID,
			"agent_id": "agent-uuid", "idempotency_key": key, "repository": repository, "risk_level": "standard",
		}
	}

	// Without a grant the existing per-call confirmation gate is unchanged.
	gated := callOperationTool(t, rt, "multica_materialize_plan_task", materialize(created.Tasks[0].ID, "k-none", "andrew/mcpx"))
	if gated["status"] != "waiting_confirmation" || fake.creates != 0 {
		t.Fatalf("expected confirmation without grant: %+v creates=%d", gated, fake.creates)
	}
	if code := gated["data"].(map[string]any)["run_grant"].(map[string]any)["code"]; code != "no_active_grant" {
		t.Fatalf("fallback code = %v", code)
	}

	// One confirmation creates the grant; both new public tools are admitted by operation_batch.
	grantDone := waitOperation(t, rt, session.ID, callOperationTool(t, rt, "operation_batch", map[string]any{
		"remote_session_id": session.ID, "purpose": "authorize this plan once",
		"operations": []any{map[string]any{"id": "grant", "tool": "plan_run_grant", "arguments": map[string]any{
			"remote_session_id": session.ID, "purpose": "authorize run", "action": "create", "plan_id": created.ID,
			"repositories": []any{"https://github.com/Andrew/MCPX"}, "auto_merge_max_risk": "standard",
			"max_tokens": 500000, "max_runtime_minutes": 240, "max_attempts": 5, "expires_in_minutes": 1440,
			"idempotency_key": "grant-1", "user_confirmed": true,
		}}},
	}))
	if grantDone["status"] != "succeeded" {
		t.Fatalf("grant batch = %+v", grantDone)
	}
	grant, err := rt.plans.RunGrant(context.Background(), session.ID, created.ID, "")
	if err != nil || grant.Effective != plan.RunGrantEffectiveActive {
		t.Fatalf("grant = %+v err=%v", grant, err)
	}

	dispatched := waitOperation(t, rt, session.ID, callOperationTool(t, rt, "operation_batch", map[string]any{
		"remote_session_id": session.ID, "purpose": "dispatch under grant",
		"operations": []any{map[string]any{"id": "dispatch", "tool": "multica_materialize_plan_task", "arguments": materialize(created.Tasks[0].ID, "k-one", "andrew/mcpx")}},
	}))
	if dispatched["status"] != "succeeded" || fake.creates != 1 {
		encoded, _ := json.Marshal(dispatched)
		t.Fatalf("dispatch under grant = %s creates=%d", encoded, fake.creates)
	}
	if description := fake.descriptions[0]; !strings.Contains(description, "grant="+grant.ID) || !strings.Contains(description, "auto_merge=true") {
		t.Fatalf("issue description lacks dispatch policy: %q", description)
	}

	// Out-of-scope repository falls back to confirmation and creates nothing.
	outside := callOperationTool(t, rt, "multica_materialize_plan_task", materialize(created.Tasks[1].ID, "k-two", "andrew/pxread"))
	if outside["status"] != "waiting_confirmation" || fake.creates != 1 {
		t.Fatalf("out of scope = %+v creates=%d", outside, fake.creates)
	}
	if code := outside["data"].(map[string]any)["run_grant"].(map[string]any)["code"]; code != "repository_out_of_scope" {
		t.Fatalf("fallback code = %v", code)
	}

	revoked := callOperationTool(t, rt, "plan_run_grant", map[string]any{
		"remote_session_id": session.ID, "purpose": "stop the run", "action": "revoke", "plan_id": created.ID, "reason": "user stopped",
	})
	if revoked["status"] != "success" && revoked["status"] != "ok" && revoked["status"] != "succeeded" {
		t.Fatalf("revoke = %+v", revoked)
	}
	afterRevoke := callOperationTool(t, rt, "multica_materialize_plan_task", materialize(created.Tasks[1].ID, "k-three", "andrew/mcpx"))
	if afterRevoke["status"] != "waiting_confirmation" || fake.creates != 1 {
		t.Fatalf("after revoke = %+v creates=%d", afterRevoke, fake.creates)
	}

	// Explicit confirmation still works exactly as before.
	confirmed := materialize(created.Tasks[1].ID, "k-three", "andrew/mcpx")
	confirmed["user_confirmed"] = true
	if result := callOperationTool(t, rt, "multica_materialize_plan_task", confirmed); fake.creates != 2 {
		t.Fatalf("confirmed dispatch = %+v creates=%d", result, fake.creates)
	}

	loaded, err := rt.plans.Get(context.Background(), session.ID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, event := range loaded.Events {
		counts[event.Type]++
	}
	if counts["plan.run_grant.created"] != 1 || counts["plan.run_grant.used"] != 1 || counts["plan.run_grant.fallback"] != 1 ||
		counts["plan.run_grant.revoked"] != 1 || counts["plan_task.multica_dispatched"] != 2 {
		t.Fatalf("audit events = %+v", counts)
	}
}

