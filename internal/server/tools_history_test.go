package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestMain 让测试二进制在设置了 MCPX_FAKE_CHRONICLE 时扮演假的 chronicle 可执行文件。
// 这样测试不依赖真实的 chronicle，也不需要 shell 脚本，Windows 和 Unix 通用。
func TestMain(m *testing.M) {
	if mode := os.Getenv("MCPX_FAKE_CHRONICLE"); mode != "" {
		os.Exit(fakeChronicleMain(mode))
	}
	os.Exit(m.Run())
}

func fakeChronicleMain(mode string) int {
	switch mode {
	case "echo":
		body := map[string]any{"args": os.Args[1:], "pythonutf8": os.Getenv("PYTHONUTF8")}
		if len(os.Args) > 1 && os.Args[1] == "messages" {
			body["messages"] = []any{}
		}
		encoded, _ := json.Marshal(body)
		fmt.Println(string(encoded))
	case "text":
		fmt.Println("not json at all")
	case "empty":
	case "big":
		_, _ = io.WriteString(os.Stdout, strings.Repeat("x", 4096))
	case "fail":
		fmt.Fprintln(os.Stderr, "boom: agentsview unavailable")
		return 3
	case "slow":
		time.Sleep(30 * time.Second)
	}
	return 0
}

// useFakeChronicle 把 CHRONICLE_BIN 指向测试二进制自身，并选择假的行为模式。
func useFakeChronicle(t *testing.T, mode string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(chronicleBinEnv, self)
	t.Setenv("MCPX_FAKE_CHRONICLE", mode)
}

func callHistory(t *testing.T, runtime *Runtime, name, arguments string) (map[string]any, error) {
	t.Helper()
	handlers := map[string]func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error){
		"history_search":   runtime.toolHistorySearch,
		"history_get":      runtime.toolHistoryGet,
		"history_show":     runtime.toolHistoryShow,
		"history_list":     runtime.toolHistoryList,
		"history_projects": runtime.toolHistoryProjects,
	}
	result, err := handlers[name](context.Background(), &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Name: name, Arguments: []byte(arguments)},
	})
	if err != nil {
		return nil, err
	}
	structured, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured content is %T", result.StructuredContent)
	}
	return structured, nil
}

func TestHistoryToolsRegistered(t *testing.T) {
	runtime := newWorkspaceRuntime(t, "demo")
	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "1.0.0"}, nil)
	runtime.registerTools(server)

	toolMap := runtime.listedToolMap()
	for _, name := range []string{"history_search", "history_get", "history_show", "history_list", "history_projects"} {
		tool, ok := toolMap[name]
		if !ok {
			t.Errorf("expected tool %q to be registered in tool catalog", name)
			continue
		}
		if strings.Contains(strings.ToLower(tool.Description), "hstry") {
			t.Errorf("tool %q description still mentions hstry: %s", name, tool.Description)
		}
	}
	for _, name := range []string{"history_peek", "history_stats"} {
		if _, ok := toolMap[name]; ok {
			t.Errorf("retired hstry alias %q must not be registered", name)
		}
	}
}

func TestHistoryToolValidation(t *testing.T) {
	runtime := newWorkspaceRuntime(t, "demo")
	useFakeChronicle(t, "echo")
	for _, tc := range []struct{ tool, args string }{
		{"history_search", `{}`},
		{"history_search", `{"query":"a","any":["b"]}`},
		{"history_search", `{"any":["-x"]}`},
		{"history_search", `{"any":["a"],"mode":"semantic"}`},
		{"history_search", `{"query":"a","mode":"bogus"}`},
		{"history_search", `{"query":"a","since":"--machine=x"}`},
		{"history_get", `{}`},
		{"history_get", `{"conversation_id":"--json"}`},
		{"history_show", `{}`},
		{"history_show", `{"conversation_id":"abc","direction":"sideways"}`},
		{"history_list", `{"agent":"-rf"}`},
	} {
		if _, err := callHistory(t, runtime, tc.tool, tc.args); err == nil {
			t.Errorf("%s %s: expected validation error", tc.tool, tc.args)
		}
	}
}

