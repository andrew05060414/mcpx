// Package readlink reads one page from an approved platform (Xiaohongshu,
// Bilibili, Zhihu, X) through the user's logged-in Ego Lite browser and stores
// the cleaned text in Karakeep. It stops, and never works around, login,
// risk-control, and rate-limit pages.
package readlink

import (
	"context"
	"time"
)

// Reader fetches a cleaned article for a validated target.
type Reader interface {
	Read(ctx context.Context, target Target) (Article, error)
}

// Service validates a URL, reads it, and stores the capture.
type Service struct {
	Reader  Reader
	Store   Store
	Resolve Resolver
}

// Result is a successful read_link outcome.
type Result struct {
	Article
	Storage Storage `json:"storage"`
}

// Read validates rawURL, reads the page, and saves the capture.
func (s *Service) Read(ctx context.Context, rawURL string) (Result, error) {
	if s == nil || s.Reader == nil || s.Store == nil {
		return Result{}, newError("SERVICE_NOT_CONFIGURED", "read_link is not configured.", 503)
	}
	target, err := ValidateInputURL(rawURL)
	if err != nil {
		return Result{}, err
	}
	if err := AssertPublicHost(ctx, target.Host, s.Resolve); err != nil {
		return Result{}, err
	}
	article, err := s.Reader.Read(ctx, target)
	if err != nil {
		return Result{}, err
	}
	// Re-check what the reader reports: platform, final host, and cleaned text.
	final, err := ValidateFinalURL(orDefault(article.CanonicalURL, target.SourceURL), target.Platform)
	if err != nil {
		return Result{}, err
	}
	captured, parseErr := time.Parse(time.RFC3339, article.CapturedAt)
	if parseErr != nil {
		captured = time.Now()
	}
	article, err = NormalizeArticle(target.Platform, target.SourceURL, final.SourceURL, Snapshot{
		Title: article.Title, Description: article.Description, Author: article.Author, Body: article.BodyText,
	}, captured)
	if err != nil {
		return Result{}, err
	}
	storage, err := s.Store.Save(ctx, article)
	if err != nil {
		return Result{}, err
	}
	return Result{Article: article, Storage: storage}, nil
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
