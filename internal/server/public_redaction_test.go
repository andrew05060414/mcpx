package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/mcpresult"
)

func TestSanitizePublicToolResultRedactsSyntheticCredentialAtMCPBoundary(t *testing.T) {
	runtime := &Runtime{}
	result := &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: "upstream token=PX363_SYNTHETIC_SECRET_0123456789"},
			mcpresult.NewResourceLink("mcpx://artifact?token=PX363_SYNTHETIC_SECRET_0123456789", "artifact", "secret=PX363_SYNTHETIC_SECRET_0123456789", "text/plain"),
		},
		StructuredContent: map[string]any{
			"token":              "PX363_SYNTHETIC_SECRET_0123456789",
			"confirmation_token": "ct_keep_for_protocol",
			"safe":               "visible",
		},
		Meta: mcp.Meta{"provider": "fake", "api_key": "PX363_SYNTHETIC_SECRET_0123456789"},
	}

	clean := runtime.sanitizePublicToolResult(result)
	encoded, err := json.Marshal(clean)
	if err != nil {
		t.Fatal(err)
	}
	wire := string(encoded)
	if strings.Contains(wire, "PX363_SYNTHETIC_SECRET_0123456789") {
		t.Fatalf("synthetic credential leaked through MCPX: %s", wire)
	}
	if !strings.Contains(wire, "ct_keep_for_protocol") || !strings.Contains(wire, "visible") {
		t.Fatalf("safe protocol/data fields were lost: %s", wire)
	}
}

func TestInstrumentToolRedactsSyntheticCredentialBeforeMCPResponse(t *testing.T) {
	runtime := &Runtime{}
	handler := runtime.instrumentTool("redact_test", func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "token=PX363_SYNTHETIC_SECRET_0123456789"}},
			StructuredContent: map[string]any{
				"token":              "PX363_SYNTHETIC_SECRET_0123456789",
				"confirmation_token": "ct_keep_for_protocol",
			},
		}, nil
	})
	result, err := handler(context.Background(), mcpresult.Request(nil))
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "PX363_SYNTHETIC_SECRET_0123456789") {
		t.Fatalf("instrumented MCP response leaked synthetic credential: %s", wire)
	}
	if !strings.Contains(string(wire), "ct_keep_for_protocol") {
		t.Fatalf("instrumented MCP response lost confirmation handle: %s", wire)
	}
}
