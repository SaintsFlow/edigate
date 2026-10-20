package main

// The commands of the audit chain (controls AUD-01 to AUD-03):
//
//	audit keygen --out <file>
//	audit verify --tenant T [--from N --to M] [--anchor-dir D] [--pubkey K]
//
// keygen makes an ed25519 key pair: the private key goes to the file (mode 0600, never
// overwritten) and is what EDIGATE_AUDIT_SIGNING_KEY holds; the public key is printed and is
// what --pubkey takes. verify reads EDIGATE_DB_URL, checks the hash chain of the tenant and
// the signed anchors of the database and of the anchor file (--anchor-dir, or
// EDIGATE_AUDIT_ANCHOR_DIR). It prints "ok <rows>" and exits 0, or says what is wrong
// ("broken at seq N", an anchor that does not hold, a tail shorter than the file) and exits 1.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/SaintsFlow/edigate/internal/audit"
	"github.com/SaintsFlow/edigate/internal/store"
)

func auditCommand(ctx context.Context, args []string, out io.Writer) error {
	const form = "audit: expected: keygen --out FILE | verify --tenant T [--from N --to M] [--anchor-dir D] [--pubkey K]"
	if len(args) == 0 {
		return usageError{form}
	}
	switch args[0] {
	case "keygen":
		return auditKeygen(args[1:], out)
	case "verify":
		return auditVerify(ctx, args[1:], out)
	}
	return usageError{form}
}

func auditKeygen(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("audit keygen", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	path := fs.String("out", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *path == "" {
		return usageError{"audit keygen: --out FILE is required"}
	}
	pub, priv, err := audit.GenerateKey()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(*path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("the key file: %w", err)
	}
	if _, err := f.WriteString(priv + "\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Fprintln(out, pub)
	return nil
}

func auditVerify(ctx context.Context, args []string, out io.Writer) error {
	const form = "audit verify: --tenant T [--from N --to M] [--anchor-dir D] [--pubkey K]"
	fs := flag.NewFlagSet("audit verify", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	tenant := fs.String("tenant", "", "")
	from := fs.Int64("from", 0, "")
	to := fs.Int64("to", 0, "")
	dir := fs.String("anchor-dir", os.Getenv("EDIGATE_AUDIT_ANCHOR_DIR"), "")
	pubText := fs.String("pubkey", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *tenant == "" || *from < 0 || *to < 0 || (*to != 0 && *to < *from) {
		return usageError{form}
	}
	opts := audit.Options{Tenant: *tenant, From: *from, To: *to, AnchorDir: *dir}
	if *pubText != "" {
		pub, err := audit.ParsePublicKey(*pubText)
		if err != nil {
			return usageError{"audit verify: --pubkey: " + err.Error()}
		}
		opts.PubKey = pub
	}
	return withStore(ctx, func(st *store.Store) error {
		res, err := st.VerifyAudit(ctx, opts)
		if err != nil {
			return err
		}
		if res.Failure != "" {
			fmt.Fprintln(out, res.Failure)
			return errors.New("the audit log does not verify")
		}
		fmt.Fprintf(out, "ok %d\n", res.Rows)
		return nil
	})
}
