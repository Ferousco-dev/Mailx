package main

import (
	"crypto/subtle"
	"errors"
	"os"

	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/dkim"
	maildomain "github.com/Ferousco-dev/mailx/internal/domain"
	"github.com/Ferousco-dev/mailx/internal/secretbox"
)

// buildDKIM wires DKIM key management and signing. MAILX_DKIM_MASTER_KEY
// (base64, 32 bytes) encrypts DKIM private keys at rest. It MUST differ from
// MAILX_WEBHOOK_MASTER_KEY: the two secrets protect unrelated data and must not
// share key material. Errors name the variables, never the values.
func buildDKIM(db *database.DB, o obs) (*dkim.Service, error) {
	key, err := secretbox.DecodeKey(os.Getenv("MAILX_DKIM_MASTER_KEY"), "MAILX_DKIM_MASTER_KEY")
	if err != nil {
		return nil, err
	}
	if other, derr := secretbox.DecodeKey(os.Getenv("MAILX_WEBHOOK_MASTER_KEY"), "MAILX_WEBHOOK_MASTER_KEY"); derr == nil &&
		subtle.ConstantTimeCompare(key, other) == 1 {
		return nil, errors.New("MAILX_DKIM_MASTER_KEY must differ from MAILX_WEBHOOK_MASTER_KEY")
	}
	box, err := secretbox.New(key)
	if err != nil {
		return nil, err
	}
	var observer dkim.Observer
	if o.metrics != nil {
		observer = o.metrics
	}
	return dkim.NewService(db, box, maildomain.NewNetTXTResolver(), observer)
}
