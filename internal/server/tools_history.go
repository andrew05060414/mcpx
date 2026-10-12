package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/mcpresult"
)

// getHstryBinary finds the hstry CLI binary across standard installation paths.
func getHstryBinary() string {
	if path, err := exec.LookPath("hstry"); err == nil {
		return path
	}
	if home := os.Getenv("USERPROFILE"); home != "" {
		candidates := []string{
			filepath.Join(home, ".cargo", "bin", "hstry.exe"),
			filepath.Join(home, ".cargo", "bin", "hstry"),
			filepath.Join(home, ".local", "bin", "hstry.exe"),
			filepath.Join(home, ".local", "bin", "hstry"),
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				return c
			}
		}
	}
	if home := os.Getenv("HOME"); home != "" {
		candidate := filepath.Join(home, ".cargo", "bin", "hstry")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return "hstry"
}

// runHstryCLI executes hstry with HSTRY_NO_SERVICE=1 and captures JSON output.
func runHstryCLI(ctx context.Context, args ...string) (any, error) {
	binary := getHstryBinary()
	cmd := exec.CommandContext(ctx, binary, args...)

	env := os.Environ()
	// Ensure HSTRY_NO_SERVICE=1 is always set for non-blocking local queries
	env = append(env, "HSTRY_NO_SERVICE=1")
	cmd.Env = env

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		errOutput := strings.TrimSpace(stderr.String())
		if errOutput == "" {
			errOutput = strings.TrimSpace(stdout.String())
		}
		if errOutput == "" {
			errOutput = err.Error()
		}
		return nil, fmt.Errorf("hstry %s failed: %s", strings.Join(args, " "), errOutput)
	}

	rawOut := strings.TrimSpace(stdout.String())
	if rawOut == "" {
		return map[string]any{"status": "ok"}, nil
	}

	var parsed any
	if err := json.Unmarshal([]byte(rawOut), &parsed); err == nil {
		return parsed, nil
	}

	return map[string]any{"raw_output": rawOut}, nil
}

// registerHistoryTools registers the 5 core HSTRY search & peek tools in MCPX.
func (r *Runtime) registerHistoryTools(s *mcp.Server) {
	// 1. history_search
	r.addTool(s, cleanCoreTool(
		"history_search",
		"在统一本地 AI 对话原文档案库 (HSTRY) 中搜索过去的讨论、架构决策与经验。返回匹配列表与会话短 ID（如 'limp-shed'）。",
		map[string]any{
			"query":     stringSchema("搜索关键词或语句（必填）"),
			"scope":     enumSchema("搜索范围：local (当前机器本地库，默认/推荐), remote (远端), all", "local", "remote", "all"),
			"source":    stringSchema("过滤工具来源（例如 cursor-*, antigravity-*, claude-code-*, opencode-*, zcode, codex-* 等）"),
			"workspace": stringSchema("过滤关联的工作区路径或名称"),
			"limit":     numberSchema("最多返回结果条数（默认 8，防上下文膨胀）"),
			"role":      enumSchema("过滤消息角色", "user", "assistant", "system", "tool"),
			"mode":      enumSchema("搜索模式", "auto", "natural", "code"),
			"after":     stringSchema("起始日期过滤 (ISO 8601 或相对时间如 '2d', '1w', '2025-01-15')"),
			"before":    stringSchema("截止日期过滤"),
		},
		[]string{"query"},
		readOnlyToolAnnotation,
	), r.toolHistorySearch)

	// 2. history_peek
	r.addTool(s, cleanCoreTool(
		"history_peek",
		"【低 Token 推荐】获取某个历史会话的高密度结构化摘要（首尾 User Prompt、最终结论、操作文件列表、执行命令与工具统计）。",
		map[string]any{
			"conversation_id": stringSchema("会话 ID（支持 search 返回的短 ID 如 'limp-shed' 或完整 UUID）"),
			"chars":           numberSchema("截断助手最终回复的最大字符数（默认 400）"),
		},
		[]string{"conversation_id"},
		readOnlyToolAnnotation,
	), r.toolHistoryPeek)

	// 3. history_show
	r.addTool(s, cleanCoreTool(
		"history_show",
		"读取某个具体会话的完整对话或消息明细（仅在 history_peek 证据不足时使用）。",
		map[string]any{
			"conversation_id": stringSchema("会话 ID 或短标识符"),
		},
		[]string{"conversation_id"},
		readOnlyToolAnnotation,
	), r.toolHistoryShow)

	// 4. history_list
	r.addTool(s, cleanCoreTool(
		"history_list",
		"列出最近同步的历史会话记录列表。",
		map[string]any{
			"source":    stringSchema("按工具来源过滤"),
			"workspace": stringSchema("按工作区过滤"),
			"limit":     numberSchema("最多返回条数（默认 10）"),
		},
		nil,
		readOnlyToolAnnotation,
	), r.toolHistoryList)

	// 5. history_stats
	r.addTool(s, cleanCoreTool(
		"history_stats",
		"查看本地 HSTRY 档案库的全局统计（总会话数、总消息数、各工具源分布）。",
		map[string]any{},
		nil,
		readOnlyToolAnnotation,
	), r.toolHistoryStats)
}

