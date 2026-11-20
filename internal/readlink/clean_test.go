package readlink

import (
	"strings"
	"testing"
	"time"
)

func TestCleanTextRemovesNoiseAndHidesSecrets(t *testing.T) {
	input := "  first line\u200b \r\n\r\n\r\nCookie: session=abc\nAuthorization = Bearer xyz\nsee https://x.com/a?token=SECRET123&ok=1 now\nlast\x00 line"
	got, truncated := cleanText(input)
	if truncated {
		t.Fatal("short text must not be truncated")
	}
	if strings.ContainsAny(got, "\u200b\x00") {
		t.Fatalf("zero-width or NUL kept: %q", got)
	}
	if strings.Contains(got, "session=abc") || strings.Contains(got, "Bearer xyz") || strings.Contains(got, "SECRET123") {
		t.Fatalf("secret leaked: %q", got)
	}
	want := "first line\n[敏感认证内容已省略]\n[敏感认证内容已省略]\nsee https://x.com/a?token=[redacted]&ok=1 now\nlast line"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestCleanTextTruncatesLongBody(t *testing.T) {
	got, truncated := cleanText(strings.Repeat("字", maxCleanChars+500))
	if !truncated || !strings.HasSuffix(got, truncatedNotice) {
		t.Fatalf("expected truncation notice, truncated=%v", truncated)
	}
	if runeLen(strings.TrimSuffix(got, truncatedNotice)) != maxCleanChars {
		t.Fatalf("kept %d characters", runeLen(strings.TrimSuffix(got, truncatedNotice)))
	}
}

func TestDetectStopStateMapsStopCodes(t *testing.T) {
	cases := []struct {
		name   string
		status int
		url    string
		title  string
		text   string
		want   string
	}{
		{"ok", 200, "https://www.zhihu.com/question/1", "A question", "plain readable answer text", ""},
		{"http 429", 429, "https://www.zhihu.com/q", "t", "x", "RATE_LIMITED"},
		{"rate text", 200, "https://www.zhihu.com/q", "t", "操作频繁，请稍后再试", "RATE_LIMITED"},
		{"http 403", 403, "https://www.zhihu.com/q", "t", "x", "RISK_CONTROLLED"},
		{"captcha", 200, "https://www.xiaohongshu.com/explore/1", "t", "请完成安全验证", "RISK_CONTROLLED"},
		{"website-login error", 200, "https://www.xiaohongshu.com/website-login/error?error_code=300012", "t", "", "RISK_CONTROLLED"},
		{"http 401", 401, "https://x.com/i", "t", "x", "AUTH_REQUIRED"},
		{"login url", 200, "https://www.zhihu.com/signin?next=%2F", "t", "x", "AUTH_REQUIRED"},
		{"login text", 200, "https://x.com/jack", "t", "Sign in to X", "AUTH_REQUIRED"},
	}
	for _, tc := range cases {
		err := DetectStopState(tc.status, tc.url, tc.title, tc.text)
		if got := errCode(err); got != tc.want {
			t.Fatalf("%s: code %q, want %q", tc.name, got, tc.want)
		}
	}
	// Only the first 5000 characters of the body are scanned for stop phrases.
	late := strings.Repeat("a", 6000) + " 登录"
	if err := DetectStopState(200, "https://www.zhihu.com/q", "t", late); err != nil {
		t.Fatalf("stop phrase beyond the scan window must be ignored: %v", err)
	}
}

func TestDetectStopStateDistinguishesArticlesFromStopPages(t *testing.T) {
	cases := []struct {
		name  string
		url   string
		title string
		text  string
		want  string
	}{
		{name: "investment risk", text: "投资有风险，入市需谨慎……"},
		{name: "database challenge", text: "The challenge of scaling databases is real."},
		{name: "router login tutorial", text: "本期视频教你如何登录路由器后台。"},
		{name: "ordinary title", title: "投资风险与登录教程：a challenge", text: "普通正文"},
		{name: "login path in article", text: "打开 /login 或 /signin 查看 website-login 示例。"},
		{name: "risk URL examples in article", text: "website-login/error 和 error_code= 是接口示例。"},
		{name: "security verification", text: "请完成安全验证", want: "RISK_CONTROLLED"},
		{name: "verification code", text: "验证码", want: "RISK_CONTROLLED"},
		{name: "captcha", text: "CAPTCHA", want: "RISK_CONTROLLED"},
		{name: "human verification", text: "Verify you are human", want: "RISK_CONTROLLED"},
		{name: "login prompt", text: "请先登录", want: "AUTH_REQUIRED"},
		{name: "sign in prompt", text: "Sign in to X", want: "AUTH_REQUIRED"},
		{name: "login URL", url: "https://www.zhihu.com/login", want: "AUTH_REQUIRED"},
		{name: "signin URL", url: "https://www.zhihu.com/signin?next=%2F", want: "AUTH_REQUIRED"},
		{name: "website login URL", url: "https://www.xiaohongshu.com/website-login", want: "AUTH_REQUIRED"},
		{name: "website risk URL", url: "https://www.xiaohongshu.com/website-login/error?error_code=300012", want: "RISK_CONTROLLED"},
		{name: "challenge URL", url: "https://www.zhihu.com/challenge", want: "RISK_CONTROLLED"},
		{name: "verification title", title: "安全验证", want: "RISK_CONTROLLED"},
		{name: "login title", title: "请先登录", want: "AUTH_REQUIRED"},
		{name: "verification at scan boundary", text: strings.Repeat("字", 4997) + "验证码", want: "RISK_CONTROLLED"},
		{name: "verification after scan boundary", text: strings.Repeat("字", 5000) + "验证码"},
		{name: "login after scan boundary", text: strings.Repeat("字", 5000) + "登录"},
		{name: "login prompt after scan boundary", text: strings.Repeat("字", 5000) + "请先登录"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.url == "" {
				tc.url = "https://www.zhihu.com/question/1"
			}
			if tc.title == "" {
				tc.title = "A question"
			}
			got := errCode(DetectStopState(200, tc.url, tc.title, tc.text))
			t.Logf("code=%q, want=%q", got, tc.want)
			if got != tc.want {
				t.Fatalf("code %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNormalizeArticle(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

	article, err := NormalizeArticle(PlatformZhihu, "https://www.zhihu.com/q/1", "https://www.zhihu.com/q/1",
		Snapshot{Title: " Title ", Description: "desc", Author: "au", Body: "body text"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if article.Title != "Title" || article.BodyText != "body text" || article.CapturedAt != "2026-11-02T12:00:00Z" ||
		article.CanonicalURL != "https://www.zhihu.com/q/1" {
		t.Fatalf("article = %+v", article)
	}

	withSubtitles, err := NormalizeArticle(PlatformBilibili, "s", "f", Snapshot{Title: "v", Body: "intro", Subtitles: "line a\nline b"}, now)
	if err != nil || withSubtitles.BodyText != "line a\nline b" {
		t.Fatalf("subtitles must replace the body: %+v err=%v", withSubtitles, err)
	}

	if _, err := NormalizeArticle(PlatformX, "s", "f", Snapshot{}, now); errCode(err) != "CONTENT_EMPTY" {
		t.Fatalf("empty = %v", err)
	}
	if _, err := NormalizeArticle(PlatformX, "s", "f", Snapshot{Title: "only title"}, now); errCode(err) != "CONTENT_PARTIAL" {
		t.Fatalf("partial = %v", err)
	}
	untitled, err := NormalizeArticle(PlatformX, "s", "f", Snapshot{Body: "just body"}, now)
	if err != nil || untitled.Title != "x capture" {
		t.Fatalf("untitled = %+v err=%v", untitled, err)
	}
}
