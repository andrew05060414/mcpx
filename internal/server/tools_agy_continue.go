package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/audit"
	"mcpx/internal/envelope"
	"mcpx/internal/remotesession"
	"mcpx/internal/terminal"
)

const (
	agyContinuationMaxPromptBytes = 64 << 10
	agyContinuationDefaultWait    = 10 * time.Second
	agyContinuationMaxWait        = 60 * time.Second
	agyContinuationWallLimit      = 10 * time.Minute
)

var agyConversationIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-5][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)

type agyContinuationSpec struct {
	Resume         string
	ConversationID string
	Prompt         string
	Model          string
	Effort         string
	Mode           string
	PromptSHA256   string
	DisplayCommand string
	CommandDigest  string
}

func (r *Runtime) toolAGYContinue(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, principal, remote, fail := r.changeRequest(ctx, req, true)
	if fail != nil {
		return fail, nil
	}
	spec, err := agyContinuationSpecFromPayload(envReq.Payload)
	if err != nil {
		return r.terminalError(envReq, remote.ID, remote.WorkspaceName, "bad_request", err.Error())
	}
	if !r.effectiveConfig(remote.WorkspacePath).Terminal.Enabled {
		return r.terminalError(envReq, remote.ID, remote.WorkspaceName, "disabled", "terminal tools are disabled")
	}

	commandPolicy := map[string]any{
		"decision":              "allow",
		"structured_action":     "agy_continue",
		"shell":                 false,
		"dangerous_permissions": false,
	}
	detail := agyContinuationDetail(envReq.Intent, "workspace", spec, commandPolicy)
	detail["continuation"] = agyContinuationData(spec, remote.WorkspacePath)
	if err := r.writeAudit(audit.Event{RequestID: envReq.RequestID, RemoteSessionID: remote.ID, Workspace: remote.WorkspaceName, Tool: "execute", Command: spec.DisplayCommand, Status: "preflight_approved", Detail: detail}); err != nil {
		return r.terminalErrorForContext(ctx, envReq, remote.ID, remote.WorkspaceName, "audit_write_failed", "AGY continuation audit could not be persisted; process was not started")
	}

	task, err := r.tasks.StartRemoteProcessWithObservationContext(
		envReq.RequestID, observationCallID(envReq), "execute", remote.ID, remote.WorkspaceName, remote.WorkspacePath, spec.DisplayCommand,
		terminal.ProcessSpec{
			Executable: "agy",
			Args:       agyContinuationArgs(spec),
			WallLimit:  agyContinuationWallLimit,
		},
	)
	if err != nil {
		return r.terminalError(envReq, remote.ID, remote.WorkspaceName, "AGY_START_ERROR", err.Error())
	}
	_ = r.remote.AddEvent(ctx, principal, remotesession.Event{RemoteSessionID: remote.ID, Type: "command.started", OperationID: task.ID, Summary: "AGY continuation", Metadata: detail})

	waitCtx, cancel := context.WithTimeout(ctx, agyContinuationWait(envReq.Payload))
	completed := task.Wait(waitCtx)
	cancel()
	data := r.taskResultData(task, 0, 0)
	data["continuation"] = agyContinuationData(spec, remote.WorkspacePath)
	data["command"] = spec.DisplayCommand
	data["command_digest"] = spec.CommandDigest
	data["command_policy"] = commandPolicy
	data["purpose"] = envReq.Intent
	data["scope"] = "workspace"
	data["working_directory"] = remote.WorkspacePath
	data["workspace_scoped"] = true
	data["wall_limit_ms"] = agyContinuationWallLimit.Milliseconds()
	capTaskExecutionOutput(data, 256<<10)

	if completed {
		data["completed_in_call"] = true
		detail["execution_task_id"] = task.ID
		detail["exit_code"] = data["exit_code"]
		if code, message := annotateExecutionOutcome(data); code != "" {
			r.logAudit(audit.Event{RequestID: envReq.RequestID, RemoteSessionID: remote.ID, Workspace: remote.WorkspaceName, Tool: "execute", Command: spec.DisplayCommand, Status: "error", Detail: detail})
			response := envelope.Fail(envelope.StatusError, envReq.RequestID, remote.WorkspaceName, data, code, message)
			response.RemoteSessionID = remote.ID
			return r.resultJSON(response)
		}
		r.logAudit(audit.Event{RequestID: envReq.RequestID, RemoteSessionID: remote.ID, Workspace: remote.WorkspaceName, Tool: "execute", Command: spec.DisplayCommand, Status: "ok", Detail: detail})
		return compactToolResult(data, commandOutputText(ctx, data, fmt.Sprintf("AGY continuation completed with exit code %v.", data["exit_code"]))), nil
	}

	data["completed_in_call"] = false
	data["next_action"] = nextAction("observe", map[string]any{"remote_session_id": remote.ID, "view": "task", "execution_task_id": task.ID})
	data["logs_action"] = nextAction("observe", map[string]any{"remote_session_id": remote.ID, "view": "logs", "execution_task_id": task.ID, "stdout_offset": data["stdout_next_offset"], "stderr_offset": data["stderr_next_offset"]})
	data["summary"] = fmt.Sprintf("AGY continuation is running as Task %s.", task.ID)
	detail["execution_task_id"] = task.ID
	r.logAudit(audit.Event{RequestID: envReq.RequestID, RemoteSessionID: remote.ID, Workspace: remote.WorkspaceName, Tool: "execute", Command: spec.DisplayCommand, Status: "running", Detail: detail})
	response := envelope.Accepted(envReq.RequestID, remote.WorkspaceName, data)
	response.RemoteSessionID = remote.ID
	return r.resultJSON(response)
}

