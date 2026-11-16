package readlink

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"mcpx/internal/processutil"
)

//go:embed extract.js
var extractionScript string

const (
	resultMarker   = "__READ_LINK_EGO_RESULT__"
	maxOutputBytes = 4 << 20
	// One fixed task-space name: if a run is killed before it can finish the
	// space, the next run resumes (and finishes) it instead of piling up spaces.
	taskSpaceName   = "mcpx-read-link"
	defaultTimeout  = 45 * time.Second
	defaultSettle   = 8 * time.Second
	processOverhead = 15 * time.Second
)

// Runner executes a script inside Ego Lite's embedded Node runtime and returns
// the combined stdout and stderr (Ego prints script console output on stderr).
type Runner func(ctx context.Context, bin, script string, timeout time.Duration) (string, error)

var errRunnerTimeout = errors.New("ego-browser timeout")

// asciiJSON marshals value with every non-ASCII character escaped so the script
// survives whatever encoding the stdin pipe uses.
func asciiJSON(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	for _, r := range string(raw) {
		switch {
		case r < 0x80:
			out.WriteRune(r)
		case r < 0x10000:
			fmt.Fprintf(&out, "\\u%04x", r)
		default:
			r -= 0x10000
			fmt.Fprintf(&out, "\\u%04x\\u%04x", 0xd800+(r>>10), 0xdc00+(r&0x3ff))
		}
	}
	return out.String(), nil
}

// BuildScript returns the script run inside Ego Lite. It opens its own task
// space and page, reads one URL, prints one marker line, and always closes the
// task space so it never touches the user's own tabs.
func BuildScript(target Target, timeout, settle time.Duration) (string, error) {
	input, err := asciiJSON(map[string]any{
		"spaceName": taskSpaceName,
		"url":       target.NavigationURL,
		"platform":  target.Platform,
		"timeoutMs": timeout.Milliseconds(),
		"settleMs":  settle.Milliseconds(),
	})
	if err != nil {
		return "", err
	}
	return "const input = " + input + ";\n" +
		"const extract = " + strings.TrimSpace(extractionScript) + ";\n" +
		`const task = await taskSpace(input.spaceName);
let out;
try {
  const page = task.page('p1');
  await page.goto(input.url, { waitUntil: 'domcontentloaded', timeout: input.timeoutMs });
  try {
    await page.waitForFunction(() => document.body && document.body.innerText.length > 200, undefined, { timeout: input.settleMs });
  } catch {
    // Short or slow pages are still extracted; the host decides whether the content is usable.
  }
  const url = await page.url();
  const snapshot = await page.evaluate(extract, input.platform);
  out = { ok: true, url, snapshot };
} catch (error) {
  out = { ok: false, message: String((error && error.message) || error).slice(0, 300) };
} finally {
  try { await task.finish({ keep: [] }); } catch {}
}
console.log('` + resultMarker + `' + JSON.stringify(out));
`, nil
}

type boundedBuffer struct {
	buf      bytes.Buffer
	exceeded bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > maxOutputBytes {
		b.exceeded = true
		return len(p), nil
	}
	return b.buf.Write(p)
}

// ExecRunner runs "<bin> nodejs" with the script on stdin.
func ExecRunner(ctx context.Context, bin, script string, timeout time.Duration) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, bin, "nodejs")
	processutil.ConfigureBackground(cmd)
	cmd.Stdin = strings.NewReader(script)
	var out boundedBuffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if runCtx.Err() != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return "", errRunnerTimeout
		}
		return "", runCtx.Err()
	}
	if out.exceeded {
		return "", errors.New("ego-browser output too large")
	}
	if err != nil {
		// A non-zero exit can still carry a result line; let the caller decide.
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return "", err
		}
	}
	return out.buf.String(), nil
}

type egoPayload struct {
	OK       bool     `json:"ok"`
	URL      string   `json:"url"`
	Message  string   `json:"message"`
	Snapshot Snapshot `json:"snapshot"`
}

// EgoConfig configures the Ego Lite reader.
type EgoConfig struct {
	Bin     string
	Timeout time.Duration
	Settle  time.Duration
	Run     Runner
	Resolve Resolver
	Now     func() time.Time
}

