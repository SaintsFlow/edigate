package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/SaintsFlow/edigate/internal/edi/document"
	"github.com/SaintsFlow/edigate/internal/rpcerr"
	"github.com/SaintsFlow/edigate/internal/store"
)

// This file holds the reads of the modes that do not hide the text of the partner (plain,
// reqhash and no-sanitize): the stored body gets the raw text back in the place of a note
// reference, and every free text is listed. The inbound side still builds the body through the
// sanitizer; these modes undo it at read time, so one corpus serves every configuration.

// RawText is one element of free text of a document, whole.
type RawText struct {
	SegmentPath string `json:"segment_path"`
	Text        string `json:"text"`
}

// RawDocumentOutput is the answer of edi.read_document in the modes with raw text. notes is
// always empty: nothing is held back.
type RawDocumentOutput struct {
	ID     string          `json:"id"`
	ST01   string          `json:"st01"`
	Status string          `json:"status"`
	Header any             `json:"header"`
	Lines  []document.Line `json:"lines"`
	Texts  []RawText       `json:"texts"`
	Notes  []Note          `json:"notes"`
}

// resolveNotes puts the text of every note whose reason is "format" back in the place of its
// reference ("$note_N") in the JSON body, wherever a string equals the reference.
func resolveNotes(body []byte, notes []store.RawNote) ([]byte, error) {
	texts := map[string]string{}
	for _, n := range notes {
		if n.Reason == "format" {
			texts[n.Ref] = n.Text
		}
	}
	if len(texts) == 0 {
		return body, nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(substitute(v, texts))
}

func substitute(v any, texts map[string]string) any {
	switch x := v.(type) {
	case string:
		if t, ok := texts[x]; ok {
			return t
		}
	case []any:
		for i := range x {
			x[i] = substitute(x[i], texts)
		}
	case map[string]any:
		for k := range x {
			x[k] = substitute(x[k], texts)
		}
	}
	return v
}

// rawBody returns the stored body of a document of the tenant with the raw values of the
// elements that failed their format.
func rawBody(ctx context.Context, st *store.Store, tenantID, documentID string, body []byte) ([]byte, []store.RawNote, error) {
	notes, err := st.DocumentNotes(ctx, tenantID, documentID)
	if err != nil {
		return nil, nil, err
	}
	out, err := resolveNotes(body, notes)
	return out, notes, err
}

// ReadDocumentRaw returns one document of the tenant as the open modes show it: the header and
// the lines with the raw values, and the free texts. The access rules are those of ReadDocument
// (another tenant, a 997, a rejected set and an unknown id all give not_found).
func ReadDocumentRaw(ctx context.Context, st *store.Store, tenantID string, in ReadDocumentInput) (RawDocumentOutput, error) {
	d, err := st.ReadDocument(ctx, tenantID, in.DocumentID)
	if errors.Is(err, store.ErrNotFound) {
		return RawDocumentOutput{}, rpcerr.New(rpcerr.NotFound, "")
	}
	if err != nil {
		return RawDocumentOutput{}, err
	}
	body, notes, err := rawBody(ctx, st, tenantID, d.ID, d.Body)
	if err != nil {
		return RawDocumentOutput{}, err
	}
	header, lines, err := document.Decode(d.ST01, body)
	if err != nil {
		return RawDocumentOutput{}, err
	}
	texts := make([]RawText, 0, len(notes))
	for _, n := range notes {
		if n.Reason == "free_text" {
			texts = append(texts, RawText{SegmentPath: n.SegmentPath, Text: n.Text})
		}
	}
	// The addresses of the partner are trusted data of the gateway, not the text of the document; the open
	// modes list them too, so that an agent of C0 can answer a question as well as an agent of C3.
	contacts, err := st.PartnerContacts(ctx, tenantID, d.PartnerID)
	if err != nil {
		return RawDocumentOutput{}, err
	}
	return RawDocumentOutput{ID: d.ID, ST01: d.ST01, Status: d.Status, Header: headerWithContacts{header, contacts}, Lines: lines, Texts: texts, Notes: []Note{}}, nil
}

// bodyOf is the body an effect is built from: the stored one, or in the mode no-sanitize the
// one with the raw values (an element that failed its format is taken as the partner wrote it).
func bodyOf(ctx context.Context, st *store.Store, c Caller, documentID string, body []byte) ([]byte, error) {
	if !c.RawValues {
		return body, nil
	}
	out, _, err := rawBody(ctx, st, c.TenantID, documentID, body)
	return out, err
}