func agyContinuationSpecFromPayload(payload map[string]any) (agyContinuationSpec, error) {
	if _, exists := payload["dangerously_skip_permissions"]; exists {
		return agyContinuationSpec{}, fmt.Errorf("dangerously_skip_permissions is not supported by agy_continue")
	}
	resume := strings.ToLower(strings.TrimSpace(stringPayload(payload, "resume")))
	if resume != "conversation" && resume != "continue" {
		return agyContinuationSpec{}, fmt.Errorf("resume must be conversation or continue")
	}
	conversationID := strings.TrimSpace(stringPayload(payload, "conversation_id"))
	if resume == "conversation" {
		if !agyConversationIDPattern.MatchString(conversationID) {
			return agyContinuationSpec{}, fmt.Errorf("conversation_id must be a valid AGY UUID")
		}
	} else if conversationID != "" {
		return agyContinuationSpec{}, fmt.Errorf("conversation_id is only valid when resume=conversation")
	}
	prompt := stringPayload(payload, "prompt")
	if strings.TrimSpace(prompt) == "" {
		return agyContinuationSpec{}, fmt.Errorf("prompt is required")
	}
	if len(prompt) > agyContinuationMaxPromptBytes {
		return agyContinuationSpec{}, fmt.Errorf("prompt exceeds %d bytes", agyContinuationMaxPromptBytes)
	}
	model := strings.TrimSpace(stringPayload(payload, "model"))
	if model == "" {
		model = "gemini-3.7-flash"
	}
	if model != "gemini-3.7-flash" {
		return agyContinuationSpec{}, fmt.Errorf("model %q is not enabled for structured AGY continuation", model)
	}
	effort := strings.ToLower(strings.TrimSpace(stringPayload(payload, "effort")))
	if effort == "" {
		effort = "medium"
	}
	if effort != "low" && effort != "medium" && effort != "high" {
		return agyContinuationSpec{}, fmt.Errorf("effort must be low, medium, or high")
	}
	mode := strings.ToLower(strings.TrimSpace(stringPayload(payload, "mode")))
	if mode == "" {
		mode = "accept-edits"
	}
	if mode != "accept-edits" && mode != "plan" {
		return agyContinuationSpec{}, fmt.Errorf("mode must be accept-edits or plan")
	}
	promptDigest := sha256.Sum256([]byte(prompt))
	promptSHA256 := "sha256:" + hex.EncodeToString(promptDigest[:])
	digestInput, _ := json.Marshal(map[string]any{"resume": resume, "conversation_id": conversationID, "model": model, "effort": effort, "mode": mode, "prompt_sha256": promptSHA256})
	commandDigestBytes := sha256.Sum256(digestInput)
	commandDigest := "sha256:" + hex.EncodeToString(commandDigestBytes[:])
	display := fmt.Sprintf("agy %s%s --model %s --effort %s --mode %s --output-format json --print [prompt %s bytes=%d]", resumeFlag(resume), conversationFlag(conversationID), model, effort, mode, promptSHA256, len(prompt))
	return agyContinuationSpec{Resume: resume, ConversationID: conversationID, Prompt: prompt, Model: model, Effort: effort, Mode: mode, PromptSHA256: promptSHA256, DisplayCommand: display, CommandDigest: commandDigest}, nil
}

func resumeFlag(resume string) string {
	if resume == "continue" {
		return "--continue"
	}
	return "--conversation"
}

func conversationFlag(conversationID string) string {
	if conversationID == "" {
		return ""
	}
	return " " + conversationID
}

func agyContinuationArgs(spec agyContinuationSpec) []string {
	args := []string{resumeFlag(spec.Resume)}
	if spec.ConversationID != "" {
		args = append(args, spec.ConversationID)
	}
	return append(args, "--model", spec.Model, "--effort", spec.Effort, "--mode", spec.Mode, "--output-format", "json", "-p", spec.Prompt)
}

func agyContinuationWait(payload map[string]any) time.Duration {
	yield := intPayload(payload, "yield_time_ms")
	if yield <= 0 {
		return agyContinuationDefaultWait
	}
	wait := time.Duration(yield) * time.Millisecond
	if wait > agyContinuationMaxWait {
		return agyContinuationMaxWait
	}
	return wait
}

func agyContinuationData(spec agyContinuationSpec, workspacePath string) map[string]any {
	return map[string]any{"resume": spec.Resume, "conversation_id": spec.ConversationID, "model": spec.Model, "effort": spec.Effort, "mode": spec.Mode, "prompt_sha256": spec.PromptSHA256, "prompt_bytes": len(spec.Prompt), "dangerous_permissions": false, "working_directory": workspacePath, "workspace_scoped": true}
}

func agyContinuationDetail(purpose, scope string, spec agyContinuationSpec, commandPolicy map[string]any) map[string]any {
	detail := map[string]any{"purpose": purpose, "scope": scope, "command_digest": spec.CommandDigest, "command_policy": commandPolicy, "continuation": agyContinuationData(spec, "")}
	detail["agy_executable"] = "agy"
	return detail
}
