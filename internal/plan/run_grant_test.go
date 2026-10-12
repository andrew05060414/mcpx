package plan

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func runGrantFixture(t *testing.T) (*Service, string, Plan, func(time.Duration)) {
	t.Helper()
	store := openPlanStore(t)
	t.Cleanup(func() { store.Close() })
	remoteSessionID := seedPlanSession(t, store.DB())
	service := NewService(store.DB())
	clock := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return clock }
	created, err := service.Create(context.Background(), remoteSessionID, "principal-test", CreateInput{
		Goal: "run grant", Tasks: []TaskInput{{ID: "a", Title: "A"}, {ID: "b", Title: "B"}, {ID: "c", Title: "C"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return service, remoteSessionID, created, func(d time.Duration) { clock = clock.Add(d) }
}

func defaultGrantInput() RunGrantInput {
	return RunGrantInput{
		Repositories: []string{"https://github.com/Andrew/MCPX.git"}, AutoMergeMaxRisk: RiskStandard,
		MaxTokens: 1000, MaxRuntimeMinutes: 60, MaxAttempts: 2, ExpiresInMinutes: 24 * 60,
	}
}

func TestRunGrantAuthorizesWithinScopeAndCountsAttemptsOncePerKey(t *testing.T) {
	ctx := context.Background()
	service, sessionID, item, _ := runGrantFixture(t)
	grant, err := service.CreateRunGrant(ctx, sessionID, item.ID, "principal-test", defaultGrantInput())
	if err != nil {
		t.Fatal(err)
	}
	if grant.Effective != RunGrantEffectiveActive || grant.Repositories[0] != "andrew/mcpx" {
		t.Fatalf("grant = %+v", grant)
	}
	use := RunGrantUse{PlanTaskID: item.Tasks[0].ID, IdempotencyKey: "k1", Repository: "andrew/mcpx", RiskLevel: RiskStandard, PrincipalID: "agent"}
	first, err := service.AuthorizeRunGrantUse(ctx, sessionID, item.ID, use)
	if err != nil || !first.Authorized || !first.AttemptConsumed || first.Replay || !first.AutoMergeAllowed {
		t.Fatalf("first = %+v err=%v", first, err)
	}
	replay, err := service.AuthorizeRunGrantUse(ctx, sessionID, item.ID, use)
	if err != nil || !replay.Authorized || replay.AttemptConsumed || !replay.Replay || replay.Grant.AttemptsUsed != 1 {
		t.Fatalf("replay = %+v err=%v", replay, err)
	}
	review := use
	review.PlanTaskID, review.IdempotencyKey, review.RiskLevel = item.Tasks[1].ID, "k2", RiskReview
	second, err := service.AuthorizeRunGrantUse(ctx, sessionID, item.ID, review)
	if err != nil || !second.Authorized || second.AutoMergeAllowed || second.Grant.AttemptsUsed != 2 {
		t.Fatalf("second = %+v err=%v", second, err)
	}
	third := use
	third.PlanTaskID, third.IdempotencyKey = item.Tasks[2].ID, "k3"
	exhausted, err := service.AuthorizeRunGrantUse(ctx, sessionID, item.ID, third)
	if err != nil || exhausted.Authorized || exhausted.Code != RunGrantEffectiveAttemptsExhausted {
		t.Fatalf("exhausted = %+v err=%v", exhausted, err)
	}
	// A same-key retry still replays after the budget is spent.
	if again, err := service.AuthorizeRunGrantUse(ctx, sessionID, item.ID, use); err != nil || !again.Authorized || !again.Replay {
		t.Fatalf("replay after exhaustion = %+v err=%v", again, err)
	}
	loaded, err := service.Get(ctx, sessionID, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, event := range loaded.Events {
		counts[event.Type]++
	}
	if counts["plan.run_grant.created"] != 1 || counts["plan.run_grant.used"] != 4 || counts["plan.run_grant.fallback"] != 1 {
		t.Fatalf("audit events = %+v", counts)
	}
}

func TestRunGrantFallsBackOutsideScope(t *testing.T) {
	ctx := context.Background()
	service, sessionID, item, _ := runGrantFixture(t)
	base := RunGrantUse{PlanTaskID: item.Tasks[0].ID, IdempotencyKey: "k", Repository: "andrew/mcpx", RiskLevel: RiskRoutine, PrincipalID: "agent"}
	none, err := service.AuthorizeRunGrantUse(ctx, sessionID, item.ID, base)
	if err != nil || none.Authorized || none.Code != "no_active_grant" {
		t.Fatalf("without grant = %+v err=%v", none, err)
	}
	if _, err := service.CreateRunGrant(ctx, sessionID, item.ID, "principal-test", defaultGrantInput()); err != nil {
		t.Fatal(err)
	}
	cases := map[string]RunGrantUse{
		"repository_out_of_scope": {Repository: "andrew/pxread", RiskLevel: RiskRoutine},
		"repository_required":     {Repository: "", RiskLevel: RiskRoutine},
		"risk_level_required":     {Repository: "andrew/mcpx", RiskLevel: ""},
	}
	for code, override := range cases {
		use := base
		use.Repository, use.RiskLevel = override.Repository, override.RiskLevel
		decision, err := service.AuthorizeRunGrantUse(ctx, sessionID, item.ID, use)
		if err != nil || decision.Authorized || decision.Code != code {
			t.Fatalf("%s: decision = %+v err=%v", code, decision, err)
		}
	}
	signoff := base
	signoff.RiskLevel = RiskSignoff
	decision, err := service.AuthorizeRunGrantUse(ctx, sessionID, item.ID, signoff)
	if err != nil || !decision.Authorized || decision.AutoMergeAllowed {
		t.Fatalf("signoff dispatch must be allowed but never auto-merge: %+v err=%v", decision, err)
	}
}

func TestRunGrantExpiryRuntimeTokensAndRevocation(t *testing.T) {
	ctx := context.Background()
	service, sessionID, item, advance := runGrantFixture(t)
	input := defaultGrantInput()
	input.MaxAttempts = 10
	if _, err := service.CreateRunGrant(ctx, sessionID, item.ID, "principal-test", input); err != nil {
		t.Fatal(err)
	}
	use := func(task int, key string) RunGrantDecision {
		t.Helper()
		decision, err := service.AuthorizeRunGrantUse(ctx, sessionID, item.ID, RunGrantUse{
			PlanTaskID: item.Tasks[task].ID, IdempotencyKey: key, Repository: "andrew/mcpx", RiskLevel: RiskRoutine, PrincipalID: "agent",
		})
		if err != nil {
			t.Fatal(err)
		}
		return decision
	}
	if !use(0, "a").Authorized {
		t.Fatal("first use should be authorized")
	}
	advance(61 * time.Minute)
	if decision := use(1, "b"); decision.Authorized || decision.Code != RunGrantEffectiveRuntimeExhausted {
		t.Fatalf("runtime budget: %+v", decision)
	}

	// A new grant supersedes the old one and starts its own runtime window.
	if _, err := service.CreateRunGrant(ctx, sessionID, item.ID, "principal-test", input); err != nil {
		t.Fatal(err)
	}
	grants, err := service.ListRunGrants(ctx, sessionID, item.ID)
	if err != nil || len(grants) != 2 || grants[1].Effective != RunGrantEffectiveSuperseded {
		t.Fatalf("grants = %+v err=%v", grants, err)
	}
	if _, err := service.ReportRunGrantUsage(ctx, sessionID, item.ID, "scheduler", 1000); err != nil {
		t.Fatal(err)
	}
	if lowered, err := service.ReportRunGrantUsage(ctx, sessionID, item.ID, "scheduler", 10); err != nil || lowered.TokensUsed != 1000 {
		t.Fatalf("token usage must be monotonic: %+v err=%v", lowered, err)
	}
	if decision := use(1, "c"); decision.Authorized || decision.Code != RunGrantEffectiveTokensExhausted {
		t.Fatalf("token budget: %+v", decision)
	}

	if _, err := service.CreateRunGrant(ctx, sessionID, item.ID, "principal-test", input); err != nil {
		t.Fatal(err)
	}
	revoked, err := service.RevokeRunGrant(ctx, sessionID, item.ID, "principal-test", "stop")
	if err != nil || revoked.Effective != RunGrantEffectiveRevoked {
		t.Fatalf("revoke = %+v err=%v", revoked, err)
	}
	if decision := use(1, "d"); decision.Authorized || decision.Code != "no_active_grant" {
		t.Fatalf("after revoke: %+v", decision)
	}
	if _, err := service.RevokeRunGrant(ctx, sessionID, item.ID, "principal-test", "again"); !errors.Is(err, ErrRunGrantNotFound) {
		t.Fatalf("second revoke err=%v", err)
	}

	short := input
	short.ExpiresInMinutes = 5
	if _, err := service.CreateRunGrant(ctx, sessionID, item.ID, "principal-test", short); err != nil {
		t.Fatal(err)
	}
	advance(6 * time.Minute)
	if decision := use(1, "e"); decision.Authorized || decision.Code != RunGrantEffectiveExpired {
		t.Fatalf("expiry: %+v", decision)
	}
}

func TestRunGrantRejectsInvalidScope(t *testing.T) {
	ctx := context.Background()
	service, sessionID, item, _ := runGrantFixture(t)
	mutate := map[string]func(*RunGrantInput){
		"signoff":   func(in *RunGrantInput) { in.AutoMergeMaxRisk = RiskSignoff },
		"wildcard":  func(in *RunGrantInput) { in.Repositories = []string{"andrew/*"} },
		"no_repo":   func(in *RunGrantInput) { in.Repositories = nil },
		"tokens":    func(in *RunGrantInput) { in.MaxTokens = 0 },
		"attempts":  func(in *RunGrantInput) { in.MaxAttempts = 0 },
		"expiry":    func(in *RunGrantInput) { in.ExpiresInMinutes = 0 },
		"runtime":   func(in *RunGrantInput) { in.MaxRuntimeMinutes = 0 },
		"bad_level": func(in *RunGrantInput) { in.AutoMergeMaxRisk = "yolo" },
	}
	for name, change := range mutate {
		input := defaultGrantInput()
		change(&input)
		if _, err := service.CreateRunGrant(ctx, sessionID, item.ID, "principal-test", input); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("%s: err=%v", name, err)
		}
	}
	if _, err := service.CreateRunGrant(ctx, "other-session", item.ID, "principal-test", defaultGrantInput()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign session err=%v", err)
	}
	if !strings.Contains(NormalizeRepository("git@github.com:Andrew/pxread.git"), "andrew/pxread") {
		t.Fatal("ssh remote should normalize")
	}
}

