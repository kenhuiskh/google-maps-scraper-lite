package gmaps

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (s *JobStore) CreateStrategyJobsWithSource(ctx context.Context, jobs []StrategyJobCreate) ([]string, string, error) {
	if len(jobs) == 0 {
		return nil, "", nil
	}
	type encodedJob struct {
		queriesJSON string
		configJSON  string
		job         StrategyJobCreate
	}
	encoded := make([]encodedJob, 0, len(jobs))
	for _, job := range jobs {
		queriesJSON, err := json.Marshal(job.Queries)
		if err != nil {
			return nil, "", err
		}
		configJSON, err := json.Marshal(job.Config)
		if err != nil {
			return nil, "", err
		}
		encoded = append(encoded, encodedJob{
			queriesJSON: string(queriesJSON),
			configJSON:  string(configJSON),
			job:         job,
		})
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback()
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE status IN (?, ?)`,
		JobStatusStarting, JobStatusRunning).Scan(&active); err != nil {
		return nil, "", err
	}

	ids := make([]string, 0, len(encoded))
	var startedID string
	var previous time.Time
	for i, job := range encoded {
		now := time.Now().UTC()
		if !previous.IsZero() && !now.After(previous) {
			now = previous.Add(time.Nanosecond)
		}
		previous = now
		id := newJobID(now)
		status := JobStatusPending
		event := "queued"
		message := "job queued by strategy"
		if i == 0 && active == 0 {
			status = JobStatusStarting
			event = "starting"
			message = "strategy job accepted by control UI"
			startedID = id
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO jobs
			(id, queries_json, config_json, status, template_id, strategy_id, strategy_run_id, created_at, updated_at)
			VALUES (?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?, ?)`,
			id, job.queriesJSON, job.configJSON, status, job.job.TemplateID, job.job.StrategyID, job.job.StrategyRunID, now, now); err != nil {
			return nil, "", err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO job_events (job_id, event, message, created_at) VALUES (?, ?, ?, ?)`,
			id, event, message, now); err != nil {
			return nil, "", err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO job_execution_stats (job_id, updated_at) VALUES (?, ?)`, id, now); err != nil {
			return nil, "", err
		}
		ids = append(ids, id)
	}
	return ids, startedID, tx.Commit()
}

