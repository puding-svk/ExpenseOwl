// Package tbimport imports Tatra banka e-mail notifications into ExpenseOwl.
// It is a port of the former tb-balance-checker script.
package tbimport

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/tanq16/expenseowl/internal/storage"
)

const (
	AccountCurrent = "bu" // current account ("na ucte")
	AccountCard    = "kk" // credit card ("na kreditnej karte")
)

// Message is a decoded notification e-mail.
type Message struct {
	Subject string
	Body    string
}

// Rows returns the non-empty body lines, with line breaks normalized but
// otherwise untouched.
func (m Message) Rows() []string {
	var rows []string
	for _, row := range strings.Split(strings.ReplaceAll(strings.TrimSpace(m.Body), "\r", ""), "\n") {
		if row != "" {
			rows = append(rows, row)
		}
	}
	return rows
}

var balanceSubjects = []string{"debet na ", "kredit na ", "uhrada dlznej sumy na ", "stav na "}

// ParseBalance returns the account and its available balance ("disponibilny
// zostatok") when the message is a balance-carrying notification.
func ParseBalance(m Message) (string, float64, bool) {
	subject := strings.ToLower(m.Subject)
	if !containsAny(subject, balanceSubjects) {
		return "", 0, false
	}
	account := ""
	switch {
	case strings.Contains(subject, "na ucte"):
		account = AccountCurrent
	case strings.Contains(subject, "na kreditnej karte"):
		account = AccountCard
	default:
		return "", 0, false
	}
	const label = "disponibilnyzostatok:"
	for _, row := range m.Rows() {
		compact := strings.ToLower(strings.ReplaceAll(row, " ", ""))
		if idx := strings.Index(compact, label); idx != -1 {
			value := strings.TrimSuffix(strings.TrimSpace(compact[idx+len(label):]), "eur")
			if balance, ok := parseAmount(value); ok {
				return account, balance, true
			}
			return "", 0, false
		}
	}
	return "", 0, false
}

// Matches reports whether all of the rule's conditions hold for the message.
func Matches(rule storage.ImportRule, m Message) bool {
	subject := strings.ToLower(m.Subject)
	for _, term := range rule.SubjectContains {
		if !strings.Contains(subject, strings.ToLower(term)) {
			return false
		}
	}
	rows := m.Rows()
	for _, group := range rule.RowConditions {
		if !anyRowContainsAll(rows, group) {
			return false
		}
	}
	return true
}

// ExtractAmount returns the first positive amount found between the rule's
// prefix and postfix.
func ExtractAmount(rule storage.ImportRule, m Message) (float64, bool) {
	for _, row := range m.Rows() {
		if value, ok := between(row, rule.AmountPrefix, rule.AmountPostfix); ok {
			if amount, ok := parseAmount(value); ok && amount > 0 {
				return amount, true
			}
		}
	}
	return 0, false
}

// Match is the outcome of applying the rules to one message.
type Match struct {
	Rule   storage.ImportRule
	Amount float64
}

// FirstMatch returns the first enabled rule (in order) that matches and yields
// an amount.
func FirstMatch(rules []storage.ImportRule, m Message) (Match, bool) {
	for _, rule := range rules {
		if !rule.Enabled || !Matches(rule, m) {
			continue
		}
		if amount, ok := ExtractAmount(rule, m); ok {
			return Match{Rule: rule, Amount: amount}, true
		}
	}
	return Match{}, false
}

// Kind classifies the notification by its subject.
func Kind(m Message) string {
	subject := strings.ToLower(m.Subject)
	switch {
	case strings.HasPrefix(subject, "debet"):
		return "debit"
	case strings.HasPrefix(subject, "kredit"), strings.HasPrefix(subject, "uhrada"):
		return "credit"
	case strings.HasPrefix(subject, "stav"):
		return "statement"
	}
	return "other"
}

var reTransactionAmount = regexp.MustCompile(`(?i)(?:znizeny o|zvyseny o|vo vyske)\s+([\d ]+[.,]\d{2})\s*EUR`)

// Describe returns the transaction amount and description for display. Both
// are best-effort and do not depend on any rule.
func Describe(m Message) (float64, string) {
	var amount float64
	var description string
	for _, row := range m.Rows() {
		if amount == 0 {
			if sub := reTransactionAmount.FindStringSubmatch(row); sub != nil {
				amount, _ = parseAmount(sub[1])
			}
		}
		if description == "" {
			if value, ok := strings.CutPrefix(strings.TrimSpace(row), "Popis transakcie:"); ok {
				description = strings.TrimSpace(value)
			}
		}
	}
	return amount, description
}

// IsTransaction reports whether the message reports money moving in or out.
func IsTransaction(m Message) bool {
	kind := Kind(m)
	return kind == "debit" || kind == "credit"
}

func containsAny(s string, terms []string) bool {
	for _, t := range terms {
		if strings.Contains(s, t) {
			return true
		}
	}
	return false
}

func anyRowContainsAll(rows []string, terms []string) bool {
	for _, row := range rows {
		lower := strings.ToLower(row)
		all := true
		for _, term := range terms {
			if !strings.Contains(lower, strings.ToLower(term)) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// between returns the text between prefix and the next postfix, case-insensitively.
func between(row, prefix, postfix string) (string, bool) {
	lower := strings.ToLower(row)
	start := strings.Index(lower, strings.ToLower(prefix))
	if start == -1 {
		return "", false
	}
	start += len(prefix)
	end := strings.Index(lower[start:], strings.ToLower(postfix))
	if end <= 0 {
		return "", false
	}
	// amounts are ASCII, so the lowered text is as good as the original
	return lower[start : start+end], true
}

// parseAmount accepts "1 628,78", "3905.83" and similar.
func parseAmount(s string) (float64, bool) {
	s = strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(s), " ", ""), " ", "")
	s = strings.ReplaceAll(s, ",", ".")
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return float64(int64(v*100+sign(v)*0.5)) / 100, true
}

func sign(v float64) float64 {
	if v < 0 {
		return -1
	}
	return 1
}
