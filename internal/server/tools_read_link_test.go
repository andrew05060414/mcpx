package server

import (
	"context"
	"net/netip"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/config"
	"mcpx/internal/readlink"
)

type stubLinkReader struct {
	article readlink.Article
	err     error
	urls    []string
}

func (s *stubLinkReader) Read(_ context.Context, target readlink.Target) (readlink.Article, error) {
	s.urls = append(s.urls, target.NavigationURL)
	return s.article, s.err
}

func stubLinkRuntime(reader readlink.Reader) *Runtime {
	return &Runtime{readLinkSvc: &readlink.Service{
		Reader: reader,
		Store:  readlink.NewMemoryStore(),
		Resolve: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		},
	}}
}

func TestReadLinkIsListedAsANativeTool(t *testing.T) {
	runtime := &Runtime{}
	protocol := mcp.NewServer(&mcp.Implementation{Name: "mcpx-test", Version: "0.1.0"}, nil)
	runtime.registerTools(protocol)
	tool, ok := runtime.listedToolMap()["read_link"]
	if !ok {
		t.Fatal("read_link must appear in tools/list without discover or mcp_tool")
	}
	if tool.Annotations == nil || tool.Annotations.ReadOnlyHint || tool.Annotations.OpenWorldHint == nil || !*tool.Annotations.OpenWorldHint {
		t.Fatalf("read_link writes to Karakeep and opens the web: %+v", tool.Annotations)
	}
	found := false
	for _, name := range capabilityToolNames() {
		found = found || name == "read_link"
	}
	if !found {
		t.Fatal("read_link missing from the capability definitions")
	}
	if runtime.toolHandlers["read_link"] == nil {
		t.Fatal("read_link must be dispatchable")
	}
}

func TestReadLinkToolReturnsTheCleanedArticleAndStorage(t *testing.T) {
	reader := &stubLinkReader{article: readlink.Article{
		Platform: "zhihu", SourceURL: "https://www.zhihu.com/question/1", CanonicalURL: "https://www.zhihu.com/question/1",
		Title: "Q", BodyText: "answer text", CapturedAt: "2026-11-02T12:00:00Z",
	}}
	response := stubLinkRuntime(reader).callReadLink(t, "https://www.zhihu.com/question/1?utm_source=x")
	if !statusOK(response) {
		t.Fatalf("response = %+v", response)
	}
	data, _ := response["data"].(map[string]any)
	storage, _ := data["storage"].(map[string]any)
	if data["title"] != "Q" || data["bodyText"] != "answer text" || data["platform"] != "zhihu" ||
		storage["kind"] != "memory" || storage["status"] != "saved" {
		t.Fatalf("data = %+v", data)
	}
	if len(reader.urls) != 1 {
		t.Fatalf("one read per call, got %d", len(reader.urls))
	}
}

func TestReadLinkToolReportsStopCodesWithoutReadingMore(t *testing.T) {
	reader := &stubLinkReader{err: readlinkError(t, "RISK_CONTROLLED")}
	response := stubLinkRuntime(reader).callReadLink(t, "https://www.zhihu.com/question/1")
	if statusOK(response) || errorCode(response) != "risk_controlled" {
		t.Fatalf("response = %+v", response)
	}

	rejected := &stubLinkReader{}
	response = stubLinkRuntime(rejected).callReadLink(t, "https://example.com/post")
	if errorCode(response) != "unsupported_url" || len(rejected.urls) != 0 {
		t.Fatalf("unsupported hosts must not reach the browser: %+v urls=%v", response, rejected.urls)
	}
}

func TestNewReadLinkServiceChoosesStoreFromConfig(t *testing.T) {
	for storage, want := range map[string]string{"": "karakeep", "karakeep": "karakeep", "memory": "memory", "dry_run": "dry_run"} {
		service := newReadLinkService(config.ReadLinkConfig{Storage: storage, KarakeepURL: "http://127.0.0.1:1"})
		if service.Store == nil || service.Reader == nil {
			t.Fatalf("%q: service incomplete", storage)
		}
		switch want {
		case "karakeep":
			if _, ok := service.Store.(*readlink.KarakeepStore); !ok {
				t.Fatalf("%q: %T", storage, service.Store)
			}
		case "memory":
			if _, ok := service.Store.(*readlink.MemoryStore); !ok {
				t.Fatalf("%q: %T", storage, service.Store)
			}
		case "dry_run":
			if _, ok := service.Store.(readlink.DryRunStore); !ok {
				t.Fatalf("%q: %T", storage, service.Store)
			}
		}
	}
	if service := newReadLinkService(config.ReadLinkConfig{Storage: "bogus"}); service.Store != nil {
		t.Fatal("an unknown storage mode must leave the tool unconfigured")
	}
	response := (&Runtime{readLinkSvc: newReadLinkService(config.ReadLinkConfig{Storage: "bogus"})}).callReadLink(t, "https://www.zhihu.com/question/1")
	if errorCode(response) != "service_not_configured" {
		t.Fatalf("response = %+v", response)
	}
}

func (r *Runtime) callReadLink(t *testing.T, url string) map[string]any {
	t.Helper()
	return callEnvelope(t, r.toolReadLink, context.Background(), map[string]any{"url": url})
}

func readlinkError(t *testing.T, code string) error {
	t.Helper()
	// Reader errors reach the tool as public read_link errors; build one through validation.
	switch code {
	case "RISK_CONTROLLED":
		return readlink.DetectStopState(200, "https://www.zhihu.com/q", "t", "请完成安全验证")
	}
	t.Fatalf("unknown code %s", code)
	return nil
}
