package server

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/mcpresult"
)

// 历史查询工具转发到命令行 `chronicle`（统一入口，后端是 AgentsView 档案库）。
// 旧版转发到已于 2026-10-17 退役的 hstry，对应关系：
//
//	history_search   -> chronicle search   （改用 AgentsView 的参数；去掉 scope/role/after/before，mode 改为 keyword|fts|semantic|hybrid|regex）
//	history_get      -> chronicle get      （会话元数据与信号）
//	history_show     -> chronicle messages （按窗口分页读取消息，默认 50 条）
//	history_list     -> chronicle list
//	history_projects -> chronicle projects （各项目会话数）
//
// history_peek / history_stats 是退役 hstry 的旧名别名，本地日志无真实调用，已删除。
const (
	historySearchDefaultLimit = 8
	historyListDefaultLimit   = 10
	historyMessagesDefault    = 50
	historyMaxLimit           = 500
)

// registerHistoryTools registers the AgentsView/chronicle history tools in MCPX.
func (r *Runtime) registerHistoryTools(s *mcp.Server) {
	filters := func(props map[string]any) map[string]any {
		props["agent"] = stringSchema("按来源工具过滤（claude、codex、cursor、opencode、antigravity 等）")
		props["project"] = stringSchema("按项目名过滤（见 history_projects）")
		props["since"] = stringSchema("只看此后活跃的会话：相对时间（12h、14d、2w、3m、1y）或 YYYY-MM-DD")
		props["date_from"] = stringSchema("活跃日期不早于 YYYY-MM-DD")
		props["date_to"] = stringSchema("活跃日期不晚于 YYYY-MM-DD")
		return props
	}

	r.addTool(s, cleanCoreTool(
		"history_search",
		"在 AgentsView 档案库（经 chronicle 命令）中搜索过去在各 AI 工具里的对话、决定和代码。返回匹配片段与会话 ID（UUID）。多个词默认都要出现；用 any 传多个词可改为任意一个出现即可。",
		filters(map[string]any{
			"query":  stringSchema("搜索关键词或语句；用 any 时省略"),
			"any":    arraySchema(map[string]any{"type": "string"}, "任意一个词出现即可（OR，纯关键词）；不能与 mode=semantic/hybrid/regex 同用"),
			"mode":   enumSchema("搜索模式：keyword（默认）、fts（快速分词，仅消息）、semantic（语义）、hybrid（语义加全文）、regex（RE2 正则）", "keyword", "fts", "semantic", "hybrid", "regex"),
			"limit":  numberSchema(fmt.Sprintf("最多返回结果条数（默认 %d，上限 %d）", historySearchDefaultLimit, historyMaxLimit)),
			"cursor": numberSchema("消息级分页游标；原样传入上页 next_cursor。会话去重分页请用原生 MCP search_content"),
		}),
		nil,
		readOnlyToolAnnotation,
	), r.toolHistorySearch)

	r.addTool(s, cleanCoreTool(
		"history_get",
		"读取一个会话的元数据与信号：项目、来源、起止时间、消息数、首条提问、结局和健康度。比 history_show 省 token；要看具体对话再用 history_show。",
		map[string]any{
			"conversation_id": stringSchema("会话 ID（UUID，来自 history_search / history_list）"),
		},
		[]string{"conversation_id"},
		readOnlyToolAnnotation,
	), r.toolHistoryGet)

	r.addTool(s, cleanCoreTool(
		"history_show",
		"读取一个会话的消息明细（分页窗口，默认从头 50 条）。仅在 history_get 证据不足时使用。",
		map[string]any{
			"conversation_id": stringSchema("会话 ID（UUID）"),
			"from":            numberSchema("起始序号（含）；direction=desc 时省略表示最新一页"),
			"limit":           numberSchema(fmt.Sprintf("最多返回消息数（默认 %d，上限 %d）", historyMessagesDefault, historyMaxLimit)),
			"around":          numberSchema("以此序号为中心取窗口（可配合 before/after）"),
			"before":          numberSchema("around 之前取几条（默认 5）"),
			"after":           numberSchema("around 之后取几条（默认 5）"),
			"direction":       enumSchema("排序方向", "asc", "desc"),
			"role":            stringSchema("只要这些角色，逗号分隔，例如 user,assistant"),
		},
		[]string{"conversation_id"},
		readOnlyToolAnnotation,
	), r.toolHistoryShow)

	r.addTool(s, cleanCoreTool(
		"history_list",
		"列出 AgentsView 档案库里的会话（默认按最近活跃排序）。",
		filters(map[string]any{
			"limit": numberSchema(fmt.Sprintf("最多返回条数（默认 %d，上限 %d）", historyListDefaultLimit, historyMaxLimit)),
		}),
		nil,
		readOnlyToolAnnotation,
	), r.toolHistoryList)

	r.addTool(s, cleanCoreTool(
		"history_projects",
		"列出档案库里各项目及其会话数。",
		map[string]any{},
		nil,
		readOnlyToolAnnotation,
	), r.toolHistoryProjects)

}

