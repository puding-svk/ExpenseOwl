package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

const (
	createImportRulesTableSQL = `
	CREATE TABLE IF NOT EXISTS import_rules (
		id VARCHAR(36) PRIMARY KEY,
		position INTEGER NOT NULL,
		enabled BOOLEAN NOT NULL DEFAULT TRUE,
		name VARCHAR(255) NOT NULL,
		category VARCHAR(255) NOT NULL,
		income BOOLEAN NOT NULL DEFAULT FALSE,
		subject_contains TEXT NOT NULL,
		row_conditions TEXT NOT NULL,
		amount_prefix TEXT NOT NULL,
		amount_postfix TEXT NOT NULL
	);`

	createImportBalancesTableSQL = `
	CREATE TABLE IF NOT EXISTS import_balances (
		account VARCHAR(16) PRIMARY KEY,
		balance NUMERIC(12, 2) NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL
	);`

	createImportStateTableSQL = `
	CREATE TABLE IF NOT EXISTS import_state (
		key VARCHAR(64) PRIMARY KEY,
		value TEXT NOT NULL
	);`

	createImportLogTableSQL = `
	CREATE TABLE IF NOT EXISTS import_log (
		id BIGSERIAL PRIMARY KEY,
		uid_validity BIGINT NOT NULL,
		uid BIGINT NOT NULL,
		received_at TIMESTAMPTZ NOT NULL,
		subject TEXT NOT NULL,
		body TEXT NOT NULL,
		kind VARCHAR(16) NOT NULL,
		account VARCHAR(16) NOT NULL,
		amount NUMERIC(12, 2) NOT NULL,
		description TEXT NOT NULL,
		status VARCHAR(16) NOT NULL,
		rule_id VARCHAR(36) NOT NULL,
		rule_name VARCHAR(255) NOT NULL,
		expense_id VARCHAR(36) NOT NULL,
		note TEXT NOT NULL,
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		UNIQUE (uid_validity, uid)
	);`

	importRuleColumns = `id, position, enabled, name, category, income, subject_contains, row_conditions, amount_prefix, amount_postfix`
	importLogColumns  = `id, uid_validity, uid, received_at, subject, body, kind, account, amount, description, status, rule_id, rule_name, expense_id, note, created_at`
)

var autoImportTables = []string{createImportRulesTableSQL, createImportBalancesTableSQL, createImportStateTableSQL, createImportLogTableSQL}

// ------------------------------------------------------------
// Rules
// ------------------------------------------------------------

func scanImportRule(scanner interface{ Scan(...any) error }) (ImportRule, error) {
	var r ImportRule
	var subj, rows string
	err := scanner.Scan(&r.ID, &r.Position, &r.Enabled, &r.Name, &r.Category, &r.Income, &subj, &rows, &r.AmountPrefix, &r.AmountPostfix)
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal([]byte(subj), &r.SubjectContains); err != nil {
		return r, fmt.Errorf("failed to parse subject conditions: %v", err)
	}
	if err := json.Unmarshal([]byte(rows), &r.RowConditions); err != nil {
		return r, fmt.Errorf("failed to parse row conditions: %v", err)
	}
	return r, nil
}

