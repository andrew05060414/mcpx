package readlink

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func publicResolver(context.Context, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
}

func egoOutput(t *testing.T, payload map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return "some console noise\n" + resultMarker + string(raw) + "\n"
}

func newTestReader(run Runner) *EgoReader {
	return NewEgoReader(EgoConfig{
		Bin: "ego-test", Timeout: time.Second, Settle: time.Millisecond, Run: run, Resolve: publicResolver,
		Now: func() time.Time { return time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC) },
	})
}

func mustTarget(t *testing.T, raw string) Target {
	t.Helper()
	target, err := ValidateInputURL(raw)
	if err != nil {
		t.Fatal(err)
	}
	return target
}

func okPayload(url, title, body string) map[string]any {
	return map[string]any{"ok": true, "url": url, "snapshot": map[string]any{
		"title": title, "description": "d", "author": "a", "body": body, "status": 200, "url": url,
	}}
}

func TestBuildScriptIsASCIIAndClosesTaskSpace(t *testing.T) {
	target := mustTarget(t, "https://www.zhihu.com/question/1?q=中文&emoji=😀")
	script, err := BuildScript(target, 45*time.Second, 8*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range script {
		if r > 0x7f {
			t.Fatalf("script has a non-ASCII character at %d: %q", i, r)
		}
	}
	for _, want := range []string{resultMarker, "taskSpace(input.spaceName)", "task.finish({ keep: [] })", taskSpaceName, "\"timeoutMs\":45000", "\"settleMs\":8000", "zhihu"} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q", want)
		}
	}
}

func TestEgoReaderReadsAndCleansArticle(t *testing.T) {
	var gotScript, gotBin string
	var gotTimeout time.Duration
	reader := newTestReader(func(_ context.Context, bin, script string, timeout time.Duration) (string, error) {
		gotBin, gotScript, gotTimeout = bin, script, timeout
		return egoOutput(t, okPayload("https://www.zhihu.com/question/1?token=abc", " Q title ", "Answer body\nCookie: x=1")), nil
	})
	article, err := reader.Read(context.Background(), mustTarget(t, "https://www.zhihu.com/question/1?utm_source=a"))
	if err != nil {
		t.Fatal(err)
	}
	if gotBin != "ego-test" || !strings.Contains(gotScript, "https://www.zhihu.com/question/1?utm_source=a") {
		t.Fatalf("bin=%q script=%q", gotBin, gotScript)
	}
	if gotTimeout != time.Second+time.Millisecond+processOverhead {
		t.Fatalf("process timeout = %s", gotTimeout)
	}
	if article.Title != "Q title" || strings.Contains(article.BodyText, "x=1") || strings.Contains(article.CanonicalURL, "token") {
		t.Fatalf("article = %+v", article)
	}
	if article.SourceURL != "https://www.zhihu.com/question/1" || article.Platform != PlatformZhihu {
		t.Fatalf("source/platform = %+v", article)
	}
}

