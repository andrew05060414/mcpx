package server

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestHistoryToolsRegistered(t *testing.T) {
	runtime := newWorkspaceRuntime(t, "demo")
	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "1.0.0"}, nil)
	runtime.registerTools(server)

	expectedTools := []string{
		"history_search",
		"history_peek",
		"history_show",
		"history_list",
		"history_stats",
	}

	toolMap := runtime.listedToolMap()
	for _, name := range expectedTools {
		if _, ok := toolMap[name]; !ok {
			t.Errorf("expected tool %q to be registered in tool catalog", name)
		}
	}
}

func TestHistoryToolValidation(t *testing.T) {
	runtime := newWorkspaceRuntime(t, "demo")
	ctx := context.Background()

	// search without query should fail validation
	_, err := runtime.toolHistorySearch(ctx, &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{
			Name:      "history_search",
			Arguments: []byte(`{}`),
		},
	})
	if err == nil {
		t.Errorf("expected error when query is missing in history_search")
	}

	// peek without conversation_id should fail validation
	_, err = runtime.toolHistoryPeek(ctx, &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{
			Name:      "history_peek",
			Arguments: []byte(`{}`),
		},
	})
	if err == nil {
		t.Errorf("expected error when conversation_id is missing in history_peek")
	}

	// show without conversation_id should fail validation
	_, err = runtime.toolHistoryShow(ctx, &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{
			Name:      "history_show",
			Arguments: []byte(`{}`),
		},
	})
	if err == nil {
		t.Errorf("expected error when conversation_id is missing in history_show")
	}
}