func TestBuildHistoryArgs(t *testing.T) {
	decode := func(s string) map[string]any {
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	tests := []struct {
		name  string
		build func(map[string]any) ([]string, error)
		input string
		want  []string
	}{
		{"search default", buildHistorySearchArgs, `{"query":"hello world"}`,
			[]string{"search", "--json", "--include-one-shot", "--limit", "8", "--fts", "--", "hello world"}},
		{"search filters+mode", buildHistorySearchArgs,
			`{"query":"-dash; rm -rf $HOME & \"x\"","mode":"hybrid","limit":3,"agent":"claude","project":"mcpx","since":"2w","date_from":"2026-01-01","date_to":"2026-02-01"}`,
			[]string{"search", "--json", "--include-one-shot", "--limit", "3", "--hybrid", "--agent", "claude", "--project", "mcpx", "--since", "2w", "--date-from", "2026-01-01", "--date-to", "2026-02-01", "--", `-dash; rm -rf $HOME & "x"`}},
		{"search any", buildHistorySearchArgs, `{"any":["hstry 退役","chronicle"],"limit":9999}`,
			[]string{"search", "--json", "--include-one-shot", "--limit", "500", "--fts", "--any", "hstry 退役", "chronicle"}},
		{"get", buildHistoryGetArgs, `{"conversation_id":" abc-123 "}`,
			[]string{"get", "--json", "--", "abc-123"}},
		{"messages default", buildHistoryMessagesArgs, `{"conversation_id":"abc"}`,
			[]string{"messages", "--json", "--limit", "50", "--role", "user,assistant", "--", "abc"}},
		{"messages window", buildHistoryMessagesArgs,
			`{"conversation_id":"abc","around":10,"before":2,"after":3,"from":0,"limit":7,"direction":"desc","role":"user,assistant"}`,
			[]string{"messages", "--json", "--limit", "7", "--from", "0", "--around", "10", "--before", "2", "--after", "3", "--direction", "desc", "--role", "user,assistant", "--", "abc"}},
		{"list", buildHistoryListArgs, `{"project":"mcpx"}`,
			[]string{"list", "--json", "--include-one-shot", "--limit", "10", "--project", "mcpx"}},
	}
	for _, tc := range tests {
		got, err := tc.build(decode(tc.input))
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s:\n got  %q\n want %q", tc.name, got, tc.want)
		}
	}
}

func TestChronicleBinaryEnvOverrideWins(t *testing.T) {
	useFakeChronicle(t, "echo")
	self, _ := os.Executable()
	got, err := findChronicleBinary()
	if err != nil || got != self {
		t.Fatalf("findChronicleBinary() = %q, %v; want %q", got, err, self)
	}

	t.Setenv(chronicleBinEnv, filepath.Join(t.TempDir(), "missing"))
	if _, err := findChronicleBinary(); err == nil || !strings.Contains(err.Error(), chronicleBinEnv) {
		t.Fatalf("missing override must fail and name %s, got %v", chronicleBinEnv, err)
	}
}

func TestChronicleBinaryDiscoveryFromPathAndHome(t *testing.T) {
	self, _ := os.Executable()
	name := "chronicle"
	if runtime.GOOS == "windows" {
		name = "chronicle.exe"
	}

	// PATH
	dir := t.TempDir()
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(chronicleBinEnv, "")
	t.Setenv("PATH", dir)
	got, err := findChronicleBinary()
	if err != nil || !strings.EqualFold(filepath.Dir(got), dir) {
		t.Fatalf("PATH discovery = %q, %v", got, err)
	}

	// ~/.local/bin
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, name), data, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	got, err = findChronicleBinary()
	if err != nil || !strings.EqualFold(filepath.Dir(got), bin) {
		t.Fatalf("home discovery = %q, %v", got, err)
	}

	// 都没有
	empty := t.TempDir()
	t.Setenv("PATH", empty)
	t.Setenv("HOME", empty)
	t.Setenv("USERPROFILE", empty)
	if _, err := findChronicleBinary(); err == nil {
		t.Fatal("expected not-found error")
	}
}

func TestRunChronicleCLIPassesArgvVerbatim(t *testing.T) {
	runtime := newWorkspaceRuntime(t, "demo")
	useFakeChronicle(t, "echo")
	evil := `a & b | c > d "e" 'f' $(g) ` + "`h`" + ` %PATH% ^ !x`
	out, err := callHistory(t, runtime, "history_search", mustJSON(t, map[string]any{"query": evil}))
	if err != nil {
		t.Fatal(err)
	}
	data := out["data"].(map[string]any)
	args := data["args"].([]any)
	if args[len(args)-1] != evil || args[len(args)-2] != "--" {
		t.Fatalf("query was not passed as one verbatim argument: %v", args)
	}
	if data["pythonutf8"] != "1" {
		t.Fatalf("PYTHONUTF8 not forced for child: %v", data["pythonutf8"])
	}
}

