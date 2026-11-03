package readlink

import (
	"context"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
)

// Platform names accepted by read_link.
const (
	PlatformXiaohongshu = "xiaohongshu"
	PlatformBilibili    = "bilibili"
	PlatformZhihu       = "zhihu"
	PlatformX           = "x"
)

var platformHosts = map[string][]string{
	PlatformXiaohongshu: {"xiaohongshu.com", "www.xiaohongshu.com"},
	PlatformBilibili:    {"bilibili.com", "www.bilibili.com"},
	PlatformZhihu:       {"zhihu.com", "www.zhihu.com", "zhuanlan.zhihu.com"},
	PlatformX:           {"x.com", "www.x.com", "twitter.com", "www.twitter.com"},
}

var shortHosts = map[string][]string{
	PlatformXiaohongshu: {"xhslink.com", "www.xhslink.com"},
	PlatformBilibili:    {"b23.tv", "www.b23.tv"},
	PlatformX:           {"t.co"},
}

var (
	secretQueryKey   = regexp.MustCompile(`(?i)^(?:token|access_token|refresh_token|auth|auth_token|authorization|cookie|set-cookie|sign|signature|xsec_token|share_token)$`)
	trackingQueryKey = regexp.MustCompile(`(?i)^(?:utm_.+|spm_id_from|share_source|share_medium|source)$`)
)

// Target is a validated input URL.
type Target struct {
	NavigationURL string // what the browser opens (keeps the share token the platform needs)
	SourceURL     string // sanitized: no fragment, secret or tracking query keys
	Host          string
	Platform      string
	ShortHost     bool
}

// FinalURL is a validated post-redirect URL.
type FinalURL struct {
	URL       string
	SourceURL string
	Host      string
	Platform  string
}

// Resolver looks up host addresses; replaced in tests.
type Resolver func(ctx context.Context, host string) ([]netip.Addr, error)

// DefaultResolver uses the system resolver.
func DefaultResolver(ctx context.Context, host string) ([]netip.Addr, error) {
	records, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	return records, nil
}

func normalizeHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

func containsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func exactHostPlatform(host string, table map[string][]string) string {
	for platform, hosts := range table {
		if containsString(hosts, host) {
			return platform
		}
	}
	return ""
}

func platformForHost(host string) string {
	if platform := exactHostPlatform(host, platformHosts); platform != "" {
		return platform
	}
	return exactHostPlatform(host, shortHosts)
}

func isShortHost(host string) bool {
	for _, hosts := range shortHosts {
		if containsString(hosts, host) {
			return true
		}
	}
	return false
}

// isPrivateIP reports whether host is an IP literal in a private, loopback,
// link-local, multicast, or otherwise non-public range. Unparsable IPv6 literals
// count as private so they fail closed. Hostnames that are not IP literals return false.
func isPrivateIP(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"))
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return strings.Contains(host, ":")
	}
	return isPrivateAddr(addr)
}

func isPrivateAddr(addr netip.Addr) bool {
	if addr.Zone() != "" {
		addr = addr.WithZone("")
	}
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	if addr.Is4() {
		o := addr.As4()
		a, b := o[0], o[1]
		return a == 0 || a == 10 || a == 127 || (a == 100 && b >= 64 && b <= 127) ||
			(a == 169 && b == 254) || (a == 172 && b >= 16 && b <= 31) ||
			(a == 192 && (b == 0 || b == 168)) || (a == 198 && b >= 18 && b <= 19) ||
			(a == 203 && b == 0) || a >= 224
	}
	if addr.IsUnspecified() || addr.IsLoopback() {
		return true
	}
	b := addr.As16()
	return (b[0]&0xfe) == 0xfc || (b[0] == 0xfe && (b[1]&0xc0) == 0x80) || b[0] == 0xff
}

func isBlockedHostname(host string) bool {
	host = normalizeHost(host)
	return host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") ||
		strings.HasSuffix(host, ".internal") || isPrivateIP(host)
}

func isIPLiteral(host string) bool {
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	return false
}

