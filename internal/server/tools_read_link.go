package server

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcpx/internal/config"
	"mcpx/internal/envelope"
	"mcpx/internal/mcpresult"
	"mcpx/internal/readlink"
)

// readLinkToolAnnotation: the tool writes one bookmark to Karakeep, never
// deletes anything, and opens a page on the open web through a logged-in browser.
var readLinkToolAnnotation = toolAnnotation{
	ReadOnly: false, Destructive: false, Idempotent: false, OpenWorld: true,
	Title: "Read a link with Ego Lite and save it to Karakeep",
}

func (r *Runtime) registerReadLinkTool(s *mcp.Server) {
	r.addToolIfAbsent(s, cleanCoreTool("read_link",
		"Read one Xiaohongshu, Bilibili, Zhihu, or X URL in the user's logged-in Ego Lite browser and save the text to Karakeep. One URL per call. On AUTH_REQUIRED, RISK_CONTROLLED, or RATE_LIMITED, stop and tell the user; do not retry.",
		map[string]any{
			"url": stringSchema("One HTTPS URL on an approved platform."),
		},
		[]string{"url"},
		readLinkToolAnnotation,
	), r.toolReadLink)
}

func (r *Runtime) toolReadLink(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	envReq, parseErr := r.parseEnv(ctx, req)
	if parseErr != nil {
		return mcpresult.NewError(parseErr.Error()), nil
	}
	args := mcpresult.Arguments(req)
	result, err := r.readLinkService().Read(ctx, stringArg(args, "url"))
	if err != nil {
		public := readlink.AsError(err)
		var details any
		if len(public.Details) > 0 {
			details = public.Details
		}
		return r.resultJSON(envelope.Fail(envelope.StatusError, envReq.RequestID, "", details, public.Code, public.Message))
	}
	return compactToolResult(result, fmt.Sprintf("Read %s link and saved it (%s).", result.Platform, result.Storage.Kind)), nil
}

// readLinkService builds the service on first use from the process-wide config.
func (r *Runtime) readLinkService() *readlink.Service {
	r.readLinkMu.Lock()
	defer r.readLinkMu.Unlock()
	if r.readLinkSvc == nil {
		r.readLinkSvc = newReadLinkService(r.cfg.ReadLink)
	}
	return r.readLinkSvc
}

func newReadLinkService(cfg config.ReadLinkConfig) *readlink.Service {
	service := &readlink.Service{
		Reader: readlink.NewEgoReader(readlink.EgoConfig{
			Bin:     cfg.EgoBin,
			Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second,
			Settle:  time.Duration(cfg.SettleSeconds) * time.Second,
		}),
	}
	switch strings.ToLower(strings.TrimSpace(cfg.Storage)) {
	case "", "karakeep":
		service.Store = &readlink.KarakeepStore{BaseURL: cfg.KarakeepURL, APIKeyFile: cfg.KarakeepAPIKeyFile}
	case "memory":
		service.Store = readlink.NewMemoryStore()
	case "dry_run":
		service.Store = readlink.DryRunStore{}
	}
	return service
}
