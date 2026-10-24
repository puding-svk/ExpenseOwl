package tbimport

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
)

// fetchMessages downloads the given UIDs without changing their flags and
// returns them ordered by UID.
func fetchMessages(c *client.Client, uids []uint32) ([]fetched, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	set := new(imap.SeqSet)
	set.AddNum(uids...)
	section := &imap.BodySectionName{Peek: true}
	ch := make(chan *imap.Message, 16)
	done := make(chan error, 1)
	go func() {
		done <- c.UidFetch(set, []imap.FetchItem{imap.FetchUid, imap.FetchInternalDate, section.FetchItem()}, ch)
	}()
	var out []fetched
	var parseErr error
	for m := range ch {
		body := m.GetBody(section)
		if body == nil {
			continue
		}
		raw, err := io.ReadAll(body)
		if err != nil {
			parseErr = err
			continue
		}
		msg, date, err := decodeMessage(raw)
		if err != nil {
			parseErr = fmt.Errorf("message %d: %v", m.Uid, err)
			continue
		}
		if date.IsZero() {
			date = m.InternalDate
		}
		out = append(out, fetched{uid: m.Uid, date: date, msg: msg})
	}
	if err := <-done; err != nil {
		return nil, fmt.Errorf("fetch: %v", err)
	}
	if parseErr != nil {
		return nil, parseErr
	}
	sort.Slice(out, func(a, b int) bool { return out[a].uid < out[b].uid })
	return out, nil
}

// decodeMessage returns the decoded subject and plain-text body.
func decodeMessage(raw []byte) (Message, time.Time, error) {
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return Message{}, time.Time{}, err
	}
	dec := new(mime.WordDecoder)
	subject, err := dec.DecodeHeader(m.Header.Get("Subject"))
	if err != nil {
		subject = m.Header.Get("Subject")
	}
	date, _ := m.Header.Date()
	body, err := plainText(m.Header.Get("Content-Type"), m.Header.Get("Content-Transfer-Encoding"), m.Body)
	if err != nil {
		return Message{}, time.Time{}, err
	}
	return Message{Subject: subject, Body: body}, date, nil
}

// plainText returns the first text/plain part of the body.
func plainText(contentType, encoding string, r io.Reader) (string, error) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		mediaType = "text/plain"
	}
	if strings.HasPrefix(mediaType, "multipart/") {
		mr := multipart.NewReader(r, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				return "", nil
			}
			if err != nil {
				return "", err
			}
			text, err := plainText(part.Header.Get("Content-Type"), part.Header.Get("Content-Transfer-Encoding"), part)
			if err != nil || text != "" {
				return text, err
			}
		}
	}
	if mediaType != "text/plain" {
		return "", nil
	}
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "quoted-printable":
		r = quotedprintable.NewReader(r)
	case "base64":
		r = base64.NewDecoder(base64.StdEncoding, r)
	}
	data, err := io.ReadAll(r)
	return string(data), err
}
