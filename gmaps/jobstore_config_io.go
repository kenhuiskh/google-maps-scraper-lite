package gmaps

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func (s *JobStore) ExportReusableConfig(ctx context.Context) (ReusableConfigExport, error) {
	return s.ExportReusableConfigSelection(ctx, nil, nil)
}

func (s *JobStore) ExportReusableConfigSelection(ctx context.Context, templateIDs, strategyIDs []string) (ReusableConfigExport, error) {
	templates, err := s.ListJobTemplates(ctx)
	if err != nil {
		return ReusableConfigExport{}, err
	}
	strategies, err := s.ListStrategies(ctx)
	if err != nil {
		return ReusableConfigExport{}, err
	}
	selectedTemplates := normalizedIDSet(templateIDs)
	selectedStrategies := normalizedIDSet(strategyIDs)
	filtered := len(selectedTemplates) > 0 || len(selectedStrategies) > 0
	if filtered {
		knownTemplates := make(map[string]struct{}, len(templates))
		for _, tpl := range templates {
			knownTemplates[tpl.ID] = struct{}{}
		}
		for id := range selectedTemplates {
			if _, ok := knownTemplates[id]; !ok {
				return ReusableConfigExport{}, fmt.Errorf("%w: template %q was not found", ErrConfigExportInvalid, id)
			}
		}
		knownStrategies := make(map[string]struct{}, len(strategies))
		for _, strategy := range strategies {
			knownStrategies[strategy.ID] = struct{}{}
		}
		for id := range selectedStrategies {
			if _, ok := knownStrategies[id]; !ok {
				return ReusableConfigExport{}, fmt.Errorf("%w: strategy %q was not found", ErrConfigExportInvalid, id)
			}
		}
		for _, strategy := range strategies {
			if _, ok := selectedStrategies[strategy.ID]; !ok {
				continue
			}
			for _, tpl := range strategy.Templates {
				selectedTemplates[tpl.ID] = struct{}{}
			}
		}
	}
	out := ReusableConfigExport{
		Version:    ConfigExportVersion,
		ExportedAt: time.Now().UTC(),
		Source:     "google-maps-scraper-lite",
		Templates:  make([]ReusableConfigTemplate, 0, len(templates)),
		Strategies: make([]ReusableConfigStrategy, 0, len(strategies)),
	}
	for _, tpl := range templates {
		if filtered {
			if _, ok := selectedTemplates[tpl.ID]; !ok {
				continue
			}
		}
		out.Templates = append(out.Templates, ReusableConfigTemplate{
			ID:         tpl.ID,
			Name:       tpl.Name,
			ParamsJSON: tpl.ParamsJSON,
			CreatedAt:  tpl.CreatedAt,
			LastUsedAt: tpl.LastUsedAt,
		})
	}
	for _, strategy := range strategies {
		if filtered {
			if _, ok := selectedStrategies[strategy.ID]; !ok {
				continue
			}
		}
		templateIDs := make([]string, 0, len(strategy.Templates))
		for _, tpl := range strategy.Templates {
			templateIDs = append(templateIDs, tpl.ID)
		}
		exported := ReusableConfigStrategy{
			ID:          strategy.ID,
			Name:        strategy.Name,
			Notes:       strategy.Notes,
			TemplateIDs: templateIDs,
			CreatedAt:   strategy.CreatedAt,
			UpdatedAt:   strategy.UpdatedAt,
		}
		if strategy.LastUsedAt.Valid {
			exported.LastUsedAt = strategy.LastUsedAt.Time
		}
		out.Strategies = append(out.Strategies, exported)
	}
	return out, nil
}

func normalizedIDSet(ids []string) map[string]struct{} {
	out := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id != "" {
			out[id] = struct{}{}
		}
	}
	return out
}

