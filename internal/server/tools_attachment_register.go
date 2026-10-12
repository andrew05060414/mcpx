package server

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var (
	arcControlReadAnnotation = toolAnnotation{
		ReadOnly: true, Destructive: false, Idempotent: true, OpenWorld: true,
		Title: "Arc control-plane read",
	}
	arcControlWriteAnnotation = toolAnnotation{
		ReadOnly: false, Destructive: true, Idempotent: false, OpenWorld: true,
		Title: "Arc control-plane write (confirmation required)",
	}
)

var (
	reBearerToken   = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9\-._~+/]+=*`)
	reGitHubToken   = regexp.MustCompile(`(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9_]{20,}`)
	reGitHubFinePAT = regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`)
	reJWTToken      = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]+`)
	reGenericSecret = regexp.MustCompile(`(?i)\b(?:sk-|api[_-]?key[_=-]|token[_=-]|secret[_=-])[A-Za-z0-9+/=_\-.]{12,}`)
	reSecretEnvName = regexp.MustCompile(`(?i)(token|secret|password|passwd|api[_-]?key|authorization|credential|pat)`)
)

func (r *Runtime) registerArcControlPlaneTools(s *mcp.Server) {
	r.registerAIUsageTools(s)
	r.registerMulticaControlTools(s)
}

func (r *Runtime) addToolIfAbsent(s *mcp.Server, tool mcp.Tool, handler mcp.ToolHandler) {
	if r == nil || strings.TrimSpace(tool.Name) == "" {
		return
	}
	r.toolIndexMu.RLock()
	existing, exists := r.toolIndex[tool.Name]
	existingHandler := r.toolHandlers[tool.Name]
	r.toolIndexMu.RUnlock()
	if exists && existingHandler != nil {
		// New() snapshots the catalog onto a throwaway server. Re-bind the
		// already-chosen handler (local tools_multica.go wins on first register)
		// onto this MCP server instead of dropping the tool from tools/list.
		tt := existing
		s.AddTool(&tt, existingHandler)
		return
	}
	r.addTool(s, tool, handler)
}

func redactSecretText(value string) string {
	if value == "" {
		return value
	}
	out := reBearerToken.ReplaceAllString(value, "Bearer [REDACTED]")
	out = reGitHubFinePAT.ReplaceAllString(out, "[REDACTED_GITHUB_PAT]")
	out = reGitHubToken.ReplaceAllString(out, "[REDACTED_GITHUB_TOKEN]")
	out = reJWTToken.ReplaceAllString(out, "[REDACTED_JWT]")
	out = reGenericSecret.ReplaceAllString(out, "[REDACTED_SECRET]")
	return out
}

func envLooksSecret(name string) bool {
	return reSecretEnvName.MatchString(name)
}

func boolLike(payload map[string]any, key string) bool {
	if payload == nil {
		return false
	}
	switch value := payload[key].(type) {
	case bool:
		return value
	case string:
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "1", "true", "yes", "on":
			return true
		}
	case float64:
		return value != 0
	case json.Number:
		n, err := value.Int64()
		return err == nil && n != 0
	}
	return false
}

func stringArg(payload map[string]any, key string) string {
	if payload == nil {
		return ""
	}
	value, _ := payload[key].(string)
	return strings.TrimSpace(value)
}

func intArg(payload map[string]any, key string, fallback int) int {
	if payload == nil {
		return fallback
	}
	switch value := payload[key].(type) {
	case float64:
		if int(value) > 0 {
			return int(value)
		}
	case int:
		if value > 0 {
			return value
		}
	case json.Number:
		if parsed, err := value.Int64(); err == nil && parsed > 0 {
			return int(parsed)
		}
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		if err == nil && parsed > 0 {
			return parsed
		}
	}
	return fallback
}
