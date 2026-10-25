package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestHistoryRejectsInvalidPagination(t *testing.T) {
	for _, value := range []any{-1.0, 1.9, "1"} {
		if _, err := buildHistorySearchArgs(map[string]any{"query": "reader", "cursor": value}); err == nil {
			t.Fatal("invalid cursor accepted")
		}
		for _, key := range []string{"from", "around", "before", "after"} {
			if _, err := buildHistoryMessagesArgs(map[string]any{"conversation_id": "session", "role": "user,assistant", key: value}); err == nil {
				t.Fatalf("invalid %s accepted", key)
			}
		}
	}
}

func TestHistoryDialogueDropsToolsAndBoundsUnicode(t *testing.T) {
	input := map[string]any{"messages": []any{
		map[string]any{"role": "assistant", "content": strings.Repeat("中", 2001), "ordinal": 9, "timestamp": "2026-01-01T00:00:00Z", "tool_calls": []any{map[string]any{"input_json": "huge"}}},
		map[string]any{"role": "tool", "content": "payload"},
	}, "next_from": 11}
	value, err := historyDialogue(input)
	if err != nil {
		t.Fatal(err)
	}
	object := value.(map[string]any)
	rows := object["messages"].([]any)
	if len(rows) != 1 || object["next_from"] != 11 {
		t.Fatal("dialogue projection lost pagination")
	}
	msg := rows[0].(map[string]any)
	if len([]rune(msg["content"].(string))) != 2000 || msg["truncated"] != true {
		t.Fatal("unicode truncation failed")
	}
	b, _ := json.Marshal(value)
	if strings.Contains(string(b), "tool_calls") || strings.Contains(string(b), "huge") {
		t.Fatal("raw tool payload escaped")
	}
	if _, err := historyDialogue(map[string]any{"truncated": true, "raw_output": "tool dump"}); err == nil {
		t.Fatal("unstructured output must fail closed")
	}
}
