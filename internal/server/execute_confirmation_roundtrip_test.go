package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestExecuteConfirmationRecoveryIsScopedAndConsumedOnce(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	rt.cfg.Security.Commands.Default = "confirm"
	rt.cfg.Security.Commands.Allow = nil
	firstSession := operationTestSession(t, rt, "demo")
	secondSession := operationTestSession(t, rt, "demo")
	request := map[string]any{
		"action": "run", "remote_session_id": firstSession.ID,
		"command": "echo mcpx-confirmation-roundtrip",
		"purpose": "verify exact approval recovery", "scope": "workspace",
	}
	waiting := callEnvelope(t, rt.toolExecute, context.Background(), request)
	if waiting["status"] != "waiting_confirmation" || errorCode(waiting) != "user_confirmation_required" {
		t.Fatalf("unapproved command did not wait: %+v", waiting)
	}
	data := waiting["data"].(map[string]any)
	digest := data["pending_digest"]
	if digest == nil || digest != data["command_digest"] {
		t.Fatalf("confirmation digest missing or inconsistent: %+v", data)
	}
	recovery := waiting["error"].(map[string]any)["recovery"].(map[string]any)
	retry := recovery["arguments"].(map[string]any)
	if recovery["tool"] != "execute" || retry["user_confirmed"] != true {
		t.Fatalf("public recovery does not carry confirmation: %+v", recovery)
	}
	for _, key := range []string{"command", "purpose", "scope", "remote_session_id"} {
		if retry[key] != request[key] {
			t.Fatalf("recovery changed %s: %+v", key, retry)
		}
	}

	withoutApproval := cloneMap(request)
	withoutApproval["user_confirmed"] = false
	if result := callEnvelope(t, rt.toolExecute, context.Background(), withoutApproval); result["status"] != "waiting_confirmation" {
		t.Fatalf("user_confirmed=false bypassed approval: %+v", result)
	}
	for name, change := range map[string]func(map[string]any){
		"command": func(args map[string]any) { args["command"] = "echo changed-command" },
		"purpose": func(args map[string]any) { args["purpose"] = "changed purpose" },
		"session": func(args map[string]any) { args["remote_session_id"] = secondSession.ID },
	} {
		changed := cloneMap(retry)
		change(changed)
		result := callEnvelope(t, rt.toolExecute, context.Background(), changed)
		if result["status"] != "waiting_confirmation" || result["data"].(map[string]any)["pending_digest"] == digest {
			t.Fatalf("%s reused original approval: %+v", name, result)
		}
	}

	confirmed := callEnvelope(t, rt.toolExecute, context.Background(), retry)
	if !statusOK(confirmed) || confirmed["data"].(map[string]any)["command_digest"] != digest {
		t.Fatalf("exact recovery did not execute: %+v", confirmed)
	}
	consumed := callEnvelope(t, rt.toolExecute, context.Background(), retry)
	if consumed["status"] != "waiting_confirmation" {
		t.Fatalf("consumed approval authorized another execution: %+v", consumed)
	}
}

func TestExecuteConcurrentConfirmedRetriesStartProcessOnce(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	rt.cfg.Security.Commands.Default = "confirm"
	rt.cfg.Security.Commands.Allow = nil
	session := operationTestSession(t, rt, "demo")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(session.WorkspacePath, "confirmed-processes.jsonl")
	request := map[string]any{
		"action": "run", "remote_session_id": session.ID,
		"purpose": "run one confirmed child process", "scope": "workspace",
		"argv":  []any{executable, "-test.run=^TestIssue823ArgvChild$", "--", "--issue823-argv-child", output, "executed"},
		"shell": false,
	}
	waiting := callEnvelope(t, rt.toolExecute, context.Background(), request)
	if waiting["status"] != "waiting_confirmation" {
		t.Fatalf("command did not wait for confirmation: %+v", waiting)
	}
	retry := waiting["error"].(map[string]any)["recovery"].(map[string]any)["arguments"].(map[string]any)
	start := make(chan struct{})
	results := make(chan map[string]any, 2)
	for range 2 {
		go func() {
			<-start
			results <- callEnvelope(t, rt.toolExecute, context.Background(), cloneMap(retry))
		}()
	}
	close(start)
	succeeded := 0
	for range 2 {
		result := <-results
		if statusOK(result) {
			succeeded++
			continue
		}
		if result["status"] != "waiting_confirmation" && errorCode(result) != "confirmation_state_error" {
			t.Fatalf("unexpected concurrent retry result: %+v", result)
		}
	}
	if succeeded != 1 {
		t.Fatalf("one approval launched %d processes, want exactly one", succeeded)
	}
	content, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var args []string
	if err := json.Unmarshal(content, &args); err != nil || len(args) != 1 || args[0] != "executed" {
		t.Fatalf("process ran more than once or received wrong arguments: %q, %v", content, err)
	}
}

func TestExecuteRecoveryPreservesIdempotencyKey(t *testing.T) {
	for _, requestBody := range []map[string]any{
		{"command": "echo confirmed-key"},
		{"runtime": "node", "script": "console.log('confirmed-key')"},
	} {
		rt := newWorkspaceRuntime(t, "demo")
		rt.cfg.Security.Commands.Default = "confirm"
		rt.cfg.Security.Commands.Allow = nil
		session := operationTestSession(t, rt, "demo")
		request := map[string]any{
			"action": "run", "remote_session_id": session.ID,
			"purpose": "verify recovery keeps the idempotency key", "scope": "workspace",
			"idempotency_key": "confirmation-key",
		}
		for key, value := range requestBody {
			request[key] = value
		}
		waiting := callEnvelope(t, rt.toolExecute, context.Background(), request)
		if waiting["status"] != "waiting_confirmation" {
			t.Fatalf("request did not wait: %+v", waiting)
		}
		retry := waiting["error"].(map[string]any)["recovery"].(map[string]any)["arguments"].(map[string]any)
		if retry["idempotency_key"] != request["idempotency_key"] {
			t.Fatalf("recovery lost idempotency key: %+v", retry)
		}
		if _, runtimeRequest := requestBody["runtime"]; runtimeRequest {
			// Script source is intentionally omitted from the public response.
			retry["script"] = request["script"]
			continue
		}
		confirmed := callEnvelope(t, rt.toolExecute, context.Background(), retry)
		if !statusOK(confirmed) {
			t.Fatalf("confirmed recovery failed: %+v", confirmed)
		}
		replay := callEnvelope(t, rt.toolExecute, context.Background(), retry)
		if !statusOK(replay) || replay["data"].(map[string]any)["idempotent_replay"] != true {
			t.Fatalf("confirmed recovery did not replay: %+v", replay)
		}
	}
}
