package tbimport

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/smtp"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/google/uuid"
	"github.com/tanq16/expenseowl/internal/storage"
)

// Config of the mailbox poller, read from TB_* environment variables.
type Config struct {
	IMAPAddr      string
	User          string
	Pass          string
	Mailbox       string
	Interval      time.Duration
	MarkSeen      bool
	SMTPAddr      string
	NotifyFrom    string
	NotifyTo      []string // seeds the recipients in the settings on first start
	BackfillDays  int
	RetentionDays int
}

// ConfigFromEnv returns the poller config, or false when TB_IMPORT_ENABLED is not "true".
func ConfigFromEnv() (Config, bool, error) {
	if os.Getenv("TB_IMPORT_ENABLED") != "true" {
		return Config{}, false, nil
	}
	cfg := Config{
		IMAPAddr:      envOr("TB_IMAP_ADDR", "imap.gmail.com:993"),
		User:          os.Getenv("TB_IMAP_USER"),
		Pass:          os.Getenv("TB_IMAP_PASS"),
		Mailbox:       envOr("TB_MAILBOX", "TB"),
		MarkSeen:      envOr("TB_MARK_SEEN", "true") == "true",
		SMTPAddr:      envOr("TB_SMTP_ADDR", "smtp.gmail.com:587"),
		NotifyFrom:    envOr("TB_NOTIFY_FROM", os.Getenv("TB_IMAP_USER")),
		NotifyTo:      splitAddresses(os.Getenv("TB_NOTIFY_TO")),
		BackfillDays:  30,
		RetentionDays: 180,
	}
	if cfg.User == "" || cfg.Pass == "" {
		return cfg, false, fmt.Errorf("TB_IMAP_USER and TB_IMAP_PASS are required when TB_IMPORT_ENABLED=true")
	}
	interval, err := time.ParseDuration(envOr("TB_POLL_INTERVAL", "60s"))
	if err != nil || interval < 10*time.Second {
		return cfg, false, fmt.Errorf("invalid TB_POLL_INTERVAL (minimum 10s)")
	}
	cfg.Interval = interval
	for name, dst := range map[string]*int{"TB_BACKFILL_DAYS": &cfg.BackfillDays, "TB_LOG_RETENTION_DAYS": &cfg.RetentionDays} {
		if v := os.Getenv(name); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return cfg, false, fmt.Errorf("invalid %s", name)
			}
			*dst = n
		}
	}
	return cfg, true, nil
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func splitAddresses(s string) []string {
	var out []string
	for _, a := range strings.FieldsFunc(s, func(r rune) bool { return r == ';' || r == ',' }) {
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, a)
		}
	}
	return out
}

// Status is the poller state shown in the UI.
type Status struct {
	Running     bool      `json:"running"`
	Mailbox     string    `json:"mailbox"`
	Interval    string    `json:"interval"`
	MarkSeen    bool      `json:"markSeen"`
	NotifyFrom  string    `json:"notifyFrom"`
	LastPoll    time.Time `json:"lastPoll"`
	LastSuccess time.Time `json:"lastSuccess"`
	LastError   string    `json:"lastError"`
	LastCount   int       `json:"lastCount"`
	LastNotify  time.Time `json:"lastNotify"`
}

// Importer polls the mailbox and turns notifications into expenses.
type Importer struct {
	cfg     Config
	store   storage.Storage
	ai      storage.AutoImportStore
	trigger chan struct{}

	statusMu   sync.Mutex
	status     Status
	lastPruned time.Time
}

// processMu serializes rule application between the poller and the API.
var processMu sync.Mutex

func NewImporter(cfg Config, store storage.Storage, ai storage.AutoImportStore) *Importer {
	return &Importer{
		cfg:     cfg,
		store:   store,
		ai:      ai,
		trigger: make(chan struct{}, 1),
		status: Status{
			Running: true, Mailbox: cfg.Mailbox, Interval: cfg.Interval.String(),
			MarkSeen: cfg.MarkSeen, NotifyFrom: cfg.NotifyFrom,
		},
	}
}

func (i *Importer) Status() Status {
	i.statusMu.Lock()
	defer i.statusMu.Unlock()
	return i.status
}

// PollNow requests an immediate poll.
func (i *Importer) PollNow() {
	select {
	case i.trigger <- struct{}{}:
	default:
	}
}

