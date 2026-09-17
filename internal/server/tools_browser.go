package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/approval"
	"mcpx/internal/browseruse"
	"mcpx/internal/envelope"
	"mcpx/internal/remotesession"
)

func (r *Runtime) toolBrowser(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, principal, session, fail := r.changeRequest(ctx, req, false)
	if fail != nil {
		return fail, nil
	}
	if session.Role != "owner" && session.Role != "editor" {
		return r.remoteError(envReq, session.ID, session.WorkspaceName, remotesession.ErrForbidden)
	}
	action := strings.TrimSpace(stringPayload(envReq.Payload, "action"))
	selector := strings.TrimSpace(stringPayload(envReq.Payload, "browser_instance_id"))

	discovery, err := browseruse.InspectOfficial(ctx)
	if err != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "browser_unavailable", err.Error())
	}
	backends := browseruse.FilterBackends(discovery.Backends, selector)

	if action == "status" {
		items := make([]map[string]any, 0, len(backends))
		for _, backend := range backends {
			items = append(items, publicBrowserBackend(backend))
		}
		installations := make([]map[string]any, 0, len(discovery.Installations))
		for _, installation := range discovery.Installations {
			installations = append(installations, publicBrowserInstallation(installation))
		}
		state := discovery.State()
		connected := len(backends) > 0
		message := "OpenAI browser extension was not detected; install the official ChatGPT browser extension"
		if len(discovery.Installations) > 0 {
			message = "OpenAI browser extension is installed but not connected; open the browser and ensure the extension is enabled"
		}
		if connected {
			state = "connected"
			message = fmt.Sprintf("OpenAI browser extension connected (%d browser(s))", len(items))
		} else if selector != "" {
			state = "installed_disconnected"
			message = fmt.Sprintf("OpenAI browser extension instance %q is not connected", selector)
		}
		return compactToolResult(map[string]any{
			"provider":           "openai_official_extension",
			"state":              state,
			"connected":          connected,
			"installed":          len(discovery.Installations) > 0,
			"browsers":           items,
			"browser_count":      len(items),
			"installations":      installations,
			"installation_count": len(installations),
			"pipe_count":         discovery.PipeCount,
			"help_url":           browseruse.OfficialBrowserHelpURL,
			"message":            message,
		}, message), nil
	}

	if len(backends) == 0 {
		message := "OpenAI official browser extension is not installed or not detected; install the official ChatGPT browser extension"
		if len(discovery.Installations) > 0 {
			message = "OpenAI official browser extension is installed but not connected; open the browser and ensure the extension is enabled"
		}
		if selector != "" {
			message = fmt.Sprintf("OpenAI browser extension instance %q is not connected", selector)
		}
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "browser_not_found", message)
	}

	if action == "tabs" {
		return r.browserUserTabs(ctx, envReq, session.ID, session.WorkspaceName, backends)
	}

	if err := validatePurpose(envReq.Intent); err != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "PURPOSE_REQUIRED", err.Error())
	}
	if len(backends) != 1 {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "browser_ambiguous", "multiple official browser instances are connected; set browser_instance_id")
	}
	command, err := browserServiceCommand(action, envReq.Payload)
	if err != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "bad_request", err.Error())
	}
	backend := backends[0]
	instanceID := backend.Info.Metadata.ExtensionInstanceID
	digest := browserCommandDigest(session.ID, instanceID, command)
	pending, pendingOK := r.pendingBrowserConfirmation(session.ID, principal.ID, digest)
	approvedFingerprints := []string(nil)
	usedPending := false
	if boolPayload(envReq.Payload, "user_confirmed") && pendingOK && strings.TrimSpace(pending.ConfirmationToken) != "" {
		approvedFingerprints = []string{pending.ConfirmationToken}
		usedPending = true
	}
	if r.browserService == nil {
		r.browserService = browseruse.NewService()
	}
	serviceResponse, serviceErr := r.browserService.Execute(ctx, browseruse.ServiceRequest{
		SessionID:            browserServiceSessionID(session.ID),
		TurnID:               browserServiceTurnID(session.ID),
		BrowserInstanceID:    instanceID,
		Command:              command,
		ApprovedFingerprints: approvedFingerprints,
	})
	if serviceErr != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, "browser_service_failed", serviceErr.Error())
	}
	if usedPending {
		_, _ = r.approvals.Consume(pending.ID)
	}
	if len(serviceResponse.Elicitations) > 0 {
		return r.browserConfirmationRequired(envReq, principal.ID, session.ID, session.WorkspaceName, instanceID, action, digest, serviceResponse.Elicitations[0])
	}
	if serviceResponse.Error != nil {
		return r.terminalError(envReq, session.ID, session.WorkspaceName, browserActionErrorCode(serviceResponse.Error), serviceResponse.Error.Message)
	}
	data := map[string]any{
		"provider": "openai_official_browser_service",
		"action":   action,
		"browser":  publicBrowserBackend(backend),
		"result":   serviceResponse.Result,
	}
	if len(serviceResponse.ContentItems) > 0 {
		data["notifications"] = serviceResponse.ContentItems
	}
	if len(serviceResponse.ResponseMeta) > 0 {
		data["response_meta"] = serviceResponse.ResponseMeta
	}
	return compactToolResult(data, "browser action completed: "+action), nil
}

