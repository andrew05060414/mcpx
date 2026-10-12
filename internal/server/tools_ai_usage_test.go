package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func liveAIUsageFixture() string {
	return `{
  "schema_version": "1.0",
  "kind": "ai_usage",
  "timestamp": "2026-09-18T14:57:46+08:00",
  "overall": "ok",
  "fix_first": null,
  "harnesses": [
    {
      "id": "opencode",
      "name": "OpenCode",
      "health": "ok",
      "can_work": true,
      "remaining": null,
      "remaining_unit": null,
      "reset_at": null,
      "burn": null,
      "burn_unit": null,
      "quota_status": "not_wired",
      "accounts_unhealthy": 0,
      "runtime_status": "ok",
      "probe_status": "ok",
      "host": "Arknights",
      "age_seconds": 245.4,
      "note": "runtime ok; probes ok; quota not collected"
    },
    {
      "id": "codex",
      "name": "Codex",
      "health": "ok",
      "can_work": true,
      "remaining": null,
      "remaining_unit": null,
      "reset_at": null,
      "burn": null,
      "burn_unit": null,
      "quota_status": "not_wired",
      "accounts_unhealthy": 0,
      "runtime_status": "ok",
      "probe_status": "ok",
      "host": "Arknights",
      "age_seconds": 245.4,
      "note": "runtime ok; probes ok; also oracle-arm:warn; quota not collected"
    },
    {
      "id": "claude",
      "name": "Claude",
      "health": "ok",
      "can_work": true,
      "remaining": null,
      "remaining_unit": null,
      "reset_at": null,
      "burn": null,
      "burn_unit": null,
      "quota_status": "not_wired",
      "accounts_unhealthy": 0,
      "runtime_status": "ok",
      "probe_status": "ok",
      "host": "Arknights",
      "age_seconds": 245.4,
      "note": "runtime ok; probes ok; also oracle-arm:warn; quota not collected"
    }
  ],
  "cockpit_usage": {
    "status": "reserved_phase2",
    "message": "Cockpit quota --json is not wired; remaining/reset/burn stay null until a collector sends real numbers.",
    "providers": []
  },
  "fleet": {
    "overall": "warn",
    "reporting_hosts": ["Arknights", "MEmini-B506", "oracle-arm"],
    "active_incidents": 5
  }
}`
}

func TestAIHealthBaseCandidatesPreferPort18081(t *testing.T) {
	t.Setenv("MCPX_AI_HEALTH_BASE_URL", "")
	got := aiHealthBaseCandidates("")
	if len(got) < 2 || got[0] != "http://192.168.0.102:18081" || got[1] != "http://192.168.0.102" {
		t.Fatalf("default bases = %v", got)
	}
	explicit := aiHealthBaseCandidates("http://192.168.0.102")
	if explicit[0] != "http://192.168.0.102:18081" || explicit[1] != "http://192.168.0.102" {
		t.Fatalf("explicit host without port = %v", explicit)
	}
}