func (i *Importer) Run(ctx context.Context) {
	log.Printf("TB import: polling mailbox %q every %s", i.cfg.Mailbox, i.cfg.Interval)
	if err := seedRecipients(i.ai, i.cfg.NotifyTo); err != nil {
		log.Printf("TB import ERROR: failed to seed balance e-mail recipients: %v", err)
	}
	ticker := time.NewTicker(i.cfg.Interval)
	defer ticker.Stop()
	for {
		i.pollOnce()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-i.trigger:
		}
	}
}

func (i *Importer) pollOnce() {
	started := time.Now()
	count, err := i.poll()
	i.statusMu.Lock()
	i.status.LastPoll = started
	i.status.LastCount = count
	if err != nil {
		i.status.LastError = err.Error()
		log.Printf("TB import ERROR: %v", err)
	} else {
		i.status.LastError = ""
		i.status.LastSuccess = started
	}
	i.statusMu.Unlock()
	if time.Since(i.lastPruned) > 24*time.Hour && i.cfg.RetentionDays > 0 {
		if n, err := i.ai.PruneImportLog(time.Now().AddDate(0, 0, -i.cfg.RetentionDays)); err != nil {
			log.Printf("TB import ERROR: failed to prune log: %v", err)
		} else if n > 0 {
			log.Printf("TB import: pruned %d old log entries", n)
		}
		i.lastPruned = time.Now()
	}
}

type fetched struct {
	uid  uint32
	date time.Time
	msg  Message
}

// poll processes all messages that arrived since the stored cursor.
func (i *Importer) poll() (int, error) {
	c, err := client.DialWithDialerTLS(&net.Dialer{Timeout: 30 * time.Second}, i.cfg.IMAPAddr, nil)
	if err != nil {
		return 0, fmt.Errorf("connect %s: %v", i.cfg.IMAPAddr, err)
	}
	c.Timeout = 2 * time.Minute
	defer c.Logout()
	if err := c.Login(i.cfg.User, i.cfg.Pass); err != nil {
		return 0, fmt.Errorf("login: %v", err)
	}
	mbox, err := c.Select(i.cfg.Mailbox, !i.cfg.MarkSeen)
	if err != nil {
		return 0, fmt.Errorf("select %q: %v", i.cfg.Mailbox, err)
	}
	cursor, err := i.ai.GetImportCursor()
	if err != nil {
		return 0, fmt.Errorf("read cursor: %v", err)
	}
	if cursor.UIDValidity != mbox.UidValidity {
		return i.backfill(c, mbox)
	}

	from := new(imap.SeqSet)
	from.AddRange(cursor.LastUID+1, 0)
	uids, err := c.UidSearch(&imap.SearchCriteria{Uid: from})
	if err != nil {
		return 0, fmt.Errorf("search: %v", err)
	}
	// "n:*" always includes the newest message, even when it is older than n
	uids = slices.DeleteFunc(uids, func(u uint32) bool { return u <= cursor.LastUID })
	if len(uids) == 0 {
		return 0, nil
	}
	messages, err := fetchMessages(c, uids)
	if err != nil {
		return 0, err
	}

	processMu.Lock()
	defer processMu.Unlock()
	rules, err := i.ai.GetImportRules()
	if err != nil {
		return 0, fmt.Errorf("load rules: %v", err)
	}
	balanceSeen := false
	done := new(imap.SeqSet)
	for _, m := range messages {
		isBalance, err := i.process(rules, mbox.UidValidity, m, false)
		if err != nil {
			return len(done.Set), err
		}
		balanceSeen = balanceSeen || isBalance
		done.AddNum(m.uid)
		cursor.LastUID = m.uid
		if err := i.ai.SetImportCursor(cursor); err != nil {
			return len(done.Set), fmt.Errorf("save cursor: %v", err)
		}
	}
	if i.cfg.MarkSeen && !done.Empty() {
		if err := c.UidStore(done, imap.FormatFlagsOp(imap.AddFlags, true), []interface{}{imap.SeenFlag}, nil); err != nil {
			log.Printf("TB import: failed to mark messages as seen: %v", err)
		}
	}
	if balanceSeen {
		i.notify()
	}
	return len(messages), nil
}