func (s *JobStore) SaveStrategy(ctx context.Context, id, name, notes string, templateIDs []string) (string, error) {
	now := time.Now().UTC()
	id = strings.TrimSpace(id)
	if id == "" {
		id = newStrategyID(now)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("strategy name is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO strategies (id, name, notes, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET name = excluded.name, notes = excluded.notes, updated_at = excluded.updated_at`,
		id, name, strings.TrimSpace(notes), now, now); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM strategy_templates WHERE strategy_id = ?`, id); err != nil {
		return "", err
	}
	seen := make(map[string]struct{}, len(templateIDs))
	position := 0
	for _, templateID := range templateIDs {
		templateID = strings.TrimSpace(templateID)
		if templateID == "" {
			continue
		}
		if _, ok := seen[templateID]; ok {
			continue
		}
		seen[templateID] = struct{}{}
		if _, err := tx.ExecContext(ctx, `INSERT INTO strategy_templates (strategy_id, template_id, position, created_at)
			VALUES (?, ?, ?, ?)`, id, templateID, position, now); err != nil {
			return "", err
		}
		position++
	}
	return id, tx.Commit()
}

func (s *JobStore) GetStrategy(ctx context.Context, id string) (*Strategy, error) {
	var strategy Strategy
	err := s.db.QueryRowContext(ctx, `SELECT id, name, notes, created_at, updated_at, last_used_at
		FROM strategies WHERE id = ?`, id).Scan(&strategy.ID, &strategy.Name, &strategy.Notes, &strategy.CreatedAt, &strategy.UpdatedAt, &strategy.LastUsedAt)
	if err != nil {
		return nil, err
	}
	templates, err := s.listStrategyTemplates(ctx, id)
	if err != nil {
		return nil, err
	}
	strategy.Templates = templates
	return &strategy, nil
}

func (s *JobStore) ListStrategies(ctx context.Context) ([]Strategy, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, notes, created_at, updated_at, last_used_at
		FROM strategies ORDER BY updated_at DESC, created_at DESC`)
	if err != nil {
		return nil, err
	}
	var strategies []Strategy
	for rows.Next() {
		var strategy Strategy
		if err := rows.Scan(&strategy.ID, &strategy.Name, &strategy.Notes, &strategy.CreatedAt, &strategy.UpdatedAt, &strategy.LastUsedAt); err != nil {
			rows.Close()
			return nil, err
		}
		strategies = append(strategies, strategy)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range strategies {
		templates, err := s.listStrategyTemplates(ctx, strategies[i].ID)
		if err != nil {
			return nil, err
		}
		strategies[i].Templates = templates
	}
	return strategies, nil
}

func (s *JobStore) listStrategyTemplates(ctx context.Context, strategyID string) ([]JobTemplate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT jt.id, jt.name, jt.params_json, jt.created_at, jt.last_used_at
		FROM strategy_templates st
		JOIN job_templates jt ON jt.id = st.template_id
		WHERE st.strategy_id = ?
		ORDER BY st.position ASC`, strategyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var templates []JobTemplate
	for rows.Next() {
		var tpl JobTemplate
		if err := rows.Scan(&tpl.ID, &tpl.Name, &tpl.ParamsJSON, &tpl.CreatedAt, &tpl.LastUsedAt); err != nil {
			return nil, err
		}
		templates = append(templates, tpl)
	}
	return templates, rows.Err()
}

func (s *JobStore) DeleteStrategy(ctx context.Context, id string, deleteTemplates bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var templateIDs []string
	if deleteTemplates {
		rows, err := tx.QueryContext(ctx, `SELECT template_id FROM strategy_templates WHERE strategy_id = ?`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var tplID string
			if err := rows.Scan(&tplID); err != nil {
				rows.Close()
				return err
			}
			templateIDs = append(templateIDs, tplID)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}

	res, err := tx.ExecContext(ctx, `DELETE FROM strategies WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}

	// Strategy is gone (its join rows cascaded). Delete only templates that are
	// now unreferenced; shared templates still have a row for another strategy.
	for _, tplID := range templateIDs {
		var refs int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM strategy_templates WHERE template_id = ?`, tplID).Scan(&refs); err != nil {
			return err
		}
		if refs > 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM job_templates WHERE id = ?`, tplID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *JobStore) MarkStrategyUsed(ctx context.Context, id string) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `UPDATE strategies SET last_used_at = ?, updated_at = ? WHERE id = ?`, now, now, id)
	return err
}

// BulkUpdateStrategyLang updates the Lang field in-place for every template
// belonging to the given strategy. Because templates may be shared across
// strategies, this affects all strategies that reference the same templates.
func (s *JobStore) BulkUpdateStrategyLang(ctx context.Context, strategyID, lang string) (int, error) {
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM strategies WHERE id = ?`, strategyID).Scan(&exists); err != nil {
		return 0, err
	}
	if exists == 0 {
		return 0, sql.ErrNoRows
	}

	rows, err := s.db.QueryContext(ctx, `SELECT template_id FROM strategy_templates WHERE strategy_id = ?`, strategyID)
	if err != nil {
		return 0, err
	}
	var templateIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		templateIDs = append(templateIDs, id)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	for _, tplID := range templateIDs {
		var paramsJSON string
		if err := tx.QueryRowContext(ctx, `SELECT params_json FROM job_templates WHERE id = ?`, tplID).Scan(&paramsJSON); err != nil {
			return 0, err
		}
		var params map[string]any
		if err := json.Unmarshal([]byte(paramsJSON), &params); err != nil {
			return 0, err
		}
		if lang == "" {
			delete(params, "Lang")
		} else {
			params["Lang"] = lang
		}
		updated, err := json.Marshal(params)
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE job_templates SET params_json = ?, last_used_at = ? WHERE id = ?`,
			string(updated), now, tplID); err != nil {
			return 0, err
		}
	}
	return len(templateIDs), tx.Commit()
}

