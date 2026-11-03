package readlink

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type recordedRequest struct {
	Method string
	Path   string
	Auth   string
	Body   map[string]any
}

func karakeepFake(t *testing.T, patchStatus int) (*httptest.Server, *[]recordedRequest) {
	t.Helper()
	var mu sync.Mutex
	var seen []recordedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		seen = append(seen, recordedRequest{r.Method, r.URL.Path, r.Header.Get("Authorization"), body})
		mu.Unlock()
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"bm 1"}`))
		case http.MethodPatch:
			w.WriteHeader(patchStatus)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(server.Close)
	return server, &seen
}

func writeKeyFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "karakeep.key")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func sampleArticle() Article {
	return Article{
		Platform: PlatformZhihu, SourceURL: "https://www.zhihu.com/q/1", CanonicalURL: "https://www.zhihu.com/q/1",
		Title: "T", Description: "", Author: "au", BodyText: strings.Repeat("字", 60_000), CapturedAt: "2026-11-02T12:00:00Z",
	}
}

func TestKarakeepStoreCreatesThenPatchesBookmark(t *testing.T) {
	server, seen := karakeepFake(t, http.StatusOK)
	store := &KarakeepStore{BaseURL: server.URL + "/api/v1/", APIKeyFile: writeKeyFile(t, "  secret-key\n")}
	storage, err := store.Save(context.Background(), sampleArticle())
	if err != nil {
		t.Fatal(err)
	}
	if storage != (Storage{Kind: "karakeep", BookmarkID: "bm 1", Status: "saved"}) {
		t.Fatalf("storage = %+v", storage)
	}
	if len(*seen) != 2 {
		t.Fatalf("requests = %+v", *seen)
	}
	post, patch := (*seen)[0], (*seen)[1]
	if post.Method != "POST" || post.Path != "/api/v1/bookmarks" || post.Auth != "Bearer secret-key" {
		t.Fatalf("post = %+v", post)
	}
	if post.Body["type"] != "link" || post.Body["url"] != "https://www.zhihu.com/q/1" || post.Body["source"] != "api" || post.Body["crawlPriority"] != "low" {
		t.Fatalf("post body = %+v", post.Body)
	}
	if got := runeLen(post.Body["note"].(string)); got != noteMaxChars {
		t.Fatalf("note length %d, want %d", got, noteMaxChars)
	}
	if got := runeLen(post.Body["summary"].(string)); got != summaryMaxChars {
		t.Fatalf("summary falls back to the first %d characters, got %d", summaryMaxChars, got)
	}
	if patch.Method != "PATCH" || patch.Path != "/api/v1/bookmarks/bm 1" || patch.Auth != "Bearer secret-key" {
		t.Fatalf("patch = %+v", patch)
	}
	if _, hasText := patch.Body["text"]; hasText || patch.Body["author"] != "au" || patch.Body["title"] != "T" {
		t.Fatalf("patch body = %+v (must keep text out, author in)", patch.Body)
	}
	if _, hasDescription := patch.Body["description"]; hasDescription {
		t.Fatal("an empty description must be omitted")
	}
}

func TestKarakeepStoreReportsFailuresWithoutLeakingTheKey(t *testing.T) {
	server, _ := karakeepFake(t, http.StatusInternalServerError)
	keyFile := writeKeyFile(t, "super-secret-key")
	store := &KarakeepStore{BaseURL: server.URL, APIKeyFile: keyFile}
	_, err := store.Save(context.Background(), sampleArticle())
	typed := AsError(err)
	if typed.Code != "STORAGE_FAILED" || typed.Details["upstreamStatus"] != 500 || typed.Details["bookmarkId"] != "bm 1" {
		t.Fatalf("patch failure must keep the created bookmark id: %+v", typed)
	}
	if strings.Contains(typed.Message, "super-secret-key") || strings.Contains(typed.Message, keyFile) {
		t.Fatalf("message leaks credentials: %s", typed.Message)
	}

	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	unreachable := &KarakeepStore{BaseURL: closed.URL, APIKeyFile: keyFile}
	if _, err := unreachable.Save(context.Background(), sampleArticle()); errCode(err) != "STORAGE_FAILED" {
		t.Fatalf("unreachable = %v", err)
	}
}

func TestKarakeepStoreRequiresConfiguration(t *testing.T) {
	article := sampleArticle()
	cases := map[string]*KarakeepStore{
		"no url":       {APIKeyFile: writeKeyFile(t, "k")},
		"no key file":  {BaseURL: "http://127.0.0.1:1"},
		"missing file": {BaseURL: "http://127.0.0.1:1", APIKeyFile: filepath.Join(t.TempDir(), "absent")},
		"empty key":    {BaseURL: "http://127.0.0.1:1", APIKeyFile: writeKeyFile(t, " \n")},
	}
	for name, store := range cases {
		if _, err := store.Save(context.Background(), article); errCode(err) != "STORAGE_NOT_CONFIGURED" {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestKarakeepStoreMissingBookmarkID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	defer server.Close()
	store := &KarakeepStore{BaseURL: server.URL, APIKeyFile: writeKeyFile(t, "k")}
	if _, err := store.Save(context.Background(), sampleArticle()); errCode(err) != "STORAGE_FAILED" {
		t.Fatalf("err = %v", err)
	}
}

func TestMemoryAndDryRunStores(t *testing.T) {
	memory := NewMemoryStore()
	first, _ := memory.Save(context.Background(), sampleArticle())
	again, _ := memory.Save(context.Background(), sampleArticle())
	other := sampleArticle()
	other.CanonicalURL = "https://www.zhihu.com/q/2"
	second, _ := memory.Save(context.Background(), other)
	if first.BookmarkID == "" || first.BookmarkID != again.BookmarkID || second.BookmarkID == first.BookmarkID {
		t.Fatalf("ids: %+v %+v %+v", first, again, second)
	}
	dry, err := DryRunStore{}.Save(context.Background(), sampleArticle())
	if err != nil || dry.Kind != "dry_run" || dry.Status != "skipped" || dry.BookmarkID != "" {
		t.Fatalf("dry run = %+v err=%v", dry, err)
	}
}