func (r *Runtime) browserUserTabs(ctx context.Context, envReq envelope.Request, remoteID, workspace string, backends []browseruse.Backend) (*mcp.CallToolResult, error) {
	groups := make([]map[string]any, 0, len(backends))
	total := 0
	for _, backend := range backends {
		tabs, tabErr := (browseruse.Client{Pipe: backend.Pipe}).UserTabs(ctx)
		if tabErr != nil {
			return r.terminalError(envReq, remoteID, workspace, "browser_tabs_failed", tabErr.Error())
		}
		publicTabs := make([]map[string]any, 0, len(tabs))
		for _, tab := range tabs {
			item := map[string]any{"id": string(tab.ID)}
			if tab.ProviderTabID != "" {
				item["provider_tab_id"] = tab.ProviderTabID
			}
			if tab.Title != "" {
				item["title"] = tab.Title
			}
			if tab.URL != "" {
				item["url"] = tab.URL
			}
			if tab.LastOpened != "" {
				item["last_opened"] = tab.LastOpened
			}
			if tab.TabGroup != "" {
				item["tab_group"] = tab.TabGroup
			}
			publicTabs = append(publicTabs, item)
		}
		total += len(publicTabs)
		groups = append(groups, map[string]any{
			"browser": publicBrowserBackend(backend),
			"tabs":    publicTabs,
			"count":   len(publicTabs),
		})
	}
	return compactToolResult(map[string]any{
		"provider": "openai_official_extension",
		"groups":   groups,
		"count":    total,
	}, fmt.Sprintf("listed %d browser tab(s)", total)), nil
}

func browserActionErrorCode(serviceError *browseruse.ServiceError) string {
	if serviceError == nil {
		return "browser_action_failed"
	}
	message := strings.ToLower(strings.TrimSpace(serviceError.Message))
	switch {
	case strings.Contains(message, "dom node") && (strings.Contains(message, "stale") || strings.Contains(message, "missing")):
		return "browser_node_stale"
	case strings.Contains(message, "tab not found"):
		return "browser_tab_not_found"
	case strings.Contains(message, "browser extension instance is not available"), strings.Contains(message, "no openai extension browser is available"):
		return "browser_not_found"
	case strings.Contains(message, "multiple extension browsers are available"):
		return "browser_ambiguous"
	default:
		return "browser_action_failed"
	}
}

