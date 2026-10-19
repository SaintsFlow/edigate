package store_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/SaintsFlow/edigate/internal/adapters"
	"github.com/SaintsFlow/edigate/internal/edi/outbound"
	"github.com/SaintsFlow/edigate/internal/store"
	"github.com/SaintsFlow/edigate/internal/store/storetest"
	"github.com/SaintsFlow/edigate/internal/txn"
)

// T-ACK-06: a thousand documents made at the same time by many goroutines get a thousand
// different sets of control numbers: the ISA13 values differ, the GS06 values differ and
// the ST02 values differ (invariant I7). Every document has its own interchange row.
func TestT_ACK_06_ThousandDocumentsGetThousandSetsOfNumbers(t *testing.T) {
	st, pool := storetest.New(t)
	ctx := context.Background()
	const partner = "ZZ:ZZTESTBUYER"
	if err := st.AddTenant(ctx, "ta", "ta", "ZZ:ZZTESTSUPPL"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddPartner(ctx, store.Partner{TenantID: "ta", PartnerID: partner, Role: "customer", Name: partner}); err != nil {
		t.Fatal(err)
	}

	const workers, each = 20, 50
	errs := make(chan error, workers*each)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				errs <- reserveDocument(ctx, st, partner, fmt.Sprintf("PO-%d-%d", w, i))
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	var docs, isa, gs, stn, interchanges int
	if err := pool.QueryRow(ctx,
		`SELECT count(*), count(DISTINCT isa13), count(DISTINCT gs06), count(DISTINCT st02) FROM outbound_docs`).
		Scan(&docs, &isa, &gs, &stn); err != nil {
		t.Fatal(err)
	}
	if docs != workers*each || isa != docs || gs != docs || stn != docs {
		t.Fatalf("documents %d, different ISA13 %d, GS06 %d, ST02 %d; want %d of each", docs, isa, gs, stn, workers*each)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM edi_interchanges WHERE direction = 'out'`).Scan(&interchanges); err != nil {
		t.Fatal(err)
	}
	if interchanges != docs {
		t.Errorf("outgoing interchanges = %d, want %d", interchanges, docs)
	}
	// The numbers in the row are the numbers in the bytes.
	var raw []byte
	var isa13, gs06, st02 string
	if err := pool.QueryRow(ctx,
		`SELECT i.raw, d.isa13, d.gs06, d.st02 FROM outbound_docs d JOIN edi_interchanges i ON i.isa13 = d.isa13 AND i.direction = 'out' LIMIT 1`).
		Scan(&raw, &isa13, &gs06, &st02); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"*" + isa13 + "*", "*" + gs06 + "*X*004010~", "ST*855*" + st02 + "~"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the bytes lack %q", want)
		}
	}
}

// reserveDocument queues one 855 the way a tool does, without the policy.
func reserveDocument(ctx context.Context, st *store.Store, partner, ref string) error {
	spec := outbound.Spec{
		ST01: "855", PartnerID: partner, Ref: ref, OrderDate: "20261018", Currency: "USD", AckType: "AC",
		Lines: []outbound.Line{{SKU: "SKU-1", Qty: "1.000", Unit: "EA", UnitPrice: "1.00", Status: "IA"}},
	}
	payload, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		return err
	}
	bk, err := txn.BusinessKeyJSON(partner, ref)
	if err != nil {
		return err
	}
	hash, err := txn.ReqHash(spec)
	if err != nil {
		return err
	}
	res, err := st.Reserve(ctx, store.ReserveInput{
		TenantID: "ta", Tool: "erp.confirm_order", PrincipalID: "test", TraceID: "test",
		DocumentID: "11111111-1111-4111-8111-111111111111", Key: key[:], BusinessKey: bk, ReqHash: hash,
		EffectType: adapters.EffectSend855, Payload: payload,
	})
	if err != nil {
		return err
	}
	if res.Outcome != store.ReservedNew {
		return fmt.Errorf("outcome %s", res.Outcome)
	}
	return nil
}
