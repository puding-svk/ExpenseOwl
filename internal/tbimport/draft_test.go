package tbimport

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/tanq16/expenseowl/internal/storage"
)

func TestMerchantGuess(t *testing.T) {
	for in, want := range map[string]string{
		"Platba kartou 4405**0000, LIDL SLOVENSKA REPUBLIKA.":  "LIDL SLOVENSKA REPUBLIKA",
		"BILLA 101, Bratislava SK, dom nakup - autorizovana":   "BILLA",
		"Kaktus, Bratislava SK, dom nakup - autorizovana":      "Kaktus",
		"GOPAY, *FORPSI.COM KTIS CZ, int nakup - autorizovana": "GOPAY",
		"TAKKO Fashion 3903, Bratislava SK, dom nakup":         "TAKKO Fashion",
		"Platba kartou 4405**0000, nutraceutics.sk.":           "nutraceutics.sk",
		"Vklad hotovosti cez bankomat":                         "Vklad hotovosti cez bankomat",
		"Platba 1100/000000-2614962860":                        "Platba 1100/000000-2614962860",
		"":                                                     "",
		"Platba kartou bez ciarky":                             "",
	} {
		if got := MerchantGuess(in); got != want {
			t.Errorf("MerchantGuess(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExpenseName(t *testing.T) {
	for in, want := range map[string]string{
		"LIDL SLOVENSKA REPUBLIKA": "Lidl slovenska",
		"BILLA":                    "Billa",
		"TAKKO Fashion":            "Takko fashion",
		"  ":                       "",
	} {
		if got := ExpenseName(in); got != want {
			t.Errorf("ExpenseName(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeStore implements the few storage methods the draft code uses.
type fakeStore struct {
	storage.Storage
	storage.AutoImportStore
	mu       sync.Mutex
	expenses []storage.Expense
	entries  map[int64]storage.ImportLogEntry
}

func (f *fakeStore) GetAllExpenses() ([]storage.Expense, error) { return f.expenses, nil }
func (f *fakeStore) GetExpense(id string) (storage.Expense, error) {
	for _, e := range f.expenses {
		if e.ID == id {
			return e, nil
		}
	}
	return storage.Expense{}, fmt.Errorf("expense with ID %s %w", id, storage.ErrNotFound)
}
func (f *fakeStore) AddExpense(e storage.Expense) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expenses = append(f.expenses, e)
	return nil
}
func (f *fakeStore) GetImportLogEntry(id int64) (storage.ImportLogEntry, error) {
	e, ok := f.entries[id]
	if !ok {
		return e, fmt.Errorf("import log entry %d %w", id, storage.ErrNotFound)
	}
	return e, nil
}
func (f *fakeStore) UpdateImportLogEntry(e storage.ImportLogEntry) error {
	f.entries[e.ID] = e
	return nil
}

func newFake() *fakeStore {
	t0 := time.Date(2026, 10, 24, 15, 22, 52, 0, time.UTC)
	return &fakeStore{
		expenses: []storage.Expense{
			{ID: "old1", Name: "Burgerlab", Category: "Food", Amount: -10, Date: t0.AddDate(0, -2, 0)},
			{ID: "old2", Name: "burgerlab", Category: "Restaurants", Amount: -12, Date: t0.AddDate(0, -1, 0)},
		},
		entries: map[int64]storage.ImportLogEntry{
			1: {ID: 1, Kind: "debit", Status: storage.ImportStatusUnmatched, Amount: 41, ReceivedAt: t0,
				Subject: "debet na kreditnej karte (ID=1)", Description: "Burgerlab, Bratislava SK, dom nakup - autorizovana"},
			2: {ID: 2, Kind: "credit", Status: storage.ImportStatusHistory, Amount: 100, ReceivedAt: t0,
				Subject: "Kredit na ucte (ID=2)", Description: "Vklad hotovosti cez bankomat"},
			3: {ID: 3, Kind: "debit", Status: storage.ImportStatusImported, ExpenseID: "old1", Amount: 10, ReceivedAt: t0},
			4: {ID: 4, Kind: "debit", Status: storage.ImportStatusManual, ExpenseID: "deleted", Amount: 5, ReceivedAt: t0,
				Description: "Kaktus, Bratislava SK, dom nakup"},
			5: {ID: 5, Kind: "statement", Status: storage.ImportStatusBalance, ReceivedAt: t0},
			6: {ID: 6, Kind: "debit", Status: storage.ImportStatusPending, ReceivedAt: t0},
		},
	}
}

func TestBuildDraft(t *testing.T) {
	f := newFake()
	cases := []struct {
		id       int64
		addable  bool
		warning  bool
		name     string
		category string
		income   bool
	}{
		{1, true, false, "Burgerlab", "Restaurants", false}, // latest same-name expense wins, case-insensitive
		{2, true, true, "Vklad hotovosti", "", true},        // history: allowed with a warning; no earlier expense
		{3, false, false, "", "", false},                    // already imported and the expense exists
		{4, true, true, "Kaktus", "", false},                // linked expense was deleted
		{5, false, false, "", "", false},                    // not a transaction
		{6, false, false, "", "", false},                    // still processing
	}
	for _, c := range cases {
		d, err := BuildDraft(f, f, c.id)
		if err != nil {
			t.Fatalf("entry %d: %v", c.id, err)
		}
		if d.Addable != c.addable || (d.Warning != "") != c.warning || d.Name != c.name || d.Category != c.category || d.Income != c.income {
			t.Errorf("entry %d: got addable=%v warning=%q name=%q category=%q income=%v",
				c.id, d.Addable, d.Warning, d.Name, d.Category, d.Income)
		}
		if !d.Addable && d.Reason == "" {
			t.Errorf("entry %d: missing reason", c.id)
		}
	}
}

func TestAddManualExpense(t *testing.T) {
	f := newFake()
	exp := storage.Expense{Name: "Burgerlab", Category: "Restaurants", Amount: -41}
	e, err := AddManualExpense(f, f, 1, exp)
	if err != nil {
		t.Fatal(err)
	}
	if e.Status != storage.ImportStatusManual || e.ExpenseID == "" || e.Note == "" {
		t.Errorf("entry not linked: %+v", e)
	}
	added, err := f.GetExpense(e.ExpenseID)
	if err != nil {
		t.Fatal(err)
	}
	if !added.Date.Equal(f.entries[1].ReceivedAt) {
		t.Errorf("expense date %v, want the e-mail date", added.Date)
	}
	// a second submit is refused
	if _, err := AddManualExpense(f, f, 1, exp); !IsNotAddable(err) {
		t.Errorf("second add: got %v, want a not-addable error", err)
	}
	// invalid expenses are refused without touching the entry
	if _, err := AddManualExpense(f, f, 2, storage.Expense{Name: "", Category: "x", Amount: 1}); !IsNotAddable(err) {
		t.Errorf("invalid expense: got %v", err)
	}
	if f.entries[2].Status != storage.ImportStatusHistory {
		t.Errorf("entry 2 changed after a refused add: %s", f.entries[2].Status)
	}
	// re-adding after the linked expense was deleted works
	if _, err := AddManualExpense(f, f, 4, storage.Expense{Name: "Kaktus", Category: "Food", Amount: -5}); err != nil {
		t.Errorf("re-add after delete: %v", err)
	}
}

func TestAddManualExpenseConcurrent(t *testing.T) {
	f := newFake()
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := AddManualExpense(f, f, 1, storage.Expense{Name: "Burgerlab", Category: "Food", Amount: -41}); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 1 || len(f.expenses) != 3 {
		t.Errorf("concurrent adds: %d succeeded, %d expenses; want exactly one new expense", ok, len(f.expenses))
	}
}
