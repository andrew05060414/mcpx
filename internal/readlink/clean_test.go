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