func TestAIUsageGetStatusWrapsAiUsageJSONAsIs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("Cache-Control=%q", r.Header.Get("Cache-Control"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		switch r.URL.Path {
		case "/api/ai-usage":
			_, _ = w.Write([]byte(liveAIUsageFixture()))
		case "/api/ai-health":
			_, _ = w.Write([]byte(`{"schema_version":"1.0","harnesses":[],"hosts":{"too":"big"}}`))
		case "/api/status":
			_, _ = w.Write([]byte(`{"overall":"ok","services":[{},{}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	rt := &Runtime{}
	response := callEnvelope(t, rt.toolAIUsageGetStatus, context.Background(), map[string]any{"base_url": server.URL})
	if !statusOK(response) {
		t.Fatalf("ai_usage_get_status failed: %+v", response)
	}
	data, _ := response["data"].(map[string]any)
	if data["kind"] != "ai_usage" || data["overall"] != "ok" || data["schema_version"] != "1.0" {
		t.Fatalf("must wrap /api/ai-usage as-is: %+v", data)
	}
	if data["hosts"] != nil {
		t.Fatalf("must not dump ai-health hosts/probes: %+v", data["hosts"])
	}
	cockpit, _ := data["cockpit_usage"].(map[string]any)
	if cockpit["status"] != "reserved_phase2" {
		t.Fatalf("cockpit_usage=%+v", cockpit)
	}
	byName := map[string]map[string]any{}
	for _, item := range asMapSlice(data["harnesses"]) {
		byName[item["name"].(string)] = item
	}
	for _, name := range []string{"OpenCode", "Codex", "Claude"} {
		item := byName[name]
		if item["health"] != "ok" || item["can_work"] != true || item["host"] != "Arknights" {
			t.Fatalf("%s health contract: %+v", name, item)
		}
		if item["remaining"] != nil || item["reset_at"] != nil || item["burn"] != nil || item["quota_status"] != "not_wired" {
			t.Fatalf("%s invented quota: %+v", name, item)
		}
	}
	text := strings.ToLower(fmt.Sprint(response))
	if strings.Contains(text, "0% left") || strings.Contains(text, "0% remaining") {
		t.Fatalf("must not say remaining is 0: %s", text)
	}
}

func TestAIUsageGetStatusFallsBackToHealthHarnesses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/ai-usage":
			http.NotFound(w, r)
		case "/api/ai-health":
			_, _ = w.Write([]byte(`{
				"schema_version":"1.0",
				"overall":"ok",
				"harnesses":[{"id":"opencode","name":"OpenCode","health":"ok","can_work":true,"remaining":null,"reset_at":null,"burn":null,"quota_status":"not_wired"}],
				"cockpit_usage":{"status":"reserved_phase2"},
				"hosts":{"Arknights":{"probes":[{"name":"opencode"},{"name":"codex"}]}}
			}`))
		case "/api/status":
			_, _ = w.Write([]byte(`{"overall":"ok"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	rt := &Runtime{}
	response := callEnvelope(t, rt.toolAIUsageGetStatus, context.Background(), map[string]any{"base_url": server.URL})
	if !statusOK(response) {
		t.Fatalf("fallback failed: %+v", response)
	}
	data, _ := response["data"].(map[string]any)
	if data["kind"] != "ai_usage" || data["hosts"] != nil {
		t.Fatalf("fallback must use harnesses[] and omit probes: %+v", data)
	}
	items := asMapSlice(data["harnesses"])
	if len(items) != 1 || items[0]["remaining"] != nil || items[0]["quota_status"] != "not_wired" {
		t.Fatalf("fallback invented quota: %+v", items)
	}
}

func TestAIUsageGetStatusPassesThroughWiredQuota(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/ai-usage" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{
			"kind":"ai_usage","overall":"ok",
			"harnesses":[{"id":"codex","name":"Codex","health":"ok","can_work":true,"remaining":80,"remaining_unit":"percent","reset_at":"2026-09-19T00:00:00+08:00","burn":2.0,"burn_unit":"percent_per_hour","quota_status":"wired"}],
			"cockpit_usage":{"status":"reserved_phase2"}
		}`))
	}))
	t.Cleanup(server.Close)
	rt := &Runtime{}
	response := callEnvelope(t, rt.toolAIUsageGetStatus, context.Background(), map[string]any{"base_url": server.URL})
	item := asMapSlice(response["data"].(map[string]any)["harnesses"])[0]
	if item["quota_status"] != "wired" || item["remaining"] != float64(80) || item["burn"] != float64(2) {
		t.Fatalf("wired quota must pass through: %+v", item)
	}
}

func TestAIUsageGetStatusRefusesHTMLScrape(t *testing.T) {
	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!doctype html><html><body>quota 12%</body></html>"))
	}))
	t.Cleanup(html.Close)

	rt := &Runtime{}
	response := callEnvelope(t, rt.toolAIUsageGetStatus, context.Background(), map[string]any{"base_url": html.URL})
	if statusOK(response) {
		t.Fatalf("HTML scrape must fail: %+v", response)
	}
	errBody, _ := response["error"].(map[string]any)
	message := strings.ToLower(fmt.Sprint(errBody["message"]))
	if !strings.Contains(message, "html") || strings.Contains(message, "12%") {
		t.Fatalf("expected HTML refusal without scraped quota, got %+v", errBody)
	}
}
