package server

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestHistoryToolsAgainstRealChronicle 用真实的 chronicle 命令验证整条链路。
// 需要本机已装 chronicle 和 AgentsView，所以默认跳过：MCPX_REAL_CHRONICLE=1 go test ./internal/server -run RealChronicle -v
func TestHistoryToolsAgainstRealChronicle(t *testing.T) {
	if os.Getenv("MCPX_REAL_CHRONICLE") != "1" {
		t.Skip("set MCPX_REAL_CHRONICLE=1 to run against the installed chronicle")
	}
	runtime := newWorkspaceRuntime(t, "demo")

	list, err := callHistory(t, runtime, "history_list", `{"limit":2}`)
	if err != nil {
		t.Fatal(err)
	}
	sessions, _ := list["data"].(map[string]any)["sessions"].([]any)
	if len(sessions) == 0 {
		t.Fatalf("history_list returned no sessions: %v", list)
	}
	id, _ := sessions[0].(map[string]any)["id"].(string)
	if id == "" {
		t.Fatalf("session without id: %v", sessions[0])
	}

	if _, err := callHistory(t, runtime, "history_get", mustJSON(t, map[string]any{"conversation_id": id})); err != nil {
		t.Fatalf("history_get: %v", err)
	}
	shown, err := callHistory(t, runtime, "history_show", mustJSON(t, map[string]any{"conversation_id": id, "limit": 2}))
	if err != nil {
		t.Fatalf("history_show: %v", err)
	}
	if msgs, _ := shown["data"].(map[string]any)["messages"].([]any); len(msgs) == 0 || len(msgs) > 2 {
		t.Fatalf("history_show limit not honored: %d messages", len(msgs))
	}
	projects, err := callHistory(t, runtime, "history_projects", `{}`)
	if err != nil {
		t.Fatalf("history_projects: %v", err)
	}
	if encoded, _ := json.Marshal(projects); !strings.Contains(string(encoded), "session_count") {
		t.Fatalf("history_projects output unexpected: %s", encoded)
	}
	// 含 cmd.exe 元字符的查询必须原样到达搜索，不能被当成命令执行。
	for _, query := range []string{"chronicle & echo INJECTED", `"quoted" | more %PATH%`} {
		found, err := callHistory(t, runtime, "history_search", mustJSON(t, map[string]any{"query": query, "limit": 1}))
		if err != nil {
			t.Fatalf("history_search %q: %v", query, err)
		}
		if encoded, _ := json.Marshal(found); strings.Contains(string(encoded), "raw_output") && strings.Contains(string(encoded), "INJECTED") {
			t.Fatalf("query was interpreted by a shell: %s", encoded)
		}
	}
	anyResult, err := callHistory(t, runtime, "history_search", `{"any":["hstry 退役","chronicle 前门"],"limit":2}`)
	if err != nil {
		t.Fatalf("history_search any: %v", err)
	}
	if matches, _ := anyResult["data"].(map[string]any)["matches"].([]any); len(matches) == 0 {
		t.Fatalf("history_search any returned no matches: %v", anyResult)
	}
	t.Logf("verified with session %s", id)
}
