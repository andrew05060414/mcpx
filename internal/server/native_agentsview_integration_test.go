package server

import (
	"context"
	"encoding/json"
	"mcpx/internal/config"
	"os"
	"strings"
	"testing"
	"time"
)

func TestNativeAgentsViewIntegration(t *testing.T) {
	launcher, python := os.Getenv("MCPX_MEMORY_TEST_LAUNCHER"), os.Getenv("MCPX_MEMORY_TEST_PYTHON")
	if launcher == "" || python == "" {
		t.Skip("set MCPX_MEMORY_TEST_LAUNCHER and MCPX_MEMORY_TEST_PYTHON for read-only history verification")
	}
	rt := newWorkspaceRuntime(t, "demo")
	rt.cfg.Discovery.MCP.Enabled = true
	workspace, _ := rt.reg.Get("demo")
	if err := config.WriteMCPFile(config.ProjectMCPPath(workspace.Path), config.MCPFile{MCPServers: map[string]config.MCPServer{
		"agentsview-memory": {Command: python, Args: []string{"-I", launcher}},
	}}); err != nil {
		t.Fatal(err)
	}
	opened := callEnvelope(t, rt.toolSession, context.Background(), map[string]any{"action": "open", "workspace": "demo"})
	id := opened["remote_session_id"]
	call := func(name string, args map[string]any) map[string]any {
		t.Helper()
		start := time.Now()
		res := callRawToolResult(t, rt.toolHandlers["mcp_tool"], map[string]any{"action": "call", "remote_session_id": id, "server": "agentsview-memory", "tool": name, "arguments": args, "purpose": "read-only integration verification"})
		if res.IsError {
			t.Fatalf("%s failed (error details suppressed)", name)
		}
		b, _ := json.Marshal(res.StructuredContent)
		var body map[string]any
		if err := json.Unmarshal(b, &body); err != nil {
			t.Fatal(err)
		}
		if status, _ := body["status"].(string); status == "error" || status == "needs_confirmation" || status == "denied" {
			t.Fatalf("%s did not return upstream data: status=%s code=%v", name, status, body["error"])
		}
		t.Logf("%s %dms, %d structured bytes, no confirmation", name, time.Since(start).Milliseconds(), len(b))
		return body
	}
	if call("get_memory_status", map[string]any{})["archive"] == nil {
		t.Fatal("missing archive status")
	}
	page := call("search_content", map[string]any{"pattern": `"pxread" OR "压缩"`, "mode": "fts", "limit": 3})
	matches, _ := page["matches"].([]any)
	if len(matches) == 0 {
		t.Fatal("empty real search")
	}
	hit := matches[0].(map[string]any)
	if hit["agent"] == nil || hit["timestamp"] == nil {
		t.Fatal("missing provenance")
	}
	shown := call("get_messages", map[string]any{"session_id": hit["session_id"], "limit": 5})
	b, _ := json.Marshal(shown)
	if strings.Contains(string(b), `"tool_calls"`) || len(b) > 100000 {
		t.Fatal("unexpected raw transcript payload")
	}
	if rows, _ := shown["messages"].([]any); len(rows) == 0 {
		t.Fatal("no dialogue")
	}
}
