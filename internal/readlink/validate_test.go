package readlink

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
)

func errCode(err error) string {
	var typed *Error
	if errors.As(err, &typed) {
		return typed.Code
	}
	if err == nil {
		return ""
	}
	return "non-readlink-error"
}

func TestValidateInputURLAcceptsApprovedPlatforms(t *testing.T) {
	cases := map[string]string{
		"https://www.zhihu.com/question/1/answer/2":    PlatformZhihu,
		"https://zhuanlan.zhihu.com/p/123":             PlatformZhihu,
		"https://www.bilibili.com/video/BV16hb56JEeU/": PlatformBilibili,
		"https://x.com/jack/status/20":                 PlatformX,
		"https://twitter.com/jack/status/20":           PlatformX,
		"https://www.xiaohongshu.com/explore/abc":      PlatformXiaohongshu,
		"https://b23.tv/abc":                           PlatformBilibili,
		"https://www.zhihu.com:443/question/1":         PlatformZhihu,
		"https://WWW.ZHIHU.COM./question/1":            PlatformZhihu,
	}
	for raw, platform := range cases {
		target, err := ValidateInputURL(raw)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if target.Platform != platform {
			t.Fatalf("%s: platform %q, want %q", raw, target.Platform, platform)
		}
	}
}

func TestValidateInputURLRejectsUnsafeOrUnsupported(t *testing.T) {
	cases := map[string]string{
		"":                                "INVALID_URL",
		"not a url":                       "INVALID_URL",
		"http://www.zhihu.com/question/1": "INVALID_URL",
		"https://user:pw@www.zhihu.com/q": "INVALID_URL",
		"https://www.zhihu.com:8443/q":    "INVALID_URL",
		"https://127.0.0.1/q":             "SSRF_BLOCKED",
		"https://[::1]/q":                 "SSRF_BLOCKED",
		"https://10.0.0.5/q":              "SSRF_BLOCKED",
		"https://localhost/q":             "SSRF_BLOCKED",
		"https://nas.internal/q":          "SSRF_BLOCKED",
		"https://example.com/q":           "UNSUPPORTED_URL",
		"https://zhihu.com.evil.com/q":    "UNSUPPORTED_URL",
		"https://evilzhihu.com/q":         "UNSUPPORTED_URL",
		"https://8.8.8.8/q":               "UNSUPPORTED_URL",
		"https://sub.www.zhihu.com/q":     "UNSUPPORTED_URL",
		"ftp://www.zhihu.com/q":           "INVALID_URL",
	}
	for raw, want := range cases {
		_, err := ValidateInputURL(raw)
		if got := errCode(err); got != want {
			t.Fatalf("%q: code %q, want %q", raw, got, want)
		}
	}
	long := "https://www.zhihu.com/" + strings.Repeat("a", 5000)
	if _, err := ValidateInputURL(long); errCode(err) != "INVALID_URL" {
		t.Fatalf("long url must be rejected, got %v", err)
	}
}

func TestValidateInputURLKeepsNavigationURLAndSanitizesSourceURL(t *testing.T) {
	raw := "https://www.xiaohongshu.com/explore/abc?xsec_token=SECRET&utm_source=x&keep=1&source=web#frag"
	target, err := ValidateInputURL(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(target.NavigationURL, "xsec_token=SECRET") {
		t.Fatalf("navigation URL must keep the share token the platform needs: %s", target.NavigationURL)
	}
	if strings.Contains(target.SourceURL, "SECRET") || strings.Contains(target.SourceURL, "utm_") ||
		strings.Contains(target.SourceURL, "source=") || strings.Contains(target.SourceURL, "#") {
		t.Fatalf("source URL still carries secrets or tracking: %s", target.SourceURL)
	}
	if target.SourceURL != "https://www.xiaohongshu.com/explore/abc?keep=1" {
		t.Fatalf("source URL = %s", target.SourceURL)
	}
}

func TestValidateFinalURL(t *testing.T) {
	ok, err := ValidateFinalURL("https://www.zhihu.com/question/1?token=abc", PlatformZhihu)
	if err != nil || ok.Platform != PlatformZhihu || strings.Contains(ok.SourceURL, "token") {
		t.Fatalf("final = %+v err=%v", ok, err)
	}
	cases := map[string]string{
		"https://www.bilibili.com/video/BV1": "UNSUPPORTED_REDIRECT", // other approved platform
		"https://evil.example/x":             "UNSUPPORTED_REDIRECT",
		"https://b23.tv/abc":                 "UNSUPPORTED_REDIRECT", // short hosts are input-only
		"http://www.zhihu.com/question/1":    "SSRF_BLOCKED",
		"https://u:p@www.zhihu.com/question": "SSRF_BLOCKED",
		"%%%":                                "INVALID_REDIRECT",
	}
	for raw, want := range cases {
		if _, err := ValidateFinalURL(raw, PlatformZhihu); errCode(err) != want {
			t.Fatalf("%q: code %q, want %q", raw, errCode(err), want)
		}
	}
}

func TestPrivateAddressRanges(t *testing.T) {
	private := []string{"0.0.0.1", "10.1.2.3", "127.0.0.1", "100.64.0.1", "169.254.1.1", "172.16.0.1", "172.31.255.1",
		"192.168.1.1", "192.0.2.1", "198.18.0.1", "203.0.113.9", "224.0.0.1", "255.255.255.255",
		"::", "::1", "fc00::1", "fd12::1", "fe80::1", "ff02::1", "::ffff:127.0.0.1", "::ffff:10.0.0.1"}
	for _, ip := range private {
		if !isPrivateIP(ip) {
			t.Fatalf("%s must be private", ip)
		}
	}
	public := []string{"8.8.8.8", "1.1.1.1", "172.32.0.1", "100.63.255.255", "198.20.0.1", "2606:4700:4700::1111", "::ffff:8.8.8.8"}
	for _, ip := range public {
		if isPrivateIP(ip) {
			t.Fatalf("%s must be public", ip)
		}
	}
	if isPrivateIP("www.zhihu.com") {
		t.Fatal("hostnames are not IP literals")
	}
}

func TestAssertPublicHost(t *testing.T) {
	resolverFor := func(addrs ...string) Resolver {
		return func(context.Context, string) ([]netip.Addr, error) {
			out := make([]netip.Addr, 0, len(addrs))
			for _, a := range addrs {
				out = append(out, netip.MustParseAddr(a))
			}
			return out, nil
		}
	}
	ctx := context.Background()
	if err := AssertPublicHost(ctx, "www.zhihu.com", resolverFor("8.8.8.8")); err != nil {
		t.Fatal(err)
	}
	if got := errCode(AssertPublicHost(ctx, "www.zhihu.com", resolverFor("8.8.8.8", "10.0.0.1"))); got != "SSRF_BLOCKED" {
		t.Fatalf("mixed answers must be blocked, got %q", got)
	}
	if got := errCode(AssertPublicHost(ctx, "www.zhihu.com", resolverFor())); got != "SSRF_BLOCKED" {
		t.Fatalf("empty answers must be blocked, got %q", got)
	}
	failing := func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("nxdomain") }
	if got := errCode(AssertPublicHost(ctx, "www.zhihu.com", failing)); got != "HOST_RESOLUTION_FAILED" {
		t.Fatalf("resolution failure = %q", got)
	}
	if got := errCode(AssertPublicHost(ctx, "localhost", resolverFor("8.8.8.8"))); got != "SSRF_BLOCKED" {
		t.Fatalf("localhost = %q", got)
	}
}
