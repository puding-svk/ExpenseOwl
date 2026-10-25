package api

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/tanq16/expenseowl/internal/storage"
	"github.com/tanq16/expenseowl/internal/tbimport"
	"github.com/tanq16/expenseowl/internal/web"
)

// AutoImportHandler serves the bank e-mail auto-import page and API.
type AutoImportHandler struct {
	storage  storage.Storage
	ai       storage.AutoImportStore // nil when the backend does not support it
	importer *tbimport.Importer      // nil when the poller is disabled
}

func NewAutoImportHandler(s storage.Storage, importer *tbimport.Importer) *AutoImportHandler {
	ai, _ := s.(storage.AutoImportStore)
	return &AutoImportHandler{storage: s, ai: ai, importer: importer}
}

func (h *AutoImportHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/autoimport", h.ServePage)
	mux.HandleFunc("/autoimport/status", h.GetStatus)
	mux.HandleFunc("/autoimport/poll", h.PollNow)
	mux.HandleFunc("/autoimport/settings/edit", h.UpdateSettings)
	mux.HandleFunc("/autoimport/rules", h.GetRules)
	mux.HandleFunc("/autoimport/rules/reorder", h.ReorderRules)
	mux.HandleFunc("/autoimport/rules/test", h.TestRule)
	mux.HandleFunc("/autoimport/rules/import", h.ImportLegacyRules)
	mux.HandleFunc("/autoimport/rule", h.AddRule)
	mux.HandleFunc("/autoimport/rule/edit", h.EditRule)
	mux.HandleFunc("/autoimport/rule/delete", h.DeleteRule)
	mux.HandleFunc("/autoimport/log", h.GetLog)
	mux.HandleFunc("/autoimport/log/apply", h.ApplyRules)
	mux.HandleFunc("/autoimport/log/draft", h.GetDraft)
	mux.HandleFunc("/autoimport/log/expense", h.AddExpenseFromEntry)
}

// requires checks the method and that the backend supports auto-import.
func (h *AutoImportHandler) requires(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Error: "Method not allowed"})
		return false
	}
	if h.ai == nil {
		writeJSON(w, http.StatusNotImplemented, ErrorResponse{Error: "Auto-import requires the postgres storage backend"})
		return false
	}
	return true
}

func internalError(w http.ResponseWriter, what string, err error) {
	writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: "Failed to " + what})
	log.Printf("API ERROR: Failed to %s: %v\n", what, err)
}

func (h *AutoImportHandler) ServePage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Error: "Method not allowed"})
		return
	}
	w.Header().Set("Content-Type", "text/html")
	if err := web.ServeTemplate(w, "autoimport.html"); err != nil {
		http.Error(w, "Failed to serve template", http.StatusInternalServerError)
	}
}

// ------------------------------------------------------------
// Status and settings
// ------------------------------------------------------------

func (h *AutoImportHandler) GetStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, ErrorResponse{Error: "Method not allowed"})
		return
	}
	resp := map[string]any{"available": h.ai != nil, "poller": nil}
	if h.importer != nil {
		resp["poller"] = h.importer.Status()
	}
	if h.ai == nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	settings, err := h.ai.GetImportSettings()
	if err != nil {
		internalError(w, "get import settings", err)
		return
	}
	balances, err := h.ai.GetImportBalances()
	if err != nil {
		internalError(w, "get balances", err)
		return
	}
	total, known, err := tbimport.Total(h.ai, settings)
	if err != nil {
		internalError(w, "compute total", err)
		return
	}
	resp["settings"] = settings
	resp["balances"] = balances
	resp["total"] = total
	resp["totalKnown"] = known
	writeJSON(w, http.StatusOK, resp)
}

func (h *AutoImportHandler) PollNow(w http.ResponseWriter, r *http.Request) {
	if !h.requires(w, r, http.MethodPost) {
		return
	}
	if h.importer == nil {
		writeJSON(w, http.StatusConflict, ErrorResponse{Error: "The mailbox poller is disabled"})
		return
	}
	h.importer.PollNow()
	writeJSON(w, http.StatusOK, map[string]string{"status": "success"})
}

