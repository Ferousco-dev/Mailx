package main

import (
	"fmt"
	"os"

	"github.com/Ferousco-dev/mailx/internal/humanauth"
	"github.com/Ferousco-dev/mailx/internal/secretbox"
)

// buildOAuthMFAOptions wires OAuth providers and TOTP MFA from the
// environment, each independently and following billingconfig.go's
// graceful-degradation pattern: an unset variable disables only that feature.
//
//   - Google: MAILX_GOOGLE_OAUTH_CLIENT_ID + MAILX_GOOGLE_OAUTH_CLIENT_SECRET
//   - GitHub: MAILX_GITHUB_OAUTH_CLIENT_ID + MAILX_GITHUB_OAUTH_CLIENT_SECRET
//   - both need MAILX_OAUTH_REDIRECT_BASE_URL (redirect_uri =
//     <base>/v1/auth/oauth/<provider>/callback)
//   - MFA: MAILX_MFA_MASTER_KEY (base64 32 bytes; seals TOTP secrets and must
//     differ from the DKIM/webhook master keys)
//
// Half-configured providers (id without secret, or no redirect base) are a
// startup error rather than silently disabled, so a typo is noticed.
func buildOAuthMFAOptions(log func(msg string, args ...any)) ([]humanauth.Option, error) {
	var opts []humanauth.Option
	base := os.Getenv("MAILX_OAUTH_REDIRECT_BASE_URL")
	for _, p := range []struct {
		name, idEnv, secretEnv string
		build                  func(id, secret, base string) *humanauth.OAuthProvider
	}{
		{"google", "MAILX_GOOGLE_OAUTH_CLIENT_ID", "MAILX_GOOGLE_OAUTH_CLIENT_SECRET", humanauth.GoogleProvider},
		{"github", "MAILX_GITHUB_OAUTH_CLIENT_ID", "MAILX_GITHUB_OAUTH_CLIENT_SECRET", humanauth.GitHubProvider},
	} {
		id, secret := os.Getenv(p.idEnv), os.Getenv(p.secretEnv)
		if id == "" && secret == "" {
			continue
		}
		if id == "" || secret == "" || base == "" {
			return nil, fmt.Errorf("%s OAuth needs %s, %s and MAILX_OAUTH_REDIRECT_BASE_URL together", p.name, p.idEnv, p.secretEnv)
		}
		opts = append(opts, humanauth.WithOAuthProvider(p.build(id, secret, base)))
		log("oauth_provider_enabled", "provider", p.name)
	}
	if enc := os.Getenv("MAILX_MFA_MASTER_KEY"); enc != "" {
		key, err := secretbox.DecodeKey(enc, "MAILX_MFA_MASTER_KEY")
		if err != nil {
			return nil, err
		}
		for _, other := range []string{"MAILX_DKIM_MASTER_KEY", "MAILX_WEBHOOK_MASTER_KEY"} {
			if o, derr := secretbox.DecodeKey(os.Getenv(other), other); derr == nil && string(o) == string(key) {
				return nil, fmt.Errorf("MAILX_MFA_MASTER_KEY must differ from %s", other)
			}
		}
		box, err := secretbox.New(key)
		if err != nil {
			return nil, err
		}
		opts = append(opts, humanauth.WithMFABox(box))
	} else {
		log("mfa_disabled", "hint", "MAILX_MFA_MASTER_KEY not set: MFA enrollment is unavailable")
	}
	return opts, nil
}
