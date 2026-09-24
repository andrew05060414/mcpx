package plan

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var (
	ErrDispatchInProgress = errors.New("Multica dispatch is already being materialized")
	ErrDispatchConflict   = errors.New("Plan Task already has a Multica dispatch with different routing")
)

// ClaimMulticaDispatch reserves the one allowed Multica Issue for a Plan Task.
// A stale creating claim may be reclaimed after the caller first reconciles
// the stable marker stored in the external Issue description.
func (s *Service) ClaimMulticaDispatch(
	ctx context.Context,
	remoteSessionID, planID, taskID, principalID, assigneeID, projectID, priority string,
	lease time.Duration,
) (MulticaDispatch, bool, error) {
	if remoteSessionID == "" || planID == "" || taskID == "" || principalID == "" || assigneeID == "" {
		return MulticaDispatch{}, false, fmt.Errorf("%w: session, plan, task, principal, and assignee are required", ErrInvalidInput)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MulticaDispatch{}, false, err
	}
	defer tx.Rollback()
	item, err := loadPlan(ctx, tx, remoteSessionID, planID)
	if err != nil {
		return MulticaDispatch{}, false, err
	}
	task, _, err := findTask(item.Tasks, taskID)
	if err != nil {
		return MulticaDispatch{}, false, err
	}
	now := s.now().UTC()
	var d MulticaDispatch
	var createdAt, updatedAt int64
	err = tx.QueryRowContext(ctx, `SELECT plan_id, task_id, issue_id, issue_identifier, status, assignee_id, project_id, priority, created_at, updated_at
		FROM plan_task_multica_dispatches WHERE task_id = ? AND plan_id = ? AND remote_session_id = ?`, taskID, planID, remoteSessionID).Scan(
		&d.PlanID, &d.PlanTaskID, &d.IssueID, &d.IssueIdentifier, &d.Status, &d.AssigneeID, &d.ProjectID, &d.Priority, &createdAt, &updatedAt)
	if err == nil {
		d.CreatedAt = time.UnixMilli(createdAt).UTC()
		d.UpdatedAt = time.UnixMilli(updatedAt).UTC()
		if d.AssigneeID != assigneeID || d.ProjectID != projectID || d.Priority != priority {
			return d, false, ErrDispatchConflict
		}
		if d.Status == "dispatched" {
			return d, false, nil
		}
		if item.Status == PlanCompleted || item.Status == PlanCancelled {
			return d, false, fmt.Errorf("%w: plan is %s", ErrInvalidState, item.Status)
		}
		if task.Status != TaskTodo {
			return d, false, fmt.Errorf("%w: task %s is %s", ErrInvalidState, task.ID, task.Status)
		}
		if err := dependenciesComplete(item.Tasks, task); err != nil {
			return d, false, err
		}
		if now.Sub(d.UpdatedAt) < lease {
			return d, false, ErrDispatchInProgress
		}
		result, err := tx.ExecContext(ctx, `UPDATE plan_task_multica_dispatches SET updated_at = ?, created_by = ?
			WHERE task_id = ? AND status = 'creating' AND updated_at = ?`, now.UnixMilli(), principalID, taskID, updatedAt)
		if err != nil {
			return d, false, err
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			if err == nil {
				err = ErrDispatchInProgress
			}
			return d, false, err
		}
		d.UpdatedAt = now
		if err := tx.Commit(); err != nil {
			return MulticaDispatch{}, false, err
		}
		return d, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return MulticaDispatch{}, false, err
	}
	if item.Status == PlanCompleted || item.Status == PlanCancelled {
		return MulticaDispatch{}, false, fmt.Errorf("%w: plan is %s", ErrInvalidState, item.Status)
	}
	if task.Status != TaskTodo {
		return MulticaDispatch{}, false, fmt.Errorf("%w: task %s is %s", ErrInvalidState, task.ID, task.Status)
	}
	if err := dependenciesComplete(item.Tasks, task); err != nil {
		return MulticaDispatch{}, false, err
	}
	d = MulticaDispatch{
		PlanID: planID, PlanTaskID: taskID, Status: "creating", AssigneeID: assigneeID,
		ProjectID: projectID, Priority: priority, CreatedAt: now, UpdatedAt: now,
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO plan_task_multica_dispatches
		(plan_id, task_id, remote_session_id, status, assignee_id, project_id, priority, created_by, created_at, updated_at)
		VALUES (?, ?, ?, 'creating', ?, ?, ?, ?, ?, ?)`, planID, taskID, remoteSessionID, assigneeID, projectID, priority, principalID, now.UnixMilli(), now.UnixMilli()); err != nil {
		return MulticaDispatch{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return MulticaDispatch{}, false, err
	}
	return d, true, nil
}

func (s *Service) CompleteMulticaDispatch(ctx context.Context, remoteSessionID, principalID string, dispatch MulticaDispatch, issueID, identifier string) (MulticaDispatch, error) {
	if issueID == "" || dispatch.PlanID == "" || dispatch.PlanTaskID == "" {
		return MulticaDispatch{}, fmt.Errorf("%w: plan task and Multica issue ID are required", ErrInvalidInput)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MulticaDispatch{}, err
	}
	defer tx.Rollback()
	item, err := loadPlan(ctx, tx, remoteSessionID, dispatch.PlanID)
	if err != nil {
		return MulticaDispatch{}, err
	}
	if _, _, err := findTask(item.Tasks, dispatch.PlanTaskID); err != nil {
		return MulticaDispatch{}, err
	}
	now := s.now().UTC()
	result, err := tx.ExecContext(ctx, `UPDATE plan_task_multica_dispatches
		SET status = 'dispatched', issue_id = ?, issue_identifier = ?, updated_at = ?
		WHERE plan_id = ? AND task_id = ? AND remote_session_id = ? AND status = 'creating'`,
		issueID, identifier, now.UnixMilli(), dispatch.PlanID, dispatch.PlanTaskID, remoteSessionID)
	if err != nil {
		return MulticaDispatch{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return MulticaDispatch{}, err
	}
	if changed == 0 {
		var existing MulticaDispatch
		var createdAt, updatedAt int64
		if err := tx.QueryRowContext(ctx, `SELECT plan_id, task_id, issue_id, issue_identifier, status, assignee_id, project_id, priority, created_at, updated_at
			FROM plan_task_multica_dispatches WHERE plan_id = ? AND task_id = ? AND remote_session_id = ?`,
			dispatch.PlanID, dispatch.PlanTaskID, remoteSessionID).Scan(
			&existing.PlanID, &existing.PlanTaskID, &existing.IssueID, &existing.IssueIdentifier, &existing.Status,
			&existing.AssigneeID, &existing.ProjectID, &existing.Priority, &createdAt, &updatedAt); err != nil {
			return MulticaDispatch{}, err
		}
		if existing.Status != "dispatched" || existing.IssueID != issueID {
			return MulticaDispatch{}, ErrDispatchConflict
		}
		existing.CreatedAt, existing.UpdatedAt = time.UnixMilli(createdAt).UTC(), time.UnixMilli(updatedAt).UTC()
		return existing, tx.Commit()
	}
	if item.Status != PlanCompleted && item.Status != PlanCancelled {
		if _, err := tx.ExecContext(ctx, `UPDATE plan_tasks SET status = 'in_progress', updated_at = ? WHERE id = ? AND plan_id = ? AND status = 'todo'`,
			now.UnixMilli(), dispatch.PlanTaskID, dispatch.PlanID); err != nil {
			return MulticaDispatch{}, err
		}
		if item.Status == PlanReady || item.Status == PlanBlocked {
			if _, err := tx.ExecContext(ctx, `UPDATE plans SET status = 'in_progress', version = version + 1, updated_at = ? WHERE id = ? AND remote_session_id = ?`,
				now.UnixMilli(), dispatch.PlanID, remoteSessionID); err != nil {
				return MulticaDispatch{}, err
			}
		} else if _, err := tx.ExecContext(ctx, `UPDATE plans SET version = version + 1, updated_at = ? WHERE id = ? AND remote_session_id = ?`,
			now.UnixMilli(), dispatch.PlanID, remoteSessionID); err != nil {
			return MulticaDispatch{}, err
		}
	}
	if err := insertEventTx(ctx, tx, dispatch.PlanID, dispatch.PlanTaskID, "plan_task.multica_dispatched", "", principalID,
		map[string]any{"issue_id": issueID, "issue_identifier": identifier, "assignee_id": dispatch.AssigneeID, "project_id": dispatch.ProjectID}, now); err != nil {
		return MulticaDispatch{}, err
	}
	if err := tx.Commit(); err != nil {
		return MulticaDispatch{}, err
	}
	dispatch.Status, dispatch.IssueID, dispatch.IssueIdentifier, dispatch.UpdatedAt = "dispatched", issueID, identifier, now
	return dispatch, nil
}