func browserServiceCommand(action string, payload map[string]any) (map[string]any, error) {
	command := map[string]any{}
	tabID := func() (string, error) {
		value := strings.TrimSpace(stringPayload(payload, "tab_id"))
		if value == "" {
			return "", fmt.Errorf("%s requires tab_id", action)
		}
		return value, nil
	}
	withTab := func(commandType string) (map[string]any, error) {
		value, err := tabID()
		if err != nil {
			return nil, err
		}
		result := map[string]any{"type": commandType, "tab_id": value}
		if timeout, ok := browserNumber(payload, "timeout_ms"); ok {
			result["timeout_ms"] = timeout
		}
		return result, nil
	}

	switch action {
	case "claim":
		return withTab("browser_user_claim_tab")
	case "agent_tabs":
		return map[string]any{"type": "list_tabs"}, nil
	case "get_tab":
		return withTab("get_tab")
	case "create_tab":
		return map[string]any{"type": "create_tab"}, nil
	case "close_tab":
		return withTab("close_tab")
	case "navigate":
		result, err := withTab("navigate_tab_url")
		if err != nil {
			return nil, err
		}
		url := strings.TrimSpace(stringPayload(payload, "url"))
		if url == "" {
			return nil, fmt.Errorf("navigate requires url")
		}
		result["url"] = url
		return result, nil
	case "back":
		return withTab("navigate_tab_back")
	case "forward":
		return withTab("navigate_tab_forward")
	case "reload":
		return withTab("navigate_tab_reload")
	case "snapshot":
		return withTab("dom_cua_get_visible_dom")
	case "click", "double_click":
		value, err := tabID()
		if err != nil {
			return nil, err
		}
		nodeID := strings.TrimSpace(stringPayload(payload, "node_id"))
		if nodeID != "" {
			typeName := "dom_cua_click"
			if action == "double_click" {
				typeName = "dom_cua_double_click"
			}
			result := map[string]any{"type": typeName, "tab_id": value, "node_id": nodeID}
			if timeout, ok := browserNumber(payload, "timeout_ms"); ok {
				result["timeout_ms"] = timeout
			}
			return result, nil
		}
		x, xOK := browserNumber(payload, "x")
		y, yOK := browserNumber(payload, "y")
		if !xOK || !yOK {
			return nil, fmt.Errorf("%s requires node_id or both x and y", action)
		}
		typeName := "cua_click"
		if action == "double_click" {
			typeName = "cua_double_click"
		}
		result := map[string]any{"type": typeName, "tab_id": value, "x": x, "y": y}
		if err := browserCopyKeys(payload, result); err != nil {
			return nil, err
		}
		if button, ok := browserNumber(payload, "button"); ok && action == "click" {
			result["button"] = button
		}
		return result, nil
	case "type":
		result, err := withTab("dom_cua_type")
		if err != nil {
			return nil, err
		}
		text, ok := payload["text"].(string)
		if !ok {
			return nil, fmt.Errorf("type requires text")
		}
		result["text"] = text
		return result, nil
	case "keypress":
		result, err := withTab("dom_cua_keypress")
		if err != nil {
			return nil, err
		}
		keys, err := browserStringList(payload, "keys", true)
		if err != nil {
			return nil, err
		}
		result["keys"] = keys
		return result, nil
	case "scroll":
		value, err := tabID()
		if err != nil {
			return nil, err
		}
		sx, sxOK := browserNumber(payload, "scroll_x")
		sy, syOK := browserNumber(payload, "scroll_y")
		if !sxOK || !syOK {
			return nil, fmt.Errorf("scroll requires scroll_x and scroll_y")
		}
		x, xOK := browserNumber(payload, "x")
		y, yOK := browserNumber(payload, "y")
		if xOK != yOK {
			return nil, fmt.Errorf("scroll x and y must be provided together")
		}
		if xOK {
			result := map[string]any{"type": "cua_scroll", "tab_id": value, "x": x, "y": y, "scroll_x": sx, "scroll_y": sy}
			if err := browserCopyKeys(payload, result); err != nil {
				return nil, err
			}
			return result, nil
		}
		result := map[string]any{"type": "dom_cua_scroll", "tab_id": value, "scroll_x": sx, "scroll_y": sy}
		if nodeID := strings.TrimSpace(stringPayload(payload, "node_id")); nodeID != "" {
			result["node_id"] = nodeID
		}
		return result, nil
	case "move":
		value, err := tabID()
		if err != nil {
			return nil, err
		}
		x, xOK := browserNumber(payload, "x")
		y, yOK := browserNumber(payload, "y")
		if !xOK || !yOK {
			return nil, fmt.Errorf("move requires x and y")
		}
		result := map[string]any{"type": "cua_move", "tab_id": value, "x": x, "y": y}
		if err := browserCopyKeys(payload, result); err != nil {
			return nil, err
		}
		return result, nil
	case "drag":
		value, err := tabID()
		if err != nil {
			return nil, err
		}
		path, err := browserPointPath(payload)
		if err != nil {
			return nil, err
		}
		result := map[string]any{"type": "cua_drag", "tab_id": value, "path": path}
		if err := browserCopyKeys(payload, result); err != nil {
			return nil, err
		}
		return result, nil
	case "screenshot":
		result, err := withTab("tab_screenshot")
		if err != nil {
			return nil, err
		}
		if boolPayload(payload, "full_page") {
			result["fullPage"] = true
		}
		cropKeys := []string{"crop_x", "crop_y", "crop_width", "crop_height"}
		cropValues := make([]float64, len(cropKeys))
		cropCount := 0
		for index, key := range cropKeys {
			if value, ok := browserNumber(payload, key); ok {
				cropValues[index] = value
				cropCount++
			}
		}
		if cropCount != 0 && cropCount != len(cropKeys) {
			return nil, fmt.Errorf("screenshot crop requires crop_x, crop_y, crop_width, and crop_height together")
		}
		if cropCount == len(cropKeys) {
			result["cropX"] = cropValues[0]
			result["cropY"] = cropValues[1]
			result["cropWidth"] = cropValues[2]
			result["cropHeight"] = cropValues[3]
		}
		return result, nil
	case "official":
		return browserOfficialCommand(payload)
	default:
		return command, fmt.Errorf("unsupported browser action %q", action)
	}
}

