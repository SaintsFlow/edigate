// Package policy asks the embedded OPA whether a tool call may go on, and at which
// price: at once, after one operator, or after two.
//
// Input: the principal and a ToolCall that the server filled from its own data
// (the principal record, the partner record, the stored document, the payload of
// the effect). The client chooses none of it. Output: a Decision with the names
// of the rules that forbade the action (Deny) or turned it into an approval
// request (Fired). An error of the engine means no decision: the caller must not
// do the effect.
//
// The Rego text and its data are embedded (policies/) and prepared once at start.
// Amounts reach the policy as whole cents, so no rounding can move a limit.
package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"sort"
	"strings"

	"github.com/open-policy-agent/opa/v1/rego"
	"github.com/open-policy-agent/opa/v1/storage/inmem"

	"github.com/SaintsFlow/edigate/policies"
)

// Approval levels of a decision.
const (
	ApprovalNone = "none"
	ApprovalOne  = "one"
	ApprovalTwo  = "two"
)

// Principal is the caller as the server knows it from the principal record.
type Principal struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	TenantID string `json:"tenant"`
}

// Partner is the partner record the policy checks the call against. A limit
// that is nil is not set.
type Partner struct {
	Role          string   `json:"role"`
	Active        bool     `json:"active"`
	AutoMaxCents  *int64   `json:"auto_max_cents"`
	DailyMaxCents *int64   `json:"daily_max_cents"`
	ForecastCents *int64   `json:"forecast_cents"`
	ShipToCodes   []string `json:"ship_to_codes"`
	Contacts      []string `json:"contacts"`
}

// Document is the source document: its set and the role of its sender.
type Document struct {
	ST01       string `json:"st01"`
	SenderRole string `json:"sender_role"`
}

// SupplierOrder is the order to a supplier an invoice is checked against. Lines
// are in the normal form of InvoiceLines.
type SupplierOrder struct {
	TotalCents int64      `json:"total_cents"`
	Currency   string     `json:"currency"`
	LinesNorm  [][]string `json:"lines_norm"`
}

// ToolCall is everything the policy sees about a call.
type ToolCall struct {
	Tool string
	// Effect is the payload of the effect (the same value the request hash is taken
	// over); it reaches the policy as input.effect, with amount_cents added.
	Effect         any
	AmountCents    int64
	Partner        *Partner // nil: the partner is not registered
	DailyAutoCents int64
	Document       Document
	SupplierOrder  *SupplierOrder // nil: no such order
}

// Decision is the answer of the policy.
type Decision struct {
	Allow    bool     `json:"allow"`
	Tier     int      `json:"tier"`
	Approval string   `json:"approval"`
	Deny     []string `json:"deny"`
	Fired    []string `json:"fired"`
}

// Engine is the prepared policy.
type Engine struct {
	query rego.PreparedEvalQuery
}

// Option changes the data the policy reads (data.json is the default).
type Option func(data map[string]any)

// WithUpstreamAllow sets data.upstream_allow: the up.* tools each tenant may call.
// Without it every upstream tool is forbidden (TL-07).
func WithUpstreamAllow(allow map[string][]string) Option {
	return func(data map[string]any) {
		generic := map[string]any{}
		for tenant, names := range allow {
			list := make([]any, 0, len(names))
			for _, n := range names {
				list = append(list, n)
			}
			generic[tenant] = list
		}
		data["upstream_allow"] = generic
	}
}

// New compiles the embedded policy and prepares it for evaluation.
func New(ctx context.Context, opts ...Option) (*Engine, error) {
	module, err := fs.ReadFile(policies.FS, "edigate.rego")
	if err != nil {
		return nil, err
	}
	raw, err := fs.ReadFile(policies.FS, "data.json")
	if err != nil {
		return nil, err
	}
	var data map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&data); err != nil {
		return nil, fmt.Errorf("policy data: %w", err)
	}
	for _, opt := range opts {
		opt(data)
	}
	store := inmem.NewFromObject(data)
	q, err := rego.New(
		rego.Query("data.edigate.decision"),
		rego.Module("edigate.rego", string(module)),
		rego.Store(store),
	).PrepareForEval(ctx)
	if err != nil {
		return nil, fmt.Errorf("prepare the policy: %w", err)
	}
	return &Engine{query: q}, nil
}