func (s *JobStore) ImportReusableConfig(ctx context.Context, cfg ReusableConfigExport, mode ConfigImportMode) (ConfigImportSummary, error) {
	if mode == "" {
		mode = ConfigImportRename
	}
	if !validConfigImportMode(mode) {
		return ConfigImportSummary{}, fmt.Errorf("%w: unsupported collision mode %q", ErrConfigImportInvalid, mode)
	}
	if err := validateReusableConfig(cfg); err != nil {
		return ConfigImportSummary{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ConfigImportSummary{}, err
	}
	defer tx.Rollback()

	summary := ConfigImportSummary{Mode: mode}
	now := time.Now().UTC()
	existingTemplates, err := txStringSet(ctx, tx, `SELECT id FROM job_templates`)
	if err != nil {
		return ConfigImportSummary{}, err
	}
	existingStrategies, err := txStringSet(ctx, tx, `SELECT id FROM strategies`)
	if err != nil {
		return ConfigImportSummary{}, err
	}

	templatePayload := make(map[string]ReusableConfigTemplate, len(cfg.Templates))
	templateMap := make(map[string]string, len(cfg.Templates))
	usedTemplateIDs := copyStringSet(existingTemplates)
	for _, tpl := range cfg.Templates {
		templatePayload[tpl.ID] = tpl
		targetID := tpl.ID
		_, exists := existingTemplates[tpl.ID]
		switch mode {
		case ConfigImportSkip:
			if exists {
				summary.Templates.Skipped++
				templateMap[tpl.ID] = tpl.ID
				continue
			}
			summary.Templates.Created++
		case ConfigImportOverwrite:
			if exists {
				summary.Templates.Updated++
			} else {
				summary.Templates.Created++
			}
		case ConfigImportRename:
			if exists {
				targetID = uniqueImportID("tpl", tpl.ID, usedTemplateIDs)
				summary.Templates.Renamed++
				summary.Templates.Created++
			} else {
				summary.Templates.Created++
			}
		case ConfigImportDuplicate:
			targetID = uniqueImportID("tpl", tpl.ID, usedTemplateIDs)
			summary.Templates.Duplicated++
			summary.Templates.Created++
		}
		usedTemplateIDs[targetID] = struct{}{}
		templateMap[tpl.ID] = targetID
		name := importName(tpl.Name, targetID, targetID != tpl.ID, mode)
		if err := upsertTemplateTx(ctx, tx, targetID, name, tpl.ParamsJSON, importTime(tpl.CreatedAt, now), importTime(tpl.LastUsedAt, now)); err != nil {
			return ConfigImportSummary{}, err
		}
	}

	for _, strategy := range cfg.Strategies {
		for _, originalTemplateID := range strategy.TemplateIDs {
			if _, ok := templateMap[originalTemplateID]; ok {
				continue
			}
			if _, ok := templatePayload[originalTemplateID]; ok {
				return ConfigImportSummary{}, fmt.Errorf("%w: strategy %q references skipped template %q", ErrConfigImportConflict, strategy.ID, originalTemplateID)
			}
			if _, ok := existingTemplates[originalTemplateID]; ok {
				templateMap[originalTemplateID] = originalTemplateID
				continue
			}
			return ConfigImportSummary{}, fmt.Errorf("%w: strategy %q references missing template %q", ErrConfigImportInvalid, strategy.ID, originalTemplateID)
		}
	}

	usedStrategyIDs := copyStringSet(existingStrategies)
	for _, strategy := range cfg.Strategies {
		targetID := strategy.ID
		_, exists := existingStrategies[strategy.ID]
		switch mode {
		case ConfigImportSkip:
			if exists {
				summary.Strategies.Skipped++
				continue
			}
			summary.Strategies.Created++
		case ConfigImportOverwrite:
			if exists {
				summary.Strategies.Updated++
			} else {
				summary.Strategies.Created++
			}
		case ConfigImportRename:
			if exists {
				targetID = uniqueImportID("str", strategy.ID, usedStrategyIDs)
				summary.Strategies.Renamed++
				summary.Strategies.Created++
			} else {
				summary.Strategies.Created++
			}
		case ConfigImportDuplicate:
			targetID = uniqueImportID("str", strategy.ID, usedStrategyIDs)
			summary.Strategies.Duplicated++
			summary.Strategies.Created++
		}
		usedStrategyIDs[targetID] = struct{}{}
		templateIDs := make([]string, 0, len(strategy.TemplateIDs))
		for _, originalTemplateID := range strategy.TemplateIDs {
			templateIDs = append(templateIDs, templateMap[originalTemplateID])
		}
		name := importName(strategy.Name, targetID, targetID != strategy.ID, mode)
		if err := upsertStrategyTx(ctx, tx, targetID, name, strategy.Notes, templateIDs, importTime(strategy.CreatedAt, now), importTime(strategy.UpdatedAt, now), strategy.LastUsedAt); err != nil {
			return ConfigImportSummary{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return ConfigImportSummary{}, err
	}
	return summary, nil
}

func validConfigImportMode(mode ConfigImportMode) bool {
	switch mode {
	case ConfigImportRename, ConfigImportSkip, ConfigImportOverwrite, ConfigImportDuplicate:
		return true
	default:
		return false
	}
}

func validateReusableConfig(cfg ReusableConfigExport) error {
	if cfg.Version != ConfigExportVersion {
		return fmt.Errorf("%w: unsupported version %d", ErrConfigImportInvalid, cfg.Version)
	}
	templateIDs := make(map[string]struct{}, len(cfg.Templates))
	for _, tpl := range cfg.Templates {
		id := strings.TrimSpace(tpl.ID)
		if id == "" {
			return fmt.Errorf("%w: template id is required", ErrConfigImportInvalid)
		}
		if _, ok := templateIDs[id]; ok {
			return fmt.Errorf("%w: duplicate template id %q", ErrConfigImportInvalid, id)
		}
		templateIDs[id] = struct{}{}
		if strings.TrimSpace(tpl.Name) == "" {
			return fmt.Errorf("%w: template %q name is required", ErrConfigImportInvalid, id)
		}
		if !json.Valid([]byte(strings.TrimSpace(tpl.ParamsJSON))) {
			return fmt.Errorf("%w: template %q params_json must be valid JSON", ErrConfigImportInvalid, id)
		}
	}
	strategyIDs := make(map[string]struct{}, len(cfg.Strategies))
	for _, strategy := range cfg.Strategies {
		id := strings.TrimSpace(strategy.ID)
		if id == "" {
			return fmt.Errorf("%w: strategy id is required", ErrConfigImportInvalid)
		}
		if _, ok := strategyIDs[id]; ok {
			return fmt.Errorf("%w: duplicate strategy id %q", ErrConfigImportInvalid, id)
		}
		strategyIDs[id] = struct{}{}
		if strings.TrimSpace(strategy.Name) == "" {
			return fmt.Errorf("%w: strategy %q name is required", ErrConfigImportInvalid, id)
		}
	}
	return nil
}

func txStringSet(ctx context.Context, tx *sql.Tx, query string) (map[string]struct{}, error) {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = struct{}{}
	}
	return out, rows.Err()
}

func copyStringSet(in map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for k := range in {
		out[k] = struct{}{}
	}
	return out
}

func uniqueImportID(prefix, original string, used map[string]struct{}) string {
	base := strings.TrimSpace(original)
	for i := 1; ; i++ {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%d", prefix, base, i)))
		id := fmt.Sprintf("%s_%x", prefix, sum[:8])
		if _, ok := used[id]; !ok {
			return id
		}
	}
}

func importName(name, targetID string, changed bool, mode ConfigImportMode) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = targetID
	}
	if !changed {
		return name
	}
	suffix := " imported"
	if mode == ConfigImportDuplicate {
		suffix = " copy"
	}
	if strings.HasSuffix(name, suffix) {
		return name
	}
	return strings.TrimSpace(name + suffix)
}

