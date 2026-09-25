package humanauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
)

// OAuth 2.0 Authorization Code sign-in with Google and GitHub, hand-rolled on
// net/http (DEC-231). No token is ever logged or stored: the provider access
// token is used once to fetch the verified email and then discarded.

const (
	// OAuthStateTTL bounds a started-but-unfinished OAuth flow.
	OAuthStateTTL = 10 * time.Minute
	// maxOAuthResponseBytes caps provider response bodies.
	maxOAuthResponseBytes = 1 << 20
)

var (
	// ErrOAuthProviderNotConfigured is returned for an unknown or unconfigured provider.
	ErrOAuthProviderNotConfigured = errors.New("humanauth: oauth provider is not configured")
	// ErrOAuthStateInvalid covers missing, unknown, expired, reused, or
	// wrong-provider state values.
	ErrOAuthStateInvalid = errors.New("humanauth: oauth state invalid or expired")
	// ErrOAuthProvider covers any provider-side failure (denied consent, bad
	// code, unreachable endpoint, no verified email).
	ErrOAuthProvider = errors.New("humanauth: oauth provider error")
	// ErrOAuthAccountRequiresPasswordLogin means an account with this email
	// already has a real password and cannot be auto-linked - see
	// database.ErrOAuthAccountRequiresPasswordLogin's doc (CWE-287 fix,
	// CodeRabbit, PR #26): silently linking here would let an attacker who
	// pre-registered a victim's email with a password they control capture
	// the victim's own subsequent OAuth login onto the attacker's account.
	ErrOAuthAccountRequiresPasswordLogin = errors.New("humanauth: an account with this email already has a password; log in with it first to link this sign-in method")
)

// OAuthProvider is one configured provider. Endpoint URLs must be https.
type OAuthProvider struct {
	Name         string // "google" | "github"
	ClientID     string
	ClientSecret string
	AuthURL      string
	TokenURL     string
	UserInfoURL  string
	EmailsURL    string // github only
	Scopes       []string
	RedirectURL  string
	HTTPClient   *http.Client
}

// GoogleProvider returns Google's production endpoints.
func GoogleProvider(clientID, clientSecret, redirectBaseURL string) *OAuthProvider {
	return &OAuthProvider{
		Name: "google", ClientID: clientID, ClientSecret: clientSecret,
		AuthURL:     "https://accounts.google.com/o/oauth2/v2/auth",
		TokenURL:    "https://oauth2.googleapis.com/token",
		UserInfoURL: "https://openidconnect.googleapis.com/v1/userinfo",
		Scopes:      []string{"openid", "email", "profile"},
		RedirectURL: oauthRedirectURL(redirectBaseURL, "google"),
	}
}

// GitHubProvider returns GitHub's production endpoints.
func GitHubProvider(clientID, clientSecret, redirectBaseURL string) *OAuthProvider {
	return &OAuthProvider{
		Name: "github", ClientID: clientID, ClientSecret: clientSecret,
		AuthURL:     "https://github.com/login/oauth/authorize",
		TokenURL:    "https://github.com/login/oauth/access_token",
		UserInfoURL: "https://api.github.com/user",
		EmailsURL:   "https://api.github.com/user/emails",
		Scopes:      []string{"read:user", "user:email"},
		RedirectURL: oauthRedirectURL(redirectBaseURL, "github"),
	}
}

func oauthRedirectURL(base, provider string) string {
	return strings.TrimRight(base, "/") + "/v1/auth/oauth/" + provider + "/callback"
}

