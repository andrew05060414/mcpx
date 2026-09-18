package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestAddToolIfAbsentKeepsExistingHandler(t *testing.T) {
	rt := &Runtime{}
	protocol := mcp.NewServer(&mcp.Implementation{Name: "mcpx-test", Version: "0.1.0"}, nil)
	original := func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return compactToolResult(map[string]any{"kept": true}, "kept local"), nil
	}
	replacement := func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return compactToolResult(map[string]any{"kept": false}, "replaced"), nil
	}
	rt.addTool(protocol, cleanCoreTool("multica_list_executions", "local", map[string]any{}, nil, arcControlReadAnnotation), original)
	rt.addToolIfAbsent(protocol, cleanCoreTool("multica_list_executions", "pr overlay", map[string]any{}, nil, arcControlReadAnnotation), replacement)
	if rt.listedToolMap()["multica_list_executions"].Description != "local" {
		t.Fatalf("local tool description was overwritten: %+v", rt.listedToolMap()["multica_list_executions"])
	}
	second := mcp.NewServer(&mcp.Implementation{Name: "mcpx-test-2", Version: "0.1.0"}, nil)
	rt.addToolIfAbsent(second, cleanCoreTool("multica_list_executions", "pr overlay", map[string]any{}, nil, arcControlReadAnnotation), replacement)
	if rt.listedToolMap()["multica_list_executions"].Description != "local" {
		t.Fatalf("resnapshot must keep the first handler: %+v", rt.listedToolMap()["multica_list_executions"])
	}
}

func TestRedactSecretTextHidesTokens(t *testing.T) {
	raw := "Authorization: Bearer ghp_abcdefghijklmnopqrstuvwxyz0123456789 jwt=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.abc github_pat_abcdefghijklmnopqrstuvwxyz"
	got := redactSecretText(raw)
	if strings.Contains(got, "ghp_") || strings.Contains(got, "github_pat_") || strings.Contains(got, "eyJhbGci") {
		t.Fatalf("token leaked: %s", got)
	}
}

func TestMulticaListExecutionsUsesCLIJSON(t *testing.T) {
	restoreControlCLI(t)
	controlLookPath = func(file string) (string, error) { return "/bin/" + file, nil }
	controlRunCLI = func(_ context.Context, bin string, argv []string) (controlCLIResult, error) {
		if bin != "/bin/multica" || strings.Join(argv[:3], " ") != "issue list --output" {
			t.Fatalf("unexpected argv %s %v", bin, argv)
		}
		return controlCLIResult{
			Args:     append([]string{bin}, argv...),
			Stdout:   `[{"id":"iss-1","identifier":"TEC-42","status":"in_progress","title":"[GH andrew05060414/pxread#677]"}]`,
			ExitCode: 0,
		}, nil
	}
	rt := &Runtime{}
	response := callEnvelope(t, rt.toolMulticaListExecutions, context.Background(), map[string]any{"limit": 20})
	if !statusOK(response) {
		t.Fatalf("list executions: %+v", response)
	}
	data, _ := response["data"].(map[string]any)
	items := asMapSlice(data["executions"])
	if len(items) != 1 || items[0]["identifier"] != "TEC-42" {
		t.Fatalf("executions=%+v", items)
	}
}

func TestMulticaWriteToolsRequireConfirmationAndSkipCLI(t *testing.T) {
	restoreControlCLI(t)
	called := 0
	controlLookPath = func(file string) (string, error) { return "/bin/" + file, nil }
	controlRunCLI = func(context.Context, string, []string) (controlCLIResult, error) {
		called++
		return controlCLIResult{}, errors.New("should not run")
	}
	rt := &Runtime{}
	response := callEnvelope(t, rt.toolMulticaAssignExecution, context.Background(), map[string]any{
		"execution_id": "iss-1", "assignee": "sp-engineer",
	})
	if response["status"] != "waiting_confirmation" {
		t.Fatalf("assign must wait for confirmation: %+v", response)
	}
	if called != 0 {
		t.Fatalf("CLI ran before confirmation: %d", called)
	}
}