// ResolveEgoBin picks the Ego Lite CLI: the configured path, then PATH, then the
// per-user install location (a Windows service has a different PATH than the user).
func ResolveEgoBin(configured string) string {
	if strings.TrimSpace(configured) != "" {
		return strings.TrimSpace(configured)
	}
	if found, err := exec.LookPath("ego-browser"); err == nil {
		return found
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, name := range []string{"ego-browser.exe", "ego-browser"} {
			candidate := filepath.Join(home, ".local", "share", "ego", "active_version_dir", name)
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	}
	return "ego-browser"
}

// EgoReader reads one page at a time through the logged-in Ego Lite browser.
// Reads are serialized: low frequency keeps the logged-in accounts away from risk controls.
type EgoReader struct {
	cfg   EgoConfig
	slots chan struct{}
}

// NewEgoReader applies defaults and returns a reader.
func NewEgoReader(cfg EgoConfig) *EgoReader {
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.Settle <= 0 {
		cfg.Settle = defaultSettle
	}
	if cfg.Run == nil {
		cfg.Run = ExecRunner
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	cfg.Bin = ResolveEgoBin(cfg.Bin)
	return &EgoReader{cfg: cfg, slots: make(chan struct{}, 1)}
}

// Read opens target in Ego Lite and returns the cleaned article.
func (r *EgoReader) Read(ctx context.Context, target Target) (Article, error) {
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	case <-ctx.Done():
		return Article{}, newError("READ_FAILED", "the page could not be read with Ego Lite.", 502)
	}
	payload, err := r.run(ctx, target)
	if err != nil {
		return Article{}, err
	}
	if !payload.OK {
		message := strings.ToLower(payload.Message)
		if strings.Contains(message, "timeout") || strings.Contains(message, "timed out") {
			return Article{}, newError("READ_TIMEOUT", "the page did not finish loading within the time limit.", 504)
		}
		return Article{}, newError("READ_FAILED", "the page could not be read with Ego Lite.", 502)
	}
	final, err := ValidateFinalURL(payload.URL, target.Platform)
	if err != nil {
		return Article{}, err
	}
	if err := AssertPublicHost(ctx, final.Host, r.cfg.Resolve); err != nil {
		return Article{}, err
	}
	snap := payload.Snapshot
	if err := DetectStopState(snap.Status, final.URL, snap.Title, snap.Body); err != nil {
		return Article{}, err
	}
	return NormalizeArticle(final.Platform, target.SourceURL, final.SourceURL, snap, r.cfg.Now())
}

func (r *EgoReader) run(ctx context.Context, target Target) (egoPayload, error) {
	script, err := BuildScript(target, r.cfg.Timeout, r.cfg.Settle)
	if err != nil {
		return egoPayload{}, newError("READ_FAILED", "the page could not be read with Ego Lite.", 502)
	}
	// The child needs a little longer than the page timeout to start, evaluate and close the task space.
	output, err := r.cfg.Run(ctx, r.cfg.Bin, script, r.cfg.Timeout+r.cfg.Settle+processOverhead)
	if err != nil {
		switch {
		case errors.Is(err, errRunnerTimeout):
			return egoPayload{}, newError("READ_TIMEOUT", "the page did not finish loading within the time limit.", 504)
		case errors.Is(err, exec.ErrNotFound), errors.Is(err, fs.ErrNotExist):
			return egoPayload{}, newError("BROWSER_NOT_CONFIGURED", "ego-browser was not found; set read_link.ego_bin in the MCPX config.", 503)
		default:
			return egoPayload{}, newError("READ_FAILED", "the page could not be read with Ego Lite.", 502)
		}
	}
	lines := strings.Split(output, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimRight(lines[i], "\r")
		if !strings.HasPrefix(line, resultMarker) {
			continue
		}
		var payload egoPayload
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, resultMarker)), &payload); err != nil {
			return egoPayload{}, newError("READ_FAILED", "the page could not be read with Ego Lite.", 502)
		}
		return payload, nil
	}
	return egoPayload{}, newError("BROWSER_UNAVAILABLE", "Ego Lite did not return a result; check that it is running.", 503)
}