// backfill logs the recent history of a new (or reset) mailbox without
// creating expenses, then starts tracking new messages from there.
func (i *Importer) backfill(c *client.Client, mbox *imap.MailboxStatus) (int, error) {
	log.Printf("TB import: initializing mailbox %q (UID validity %d), logging the last %d days as history", i.cfg.Mailbox, mbox.UidValidity, i.cfg.BackfillDays)
	var messages []fetched
	if i.cfg.BackfillDays > 0 {
		uids, err := c.UidSearch(&imap.SearchCriteria{Since: time.Now().AddDate(0, 0, -i.cfg.BackfillDays)})
		if err != nil {
			return 0, fmt.Errorf("search: %v", err)
		}
		if messages, err = fetchMessages(c, uids); err != nil {
			return 0, err
		}
	}
	processMu.Lock()
	defer processMu.Unlock()
	cursor := storage.ImportCursor{UIDValidity: mbox.UidValidity}
	if mbox.UidNext > 0 {
		cursor.LastUID = mbox.UidNext - 1
	}
	for _, m := range messages {
		if _, err := i.process(nil, mbox.UidValidity, m, true); err != nil {
			return 0, err
		}
		cursor.LastUID = max(cursor.LastUID, m.uid)
	}
	if err := i.ai.SetImportCursor(cursor); err != nil {
		return 0, fmt.Errorf("save cursor: %v", err)
	}
	return len(messages), nil
}

// process logs one message, updates balances and applies the rules. It
// reports whether the message carried an account balance.
func (i *Importer) process(rules []storage.ImportRule, uidValidity uint32, m fetched, history bool) (bool, error) {
	entry := newLogEntry(uidValidity, m)
	account, balance, isBalance := ParseBalance(m.msg)
	if isBalance {
		entry.Account = account
		if err := i.ai.SetImportBalance(storage.ImportBalance{Account: account, Balance: balance, UpdatedAt: m.date}); err != nil {
			return false, fmt.Errorf("save balance: %v", err)
		}
	}
	switch {
	case history:
		entry.Status = storage.ImportStatusHistory
	case !IsTransaction(m.msg):
		entry.Status = storage.ImportStatusIgnored
		if isBalance {
			entry.Status = storage.ImportStatusBalance
		}
	default:
		entry.Status = storage.ImportStatusUnmatched
		if _, ok := FirstMatch(rules, m.msg); ok {
			entry.Status = storage.ImportStatusPending
		}
	}
	inserted, err := i.ai.AddImportLogEntry(&entry)
	if err != nil {
		return false, fmt.Errorf("save log entry: %v", err)
	}
	if !inserted {
		return isBalance, nil // already processed earlier
	}
	if entry.Status == storage.ImportStatusPending {
		match, _ := FirstMatch(rules, m.msg)
		if err := createExpense(i.store, i.ai, &entry, match, ""); err != nil {
			return false, err
		}
	}
	return isBalance, nil
}

func newLogEntry(uidValidity uint32, m fetched) storage.ImportLogEntry {
	amount, description := Describe(m.msg)
	return storage.ImportLogEntry{
		UIDValidity: uidValidity,
		UID:         m.uid,
		ReceivedAt:  m.date,
		Subject:     m.msg.Subject,
		Body:        m.msg.Body,
		Kind:        Kind(m.msg),
		Amount:      amount,
		Description: description,
	}
}

// createExpense records the match as an expense and marks the entry imported.
func createExpense(store storage.Storage, ai storage.AutoImportStore, entry *storage.ImportLogEntry, match Match, note string) error {
	amount := match.Amount
	if !match.Rule.Income {
		amount = -amount
	}
	expense := storage.Expense{
		ID:       uuid.New().String(),
		Name:     match.Rule.Name,
		Category: match.Rule.Category,
		Amount:   amount,
		Date:     entry.ReceivedAt,
		Tags:     []string{},
	}
	if err := expense.Validate(); err != nil {
		entry.Status, entry.Note = storage.ImportStatusError, err.Error()
		return ai.UpdateImportLogEntry(*entry)
	}
	if err := store.AddExpense(expense); err != nil {
		entry.Status, entry.Note = storage.ImportStatusError, err.Error()
		if uerr := ai.UpdateImportLogEntry(*entry); uerr != nil {
			log.Printf("TB import ERROR: failed to update log entry %d: %v", entry.ID, uerr)
		}
		return fmt.Errorf("add expense: %v", err)
	}
	log.Printf("TB import: %s %.2f EUR (%s) from message %d", match.Rule.Name, amount, match.Rule.Category, entry.UID)
	entry.Status = storage.ImportStatusImported
	entry.RuleID, entry.RuleName, entry.ExpenseID = match.Rule.ID, match.Rule.Name, expense.ID
	entry.Amount, entry.Note = match.Amount, note
	return ai.UpdateImportLogEntry(*entry)
}