func TestMulticaAssignExecutionRunsAfterConfirm(t *testing.T) {
	restoreControlCLI(t)
	controlLookPath = func(file string) (string, error) { return "/bin/" + file, nil }
	controlRunCLI = func(_ context.Context, _ string, argv []string) (controlCLIResult, error) {
		return controlCLIResult{
			Args:     append([]string{"multica"}, argv...),
			Stdout:   `{"id":"iss-1","assignee":"sp-engineer","status":"in_progress"}`,
			ExitCode: 0,
		}, nil
	}
	rt := &Runtime{}
	response := callEnvelope(t, rt.toolMulticaAssignExecution, context.Background(), map[string]any{
		"execution_id": "iss-1", "assignee": "sp-engineer", "user_confirmed": true,
	})
	if !statusOK(response) {
		t.Fatalf("assign after confirm: %+v", response)
	}
}

func TestGitHubCommentResolvesLinkedIssueAndRedactsTokens(t *testing.T) {
	restoreControlCLI(t)
	controlLookPath = func(file string) (string, error) { return "/bin/" + file, nil }
	controlRunCLI = func(_ context.Context, bin string, argv []string) (controlCLIResult, error) {
		joined := strings.Join(append([]string{bin}, argv...), " ")
		if strings.Contains(bin, "multica") {
			return controlCLIResult{
				Args: append([]string{bin}, argv...),
				Stdout: `{
					"id":"iss-1",
					"metadata":{"github_repo":"andrew05060414/pxread","github_issue_number":677},
					"title":"close the loop"
				}`,
			}, nil
		}
		if strings.Contains(joined, "issue comment") && strings.Contains(joined, "--repo andrew05060414/pxread") {
			return controlCLIResult{Args: append([]string{bin}, argv...), Stdout: `https://github.com/andrew05060414/pxread/issues/677#issuecomment-1`}, nil
		}
		return controlCLIResult{Args: append([]string{bin}, argv...), Stderr: "Authorization: Bearer ghp_abcdefghijklmnopqrstuvwxyz012345", ExitCode: 1}, fmt.Errorf("gh failed token=ghp_abcdefghijklmnopqrstuvwxyz012345")
	}
	rt := &Runtime{}
	response := callEnvelope(t, rt.toolGitHubComment, context.Background(), map[string]any{
		"execution_id": "iss-1", "body": "shipped from MCPX", "user_confirmed": true,
	})
	if !statusOK(response) {
		t.Fatalf("github_comment: %+v", response)
	}
	encoded, _ := json.Marshal(response)
	if strings.Contains(string(encoded), "ghp_") {
		t.Fatalf("token leaked into model result: %s", encoded)
	}
	data, _ := response["data"].(map[string]any)
	if data["repo"] != "andrew05060414/pxread" || data["number"] != float64(677) {
		t.Fatalf("resolved github target=%+v", data)
	}
}

func TestControlCLIErrorRedactsSecrets(t *testing.T) {
	restoreControlCLI(t)
	controlLookPath = func(file string) (string, error) { return "/bin/" + file, nil }
	controlRunCLI = func(_ context.Context, bin string, argv []string) (controlCLIResult, error) {
		return controlCLIResult{
			Args:     append([]string{bin, "--token", "ghp_abcdefghijklmnopqrstuvwxyz012345"}, argv...),
			Stderr:   "fatal: token ghp_abcdefghijklmnopqrstuvwxyz012345 rejected",
			ExitCode: 1,
		}, fmt.Errorf("token ghp_abcdefghijklmnopqrstuvwxyz012345 rejected")
	}
	rt := &Runtime{}
	response := callEnvelope(t, rt.toolMulticaListAgents, context.Background(), map[string]any{})
	if statusOK(response) {
		t.Fatalf("expected CLI failure: %+v", response)
	}
	encoded, _ := json.Marshal(response)
	if strings.Contains(string(encoded), "ghp_") {
		t.Fatalf("token leaked: %s", encoded)
	}
}

func restoreControlCLI(t *testing.T) {
	t.Helper()
	origLook, origRun := controlLookPath, controlRunCLI
	t.Cleanup(func() {
		controlLookPath = origLook
		controlRunCLI = origRun
	})
}