func (p *OAuthProvider) validate() error {
	if p.Name != "google" && p.Name != "github" {
		return fmt.Errorf("humanauth: unknown oauth provider %q", p.Name)
	}
	if p.ClientID == "" || p.ClientSecret == "" {
		return fmt.Errorf("humanauth: oauth provider %s needs a client id and secret", p.Name)
	}
	urls := []string{p.AuthURL, p.TokenURL, p.UserInfoURL}
	if p.Name == "github" {
		urls = append(urls, p.EmailsURL)
	}
	for _, u := range urls {
		pu, err := url.Parse(u)
		if err != nil || pu.Scheme != "https" || pu.Host == "" {
			return fmt.Errorf("humanauth: oauth provider %s endpoints must be absolute https URLs", p.Name)
		}
	}
	ru, err := url.Parse(p.RedirectURL)
	if err != nil || (ru.Scheme != "https" && ru.Scheme != "http") || ru.Host == "" {
		return fmt.Errorf("humanauth: oauth redirect base URL must be an absolute http(s) URL")
	}
	return nil
}

// WithOAuthProvider enables sign-in with p. Invalid config is a startup error
// surfaced by NewService.
func WithOAuthProvider(p *OAuthProvider) Option {
	return func(s *Service) {
		if s.oauth == nil {
			s.oauth = map[string]*OAuthProvider{}
		}
		s.oauth[p.Name] = p
	}
}

// OAuthConfigured reports whether provider is enabled.
func (s *Service) OAuthConfigured(provider string) bool { return s.oauth[provider] != nil }

// StartOAuth creates a single-use state and returns the provider consent URL.
func (s *Service) StartOAuth(ctx context.Context, provider string) (string, error) {
	p := s.oauth[provider]
	if p == nil {
		return "", ErrOAuthProviderNotConfigured
	}
	state, err := generateRawToken()
	if err != nil {
		return "", err
	}
	if err := s.db.CreateOAuthState(ctx, hashRawToken(state), provider, s.now().Add(OAuthStateTTL)); err != nil {
		return "", fmt.Errorf("humanauth: store oauth state: %w", err)
	}
	q := url.Values{}
	q.Set("client_id", p.ClientID)
	q.Set("redirect_uri", p.RedirectURL)
	q.Set("response_type", "code")
	q.Set("scope", strings.Join(p.Scopes, " "))
	q.Set("state", state)
	if provider == "google" {
		q.Set("prompt", "select_account")
	}
	return p.AuthURL + "?" + q.Encode(), nil
}

type oauthUser struct {
	ID        string
	Email     string
	Name      string
	AvatarURL string
}

// CompleteOAuth validates and consumes state, exchanges code, fetches the
// provider-verified email, resolves (link or create) the human, and returns a
// session - or *MFARequiredError when the account has MFA enabled, so OAuth
// never bypasses the second factor.
func (s *Service) CompleteOAuth(ctx context.Context, provider, code, state string) (Session, error) {
	p := s.oauth[provider]
	if p == nil {
		return Session{}, ErrOAuthProviderNotConfigured
	}
	if state == "" {
		return Session{}, ErrOAuthStateInvalid
	}
	// The state is looked up by its SHA-256 hash, so the comparison never
	// touches the raw value; DELETE ... RETURNING-style consume makes it
	// single-use even under concurrent callbacks.
	ok, err := s.db.ConsumeOAuthState(ctx, hashRawToken(state), provider, s.now())
	if err != nil {
		return Session{}, fmt.Errorf("humanauth: consume oauth state: %w", err)
	}
	if !ok {
		return Session{}, ErrOAuthStateInvalid
	}
	if code == "" {
		return Session{}, ErrOAuthProvider
	}
	token, err := p.exchange(ctx, code)
	if err != nil {
		return Session{}, err
	}
	u, err := p.fetchUser(ctx, token)
	if err != nil {
		return Session{}, err
	}
	name := strings.TrimSpace(u.Name)
	if name == "" {
		name = strings.SplitN(u.Email, "@", 2)[0]
	}
	if r := []rune(name); len(r) > 200 {
		name = string(r[:200])
	}
	var avatar *string
	if strings.HasPrefix(u.AvatarURL, "https://") {
		avatar = &u.AvatarURL
	}
	h, _, err := s.db.ResolveOAuthHuman(ctx, provider, u.ID, u.Email, name, avatar)
	if errors.Is(err, database.ErrOAuthAccountRequiresPasswordLogin) {
		return Session{}, ErrOAuthAccountRequiresPasswordLogin
	}
	if err != nil {
		return Session{}, fmt.Errorf("humanauth: resolve oauth human: %w", err)
	}
	if h.MFAEnabled {
		return Session{}, s.newMFAChallenge(ctx, h)
	}
	return s.completeLogin(ctx, h)
}

