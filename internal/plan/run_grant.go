package plan

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Risk levels mirror the Agent Factory lanes: 例行 / 标准 / 需过目 / 需签字.
const (
	RiskNone     = "none"
	RiskRoutine  = "routine"
	RiskStandard = "standard"
	RiskReview   = "review"
	RiskSignoff  = "signoff"
)

const (
	RunGrantActive     = "active"
	RunGrantRevoked    = "revoked"
	RunGrantSuperseded = "superseded"
)

// Effective states are computed at read time; only active/revoked/superseded
// are stored because expiry and budgets change with time and usage.
const (
	RunGrantEffectiveActive            = "active"
	RunGrantEffectiveRevoked           = "revoked"
	RunGrantEffectiveSuperseded        = "superseded"
	RunGrantEffectiveExpired           = "expired"
	RunGrantEffectiveRuntimeExhausted  = "runtime_exhausted"
	RunGrantEffectiveTokensExhausted   = "tokens_exhausted"
	RunGrantEffectiveAttemptsExhausted = "attempts_exhausted"
	RunGrantEffectivePlanClosed        = "plan_closed"
)

const (
	maxRunGrantAttempts       = 1000
	maxRunGrantLifetime       = 30 * 24 * time.Hour
	maxRunGrantRepositories   = 20
	maxRunGrantNoteCharacters = 500
)

var ErrRunGrantNotFound = errors.New("plan has no active run grant")

func riskRank(level string) int {
	switch level {
	case RiskRoutine:
		return 1
	case RiskStandard:
		return 2
	case RiskReview:
		return 3
	case RiskSignoff:
		return 4
	default:
		return 0
	}
}

// TaskRiskLevels are the accepted per-task risk declarations.
func TaskRiskLevels() []string { return []string{RiskRoutine, RiskStandard, RiskReview, RiskSignoff} }

// AutoMergeRiskCeilings excludes signoff: 需签字 always needs a human.
func AutoMergeRiskCeilings() []string {
	return []string{RiskNone, RiskRoutine, RiskStandard, RiskReview}
}

// AutoMergeAllowed reports whether a task at risk may merge without a human
// under ceiling. Signoff is never auto-mergeable.
func AutoMergeAllowed(risk, ceiling string) bool {
	if risk == RiskSignoff || riskRank(risk) == 0 || riskRank(ceiling) == 0 {
		return false
	}
	return riskRank(risk) <= riskRank(ceiling)
}

