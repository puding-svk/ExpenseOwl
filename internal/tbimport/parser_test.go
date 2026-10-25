package tbimport

import (
	"os"
	"testing"

	"github.com/tanq16/expenseowl/internal/storage"
)

const footer = "\r\nS pozdravom\r\n\r\nTATRA BANKA, a.s.\r\nhttp://www.tatrabanka.sk\r\n"

var (
	msgAccountDebit = Message{
		Subject: "Debet na ucte (ID=241026/676583-5)",
		Body: "Vazeny klient,\r\n\r\n24.10.2026 18:53 bol zostatok Vasho uctu SK0011000000001234567890 znizeny o 43,29 EUR.\r\n" +
			"uctovny zostatok:                             1 528,78 EUR\r\n" +
			"aktualny zostatok:                            1 528,78 EUR\r\n" +
			"disponibilny zostatok:                        5 028,78 EUR\r\n\r\n" +
			"Popis transakcie: Platba kartou 4405**0000, LIDL SLOVENSKA REPUBLIKA.\r\n" + footer,
	}
	msgAccountCredit = Message{
		Subject: "Kredit na ucte (ID=241026/725344-5)",
		Body: "Vazeny klient,\r\n\r\n24.10.2026 19:55 bol zostatok Vasho uctu SK0011000000001234567890 zvyseny o 1 100,00 EUR.\r\n" +
			"uctovny zostatok:                             1 628,78 EUR\r\n" +
			"disponibilny zostatok:                        5 128,78 EUR\r\n\r\n" +
			"Ucet protistrany: SK0000000000000000000000 ACME s.r.o.\r\n" +
			"Informacia pre prijemcu: mzda 10/2026\r\n" + footer,
	}
	msgCardDebit = Message{
		Subject: "debet na kreditnej karte (ID=6656562005)",
		Body: "Vazeny klient,\r\n\r\ndna 24.10.2026 17:24 bol znizeny zostatok uveroveho ramca Vasej kreditnej karty o sumu transakcie vo vyske 15.00 EUR \r\n" +
			"Cislo karty: 4337**0000\r\n" +
			"Popis transakcie: Billa 0123, Bratislava SK, dom nakup - autorizovana\r\n" +
			"Disponibilny zostatok: 3905.83 EUR \r\n" + footer,
	}
	msgStatement = Message{
		Subject: "Stav na ucte (ID=241026/88985-5)",
		Body: "Vazeny klient,\r\n\r\n24.10.2026 07:00 bol na vasom ucte SK0011000000001234567890:\r\n" +
			"uctovny zostatok:                              1 588,07 EUR\r\n" +
			"disponibilny zostatok:                         5 088,07 EUR\r\n" + footer,
	}
	msgSavings = Message{
		Subject: "Kredit na Sporeni k uctu (ID=141026/533883-5)",
		Body: "Vazeny klient,\r\n\r\n14.10.2026 11:46 bol zostatok Vasho sporenia Skolne zvyseny o 510,00 EUR.\r\n" +
			"disponibilny zostatok:                           510,00 EUR\r\n" + footer,
	}
)

func TestParseBalance(t *testing.T) {
	cases := []struct {
		msg     Message
		account string
		balance float64
		ok      bool
	}{
		{msgAccountDebit, AccountCurrent, 5028.78, true},
		{msgAccountCredit, AccountCurrent, 5128.78, true},
		{msgCardDebit, AccountCard, 3905.83, true},
		{msgStatement, AccountCurrent, 5088.07, true},
		{msgSavings, "", 0, false},
		{Message{Subject: "Hello", Body: "disponibilny zostatok: 1,00 EUR"}, "", 0, false},
	}
	for _, c := range cases {
		account, balance, ok := ParseBalance(c.msg)
		if account != c.account || balance != c.balance || ok != c.ok {
			t.Errorf("%q: got (%q, %v, %v), want (%q, %v, %v)", c.msg.Subject, account, balance, ok, c.account, c.balance, c.ok)
		}
	}
}

func loadLegacy(t *testing.T) []storage.ImportRule {
	t.Helper()
	data, err := os.ReadFile("testdata/known_transactions.json")
	if err != nil {
		t.Fatal(err)
	}
	rules, err := ParseLegacyRules(data)
	if err != nil {
		t.Fatal(err)
	}
	return rules
}

