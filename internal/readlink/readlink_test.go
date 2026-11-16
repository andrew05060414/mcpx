package readlink

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeReader struct {
	calls   int
	article Article
	err     error
}

func (f *fakeReader) Read(_ context.Context, _ Target) (Article, error) {
	f.calls++
	return f.article, f.err
}

func goodArticle() Article {
	return Article{
		Platform: PlatformZhihu, SourceURL: "https://www.zhihu.com/question/1", CanonicalURL: "https://www.zhihu.com/question/1",
		Title: "T", BodyText: "body", CapturedAt: "2026-11-02T12:00:00Z",
	}
}

func TestServiceReadsAndStores(t *testing.T) {
	reader := &fakeReader{article: goodArticle()}
	store := NewMemoryStore()
	service := &Service{Reader: reader, Store: store, Resolve: publicResolver}
	result, err := service.Read(context.Background(), "https://www.zhihu.com/question/1?utm_source=a")
	if err != nil {
		t.Fatal(err)
	}
	if result.Title != "T" || result.BodyText != "body" || result.Storage.Kind != "memory" || result.Storage.Status != "saved" {
		t.Fatalf("result = %+v", result)
	}
	if result.SourceURL != "https://www.zhihu.com/question/1" {
		t.Fatalf("source URL must be sanitized: %s", result.SourceURL)
	}
}

func TestServiceDoesNotTouchTheBrowserForRejectedURLs(t *testing.T) {
	reader := &fakeReader{article: goodArticle()}
	service := &Service{Reader: reader, Store: NewMemoryStore(), Resolve: publicResolver}
	for raw, want := range map[string]string{
		"https://example.com/a":           "UNSUPPORTED_URL",
		"http://www.zhihu.com/question/1": "INVALID_URL",
		"https://192.168.1.1/a":           "SSRF_BLOCKED",
	} {
		if _, err := service.Read(context.Background(), raw); errCode(err) != want {
			t.Fatalf("%s: %v", raw, err)
		}
	}
	if reader.calls != 0 {
		t.Fatalf("browser was used %d times for rejected URLs", reader.calls)
	}
}

func TestServicePropagatesReaderStopCodesAndSkipsStorage(t *testing.T) {
	store := &countingStore{}
	service := &Service{Reader: &fakeReader{err: newError("RISK_CONTROLLED", "x", 424)}, Store: store, Resolve: publicResolver}
	if _, err := service.Read(context.Background(), "https://www.zhihu.com/question/1"); errCode(err) != "RISK_CONTROLLED" {
		t.Fatalf("err = %v", err)
	}
	if store.calls != 0 {
		t.Fatal("nothing may be stored after a stop code")
	}
}

func TestServiceRejectsReaderResultOutsideThePlatform(t *testing.T) {
	article := goodArticle()
	article.CanonicalURL = "https://evil.example/x"
	store := &countingStore{}
	service := &Service{Reader: &fakeReader{article: article}, Store: store, Resolve: publicResolver}
	if _, err := service.Read(context.Background(), "https://www.zhihu.com/question/1"); errCode(err) != "UNSUPPORTED_REDIRECT" {
		t.Fatalf("err = %v", err)
	}
	if store.calls != 0 {
		t.Fatal("nothing may be stored for an off-platform result")
	}
}

func TestServiceStorageFailureIsReported(t *testing.T) {
	service := &Service{Reader: &fakeReader{article: goodArticle()}, Store: &countingStore{err: newError("STORAGE_FAILED", "x", 502)}, Resolve: publicResolver}
	if _, err := service.Read(context.Background(), "https://www.zhihu.com/question/1"); errCode(err) != "STORAGE_FAILED" {
		t.Fatalf("err = %v", err)
	}
}

func TestServiceWithoutDependenciesIsNotConfigured(t *testing.T) {
	var nilService *Service
	if _, err := nilService.Read(context.Background(), "https://www.zhihu.com/question/1"); errCode(err) != "SERVICE_NOT_CONFIGURED" {
		t.Fatalf("nil service = %v", err)
	}
	if _, err := (&Service{Reader: &fakeReader{}}).Read(context.Background(), "https://www.zhihu.com/question/1"); errCode(err) != "SERVICE_NOT_CONFIGURED" {
		t.Fatalf("no store = %v", err)
	}
}

func TestAsErrorHidesUnknownErrors(t *testing.T) {
	got := AsError(errors.New("secret internals: token=abc"))
	if got.Code != "INTERNAL_ERROR" || got.HTTPStatus != 500 || got.Message == "" || strings.Contains(got.Message, "secret") || strings.Contains(got.Message, "token") {
		t.Fatalf("got = %+v", got)
	}
}

type countingStore struct {
	calls int
	err   error
}

func (c *countingStore) Save(context.Context, Article) (Storage, error) {
	c.calls++
	if c.err != nil {
		return Storage{}, c.err
	}
	return Storage{Kind: "memory", Status: "saved"}, nil
}
