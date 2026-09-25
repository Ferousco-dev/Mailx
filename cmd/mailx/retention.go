package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/storage"
)

// v0.46 data retention and GDPR subject requests are deliberately
// CLI-only, matching v0.19's precedent for tenant/key management (see
// apikeys.go's doc): no self-service endpoint for an operation this
// consequential (bulk/irreversible data deletion) in this milestone.

func cmdSetRetention(args []string, output io.Writer) error {
	fs := flag.NewFlagSet("set-retention", flag.ContinueOnError)
	tenantID := fs.String("tenant", "", "tenant id (required)")
	days := fs.String("days", "", `retention window in days, or "default" to clear back to the platform default (required)`)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *tenantID == "" || *days == "" {
		return fmt.Errorf(`usage: mailx set-retention -tenant <id> -days <n|default>`)
	}
	var daysPtr *int
	if *days != "default" {
		n, err := strconv.Atoi(*days)
		if err != nil || n <= 0 {
			return fmt.Errorf("-days must be a positive integer or \"default\", got %q", *days)
		}
		daysPtr = &n
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := connectForAdmin(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := db.SetTenantRetention(ctx, *tenantID, daysPtr); err != nil {
		return err
	}
	if daysPtr == nil {
		fmt.Fprintf(output, "tenant %s retention reset to platform default (%d days)\n", *tenantID, database.DefaultRetentionDays)
	} else {
		fmt.Fprintf(output, "tenant %s retention set to %d days\n", *tenantID, *daysPtr)
	}
	return nil
}

func cmdShowRetention(args []string, output io.Writer) error {
	fs := flag.NewFlagSet("show-retention", flag.ContinueOnError)
	tenantID := fs.String("tenant", "", "tenant id (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *tenantID == "" {
		return fmt.Errorf("usage: mailx show-retention -tenant <id>")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := connectForAdmin(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	t, err := db.GetTenant(ctx, *tenantID)
	if err != nil {
		return err
	}
	if t.RetentionDays == nil {
		fmt.Fprintf(output, "tenant %s: using platform default (%d days)\n", t.ID, database.DefaultRetentionDays)
	} else {
		fmt.Fprintf(output, "tenant %s: %d days\n", t.ID, *t.RetentionDays)
	}
	return nil
}

func cmdPurgeExpired(args []string, output io.Writer) error {
	fs := flag.NewFlagSet("purge-expired", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := connectForAdmin(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	store, err := storage.NewFileStore(storageRoot())
	if err != nil {
		return err
	}

	var diskErrs int
	// deleteDisk runs BEFORE each id's database row is deleted (see
	// database.PurgeExpiredMessages' doc) - an id whose disk delete fails
	// keeps its DB row this run and is retried, disk and DB together, on
	// the next purge-expired invocation.
	deleteDisk := func(id string) error {
		if err := store.Delete(id); err != nil {
			fmt.Fprintf(output, "warning: message %s failed to delete from disk, left in place for retry: %v\n", id, err)
			diskErrs++
			return err
		}
		return nil
	}
	ids, err := db.PurgeExpiredMessages(ctx, deleteDisk)
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "purged %d message(s)", len(ids))
	if diskErrs > 0 {
		fmt.Fprintf(output, " (%d disk cleanup warning(s), see above)", diskErrs)
	}
	fmt.Fprintln(output)
	return nil
}

func cmdGDPRExport(args []string, output io.Writer) error {
	fs := flag.NewFlagSet("gdpr-export", flag.ContinueOnError)
	tenantID := fs.String("tenant", "", "tenant id (required)")
	email := fs.String("email", "", "the recipient address to export (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *tenantID == "" || *email == "" {
		return fmt.Errorf("usage: mailx gdpr-export -tenant <id> -email <address>")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := connectForAdmin(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	data, err := db.ExportSubjectData(ctx, *tenantID, *email)
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "email: %s\n", data.Email)
	if data.Contact == nil {
		fmt.Fprintln(output, "contact: none")
	} else {
		fmt.Fprintf(output, "contact_id: %s\n", data.Contact.ID)
		fmt.Fprintf(output, "contact_name: %s\n", data.Contact.Name)
		fmt.Fprintf(output, "contact_created_at: %s\n", data.Contact.CreatedAt.Format(time.RFC3339))
		fmt.Fprintf(output, "contact_attributes: %d\n", len(data.Contact.Attributes))
		for k, v := range data.Contact.Attributes {
			fmt.Fprintf(output, "  - %s=%s\n", k, v)
		}
	}
	fmt.Fprintf(output, "audience_memberships: %d\n", len(data.AudienceID))
	for _, aid := range data.AudienceID {
		fmt.Fprintf(output, "  - %s\n", aid)
	}
	fmt.Fprintf(output, "recipient_records: %d\n", len(data.Recipients))
	for _, r := range data.Recipients {
		fmt.Fprintf(output, "  - message_id=%s address=%s status=%s created_at=%s\n", r.MessageID, r.Address, r.Status, r.CreatedAt.Format(time.RFC3339))
	}
	return nil
}

func cmdGDPRDelete(args []string, output io.Writer) error {
	fs := flag.NewFlagSet("gdpr-delete", flag.ContinueOnError)
	tenantID := fs.String("tenant", "", "tenant id (required)")
	email := fs.String("email", "", "the recipient address to erase (required)")
	confirm := fs.Bool("confirm", false, "required: acknowledges this is irreversible")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *tenantID == "" || *email == "" {
		return fmt.Errorf("usage: mailx gdpr-delete -tenant <id> -email <address> -confirm")
	}
	if !*confirm {
		return fmt.Errorf("refusing to run without -confirm: this permanently deletes the contact record and every recipient row for %s in tenant %s", *email, *tenantID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := connectForAdmin(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	result, err := db.DeleteSubjectData(ctx, *tenantID, *email)
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "contact_deleted: %v\n", result.ContactDeleted)
	fmt.Fprintf(output, "recipient_records_deleted: %d\n", result.RecipientsDeleted)
	fmt.Fprintf(output, "broadcast_recipient_records_deleted: %d\n", result.BroadcastRecipientsDeleted)
	return nil
}
