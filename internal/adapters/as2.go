package adapters

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

const as2Timeout = 15 * time.Second

// Transmission is the payload of an outbox row of a document sent to a partner. It is
// written once, when the row is made (control numbers and bytes included), and every
// repeat of the transmission sends exactly this.
type Transmission struct {
	DocID     string `json:"doc_id"`
	MessageID string `json:"message_id"`
	From      string `json:"from"` // AS2-From: the interchange id of the tenant
	To        string `json:"to"`   // AS2-To: the partner
	ISA13     string `json:"isa13"`
	Body      string `json:"body"` // the X12 interchange; printable ASCII, so a string holds it
}

// Receipt is the MDN the partner answers with (the JSON form of wave 2-2).
type Receipt struct {
	MessageID         string `json:"message_id"`
	OriginalMessageID string `json:"original_message_id"`
	MICSHA256         string `json:"mic_sha256"`
	Disposition       string `json:"disposition"`
}

// Counts reports whether the receipt is the MDN of this transmission: it names our
// Message-Id, carries the SHA-256 of the bytes we sent, and says "processed".
func (r Receipt) Counts(t Transmission) bool {
	sum := sha256.Sum256([]byte(t.Body))
	return r.OriginalMessageID == t.MessageID && r.MICSHA256 == hex.EncodeToString(sum[:]) && r.Disposition == "processed"
}

// AS2 sends interchanges to a partner (here: partner-emulator) with POST <base>/as2.
// It sends once and returns the synchronous MDN; deciding what a missing or wrong MDN
// means, and when to repeat, is the relay's job.
type AS2 struct {
	base    string
	http    *http.Client
	replyTo string
}

// WithReplyTo makes the client send the stand header Edigate-Reply-To (see ReplyToHeader).
func (a *AS2) WithReplyTo(publicURL string) *AS2 {
	a.replyTo = publicURL
	return a
}

// NewAS2 builds the client for the partner at baseURL (EDIGATE_PARTNER_URL).
func NewAS2(baseURL string) *AS2 {
	return &AS2{base: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: as2Timeout}}
}

// Transmit posts the interchange. Any answer other than 200 with a readable MDN is an error.
func (a *AS2) Transmit(ctx context.Context, t Transmission) (Receipt, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+"/as2", bytes.NewReader([]byte(t.Body)))
	if err != nil {
		return Receipt{}, err
	}
	req.Header.Set("Content-Type", "application/edi-x12")
	req.Header.Set("AS2-From", t.From)
	req.Header.Set("AS2-To", t.To)
	req.Header.Set("Message-Id", t.MessageID)
	if a.replyTo != "" {
		req.Header.Set(ReplyToHeader, a.replyTo)
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return Receipt{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Receipt{}, &StatusError{Code: resp.StatusCode}
	}
	var r Receipt
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r); err != nil {
		return Receipt{}, err
	}
	return r, nil
}
