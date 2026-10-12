package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/envelope"
	"mcpx/internal/mcpresult"
)

const (
	defaultAIHealthHost    = "http://192.168.0.102"
	defaultAIHealthPort    = "18081"
	aiHealthRequestTimeout = 8 * time.Second
	aiHealthMaxBodyBytes   = 2 << 20
)

var aiUsageHTTPClient = &http.Client{Timeout: aiHealthRequestTimeout}

func (r *Runtime) registerAIUsageTools(s *mcp.Server) {
	r.addToolIfAbsent(s, cleanCoreTool("ai_usage_get_status",
		"Read NAS AI Health Console GET /api/ai-usage and return that JSON as-is (OpenCode/Codex/Claude health, can_work, remaining/reset_at/burn only when the collector sent them). Does not scrape HTML and does not invent quota numbers. Default origin is http://192.168.0.102:18081; override with MCPX_AI_HEALTH_BASE_URL. Falls back to /api/ai-health harnesses[] if /api/ai-usage is missing.",
		map[string]any{
			"base_url": stringSchema("Optional AI Health Console origin, e.g. http://192.168.0.102:18081. When omitted, MCPX_AI_HEALTH_BASE_URL then the LAN default is used."),
		},
		nil,
		arcControlReadAnnotation,
	), r.toolAIUsageGetStatus)
}

func (r *Runtime) toolAIUsageGetStatus(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, parseErr := r.parseEnv(ctx, req)
	if parseErr != nil {
		return mcpresult.NewError(parseErr.Error()), nil
	}
	args := mcpresult.Arguments(req)
	bases := aiHealthBaseCandidates(stringArg(args, "base_url"))
	payload, err := fetchAIUsagePayload(ctx, bases)
	if err != nil {
		response := envelope.Fail(envelope.StatusError, envReq.RequestID, "", nil, "AI_HEALTH_UNAVAILABLE", redactSecretText(err.Error()))
		return r.resultJSON(response)
	}
	return compactToolResult(payload, aiUsageHumanSummary(payload)), nil
}

func aiHealthBaseCandidates(explicit string) []string {
	var raw []string
	if trimmed := strings.TrimSpace(explicit); trimmed != "" {
		raw = append(raw, trimmed)
	}
	if env := strings.TrimSpace(os.Getenv("MCPX_AI_HEALTH_BASE_URL")); env != "" {
		for _, part := range strings.Split(env, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				raw = append(raw, trimmed)
			}
		}
	}
	if len(raw) == 0 {
		raw = []string{defaultAIHealthHost + ":" + defaultAIHealthPort, defaultAIHealthHost}
	}
	seen := map[string]bool{}
	var out []string
	add := func(value string) {
		normalized := strings.TrimRight(strings.TrimSpace(value), "/")
		if normalized == "" || seen[normalized] {
			return
		}
		seen[normalized] = true
		out = append(out, normalized)
	}
	for _, candidate := range raw {
		if withPort, ok := withDefaultAIHealthPort(candidate); ok {
			add(withPort)
		}
		add(candidate)
	}
	return out
}

func withDefaultAIHealthPort(raw string) (string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return "", false
	}
	if parsed.Port() != "" {
		return "", false
	}
	parsed.Host = parsed.Hostname() + ":" + defaultAIHealthPort
	return strings.TrimRight(parsed.String(), "/"), true
}