func TestRunChronicleCLIOutputShapes(t *testing.T) {
	cases := []struct {
		mode  string
		check func(t *testing.T, data any, err error)
	}{
		{"text", func(t *testing.T, data any, err error) {
			if err != nil || data.(map[string]any)["raw_output"] != "not json at all" {
				t.Fatalf("data=%v err=%v", data, err)
			}
		}},
		{"empty", func(t *testing.T, data any, err error) {
			if err != nil || data.(map[string]any)["status"] != "ok" {
				t.Fatalf("data=%v err=%v", data, err)
			}
		}},
		{"fail", func(t *testing.T, data any, err error) {
			if err == nil || !strings.Contains(err.Error(), "boom: agentsview unavailable") || !strings.Contains(err.Error(), "search") {
				t.Fatalf("expected error carrying stderr, got data=%v err=%v", data, err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			useFakeChronicle(t, tc.mode)
			data, err := runChronicleCLI(context.Background(), "search", "--json", "--", "q")
			tc.check(t, data, err)
		})
	}
}

func TestRunChronicleCLITruncatesLargeOutput(t *testing.T) {
	useFakeChronicle(t, "big")
	old := chronicleMaxOutput
	chronicleMaxOutput = 100
	defer func() { chronicleMaxOutput = old }()
	data, err := runChronicleCLI(context.Background(), "list")
	if err != nil {
		t.Fatal(err)
	}
	m := data.(map[string]any)
	if m["truncated"] != true || len(m["raw_output"].(string)) != 100 {
		t.Fatalf("expected truncated 100-byte output, got truncated=%v len=%d", m["truncated"], len(m["raw_output"].(string)))
	}
}

func TestRunChronicleCLITimeout(t *testing.T) {
	useFakeChronicle(t, "slow")
	old := chronicleTimeout
	chronicleTimeout = 300 * time.Millisecond
	defer func() { chronicleTimeout = old }()
	start := time.Now()
	_, err := runChronicleCLI(context.Background(), "list")
	if err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("expected timeout error, got %v", err)
	}
	if time.Since(start) > 15*time.Second {
		t.Fatalf("timeout did not stop the child promptly: %s", time.Since(start))
	}
}

func TestParseCmdShim(t *testing.T) {
	dir := t.TempDir()
	python := filepath.Join(dir, "python.exe")
	script := filepath.Join(dir, "chronicle.py")
	for _, p := range []string{python, script} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	getenv := func(k string) string {
		if k == "SHIMDIR" {
			return dir
		}
		return ""
	}
	exe, pre, ok := parseCmdShim("@\"%SHIMDIR%\\python.exe\" -I \"%SHIMDIR%\\chronicle.py\" %*\r\n", getenv)
	// 路径分隔符在 Unix 上不会被 Stat 识别，只在 Windows 上要求解析成功。
	if runtime.GOOS == "windows" {
		if !ok || exe != dir+`\python.exe` || !reflect.DeepEqual(pre, []string{"-I", dir + `\chronicle.py`}) {
			t.Fatalf("exe=%q pre=%q ok=%v", exe, pre, ok)
		}
	}
	// 不是 python 薄壳的批处理不解析。
	if _, _, ok := parseCmdShim("@echo off\r\nsomething.exe %*\r\n", getenv); ok {
		t.Fatal("non-shim batch file must not be parsed")
	}
	// 解释器不存在时不解析。
	if _, _, ok := parseCmdShim(`"C:\nope\python.exe" "C:\nope\chronicle.py" %*`, getenv); ok {
		t.Fatal("missing interpreter must not be parsed")
	}
}

func TestChronicleInvocationRejectsBatchMetacharacters(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("batch files only matter on Windows")
	}
	bat := filepath.Join(t.TempDir(), "chronicle.cmd")
	if err := os.WriteFile(bat, []byte("@echo off\r\necho %*\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := chronicleInvocation(bat, []string{"search", "--", `a & calc`}); err == nil {
		t.Fatal("expected rejection of cmd metacharacters for an unparseable .cmd")
	}
	if _, _, err := chronicleInvocation(bat, []string{"search", "--", "plain words"}); err != nil {
		t.Fatalf("plain args must be allowed: %v", err)
	}
}
