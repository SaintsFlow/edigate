package audit_test

// Tests of the audit chain and its anchors (controls AUD-01 and AUD-03 of the threat model):
// T-AUD-01 a changed row is found by `audit verify` with its number, also when the attacker
// takes the trigger off; T-AUD-03 a tail cut out together with its anchors in the database
// is found from the anchor file; the chain stays one line under parallel calls.

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SaintsFlow/edigate/internal/audit"
	"github.com/SaintsFlow/edigate/internal/store"
	"github.com/SaintsFlow/edigate/internal/store/storetest"
)

// fill appends n events of the tenant, with details of different shapes.
func fill(t *testing.T, s *store.Store, tenant string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		e := store.AuditEvent{
			TenantID: tenant, EventType: "tool_call", TraceID: fmt.Sprintf("trace-%d", i), PrincipalID: "agent-1",
			Tool: "edi.list_inbox", Outcome: "ok",
		}
		if i%3 == 0 {
			e.Detail = []byte(fmt.Sprintf(`{"b":%d,  "a":"x<y>&z","n":1.50}`, i))
			e.IdemKey = []byte{byte(i), 1, 2, 3}
		}
		if i%5 == 0 {
			e.PolicyExplain = []byte(`{"deny":["ship_to.unknown"],"fired":[]}`)
			e.PayloadSHA256 = make([]byte, 32)
		}
		if err := s.AppendAudit(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
}

// withoutTrigger runs f with the append-only triggers off, as an attacker with the rights of the
// database owner would (outside the threat model: this is the case the chain is for).
func withoutTrigger(t *testing.T, pool *pgxpool.Pool, f func()) {
	t.Helper()
	ctx := context.Background()
	for _, table := range []string{"audit_log", "audit_anchors"} {
		if _, err := pool.Exec(ctx, "ALTER TABLE "+table+" DISABLE TRIGGER USER"); err != nil {
			t.Fatal(err)
		}
	}
	f()
	for _, table := range []string{"audit_log", "audit_anchors"} {
		if _, err := pool.Exec(ctx, "ALTER TABLE "+table+" ENABLE TRIGGER USER"); err != nil {
			t.Fatal(err)
		}
	}
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func seqs(t *testing.T, pool *pgxpool.Pool, tenant string) []int64 {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT seq FROM audit_log WHERE tenant_id = $1 ORDER BY seq`, tenant)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var s int64
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func verify(t *testing.T, pool *pgxpool.Pool, o audit.Options) audit.Result {
	t.Helper()
	res, err := audit.Verify(context.Background(), pool, o)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// The canonical form must not depend on how the database keeps a JSON value.
func TestHashIgnoresTheFormOfJSON(t *testing.T) {
	base := audit.Row{Seq: 7, TenantID: "t", At: time.Date(2026, 10, 19, 12, 0, 0, 123456000, time.UTC),
		EventType: "e", TraceID: "tr", PrincipalID: "p", PrevHash: audit.ZeroHash()}
	a, b := base, base
	a.Detail = []byte(`{"b": 1, "a": ["x", {"d":2.0,"c":null}]}`)
	b.Detail = []byte(`{"a":["x",{"c":null,"d":2}],"b":1}`)
	ha, err := audit.Hash(a)
	if err != nil {
		t.Fatal(err)
	}
	hb, err := audit.Hash(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(ha) != string(hb) {
		t.Error("the same JSON in another form must give the same hash")
	}
	b.Detail = []byte(`{"a":["x",{"c":null,"d":3}],"b":1}`)
	if hb, _ = audit.Hash(b); string(ha) == string(hb) {
		t.Error("another value must give another hash")
	}
}

// T-AUD-01: the trigger refuses UPDATE and DELETE; a row changed behind the trigger's back
// (or one removed, or one rewritten together with its own hash) is found with its number.
func TestT_AUD_01_ChangedRowIsFoundByVerify(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		tamper func(t *testing.T, pool *pgxpool.Pool, target int64)
		want   func(all []int64, target int64) int64 // the seq that must be named
	}{
		"edited row": {
			tamper: func(t *testing.T, pool *pgxpool.Pool, target int64) {
				exec(t, pool, `UPDATE audit_log SET outcome = 'denied-by-nobody' WHERE seq = $1`, target)
			},
			want: func(_ []int64, target int64) int64 { return target },
		},
		"edited row with its hash recomputed by the attacker": {
			tamper: func(t *testing.T, pool *pgxpool.Pool, target int64) {
				exec(t, pool, `UPDATE audit_log SET outcome = 'denied-by-nobody', hash = sha256('forged') WHERE seq = $1`, target)
			},
			want: func(_ []int64, target int64) int64 { return target },
		},
		"deleted row": {
			tamper: func(t *testing.T, pool *pgxpool.Pool, target int64) {
				exec(t, pool, `DELETE FROM audit_log WHERE seq = $1`, target)
			},
			want: func(all []int64, target int64) int64 { // the row after the hole no longer follows its predecessor
				for i, s := range all {
					if s == target {
						return all[i+1]
					}
				}
				return -1
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, pool := storetest.New(t)
			fill(t, s, "t1", 20)
			fill(t, s, "t2", 5)
			all := seqs(t, pool, "t1")

			if res := verify(t, pool, audit.Options{Tenant: "t1"}); res.Failure != "" || res.Rows != 20 {
				t.Fatalf("before the change: %+v", res)
			}
			if _, err := pool.Exec(ctx, `UPDATE audit_log SET outcome = 'x'`); err == nil || !strings.Contains(err.Error(), "append-only") {
				t.Fatalf("UPDATE must fail with the trigger error, got %v", err)
			}
			if _, err := pool.Exec(ctx, `DELETE FROM audit_log`); err == nil || !strings.Contains(err.Error(), "append-only") {
				t.Fatalf("DELETE must fail with the trigger error, got %v", err)
			}

			target := all[9]
			withoutTrigger(t, pool, func() { tc.tamper(t, pool, target) })
			res := verify(t, pool, audit.Options{Tenant: "t1"})
			if want := fmt.Sprintf("broken at seq %d", tc.want(all, target)); !strings.HasPrefix(res.Failure, want) {
				t.Errorf("failure = %q, want it to start with %q", res.Failure, want)
			}
			if res := verify(t, pool, audit.Options{Tenant: "t2"}); res.Failure != "" || res.Rows != 5 {
				t.Errorf("another tenant must stay clean: %+v", res)
			}
		})
	}
}

// Rows before the change and a range after it still verify.
func TestVerifyRange(t *testing.T) {
	s, pool := storetest.New(t)
	fill(t, s, "t1", 20)
	all := seqs(t, pool, "t1")
	withoutTrigger(t, pool, func() {
		exec(t, pool, `UPDATE audit_log SET outcome = 'changed' WHERE seq = $1`, all[10])
	})
	if res := verify(t, pool, audit.Options{Tenant: "t1", To: all[9]}); res.Failure != "" || res.Rows != 10 {
		t.Errorf("rows before the change: %+v", res)
	}
	if res := verify(t, pool, audit.Options{Tenant: "t1", From: all[11]}); res.Failure != "" || res.Rows != 9 {
		t.Errorf("rows after the change: %+v", res)
	}
	if res := verify(t, pool, audit.Options{Tenant: "t1", From: all[5], To: all[15]}); !strings.HasPrefix(res.Failure, fmt.Sprintf("broken at seq %d", all[10])) {
		t.Errorf("a range with the change: %+v", res)
	}
}

// Fifty goroutines write to one tenant: the chain is one line, no row follows two others.
func TestChainStaysOneLineUnderParallelCalls(t *testing.T) {
	s, pool := storetest.New(t)
	ctx := context.Background()
	const workers, each = 50, 4
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				tenant := "t1"
				if w%10 == 0 {
					tenant = "t2"
				}
				err := s.AppendAudit(ctx, store.AuditEvent{TenantID: tenant, EventType: "tool_call", TraceID: fmt.Sprintf("w%d-%d", w, i), PrincipalID: "agent", Outcome: "ok"})
				if err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	for tenant, want := range map[string]int{"t1": 45 * each, "t2": 5 * each} {
		if res := verify(t, pool, audit.Options{Tenant: tenant}); res.Failure != "" || res.Rows != want {
			t.Errorf("%s: %+v, want %d rows in one chain", tenant, res, want)
		}
		var forks int
		err := pool.QueryRow(ctx, `SELECT count(*) FROM (SELECT prev_hash FROM audit_log WHERE tenant_id = $1 GROUP BY prev_hash HAVING count(*) > 1) x`, tenant).Scan(&forks)
		if err != nil || forks != 0 {
			t.Errorf("%s: %d rows share a predecessor (%v)", tenant, forks, err)
		}
	}
}

func newAnchorer(t *testing.T, dir string) (*audit.Anchorer, ed25519.PublicKey) {
	t.Helper()
	pubText, privText, err := audit.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	a, err := audit.NewAnchorer(privText, dir)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := audit.ParsePublicKey(pubText)
	if err != nil {
		t.Fatal(err)
	}
	return a, pub
}

// An anchor is made for every full 100 rows of a tenant and nothing before.
func TestAnchorsEvery100Rows(t *testing.T) {
	s, pool := storetest.New(t)
	ctx := context.Background()
	dir := t.TempDir()
	a, pub := newAnchorer(t, dir)

	fill(t, s, "t1", 99)
	fill(t, s, "t2", 30)
	if err := s.AnchorAuditNow(ctx, a); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_anchors`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("anchors after 99 rows = %d (%v)", n, err)
	}
	fill(t, s, "t1", 101)    // 200 rows
	for i := 0; i < 2; i++ { // a second pass changes nothing
		if err := s.AnchorAuditNow(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	all := seqs(t, pool, "t1")
	rows, err := pool.Query(ctx, `SELECT seq_upto FROM audit_anchors WHERE tenant_id = 't1' ORDER BY seq_upto`)
	if err != nil {
		t.Fatal(err)
	}
	var got []int64
	for rows.Next() {
		var s int64
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		got = append(got, s)
	}
	rows.Close()
	if len(got) != 2 || got[0] != all[99] || got[1] != all[199] {
		t.Fatalf("anchors at %v, want the 100th and 200th rows %d and %d", got, all[99], all[199])
	}
	if res := verify(t, pool, audit.Options{Tenant: "t1", AnchorDir: dir, PubKey: pub}); res.Failure != "" || res.Rows != 200 {
		t.Errorf("verify with the anchors: %+v", res)
	}
	data, err := os.ReadFile(audit.FilePath(dir, "t1"))
	if err != nil || strings.Count(string(data), "\n") != 2 {
		t.Errorf("anchor file: %q (%v)", data, err)
	}
	if _, err := os.Stat(audit.FilePath(dir, "t2")); err == nil {
		t.Error("a tenant with fewer than 100 rows must have no anchor file entries")
	}

	_, wrong, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	if res := verify(t, pool, audit.Options{Tenant: "t1", PubKey: wrong.Public().(ed25519.PublicKey)}); !strings.Contains(res.Failure, "bad signature") {
		t.Errorf("a wrong key must give a bad signature, got %+v", res)
	}
}

// T-AUD-03: the attacker removes the newest rows and their anchors in the database. The
// database alone shows nothing; the anchor file outside it does.
func TestT_AUD_03_CutTailIsFoundFromTheAnchorFile(t *testing.T) {
	s, pool := storetest.New(t)
	ctx := context.Background()
	dir := t.TempDir()
	a, pub := newAnchorer(t, dir)
	fill(t, s, "t1", 250)
	if err := s.AnchorAuditNow(ctx, a); err != nil {
		t.Fatal(err)
	}
	all := seqs(t, pool, "t1")
	first := all[99] // the anchor that stays
	withoutTrigger(t, pool, func() {
		exec(t, pool, `DELETE FROM audit_anchors WHERE tenant_id = 't1' AND seq_upto > $1`, first)
		exec(t, pool, `DELETE FROM audit_log WHERE tenant_id = 't1' AND seq > $1`, first)
	})

	res := verify(t, pool, audit.Options{Tenant: "t1", PubKey: pub})
	if res.Failure != "" || res.Rows != 100 {
		t.Fatalf("without the file the cut is invisible: %+v", res)
	}
	res = verify(t, pool, audit.Options{Tenant: "t1", PubKey: pub, AnchorDir: dir})
	if !strings.Contains(res.Failure, "shorter") || !strings.Contains(res.Failure, fmt.Sprint(all[199])) {
		t.Errorf("the cut must be found from the file and name seq_upto %d, got %q", all[199], res.Failure)
	}
}

// A cut that leaves the anchors of the database is found without the file; a file that was
// removed while the database has anchors is reported; a forged anchor in the file is reported.
func TestAnchorChecks(t *testing.T) {
	s, pool := storetest.New(t)
	ctx := context.Background()
	dir := t.TempDir()
	a, pub := newAnchorer(t, dir)
	fill(t, s, "t1", 120)
	if err := s.AnchorAuditNow(ctx, a); err != nil {
		t.Fatal(err)
	}
	all := seqs(t, pool, "t1")

	if res := verify(t, pool, audit.Options{Tenant: "t1", PubKey: pub, AnchorDir: t.TempDir()}); !strings.Contains(res.Failure, "missing") {
		t.Errorf("a missing anchor file: %+v", res)
	}
	path := audit.FilePath(dir, "t1")
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	forged := strings.Replace(string(good), `"seq_upto":`+fmt.Sprint(all[99]), `"seq_upto":`+fmt.Sprint(all[98]), 1)
	if err := os.WriteFile(path, []byte(forged), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := verify(t, pool, audit.Options{Tenant: "t1", PubKey: pub, AnchorDir: dir}); !strings.Contains(res.Failure, "bad signature") {
		t.Errorf("a forged anchor in the file: %+v", res)
	}
	if err := os.WriteFile(path, good, 0o644); err != nil {
		t.Fatal(err)
	}

	withoutTrigger(t, pool, func() {
		exec(t, pool, `DELETE FROM audit_log WHERE tenant_id = 't1' AND seq > $1`, all[50])
	})
	if res := verify(t, pool, audit.Options{Tenant: "t1", PubKey: pub}); !strings.Contains(res.Failure, "shorter") {
		t.Errorf("a cut that leaves the anchor of the database: %+v", res)
	}
}

// Two writers at once (two gateway processes) leave one line per anchor in the file.
func TestTwoAnchorWritersLeaveNoDuplicates(t *testing.T) {
	s, _ := storetest.New(t)
	ctx := context.Background()
	dir := t.TempDir()
	a, _ := newAnchorer(t, dir)
	fill(t, s, "t1", 300)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.AnchorAuditNow(ctx, a); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	data, err := os.ReadFile(audit.FilePath(dir, "t1"))
	if err != nil || strings.Count(string(data), "\n") != 3 {
		t.Errorf("anchor file: %q (%v), want 3 lines", data, err)
	}
}
