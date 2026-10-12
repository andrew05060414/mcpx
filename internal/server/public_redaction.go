package server

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/config"
	"mcpx/internal/observation"
)

// sanitizePublicToolResult is the final MCPX response boundary. Handlers may
// return upstream MCP content, local file data, or command output; all of it
// must pass the same outbound rules before it reaches the MCP client.
func (r *Runtime) sanitizePublicToolResult(result *mcp.CallToolResult) *mcp.CallToolResult {
	if result == nil {
		return nil
	}
	maxBytes := config.MaxResultBytes(r.cfg.Limits)
	if result.StructuredContent != nil {
		clean, _ := observation.SanitizePublic(result.StructuredContent, maxBytes)
		result.StructuredContent = clean
	}
	if result.Meta != nil {
		clean, _ := observation.SanitizePublic(map[string]any(result.Meta), maxBytes)
		if meta, ok := clean.(map[string]any); ok {
			result.Meta = mcp.Meta(meta)
		}
	}
	for _, content := range result.Content {
		switch typed := content.(type) {
		case *mcp.TextContent:
			if typed != nil {
				typed.Text, _ = observation.SanitizeText(typed.Text, maxBytes)
			}
		case *mcp.ResourceLink:
			if typed != nil {
				typed.URI, _ = observation.SanitizeText(typed.URI, maxBytes)
				typed.Name, _ = observation.SanitizeText(typed.Name, maxBytes)
				typed.Description, _ = observation.SanitizeText(typed.Description, maxBytes)
			}
		}
	}
	return result
}