// ApplyRules imports logged messages that were not imported yet, using the
// current rules. It returns the entries that were imported.
func ApplyRules(store storage.Storage, ai storage.AutoImportStore, ids []int64) ([]storage.ImportLogEntry, error) {
	processMu.Lock()
	defer processMu.Unlock()
	rules, err := ai.GetImportRules()
	if err != nil {
		return nil, err
	}
	imported := []storage.ImportLogEntry{}
	for _, id := range ids {
		entry, err := ai.GetImportLogEntry(id)
		if err != nil {
			return imported, err
		}
		if entry.Status != storage.ImportStatusUnmatched && entry.Status != storage.ImportStatusHistory {
			continue
		}
		match, ok := FirstMatch(rules, Message{Subject: entry.Subject, Body: entry.Body})
		if !ok {
			continue
		}
		if err := createExpense(store, ai, &entry, match, "applied manually"); err != nil {
			return imported, err
		}
		imported = append(imported, entry)
	}
	return imported, nil
}

// notify mails the total available money, as the old checker did.
func (i *Importer) notify() {
	settings, err := i.ai.GetImportSettings()
	if err != nil {
		log.Printf("TB import ERROR: failed to load settings: %v", err)
		return
	}
	if !settings.NotifyEnabled || len(settings.NotifyTo) == 0 {
		return
	}
	total, ok, err := Total(i.ai, settings)
	if err != nil || !ok {
		return
	}
	if err := i.sendMail(settings.NotifyTo, fmt.Sprintf("%.2f EUR", total)); err != nil {
		log.Printf("TB import ERROR: failed to send notification: %v", err)
		return
	}
	i.statusMu.Lock()
	i.status.LastNotify = time.Now()
	i.statusMu.Unlock()
}

// Total returns the available money after subtracting the configured limits.
// It reports false until both accounts have a known balance.
func Total(ai storage.AutoImportStore, settings storage.ImportSettings) (float64, bool, error) {
	balances, err := ai.GetImportBalances()
	if err != nil {
		return 0, false, err
	}
	var current, card *storage.ImportBalance
	for idx := range balances {
		switch balances[idx].Account {
		case AccountCurrent:
			current = &balances[idx]
		case AccountCard:
			card = &balances[idx]
		}
	}
	if current == nil || card == nil {
		return 0, false, nil
	}
	total := card.Balance - settings.CardLimit + current.Balance - settings.OverdraftLimit
	return float64(int64(total*100+sign(total)*0.5)) / 100, true, nil
}

// seedRecipients copies TB_NOTIFY_TO into the settings once, so the
// recipients can be edited on the page afterwards.
func seedRecipients(ai storage.AutoImportStore, seed []string) error {
	settings, err := ai.GetImportSettings()
	if err != nil {
		return err
	}
	if settings.NotifyTo != nil || len(seed) == 0 {
		return nil
	}
	settings.NotifyTo = seed
	if err := settings.Validate(); err != nil {
		return err
	}
	log.Printf("TB import: balance e-mail recipients initialized from TB_NOTIFY_TO (%d)", len(settings.NotifyTo))
	return ai.UpdateImportSettings(settings)
}

func (i *Importer) sendMail(to []string, body string) error {
	host, _, err := net.SplitHostPort(i.cfg.SMTPAddr)
	if err != nil {
		return err
	}
	msg := "From: " + i.cfg.NotifyFrom + "\r\n" +
		"To: " + strings.Join(to, ",") + "\r\n" +
		"Subject: Celkovy zostatok na ucte\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Date: " + time.Now().Format(time.RFC1123Z) + "\r\n\r\n" + body + "\r\n"
	auth := smtp.PlainAuth("", i.cfg.User, i.cfg.Pass, host)
	return smtp.SendMail(i.cfg.SMTPAddr, auth, i.cfg.NotifyFrom, to, []byte(msg))
}