// sanitizeURL drops the fragment and secret or tracking query keys while
// leaving the remaining query bytes untouched.
func sanitizeURL(u *url.URL) string {
	clean := *u
	clean.Fragment = ""
	clean.RawFragment = ""
	if clean.RawQuery != "" {
		kept := make([]string, 0)
		for _, pair := range strings.Split(clean.RawQuery, "&") {
			if pair == "" {
				continue
			}
			key := pair
			if i := strings.IndexByte(pair, '='); i >= 0 {
				key = pair[:i]
			}
			if decoded, err := url.QueryUnescape(key); err == nil {
				key = decoded
			}
			if secretQueryKey.MatchString(key) || trackingQueryKey.MatchString(key) {
				continue
			}
			kept = append(kept, pair)
		}
		clean.RawQuery = strings.Join(kept, "&")
	}
	return clean.String()
}

// ValidateInputURL accepts one HTTPS URL on an approved platform host.
func ValidateInputURL(raw string) (Target, error) {
	if raw == "" || len(raw) > 4096 {
		return Target{}, newError("INVALID_URL", "url must be a non-empty string up to 4096 characters.", 400)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return Target{}, newError("INVALID_URL", "url is not a valid URL.", 400)
	}
	host := normalizeHost(u.Hostname())
	if u.Scheme != "https" {
		return Target{}, newError("INVALID_URL", "only HTTPS platform URLs are accepted.", 400)
	}
	if u.User != nil || (u.Port() != "" && u.Port() != "443") {
		return Target{}, newError("INVALID_URL", "credentials and non-default ports are not accepted.", 400)
	}
	if isBlockedHostname(host) {
		return Target{}, newError("SSRF_BLOCKED", "private, local, and IP addresses are not accepted.", 400)
	}
	platform := platformForHost(host)
	if platform == "" {
		return Target{}, newError("UNSUPPORTED_URL", "only Xiaohongshu, Bilibili, Zhihu, and X URLs are accepted.", 400)
	}
	return Target{
		NavigationURL: u.String(),
		SourceURL:     sanitizeURL(u),
		Host:          host,
		Platform:      platform,
		ShortHost:     isShortHost(host),
	}, nil
}

// ValidateFinalURL re-checks the URL the page ended on after redirects.
func ValidateFinalURL(raw, expectedPlatform string) (FinalURL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return FinalURL{}, newError("INVALID_REDIRECT", "the page redirected to an invalid URL.", 400)
	}
	host := normalizeHost(u.Hostname())
	platform := exactHostPlatform(host, platformHosts)
	if platform == "" || (expectedPlatform != "" && platform != expectedPlatform) {
		return FinalURL{}, newError("UNSUPPORTED_REDIRECT", "the page redirected outside the approved platform host.", 400)
	}
	if u.Scheme != "https" || u.User != nil || isBlockedHostname(host) {
		return FinalURL{}, newError("SSRF_BLOCKED", "the page redirected to an unsafe target.", 400)
	}
	return FinalURL{URL: u.String(), SourceURL: sanitizeURL(u), Host: host, Platform: platform}, nil
}

// AssertPublicHost rejects hosts that are, or resolve to, non-public addresses.
func AssertPublicHost(ctx context.Context, host string, resolve Resolver) error {
	host = normalizeHost(host)
	if isBlockedHostname(host) {
		return newError("SSRF_BLOCKED", "private, local, and IP addresses are not accepted.", 400)
	}
	if isIPLiteral(host) {
		return nil
	}
	if resolve == nil {
		resolve = DefaultResolver
	}
	records, err := resolve(ctx, host)
	if err != nil {
		return newError("HOST_RESOLUTION_FAILED", "the platform host could not be resolved.", 502)
	}
	if len(records) == 0 {
		return newError("SSRF_BLOCKED", "the platform host resolved to a private address.", 400)
	}
	for _, record := range records {
		if isPrivateAddr(record) {
			return newError("SSRF_BLOCKED", "the platform host resolved to a private address.", 400)
		}
	}
	return nil
}
