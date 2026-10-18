package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"mcpx/internal/winproc"
)

// chronicleBinEnv 指向 chronicle 可执行文件（或 .cmd 薄壳），优先于一切自动查找。
// mcpx 作为 Windows 服务运行时，PATH 和用户目录不一定与交互式会话相同，
// 在服务环境里显式设置它是最稳妥的做法。
const chronicleBinEnv = "CHRONICLE_BIN"

var (
	// chronicleTimeout 是单次 chronicle 调用的上限。
	chronicleTimeout = 60 * time.Second
	// chronicleMaxOutput 是读取子进程 stdout 的字节上限；超出部分丢弃并标记 truncated。
	chronicleMaxOutput = 2 << 20
)

const chronicleMaxStderr = 2000

// findChronicleBinary 按顺序查找 chronicle：环境变量 CHRONICLE_BIN、PATH、~/.local/bin。
func findChronicleBinary() (string, error) {
	if override := strings.TrimSpace(os.Getenv(chronicleBinEnv)); override != "" {
		if info, err := os.Stat(override); err != nil || info.IsDir() {
			return "", fmt.Errorf("%s=%q 不是可用文件", chronicleBinEnv, override)
		}
		return override, nil
	}
	if path, err := exec.LookPath("chronicle"); err == nil {
		return path, nil
	}
	names := []string{"chronicle"}
	if runtime.GOOS == "windows" {
		names = []string{"chronicle.cmd", "chronicle.exe", "chronicle.bat"}
	}
	seen := map[string]bool{}
	var homes []string
	if home, err := os.UserHomeDir(); err == nil {
		homes = append(homes, home)
	}
	homes = append(homes, os.Getenv("USERPROFILE"), os.Getenv("HOME"))
	for _, home := range homes {
		if home == "" || seen[home] {
			continue
		}
		seen[home] = true
		for _, name := range names {
			candidate := filepath.Join(home, ".local", "bin", name)
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				return candidate, nil
			}
		}
	}
	return "", fmt.Errorf("找不到 chronicle 命令：请把它加入 PATH、放到 ~/.local/bin，或设置环境变量 %s", chronicleBinEnv)
}

var (
	cmdShimQuoted  = regexp.MustCompile(`"([^"\r\n]+)"`)
	cmdShimPercent = regexp.MustCompile(`%([A-Za-z0-9_]+)%`)
)

// parseCmdShim 解析 chronicle.cmd 这类薄壳（`"<python.exe>" -I "<chronicle.py>" %*`），
// 返回可以绕过 cmd.exe 直接执行的解释器和前置参数。解析不出来时 ok 为 false。
func parseCmdShim(content string, getenv func(string) string) (exe string, pre []string, ok bool) {
	expand := func(s string) string {
		return cmdShimPercent.ReplaceAllStringFunc(s, func(m string) string {
			return getenv(strings.Trim(m, "%"))
		})
	}
	for _, line := range strings.Split(content, "\n") {
		matches := cmdShimQuoted.FindAllStringSubmatchIndex(line, -1)
		exeIdx, scriptIdx := -1, -1
		for i, m := range matches {
			value := strings.ToLower(line[m[2]:m[3]])
			if exeIdx < 0 && strings.HasSuffix(value, ".exe") {
				exeIdx = i
			} else if exeIdx >= 0 && scriptIdx < 0 && strings.HasSuffix(value, ".py") {
				scriptIdx = i
			}
		}
		if exeIdx < 0 || scriptIdx < 0 {
			continue
		}
		exeMatch, scriptMatch := matches[exeIdx], matches[scriptIdx]
		between := line[exeMatch[1]:scriptMatch[0]]
		var flags []string
		for _, field := range strings.Fields(between) {
			if !strings.HasPrefix(field, "-") {
				return "", nil, false
			}
			flags = append(flags, field)
		}
		exe = expand(line[exeMatch[2]:exeMatch[3]])
		script := expand(line[scriptMatch[2]:scriptMatch[3]])
		if _, err := os.Stat(exe); err != nil {
			return "", nil, false
		}
		if _, err := os.Stat(script); err != nil {
			return "", nil, false
		}
		return exe, append(flags, script), true
	}
	return "", nil, false
}

