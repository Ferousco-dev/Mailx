package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Ferousco-dev/mailx/internal/auth"
	"github.com/Ferousco-dev/mailx/internal/database"
)

// API-key lifecycle management is deliberately CLI-only, not a public
// REST endpoint: v0.19 has no human-authenticated management surface yet
// (that is a future dashboard/account milestone), and an unauthenticated
// POST /v1/api-keys would let anyone mint credentials. These commands
// need direct DATABASE_URL access, matching `mailx migrate` - an operator
// with that already has the same trust level these commands require.
// The future dashboard will own this lifecycle through its own
// human-authenticated backend, calling into internal/auth.Service exactly
// as these commands do, not by re-implementing it.

func connectForAdmin(ctx context.Context) (*database.DB, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return nil, fmt.Errorf("DATABASE_URL is not set")
	}
	return database.Open(ctx, database.Config{DSN: dsn})
}

func cmdCreateTenant(args []string, output io.Writer) error {
	fs := flag.NewFlagSet("create-tenant", flag.ContinueOnError)
	name := fs.String("name", "", "tenant name (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("usage: mailx create-tenant -name <name>")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := connectForAdmin(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	tenant, err := db.CreateTenant(ctx, *name)
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "tenant_id: %s\nname: %s\n", tenant.ID, tenant.Name)
	return nil
}

func cmdCreateAPIKey(args []string, output io.Writer) error {
	fs := flag.NewFlagSet("create-api-key", flag.ContinueOnError)
	tenantID := fs.String("tenant", "", "owning tenant id (required)")
	name := fs.String("name", "", "human-readable key name (required)")
	scopes := fs.String("scopes", "", "comma-separated scopes, e.g. emails:send,emails:read,domains:read,domains:write,webhooks:read,webhooks:write (required)")
	ttl := fs.Duration("ttl", 0, "optional expiration, e.g. 720h (0 = never expires)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *tenantID == "" || *name == "" || *scopes == "" {
		return fmt.Errorf("usage: mailx create-api-key -tenant <id> -name <name> -scopes emails:send,emails:read,domains:read,domains:write,webhooks:read,webhooks:write [-ttl 720h]")
	}
	if *ttl < 0 {
		return fmt.Errorf("-ttl must not be negative (0 means never expires, got %s)", *ttl)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := connectForAdmin(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	svc := auth.NewService(db, apiKeyPepper())
	var ttlPtr *time.Duration
	if *ttl > 0 {
		ttlPtr = ttl
	}
	gen, record, err := svc.Create(ctx, *tenantID, *name, splitScopes(*scopes), ttlPtr)
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "key: %s\n", gen.Raw)
	fmt.Fprintf(output, "key_id: %s\n", record.KeyID)
	fmt.Fprintf(output, "name: %s\n", record.Name)
	fmt.Fprintf(output, "scopes: %s\n", strings.Join(record.Scopes, ","))
	fmt.Fprintln(output, "This is the ONLY time the raw key is shown. Store it now — MailX cannot recover it later.")
	return nil
}

func cmdRotateAPIKey(args []string, output io.Writer) error {
	fs := flag.NewFlagSet("rotate-api-key", flag.ContinueOnError)
	keyID := fs.String("key-id", "", "key_id of the key to rotate (required)")
	grace := fs.Duration("grace", 0, "how long the old key stays valid after rotation (0 = invalidate immediately)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *keyID == "" {
		return fmt.Errorf("usage: mailx rotate-api-key -key-id <key_id> [-grace 1h]")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := connectForAdmin(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	svc := auth.NewService(db, apiKeyPepper())
	gen, record, err := svc.Rotate(ctx, *keyID, *grace)
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "new key: %s\n", gen.Raw)
	fmt.Fprintf(output, "new key_id: %s\n", record.KeyID)
	if *grace > 0 {
		fmt.Fprintf(output, "old key %s remains valid for %s\n", *keyID, *grace)
	} else {
		fmt.Fprintf(output, "old key %s is now invalid\n", *keyID)
	}
	fmt.Fprintln(output, "This is the ONLY time the new raw key is shown. Store it now.")
	return nil
}

func cmdRevokeAPIKey(args []string, output io.Writer) error {
	fs := flag.NewFlagSet("revoke-api-key", flag.ContinueOnError)
	keyID := fs.String("key-id", "", "key_id of the key to revoke (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *keyID == "" {
		return fmt.Errorf("usage: mailx revoke-api-key -key-id <key_id>")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := connectForAdmin(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	svc := auth.NewService(db, apiKeyPepper())
	if err := svc.Revoke(ctx, *keyID); err != nil {
		return err
	}
	fmt.Fprintf(output, "key %s revoked\n", *keyID)
	return nil
}

func cmdListAPIKeys(args []string, output io.Writer) error {
	fs := flag.NewFlagSet("list-api-keys", flag.ContinueOnError)
	tenantID := fs.String("tenant", "", "tenant id (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *tenantID == "" {
		return fmt.Errorf("usage: mailx list-api-keys -tenant <id>")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := connectForAdmin(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	svc := auth.NewService(db, apiKeyPepper())
	keys, err := svc.List(ctx, *tenantID)
	if err != nil {
		return err
	}
	fmt.Fprintln(output, "KEY_ID\tNAME\tSCOPES\tCREATED_AT\tLAST_USED_AT\tEXPIRES_AT\tREVOKED_AT")
	for _, k := range keys {
		fmt.Fprintf(output, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			k.KeyID, k.Name, strings.Join(k.Scopes, ","), k.CreatedAt.Format(time.RFC3339),
			formatTimePtr(k.LastUsedAt), formatTimePtr(k.ExpiresAt), formatTimePtr(k.RevokedAt))
	}
	return nil
}

func splitScopes(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func formatTimePtr(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.Format(time.RFC3339)
}
