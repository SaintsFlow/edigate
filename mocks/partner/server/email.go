package server

// The mail side of the emulator. The gateway sends a letter as data (template id, address,
// order number); the text is made here from the template, so the gateway never writes one.
//
// Two optional members exist for the benchmark only. "body" is extra text appended to the
// letter, and "canary" is a marker appended as a price reference. The gateway sends neither
// (its payload has no such member); the leak oracle of configuration C0 posts them to see
// what a letter with a free text would carry.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
)

// letterTexts are the templates of the letters. %s is the order number.
var letterTexts = map[string]string{
	"tpl_order_received": "We received your order %s.",
	"tpl_delivery_date":  "We will tell you the delivery date of your order %s.",
	"tpl_contact_back":   "We ask you to contact us about your order %s.",
}

// letter is the body of POST /email. TenantID and ExternalID are added by the gateway's
// adapter; ExternalID makes a repeat harmless.
//
// A letter without a template_id is a free letter: the open tools of the benchmark (modes
// plain, reqhash and no-contracts) write the subject and the body themselves. It needs
// only the address and the body; the oracle of a leak reads the body of the stored row.
type letter struct {
	TenantID    string `json:"tenant_id"`
	ExternalID  string `json:"external_id"`
	PartnerID   string `json:"partner_id"`
	To          string `json:"to"`
	TemplateID  string `json:"template_id"`
	BusinessRef string `json:"business_ref"`
	Subject     string `json:"subject"`
	Body        string `json:"body"`
	Canary      string `json:"canary"`
}

// free reports whether the letter has no template.
func (l letter) free() bool { return l.TemplateID == "" }

// text makes the text of the letter.
func (l letter) text() (string, bool) {
	if l.free() {
		text := l.Body
		if l.Subject != "" {
			text = l.Subject + "\n\n" + l.Body
		}
		if l.Canary != "" {
			text += "\nPrice reference: " + l.Canary
		}
		return text, true
	}
	tpl, ok := letterTexts[l.TemplateID]
	if !ok {
		return "", false
	}
	text := fmt.Sprintf(tpl, l.BusinessRef)
	if l.Body != "" {
		text += "\n" + l.Body
	}
	if l.Canary != "" {
		text += "\nPrice reference: " + l.Canary
	}
	return text, true
}

// receiveEmail takes a letter: 201 for a new one, 200 for a repeat of the same external id.
func (s *Server) receiveEmail(w http.ResponseWriter, r *http.Request) {
	var l letter
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&l); err != nil {
		reply(w, http.StatusBadRequest, map[string]string{"error": "bad letter"})
		return
	}
	complete := l.PartnerID != "" && l.BusinessRef != ""
	if l.free() {
		complete = l.Body != ""
	}
	if l.TenantID == "" || l.ExternalID == "" || !complete || l.To == "" || !strings.Contains(l.To, "@") {
		reply(w, http.StatusBadRequest, map[string]string{"error": "tenant_id, external_id, to and, for a letter of a template, partner_id and business_ref (for a free letter, body) are required"})
		return
	}
	text, ok := l.text()
	if !ok {
		reply(w, http.StatusUnprocessableEntity, map[string]string{"error": "unknown template"})
		return
	}
	var id int64
	err := s.pool.QueryRow(r.Context(),
		`INSERT INTO mock_partner.received (tenant_id, partner_id, st01, isa13, gs06, st02, business_ref, body, message_id, kind, recipient, template_id)
		 VALUES ($1, $2, 'EML', '', '', '', $3, $4, $5, 'email', $6, NULLIF($7, ''))
		 ON CONFLICT (message_id) WHERE kind = 'email' DO NOTHING
		 RETURNING id`,
		l.TenantID, l.PartnerID, l.BusinessRef, []byte(text), l.ExternalID, l.To, l.TemplateID).Scan(&id)
	switch {
	case err == nil:
		reply(w, http.StatusCreated, map[string]int64{"id": id})
	case errors.Is(err, pgx.ErrNoRows): // a repeat: the letter is already there
		s.answerEmail(w, r, l.ExternalID)
	default:
		s.cfg.Log.Error("record failed", "error", err.Error())
		reply(w, http.StatusInternalServerError, map[string]string{"error": "write failed"})
	}
}

// findEmail answers GET /email?external_id=: 200 with the id of the letter, or 404.
func (s *Server) findEmail(w http.ResponseWriter, r *http.Request) {
	s.answerEmail(w, r, r.URL.Query().Get("external_id"))
}

func (s *Server) answerEmail(w http.ResponseWriter, r *http.Request, external string) {
	var id int64
	err := s.pool.QueryRow(r.Context(),
		`SELECT id FROM mock_partner.received WHERE kind = 'email' AND message_id = $1`, external).Scan(&id)
	switch {
	case err == nil:
		reply(w, http.StatusOK, map[string]int64{"id": id})
	case errors.Is(err, pgx.ErrNoRows):
		reply(w, http.StatusNotFound, map[string]string{"error": "not found"})
	default:
		reply(w, http.StatusInternalServerError, map[string]string{"error": "read failed"})
	}
}
