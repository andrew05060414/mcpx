package server

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/approval"
	"mcpx/internal/browseruse"
	"mcpx/internal/mcpresult"
)

func TestBrowserServiceCommandMapsCommonActions(t *testing.T) {
	tests := []struct {
		name    string
		action  string
		payload map[string]any
		want    map[string]any
	}{
		{
			name:   "dom click preserves supported timeout",
			action: "click",
			payload: map[string]any{
				"tab_id": "7", "node_id": "42", "timeout_ms": 1500,
			},
			want: map[string]any{"type": "dom_cua_click", "tab_id": "7", "node_id": "42", "timeout_ms": float64(1500)},
		},
		{
			name:   "navigate preserves supported timeout",
			action: "navigate",
			payload: map[string]any{
				"tab_id": "7", "url": "https://example.com", "timeout_ms": 2500,
			},
			want: map[string]any{"type": "navigate_tab_url", "tab_id": "7", "url": "https://example.com", "timeout_ms": float64(2500)},
		},
		{
			name:    "back forces nonblocking upstream navigation",
			action:  "back",
			payload: map[string]any{"tab_id": "7", "timeout_ms": 15000},
			want:    map[string]any{"type": "navigate_tab_back", "tab_id": "7", "timeout_ms": float64(0)},
		},
		{
			name:    "forward forces nonblocking upstream navigation",
			action:  "forward",
			payload: map[string]any{"tab_id": "7"},
			want:    map[string]any{"type": "navigate_tab_forward", "tab_id": "7", "timeout_ms": float64(0)},
		},
		{
			name:   "coordinate click",
			action: "click",
			payload: map[string]any{
				"tab_id": "7", "x": 12, "y": 34, "button": 3, "keys": []any{"Shift"},
			},
			want: map[string]any{"type": "cua_click", "tab_id": "7", "x": float64(12), "y": float64(34), "button": float64(3), "keys": []string{"Shift"}},
		},
		{
			name:   "dom scroll",
			action: "scroll",
			payload: map[string]any{
				"tab_id": "7", "node_id": "9", "scroll_x": 0, "scroll_y": 500,
			},
			want: map[string]any{"type": "dom_cua_scroll", "tab_id": "7", "node_id": "9", "scroll_x": float64(0), "scroll_y": float64(500)},
		},
		{
			name:   "coordinate scroll",
			action: "scroll",
			payload: map[string]any{
				"tab_id": "7", "x": 100, "y": 200, "scroll_x": 0, "scroll_y": -300,
			},
			want: map[string]any{"type": "cua_scroll", "tab_id": "7", "x": float64(100), "y": float64(200), "scroll_x": float64(0), "scroll_y": float64(-300)},
		},
		{
			name:   "screenshot",
			action: "screenshot",
			payload: map[string]any{
				"tab_id": "7", "full_page": true, "crop_x": 1, "crop_y": 2, "crop_width": 300, "crop_height": 200,
			},
			want: map[string]any{
				"type": "tab_screenshot", "tab_id": "7", "fullPage": true,
				"cropX": float64(1), "cropY": float64(2), "cropWidth": float64(300), "cropHeight": float64(200),
			},
		},
		{
			name:   "drag",
			action: "drag",
			payload: map[string]any{
				"tab_id": "7", "path": []any{map[string]any{"x": 1, "y": 2}, map[string]any{"x": 3, "y": 4}},
			},
			want: map[string]any{
				"type": "cua_drag", "tab_id": "7",
				"path": []map[string]any{{"x": float64(1), "y": float64(2)}, {"x": float64(3), "y": float64(4)}},
			},
		},
		{
			name:   "official advanced command preserves supported timeout",
			action: "official",
			payload: map[string]any{
				"command": map[string]any{"type": "playwright_wait_for_load_state", "tab_id": "7", "timeout_ms": 1500},
			},
			want: map[string]any{"type": "playwright_wait_for_load_state", "tab_id": "7", "timeout_ms": 1500},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := browserServiceCommand(test.action, test.payload)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("command = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestBrowserServiceCommandRejectsInvalidMixedArguments(t *testing.T) {
	for _, test := range []struct {
		name    string
		action  string
		payload map[string]any
	}{
		{name: "click missing y", action: "click", payload: map[string]any{"tab_id": "1", "x": 1}},
		{name: "coordinate click timeout unsupported", action: "click", payload: map[string]any{"tab_id": "1", "x": 1, "y": 2, "timeout_ms": 100}},
		{name: "scroll missing y", action: "scroll", payload: map[string]any{"tab_id": "1", "x": 1, "scroll_x": 0, "scroll_y": 1}},
		{name: "invalid optional keys", action: "move", payload: map[string]any{"tab_id": "1", "x": 1, "y": 2, "keys": "Shift"}},
		{name: "partial crop", action: "screenshot", payload: map[string]any{"tab_id": "1", "crop_x": 1}},
		{name: "official missing command", action: "official", payload: map[string]any{}},
		{name: "official missing type", action: "official", payload: map[string]any{"command": map[string]any{"tab_id": "1"}}},
		{name: "official reserved list browsers", action: "official", payload: map[string]any{"command": map[string]any{"type": "list_browsers"}}},
		{name: "official browser id override", action: "official", payload: map[string]any{"command": map[string]any{"type": "get_tab", "browser_id": "other"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := browserServiceCommand(test.action, test.payload); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestBrowserConfirmationUsesLatestMatchingPrompt(t *testing.T) {
	store := approval.NewStore()
	runtime := &Runtime{approvals: store}
	old := time.Now().Add(-time.Minute)
	newer := old.Add(30 * time.Second)
	for _, item := range []approval.Pending{
		{Tool: "browser", PrincipalID: "user", RemoteSessionID: "session", CommandDigest: "digest", ConfirmationToken: "old", ContentKey: "old", CreatedAt: old},
		{Tool: "browser", PrincipalID: "user", RemoteSessionID: "session", CommandDigest: "digest", ConfirmationToken: "new", ContentKey: "new", CreatedAt: newer},
	} {
		if _, err := store.PutPending(item); err != nil {
			t.Fatal(err)
		}
	}
	pending, ok := runtime.pendingBrowserConfirmation("session", "user", "digest")
	if !ok || pending.ConfirmationToken != "new" {
		t.Fatalf("pending = %+v, ok=%v", pending, ok)
	}
}

func TestBrowserPublicSchemaAndRiskMetadata(t *testing.T) {
	runtime := &Runtime{}
	protocol := mcp.NewServer(&mcp.Implementation{Name: "mcpx-test", Version: "0.1.0"}, nil)
	runtime.registerTools(protocol)
	tool := runtime.listedToolMap()["browser"]
	if tool.Annotations == nil || tool.Annotations.ReadOnlyHint || tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint || tool.Annotations.IdempotentHint || tool.Annotations.OpenWorldHint == nil || !*tool.Annotations.OpenWorldHint {
		t.Fatalf("browser top-level annotations must conservatively cover external mutation: %+v", tool.Annotations)
	}
	var schema map[string]any
	if err := json.Unmarshal(mcpresult.ToolSchemaJSON(tool), &schema); err != nil {
		t.Fatal(err)
	}
	properties, _ := schema["properties"].(map[string]any)
	for _, field := range []string{"remote_session_id", "action", "browser_instance_id", "purpose", "user_confirmed", "tab_id", "url", "node_id", "text", "keys", "path", "full_page", "command"} {
		if properties[field] == nil {
			t.Fatalf("browser schema missing %q", field)
		}
	}
	if properties["timeout_ms"] == nil {
		t.Fatal("browser schema must expose timeout_ms for strong-typed actions whose official handlers support it")
	}
	actionSchema, _ := properties["action"].(map[string]any)
	actions, _ := actionSchema["enum"].([]any)
	for _, action := range []string{"status", "tabs", "claim", "agent_tabs", "get_tab", "create_tab", "close_tab", "navigate", "back", "forward", "reload", "snapshot", "click", "double_click", "type", "keypress", "scroll", "move", "drag", "screenshot", "official"} {
		if !containsSchemaRequired(actions, action) {
			t.Fatalf("browser action enum missing %q: %v", action, actions)
		}
	}
	risk, _ := tool.Meta["mcpx/action_risk"].(map[string]any)
	for _, action := range []string{"status", "tabs", "snapshot", "screenshot"} {
		entry, _ := risk[action].(map[string]any)
		if entry["read_only"] != true || entry["destructive"] != false || entry["open_world"] != true {
			t.Fatalf("browser %s risk=%+v", action, entry)
		}
	}
	for _, action := range []string{"navigate", "click", "type", "drag", "close_tab", "official"} {
		entry, _ := risk[action].(map[string]any)
		if entry["read_only"] != false || entry["destructive"] != true || entry["open_world"] != true {
			t.Fatalf("browser %s risk=%+v", action, entry)
		}
	}
}

func TestBrowserCommandDigestBindsBrowserAndCommand(t *testing.T) {
	first := browserCommandDigest("session", "browser-a", map[string]any{"type": "navigate_tab_url", "tab_id": "1", "url": "https://example.com"})
	same := browserCommandDigest("session", "browser-a", map[string]any{"url": "https://example.com", "tab_id": "1", "type": "navigate_tab_url"})
	changed := browserCommandDigest("session", "browser-b", map[string]any{"type": "navigate_tab_url", "tab_id": "1", "url": "https://example.com"})
	if first != same {
		t.Fatalf("stable digest changed: %q != %q", first, same)
	}
	if first == changed {
		t.Fatal("digest must bind browser instance")
	}
}

func TestBrowserNavigationTimeoutRecoveryRequiresObservedURLChange(t *testing.T) {
	timeout := &browseruse.ServiceError{Message: "Error: Timed out waiting for tab 7 to navigate to https://www.bing.com/."}
	if !browserNavigationTimedOut(timeout) {
		t.Fatal("expected navigation timeout to be recognized")
	}
	if browserNavigationTimedOut(&browseruse.ServiceError{Message: "unexpected browser service failure"}) {
		t.Fatal("unrelated error must not be treated as navigation timeout")
	}
	before := browserTabState{ID: "7", URL: "https://example.com/"}
	redirected := browserTabState{ID: "7", URL: "https://cn.bing.com/"}
	if !browserNavigationAdvanced(before, redirected) {
		t.Fatal("changed URL must allow timeout recovery")
	}
	if browserNavigationAdvanced(before, before) {
		t.Fatal("unchanged URL must not allow timeout recovery")
	}
	if browserNavigationAdvanced(browserTabState{}, redirected) {
		t.Fatal("missing pre-navigation URL must fail closed")
	}
}

func TestBrowserActionErrorCodeClassifiesExpectedStateErrors(t *testing.T) {
	for _, test := range []struct {
		name    string
		message string
		want    string
	}{
		{name: "stale dom node", message: "Error: DOM node 999999 is stale or missing", want: "browser_node_stale"},
		{name: "missing tab", message: "Error: Tab not found: 999999999. Existing tabs: 7|Example|https://example.com", want: "browser_tab_not_found"},
		{name: "missing browser instance", message: "Browser extension instance is not available: instance-x", want: "browser_not_found"},
		{name: "ambiguous browser", message: "Multiple extension browsers are available; browser_instance_id is required", want: "browser_ambiguous"},
		{name: "stale debugger attachment", message: "Error: Debugger unattached", want: "browser_attachment_stale"},
		{name: "unknown upstream failure", message: "unexpected browser service failure", want: "browser_action_failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := browserActionErrorCode(&browseruse.ServiceError{Message: test.message}); got != test.want {
				t.Fatalf("browserActionErrorCode(%q) = %q, want %q", test.message, got, test.want)
			}
		})
	}
}
