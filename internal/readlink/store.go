package readlink

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	noteMaxChars    = 50_000
	summaryMaxChars = 1000
	storeTimeout    = 15 * time.Second
	maxStoreBody    = 1 << 20
)

// Storage describes where a capture ended up.
type Storage struct {
	Kind       string `json:"kind"`
	BookmarkID string `json:"bookmarkId,omitempty"`
	Status     string `json:"status"`
}

// Store saves a capture.
type Store interface {
	Save(ctx context.Context, article Article) (Storage, error)
}

// DryRunStore records nothing; used for smoke tests that must not write to Karakeep.
type DryRunStore struct{}

func (DryRunStore) Save(context.Context, Article) (Storage, error) {
	return Storage{Kind: "dry_run", Status: "skipped"}, nil
}

// MemoryStore keeps captures for the life of the process.
type MemoryStore struct {
	mu    sync.Mutex
	items map[string]memoryItem
}

type memoryItem struct {
	id      string
	article Article
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{items: map[string]memoryItem{}} }

func (m *MemoryStore) Save(_ context.Context, article Article) (Storage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := m.items[article.CanonicalURL].id
	if id == "" {
		id = fmt.Sprintf("memory-%d", len(m.items)+1)
	}
	m.items[article.CanonicalURL] = memoryItem{id: id, article: article}
	return Storage{Kind: "memory", BookmarkID: id, Status: "saved"}, nil
}

// KarakeepStore writes a link bookmark carrying the extracted text.
type KarakeepStore struct {
	BaseURL    string
	APIKeyFile string
	HTTPClient *http.Client
	readAPIKey func() (string, error)
}

func (k *KarakeepStore) apiKey() (string, error) {
	if k.readAPIKey != nil {
		return k.readAPIKey()
	}
	if strings.TrimSpace(k.APIKeyFile) == "" {
		return "", errNotConfigured
	}
	raw, err := os.ReadFile(k.APIKeyFile)
	if err != nil {
		return "", errNotConfigured
	}
	key := strings.TrimSpace(string(raw))
	if key == "" {
		return "", errNotConfigured
	}
	return key, nil
}

var errNotConfigured = newError("STORAGE_NOT_CONFIGURED", "set read_link.karakeep_url and read_link.karakeep_api_key_file before saving captures.", 503)

func (k *KarakeepStore) endpoint(path string) string {
	return strings.TrimRight(k.BaseURL, "/") + "/" + strings.TrimLeft(path, "/")
}

func (k *KarakeepStore) requestJSON(ctx context.Context, method, target, key string, body any) (map[string]any, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, newError("STORAGE_FAILED", "Karakeep could not be reached.", 502)
	}
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(raw))
	if err != nil {
		return nil, newError("STORAGE_FAILED", "Karakeep could not be reached.", 502)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	client := k.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, newError("STORAGE_FAILED", "Karakeep could not be reached.", 502)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxStoreBody))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, newError("STORAGE_FAILED", "Karakeep rejected the capture.", 502).withDetail("upstreamStatus", resp.StatusCode)
	}
	var payload map[string]any
	_ = json.Unmarshal(data, &payload)
	return payload, nil
}

func (k *KarakeepStore) Save(ctx context.Context, article Article) (Storage, error) {
	if strings.TrimSpace(k.BaseURL) == "" {
		return Storage{}, errNotConfigured
	}
	if _, err := url.Parse(k.BaseURL); err != nil {
		return Storage{}, errNotConfigured
	}
	key, err := k.apiKey()
	if err != nil {
		return Storage{}, err
	}
	note := truncateRunes(article.BodyText, noteMaxChars)
	summary := article.Description
	if summary == "" {
		summary = truncateRunes(article.BodyText, summaryMaxChars)
	}
	created, err := k.requestJSON(ctx, http.MethodPost, k.endpoint("bookmarks"), key, map[string]any{
		"type":          "link",
		"url":           article.CanonicalURL,
		"title":         article.Title,
		"note":          note,
		"summary":       summary,
		"source":        "api",
		"crawlPriority": "low",
	})
	if err != nil {
		return Storage{}, err
	}
	bookmarkID, _ := created["id"].(string)
	if bookmarkID == "" {
		return Storage{}, newError("STORAGE_FAILED", "Karakeep did not return a bookmark id.", 502)
	}
	// Karakeep rejects the text field on a link bookmark, so the extracted article
	// stays in the note field next to the URL.
	patch := map[string]any{"title": article.Title, "note": note}
	if article.Description != "" {
		patch["description"] = article.Description
	}
	if article.Author != "" {
		patch["author"] = article.Author
	}
	if _, err := k.requestJSON(ctx, http.MethodPatch, k.endpoint("bookmarks/"+url.PathEscape(bookmarkID)), key, patch); err != nil {
		typed := AsError(err)
		return Storage{}, typed.withDetail("bookmarkId", bookmarkID)
	}
	return Storage{Kind: "karakeep", BookmarkID: bookmarkID, Status: "saved"}, nil
}
