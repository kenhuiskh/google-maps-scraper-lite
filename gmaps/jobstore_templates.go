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

func (s *JobStore) SaveJobTemplate(ctx context.Context, name string, params any) (string, error) {
	now := time.Now().UTC()
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(paramsJSON)
	id := fmt.Sprintf("tpl_%x", sum[:12])
	name = strings.TrimSpace(name)
	if name == "" {
		name = id
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO job_templates (id, name, params_json, created_at, last_used_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			params_json = excluded.params_json,
			last_used_at = excluded.last_used_at`,
		id, name, string(paramsJSON), now, now)
	if err != nil {
		return "", err
	}
	return id, nil
}

func (s *JobStore) SaveJobTemplateJSON(ctx context.Context, id, name, paramsJSON string) (string, error) {
	now := time.Now().UTC()
	paramsJSON = strings.TrimSpace(paramsJSON)
	if paramsJSON == "" {
		paramsJSON = "{}"
	}
	if !json.Valid([]byte(paramsJSON)) {
		return "", errors.New("template params must be valid JSON")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		sum := sha256.Sum256([]byte(paramsJSON))
		id = fmt.Sprintf("tpl_%x", sum[:12])
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = id
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO job_templates (id, name, params_json, created_at, last_used_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			params_json = excluded.params_json,
			last_used_at = excluded.last_used_at`,
		id, name, paramsJSON, now, now)
	if err != nil {
		return "", err
	}
	return id, nil
}

func (s *JobStore) GetJobTemplate(ctx context.Context, id string) (*JobTemplate, error) {
	var tpl JobTemplate
	err := s.db.QueryRowContext(ctx, `SELECT id, name, params_json, created_at, last_used_at
		FROM job_templates WHERE id = ?`, id).Scan(&tpl.ID, &tpl.Name, &tpl.ParamsJSON, &tpl.CreatedAt, &tpl.LastUsedAt)
	if err != nil {
		return nil, err
	}
	return &tpl, nil
}

func (s *JobStore) ListJobTemplates(ctx context.Context) ([]JobTemplate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, params_json, created_at, last_used_at
		FROM job_templates
		ORDER BY last_used_at DESC, created_at DESC`)
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

func (s *JobStore) DeleteJobTemplate(ctx context.Context, id string) error {
	var refs int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM strategy_templates WHERE template_id = ?`, id).Scan(&refs); err != nil {
		return err
	}
	if refs > 0 {
		return fmt.Errorf("%w: used by %d strategy entries", ErrTemplateReferenced, refs)
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM job_templates WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// TemplateDeleteResult reports the outcome of deleting one template in a batch.
// Status is one of "deleted", "skipped_referenced", or "not_found".
type TemplateDeleteResult struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// BatchDeleteJobTemplates deletes each template by reusing DeleteJobTemplate,
// which refuses templates still referenced by a strategy. Referenced templates
// are reported as "skipped_referenced", unknown ids as "not_found"; the rest as
// "deleted".
func (s *JobStore) BatchDeleteJobTemplates(ctx context.Context, ids []string) ([]TemplateDeleteResult, error) {
	results := make([]TemplateDeleteResult, 0, len(ids))
	for _, id := range ids {
		status := "deleted"
		switch err := s.DeleteJobTemplate(ctx, id); {
		case err == nil:
		case errors.Is(err, sql.ErrNoRows):
			status = "not_found"
		case errors.Is(err, ErrTemplateReferenced):
			status = "skipped_referenced"
		default:
			return nil, err
		}
		results = append(results, TemplateDeleteResult{ID: id, Status: status})
	}
	return results, nil
}