func TestEgoReaderMapsFailuresToStableCodes(t *testing.T) {
	cases := []struct {
		name string
		run  Runner
		want string
	}{
		{"no result line", func(context.Context, string, string, time.Duration) (string, error) { return "nothing useful", nil }, "BROWSER_UNAVAILABLE"},
		{"bad json", func(context.Context, string, string, time.Duration) (string, error) { return resultMarker + "{", nil }, "READ_FAILED"},
		{"timeout", func(context.Context, string, string, time.Duration) (string, error) { return "", errRunnerTimeout }, "READ_TIMEOUT"},
		{"missing binary", func(context.Context, string, string, time.Duration) (string, error) { return "", exec.ErrNotFound }, "BROWSER_NOT_CONFIGURED"},
		{"other exec error", func(context.Context, string, string, time.Duration) (string, error) { return "", errors.New("boom") }, "READ_FAILED"},
		{"page error", func(context.Context, string, string, time.Duration) (string, error) {
			return egoOutput(t, map[string]any{"ok": false, "message": "net::ERR_FAILED"}), nil
		}, "READ_FAILED"},
		{"page timeout", func(context.Context, string, string, time.Duration) (string, error) {
			return egoOutput(t, map[string]any{"ok": false, "message": "Navigation timeout of 45000 ms exceeded"}), nil
		}, "READ_TIMEOUT"},
		{"redirect off platform", func(context.Context, string, string, time.Duration) (string, error) {
			return egoOutput(t, okPayload("https://evil.example/landing", "t", "body")), nil
		}, "UNSUPPORTED_REDIRECT"},
		{"redirect to other platform", func(context.Context, string, string, time.Duration) (string, error) {
			return egoOutput(t, okPayload("https://www.bilibili.com/video/BV1", "t", "body")), nil
		}, "UNSUPPORTED_REDIRECT"},
		{"login wall", func(context.Context, string, string, time.Duration) (string, error) {
			return egoOutput(t, okPayload("https://www.zhihu.com/signin?next=%2F", "t", "body")), nil
		}, "AUTH_REQUIRED"},
		{"risk control", func(context.Context, string, string, time.Duration) (string, error) {
			return egoOutput(t, okPayload("https://www.zhihu.com/question/1", "t", "请完成安全验证")), nil
		}, "RISK_CONTROLLED"},
		{"empty page", func(context.Context, string, string, time.Duration) (string, error) {
			return egoOutput(t, okPayload("https://www.zhihu.com/question/1", "", "")), nil
		}, "CONTENT_EMPTY"},
		{"title only", func(context.Context, string, string, time.Duration) (string, error) {
			return egoOutput(t, okPayload("https://www.zhihu.com/question/1", "only title", "")), nil
		}, "CONTENT_PARTIAL"},
	}
	for _, tc := range cases {
		_, err := newTestReader(tc.run).Read(context.Background(), mustTarget(t, "https://www.zhihu.com/question/1"))
		if got := errCode(err); got != tc.want {
			t.Fatalf("%s: code %q, want %q (err=%v)", tc.name, got, tc.want, err)
		}
	}
}

func TestEgoReaderRejectsPrivateResolutionOfFinalHost(t *testing.T) {
	reader := NewEgoReader(EgoConfig{
		Bin: "ego-test",
		Run: func(context.Context, string, string, time.Duration) (string, error) {
			return egoOutput(t, okPayload("https://www.zhihu.com/question/1", "t", "body")), nil
		},
		Resolve: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("10.0.0.9")}, nil
		},
	})
	if _, err := reader.Read(context.Background(), mustTarget(t, "https://www.zhihu.com/question/1")); errCode(err) != "SSRF_BLOCKED" {
		t.Fatalf("err = %v", err)
	}
}

func TestEgoReaderReadsOnePageAtATime(t *testing.T) {
	var active, peak int32
	var calls int32
	reader := newTestReader(func(context.Context, string, string, time.Duration) (string, error) {
		now := atomic.AddInt32(&active, 1)
		for {
			seen := atomic.LoadInt32(&peak)
			if now <= seen || atomic.CompareAndSwapInt32(&peak, seen, now) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt32(&active, -1)
		atomic.AddInt32(&calls, 1)
		return egoOutput(t, okPayload("https://www.zhihu.com/question/1", "t", "body")), nil
	})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := reader.Read(context.Background(), mustTarget(t, "https://www.zhihu.com/question/1")); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if peak != 1 || calls != 4 {
		t.Fatalf("peak concurrency %d (want 1), calls %d (want 4)", peak, calls)
	}
}

func TestEgoReaderStopsWaitingWhenContextIsDone(t *testing.T) {
	release := make(chan struct{})
	reader := newTestReader(func(context.Context, string, string, time.Duration) (string, error) {
		<-release
		return egoOutput(t, okPayload("https://www.zhihu.com/question/1", "t", "body")), nil
	})
	go func() { _, _ = reader.Read(context.Background(), mustTarget(t, "https://www.zhihu.com/question/1")) }()
	time.Sleep(30 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := reader.Read(ctx, mustTarget(t, "https://www.zhihu.com/question/1")); errCode(err) != "READ_FAILED" {
		t.Fatalf("queued read must give up with its context, got %v", err)
	}
	close(release)
}

func TestResolveEgoBinPrefersConfiguredPath(t *testing.T) {
	if got := ResolveEgoBin("  C:/x/ego-browser.exe "); got != "C:/x/ego-browser.exe" {
		t.Fatalf("got %q", got)
	}
}
