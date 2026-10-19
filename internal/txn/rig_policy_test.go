package txn_test

// Helpers of the tests of wave 2-6 (policies, approvals, supplier invoices): a
// supplier with orders and invoices, operator calls over /ops/, and cloned documents.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SaintsFlow/edigate/internal/store"
)

const supplierID = "ZZ:ZZTESTVENDOR"

// supplier registers a supplier partner of the tenant and returns its token.
func (r *rig) supplier(tenant string) (id, token string) {
	r.t.Helper()
	id = supplierID
	r.must(r.st.AddPartner(context.Background(), store.Partner{TenantID: tenant, PartnerID: id, Role: "supplier", Name: id}))
	_, token = r.token(tenant, "partner", id)
	return id, token
}

// supplierOrder stores an order to the supplier: one line SKU-1, quantity 10, price 3.00.
func (r *rig) supplierOrder(tenant, number, total string) {
	r.t.Helper()
	r.exec(`INSERT INTO supplier_orders (tenant_id, partner_id, order_number, lines, total, currency)
	        VALUES ($1, $2, $3, '[{"sku":"SKU-1","qty":"10","unit_price":"3.00"}]', $4::numeric, 'USD')`,
		tenant, supplierID, number, total)
}

// invoiceLine is a line of an 810: SKU-1, quantity, price.
type invoiceLine struct{ sku, qty, price string }

// send810 posts an invoice of the supplier and returns the id of the stored document.
// tds is the TDS01 value (the total in cents).
func (r *rig) send810(tenant, partnerID, token, isa13, gs06, big02, big04, tds string, lines ...invoiceLine) string {
	r.t.Helper()
	pad := func(id string) string {
		name := strings.TrimPrefix(id, "ZZ:")
		return name + strings.Repeat(" ", 15-len(name))
	}
	body := "BIG*20261018*" + big02 + "*20261018*" + big04 + "~CUR*SE*USD~N1*BY*TEST BUYER*92*B001~"
	for i, l := range lines {
		body += fmt.Sprintf("IT1*%d*%s*EA*%s**VN*%s~", i+1, l.qty, l.price, l.sku)
	}
	body += "TDS*" + tds + "~CTT*" + strconv.Itoa(len(lines)) + "~"
	segments := strings.Count(body, "~") + 2
	raw := "ISA*00*          *00*          *ZZ*" + pad(partnerID) + "*ZZ*" + pad(gateways[tenant]) + "*261018*1200*U*00401*" + isa13 + "*0*T*>~" +
		"GS*IN*" + strings.TrimPrefix(partnerID, "ZZ:") + "*" + strings.TrimPrefix(gateways[tenant], "ZZ:") + "*20261018*1200*" + gs06 + "*X*004010~" +
		"ST*810*0001~" + body + "SE*" + strconv.Itoa(segments) + "*0001~GE*1*" + gs06 + "~IEA*1*" + isa13 + "~"

	if code, answer := r.postInbound(raw, tenant, partnerID, token, isa13); code != http.StatusOK {
		r.t.Fatalf("send 810: HTTP %d: %s", code, answer)
	}
	return r.text(`SELECT d.id::text FROM edi_documents d JOIN edi_interchanges i ON i.id = d.interchange_id
	               WHERE d.tenant_id = $1 AND i.isa13 = $2 AND i.counterparty_id = $3`, tenant, isa13, partnerID)
}

// postInbound posts an interchange to /edi/inbound. The entrance allows a partner two
// posts per second; a post that was refused for that is repeated after a pause, since
// nothing of it was processed.
func (r *rig) postInbound(raw, tenant, partnerID, token, isa13 string) (int, []byte) {
	r.t.Helper()
	for range 20 {
		req, err := http.NewRequest(http.MethodPost, r.url+"/edi/inbound", bytes.NewReader([]byte(raw)))
		r.must(err)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("AS2-From", partnerID)
		req.Header.Set("AS2-To", gateways[tenant])
		req.Header.Set("Message-Id", "<"+isa13+"@test>")
		resp, err := http.DefaultClient.Do(req)
		r.must(err)
		answer, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusTooManyRequests {
			return resp.StatusCode, answer
		}
		time.Sleep(300 * time.Millisecond)
	}
	r.t.Fatal("the inbound entrance kept answering 429")
	return 0, nil
}

// invoice calls erp.post_vendor_invoice and decodes the answer.
func (r *rig) invoice(cs *mcp.ClientSession, docID string) (answer, *jsonrpc.Error) {
	r.t.Helper()
	raw, wire := r.call(cs, "erp.post_vendor_invoice", map[string]any{"source_document_id": docID})
	if wire != nil {
		return answer{}, wire
	}
	var a answer
	r.must(json.Unmarshal(raw, &a))
	return a, nil
}

// ops calls /ops/ with the token of an operator and returns the HTTP status and the body.
func (r *rig) ops(token, method, path string) (int, []byte) {
	r.t.Helper()
	req, err := http.NewRequest(method, r.url+path, nil)
	r.must(err)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	r.must(err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	r.must(err)
	return resp.StatusCode, body
}

// pending lists the pending requests the operator sees.
func (r *rig) pending(token string) []map[string]any {
	r.t.Helper()
	code, body := r.ops(token, http.MethodGet, "/ops/approvals?status=pending")
	if code != http.StatusOK {
		r.t.Fatalf("list approvals: HTTP %d: %s", code, body)
	}
	var out struct {
		Approvals []map[string]any `json:"approvals"`
	}
	r.must(json.Unmarshal(body, &out))
	return out.Approvals
}

// approvalOf returns the id of the only approval of a key (by txn_id).
func (r *rig) approvalOf(txnID string) string {
	r.t.Helper()
	return r.text(`SELECT a.id::text FROM approvals a JOIN idempotency_keys k ON k.key = a.idem_key
	               WHERE k.txn_id = $1::uuid AND a.kind = 'effect' ORDER BY a.requested_at DESC LIMIT 1`, txnID)
}

// cloneDocs makes n copies of a stored 850 with other business references
// (prefix-1 ... prefix-n), so that a test can order many times without posting many
// interchanges (the inbound entrance has a bucket per partner).
func (r *rig) cloneDocs(doc, prefix string, n int) []string {
	r.t.Helper()
	ids := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		ref := fmt.Sprintf("%s-%d", prefix, i)
		ids = append(ids, r.text(
			`INSERT INTO edi_documents (interchange_id, tenant_id, partner_id, gs06, st01, st02, business_ref, body, status, bk_ref)
			 SELECT interchange_id, tenant_id, partner_id, gs06, st01, $2::text, $3::text,
			        jsonb_set(body, '{header,business_ref}', to_jsonb($3::text)), 'received', $3::text
			 FROM edi_documents WHERE id = $1::uuid RETURNING id::text`, doc, strconv.Itoa(1000+i), ref))
	}
	return ids
}