func importTime(value, fallback time.Time) time.Time {
	if value.IsZero() {
		return fallback
	}
	return value.UTC()
}

func upsertTemplateTx(ctx context.Context, tx *sql.Tx, id, name, paramsJSON string, createdAt, lastUsedAt time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO job_templates (id, name, params_json, created_at, last_used_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			params_json = excluded.params_json,
			last_used_at = excluded.last_used_at`,
		id, name, paramsJSON, createdAt, lastUsedAt)
	return err
}

func upsertStrategyTx(ctx context.Context, tx *sql.Tx, id, name, notes string, templateIDs []string, createdAt, updatedAt, lastUsedAt time.Time) error {
	var lastUsed any
	if !lastUsedAt.IsZero() {
		lastUsed = lastUsedAt.UTC()
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO strategies (id, name, notes, created_at, updated_at, last_used_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			notes = excluded.notes,
			updated_at = excluded.updated_at,
			last_used_at = excluded.last_used_at`,
		id, name, strings.TrimSpace(notes), createdAt, updatedAt, lastUsed); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM strategy_templates WHERE strategy_id = ?`, id); err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(templateIDs))
	for position, templateID := range templateIDs {
		templateID = strings.TrimSpace(templateID)
		if templateID == "" {
			continue
		}
		if _, ok := seen[templateID]; ok {
			continue
		}
		seen[templateID] = struct{}{}
		if _, err := tx.ExecContext(ctx, `INSERT INTO strategy_templates (strategy_id, template_id, position, created_at)
			VALUES (?, ?, ?, ?)`, id, templateID, position, updatedAt); err != nil {
			return err
		}
	}
	return nil
}

// DeleteStrategy removes a strategy. When deleteTemplates is true it also
// deletes the strategy's job templates that become unreferenced once this
// strategy's join rows are gone; templates still shared with another strategy
// are kept (only their join row to this strategy is removed via the
// strategy_templates ON DELETE CASCADE foreign key).
