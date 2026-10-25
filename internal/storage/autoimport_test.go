package storage

import (
	"reflect"
	"strings"
	"testing"
)

func TestImportSettingsValidateRecipients(t *testing.T) {
	cases := []struct {
		name    string
		in      ImportSettings
		want    []string
		wantErr string
	}{
		{"cleans and dedupes", ImportSettings{NotifyEnabled: true, NotifyTo: []string{" a@example.com ", "", "Nika <b@example.com>", "A@example.com"}},
			[]string{"a@example.com", "b@example.com"}, ""},
		{"nil becomes empty", ImportSettings{}, []string{}, ""},
		{"enabled needs a recipient", ImportSettings{NotifyEnabled: true, NotifyTo: []string{" "}}, nil, "at least one recipient"},
		{"rejects garbage", ImportSettings{NotifyTo: []string{"not an address"}}, nil, "invalid e-mail address"},
		{"rejects header injection", ImportSettings{NotifyTo: []string{"a@example.com\r\nBcc: x@evil.test"}}, nil, "invalid e-mail address"},
		{"negative limit", ImportSettings{CardLimit: -1}, nil, "negative"},
	}
	for _, c := range cases {
		err := c.in.Validate()
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%s: got %v, want error containing %q", c.name, err, c.wantErr)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(c.in.NotifyTo, c.want) {
			t.Errorf("%s: got %v, %v; want %v", c.name, c.in.NotifyTo, err, c.want)
		}
	}
	many := ImportSettings{}
	for i := 0; i < 11; i++ {
		many.NotifyTo = append(many.NotifyTo, string(rune('a'+i))+"@example.com")
	}
	if err := many.Validate(); err == nil {
		t.Error("11 recipients should be rejected")
	}
}
