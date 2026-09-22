package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
)

// This file's commands are the ONLY way to manage v0.39 sending pools:
// operator-global infrastructure, CLI-only like create-tenant/
// create-api-key in apikeys.go, deliberately never exposed as a tenant-
// facing REST API (see .ilana/architecture.md's v0.39 section and DEC-###
// for why: a tenant must not gain the ability to pick arbitrary operator
// sending infrastructure merely because the schema exists). Pools/members
// support disable, never delete, in v0.39.

func cmdCreateSendingPool(args []string, output io.Writer) error {
	fs := flag.NewFlagSet("create-sending-pool", flag.ContinueOnError)
	name := fs.String("name", "", "pool name (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("usage: mailx create-sending-pool -name <name>")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := connectForAdmin(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	p, err := db.CreateSendingPool(ctx, *name)
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "pool_id: %s\nname: %s\nenabled: %t\n", p.ID, p.Name, p.Enabled)
	return nil
}

func cmdListSendingPools(args []string, output io.Writer) error {
	fs := flag.NewFlagSet("list-sending-pools", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := connectForAdmin(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	pools, err := db.ListSendingPools(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintln(output, "ID\tNAME\tENABLED\tCREATED_AT")
	for _, p := range pools {
		fmt.Fprintf(output, "%s\t%s\t%t\t%s\n", p.ID, p.Name, p.Enabled, p.CreatedAt.Format(time.RFC3339))
	}
	return nil
}

func cmdSetSendingPoolEnabled(args []string, output io.Writer, enabled bool) error {
	name := "enable-sending-pool"
	if !enabled {
		name = "disable-sending-pool"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	id := fs.String("id", "", "pool id (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return fmt.Errorf("usage: mailx %s -id <pool-id>", name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := connectForAdmin(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := db.SetSendingPoolEnabled(ctx, *id, enabled); err != nil {
		return err
	}
	fmt.Fprintf(output, "pool_id: %s\nenabled: %t\n", *id, enabled)
	return nil
}

func cmdCreateSendingPoolMember(args []string, output io.Writer) error {
	fs := flag.NewFlagSet("create-sending-pool-member", flag.ContinueOnError)
	poolID := fs.String("pool", "", "owning pool id (required)")
	kind := fs.String("kind", "", "member kind: direct or relay (required)")
	hostname := fs.String("hostname", "", "EHLO/PTR hostname (direct members only)")
	sourceIP := fs.String("source-ip", "", "outbound source IP to bind (direct members only, optional)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *poolID == "" || *kind == "" {
		return fmt.Errorf("usage: mailx create-sending-pool-member -pool <pool-id> -kind direct|relay [-hostname <host>] [-source-ip <ip>]")
	}
	var hostnamePtr, sourceIPPtr *string
	if *hostname != "" {
		hostnamePtr = hostname
	}
	if *sourceIP != "" {
		sourceIPPtr = sourceIP
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := connectForAdmin(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	m, err := db.CreateSendingPoolMember(ctx, *poolID, database.SendingPoolMemberKind(*kind), hostnamePtr, sourceIPPtr)
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "member_id: %s\npool_id: %s\nkind: %s\nenabled: %t\n", m.ID, m.PoolID, m.Kind, m.Enabled)
	return nil
}

func cmdListSendingPoolMembers(args []string, output io.Writer) error {
	fs := flag.NewFlagSet("list-sending-pool-members", flag.ContinueOnError)
	poolID := fs.String("pool", "", "pool id (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *poolID == "" {
		return fmt.Errorf("usage: mailx list-sending-pool-members -pool <pool-id>")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := connectForAdmin(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	members, err := db.ListSendingPoolMembers(ctx, *poolID)
	if err != nil {
		return err
	}
	fmt.Fprintln(output, "ID\tKIND\tHOSTNAME\tSOURCE_IP\tENABLED")
	for _, m := range members {
		hostname, sourceIP := "", ""
		if m.Hostname != nil {
			hostname = *m.Hostname
		}
		if m.SourceIP != nil {
			sourceIP = *m.SourceIP
		}
		fmt.Fprintf(output, "%s\t%s\t%s\t%s\t%t\n", m.ID, m.Kind, hostname, sourceIP, m.Enabled)
	}
	return nil
}

func cmdSetSendingPoolMemberEnabled(args []string, output io.Writer, enabled bool) error {
	name := "enable-sending-pool-member"
	if !enabled {
		name = "disable-sending-pool-member"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	id := fs.String("id", "", "member id (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return fmt.Errorf("usage: mailx %s -id <member-id>", name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := connectForAdmin(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := db.SetSendingPoolMemberEnabled(ctx, *id, enabled); err != nil {
		return err
	}
	fmt.Fprintf(output, "member_id: %s\nenabled: %t\n", *id, enabled)
	return nil
}

// cmdAssignDomainPool is the ONLY way to set domains.sending_pool_id
// (operator-controlled infrastructure policy, never a tenant-writable
// domain field — see database.AssignDomainPool's doc and this file's
// package comment). -pool "" clears the assignment, returning the domain
// to legacy default routing.
func cmdAssignDomainPool(args []string, output io.Writer) error {
	fs := flag.NewFlagSet("assign-domain-pool", flag.ContinueOnError)
	domainID := fs.String("domain", "", "domain id (required)")
	poolID := fs.String("pool", "", "pool id to assign, or empty to clear the assignment")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *domainID == "" {
		return fmt.Errorf("usage: mailx assign-domain-pool -domain <domain-id> [-pool <pool-id>]")
	}
	var poolArg *string
	if *poolID != "" {
		poolArg = poolID
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := connectForAdmin(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := db.AssignDomainPool(ctx, *domainID, poolArg); err != nil {
		return err
	}
	assigned := "(cleared)"
	if poolArg != nil {
		assigned = *poolArg
	}
	fmt.Fprintf(output, "domain_id: %s\nsending_pool_id: %s\n", *domainID, assigned)
	return nil
}
