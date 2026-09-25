package humanauth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
)

func extractInviteToken(t *testing.T, text string) string {
	t.Helper()
	idx := strings.Index(text, "token=")
	if idx == -1 {
		t.Fatalf("no token= in invite email body: %q", text)
	}
	raw := text[idx+len("token="):]
	if end := strings.IndexAny(raw, "\n "); end != -1 {
		raw = raw[:end]
	}
	return raw
}

func TestInviteOwnerOnly(t *testing.T) {
	db := newTestDB(t)
	mailer := &fakeMailer{}
	svc, err := NewService(db, testSecret(), WithMailer(mailer), WithDashboardBaseURL("https://app.mailx.dev"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	owner, err := svc.SignUp(ctx, "Ada", "ada@example.com", "hunter22hunter")
	if err != nil {
		t.Fatal(err)
	}
	tenant, err := svc.CreateOrganization(ctx, owner.Human.ID, "Acme Inc", "")
	if err != nil {
		t.Fatal(err)
	}
	other, err := svc.SignUp(ctx, "Grace", "grace@example.com", "hunter22hunter")
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.InviteToOrganization(ctx, other.Human.ID, tenant.ID, "invitee@example.com"); !errors.Is(err, ErrNotOrgOwner) {
		t.Fatalf("expected ErrNotOrgOwner for a non-member, got %v", err)
	}

	if err := svc.InviteToOrganization(ctx, owner.Human.ID, tenant.ID, "invitee@example.com"); err != nil {
		t.Fatalf("expected owner invite to succeed, got %v", err)
	}
	if len(mailer.calls) != 1 || mailer.calls[0].to != "invitee@example.com" {
		t.Fatalf("expected exactly one invite email to invitee@example.com, got %+v", mailer.calls)
	}
	if !strings.Contains(mailer.calls[0].subject, "Acme Inc") {
		t.Fatalf("expected org name in subject, got %q", mailer.calls[0].subject)
	}
}

func TestAcceptInviteExistingAccountMustMatchEmail(t *testing.T) {
	db := newTestDB(t)
	mailer := &fakeMailer{}
	svc, err := NewService(db, testSecret(), WithMailer(mailer), WithDashboardBaseURL("https://app.mailx.dev"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	owner, err := svc.SignUp(ctx, "Ada", "ada@example.com", "hunter22hunter")
	if err != nil {
		t.Fatal(err)
	}
	tenant, err := svc.CreateOrganization(ctx, owner.Human.ID, "Acme Inc", "")
	if err != nil {
		t.Fatal(err)
	}
	invitee, err := svc.SignUp(ctx, "Grace", "grace@example.com", "hunter22hunter")
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.InviteToOrganization(ctx, owner.Human.ID, tenant.ID, "invitee@example.com"); err != nil {
		t.Fatal(err)
	}
	raw := extractInviteToken(t, mailer.calls[0].text)

	// Wrong account (Grace's email doesn't match the invitation's).
	if _, err := svc.AcceptOrgInvitation(ctx, raw, invitee.Human.ID, "", ""); !errors.Is(err, ErrOrgInvitationEmailMismatch) {
		t.Fatalf("expected ErrOrgInvitationEmailMismatch, got %v", err)
	}

	// Right account: sign up as the actual invitee, then accept.
	realInvitee, err := svc.SignUp(ctx, "Invitee", "invitee@example.com", "hunter22hunter")
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.AcceptOrgInvitation(ctx, raw, realInvitee.Human.ID, "", "")
	if err != nil {
		t.Fatalf("expected accept to succeed for the matching account, got %v", err)
	}
	if result.Session != nil {
		t.Fatal("expected no new session for an already-logged-in caller")
	}
	if result.Tenant.ID != tenant.ID {
		t.Fatalf("unexpected tenant: %+v", result.Tenant)
	}
	orgs, err := svc.ListOrganizationsForHuman(ctx, realInvitee.Human.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(orgs) != 1 || orgs[0].ID != tenant.ID {
		t.Fatalf("expected invitee to have joined the org, got %+v", orgs)
	}

	// Re-accepting the same (now-consumed) token must fail, never double-join.
	if _, err := svc.AcceptOrgInvitation(ctx, raw, realInvitee.Human.ID, "", ""); !errors.Is(err, ErrOrgInvitationInvalid) {
		t.Fatalf("expected ErrOrgInvitationInvalid on reuse, got %v", err)
	}
}

func TestAcceptInviteSignsUpUnknownInvitee(t *testing.T) {
	db := newTestDB(t)
	mailer := &fakeMailer{}
	svc, err := NewService(db, testSecret(), WithMailer(mailer), WithDashboardBaseURL("https://app.mailx.dev"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	owner, err := svc.SignUp(ctx, "Ada", "ada@example.com", "hunter22hunter")
	if err != nil {
		t.Fatal(err)
	}
	tenant, err := svc.CreateOrganization(ctx, owner.Human.ID, "Acme Inc", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.InviteToOrganization(ctx, owner.Human.ID, tenant.ID, "newbie@example.com"); err != nil {
		t.Fatal(err)
	}
	raw := extractInviteToken(t, mailer.calls[0].text)

	// No existing session (existingHumanID == ""): must sign up, using the
	// INVITATION's email, not anything client-supplied.
	result, err := svc.AcceptOrgInvitation(ctx, raw, "", "Newbie", "hunter22hunter")
	if err != nil {
		t.Fatalf("expected combined signup+accept to succeed, got %v", err)
	}
	if result.Session == nil {
		t.Fatal("expected a fresh session for a newly created account")
	}
	if result.Session.Human.Email != "newbie@example.com" {
		t.Fatalf("expected account email to be the invitation's own address, got %q", result.Session.Human.Email)
	}
	orgs, err := svc.ListOrganizationsForHuman(ctx, result.Session.Human.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(orgs) != 1 || orgs[0].ID != tenant.ID {
		t.Fatalf("expected the new account to have joined the org, got %+v", orgs)
	}

	// Short password must be rejected before any account is created.
	if err := svc.InviteToOrganization(ctx, owner.Human.ID, tenant.ID, "second@example.com"); err != nil {
		t.Fatal(err)
	}
	raw2 := extractInviteToken(t, mailer.calls[1].text)
	if _, err := svc.AcceptOrgInvitation(ctx, raw2, "", "Someone", "short"); err == nil {
		t.Fatal("expected a short password to be rejected")
	}
}

func TestAcceptInviteExpiredToken(t *testing.T) {
	db := newTestDB(t)
	mailer := &fakeMailer{}
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	current := start
	svc, err := NewService(db, testSecret(), WithMailer(mailer), WithDashboardBaseURL("https://app.mailx.dev"),
		WithNow(func() time.Time { return current }))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	owner, err := svc.SignUp(ctx, "Ada", "ada@example.com", "hunter22hunter")
	if err != nil {
		t.Fatal(err)
	}
	tenant, err := svc.CreateOrganization(ctx, owner.Human.ID, "Acme Inc", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.InviteToOrganization(ctx, owner.Human.ID, tenant.ID, "invitee@example.com"); err != nil {
		t.Fatal(err)
	}
	raw := extractInviteToken(t, mailer.calls[0].text)

	current = start.Add(OrgInvitationTTL + time.Second)
	if _, err := svc.AcceptOrgInvitation(ctx, raw, "", "Invitee", "hunter22hunter"); !errors.Is(err, ErrOrgInvitationInvalid) {
		t.Fatalf("expected ErrOrgInvitationInvalid for an expired invitation, got %v", err)
	}
}

func TestAcceptInviteEmailAlreadyTaken(t *testing.T) {
	db := newTestDB(t)
	mailer := &fakeMailer{}
	svc, err := NewService(db, testSecret(), WithMailer(mailer), WithDashboardBaseURL("https://app.mailx.dev"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	owner, err := svc.SignUp(ctx, "Ada", "ada@example.com", "hunter22hunter")
	if err != nil {
		t.Fatal(err)
	}
	tenant, err := svc.CreateOrganization(ctx, owner.Human.ID, "Acme Inc", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.InviteToOrganization(ctx, owner.Human.ID, tenant.ID, "taken@example.com"); err != nil {
		t.Fatal(err)
	}
	raw := extractInviteToken(t, mailer.calls[0].text)

	// The invitation's own email gets registered by some other means
	// before the invite is accepted (e.g. a separate signup).
	if _, err := svc.SignUp(ctx, "Someone Else", "taken@example.com", "hunter22hunter"); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.AcceptOrgInvitation(ctx, raw, "", "Taken", "hunter22hunter"); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("expected ErrEmailTaken, got %v", err)
	}

	// The invitation must NOT be consumed by the failed attempt above —
	// database.AcceptOrgInvitationWithSignup runs the email-uniqueness
	// conflict and the accepted_at update in the same transaction, so a
	// conflict rolls back the whole thing, leaving the token still usable
	// (e.g. by the account holder logging in and accepting normally).
	var invited database.OrgInvitation
	invited, err = db.GetOrgInvitationByHash(ctx, hashRawToken(raw))
	if err != nil {
		t.Fatal(err)
	}
	if invited.AcceptedAt != nil {
		t.Fatal("expected the invitation to remain unconsumed after a rolled-back signup conflict")
	}
}