// Decide evaluates the policy for one call. Any failure of the engine, or an
// answer that is not a complete decision, is an error: the default is "no".
func (e *Engine) Decide(ctx context.Context, p Principal, c ToolCall) (Decision, error) {
	in, err := buildInput(p, c)
	if err != nil {
		return Decision{}, err
	}
	rs, err := e.query.Eval(ctx, rego.EvalInput(in))
	if err != nil {
		return Decision{}, fmt.Errorf("evaluate the policy: %w", err)
	}
	if len(rs) != 1 || len(rs[0].Expressions) != 1 {
		return Decision{}, errors.New("the policy gave no decision")
	}
	raw, err := json.Marshal(rs[0].Expressions[0].Value)
	if err != nil {
		return Decision{}, err
	}
	var d Decision
	if err := json.Unmarshal(raw, &d); err != nil {
		return Decision{}, fmt.Errorf("read the decision: %w", err)
	}
	if d.Approval != ApprovalNone && d.Approval != ApprovalOne && d.Approval != ApprovalTwo {
		return Decision{}, errors.New("the policy gave an unknown approval level")
	}
	return d, nil
}

// buildInput makes the input document of the policy.
func buildInput(p Principal, c ToolCall) (map[string]any, error) {
	raw, err := json.Marshal(c.Effect)
	if err != nil {
		return nil, err
	}
	effect := map[string]any{}
	if err := json.Unmarshal(raw, &effect); err != nil {
		return nil, fmt.Errorf("the effect is not an object: %w", err)
	}
	effect["amount_cents"] = c.AmountCents
	return toGeneric(map[string]any{
		"principal":        p,
		"tool":             c.Tool,
		"effect":           effect,
		"partner":          c.Partner,
		"daily_auto_cents": c.DailyAutoCents,
		"document":         c.Document,
		"supplier_order":   c.SupplierOrder,
	})
}

// toGeneric turns structs into plain maps and slices, which is what OPA reads.
func toGeneric(v any) (map[string]any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	return out, json.Unmarshal(raw, &out)
}

// maxCents is the largest amount that stays exact in the policy engine (2^53).
const maxCents = int64(1) << 53

// Cents reads a decimal string with at most two places ("12", "12.5", "12.50")
// into whole cents. ok is false for anything else. An amount above the exact
// range of the engine is cut to maxCents, which is above any limit.
func Cents(s string) (int64, bool) {
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" || len(frac) > 2 || strings.Trim(whole+frac, "0123456789") != "" {
		return 0, false
	}
	frac += strings.Repeat("0", 2-len(frac))
	var n int64
	for _, ch := range whole + frac {
		n = n*10 + int64(ch-'0')
		if n >= maxCents || n < 0 {
			return maxCents, true
		}
	}
	return n, true
}

// InvoiceLines gives the lines of an invoice or of an order to a supplier in the
// normal form the policy compares: [sku, quantity in thousandths, unit price in
// cents] per line, merged by (sku, price) and sorted. Both sides are built by this
// one function, so the policy only tests equality.
func InvoiceLines(lines []NormLine) [][]string {
	type k struct {
		sku   string
		price int64
	}
	sum := map[k]int64{}
	for _, l := range lines {
		sum[k{l.SKU, l.PriceCents}] += l.QtyMilli
	}
	out := make([][]string, 0, len(sum))
	for key, q := range sum {
		out = append(out, []string{key.sku, fmt.Sprint(q), fmt.Sprint(key.price)})
	}
	sort.Slice(out, func(i, j int) bool { return slices.Compare(out[i], out[j]) < 0 })
	return out
}

// NormLine is one line before it is put in the normal form.
type NormLine struct {
	SKU        string
	QtyMilli   int64
	PriceCents int64
}