// BulkUpdateStrategyDedupScope updates the DedupScope field in-place for every
// template belonging to the given strategy. Empty scope disables the option.
func (s *JobStore) BulkUpdateStrategyDedupScope(ctx context.Context, strategyID, scope string) (int, error) {
	if scope != "" && scope != "run" && scope != "all" {
		return 0, fmt.Errorf("invalid dedup scope %q", scope)
	}
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM strategies WHERE id = ?`, strategyID).Scan(&exists); err != nil {
		return 0, err
	}
	if exists == 0 {
		return 0, sql.ErrNoRows
	}

	rows, err := s.db.QueryContext(ctx, `SELECT template_id FROM strategy_templates WHERE strategy_id = ?`, strategyID)
	if err != nil {
		return 0, err
	}
	var templateIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		templateIDs = append(templateIDs, id)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	for _, tplID := range templateIDs {
		var paramsJSON string
		if err := tx.QueryRowContext(ctx, `SELECT params_json FROM job_templates WHERE id = ?`, tplID).Scan(&paramsJSON); err != nil {
			return 0, err
		}
		var params map[string]any
		if err := json.Unmarshal([]byte(paramsJSON), &params); err != nil {
			return 0, err
		}
		if scope == "" {
			delete(params, "DedupScope")
		} else {
			params["DedupScope"] = scope
		}
		updated, err := json.Marshal(params)
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE job_templates SET params_json = ?, last_used_at = ? WHERE id = ?`,
			string(updated), now, tplID); err != nil {
			return 0, err
		}
	}
	return len(templateIDs), tx.Commit()
}

// BulkDuplicateStrategyTemplatesWithLang clones every template in the strategy
// with the new lang and nameSuffix appended to each template name, then
// re-links the strategy to the new copies. Original templates are untouched.
func (s *JobStore) BulkDuplicateStrategyTemplatesWithLang(ctx context.Context, strategyID, lang, nameSuffix string) (int, error) {
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM strategies WHERE id = ?`, strategyID).Scan(&exists); err != nil {
		return 0, err
	}
	if exists == 0 {
		return 0, sql.ErrNoRows
	}

	type entry struct {
		templateID string
		position   int
		name       string
		paramsJSON string
	}
	rows, err := s.db.QueryContext(ctx, `SELECT st.template_id, st.position, jt.name, jt.params_json
		FROM strategy_templates st
		JOIN job_templates jt ON jt.id = st.template_id
		WHERE st.strategy_id = ?
		ORDER BY st.position ASC`, strategyID)
	if err != nil {
		return 0, err
	}
	var entries []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.templateID, &e.position, &e.name, &e.paramsJSON); err != nil {
			rows.Close()
			return 0, err
		}
		entries = append(entries, e)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	for _, e := range entries {
		var params map[string]any
		if err := json.Unmarshal([]byte(e.paramsJSON), &params); err != nil {
			return 0, err
		}
		if lang == "" {
			delete(params, "Lang")
		} else {
			params["Lang"] = lang
		}
		newParamsJSON, err := json.Marshal(params)
		if err != nil {
			return 0, err
		}
		sum := sha256.Sum256(newParamsJSON)
		newID := fmt.Sprintf("tpl_%x", sum[:12])
		newName := strings.TrimSpace(e.name) + " " + strings.TrimSpace(nameSuffix)

		if _, err := tx.ExecContext(ctx, `INSERT INTO job_templates (id, name, params_json, created_at, last_used_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET name = excluded.name, last_used_at = excluded.last_used_at`,
			newID, newName, string(newParamsJSON), now, now); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM strategy_templates WHERE strategy_id = ? AND template_id = ?`,
			strategyID, e.templateID); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO strategy_templates (strategy_id, template_id, position, created_at)
			VALUES (?, ?, ?, ?)`, strategyID, newID, e.position, now); err != nil {
			return 0, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE strategies SET updated_at = ? WHERE id = ?`, now, strategyID); err != nil {
		return 0, err
	}
	return len(entries), tx.Commit()
}