// historyLimit 读取并限制数量参数；缺省返回 def。
func historyLimit(payload map[string]any, key string, def int) int {
	value, ok := payload[key].(float64)
	if !ok || value < 1 {
		return def
	}
	if value > historyMaxLimit {
		return historyMaxLimit
	}
	return int(value)
}

// historyFlagValue 读取要作为 flag 值传给 chronicle 的字符串；以 - 开头的值会被 flag 解析器误读，一律拒绝。
func historyFlagValue(payload map[string]any, key string) (string, error) {
	value := strings.TrimSpace(stringPayload(payload, key))
	if strings.HasPrefix(value, "-") {
		return "", fmt.Errorf("%s 不能以 - 开头", key)
	}
	return value, nil
}

func historyID(payload map[string]any) (string, error) {
	id := strings.TrimSpace(stringPayload(payload, "conversation_id"))
	if id == "" {
		return "", fmt.Errorf("conversation_id is required")
	}
	if strings.HasPrefix(id, "-") {
		return "", fmt.Errorf("conversation_id 不能以 - 开头")
	}
	return id, nil
}

// appendHistoryFilters 追加 agent/project/since/date_from/date_to 过滤参数。
func appendHistoryFilters(args []string, payload map[string]any) ([]string, error) {
	for _, filter := range []struct{ key, flag string }{
		{"agent", "--agent"},
		{"project", "--project"},
		{"since", "--since"},
		{"date_from", "--date-from"},
		{"date_to", "--date-to"},
	} {
		value, err := historyFlagValue(payload, filter.key)
		if err != nil {
			return nil, err
		}
		if value != "" {
			args = append(args, filter.flag, value)
		}
	}
	return args, nil
}

func buildHistorySearchArgs(payload map[string]any) ([]string, error) {
	query := strings.TrimSpace(stringPayload(payload, "query"))
	var anyTerms []string
	if raw, ok := payload["any"].([]any); ok {
		for _, item := range raw {
			term, _ := item.(string)
			term = strings.TrimSpace(term)
			if term == "" {
				continue
			}
			if strings.HasPrefix(term, "-") {
				return nil, fmt.Errorf("any 里的词不能以 - 开头: %q", term)
			}
			anyTerms = append(anyTerms, term)
		}
	}
	if query == "" && len(anyTerms) == 0 {
		return nil, fmt.Errorf("query is required (或提供 any)")
	}
	if query != "" && len(anyTerms) > 0 {
		return nil, fmt.Errorf("query 和 any 只能二选一")
	}

	args := []string{"search", "--json", "--include-one-shot", "--limit", strconv.Itoa(historyLimit(payload, "limit", historySearchDefaultLimit))}
	mode := stringPayload(payload, "mode")
	switch mode {
	case "", "keyword":
		args = append(args, "--fts")
	case "fts", "semantic", "hybrid", "regex":
		if len(anyTerms) > 0 && mode != "fts" {
			return nil, fmt.Errorf("any 是纯关键词查询，不能与 mode=%s 同用", mode)
		}
		args = append(args, "--"+mode)
	default:
		return nil, fmt.Errorf("未知 mode: %q", mode)
	}
	args, err := appendHistoryFilters(args, payload)
	if err != nil {
		return nil, err
	}
	if raw, exists := payload["cursor"]; exists {
		value, ok := raw.(float64)
		if !ok || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value != math.Trunc(value) || value > 2147483647 {
			return nil, fmt.Errorf("cursor must be a nonnegative integer")
		}
		args = append(args, "--cursor", strconv.Itoa(int(value)))
	}
	if len(anyTerms) > 0 {
		// chronicle 前门把 --any 之后（到下一个以 - 开头的参数为止）的词拼成 OR 查询。
		return append(append(args, "--any"), anyTerms...), nil
	}
	// "--" 之后全部是位置参数，query 以 - 开头也不会被当成 flag。
	return append(args, "--", query), nil
}

func buildHistoryGetArgs(payload map[string]any) ([]string, error) {
	id, err := historyID(payload)
	if err != nil {
		return nil, err
	}
	return []string{"get", "--json", "--", id}, nil
}

