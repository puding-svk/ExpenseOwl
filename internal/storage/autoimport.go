package storage

import (
	"fmt"
	"net/mail"
	"strings"
	"time"
)

// AutoImportStore holds the state of the bank e-mail auto-import feature.
// Only the PostgreSQL backend implements it.
type AutoImportStore interface {
	// Rules
	GetImportRules() ([]ImportRule, error)
	GetImportRule(id string) (ImportRule, error)
	AddImportRule(rule ImportRule) (ImportRule, error)
	UpdateImportRule(id string, rule ImportRule) error
	RemoveImportRule(id string) error
	ReorderImportRules(ids []string) error

	// Settings, balances and mailbox cursor
	GetImportSettings() (ImportSettings, error)
	UpdateImportSettings(settings ImportSettings) error
	GetImportBalances() ([]ImportBalance, error)
	SetImportBalance(balance ImportBalance) error
	GetImportCursor() (ImportCursor, error)
	SetImportCursor(cursor ImportCursor) error

	// Log of processed messages
	AddImportLogEntry(entry *ImportLogEntry) (bool, error)
	UpdateImportLogEntry(entry ImportLogEntry) error
	GetImportLogEntries(filter ImportLogFilter) ([]ImportLogEntry, error)
	GetImportLogEntry(id int64) (ImportLogEntry, error)
	PruneImportLog(before time.Time) (int64, error)
}

// ImportRule turns a matching bank notification into an expense (or income).
// A rule matches when the subject contains every SubjectContains term and, for
// each group in RowConditions, a single body row contains every term of that
// group. All comparisons are case-insensitive. Terms, prefix and postfix are
// kept verbatim, because leading/trailing spaces are significant.
type ImportRule struct {
	ID              string     `json:"id"`
	Position        int        `json:"position"`
	Enabled         bool       `json:"enabled"`
	Name            string     `json:"name"`
	Category        string     `json:"category"`
	Income          bool       `json:"income"`
	SubjectContains []string   `json:"subjectContains"`
	RowConditions   [][]string `json:"rowConditions"`
	AmountPrefix    string     `json:"amountPrefix"`
	AmountPostfix   string     `json:"amountPostfix"`
}

type ImportSettings struct {
	NotifyEnabled  bool     `json:"notifyEnabled"`
	NotifyTo       []string `json:"notifyTo"`       // nil until set; seeded from TB_NOTIFY_TO
	CardLimit      float64  `json:"cardLimit"`      // subtracted from the credit card available balance
	OverdraftLimit float64  `json:"overdraftLimit"` // subtracted from the current account available balance
}

type ImportBalance struct {
	Account   string    `json:"account"` // "bu" current account, "kk" credit card
	Balance   float64   `json:"balance"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ImportCursor remembers the last processed IMAP UID of the mailbox.
type ImportCursor struct {
	UIDValidity uint32 `json:"uidValidity"`
	LastUID     uint32 `json:"lastUID"`
}

const (
	ImportStatusImported  = "imported"  // expense created by a rule
	ImportStatusManual    = "manual"    // expense added by hand from this e-mail
	ImportStatusUnmatched = "unmatched" // transaction, no rule matched
	ImportStatusBalance   = "balance"   // balance-only notification
	ImportStatusIgnored   = "ignored"   // not a transaction notification
	ImportStatusHistory   = "history"   // seen during backfill, never imported automatically
	ImportStatusPending   = "pending"   // processing started but not finished
	ImportStatusError     = "error"
)

type ImportLogEntry struct {
	ID          int64     `json:"id"`
	UIDValidity uint32    `json:"uidValidity"`
	UID         uint32    `json:"uid"`
	ReceivedAt  time.Time `json:"receivedAt"`
	Subject     string    `json:"subject"`
	Body        string    `json:"body"`
	Kind        string    `json:"kind"` // debit, credit, statement, other
	Account     string    `json:"account"`
	Amount      float64   `json:"amount"`
	Description string    `json:"description"`
	Status      string    `json:"status"`
	RuleID      string    `json:"ruleID"`
	RuleName    string    `json:"ruleName"`
	ExpenseID   string    `json:"expenseID"`
	Note        string    `json:"note"`
	CreatedAt   time.Time `json:"createdAt"`
}

type ImportLogFilter struct {
	Status string    // empty for all
	Since  time.Time // zero for no lower bound
	Limit  int       // 0 for default
}

// notifications stay off until the limits have been reviewed and saved
func DefaultImportSettings() ImportSettings {
	return ImportSettings{NotifyEnabled: false}
}

func (r *ImportRule) Validate() error {
	r.Name = SanitizeString(r.Name)
	if r.Name == "" {
		return fmt.Errorf("rule 'name' cannot be empty")
	}
	if r.Category == "" {
		return fmt.Errorf("rule 'category' cannot be empty")
	}
	r.SubjectContains = cleanTerms(r.SubjectContains)
	var groups [][]string
	for _, g := range r.RowConditions {
		if g = cleanTerms(g); len(g) > 0 {
			groups = append(groups, g)
		}
	}
	r.RowConditions = groups
	if len(r.SubjectContains) == 0 && len(r.RowConditions) == 0 {
		return fmt.Errorf("rule needs at least one subject or row condition")
	}
	if strings.TrimSpace(r.AmountPrefix) == "" || strings.TrimSpace(r.AmountPostfix) == "" {
		return fmt.Errorf("rule 'amountPrefix' and 'amountPostfix' cannot be empty")
	}
	return nil
}

// drops empty terms but keeps significant whitespace of the others
func cleanTerms(terms []string) []string {
	cleaned := []string{}
	for _, t := range terms {
		if strings.TrimSpace(t) != "" {
			cleaned = append(cleaned, t)
		}
	}
	return cleaned
}

const maxNotifyRecipients = 10

func (s *ImportSettings) Validate() error {
	if s.CardLimit < 0 || s.OverdraftLimit < 0 {
		return fmt.Errorf("limits cannot be negative")
	}
	recipients := []string{} // never nil once saved, so the seed is not applied again
	seen := map[string]bool{}
	for _, r := range s.NotifyTo {
		if strings.TrimSpace(r) == "" {
			continue
		}
		addr, err := mail.ParseAddress(strings.TrimSpace(r))
		if err != nil {
			return fmt.Errorf("invalid e-mail address %q", strings.TrimSpace(r))
		}
		if key := strings.ToLower(addr.Address); !seen[key] {
			seen[key] = true
			recipients = append(recipients, addr.Address)
		}
	}
	if len(recipients) > maxNotifyRecipients {
		return fmt.Errorf("at most %d recipients", maxNotifyRecipients)
	}
	if s.NotifyEnabled && len(recipients) == 0 {
		return fmt.Errorf("add at least one recipient, or switch the balance e-mail off")
	}
	s.NotifyTo = recipients
	return nil
}