func browserOfficialCommand(payload map[string]any) (map[string]any, error) {
	raw, ok := payload["command"].(map[string]any)
	if !ok || raw == nil {
		return nil, fmt.Errorf("official requires command object")
	}
	commandType := strings.TrimSpace(stringPayload(raw, "type"))
	if commandType == "" {
		return nil, fmt.Errorf("official command.type is required")
	}
	if commandType == "list_browsers" {
		return nil, fmt.Errorf("official command type list_browsers is reserved; use browser action status")
	}
	if _, exists := raw["browser_id"]; exists {
		return nil, fmt.Errorf("official command must not set browser_id; browser_instance_id selects the verified extension instance")
	}
	result := make(map[string]any, len(raw))
	for key, value := range raw {
		result[key] = value
	}
	return result, nil
}

func browserNumber(payload map[string]any, key string) (float64, bool) {
	value, ok := payload[key]
	if !ok || value == nil {
		return 0, false
	}
	switch number := value.(type) {
	case float64:
		return number, true
	case float32:
		return float64(number), true
	case int:
		return float64(number), true
	case int64:
		return float64(number), true
	case json.Number:
		parsed, err := number.Float64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

func browserStringList(payload map[string]any, key string, required bool) ([]string, error) {
	value, exists := payload[key]
	if !exists || value == nil {
		if required {
			return nil, fmt.Errorf("%s is required", key)
		}
		return nil, nil
	}
	raw, ok := value.([]any)
	if !ok {
		if typed, typedOK := value.([]string); typedOK {
			if required && len(typed) == 0 {
				return nil, fmt.Errorf("%s must not be empty", key)
			}
			return typed, nil
		}
		return nil, fmt.Errorf("%s must be an array of strings", key)
	}
	items := make([]string, 0, len(raw))
	for _, item := range raw {
		text, ok := item.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("%s must contain non-empty strings", key)
		}
		items = append(items, text)
	}
	if required && len(items) == 0 {
		return nil, fmt.Errorf("%s must not be empty", key)
	}
	return items, nil
}

func browserCopyKeys(payload, target map[string]any) error {
	keys, err := browserStringList(payload, "keys", false)
	if err != nil {
		return err
	}
	if len(keys) > 0 {
		target["keys"] = keys
	}
	return nil
}

func browserPointPath(payload map[string]any) ([]map[string]any, error) {
	raw, ok := payload["path"].([]any)
	if !ok || len(raw) == 0 {
		return nil, fmt.Errorf("drag requires a non-empty path")
	}
	points := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		point, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("drag path entries must be objects with x and y")
		}
		x, xOK := browserNumber(point, "x")
		y, yOK := browserNumber(point, "y")
		if !xOK || !yOK {
			return nil, fmt.Errorf("drag path entries require numeric x and y")
		}
		points = append(points, map[string]any{"x": x, "y": y})
	}
	return points, nil
}