func (p *OAuthProvider) client() *http.Client {
	if p.HTTPClient != nil {
		return p.HTTPClient
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (p *OAuthProvider) doJSON(req *http.Request, out any) error {
	req.Header.Set("Accept", "application/json")
	resp, err := p.client().Do(req)
	if err != nil {
		return fmt.Errorf("%w: %s request failed", ErrOAuthProvider, p.Name)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxOAuthResponseBytes))
	if err != nil {
		return fmt.Errorf("%w: %s read failed", ErrOAuthProvider, p.Name)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: %s returned HTTP %d", ErrOAuthProvider, p.Name, resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%w: %s returned malformed JSON", ErrOAuthProvider, p.Name)
	}
	return nil
}

func (p *OAuthProvider) exchange(ctx context.Context, code string) (string, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", p.RedirectURL)
	form.Set("client_id", p.ClientID)
	form.Set("client_secret", p.ClientSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var tok struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		Error       string `json:"error"`
	}
	if err := p.doJSON(req, &tok); err != nil {
		return "", err
	}
	// GitHub reports exchange failures as HTTP 200 with an "error" field.
	if tok.Error != "" || tok.AccessToken == "" {
		return "", fmt.Errorf("%w: %s token exchange rejected", ErrOAuthProvider, p.Name)
	}
	return tok.AccessToken, nil
}

func (p *OAuthProvider) authedGET(ctx context.Context, u, token string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return p.doJSON(req, out)
}

func (p *OAuthProvider) fetchUser(ctx context.Context, token string) (oauthUser, error) {
	switch p.Name {
	case "google":
		var g struct {
			Sub           string `json:"sub"`
			Email         string `json:"email"`
			EmailVerified bool   `json:"email_verified"`
			Name          string `json:"name"`
			Picture       string `json:"picture"`
		}
		if err := p.authedGET(ctx, p.UserInfoURL, token, &g); err != nil {
			return oauthUser{}, err
		}
		// Linking by email is only safe for a provider-VERIFIED address.
		if g.Sub == "" || g.Email == "" || !g.EmailVerified {
			return oauthUser{}, fmt.Errorf("%w: google account has no verified email", ErrOAuthProvider)
		}
		return oauthUser{ID: g.Sub, Email: g.Email, Name: g.Name, AvatarURL: g.Picture}, nil
	default: // github
		var gh struct {
			ID        int64  `json:"id"`
			Login     string `json:"login"`
			Name      string `json:"name"`
			AvatarURL string `json:"avatar_url"`
		}
		if err := p.authedGET(ctx, p.UserInfoURL, token, &gh); err != nil {
			return oauthUser{}, err
		}
		var emails []struct {
			Email    string `json:"email"`
			Primary  bool   `json:"primary"`
			Verified bool   `json:"verified"`
		}
		if err := p.authedGET(ctx, p.EmailsURL, token, &emails); err != nil {
			return oauthUser{}, err
		}
		email := ""
		for _, e := range emails {
			if e.Primary && e.Verified {
				email = e.Email
			}
		}
		if gh.ID == 0 || email == "" {
			return oauthUser{}, fmt.Errorf("%w: github account has no verified primary email", ErrOAuthProvider)
		}
		name := gh.Name
		if name == "" {
			name = gh.Login
		}
		return oauthUser{ID: strconv.FormatInt(gh.ID, 10), Email: email, Name: name, AvatarURL: gh.AvatarURL}, nil
	}
}