func buildHistoryMessagesArgs(payload map[string]any) ([]string, error) {
	id, err := historyID(payload)
	if err != nil {
		return nil, err
	}
	args := []string{"messages", "--json", "--limit", strconv.Itoa(historyLimit(payload, "limit", historyMessagesDefault))}
	for _, n := range []struct{ key, flag string }{
		{"from", "--from"}, {"around", "--around"}, {"before", "--before"}, {"after", "--after"},
	} {
		if raw, exists := payload[n.key]; exists {
			value, ok := raw.(float64)
			if !ok || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value != math.Trunc(value) || value > 2147483647 {
				return nil, fmt.Errorf("%s must be a nonnegative integer", n.key)
			}
			args = append(args, n.flag, strconv.Itoa(int(value)))
		}
	}
	switch direction := stringPayload(payload, "direction"); direction {
	case "":
	case "asc", "desc":
		args = append(args, "--direction", direction)
	default:
		return nil, fmt.Errorf("未知 direction: %q", direction)
	}
	role, err := historyFlagValue(payload, "role")
	if err != nil {
		return nil, err
	}
	if role != "" {
		args = append(args, "--role", role)
	} else {
		args = append(args, "--role", "user,assistant")
	}
	return append(args, "--", id), nil
}

func buildHistoryListArgs(payload map[string]any) ([]string, error) {
	args := []string{"list", "--json", "--include-one-shot", "--limit", strconv.Itoa(historyLimit(payload, "limit", historyListDefaultLimit))}
	return appendHistoryFilters(args, payload)
}

func (r *Runtime) runHistory(ctx context.Context, args []string, argErr error, summary string) (*mcp.CallToolResult, error) {
	if argErr != nil {
		return nil, argErr
	}
	data, err := runChronicleCLI(ctx, args...)
	if err != nil {
		return nil, err
	}
	if len(args) > 0 && args[0] == "messages" {
		data, err = historyDialogue(data)
		if err != nil {
			return nil, err
		}
	}
	return compactToolResult(data, summary), nil
}

// This compatibility adapter returns only the dialogue projection. All richer
// retrieval and pagination behavior belongs to the upstream memory MCP.
func historyDialogue(data any) (any, error) {
	object, ok := data.(map[string]any)
	if !ok || object["messages"] == nil {
		return nil, fmt.Errorf("history_show requires structured messages; use AgentsView memory MCP for a smaller page")
	}
	rows, ok := object["messages"].([]any)
	if !ok {
		return nil, fmt.Errorf("invalid history messages response")
	}
	clean := make([]any, 0, len(rows))
	for _, row := range rows {
		message, ok := row.(map[string]any)
		if !ok {
			continue
		}
		role, _ := message["role"].(string)
		if role != "user" && role != "assistant" {
			continue
		}
		projected := map[string]any{}
		for _, key := range []string{"ordinal", "role", "content", "timestamp", "model", "id", "session_id"} {
			if value, ok := message[key]; ok {
				projected[key] = value
			}
		}
		if content, ok := projected["content"].(string); ok {
			chars := []rune(content)
			if len(chars) > 2000 {
				projected["content"] = string(chars[:2000])
				projected["truncated"] = true
			}
		}
		clean = append(clean, projected)
	}
	object["messages"] = clean
	object["count"] = len(clean)
	return object, nil
}

func (r *Runtime) toolHistorySearch(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args, err := buildHistorySearchArgs(mcpresult.Arguments(req))
	return r.runHistory(ctx, args, err, "Searched history archive")
}

func (r *Runtime) toolHistoryGet(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	payload := mcpresult.Arguments(req)
	args, err := buildHistoryGetArgs(payload)
	return r.runHistory(ctx, args, err, fmt.Sprintf("Retrieved session %s", stringPayload(payload, "conversation_id")))
}

func (r *Runtime) toolHistoryShow(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	payload := mcpresult.Arguments(req)
	args, err := buildHistoryMessagesArgs(payload)
	return r.runHistory(ctx, args, err, fmt.Sprintf("Retrieved messages of %s", stringPayload(payload, "conversation_id")))
}

func (r *Runtime) toolHistoryList(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args, err := buildHistoryListArgs(mcpresult.Arguments(req))
	return r.runHistory(ctx, args, err, "Listed history sessions")
}

func (r *Runtime) toolHistoryProjects(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return r.runHistory(ctx, []string{"projects", "--json"}, nil, "Listed history projects")
}
