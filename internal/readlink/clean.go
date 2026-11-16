package readlink

import (
	"regexp"
	"strings"
	"time"
)

const (
	maxCleanChars      = 80_000
	truncatedNotice    = "\n\n[正文已截断；请在库中查看完整内容]"
	redactedLine       = "[敏感认证内容已省略]"
	titleMaxChars      = 1000
	descMaxChars       = 4000
	authorMaxChars     = 500
	statusRateLimit    = 429
	statusForbidden    = 403
	statusUnauthorized = 401
)

var (
	secretLine  = regexp.MustCompile(`(?i)(?:^|\b)(?:cookie|set-cookie|authorization|bearer|access_token|refresh_token|auth_token)\s*[:=]`)
	secretQuery = regexp.MustCompile(`(?i)([?&](?:token|access_token|refresh_token|auth|auth_token|authorization|cookie|set-cookie|sign|signature|xsec_token|share_token)=)[^&\s]+`)
	blankRun    = regexp.MustCompile(`\n{3,}`)

	rateLimitPattern = regexp.MustCompile(`(?i)(?:too many requests|请求过于频繁|操作频繁|rate limit)`)
	riskPattern      = regexp.MustCompile(`(?i)(?:captcha|challenge|verify you are human|安全验证|滑块|验证码|风控|风险|访问受限|access denied|website-login.*(?:error|risk)|error_code=)`)
	authPattern      = regexp.MustCompile(`(?i)(?:/login|/signin|website-login|登录|请先登录|sign in)`)
)

// truncateRunes keeps at most max characters (runes).
func truncateRunes(value string, max int) string {
	if max <= 0 {
		return ""
	}
	count := 0
	for i := range value {
		if count == max {
			return value[:i]
		}
		count++
	}
	return value
}

func runeLen(value string) int { return len([]rune(value)) }

// cleanText strips control and zero-width characters, drops blank lines, hides
// credential-looking lines and secret query values, and caps the length.
func cleanText(value string) (text string, truncated bool) {
	value = strings.ReplaceAll(value, "\x00", "")
	value = strings.Map(func(r rune) rune {
		switch r {
		case '\u200b', '\u200c', '\u200d', '\ufeff':
			return -1
		}
		return r
	}, value)
	lines := strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n")
	safe := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if secretLine.MatchString(line) {
			safe = append(safe, redactedLine)
			continue
		}
		safe = append(safe, secretQuery.ReplaceAllString(line, "${1}[redacted]"))
	}
	cleaned := strings.TrimSpace(blankRun.ReplaceAllString(strings.Join(safe, "\n"), "\n\n"))
	if runeLen(cleaned) <= maxCleanChars {
		return cleaned, false
	}
	return truncateRunes(cleaned, maxCleanChars) + truncatedNotice, true
}

// Snapshot is what the in-page extraction script returns.
type Snapshot struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Author      string `json:"author"`
	Body        string `json:"body"`
	Subtitles   string `json:"subtitles"`
	Status      int    `json:"status"`
	URL         string `json:"url"`
}

// DetectStopState maps login, risk-control, and rate-limit pages to stop errors.
// Callers must not retry or work around these states.
func DetectStopState(status int, finalURL, title, text string) error {
	head := text
	if runeLen(head) > 5000 {
		head = truncateRunes(head, 5000)
	}
	location := strings.ToLower(finalURL + "\n" + title + "\n" + head)
	if status == statusRateLimit || rateLimitPattern.MatchString(location) {
		return newError("RATE_LIMITED", "the platform rate-limited this request; retry manually later.", 429)
	}
	if status == statusForbidden || riskPattern.MatchString(location) {
		return newError("RISK_CONTROLLED", "the platform asked for manual verification; automatic bypass is disabled.", 424)
	}
	if status == statusUnauthorized || authPattern.MatchString(location) {
		return newError("AUTH_REQUIRED", "the platform requires a manual login.", 424)
	}
	return nil
}

// Article is a cleaned capture of one page.
type Article struct {
	Platform     string `json:"platform"`
	SourceURL    string `json:"sourceUrl"`
	CanonicalURL string `json:"canonicalUrl"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	Author       string `json:"author"`
	BodyText     string `json:"bodyText"`
	CapturedAt   string `json:"capturedAt"`
}

// NormalizeArticle cleans snapshot fields and rejects empty or body-less captures.
func NormalizeArticle(platform, sourceURL, finalURL string, snap Snapshot, capturedAt time.Time) (Article, error) {
	subtitleText, _ := cleanText(snap.Subtitles)
	source := subtitleText
	if source == "" {
		source = snap.Body
	}
	bodyText, _ := cleanText(source)
	title, _ := cleanText(snap.Title)
	title = truncateRunes(title, titleMaxChars)
	description, _ := cleanText(snap.Description)
	description = truncateRunes(description, descMaxChars)
	if title == "" && bodyText == "" {
		return Article{}, newError("CONTENT_EMPTY", "the page loaded but no readable content was found.", 422)
	}
	if bodyText == "" {
		return Article{}, newError("CONTENT_PARTIAL", "the page title was readable but the body was not.", 422)
	}
	author, _ := cleanText(snap.Author)
	if title == "" {
		title = platform + " capture"
	}
	canonical := finalURL
	if canonical == "" {
		canonical = sourceURL
	}
	return Article{
		Platform:     platform,
		SourceURL:    sourceURL,
		CanonicalURL: canonical,
		Title:        title,
		Description:  description,
		Author:       truncateRunes(author, authorMaxChars),
		BodyText:     bodyText,
		CapturedAt:   capturedAt.UTC().Format(time.RFC3339),
	}, nil
}
