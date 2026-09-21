package webhook

import (
	"context"
	"errors"

	"github.com/Ferousco-dev/mailx/internal/database"
)

var ErrInvalidURL = errors.New("webhook: invalid destination URL")

type Service struct {
	db     *database.DB
	box    *SecretBox
	policy URLPolicy
}

type CreatedSubscription struct {
	Subscription database.WebhookSubscription
	Secret       string
}

func NewService(db *database.DB, box *SecretBox, policy URLPolicy) (*Service, error) {
	if db == nil || box == nil {
		return nil, errors.New("webhook: database and secret box are required")
	}
	return &Service{db: db, box: box, policy: policy}, nil
}

func (s *Service) Create(ctx context.Context, tenantID, rawURL string, eventTypes []string) (CreatedSubscription, error) {
	canonical, err := s.policy.Validate(ctx, rawURL)
	if err != nil {
		return CreatedSubscription{}, errors.Join(ErrInvalidURL, err)
	}
	types, err := normalizeEventTypes(eventTypes)
	if err != nil {
		return CreatedSubscription{}, err
	}
	secret, err := GenerateSecret()
	if err != nil {
		return CreatedSubscription{}, err
	}
	ciphertext, nonce, err := s.box.Encrypt(secret, []byte(tenantID))
	if err != nil {
		return CreatedSubscription{}, err
	}
	row, err := s.db.InsertWebhookSubscription(ctx, database.NewWebhookSubscription{
		TenantID: tenantID, URL: canonical, SecretCiphertext: ciphertext,
		SecretNonce: nonce, EventTypes: types,
	})
	return CreatedSubscription{Subscription: row, Secret: secret}, err
}

func (s *Service) Get(ctx context.Context, tenantID, id string) (database.WebhookSubscription, error) {
	return s.db.GetWebhookSubscription(ctx, tenantID, id)
}

func (s *Service) List(ctx context.Context, tenantID string, limit int, after *database.WebhookCursor) ([]database.WebhookSubscription, error) {
	return s.db.ListWebhookSubscriptions(ctx, tenantID, limit, after)
}

func (s *Service) Delete(ctx context.Context, tenantID, id string) error {
	return s.db.DisableWebhookSubscription(ctx, tenantID, id)
}

func (s *Service) RotateSecret(ctx context.Context, tenantID, id string) (CreatedSubscription, error) {
	if _, err := s.db.GetWebhookSubscription(ctx, tenantID, id); err != nil {
		return CreatedSubscription{}, err
	}
	secret, err := GenerateSecret()
	if err != nil {
		return CreatedSubscription{}, err
	}
	ciphertext, nonce, err := s.box.Encrypt(secret, []byte(tenantID))
	if err != nil {
		return CreatedSubscription{}, err
	}
	row, err := s.db.RotateWebhookSecret(ctx, tenantID, id, ciphertext, nonce)
	return CreatedSubscription{Subscription: row, Secret: secret}, err
}

func (s *Service) ListDeliveries(ctx context.Context, tenantID, subscriptionID string, limit int, after *database.WebhookDeliveryCursor) ([]database.WebhookDelivery, error) {
	if _, err := s.db.GetWebhookSubscription(ctx, tenantID, subscriptionID); err != nil {
		return nil, err
	}
	return s.db.ListWebhookDeliveriesPage(ctx, tenantID, subscriptionID, limit, after)
}
