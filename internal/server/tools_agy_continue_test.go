package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/mcpresult"
	"mcpx/internal/remotesession"
)

func TestAGYContinuationSpecUsesStructuredDirectArgs(t *testing.T) {
	prompt := "inspect the current workspace; do not modify files"
	spec, err := agyContinuationSpecFromPayload(map[string]any{
		"resume":          "conversation",
		"conversation_id": "ac72aee8-f726-4c0f-ac2f-94747d6aebd5",
		"prompt":          prompt,
		"model":           "gemini-3.7-flash",
		"effort":          "medium",
		"mode":            "accept-edits",
	})
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{"--conversation", "ac72aee8-f726-4c0f-ac2f-94747d6aebd5", "--model", "gemini-3.7-flash", "--effort", "medium", "--mode", "accept-edits", "--output-format", "json", "-p", prompt}
	if got := agyContinuationArgs(spec); len(got) != len(wantArgs) {
		t.Fatalf("args=%v, want %v", got, wantArgs)
	} else {
		for i := range wantArgs {
			if got[i] != wantArgs[i] {
				t.Fatalf("args[%d]=%q, want %q; args=%v", i, got[i], wantArgs[i], got)
			}
		}
	}
	if strings.Contains(spec.DisplayCommand, prompt) || strings.Contains(spec.DisplayCommand, "dangerously-skip-permissions") {
		t.Fatalf("display command leaked prompt or dangerous flag: %s", spec.DisplayCommand)
	}
	if spec.PromptSHA256 == "" || spec.CommandDigest == "" {
		t.Fatalf("missing prompt/digest audit fields: %+v", spec)
	}

	continueSpec, err := agyContinuationSpecFromPayload(map[string]any{"resume": "continue", "prompt": "continue safely"})
	if err != nil {
		t.Fatal(err)
	}
	if got := agyContinuationArgs(continueSpec); got[0] != "--continue" || got[1] != "--model" {
		t.Fatalf("continue args=%v", got)
	}
}

func TestAGYContinuationSpecRejectsUnsafeOrAmbiguousInputs(t *testing.T) {
	base := map[string]any{"resume": "conversation", "conversation_id": "4e95dc70-ac8d-4de4-ad7b-fa7a736d03ba", "prompt": "smoke"}
	for name, change := range map[string]func(map[string]any){
		"invalid conversation id": func(p map[string]any) { p["conversation_id"] = "not-a-uuid" },
		"unsupported model":       func(p map[string]any) { p["model"] = "arbitrary-model" },
		"invalid effort":          func(p map[string]any) { p["effort"] = "x" },
		"invalid mode":            func(p map[string]any) { p["mode"] = "bypass" },
		"dangerous permissions":   func(p map[string]any) { p["dangerously_skip_permissions"] = true },
		"continue with id":        func(p map[string]any) { p["resume"] = "continue" },
	} {
		t.Run(name, func(t *testing.T) {
			payload := map[string]any{}
			for key, value := range base {
				payload[key] = value
			}
			change(payload)
			if _, err := agyContinuationSpecFromPayload(payload); err == nil {
				t.Fatalf("invalid payload was accepted: %+v", payload)
			}
		})
	}
}

func TestExecuteSchemaExposesAGYContinuationBranch(t *testing.T) {
	rt := &Runtime{}
	// registerTools only needs the catalog helpers; no external services are started.
	protocol := mcp.NewServer(&mcp.Implementation{Name: "mcpx-test", Version: "0.1.0"}, nil)
	rt.registerTools(protocol)
	registered := rt.listedToolMap()["execute"]
	var schema map[string]any
	if err := json.Unmarshal(mcpresult.ToolSchemaJSON(registered), &schema); err != nil {
		t.Fatal(err)
	}
	branches, _ := schema["oneOf"].([]any)
	for _, raw := range branches {
		branch, _ := raw.(map[string]any)
		properties, _ := branch["properties"].(map[string]any)
		action, _ := properties["action"].(map[string]any)
		if action["const"] != "agy_continue" {
			continue
		}
		for _, field := range []string{"resume", "conversation_id", "prompt", "model", "effort", "mode"} {
			if properties[field] == nil {
				t.Fatalf("agy_continue schema missing %q: %s", field, mcpresult.ToolSchemaJSON(registered))
			}
		}
		return
	}
	t.Fatalf("execute schema has no agy_continue branch: %s", mcpresult.ToolSchemaJSON(registered))
}

func TestAGYContinuationHandlerUsesWorkspaceAndRedactsPrompt(t *testing.T) {
	rt := newWorkspaceRuntime(t, "demo")
	stubDir := t.TempDir()
	stubPath := filepath.Join(stubDir, "agy")
	if runtime.GOOS == "windows" {
		stubPath += ".cmd"
		if err := os.WriteFile(stubPath, []byte("@echo off\necho {\"stub\":true}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	} else {
		if err := os.WriteFile(stubPath, []byte("#!/bin/sh\nprintf '%s\\n' '{\"stub\":true}'\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	}

	ctx := context.Background()
	principal, err := rt.principalFromContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	registered, _ := rt.reg.Get("demo")
	created, err := rt.remote.Create(ctx, principal, remotesession.CreateInput{WorkspaceName: "demo", WorkspacePath: registered.Path})
	if err != nil {
		t.Fatal(err)
	}
	request := mcpresult.Request(map[string]any{
		"action": "agy_continue", "remote_session_id": created.Session.ID,
		"purpose": "verify structured continuation", "resume": "conversation",
		"conversation_id": "ac72aee8-f726-4c0f-ac2f-94747d6aebd5", "prompt": "secret prompt must not be logged",
		"model": "gemini-3.7-flash", "effort": "medium", "mode": "plan", "yield_time_ms": 5000,
	})
	result, err := rt.toolAGYContinue(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	response := decodeToolResult(t, result)
	if response["status"] != "ok" {
		t.Fatalf("stub continuation response=%+v", response)
	}
	data, _ := response["data"].(map[string]any)
	if strings.Contains(fmt.Sprint(data["command"]), "secret prompt") {
		t.Fatalf("prompt leaked into display command: %+v", data)
	}
	continuation, _ := data["continuation"].(map[string]any)
	if continuation["working_directory"] != registered.Path || continuation["dangerous_permissions"] != false {
		t.Fatalf("continuation scope metadata=%+v", continuation)
	}
	observed := observationArguments("execute", map[string]any{"action": "agy_continue", "prompt": "secret prompt"})
	if strings.Contains(fmt.Sprint(observed["prompt"]), "secret prompt") {
		t.Fatalf("prompt leaked into observation arguments: %+v", observed)
	}
}
