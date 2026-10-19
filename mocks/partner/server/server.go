// Package server is partner-emulator: the stand's trading partner that receives the
// documents the gateway sends and answers them the way a real partner does.
//
// Input: POST /as2 with an X12 interchange (an 855 or a 856) and the headers AS2-From (the
// gateway), AS2-To (this partner) and Message-Id. Output: the MDN as JSON, and later,
// asynchronously, a 997 posted to the gateway's /edi/inbound with the token of this partner.
// Every transmission is parsed with the strict parser of the gateway (internal/edi/x12) and
// written to mock_partner.received, also a repeat with the same ISA13: a test counts the
// different ISA13 values to see how many documents the partner really got.
//
// The behaviour is set by knobs that tests and the stand change at run time with POST /control
// (only from the stand's own addresses: loopback and private networks): the delay and the
// verdict of the 997 (A, E, R, P with mixed verdicts, none), the delay of the MDN and the
// share of transmissions that get no MDN.
package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SaintsFlow/edigate/internal/edi/x12"
)

const schemaSQL = `
CREATE SCHEMA IF NOT EXISTS mock_partner;
CREATE TABLE IF NOT EXISTS mock_partner.received (
    id           bigserial PRIMARY KEY,
    tenant_id    text NOT NULL,
    partner_id   text NOT NULL,
    st01         text NOT NULL,
    isa13        text NOT NULL,
    gs06         text NOT NULL,
    st02         text NOT NULL,
    business_ref text NOT NULL,
    body         bytea NOT NULL,
    message_id   text NOT NULL,
    received_at  timestamptz NOT NULL DEFAULT now()
);
CREATE SEQUENCE IF NOT EXISTS mock_partner.control_seq;`

// Migrate creates the schema, the table and the counter of the emulator if they are missing.
// tenant_id of a row is the id the gateway sent under (AS2-From), because the partner does
// not know tenants; partner_id is the id of this partner (AS2-To).
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, schemaSQL)
	return err
}

// Knobs are the settings that change the behaviour. The JSON names are those of POST /control.
type Knobs struct {
	AckDelayMS  int     `json:"ack_delay_ms"` // PARTNER_ACK_DELAY_MS
	AckStatus   string  `json:"ack_status"`   // PARTNER_ACK_STATUS: A, E, R, P or none
	MDNDelayMS  int     `json:"mdn_delay_ms"` // PARTNER_MDN_DELAY_MS
	MDNDropRate float64 `json:"mdn_drop"`     // PARTNER_MDN_DROP: share of transmissions without an MDN, 0 to 1
}

// Valid reports whether the knobs are in range.
func (k Knobs) Valid() bool {
	switch k.AckStatus {
	case "A", "E", "R", "P", "none":
	default:
		return false
	}
	return k.AckDelayMS >= 0 && k.MDNDelayMS >= 0 && k.MDNDropRate >= 0 && k.MDNDropRate <= 1 && !math.IsNaN(k.MDNDropRate)
}

// Config is what the emulator needs to start.
type Config struct {
	Knobs
	GatewayURL string // where the 997 goes: <GatewayURL>/edi/inbound
	TokensFile string // JSON {"<partner id>": "<token>"}, read again at every send
	Log        *slog.Logger
}

// Server is the HTTP handler of the emulator.
type Server struct {
	pool *pgxpool.Pool
	cfg  Config
	http *http.Client

	mu    sync.Mutex // guards knobs
	knobs Knobs
	wg    sync.WaitGroup // the 997 that are on their way
}

