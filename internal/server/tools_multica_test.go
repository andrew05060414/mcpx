package server

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMulticaToolsRegistered(t *testing.T) {
	runtime := newWorkspaceRuntime(t, "demo")
	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "1.0.0"}, nil)
	runtime.registerTools(server)

	expectedTools := []string{
		"multica_list_agents",
		"multica_list_projects",
		"multica_dispatch_task",
		"multica_get_issue",
		"multica_comment_issue",
		"multica_list_runs",
		"multica_get_run_messages",
		"multica_rerun_task",
		"multica_cancel_task",
	}

	toolMap := runtime.listedToolMap()
	for _, name := range expectedTools {
		if _, ok := toolMap[name]; !ok {
			t.Errorf("expected tool %q to be registered in tool catalog", name)
		}
	}
}

func TestMulticaToolValidation(t *testing.T) {
	runtime := newWorkspaceRuntime(t, "demo")
	ctx := context.Background()

	// dispatch without title should fail validation
	_, err := runtime.toolMulticaDispatchTask(ctx, &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{
			Name:      "multica_dispatch_task",
			Arguments: []byte(`{}`),
		},
	})
	if err == nil {
		t.Errorf("expected error when title is missing in dispatch_task")
	}

	// get_issue without issue_id should fail validation
	_, err = runtime.toolMulticaGetIssue(ctx, &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{
			Name:      "multica_get_issue",
			Arguments: []byte(`{}`),
		},
	})
	if err == nil {
		t.Errorf("expected error when issue_id is missing in get_issue")
	}

	// comment_issue without content should fail validation
	_, err = runtime.toolMulticaCommentIssue(ctx, &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{
			Name:      "multica_comment_issue",
			Arguments: []byte(`{"issue_id": "PX-1"}`),
		},
	})
	if err == nil {
		t.Errorf("expected error when content is missing in comment_issue")
	}
}
