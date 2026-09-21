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

// getMulticaBinary finds the multica CLI binary across standard installation paths.
func getMulticaBinary() string {
	if path, err := exec.LookPath("multica"); err == nil {
		return path
	}
	if home := os.Getenv("USERPROFILE"); home != "" {
		candidates := []string{
			filepath.Join(home, ".multica", "bin", "multica.exe"),
			filepath.Join(home, ".multica", "bin", "multica"),
			filepath.Join(home, "AppData", "Local", "Programs", "multica", "multica.exe"),
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				return c
			}
		}
	}
	if home := os.Getenv("HOME"); home != "" {
		candidate := filepath.Join(home, ".multica", "bin", "multica")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return "multica"
}

// runMulticaCLI runs the multica CLI with arguments, captures JSON output, and returns structured data.
func runMulticaCLI(ctx context.Context, args ...string) (any, error) {
	binary := getMulticaBinary()
	cmd := exec.CommandContext(ctx, binary, args...)

	if len(cmd.Env) == 0 {
		cmd.Env = os.Environ()
	}

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
		return nil, fmt.Errorf("multica %s failed: %s", strings.Join(args, " "), errOutput)
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

// registerMulticaTools adds the full Multica orchestration & agent lifecycle tools to MCPX.
func (r *Runtime) registerMulticaTools(s *mcp.Server) {
	// 1. multica_list_agents
	r.addTool(s, cleanCoreTool(
		"multica_list_agents",
		"列出 Multica 当前工作区中的所有可用 Agent 及其状态、ID、系统指令与角色能力。",
		map[string]any{
			"status": stringSchema("按状态过滤（例如 idle, busy, offline）"),
		},
		nil,
		readOnlyToolAnnotation,
	), r.toolMulticaListAgents)

	// 2. multica_get_agent
	r.addTool(s, cleanCoreTool(
		"multica_get_agent",
		"查询 Multica 某个具体 Agent 的详细系统指令 (instructions)、配置、绑定的 Runtime 及状态。",
		map[string]any{
			"agent_id": stringSchema("Agent ID (UUID 或名称)"),
		},
		[]string{"agent_id"},
		readOnlyToolAnnotation,
	), r.toolMulticaGetAgent)

	// 3. multica_create_agent
	r.addTool(s, cleanCoreTool(
		"multica_create_agent",
		"在 Multica 中动态新建一个专属 Agent（可指定系统指令、绑定的 Runtime、模型及并发上限）。",
		map[string]any{
			"name":                 stringSchema("Agent 名称（必填）"),
			"instructions":         stringSchema("Agent 系统提示词 / 工作规范指令"),
			"description":          stringSchema("Agent 角色职责简要说明"),
			"runtime_id":           stringSchema("绑定的 Runtime ID（例如 Antigravity/Codex/Claude 对应的 runtime_id）"),
			"model":                stringSchema("模型标识符（如 gemini-2.5-pro, claude-sonnet-4-6 等）"),
			"max_concurrent_tasks": numberSchema("最大并发任务数 (1-50，默认 4)"),
		},
		[]string{"name", "runtime_id"},
		mutatingToolAnnotation,
	), r.toolMulticaCreateAgent)

	// 4. multica_update_agent
	r.addTool(s, cleanCoreTool(
		"multica_update_agent",
		"更新 Multica 已有 Agent 的指令 (instructions)、名称、描述、模型或状态。",
		map[string]any{
			"agent_id":             stringSchema("Agent ID（必填）"),
			"name":                 stringSchema("新名称"),
			"instructions":         stringSchema("新系统指令内容"),
			"description":          stringSchema("新描述"),
			"model":                stringSchema("新模型"),
			"max_concurrent_tasks": numberSchema("新最大并发任务数"),
		},
		[]string{"agent_id"},
		mutatingToolAnnotation,
	), r.toolMulticaUpdateAgent)

	// 5. multica_archive_agent
	r.addTool(s, cleanCoreTool(
		"multica_archive_agent",
		"归档/删除 Multica 中指定的 Agent。",
		map[string]any{
			"agent_id": stringSchema("Agent ID（必填）"),
		},
		[]string{"agent_id"},
		mutatingToolAnnotation,
	), r.toolMulticaArchiveAgent)

	// 6. multica_list_runtimes
	r.addTool(s, cleanCoreTool(
		"multica_list_runtimes",
		"列出 Multica 在线可用的底层执行 Runtime（如 Antigravity, Claude Code, Codex, Cursor, Grok, Qoder 等）。",
		map[string]any{},
		nil,
		readOnlyToolAnnotation,
	), r.toolMulticaListRuntimes)

	// 7. multica_list_projects
	r.addTool(s, cleanCoreTool(
		"multica_list_projects",
		"列出 Multica 当前工作区中的所有 Project（项目）及其 ID、标识符与状态。",
		map[string]any{},
		nil,
		readOnlyToolAnnotation,
	), r.toolMulticaListProjects)

	// 8. multica_dispatch_task
	r.addTool(s, cleanCoreTool(
		"multica_dispatch_task",
		"在 Multica 中创建任务/Issue 并直接分派给指定的 Agent（例如 Gemini, QA Reviewer 等），任务将进入本地 Runtime 执行队列。",
		map[string]any{
			"title":        stringSchema("任务标题（简明表达目标）"),
			"description":  stringSchema("任务详细说明 / 规格要求 / 约束条件（支持多行 Markdown）"),
			"agent_id":     stringSchema("指派给的 Agent UUID 或名称（例如 'Engineer (Gemini)' 或 'b08c3670-...'）"),
			"project_id":   stringSchema("关联的 Multica Project ID"),
			"priority":     enumSchema("优先级", "low", "medium", "high", "urgent", "none"),
			"parent_issue": stringSchema("可选的父 Issue ID"),
			"stage":        numberSchema("可选的阶段分组编号 (>=1)"),
		},
		[]string{"title"},
		mutatingToolAnnotation,
	), r.toolMulticaDispatchTask)

	// 9. multica_get_issue
	r.addTool(s, cleanCoreTool(
		"multica_get_issue",
		"查询 Multica 中某个 Issue / 任务的实时详情、状态、执行者与元数据。",
		map[string]any{
			"issue_id": stringSchema("Issue ID (UUID 或短标识符如 PX-28)"),
		},
		[]string{"issue_id"},
		readOnlyToolAnnotation,
	), r.toolMulticaGetIssue)

	// 10. multica_comment_issue
	r.addTool(s, cleanCoreTool(
		"multica_comment_issue",
		"向 Multica 中的某个 Issue / 任务添加评论（追加上下文、澄清需求或反馈指令）。",
		map[string]any{
			"issue_id":          stringSchema("Issue ID (UUID 或短标识符)"),
			"content":           stringSchema("评论正文内容"),
			"parent_comment_id": stringSchema("可选的父评论 ID（回复特定评论线程）"),
		},
		[]string{"issue_id", "content"},
		mutatingToolAnnotation,
	), r.toolMulticaCommentIssue)

	// 11. multica_list_runs
	r.addTool(s, cleanCoreTool(
		"multica_list_runs",
		"查询 Multica 某个 Issue 的执行历史列表（Task ID、Agent、状态、时间）。",
		map[string]any{
			"issue_id": stringSchema("Issue ID"),
		},
		[]string{"issue_id"},
		readOnlyToolAnnotation,
	), r.toolMulticaListRuns)

	// 12. multica_get_run_messages
	r.addTool(s, cleanCoreTool(
		"multica_get_run_messages",
		"查询 Multica 某个执行 Task 的实时输出日志和 Agent 对话消息。",
		map[string]any{
			"task_id":  stringSchema("执行 Task ID"),
			"issue_id": stringSchema("可选的 Issue ID（用于辅助解析短 Task ID）"),
			"since":    numberSchema("可选的消息起始序号"),
		},
		[]string{"task_id"},
		readOnlyToolAnnotation,
	), r.toolMulticaGetRunMessages)

	// 13. multica_rerun_task
	r.addTool(s, cleanCoreTool(
		"multica_rerun_task",
		"重新入队执行某个 Multica Issue（触发 Agent 重新运行）。",
		map[string]any{
			"issue_id": stringSchema("Issue ID"),
		},
		[]string{"issue_id"},
		mutatingToolAnnotation,
	), r.toolMulticaRerunTask)

	// 14. multica_cancel_task
	r.addTool(s, cleanCoreTool(
		"multica_cancel_task",
		"取消或中断 Multica 正在运行或排队中的任务（即时中断正在运行的 Agent）。",
		map[string]any{
			"task_id":  stringSchema("执行 Task ID (UUID 或短前缀)"),
			"issue_id": stringSchema("可选的 Issue ID"),
		},
		[]string{"task_id"},
		mutatingToolAnnotation,
	), r.toolMulticaCancelTask)
}

func (r *Runtime) toolMulticaListAgents(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := []string{"agent", "list", "--output", "json"}
	status := stringPayload(mcpresult.Arguments(req), "status")
	if status != "" {
		args = append(args, "--status", status)
	}

	data, err := runMulticaCLI(ctx, args...)
	if err != nil {
		return nil, err
	}
	return compactToolResult(data, "Listed Multica agents successfully"), nil
}

func (r *Runtime) toolMulticaGetAgent(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	agentID := stringPayload(mcpresult.Arguments(req), "agent_id")
	if agentID == "" {
		return nil, fmt.Errorf("agent_id is required")
	}

	data, err := runMulticaCLI(ctx, "agent", "get", agentID, "--output", "json")
	if err != nil {
		return nil, err
	}
	return compactToolResult(data, fmt.Sprintf("Retrieved agent %s", agentID)), nil
}

func (r *Runtime) toolMulticaCreateAgent(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	payload := mcpresult.Arguments(req)
	name := stringPayload(payload, "name")
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	runtimeID := stringPayload(payload, "runtime_id")
	if runtimeID == "" {
		return nil, fmt.Errorf("runtime_id is required")
	}

	cliArgs := []string{"agent", "create", "--name", name, "--runtime-id", runtimeID, "--output", "json"}
	if desc := stringPayload(payload, "description"); desc != "" {
		cliArgs = append(cliArgs, "--description", desc)
	}
	if instructions := stringPayload(payload, "instructions"); instructions != "" {
		cliArgs = append(cliArgs, "--instructions", instructions)
	}
	if model := stringPayload(payload, "model"); model != "" {
		cliArgs = append(cliArgs, "--model", model)
	}
	if maxTasks, ok := payload["max_concurrent_tasks"].(float64); ok && maxTasks >= 1 {
		cliArgs = append(cliArgs, "--max-concurrent-tasks", strconv.Itoa(int(maxTasks)))
	}

	data, err := runMulticaCLI(ctx, cliArgs...)
	if err != nil {
		return nil, err
	}
	return compactToolResult(data, fmt.Sprintf("Created agent %q successfully", name)), nil
}

func (r *Runtime) toolMulticaUpdateAgent(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	payload := mcpresult.Arguments(req)
	agentID := stringPayload(payload, "agent_id")
	if agentID == "" {
		return nil, fmt.Errorf("agent_id is required")
	}

	cliArgs := []string{"agent", "update", agentID, "--output", "json"}
	if name := stringPayload(payload, "name"); name != "" {
		cliArgs = append(cliArgs, "--name", name)
	}
	if desc := stringPayload(payload, "description"); desc != "" {
		cliArgs = append(cliArgs, "--description", desc)
	}
	if instructions := stringPayload(payload, "instructions"); instructions != "" {
		cliArgs = append(cliArgs, "--instructions", instructions)
	}
	if model := stringPayload(payload, "model"); model != "" {
		cliArgs = append(cliArgs, "--model", model)
	}
	if maxTasks, ok := payload["max_concurrent_tasks"].(float64); ok && maxTasks >= 1 {
		cliArgs = append(cliArgs, "--max-concurrent-tasks", strconv.Itoa(int(maxTasks)))
	}

	data, err := runMulticaCLI(ctx, cliArgs...)
	if err != nil {
		return nil, err
	}
	return compactToolResult(data, fmt.Sprintf("Updated agent %s successfully", agentID)), nil
}

func (r *Runtime) toolMulticaArchiveAgent(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	agentID := stringPayload(mcpresult.Arguments(req), "agent_id")
	if agentID == "" {
		return nil, fmt.Errorf("agent_id is required")
	}

	data, err := runMulticaCLI(ctx, "agent", "archive", agentID, "--output", "json")
	if err != nil {
		return nil, err
	}
	return compactToolResult(data, fmt.Sprintf("Archived agent %s", agentID)), nil
}

func (r *Runtime) toolMulticaListRuntimes(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	data, err := runMulticaCLI(ctx, "runtime", "list", "--output", "json")
	if err != nil {
		return nil, err
	}
	return compactToolResult(data, "Listed Multica runtimes successfully"), nil
}

func (r *Runtime) toolMulticaListProjects(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	data, err := runMulticaCLI(ctx, "project", "list", "--output", "json")
	if err != nil {
		return nil, err
	}
	return compactToolResult(data, "Listed Multica projects successfully"), nil
}

func (r *Runtime) toolMulticaDispatchTask(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	payload := mcpresult.Arguments(req)
	title := stringPayload(payload, "title")
	if title == "" {
		return nil, fmt.Errorf("title is required")
	}

	cliArgs := []string{"issue", "create", "--title", title, "--output", "json"}

	description := stringPayload(payload, "description")
	if description != "" {
		tmpFile, err := os.CreateTemp("", "multica-desc-*.md")
		if err != nil {
			return nil, fmt.Errorf("failed to create temp file for description: %w", err)
		}
		tmpPath := tmpFile.Name()
		defer os.Remove(tmpPath)

		if err := os.WriteFile(tmpPath, []byte(description), 0600); err != nil {
			return nil, fmt.Errorf("failed to write description file: %w", err)
		}
		_ = tmpFile.Close()
		cliArgs = append(cliArgs, "--description-file", tmpPath, "--allow-external-file")
	}

	if agent := stringPayload(payload, "agent_id"); agent != "" {
		cliArgs = append(cliArgs, "--assignee", agent)
	}
	if project := stringPayload(payload, "project_id"); project != "" {
		cliArgs = append(cliArgs, "--project", project)
	}
	if priority := stringPayload(payload, "priority"); priority != "" {
		cliArgs = append(cliArgs, "--priority", priority)
	}
	if parent := stringPayload(payload, "parent_issue"); parent != "" {
		cliArgs = append(cliArgs, "--parent", parent)
	}
	if stageRaw, ok := payload["stage"].(float64); ok && stageRaw >= 1 {
		cliArgs = append(cliArgs, "--stage", strconv.Itoa(int(stageRaw)))
	}

	data, err := runMulticaCLI(ctx, cliArgs...)
	if err != nil {
		return nil, err
	}

	summary := fmt.Sprintf("Dispatched task %q successfully", title)
	if m, ok := data.(map[string]any); ok {
		if id, ok := m["identifier"].(string); ok && id != "" {
			summary = fmt.Sprintf("Dispatched task %s: %q", id, title)
		}
	}
	return compactToolResult(data, summary), nil
}

func (r *Runtime) toolMulticaGetIssue(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	issueID := stringPayload(mcpresult.Arguments(req), "issue_id")
	if issueID == "" {
		return nil, fmt.Errorf("issue_id is required")
	}

	data, err := runMulticaCLI(ctx, "issue", "get", issueID, "--output", "json")
	if err != nil {
		return nil, err
	}
	return compactToolResult(data, fmt.Sprintf("Retrieved issue %s", issueID)), nil
}

func (r *Runtime) toolMulticaCommentIssue(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	payload := mcpresult.Arguments(req)
	issueID := stringPayload(payload, "issue_id")
	if issueID == "" {
		return nil, fmt.Errorf("issue_id is required")
	}
	content := stringPayload(payload, "content")
	if content == "" {
		return nil, fmt.Errorf("content is required")
	}

	tmpFile, err := os.CreateTemp("", "multica-comment-*.md")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file for comment: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if err := os.WriteFile(tmpPath, []byte(content), 0600); err != nil {
		return nil, fmt.Errorf("failed to write comment file: %w", err)
	}
	_ = tmpFile.Close()

	cliArgs := []string{"issue", "comment", "add", issueID, "--content-file", tmpPath, "--allow-external-file", "--output", "json"}
	if parent := stringPayload(payload, "parent_comment_id"); parent != "" {
		cliArgs = append(cliArgs, "--parent", parent)
	}

	data, err := runMulticaCLI(ctx, cliArgs...)
	if err != nil {
		return nil, err
	}
	return compactToolResult(data, fmt.Sprintf("Added comment to issue %s", issueID)), nil
}

func (r *Runtime) toolMulticaListRuns(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	issueID := stringPayload(mcpresult.Arguments(req), "issue_id")
	if issueID == "" {
		return nil, fmt.Errorf("issue_id is required")
	}

	data, err := runMulticaCLI(ctx, "issue", "runs", issueID, "--output", "json")
	if err != nil {
		return nil, err
	}
	return compactToolResult(data, fmt.Sprintf("Listed runs for issue %s", issueID)), nil
}

func (r *Runtime) toolMulticaGetRunMessages(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	payload := mcpresult.Arguments(req)
	taskID := stringPayload(payload, "task_id")
	if taskID == "" {
		return nil, fmt.Errorf("task_id is required")
	}

	cliArgs := []string{"issue", "run-messages", taskID, "--output", "json"}
	if issueID := stringPayload(payload, "issue_id"); issueID != "" {
		cliArgs = append(cliArgs, "--issue", issueID)
	}
	if sinceRaw, ok := payload["since"].(float64); ok && sinceRaw >= 0 {
		cliArgs = append(cliArgs, "--since", strconv.Itoa(int(sinceRaw)))
	}

	data, err := runMulticaCLI(ctx, cliArgs...)
	if err != nil {
		return nil, err
	}
	return compactToolResult(data, fmt.Sprintf("Retrieved messages for task %s", taskID)), nil
}

func (r *Runtime) toolMulticaRerunTask(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	issueID := stringPayload(mcpresult.Arguments(req), "issue_id")
	if issueID == "" {
		return nil, fmt.Errorf("issue_id is required")
	}

	data, err := runMulticaCLI(ctx, "issue", "rerun", issueID, "--output", "json")
	if err != nil {
		return nil, err
	}
	return compactToolResult(data, fmt.Sprintf("Re-enqueued issue %s", issueID)), nil
}

func (r *Runtime) toolMulticaCancelTask(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	payload := mcpresult.Arguments(req)
	taskID := stringPayload(payload, "task_id")
	if taskID == "" {
		return nil, fmt.Errorf("task_id is required")
	}

	cliArgs := []string{"issue", "cancel-task", taskID, "--output", "json"}
	if issueID := stringPayload(payload, "issue_id"); issueID != "" {
		cliArgs = append(cliArgs, "--issue", issueID)
	}

	data, err := runMulticaCLI(ctx, cliArgs...)
	if err != nil {
		return nil, err
	}
	return compactToolResult(data, fmt.Sprintf("Cancelled task %s", taskID)), nil
}
