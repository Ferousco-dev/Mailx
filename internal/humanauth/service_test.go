package humanauth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Ferousco-dev/mailx/internal/database"
)

// newTestDB mirrors internal/database's own testdb_test.go convention:
// isolated PostgreSQL schema per test, migrated fresh, dropped on cleanup.
func newTestDB(t testing.TB) *database.DB {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dsn := os.Getenv("MAILX_TEST_DATABASE_URL")
	if dsn == "" {
		user := os.Getenv("USER")
		dsn = fmt.Sprintf("postgres://%s@localhost:5432/mailx_test?sslmode=disable", user)
	}
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("no local PostgreSQL available for integration tests: %v", err)
	}
	if err := admin.Ping(ctx); err != nil {
		admin.Close()
		t.Skipf("no local PostgreSQL available for integration tests: %v", err)
	}

	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	schema := "humanauth_test_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ccancel()
		_, _ = admin.Exec(cctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
		admin.Close()
	})

	scopedDSN := dsn + "&search_path=" + schema
	db, err := database.Open(ctx, database.Config{DSN: scopedDSN, MaxConns: 4})
	if err != nil {
		t.Fatalf("open scoped db: %v", err)
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func testSecret() []byte { return []byte("test-secret-at-least-32-bytes-long!!") }

func TestSignUpAndDuplicateEmail(t *testing.T) {
	db := newTestDB(t)
	svc, err := NewService(db, testSecret())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sess, err := svc.SignUp(ctx, "Ada", "ada@example.com", "hunter22hunter")
	if err != nil {
		t.Fatal(err)
	}
	if sess.AccessToken == "" || sess.RefreshToken == "" {
		t.Fatal("expected tokens")
	}
	if _, err := svc.SignUp(ctx, "Ada2", "ada@example.com", "anotherpassword"); err != ErrEmailTaken {
		t.Fatalf("expected ErrEmailTaken, got %v", err)
	}
}

// TestLoginRecordsLastLoginButSignUpDoesNot proves TouchHumanLogin fires on
// Login (not SignUp, which mints a session directly but isn't itself a
// "login" for this field's purpose - see migration 000028's doc) and that
// a second login advances the timestamp.
func TestLoginRecordsLastLoginButSignUpDoesNot(t *testing.T) {
	db := newTestDB(t)
	svc, err := NewService(db, testSecret())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := svc.SignUp(ctx, "Ada", "ada@example.com", "hunter22hunter"); err != nil {
		t.Fatal(err)
	}
	h, err := db.GetHumanByEmail(ctx, "ada@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if h.LastLoginAt != nil {
		t.Fatalf("expected no last_login_at right after signup, got %v", *h.LastLoginAt)
	}

	firstLoginSess, err := svc.Login(ctx, "ada@example.com", "hunter22hunter")
	if err != nil {
		t.Fatal(err)
	}
	if firstLoginSess.Human.LastLoginAt == nil {
		t.Fatal("expected last_login_at to be set after a successful login")
	}
	firstLoginAt := *firstLoginSess.Human.LastLoginAt

	time.Sleep(10 * time.Millisecond)
	secondLoginSess, err := svc.Login(ctx, "ada@example.com", "hunter22hunter")
	if err != nil {
		t.Fatal(err)
	}
	if !secondLoginSess.Human.LastLoginAt.After(firstLoginAt) {
		t.Fatalf("expected a later login to advance last_login_at: first=%v second=%v", firstLoginAt, *secondLoginSess.Human.LastLoginAt)
	}

	// A failed login must never touch last_login_at.
	if _, err := svc.Login(ctx, "ada@example.com", "wrong-password"); err == nil {
		t.Fatal("expected wrong password to fail")
	}
	h, err = db.GetHumanByEmail(ctx, "ada@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !h.LastLoginAt.Equal(*secondLoginSess.Human.LastLoginAt) {
		t.Fatalf("expected a failed login attempt to leave last_login_at unchanged, got %v", *h.LastLoginAt)
	}
}

func TestLoginSuccessAndFailureModesIndistinguishable(t *testing.T) {
	db := newTestDB(t)
	svc, err := NewService(db, testSecret())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := svc.SignUp(ctx, "Ada", "ada@example.com", "correct-password"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Login(ctx, "ada@example.com", "correct-password"); err != nil {
		t.Fatal(err)
	}
	_, wrongPwErr := svc.Login(ctx, "ada@example.com", "wrong-password")
	_, unknownEmailErr := svc.Login(ctx, "nobody@example.com", "whatever-password")
	if wrongPwErr != ErrInvalidCredentials || unknownEmailErr != ErrInvalidCredentials {
		t.Fatalf("expected identical ErrInvalidCredentials for both, got %v / %v", wrongPwErr, unknownEmailErr)
	}
}

func TestRefreshRotatesAndOldTokenStopsWorking(t *testing.T) {
	db := newTestDB(t)
	svc, err := NewService(db, testSecret())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sess, err := svc.SignUp(ctx, "Ada", "ada@example.com", "correct-password")
	if err != nil {
		t.Fatal(err)
	}
	newSess, err := svc.Refresh(ctx, sess.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if newSess.RefreshToken == sess.RefreshToken {
		t.Fatal("expected a new refresh token")
	}
	if _, err := svc.Refresh(ctx, sess.RefreshToken); err == nil {
		t.Fatal("expected the old, already-rotated refresh token to fail")
	}
}

// TestConcurrentRefreshOnlyOneWins proves the fix for the race Greptile
// flagged: two goroutines calling Refresh with the SAME token at the same
// time must not both mint a session (RevokeRefreshToken's RowsAffected
// check is what prevents a single-use refresh token from producing two
// valid sessions).
func TestConcurrentRefreshOnlyOneWins(t *testing.T) {
	db := newTestDB(t)
	svc, err := NewService(db, testSecret())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sess, err := svc.SignUp(ctx, "Ada", "ada@example.com", "correct-password")
	if err != nil {
		t.Fatal(err)
	}

	const n = 10
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, err := svc.Refresh(ctx, sess.RefreshToken)
			results <- err
		}()
	}
	successes := 0
	for i := 0; i < n; i++ {
		if err := <-results; err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("expected exactly 1 of %d concurrent refreshes to succeed, got %d", n, successes)
	}
}

func TestRefreshReuseOfRevokedTokenRevokesWholeSession(t *testing.T) {
	db := newTestDB(t)
	svc, err := NewService(db, testSecret())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sess, err := svc.SignUp(ctx, "Ada", "ada@example.com", "correct-password")
	if err != nil {
		t.Fatal(err)
	}
	// Rotate once: sess.RefreshToken is now revoked, newSess.RefreshToken is live.
	newSess, err := svc.Refresh(ctx, sess.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	// Reuse the already-revoked original token — this must be treated as
	// compromise and kill the whole session, including the token that
	// rotation just issued.
	if _, err := svc.Refresh(ctx, sess.RefreshToken); err == nil {
		t.Fatal("expected reuse of a revoked token to fail")
	}
	if _, err := svc.Refresh(ctx, newSess.RefreshToken); err == nil {
		t.Fatal("expected the CURRENT token to also be revoked after reuse was detected")
	}
}

func TestLogoutRevokesToken(t *testing.T) {
	db := newTestDB(t)
	svc, err := NewService(db, testSecret())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sess, err := svc.SignUp(ctx, "Ada", "ada@example.com", "correct-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Logout(ctx, sess.RefreshToken); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Refresh(ctx, sess.RefreshToken); err == nil {
		t.Fatal("expected refresh with a logged-out token to fail")
	}
}

func TestCreateAndListOrganizations(t *testing.T) {
	db := newTestDB(t)
	svc, err := NewService(db, testSecret())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sess, err := svc.SignUp(ctx, "Ada", "ada@example.com", "correct-password")
	if err != nil {
		t.Fatal(err)
	}
	tenant, err := svc.CreateOrganization(ctx, sess.Human.ID, "Acme Inc", "acme")
	if err != nil {
		t.Fatal(err)
	}
	orgs, err := svc.ListOrganizationsForHuman(ctx, sess.Human.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(orgs) != 1 || orgs[0].ID != tenant.ID {
		t.Fatalf("expected exactly the created org, got %+v", orgs)
	}
}

func TestAccessTokenRoundTripsAndRejectsTamperedOrExpired(t *testing.T) {
	secret := testSecret()
	svc := &Service{jwtSecret: secret, now: func() time.Time { return time.Now().UTC() }}

	claims := Claims{HumanID: "human-1", Role: "user", IatUnix: time.Now().Unix(), ExpUnix: time.Now().Add(time.Minute).Unix()}
	tok, err := signJWT(secret, claims)
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.VerifyAccessToken(tok)
	if err != nil {
		t.Fatal(err)
	}
	if got.HumanID != "human-1" {
		t.Fatalf("unexpected claims: %+v", got)
	}

	tampered := tok[:len(tok)-2] + "xx"
	if _, err := svc.VerifyAccessToken(tampered); err == nil {
		t.Fatal("expected tampered token to be rejected")
	}

	expiredClaims := Claims{HumanID: "human-1", Role: "user", IatUnix: time.Now().Add(-time.Hour).Unix(), ExpUnix: time.Now().Add(-time.Minute).Unix()}
	expiredTok, err := signJWT(secret, expiredClaims)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.VerifyAccessToken(expiredTok); err == nil {
		t.Fatal("expected expired token to be rejected")
	}
}
