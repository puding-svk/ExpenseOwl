package tbimport

import (
	"strings"
	"testing"
)

func TestDecodeMessage(t *testing.T) {
	plain := "Subject: Debet na ucte (ID=1)\r\nDate: Sat, 24 Oct 2026 18:53:10 +0200\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 7bit\r\n\r\n" +
		"bol zostatok znizeny o 43,29 EUR.\r\n"
	qp := "Subject: =?UTF-8?Q?Debet_na_=C3=BActe?=\r\n" +
		"Content-Type: multipart/alternative; boundary=XX\r\n\r\n" +
		"--XX\r\nContent-Type: text/html\r\n\r\n<p>html</p>\r\n" +
		"--XX\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n" +
		"Popis transakcie: Platba kartou, a very long line that is wrapped by the =\r\nencoder =C3=BA\r\n--XX--\r\n"

	m, date, err := decodeMessage([]byte(plain))
	if err != nil {
		t.Fatal(err)
	}
	if m.Subject != "Debet na ucte (ID=1)" || !strings.Contains(m.Body, "znizeny o 43,29 EUR") || date.IsZero() {
		t.Errorf("plain: unexpected %+v %v", m, date)
	}

	m, _, err = decodeMessage([]byte(qp))
	if err != nil {
		t.Fatal(err)
	}
	if m.Subject != "Debet na úcte" {
		t.Errorf("qp subject: %q", m.Subject)
	}
	if !strings.Contains(m.Body, "wrapped by the encoder ú") || strings.Contains(m.Body, "html") {
		t.Errorf("qp body: %q", m.Body)
	}
}