// NormalizeRepository accepts owner/name, a GitHub URL, or an SSH remote.
func NormalizeRepository(raw string) string {
	value := strings.ToLower(strings.TrimSpace(raw))
	for _, prefix := range []string{"https://github.com/", "http://github.com/", "git@github.com:", "github.com/"} {
		value = strings.TrimPrefix(value, prefix)
	}
	value = strings.TrimSuffix(strings.TrimSuffix(value, "/"), ".git")
	parts := strings.Split(value, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(value, " *?\\") {
		return ""
	}
	return value
}

type RunGrantInput struct {
	Repositories      []string
	AutoMergeMaxRisk  string
	MaxTokens         int64
	MaxRuntimeMinutes int
	MaxAttempts       int
	ExpiresInMinutes  int
	Note              string
}

type RunGrant struct {
	ID               string     `json:"grant_id"`
	PlanID           string     `json:"plan_id"`
	RemoteSessionID  string     `json:"remote_session_id"`
	Status           string     `json:"status"`
	Effective        string     `json:"effective_status"`
	Repositories     []string   `json:"repositories"`
	AutoMergeMaxRisk string     `json:"auto_merge_max_risk"`
	MaxTokens        int64      `json:"max_tokens"`
	TokensUsed       int64      `json:"tokens_used"`
	MaxRuntimeMs     int64      `json:"max_runtime_ms"`
	MaxAttempts      int        `json:"max_attempts"`
	AttemptsUsed     int        `json:"attempts_used"`
	ExpiresAt        time.Time  `json:"expires_at"`
	FirstUsedAt      *time.Time `json:"first_used_at,omitempty"`
	RuntimeDeadline  *time.Time `json:"runtime_deadline,omitempty"`
	Note             string     `json:"note,omitempty"`
	CreatedBy        string     `json:"created_by"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	EndedBy          string     `json:"ended_by,omitempty"`
	EndedAt          *time.Time `json:"ended_at,omitempty"`
	EndReason        string     `json:"end_reason,omitempty"`
}

// RunGrantUse describes one attempt to skip per-call confirmation.
type RunGrantUse struct {
	PlanTaskID     string
	IdempotencyKey string
	Repository     string
	RiskLevel      string
	PrincipalID    string
}

// RunGrantDecision is the auditable answer to "does the grant cover this call?".
// A denied decision is not an error: the caller falls back to per-call
// confirmation and reports Code/Reason to the user.
type RunGrantDecision struct {
	Authorized       bool      `json:"authorized"`
	Code             string    `json:"code,omitempty"`
	Reason           string    `json:"reason,omitempty"`
	Grant            *RunGrant `json:"grant,omitempty"`
	Repository       string    `json:"repository,omitempty"`
	RiskLevel        string    `json:"risk_level,omitempty"`
	AutoMergeAllowed bool      `json:"auto_merge_allowed"`
	AttemptConsumed  bool      `json:"attempt_consumed"`
	Replay           bool      `json:"replay"`
}

func (s *Service) CreateRunGrant(ctx context.Context, remoteSessionID, planID, principalID string, input RunGrantInput) (RunGrant, error) {
	if remoteSessionID == "" || planID == "" || principalID == "" {
		return RunGrant{}, fmt.Errorf("%w: session, plan, and principal are required", ErrInvalidInput)
	}
	repositories, err := normalizeRepositories(input.Repositories)
	if err != nil {
		return RunGrant{}, err
	}
	ceiling := strings.TrimSpace(input.AutoMergeMaxRisk)
	if ceiling == RiskSignoff {
		return RunGrant{}, fmt.Errorf("%w: auto_merge_max_risk cannot be signoff; 需签字 merges always need a human", ErrInvalidInput)
	}
	if ceiling != RiskNone && riskRank(ceiling) == 0 {
		return RunGrant{}, fmt.Errorf("%w: auto_merge_max_risk must be one of %s", ErrInvalidInput, strings.Join(AutoMergeRiskCeilings(), ", "))
	}
	if input.MaxTokens <= 0 {
		return RunGrant{}, fmt.Errorf("%w: max_tokens must be positive", ErrInvalidInput)
	}
	if input.MaxAttempts <= 0 || input.MaxAttempts > maxRunGrantAttempts {
		return RunGrant{}, fmt.Errorf("%w: max_attempts must be between 1 and %d", ErrInvalidInput, maxRunGrantAttempts)
	}
	lifetime := time.Duration(input.ExpiresInMinutes) * time.Minute
	if lifetime <= 0 || lifetime > maxRunGrantLifetime {
		return RunGrant{}, fmt.Errorf("%w: expires_in_minutes must be between 1 and %d", ErrInvalidInput, int(maxRunGrantLifetime/time.Minute))
	}
	runtime := time.Duration(input.MaxRuntimeMinutes) * time.Minute
	if runtime <= 0 || runtime > maxRunGrantLifetime {
		return RunGrant{}, fmt.Errorf("%w: max_runtime_minutes must be between 1 and %d", ErrInvalidInput, int(maxRunGrantLifetime/time.Minute))
	}
	note := strings.TrimSpace(input.Note)
	if len([]rune(note)) > maxRunGrantNoteCharacters {
		return RunGrant{}, fmt.Errorf("%w: note exceeds %d characters", ErrInvalidInput, maxRunGrantNoteCharacters)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RunGrant{}, err
	}
	defer tx.Rollback()
	item, err := loadPlanHeader(ctx, tx, remoteSessionID, planID)
	if err != nil {
		return RunGrant{}, err
	}
	if item.Status == PlanCompleted || item.Status == PlanCancelled {
		return RunGrant{}, fmt.Errorf("%w: plan is %s", ErrInvalidState, item.Status)
	}
	now := s.now().UTC()
	previous, err := loadActiveRunGrant(ctx, tx, remoteSessionID, planID)
	if err != nil && !errors.Is(err, ErrRunGrantNotFound) {
		return RunGrant{}, err
	}
	grantID := newID("rg_")
	if err == nil {
		if _, err := tx.ExecContext(ctx, `UPDATE plan_run_grants SET status = 'superseded', ended_by = ?, ended_at = ?, end_reason = ?, updated_at = ?
			WHERE id = ? AND status = 'active'`, principalID, now.UnixMilli(), "superseded by "+grantID, now.UnixMilli(), previous.ID); err != nil {
			return RunGrant{}, err
		}
		if err := insertEventTx(ctx, tx, planID, "", "plan.run_grant.superseded", "superseded by "+grantID, principalID,
			map[string]any{"grant_id": previous.ID, "superseded_by": grantID}, now); err != nil {
			return RunGrant{}, err
		}
	}
	encodedRepos, _ := json.Marshal(repositories)
	expiresAt := now.Add(lifetime)
	if _, err := tx.ExecContext(ctx, `INSERT INTO plan_run_grants
		(id, plan_id, remote_session_id, status, repositories_json, auto_merge_max_risk, max_tokens, max_runtime_ms, max_attempts,
		 expires_at, note, created_by, created_at, updated_at)
		VALUES (?, ?, ?, 'active', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		grantID, planID, remoteSessionID, string(encodedRepos), ceiling, input.MaxTokens, runtime.Milliseconds(), input.MaxAttempts,
		expiresAt.UnixMilli(), note, principalID, now.UnixMilli(), now.UnixMilli()); err != nil {
		return RunGrant{}, err
	}
	if err := insertEventTx(ctx, tx, planID, "", "plan.run_grant.created", note, principalID, map[string]any{
		"grant_id": grantID, "repositories": repositories, "auto_merge_max_risk": ceiling,
		"max_tokens": input.MaxTokens, "max_runtime_ms": runtime.Milliseconds(), "max_attempts": input.MaxAttempts,
		"expires_at": expiresAt.Format(time.RFC3339),
	}, now); err != nil {
		return RunGrant{}, err
	}
	if err := tx.Commit(); err != nil {
		return RunGrant{}, err
	}
	return s.RunGrant(ctx, remoteSessionID, planID, grantID)
}

// RunGrant loads one grant by ID, or the newest grant of the plan when grantID is empty.
func (s *Service) RunGrant(ctx context.Context, remoteSessionID, planID, grantID string) (RunGrant, error) {
	item, err := loadPlanHeader(ctx, s.db, remoteSessionID, planID)
	if err != nil {
		return RunGrant{}, err
	}
	query := `SELECT ` + runGrantColumns + ` FROM plan_run_grants WHERE remote_session_id = ? AND plan_id = ?`
	args := []any{remoteSessionID, planID}
	if grantID != "" {
		query += ` AND id = ?`
		args = append(args, grantID)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT 1`
	grant, err := scanRunGrant(s.db.QueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return RunGrant{}, ErrRunGrantNotFound
	}
	if err != nil {
		return RunGrant{}, err
	}
	grant.computeEffective(s.now().UTC(), item.Status)
	return grant, nil
}

// ListRunGrants returns every grant of a plan, newest first, as an audit trail.
func (s *Service) ListRunGrants(ctx context.Context, remoteSessionID, planID string) ([]RunGrant, error) {
	item, err := loadPlanHeader(ctx, s.db, remoteSessionID, planID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+runGrantColumns+` FROM plan_run_grants
		WHERE remote_session_id = ? AND plan_id = ? ORDER BY created_at DESC, id DESC`, remoteSessionID, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := s.now().UTC()
	var grants []RunGrant
	for rows.Next() {
		grant, err := scanRunGrant(rows)
		if err != nil {
			return nil, err
		}
		grant.computeEffective(now, item.Status)
		grants = append(grants, grant)
	}
	return grants, rows.Err()
}

func (s *Service) RevokeRunGrant(ctx context.Context, remoteSessionID, planID, principalID, reason string) (RunGrant, error) {
	reason = strings.TrimSpace(reason)
	if principalID == "" || reason == "" {
		return RunGrant{}, fmt.Errorf("%w: principal and reason are required", ErrInvalidInput)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RunGrant{}, err
	}
	defer tx.Rollback()
	if _, err := loadPlanHeader(ctx, tx, remoteSessionID, planID); err != nil {
		return RunGrant{}, err
	}
	grant, err := loadActiveRunGrant(ctx, tx, remoteSessionID, planID)
	if err != nil {
		return RunGrant{}, err
	}
	now := s.now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE plan_run_grants SET status = 'revoked', ended_by = ?, ended_at = ?, end_reason = ?, updated_at = ?
		WHERE id = ? AND status = 'active'`, principalID, now.UnixMilli(), reason, now.UnixMilli(), grant.ID); err != nil {
		return RunGrant{}, err
	}
	if err := insertEventTx(ctx, tx, planID, "", "plan.run_grant.revoked", reason, principalID, map[string]any{"grant_id": grant.ID}, now); err != nil {
		return RunGrant{}, err
	}
	if err := tx.Commit(); err != nil {
		return RunGrant{}, err
	}
	return s.RunGrant(ctx, remoteSessionID, planID, grant.ID)
}

// ReportRunGrantUsage records the cumulative tokens spent under the active
// grant. The value is monotonic, so retries and out-of-order reports are safe.
func (s *Service) ReportRunGrantUsage(ctx context.Context, remoteSessionID, planID, principalID string, tokensUsedTotal int64) (RunGrant, error) {
	if principalID == "" || tokensUsedTotal < 0 {
		return RunGrant{}, fmt.Errorf("%w: principal and a non-negative tokens_used_total are required", ErrInvalidInput)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RunGrant{}, err
	}
	defer tx.Rollback()
	if _, err := loadPlanHeader(ctx, tx, remoteSessionID, planID); err != nil {
		return RunGrant{}, err
	}
	grant, err := loadActiveRunGrant(ctx, tx, remoteSessionID, planID)
	if err != nil {
		return RunGrant{}, err
	}
	now := s.now().UTC()
	if tokensUsedTotal > grant.TokensUsed {
		if _, err := tx.ExecContext(ctx, `UPDATE plan_run_grants SET tokens_used = ?, updated_at = ? WHERE id = ? AND tokens_used < ?`,
			tokensUsedTotal, now.UnixMilli(), grant.ID, tokensUsedTotal); err != nil {
			return RunGrant{}, err
		}
	}
	if err := insertEventTx(ctx, tx, planID, "", "plan.run_grant.usage_reported", "", principalID, map[string]any{
		"grant_id": grant.ID, "tokens_used_total": tokensUsedTotal, "previous_tokens_used": grant.TokensUsed, "max_tokens": grant.MaxTokens,
	}, now); err != nil {
		return RunGrant{}, err
	}
	if err := tx.Commit(); err != nil {
		return RunGrant{}, err
	}
	return s.RunGrant(ctx, remoteSessionID, planID, grant.ID)
}

// AuthorizeRunGrantUse atomically checks the active grant against one
// materialization request and records the outcome as a plan event. When the
// grant covers the request, a use row is written and a new idempotency key
// consumes one attempt (unless the task is already dispatched). Same-key
// retries replay without consuming budget.
func (s *Service) AuthorizeRunGrantUse(ctx context.Context, remoteSessionID, planID string, use RunGrantUse) (RunGrantDecision, error) {
	if remoteSessionID == "" || planID == "" || use.PlanTaskID == "" || use.PrincipalID == "" || strings.TrimSpace(use.IdempotencyKey) == "" {
		return RunGrantDecision{}, fmt.Errorf("%w: session, plan, task, principal, and idempotency key are required", ErrInvalidInput)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RunGrantDecision{}, err
	}
	defer tx.Rollback()
	item, err := loadPlanHeader(ctx, tx, remoteSessionID, planID)
	if err != nil {
		return RunGrantDecision{}, err
	}
	grant, err := loadActiveRunGrant(ctx, tx, remoteSessionID, planID)
	if errors.Is(err, ErrRunGrantNotFound) {
		return RunGrantDecision{Code: "no_active_grant", Reason: "this Plan has no active run grant"}, nil
	}
	if err != nil {
		return RunGrantDecision{}, err
	}
	now := s.now().UTC()
	grant.computeEffective(now, item.Status)
	repository := NormalizeRepository(use.Repository)
	risk := strings.TrimSpace(use.RiskLevel)
	decision := RunGrantDecision{Grant: &grant, Repository: repository, RiskLevel: risk}
	keyDigest := sha256.Sum256([]byte(strings.TrimSpace(use.IdempotencyKey)))
	keyHash := hex.EncodeToString(keyDigest[:])

	deny := func(code, reason string) (RunGrantDecision, error) {
		decision.Code, decision.Reason = code, reason
		if err := insertEventTx(ctx, tx, planID, use.PlanTaskID, "plan.run_grant.fallback", reason, use.PrincipalID, map[string]any{
			"grant_id": grant.ID, "code": code, "repository": repository, "risk_level": risk,
			"idempotency_key_sha256": keyHash, "effective_status": grant.Effective,
		}, now); err != nil {
			return RunGrantDecision{}, err
		}
		if err := tx.Commit(); err != nil {
			return RunGrantDecision{}, err
		}
		return decision, nil
	}

	if _, _, err := findTaskHeader(ctx, tx, planID, use.PlanTaskID); err != nil {
		return RunGrantDecision{}, err
	}
	switch grant.Effective {
	case RunGrantEffectiveActive, RunGrantEffectiveAttemptsExhausted:
		// Attempts are checked below so same-key retries can still replay.
	default:
		return deny(grant.Effective, "run grant is "+grant.Effective)
	}
	if repository == "" {
		return deny("repository_required", "repository (owner/name) is required to use the run grant")
	}
	if !containsString(grant.Repositories, repository) {
		return deny("repository_out_of_scope", fmt.Sprintf("repository %s is not in the run grant scope", repository))
	}
	if riskRank(risk) == 0 {
		return deny("risk_level_required", "risk_level must be one of "+strings.Join(TaskRiskLevels(), ", ")+" to use the run grant")
	}
	decision.AutoMergeAllowed = AutoMergeAllowed(risk, grant.AutoMergeMaxRisk)

	var existing int
	err = tx.QueryRowContext(ctx, `SELECT attempt_consumed FROM plan_run_grant_uses WHERE grant_id = ? AND plan_task_id = ? AND idempotency_key_sha256 = ?`,
		grant.ID, use.PlanTaskID, keyHash).Scan(&existing)
	switch {
	case err == nil:
		decision.Authorized, decision.Replay = true, true
	case errors.Is(err, sql.ErrNoRows):
		var dispatchStatus string
		dispatchErr := tx.QueryRowContext(ctx, `SELECT status FROM plan_task_multica_dispatches WHERE task_id = ? AND plan_id = ? AND remote_session_id = ?`,
			use.PlanTaskID, planID, remoteSessionID).Scan(&dispatchStatus)
		if dispatchErr != nil && !errors.Is(dispatchErr, sql.ErrNoRows) {
			return RunGrantDecision{}, dispatchErr
		}
		consume := dispatchStatus != "dispatched"
		if consume && grant.AttemptsUsed >= grant.MaxAttempts {
			return deny(RunGrantEffectiveAttemptsExhausted, fmt.Sprintf("run grant used all %d attempts", grant.MaxAttempts))
		}
		consumed := 0
		if consume {
			consumed = 1
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO plan_run_grant_uses
			(grant_id, plan_task_id, idempotency_key_sha256, repository, risk_level, attempt_consumed, principal_id, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, grant.ID, use.PlanTaskID, keyHash, repository, risk, consumed, use.PrincipalID, now.UnixMilli()); err != nil {
			return RunGrantDecision{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE plan_run_grants SET attempts_used = attempts_used + ?,
			first_used_at = COALESCE(first_used_at, ?), updated_at = ? WHERE id = ? AND status = 'active'`,
			consumed, now.UnixMilli(), now.UnixMilli(), grant.ID); err != nil {
			return RunGrantDecision{}, err
		}
		grant.AttemptsUsed += consumed
		if grant.FirstUsedAt == nil {
			first := now
			grant.FirstUsedAt = &first
		}
		grant.computeEffective(now, item.Status)
		decision.Authorized, decision.AttemptConsumed = true, consume
	default:
		return RunGrantDecision{}, err
	}
	if err := insertEventTx(ctx, tx, planID, use.PlanTaskID, "plan.run_grant.used", "", use.PrincipalID, map[string]any{
		"grant_id": grant.ID, "repository": repository, "risk_level": risk, "auto_merge_allowed": decision.AutoMergeAllowed,
		"attempt_consumed": decision.AttemptConsumed, "replay": decision.Replay,
		"attempts_used": grant.AttemptsUsed, "max_attempts": grant.MaxAttempts, "idempotency_key_sha256": keyHash,
	}, now); err != nil {
		return RunGrantDecision{}, err
	}
	if err := tx.Commit(); err != nil {
		return RunGrantDecision{}, err
	}
	return decision, nil
}

func (g *RunGrant) computeEffective(now time.Time, planStatus string) {
	g.RuntimeDeadline = nil
	if g.FirstUsedAt != nil {
		deadline := g.FirstUsedAt.Add(time.Duration(g.MaxRuntimeMs) * time.Millisecond)
		g.RuntimeDeadline = &deadline
	}
	switch {
	case g.Status == RunGrantRevoked:
		g.Effective = RunGrantEffectiveRevoked
	case g.Status == RunGrantSuperseded:
		g.Effective = RunGrantEffectiveSuperseded
	case planStatus == PlanCompleted || planStatus == PlanCancelled:
		g.Effective = RunGrantEffectivePlanClosed
	case !now.Before(g.ExpiresAt):
		g.Effective = RunGrantEffectiveExpired
	case g.RuntimeDeadline != nil && !now.Before(*g.RuntimeDeadline):
		g.Effective = RunGrantEffectiveRuntimeExhausted
	case g.TokensUsed >= g.MaxTokens:
		g.Effective = RunGrantEffectiveTokensExhausted
	case g.AttemptsUsed >= g.MaxAttempts:
		g.Effective = RunGrantEffectiveAttemptsExhausted
	default:
		g.Effective = RunGrantEffectiveActive
	}
}

const runGrantColumns = `id, plan_id, remote_session_id, status, repositories_json, auto_merge_max_risk, max_tokens, tokens_used,
	max_runtime_ms, max_attempts, attempts_used, expires_at, first_used_at, note, created_by, created_at, updated_at, ended_by, ended_at, end_reason`

type rowScanner interface{ Scan(...any) error }

func scanRunGrant(row rowScanner) (RunGrant, error) {
	var g RunGrant
	var repos string
	var expiresAt, createdAt, updatedAt int64
	var firstUsedAt, endedAt sql.NullInt64
	if err := row.Scan(&g.ID, &g.PlanID, &g.RemoteSessionID, &g.Status, &repos, &g.AutoMergeMaxRisk, &g.MaxTokens, &g.TokensUsed,
		&g.MaxRuntimeMs, &g.MaxAttempts, &g.AttemptsUsed, &expiresAt, &firstUsedAt, &g.Note, &g.CreatedBy, &createdAt, &updatedAt,
		&g.EndedBy, &endedAt, &g.EndReason); err != nil {
		return RunGrant{}, err
	}
	if err := json.Unmarshal([]byte(repos), &g.Repositories); err != nil {
		return RunGrant{}, err
	}
	g.ExpiresAt = time.UnixMilli(expiresAt).UTC()
	g.CreatedAt = time.UnixMilli(createdAt).UTC()
	g.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	g.FirstUsedAt = timeFromNullable(firstUsedAt)
	g.EndedAt = timeFromNullable(endedAt)
	return g, nil
}

func loadActiveRunGrant(ctx context.Context, q queryer, remoteSessionID, planID string) (RunGrant, error) {
	grant, err := scanRunGrant(q.QueryRowContext(ctx, `SELECT `+runGrantColumns+` FROM plan_run_grants
		WHERE remote_session_id = ? AND plan_id = ? AND status = 'active'`, remoteSessionID, planID))
	if errors.Is(err, sql.ErrNoRows) {
		return RunGrant{}, ErrRunGrantNotFound
	}
	return grant, err
}

type planHeader struct {
	ID     string
	Status string
}

func loadPlanHeader(ctx context.Context, q queryer, remoteSessionID, planID string) (planHeader, error) {
	var header planHeader
	err := q.QueryRowContext(ctx, `SELECT id, status FROM plans WHERE id = ? AND remote_session_id = ?`, planID, remoteSessionID).Scan(&header.ID, &header.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return planHeader{}, ErrNotFound
	}
	return header, err
}

func findTaskHeader(ctx context.Context, q queryer, planID, taskID string) (string, string, error) {
	var id, status string
	err := q.QueryRowContext(ctx, `SELECT id, status FROM plan_tasks WHERE id = ? AND plan_id = ?`, taskID, planID).Scan(&id, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", fmt.Errorf("%w: task %s", ErrNotFound, taskID)
	}
	return id, status, err
}

func normalizeRepositories(values []string) ([]string, error) {
	if len(values) == 0 || len(values) > maxRunGrantRepositories {
		return nil, fmt.Errorf("%w: repositories must list between 1 and %d owner/name entries", ErrInvalidInput, maxRunGrantRepositories)
	}
	result := make([]string, 0, len(values))
	for _, raw := range values {
		repository := NormalizeRepository(raw)
		if repository == "" {
			return nil, fmt.Errorf("%w: repository %q must be owner/name without wildcards", ErrInvalidInput, raw)
		}
		if !containsString(result, repository) {
			result = append(result, repository)
		}
	}
	return result, nil
}