func (s *databaseStore) GetImportRules() ([]ImportRule, error) {
	rows, err := s.db.Query(`SELECT ` + importRuleColumns + ` FROM import_rules ORDER BY position, name`)
	if err != nil {
		return nil, fmt.Errorf("failed to query import rules: %v", err)
	}
	defer rows.Close()
	rules := []ImportRule{}
	for rows.Next() {
		r, err := scanImportRule(rows)
		if err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

func (s *databaseStore) GetImportRule(id string) (ImportRule, error) {
	r, err := scanImportRule(s.db.QueryRow(`SELECT `+importRuleColumns+` FROM import_rules WHERE id = $1`, id))
	if err == sql.ErrNoRows {
		return r, fmt.Errorf("import rule with ID %s not found", id)
	}
	return r, err
}

func (s *databaseStore) AddImportRule(rule ImportRule) (ImportRule, error) {
	if rule.ID == "" {
		rule.ID = uuid.New().String()
	}
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(position), 0) + 1 FROM import_rules`).Scan(&rule.Position); err != nil {
		return rule, err
	}
	subj, rows, err := marshalRuleConditions(rule)
	if err != nil {
		return rule, err
	}
	_, err = s.db.Exec(`INSERT INTO import_rules (`+importRuleColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		rule.ID, rule.Position, rule.Enabled, rule.Name, rule.Category, rule.Income, subj, rows, rule.AmountPrefix, rule.AmountPostfix)
	return rule, err
}

func (s *databaseStore) UpdateImportRule(id string, rule ImportRule) error {
	subj, rows, err := marshalRuleConditions(rule)
	if err != nil {
		return err
	}
	result, err := s.db.Exec(`
		UPDATE import_rules SET enabled = $1, name = $2, category = $3, income = $4, subject_contains = $5,
			row_conditions = $6, amount_prefix = $7, amount_postfix = $8
		WHERE id = $9`,
		rule.Enabled, rule.Name, rule.Category, rule.Income, subj, rows, rule.AmountPrefix, rule.AmountPostfix, id)
	if err != nil {
		return err
	}
	return expectOneRow(result, "import rule", id)
}

func (s *databaseStore) RemoveImportRule(id string) error {
	result, err := s.db.Exec(`DELETE FROM import_rules WHERE id = $1`, id)
	if err != nil {
		return err
	}
	return expectOneRow(result, "import rule", id)
}

// ReorderImportRules sets the position of each listed rule to its index.
func (s *databaseStore) ReorderImportRules(ids []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, id := range ids {
		if _, err := tx.Exec(`UPDATE import_rules SET position = $1 WHERE id = $2`, i+1, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func marshalRuleConditions(rule ImportRule) (string, string, error) {
	subj, err := json.Marshal(rule.SubjectContains)
	if err != nil {
		return "", "", err
	}
	rows, err := json.Marshal(rule.RowConditions)
	if err != nil {
		return "", "", err
	}
	return string(subj), string(rows), nil
}

func expectOneRow(result sql.Result, what, id string) error {
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%s with ID %s not found", what, id)
	}
	return nil
}

// ------------------------------------------------------------
// Settings, balances and cursor
// ------------------------------------------------------------

func (s *databaseStore) getImportState(key string, v any) (bool, error) {
	var value string
	err := s.db.QueryRow(`SELECT value FROM import_state WHERE key = $1`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal([]byte(value), v)
}

func (s *databaseStore) setImportState(key string, v any) error {
	value, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
		INSERT INTO import_state (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, key, string(value))
	return err
}

func (s *databaseStore) GetImportSettings() (ImportSettings, error) {
	settings := DefaultImportSettings()
	_, err := s.getImportState("settings", &settings)
	return settings, err
}

func (s *databaseStore) UpdateImportSettings(settings ImportSettings) error {
	return s.setImportState("settings", settings)
}

func (s *databaseStore) GetImportCursor() (ImportCursor, error) {
	var cursor ImportCursor
	_, err := s.getImportState("cursor", &cursor)
	return cursor, err
}

func (s *databaseStore) SetImportCursor(cursor ImportCursor) error {
	return s.setImportState("cursor", cursor)
}

func (s *databaseStore) GetImportBalances() ([]ImportBalance, error) {
	rows, err := s.db.Query(`SELECT account, balance, updated_at FROM import_balances ORDER BY account`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	balances := []ImportBalance{}
	for rows.Next() {
		var b ImportBalance
		if err := rows.Scan(&b.Account, &b.Balance, &b.UpdatedAt); err != nil {
			return nil, err
		}
		balances = append(balances, b)
	}
	return balances, rows.Err()
}

func (s *databaseStore) SetImportBalance(b ImportBalance) error {
	_, err := s.db.Exec(`
		INSERT INTO import_balances (account, balance, updated_at) VALUES ($1, $2, $3)
		ON CONFLICT (account) DO UPDATE SET balance = EXCLUDED.balance, updated_at = EXCLUDED.updated_at`,
		b.Account, b.Balance, b.UpdatedAt)
	return err
}

// ------------------------------------------------------------
// Log
// ------------------------------------------------------------

func scanImportLogEntry(scanner interface{ Scan(...any) error }) (ImportLogEntry, error) {
	var e ImportLogEntry
	var uidValidity, uid int64
	err := scanner.Scan(&e.ID, &uidValidity, &uid, &e.ReceivedAt, &e.Subject, &e.Body, &e.Kind, &e.Account, &e.Amount,
		&e.Description, &e.Status, &e.RuleID, &e.RuleName, &e.ExpenseID, &e.Note, &e.CreatedAt)
	e.UIDValidity, e.UID = uint32(uidValidity), uint32(uid)
	return e, err
}

// AddImportLogEntry inserts the entry and sets its ID. It returns false when
// the message (same UID validity and UID) was already logged.
func (s *databaseStore) AddImportLogEntry(e *ImportLogEntry) (bool, error) {
	err := s.db.QueryRow(`
		INSERT INTO import_log (uid_validity, uid, received_at, subject, body, kind, account, amount, description, status, rule_id, rule_name, expense_id, note)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		ON CONFLICT (uid_validity, uid) DO NOTHING
		RETURNING id, created_at`,
		int64(e.UIDValidity), int64(e.UID), e.ReceivedAt, e.Subject, e.Body, e.Kind, e.Account, e.Amount, e.Description,
		e.Status, e.RuleID, e.RuleName, e.ExpenseID, e.Note).Scan(&e.ID, &e.CreatedAt)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func (s *databaseStore) UpdateImportLogEntry(e ImportLogEntry) error {
	result, err := s.db.Exec(`
		UPDATE import_log SET status = $1, rule_id = $2, rule_name = $3, expense_id = $4, note = $5, amount = $6
		WHERE id = $7`, e.Status, e.RuleID, e.RuleName, e.ExpenseID, e.Note, e.Amount, e.ID)
	if err != nil {
		return err
	}
	return expectOneRow(result, "import log entry", fmt.Sprint(e.ID))
}

func (s *databaseStore) GetImportLogEntries(filter ImportLogFilter) ([]ImportLogEntry, error) {
	if filter.Limit <= 0 {
		filter.Limit = 200
	}
	rows, err := s.db.Query(`
		SELECT `+importLogColumns+` FROM import_log
		WHERE ($1 = '' OR status = $1) AND received_at >= $2
		ORDER BY received_at DESC, uid DESC LIMIT $3`, filter.Status, filter.Since, filter.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []ImportLogEntry{}
	for rows.Next() {
		e, err := scanImportLogEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

func (s *databaseStore) GetImportLogEntry(id int64) (ImportLogEntry, error) {
	e, err := scanImportLogEntry(s.db.QueryRow(`SELECT `+importLogColumns+` FROM import_log WHERE id = $1`, id))
	if err == sql.ErrNoRows {
		return e, fmt.Errorf("import log entry %d not found", id)
	}
	return e, err
}

func (s *databaseStore) PruneImportLog(before time.Time) (int64, error) {
	result, err := s.db.Exec(`DELETE FROM import_log WHERE received_at < $1`, before)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
