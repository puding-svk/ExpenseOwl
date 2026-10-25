package tbimport

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tanq16/expenseowl/internal/storage"
)

var reTrailingNumbers = regexp.MustCompile(`(\s+\d+)+$`)

// MerchantGuess returns the shop from a "Popis transakcie" description:
// "Platba kartou 4405**0000, LIDL SLOVENSKA REPUBLIKA." gives "LIDL SLOVENSKA
// REPUBLIKA" and "BILLA 292, SENEC SK, dom nakup" gives "BILLA". Shop numbers
// are dropped so a rule built from it matches every branch.
func MerchantGuess(description string) string {
	d := strings.TrimSuffix(strings.TrimSpace(description), ".")
	var name string
	if strings.HasPrefix(strings.ToLower(d), "platba kartou") {
		if _, rest, ok := strings.Cut(d, ","); ok {
			name = rest
		}
	} else {
		name, _, _ = strings.Cut(d, ",")
	}
	return reTrailingNumbers.ReplaceAllString(strings.TrimSpace(name), "")
}

// ExpenseName turns a merchant into a short expense name: the first two
// words, capitalized ("LIDL SLOVENSKA REPUBLIKA" gives "Lidl slovenska").
func ExpenseName(merchant string) string {
	words := strings.Fields(merchant)
	if len(words) == 0 {
		return ""
	}
	name := strings.ToLower(strings.Join(words[:min(2, len(words))], " "))
	return strings.ToUpper(name[:1]) + name[1:]
}

// Draft is the prefill for adding an expense from a logged e-mail.
type Draft struct {
	EntryID     int64     `json:"entryID"`
	Status      string    `json:"status"`
	Subject     string    `json:"subject"`
	Description string    `json:"description"`
	Amount      float64   `json:"amount"`
	Income      bool      `json:"income"`
	Merchant    string    `json:"merchant"`
	Name        string    `json:"name"`
	Category    string    `json:"category"` // empty when there is no earlier expense with this name
	Date        time.Time `json:"date"`
	Addable     bool      `json:"addable"`
	Warning     string    `json:"warning"` // e.g. for history e-mails
	Reason      string    `json:"reason"`  // why it cannot be added
}

var errNotAddable = errors.New("not addable")

// addable reports whether an expense may be added from the entry.
func addable(store storage.Storage, e storage.ImportLogEntry) (ok bool, warning, reason string, err error) {
	if e.Kind != "debit" && e.Kind != "credit" {
		return false, "", "this e-mail is not a transaction", nil
	}
	switch e.Status {
	case storage.ImportStatusUnmatched, storage.ImportStatusError:
		return true, "", "", nil
	case storage.ImportStatusHistory:
		return true, "This e-mail arrived before auto-import started, so the old checker may already have added it.", "", nil
	case storage.ImportStatusImported, storage.ImportStatusManual:
		if e.ExpenseID == "" {
			return false, "", "this e-mail was already added", nil
		}
		if _, err := store.GetExpense(e.ExpenseID); err == nil {
			return false, "", "this e-mail was already added as an expense", nil
		} else if !errors.Is(err, storage.ErrNotFound) {
			return false, "", "", err
		}
		return true, "The expense added from this e-mail was deleted; adding it again is allowed.", "", nil
	}
	return false, "", "this e-mail is still being processed", nil
}

// BuildDraft returns the prefill for the entry.
func BuildDraft(store storage.Storage, ai storage.AutoImportStore, id int64) (Draft, error) {
	e, err := ai.GetImportLogEntry(id)
	if err != nil {
		return Draft{}, err
	}
	merchant := MerchantGuess(e.Description)
	d := Draft{
		EntryID:     e.ID,
		Status:      e.Status,
		Subject:     e.Subject,
		Description: e.Description,
		Amount:      e.Amount,
		Income:      e.Kind == "credit",
		Merchant:    merchant,
		Name:        ExpenseName(merchant),
		Date:        e.ReceivedAt,
	}
	if d.Addable, d.Warning, d.Reason, err = addable(store, e); err != nil {
		return d, err
	}
	if d.Name != "" {
		if d.Category, err = lastCategory(store, d.Name); err != nil {
			return d, err
		}
	}
	return d, nil
}

// lastCategory returns the category of the most recent expense with this
// name (case-insensitive), or "" if there is none.
func lastCategory(store storage.Storage, name string) (string, error) {
	expenses, err := store.GetAllExpenses()
	if err != nil {
		return "", err
	}
	var latest *storage.Expense
	for i := range expenses {
		if strings.EqualFold(strings.TrimSpace(expenses[i].Name), name) &&
			(latest == nil || expenses[i].Date.After(latest.Date)) {
			latest = &expenses[i]
		}
	}
	if latest == nil {
		return "", nil
	}
	return latest.Category, nil
}

// AddManualExpense saves the expense and links it to the entry in one step,
// so the e-mail cannot be added twice (double submit, second tab, or "apply
// rules" later).
func AddManualExpense(store storage.Storage, ai storage.AutoImportStore, id int64, expense storage.Expense) (storage.ImportLogEntry, error) {
	processMu.Lock()
	defer processMu.Unlock()
	e, err := ai.GetImportLogEntry(id)
	if err != nil {
		return e, err
	}
	ok, _, reason, err := addable(store, e)
	if err != nil {
		return e, err
	}
	if !ok {
		return e, fmt.Errorf("%w: %s", errNotAddable, reason)
	}
	expense.ID = uuid.New().String()
	expense.RecurringID = ""
	if expense.Date.IsZero() {
		expense.Date = e.ReceivedAt
	}
	if err := expense.Validate(); err != nil {
		return e, fmt.Errorf("%w: %v", errNotAddable, err)
	}
	if err := store.AddExpense(expense); err != nil {
		return e, fmt.Errorf("add expense: %v", err)
	}
	e.Status, e.ExpenseID, e.RuleID, e.RuleName, e.Note = storage.ImportStatusManual, expense.ID, "", "", "added manually"
	if err := ai.UpdateImportLogEntry(e); err != nil {
		return e, fmt.Errorf("expense %s added, but linking it to the e-mail failed: %v", expense.ID, err)
	}
	return e, nil
}

// IsNotAddable reports whether err means the request itself was refused.
func IsNotAddable(err error) bool {
	return errors.Is(err, errNotAddable)
}