func fetchAIUsagePayload(ctx context.Context, bases []string) (map[string]any, error) {
	var lastErr error
	for _, base := range bases {
		usage, _, usageErr := getJSONAPI(ctx, base+"/api/ai-usage")
		if usageErr == nil && looksLikeAIUsage(usage) {
			return passThroughAIUsage(usage), nil
		}
		if usageErr != nil {
			lastErr = usageErr
		} else {
			lastErr = fmt.Errorf("%s/api/ai-usage: JSON is not kind=ai_usage", base)
		}
		health, _, healthErr := getJSONAPI(ctx, base+"/api/ai-health")
		if healthErr == nil {
			if fallback, ok := usageFromHealthHarnesses(health); ok {
				return passThroughAIUsage(fallback), nil
			}
			lastErr = fmt.Errorf("%s/api/ai-health has no harnesses[]; need GET /api/ai-usage", base)
			continue
		}
		if lastErr == nil {
			lastErr = healthErr
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no AI Health Console origin responded with JSON")
	}
	return nil, lastErr
}

func looksLikeAIUsage(payload map[string]any) bool {
	if payload == nil {
		return false
	}
	if stringMap(payload, "kind") == "ai_usage" {
		return true
	}
	_, ok := payload["harnesses"].([]any)
	return ok
}

func usageFromHealthHarnesses(health map[string]any) (map[string]any, bool) {
	harnesses, ok := health["harnesses"].([]any)
	if !ok || len(harnesses) == 0 {
		return nil, false
	}
	out := map[string]any{
		"schema_version": firstNonEmpty(stringMap(health, "schema_version"), "1.0"),
		"kind":           "ai_usage",
		"timestamp":      health["timestamp"],
		"overall":        health["overall"],
		"fix_first":      health["fix_first"],
		"harnesses":      harnesses,
	}
	if cockpit, ok := health["cockpit_usage"]; ok {
		out["cockpit_usage"] = cockpit
	}
	if fleet, ok := health["fleet"]; ok {
		out["fleet"] = fleet
	}
	return out, true
}

func passThroughAIUsage(payload map[string]any) map[string]any {
	redacted, _ := redactJSONValue(payload).(map[string]any)
	if redacted == nil {
		redacted = map[string]any{}
	}
	return redacted
}

func redactJSONValue(value any) any {
	switch typed := value.(type) {
	case string:
		return redactSecretText(typed)
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, child := range typed {
			out[key] = redactJSONValue(child)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, child := range typed {
			out[i] = redactJSONValue(child)
		}
		return out
	default:
		return value
	}
}

func getJSONAPI(ctx context.Context, endpoint string) (map[string]any, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, endpoint, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cache-Control", "no-store")
	req.Header.Set("User-Agent", "mcpx-ai-usage/1")
	resp, err := aiUsageHTTPClient.Do(req)
	if err != nil {
		return nil, endpoint, err
	}
	defer resp.Body.Close()
	limited := io.LimitReader(resp.Body, aiHealthMaxBodyBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, endpoint, err
	}
	if len(body) > aiHealthMaxBodyBytes {
		return nil, endpoint, fmt.Errorf("%s: response too large", endpoint)
	}
	contentType := resp.Header.Get("Content-Type")
	if looksLikeHTML(contentType, body) {
		return nil, endpoint, fmt.Errorf("%s: got HTML, refusing to scrape; need harness JSON from /api/ai-usage", endpoint)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, endpoint, fmt.Errorf("%s: HTTP %d", endpoint, resp.StatusCode)
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
		return nil, endpoint, fmt.Errorf("%s: body is not JSON", endpoint)
	}
	var payload any
	if err := json.Unmarshal(trimmed, &payload); err != nil {
		return nil, endpoint, fmt.Errorf("%s: invalid JSON: %w", endpoint, err)
	}
	if asMap, ok := payload.(map[string]any); ok {
		return asMap, endpoint, nil
	}
	return map[string]any{"value": payload}, endpoint, nil
}

func looksLikeHTML(contentType string, body []byte) bool {
	ct := strings.ToLower(contentType)
	if strings.Contains(ct, "text/html") {
		return true
	}
	start := bytes.TrimSpace(body)
	if len(start) == 0 {
		return false
	}
	lower := bytes.ToLower(start)
	return bytes.HasPrefix(lower, []byte("<!doctype html")) || bytes.HasPrefix(lower, []byte("<html"))
}

func aiUsageHumanSummary(payload map[string]any) string {
	overall := firstNonEmpty(stringMap(payload, "overall"), "unknown")
	harnesses := harnessObjects(payload["harnesses"])
	notWired := true
	canWork := 0
	known := 0
	for _, harness := range harnesses {
		known++
		if harness["can_work"] == true {
			canWork++
		}
		if stringMap(harness, "quota_status") == "wired" || harness["remaining"] != nil || harness["reset_at"] != nil || harness["burn"] != nil {
			notWired = false
		}
	}
	cockpit, _ := payload["cockpit_usage"].(map[string]any)
	cockpitStatus := firstNonEmpty(stringMap(cockpit, "status"), "unknown")
	if notWired {
		return fmt.Sprintf("AI health overall=%s; %d/%d harnesses can_work; quota numbers are not collected yet (quota_status=not_wired, cockpit_usage=%s). Do not treat remaining as 0.", overall, canWork, known, cockpitStatus)
	}
	return fmt.Sprintf("AI health overall=%s; %d/%d harnesses can_work; cockpit_usage=%s", overall, canWork, known, cockpitStatus)
}

func harnessObjects(raw any) []map[string]any {
	items, _ := raw.([]any)
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if asMap, ok := item.(map[string]any); ok {
			out = append(out, asMap)
		}
	}
	return out
}

func stringMap(payload map[string]any, key string) string {
	if payload == nil {
		return ""
	}
	value, _ := payload[key].(string)
	return strings.TrimSpace(value)
}

func firstNumber(payload map[string]any, key string, fallback int) int {
	if payload == nil {
		return fallback
	}
	switch value := payload[key].(type) {
	case float64:
		return int(value)
	case int:
		return value
	case json.Number:
		if parsed, err := value.Int64(); err == nil {
			return int(parsed)
		}
	}
	return fallback
}