func (r *Runtime) toolHistorySearch(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	payload := mcpresult.Arguments(req)
	query := stringPayload(payload, "query")
	if query == "" {
		return nil, fmt.Errorf("query is required")
	}

	scope := stringPayload(payload, "scope")
	if scope == "" {
		scope = "local"
	}

	limit := 8
	if l, ok := payload["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}

	cliArgs := []string{"search", query, "--scope", scope, "--limit", strconv.Itoa(limit), "--json"}

	if source := stringPayload(payload, "source"); source != "" {
		cliArgs = append(cliArgs, "--source", source)
	}
	if ws := stringPayload(payload, "workspace"); ws != "" {
		cliArgs = append(cliArgs, "--workspace", ws)
	}
	if role := stringPayload(payload, "role"); role != "" {
		cliArgs = append(cliArgs, "--role", role)
	}
	if mode := stringPayload(payload, "mode"); mode != "" {
		cliArgs = append(cliArgs, "--mode", mode)
	}
	if after := stringPayload(payload, "after"); after != "" {
		cliArgs = append(cliArgs, "--after", after)
	}
	if before := stringPayload(payload, "before"); before != "" {
		cliArgs = append(cliArgs, "--before", before)
	}

	data, err := runHstryCLI(ctx, cliArgs...)
	if err != nil {
		return nil, err
	}
	return compactToolResult(data, fmt.Sprintf("Searched history for %q", query)), nil
}

func (r *Runtime) toolHistoryPeek(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	payload := mcpresult.Arguments(req)
	convID := stringPayload(payload, "conversation_id")
	if convID == "" {
		return nil, fmt.Errorf("conversation_id is required")
	}

	cliArgs := []string{"peek", convID, "--json"}
	if chars, ok := payload["chars"].(float64); ok && chars > 0 {
		cliArgs = append(cliArgs, "--chars", strconv.Itoa(int(chars)))
	}

	data, err := runHstryCLI(ctx, cliArgs...)
	if err != nil {
		return nil, err
	}
	return compactToolResult(data, fmt.Sprintf("Peeked conversation %s", convID)), nil
}

func (r *Runtime) toolHistoryShow(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	convID := stringPayload(mcpresult.Arguments(req), "conversation_id")
	if convID == "" {
		return nil, fmt.Errorf("conversation_id is required")
	}

	data, err := runHstryCLI(ctx, "show", convID, "--json")
	if err != nil {
		return nil, err
	}
	return compactToolResult(data, fmt.Sprintf("Retrieved conversation %s", convID)), nil
}

func (r *Runtime) toolHistoryList(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	payload := mcpresult.Arguments(req)
	limit := 10
	if l, ok := payload["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}

	cliArgs := []string{"list", "--limit", strconv.Itoa(limit), "--json"}
	if source := stringPayload(payload, "source"); source != "" {
		cliArgs = append(cliArgs, "--source", source)
	}
	if ws := stringPayload(payload, "workspace"); ws != "" {
		cliArgs = append(cliArgs, "--workspace", ws)
	}

	data, err := runHstryCLI(ctx, cliArgs...)
	if err != nil {
		return nil, err
	}
	return compactToolResult(data, "Listed history conversations"), nil
}

func (r *Runtime) toolHistoryStats(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	data, err := runHstryCLI(ctx, "stats", "--json")
	if err != nil {
		return nil, err
	}
	return compactToolResult(data, "Retrieved history statistics"), nil
}