// containsBatchMeta 报告字符串里是否有 cmd.exe 会重新解析的字符。
func containsBatchMeta(s string) bool {
	return strings.ContainsAny(s, "\"%&|<>^!\r\n")
}

func isBatchFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".cmd" || ext == ".bat"
}

// chronicleInvocation 决定实际执行的程序和前置参数。
// Windows 上 chronicle 通常是 .cmd 薄壳：直接交给 exec 会经过 cmd.exe 重新解析参数
// （%*、& | ^ 等），所以先解析薄壳，直接执行里面的解释器；解析不出来才走 cmd.exe，
// 此时必须拒绝含有 cmd 元字符的参数。
func chronicleInvocation(binary string, args []string) (string, []string, error) {
	if runtime.GOOS != "windows" || !isBatchFile(binary) {
		return binary, args, nil
	}
	if content, err := os.ReadFile(binary); err == nil {
		if exe, pre, ok := parseCmdShim(string(content), os.Getenv); ok {
			return exe, append(append([]string{}, pre...), args...), nil
		}
	}
	for _, arg := range args {
		if containsBatchMeta(arg) {
			return "", nil, fmt.Errorf("chronicle 是 .cmd 批处理，参数里不能含有 \" %% & | < > ^ ! 或换行；请设置 %s 指向 chronicle.exe 或解释器脚本", chronicleBinEnv)
		}
	}
	return binary, args, nil
}

// cappedBuffer 最多保存 max 字节，超出的丢弃，但仍返回成功，避免子进程因管道写失败而卡住。
type cappedBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	remaining := c.max - c.buf.Len()
	if remaining <= 0 {
		if len(p) > 0 {
			c.truncated = true
		}
		return len(p), nil
	}
	if len(p) > remaining {
		c.buf.Write(p[:remaining])
		c.truncated = true
		return len(p), nil
	}
	c.buf.Write(p)
	return len(p), nil
}

// runChronicleCLI 以 argv 方式执行 chronicle（不拼 shell 字符串），带超时、输出上限和无窗口设置。
// 输出是 JSON 就解析返回，否则放进 raw_output。
func runChronicleCLI(ctx context.Context, args ...string) (any, error) {
	if len(args) == 0 {
		return nil, errors.New("chronicle: 缺少子命令")
	}
	binary, err := findChronicleBinary()
	if err != nil {
		return nil, err
	}
	exe, fullArgs, err := chronicleInvocation(binary, args)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, chronicleTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, fullArgs...)
	cmd.Env = append(os.Environ(), "PYTHONUTF8=1", "PYTHONIOENCODING=utf-8")
	cmd.WaitDelay = 5 * time.Second
	winproc.ConfigureNoWindow(cmd)

	stdout := &cappedBuffer{max: chronicleMaxOutput}
	stderr := &cappedBuffer{max: chronicleMaxStderr}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	verb := args[0]
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("chronicle %s 超时（%s）", verb, chronicleTimeout)
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("chronicle %s 已取消: %w", verb, ctx.Err())
		}
		detail := strings.TrimSpace(stderr.buf.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.buf.String())
		}
		if detail == "" {
			detail = err.Error()
		}
		return nil, fmt.Errorf("chronicle %s 失败: %s", verb, strings.ToValidUTF8(detail, ""))
	}

	raw := strings.TrimSpace(strings.ToValidUTF8(stdout.buf.String(), ""))
	if stdout.truncated {
		// 被截断的 JSON 无法解析，如实告知调用方缩小范围。
		return map[string]any{
			"truncated":        true,
			"max_output_bytes": chronicleMaxOutput,
			"raw_output":       raw,
			"hint":             "输出超过上限已截断；请减小 limit 或加过滤条件后重试",
		}, nil
	}
	if raw == "" {
		return map[string]any{"status": "ok"}, nil
	}
	var parsed any
	if utf8.ValidString(raw) && json.Unmarshal([]byte(raw), &parsed) == nil {
		return parsed, nil
	}
	return map[string]any{"raw_output": raw}, nil
}
