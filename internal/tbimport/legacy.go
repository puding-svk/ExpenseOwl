package tbimport

import (
	"encoding/json"
	"fmt"

	"github.com/tanq16/expenseowl/internal/storage"
)

// legacyRule is one entry of the old known_transactions.json file.
type legacyRule struct {
	Name     string `json:"name"`
	Category string `json:"category"`
	Filter   []struct {
		SubjContains []string `json:"subj_contains"`
		RowContains  []string `json:"row_contains"`
	} `json:"filter"`
	AmountPrefix  string `json:"amount_prefix"`
	AmountPostfix string `json:"amount_postfix"`
	Income        bool   `json:"income"`
}

// ParseLegacyRules converts the contents of known_transactions.json.
func ParseLegacyRules(data []byte) ([]storage.ImportRule, error) {
	var legacy []legacyRule
	if err := json.Unmarshal(data, &legacy); err != nil {
		return nil, fmt.Errorf("invalid known_transactions.json: %v", err)
	}
	rules := make([]storage.ImportRule, 0, len(legacy))
	for i, l := range legacy {
		rule := storage.ImportRule{
			Enabled:         true,
			Name:            l.Name,
			Category:        l.Category,
			Income:          l.Income,
			SubjectContains: []string{},
			RowConditions:   [][]string{},
			AmountPrefix:    l.AmountPrefix,
			AmountPostfix:   l.AmountPostfix,
		}
		for _, f := range l.Filter {
			rule.SubjectContains = append(rule.SubjectContains, f.SubjContains...)
			if len(f.RowContains) > 0 {
				rule.RowConditions = append(rule.RowConditions, f.RowContains)
			}
		}
		if err := rule.Validate(); err != nil {
			return nil, fmt.Errorf("rule %d (%s): %v", i+1, l.Name, err)
		}
		rules = append(rules, rule)
	}
	return rules, nil
}