func (h *AutoImportHandler) UpdateSettings(w http.ResponseWriter, r *http.Request) {
	if !h.requires(w, r, http.MethodPut) {
		return
	}
	var settings storage.ImportSettings
	if err := json.NewDecoder(r.Body).Decode(&settings); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "Invalid request body"})
		return
	}
	if err := settings.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return
	}
	if err := h.ai.UpdateImportSettings(settings); err != nil {
		internalError(w, "update import settings", err)
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

// ------------------------------------------------------------
// Rules
// ------------------------------------------------------------

func (h *AutoImportHandler) GetRules(w http.ResponseWriter, r *http.Request) {
	if !h.requires(w, r, http.MethodGet) {
		return
	}
	rules, err := h.ai.GetImportRules()
	if err != nil {
		internalError(w, "get import rules", err)
		return
	}
	writeJSON(w, http.StatusOK, rules)
}

func decodeRule(w http.ResponseWriter, r *http.Request) (storage.ImportRule, bool) {
	var rule storage.ImportRule
	if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "Invalid request body"})
		return rule, false
	}
	if err := rule.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return rule, false
	}
	return rule, true
}

func (h *AutoImportHandler) AddRule(w http.ResponseWriter, r *http.Request) {
	if !h.requires(w, r, http.MethodPut) {
		return
	}
	rule, ok := decodeRule(w, r)
	if !ok {
		return
	}
	rule.ID = ""
	rule, err := h.ai.AddImportRule(rule)
	if err != nil {
		internalError(w, "add import rule", err)
		return
	}
	writeJSON(w, http.StatusOK, rule)
}

func (h *AutoImportHandler) EditRule(w http.ResponseWriter, r *http.Request) {
	if !h.requires(w, r, http.MethodPut) {
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "ID parameter is required"})
		return
	}
	rule, ok := decodeRule(w, r)
	if !ok {
		return
	}
	if err := h.ai.UpdateImportRule(id, rule); err != nil {
		internalError(w, "update import rule", err)
		return
	}
	rule.ID = id
	writeJSON(w, http.StatusOK, rule)
}

func (h *AutoImportHandler) DeleteRule(w http.ResponseWriter, r *http.Request) {
	if !h.requires(w, r, http.MethodDelete) {
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "ID parameter is required"})
		return
	}
	if err := h.ai.RemoveImportRule(id); err != nil {
		internalError(w, "delete import rule", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "success"})
}

func (h *AutoImportHandler) ReorderRules(w http.ResponseWriter, r *http.Request) {
	if !h.requires(w, r, http.MethodPut) {
		return
	}
	var ids []string
	if err := json.NewDecoder(r.Body).Decode(&ids); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "Invalid request body"})
		return
	}
	if err := h.ai.ReorderImportRules(ids); err != nil {
		internalError(w, "reorder import rules", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "success"})
}

// ImportLegacyRules appends the rules of an old known_transactions.json file.
func (h *AutoImportHandler) ImportLegacyRules(w http.ResponseWriter, r *http.Request) {
	if !h.requires(w, r, http.MethodPost) {
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "Invalid request body"})
		return
	}
	rules, err := tbimport.ParseLegacyRules(data)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return
	}
	for _, rule := range rules {
		if _, err := h.ai.AddImportRule(rule); err != nil {
			internalError(w, "add import rule", err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]int{"imported": len(rules)})
}

type ruleTestResult struct {
	Entry       storage.ImportLogEntry `json:"entry"`
	Matches     bool                   `json:"matches"`     // conditions hold
	Amount      float64                `json:"amount"`      // extracted amount, 0 if none
	ShadowedBy  string                 `json:"shadowedBy"`  // earlier rule that wins
	WouldImport bool                   `json:"wouldImport"` // the entry is not imported yet and this rule would win
}