func (r *Runtime) browserConfirmationRequired(envReq envelope.Request, principalID, remoteID, workspace, instanceID, action, digest string, elicitation browseruse.Elicitation) (*mcp.CallToolResult, error) {
	if strings.TrimSpace(elicitation.Fingerprint) == "" {
		return r.terminalError(envReq, remoteID, workspace, "browser_confirmation_invalid", "Browser Service returned a permission prompt without a fingerprint")
	}
	_, err := r.approvals.PutPending(approval.Pending{
		Tool:              "browser",
		Summary:           elicitation.Message,
		Purpose:           envReq.Intent,
		Scope:             "remote_session",
		CommandDigest:     digest,
		RequestID:         envReq.RequestID,
		Workspace:         workspace,
		RemoteSessionID:   remoteID,
		PrincipalID:       principalID,
		ConfirmationToken: elicitation.Fingerprint,
		ContentKey:        browserConfirmationContentKey(principalID, digest, elicitation.Fingerprint),
	})
	if err != nil {
		return r.terminalError(envReq, remoteID, workspace, "confirmation_store_error", err.Error())
	}
	data := map[string]any{
		"action":                  action,
		"browser_instance_id":     instanceID,
		"confirmation_required":   true,
		"user_confirmed_required": true,
		"summary":                 elicitation.Message,
		"approval_scope":          "current_remote_session",
		"browser_prompt": map[string]any{
			"message": elicitation.Message,
			"meta":    elicitation.Meta,
		},
	}
	response := envelope.Fail(envelope.StatusNeedConfirmation, envReq.RequestID, workspace, data, "USER_CONFIRMATION_REQUIRED", "browser action is waiting for explicit user confirmation")
	response.RemoteSessionID = remoteID
	retry := make(map[string]any, len(envReq.Payload)+3)
	for key, value := range envReq.Payload {
		if key != "user_confirmed" {
			retry[key] = value
		}
	}
	retry["remote_session_id"] = remoteID
	retry["browser_instance_id"] = instanceID
	retry["purpose"] = envReq.Intent
	retry["user_confirmed"] = true
	addRecoveryAction(&response, "browser", "用户确认该 Browser Service 权限请求后，使用相同参数重试并设置 user_confirmed=true", retry)
	return r.resultJSON(response)
}

func (r *Runtime) pendingBrowserConfirmation(remoteID, principalID, digest string) (approval.Pending, bool) {
	var latest approval.Pending
	found := false
	for _, pending := range r.approvals.ListRemoteSession(remoteID) {
		if pending.Tool == "browser" && pending.PrincipalID == principalID && pending.CommandDigest == digest && (!found || pending.CreatedAt.After(latest.CreatedAt)) {
			latest = pending
			found = true
		}
	}
	return latest, found
}

func browserConfirmationContentKey(principalID, digest, fingerprint string) string {
	return "browser\x00" + principalID + "\x00" + digest + "\x00" + fingerprint
}

func browserCommandDigest(remoteID, instanceID string, command map[string]any) string {
	encoded, _ := json.Marshal(map[string]any{
		"remote_session_id":   remoteID,
		"browser_instance_id": instanceID,
		"command":             command,
	})
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func browserServiceSessionID(remoteID string) string {
	sum := sha256.Sum256([]byte(remoteID))
	return "mcpx_" + hex.EncodeToString(sum[:12])
}

func browserServiceTurnID(remoteID string) string {
	return browserServiceSessionID(remoteID) + "_turn"
}

func publicBrowserInstallation(installation browseruse.Installation) map[string]any {
	return map[string]any{
		"family":       installation.Family,
		"profile":      installation.Profile,
		"extension_id": installation.ExtensionID,
		"name":         installation.Name,
		"version":      installation.Version,
	}
}

func publicBrowserBackend(backend browseruse.Backend) map[string]any {
	item := map[string]any{
		"type":         backend.Info.Type,
		"family":       backend.Info.Family,
		"name":         backend.Info.Name,
		"version":      backend.Info.Version,
		"extension_id": backend.Info.Metadata.ExtensionID,
		"instance_id":  backend.Info.Metadata.ExtensionInstanceID,
	}
	if backend.Info.AgentRequestHeaderEnabled != nil {
		item["agent_request_header_enabled"] = *backend.Info.AgentRequestHeaderEnabled
	}
	if len(backend.Info.Capabilities) > 0 {
		item["capabilities"] = backend.Info.Capabilities
	}
	return item
}