// New builds the server on a pool whose database has the schema (see Migrate).
func New(pool *pgxpool.Pool, cfg Config) *Server {
	if cfg.AckStatus == "" {
		cfg.AckStatus = "A"
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Server{pool: pool, cfg: cfg, knobs: cfg.Knobs, http: &http.Client{Timeout: 15 * time.Second}}
}

// Close waits for the 997 that are on their way.
func (s *Server) Close() { s.wg.Wait() }

func (s *Server) current() Knobs {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.knobs
}

// Handler returns the routes of the emulator.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /as2", s.receive)
	mux.HandleFunc("GET /control", s.control)
	mux.HandleFunc("POST /control", s.control)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.pool.Ping(r.Context()); err != nil {
			reply(w, http.StatusServiceUnavailable, map[string]string{"error": "database unavailable"})
			return
		}
		reply(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return mux
}

// mdn is the receipt, in the form the gateway reads (the field order is the same as its own).
type mdn struct {
	MessageID         string `json:"message_id"`
	OriginalMessageID string `json:"original_message_id"`
	MICSHA256         string `json:"mic_sha256"`
	Disposition       string `json:"disposition"`
}

const (
	dispositionOK    = "processed"
	dispositionError = "processed/Error: unexpected-processing-error"
)

// received is what is kept of one set of a transmission.
type received struct{ st01, gs06, st02, ref string }

// receive takes one transmission: parse, record, answer with the MDN, send the 997 later.
func (s *Server) receive(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, x12.MaxBytes))
	if err != nil {
		reply(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "body too large"})
		return
	}
	from, to, messageID := r.Header.Get("AS2-From"), r.Header.Get("AS2-To"), r.Header.Get("Message-Id")
	if from == "" || to == "" || messageID == "" {
		reply(w, http.StatusBadRequest, map[string]string{"error": "AS2-From, AS2-To and Message-Id are required"})
		return
	}
	sum := sha256.Sum256(body)
	receipt := mdn{MessageID: newID() + "@edigate-partner", OriginalMessageID: messageID, MICSHA256: hex.EncodeToString(sum[:]), Disposition: dispositionOK}

	ic, sets, perr := parse(body)
	if perr != nil {
		s.cfg.Log.Warn("a transmission was refused", "from", from, "error", perr.Error())
		receipt.Disposition = dispositionError
		reply(w, http.StatusOK, receipt)
		return
	}
	for _, set := range sets {
		_, err := s.pool.Exec(r.Context(),
			`INSERT INTO mock_partner.received (tenant_id, partner_id, st01, isa13, gs06, st02, business_ref, body, message_id)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			from, to, set.st01, ic.ControlNumber(), set.gs06, set.st02, set.ref, body, messageID)
		if err != nil {
			s.cfg.Log.Error("record failed", "error", err.Error())
			reply(w, http.StatusInternalServerError, map[string]string{"error": "write failed"})
			return
		}
	}

	k := s.current()
	s.wg.Add(1)
	go s.acknowledge(k, from, to, ic)

	if k.MDNDelayMS > 0 {
		select {
		case <-time.After(time.Duration(k.MDNDelayMS) * time.Millisecond):
		case <-r.Context().Done():
			return
		}
	}
	if mrand.Float64() < k.MDNDropRate {
		reply(w, http.StatusServiceUnavailable, map[string]string{"error": "no receipt"}) // processed, receipt lost
		return
	}
	reply(w, http.StatusOK, receipt)
}

// parse reads the transmission with the strict parser. A set with errors refuses the
// whole transmission; only 855 and 856 are what this partner expects.
func parse(body []byte) (*x12.Interchange, []received, error) {
	ic, err := x12.Parse(body)
	if err != nil {
		return nil, nil, err
	}
	var out []received
	for _, g := range ic.Groups {
		for _, set := range g.Sets {
			if len(set.Errors) > 0 {
				return nil, nil, fmt.Errorf("set %s has errors (AK502 %s)", x12.Printable(set.ST.Value(1)), set.Errors[0].AK502)
			}
			ref := ""
			switch set.ST.Value(1) {
			case "855":
				ref = first(set, "BAK", 3)
			case "856":
				ref = first(set, "PRF", 1)
			default:
				return nil, nil, errors.New("this partner takes 855 and 856 only")
			}
			out = append(out, received{st01: set.ST.Value(1), gs06: g.GS.Value(6), st02: set.ST.Value(2), ref: ref})
		}
	}
	return ic, out, nil
}

func first(set x12.Set, id string, n int) string {
	for i := range set.Segments {
		if set.Segments[i].ID == id {
			return set.Segments[i].Value(n)
		}
	}
	return ""
}

// acknowledge sends the 997 of a transmission after the delay, unless the knob says none.
func (s *Server) acknowledge(k Knobs, gateway, partner string, ic *x12.Interchange) {
	defer s.wg.Done()
	if k.AckStatus == "none" {
		return
	}
	if k.AckDelayMS > 0 {
		time.Sleep(time.Duration(k.AckDelayMS) * time.Millisecond)
	}
	for _, g := range ic.Groups {
		raw, err := s.build997(k.AckStatus, gateway, partner, g)
		if err != nil {
			s.cfg.Log.Error("build the 997 failed", "error", err.Error())
			continue
		}
		if err := s.post(partner, gateway, raw); err != nil {
			s.cfg.Log.Error("send the 997 failed", "error", err.Error())
		}
	}
}

// ack codes of the verdicts: AK5 for a set and AK9 for the group.
func verdicts(status string, n int) (ak5 []string, ak9 string, accepted int) {
	ak5 = make([]string, n)
	for i := range ak5 {
		ak5[i] = status
		if status == "P" {
			ak5[i] = "A"
			if i%2 == 1 {
				ak5[i] = "R"
			}
		}
		if ak5[i] == "A" || ak5[i] == "E" {
			accepted++
		}
	}
	switch {
	case status == "P" && n >= 2:
		ak9 = "P"
	case status == "R":
		ak9 = "R"
	case status == "E":
		ak9 = "E"
	default:
		ak9 = "A"
	}
	return
}

// build997 writes the 997 for one group of the transmission. It names the group (AK102 is
// GS06) and every set (AK202 is ST02); the 997 has no ISA13 of the original.
func (s *Server) build997(status, gateway, partner string, g x12.Group) ([]byte, error) {
	var isa, grp, st int64
	err := s.pool.QueryRow(context.Background(),
		`SELECT nextval('mock_partner.control_seq'), nextval('mock_partner.control_seq'), nextval('mock_partner.control_seq')`).Scan(&isa, &grp, &st)
	if err != nil {
		return nil, err
	}
	ak5, ak9, accepted := verdicts(status, len(g.Sets))
	body := []x12.Segment{seg("AK1", g.GS.Value(1), g.GS.Value(6))}
	for i, set := range g.Sets {
		body = append(body, seg("AK2", set.ST.Value(1), set.ST.Value(2)), seg("AK5", ak5[i]))
	}
	n := strconv.Itoa(len(g.Sets))
	body = append(body, seg("AK9", ak9, n, n, strconv.Itoa(accepted)))

	sendQual, sendID, _ := strings.Cut(partner, ":")
	recvQual, recvID, _ := strings.Cut(gateway, ":")
	now := time.Now().UTC()
	isa13, st02, gs06 := fmt.Sprintf("%09d", isa%1000000000), fmt.Sprintf("%04d", st), strconv.FormatInt(grp, 10)
	ic := &x12.Interchange{ElementSep: '*', ComponentSep: '>', SegmentTerm: '~'}
	ic.ISA = x12.Segment{ID: "ISA", Elements: elems("00", "", "00", "", sendQual, sendID, recvQual, recvID,
		now.Format("060102"), now.Format("1504"), "U", "00401", isa13, "0", "T")}
	ic.Groups = []x12.Group{{
		GS:   seg("GS", "FA", sendID, recvID, now.Format("20060102"), now.Format("1504"), gs06, "X", "004010"),
		Sets: []x12.Set{{ST: seg("ST", "997", st02), Segments: body, SE: seg("SE", strconv.Itoa(len(body)+2), st02)}},
		GE:   seg("GE", "1", gs06),
	}}
	ic.IEA = seg("IEA", "1", isa13)
	return x12.Write(ic), nil
}

// post sends the 997 to the gateway with the token of this partner. The gateway lets a
// partner post two interchanges per second, so a 429 is tried again after a pause.
func (s *Server) post(partner, gateway string, raw []byte) error {
	token, err := s.token(partner)
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 8; attempt++ {
		req, err := http.NewRequest(http.MethodPost, strings.TrimRight(s.cfg.GatewayURL, "/")+"/edi/inbound", bytes.NewReader(raw))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("AS2-From", partner)
		req.Header.Set("AS2-To", gateway)
		req.Header.Set("Message-Id", "<"+newID()+"@edigate-partner>")
		resp, err := s.http.Do(req)
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			time.Sleep(time.Duration(300*(attempt+1)) * time.Millisecond)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("the gateway answered HTTP %d", resp.StatusCode)
		}
		return nil
	}
	return errors.New("the gateway kept answering 429")
}

// token reads the file of tokens again, so a stand can fill it after the emulator started.
func (s *Server) token(partner string) (string, error) {
	raw, err := os.ReadFile(s.cfg.TokensFile)
	if err != nil {
		return "", fmt.Errorf("read the tokens file: %w", err)
	}
	var tokens map[string]string
	if err := json.Unmarshal(raw, &tokens); err != nil {
		return "", fmt.Errorf("the tokens file: %w", err)
	}
	t := tokens[partner]
	if t == "" {
		return "", fmt.Errorf("no token for partner %q", x12.Printable(partner))
	}
	return t, nil
}

// control shows the knobs, or sets the ones that are in the body. Only the stand's own
// addresses may call it.
func (s *Server) control(w http.ResponseWriter, r *http.Request) {
	if !fromStand(r.RemoteAddr) {
		reply(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	if r.Method == http.MethodPost {
		var patch struct {
			AckDelayMS  *int     `json:"ack_delay_ms"`
			AckStatus   *string  `json:"ack_status"`
			MDNDelayMS  *int     `json:"mdn_delay_ms"`
			MDNDropRate *float64 `json:"mdn_drop"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&patch); err != nil {
			reply(w, http.StatusBadRequest, map[string]string{"error": "bad knobs"})
			return
		}
		s.mu.Lock()
		next := s.knobs
		if patch.AckDelayMS != nil {
			next.AckDelayMS = *patch.AckDelayMS
		}
		if patch.AckStatus != nil {
			next.AckStatus = *patch.AckStatus
		}
		if patch.MDNDelayMS != nil {
			next.MDNDelayMS = *patch.MDNDelayMS
		}
		if patch.MDNDropRate != nil {
			next.MDNDropRate = *patch.MDNDropRate
		}
		ok := next.Valid()
		if ok {
			s.knobs = next
		}
		s.mu.Unlock()
		if !ok {
			reply(w, http.StatusBadRequest, map[string]string{"error": "knobs out of range"})
			return
		}
	}
	reply(w, http.StatusOK, s.current())
}

// fromStand reports whether the address is loopback or in a private network (the compose network).
func fromStand(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}

func seg(id string, values ...string) x12.Segment {
	return x12.Segment{ID: id, Elements: elems(values...)}
}

func elems(values ...string) []x12.Element {
	out := make([]x12.Element, len(values))
	for i, v := range values {
		out[i] = x12.Element{Parts: []string{v}}
	}
	return out
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func reply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false) // Message-Id is usually <id@host>
	_ = enc.Encode(body)
}