// TestRule evaluates an unsaved rule against the logged messages. The rule's
// ID, if set, is used to find its position among the saved rules.
func (h *AutoImportHandler) TestRule(w http.ResponseWriter, r *http.Request) {
	if !h.requires(w, r, http.MethodPost) {
		return
	}
	var rule storage.ImportRule
	if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "Invalid request body"})
		return
	}
	if err := rule.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return
	}
	rule.Enabled = true
	rules, err := h.ai.GetImportRules()
	if err != nil {
		internalError(w, "get import rules", err)
		return
	}
	// rules that would be evaluated before this one
	var earlier []storage.ImportRule
	for _, other := range rules {
		if other.ID == rule.ID && rule.ID != "" {
			break
		}
		earlier = append(earlier, other)
	}
	entries, err := h.ai.GetImportLogEntries(storage.ImportLogFilter{Since: time.Now().AddDate(0, 0, -daysParam(r, 90)), Limit: 2000})
	if err != nil {
		internalError(w, "get import log", err)
		return
	}
	results := []ruleTestResult{}
	for _, e := range entries {
		msg := tbimport.Message{Subject: e.Subject, Body: e.Body}
		if !tbimport.Matches(rule, msg) {
			continue
		}
		res := ruleTestResult{Entry: e, Matches: true}
		res.Amount, _ = tbimport.ExtractAmount(rule, msg)
		if m, ok := tbimport.FirstMatch(earlier, msg); ok {
			res.ShadowedBy = m.Rule.Name
		}
		res.WouldImport = res.Amount > 0 && res.ShadowedBy == "" &&
			(e.Status == storage.ImportStatusUnmatched || e.Status == storage.ImportStatusHistory)
		res.Entry.Body = ""
		results = append(results, res)
	}
	writeJSON(w, http.StatusOK, map[string]any{"scanned": len(entries), "results": results})
}

// ------------------------------------------------------------
// Log
// ------------------------------------------------------------

func daysParam(r *http.Request, fallback int) int {
	days, err := strconv.Atoi(r.URL.Query().Get("days"))
	if err != nil || days <= 0 {
		return fallback
	}
	return days
}

func (h *AutoImportHandler) GetLog(w http.ResponseWriter, r *http.Request) {
	if !h.requires(w, r, http.MethodGet) {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	entries, err := h.ai.GetImportLogEntries(storage.ImportLogFilter{
		Status: r.URL.Query().Get("status"),
		Since:  time.Now().AddDate(0, 0, -daysParam(r, 30)),
		Limit:  limit,
	})
	if err != nil {
		internalError(w, "get import log", err)
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

func (h *AutoImportHandler) ApplyRules(w http.ResponseWriter, r *http.Request) {
	if !h.requires(w, r, http.MethodPost) {
		return
	}
	var ids []int64
	if err := json.NewDecoder(r.Body).Decode(&ids); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "Invalid request body"})
		return
	}
	imported, err := tbimport.ApplyRules(h.storage, h.ai, ids)
	if err != nil {
		internalError(w, "apply rules", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"imported": imported})
}

func idParam(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "ID parameter is required"})
		return 0, false
	}
	return id, true
}

// GetDraft returns the prefill for adding an expense from a logged e-mail.
func (h *AutoImportHandler) GetDraft(w http.ResponseWriter, r *http.Request) {
	if !h.requires(w, r, http.MethodGet) {
		return
	}
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	draft, err := tbimport.BuildDraft(h.storage, h.ai, id)
	if errors.Is(err, storage.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, ErrorResponse{Error: "Bank e-mail not found"})
		return
	}
	if err != nil {
		internalError(w, "build expense draft", err)
		return
	}
	writeJSON(w, http.StatusOK, draft)
}

// AddExpenseFromEntry adds the expense from the dashboard form and links it
// to the logged e-mail in one step.
func (h *AutoImportHandler) AddExpenseFromEntry(w http.ResponseWriter, r *http.Request) {
	if !h.requires(w, r, http.MethodPost) {
		return
	}
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	var expense storage.Expense
	if err := json.NewDecoder(r.Body).Decode(&expense); err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "Invalid request body"})
		return
	}
	entry, err := tbimport.AddManualExpense(h.storage, h.ai, id, expense)
	if errors.Is(err, storage.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, ErrorResponse{Error: "Bank e-mail not found"})
		return
	}
	if tbimport.IsNotAddable(err) {
		writeJSON(w, http.StatusConflict, ErrorResponse{Error: err.Error()})
		return
	}
	if err != nil {
		internalError(w, "add expense from e-mail", err)
		return
	}
	entry.Body = ""
	writeJSON(w, http.StatusOK, entry)
}