func TestLegacyRules(t *testing.T) {
	rules := loadLegacy(t)
	if len(rules) != 26 {
		t.Fatalf("got %d rules, want 26", len(rules))
	}
	r := rules[0]
	if r.Name != "Lidl" || r.Category != "Potraviny" || r.AmountPrefix != " znizeny o " || r.AmountPostfix != " EUR" {
		t.Errorf("unexpected first rule: %+v", r)
	}
	if len(r.SubjectContains) != 1 || len(r.RowConditions) != 1 || len(r.RowConditions[0]) != 3 {
		t.Errorf("unexpected conditions: %+v", r)
	}
	for _, r := range rules {
		if r.Name == "Salary" && (!r.Income || len(r.RowConditions) != 2) {
			t.Errorf("salary rule not converted correctly: %+v", r)
		}
	}
}

func TestFirstMatch(t *testing.T) {
	rules := loadLegacy(t)
	cases := []struct {
		msg    Message
		name   string
		amount float64
		income bool
	}{
		{msgAccountDebit, "Lidl", 43.29, false},
		{msgCardDebit, "Billa", 15.00, false},
		{msgAccountCredit, "Salary", 1100.00, true},
	}
	for _, c := range cases {
		m, ok := FirstMatch(rules, c.msg)
		if !ok {
			t.Errorf("%q: no match", c.msg.Subject)
			continue
		}
		if m.Rule.Name != c.name || m.Amount != c.amount || m.Rule.Income != c.income {
			t.Errorf("%q: got %s %.2f income=%v, want %s %.2f income=%v", c.msg.Subject, m.Rule.Name, m.Amount, m.Rule.Income, c.name, c.amount, c.income)
		}
	}
	for _, msg := range []Message{msgStatement, msgSavings} {
		if m, ok := FirstMatch(rules, msg); ok {
			t.Errorf("%q: unexpected match %s", msg.Subject, m.Rule.Name)
		}
	}
}

func TestDisabledRuleAndMissingAmount(t *testing.T) {
	rule := storage.ImportRule{
		Enabled: true, Name: "Lidl", Category: "Food",
		SubjectContains: []string{"debet na"},
		RowConditions:   [][]string{{"popis transakcie", "lidl"}},
		AmountPrefix:    " vo vyske ", AmountPostfix: " EUR",
	}
	// matches, but the prefix is not in an account debit e-mail
	if _, ok := FirstMatch([]storage.ImportRule{rule}, msgAccountDebit); ok {
		t.Error("rule without an extractable amount must not match")
	}
	rule.AmountPrefix = " znizeny o "
	if _, ok := FirstMatch([]storage.ImportRule{rule}, msgAccountDebit); !ok {
		t.Error("expected a match")
	}
	rule.Enabled = false
	if _, ok := FirstMatch([]storage.ImportRule{rule}, msgAccountDebit); ok {
		t.Error("disabled rule must not match")
	}
}

func TestRowConditionsMustShareARow(t *testing.T) {
	rule := storage.ImportRule{
		Enabled: true, Name: "x", Category: "x",
		RowConditions: [][]string{{"Popis transakcie", "znizeny o"}},
		AmountPrefix:  " znizeny o ", AmountPostfix: " EUR",
	}
	if Matches(rule, msgAccountDebit) {
		t.Error("terms on different rows must not satisfy one group")
	}
}

func TestDescribe(t *testing.T) {
	cases := []struct {
		msg         Message
		amount      float64
		description string
	}{
		{msgAccountDebit, 43.29, "Platba kartou 4405**0000, LIDL SLOVENSKA REPUBLIKA."},
		{msgAccountCredit, 1100.00, ""},
		{msgCardDebit, 15.00, "Billa 0123, Bratislava SK, dom nakup - autorizovana"},
		{msgStatement, 0, ""},
	}
	for _, c := range cases {
		amount, description := Describe(c.msg)
		if amount != c.amount || description != c.description {
			t.Errorf("%q: got (%v, %q), want (%v, %q)", c.msg.Subject, amount, description, c.amount, c.description)
		}
	}
}

func TestKind(t *testing.T) {
	for msg, want := range map[*Message]string{
		&msgAccountDebit: "debit", &msgCardDebit: "debit", &msgAccountCredit: "credit",
		&msgStatement: "statement", &msgSavings: "credit",
		{Subject: "Uhrada dlznej sumy na kreditnej karte (ID=1)"}: "credit",
		{Subject: "Newsletter"}:                                   "other",
	} {
		if got := Kind(*msg); got != want {
			t.Errorf("%q: got %s, want %s", msg.Subject, got, want)
		}
	}
}

func TestParseAmount(t *testing.T) {
	for in, want := range map[string]float64{"1 628,78": 1628.78, "3905.83": 3905.83, "15.00": 15, " 0,10 ": 0.1, "-5,5": -5.5} {
		if got, ok := parseAmount(in); !ok || got != want {
			t.Errorf("parseAmount(%q) = %v, %v; want %v", in, got, ok, want)
		}
	}
	if _, ok := parseAmount("abc"); ok {
		t.Error("parseAmount(abc) should fail")
	}
}
