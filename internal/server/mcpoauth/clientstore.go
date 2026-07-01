package mcpoauth

import (
	"context"
	"errors"
	"net/http"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/digitornai/digitorn/internal/persistence/models"
)

// registeredClient is a DCR-registered client for one authorization server.
type registeredClient struct {
	Issuer       string
	ClientID     string
	ClientSecret string
}

// clientStore persists DCR-registered clients — one per issuer, secret sealed at
// rest. Registration runs once per authorization server and is reused across
// users, apps and daemon restarts.
type clientStore struct {
	db     *gorm.DB
	sealer *Sealer
	http   *http.Client
}

func newClientStore(db *gorm.DB, sealer *Sealer) *clientStore {
	return &clientStore{db: db, sealer: sealer, http: &http.Client{Timeout: 15 * time.Second}}
}

// getOrRegister returns the client registered for meta.Issuer, performing RFC
// 7591 registration once when none exists. On a concurrent registration the
// persisted winner is returned (the loser's extra client is harmless).
func (c *clientStore) getOrRegister(ctx context.Context, meta authServerMetadata, clientName, redirectURI, scope string) (registeredClient, error) {
	if existing, err := c.get(ctx, meta.Issuer); err != nil {
		return registeredClient{}, err
	} else if existing != nil {
		return *existing, nil
	}
	reg, err := registerClient(ctx, c.http, meta.RegistrationEndpoint, clientName, redirectURI, scope)
	if err != nil {
		return registeredClient{}, err
	}
	rc := registeredClient{Issuer: meta.Issuer, ClientID: reg.ClientID, ClientSecret: reg.ClientSecret}
	if err := c.put(ctx, rc, meta.RegistrationEndpoint); err != nil {
		return registeredClient{}, err
	}
	if persisted, gerr := c.get(ctx, meta.Issuer); gerr == nil && persisted != nil {
		return *persisted, nil
	}
	return rc, nil
}

func (c *clientStore) get(ctx context.Context, issuer string) (*registeredClient, error) {
	var row models.OAuthClient
	err := c.db.WithContext(ctx).Where("issuer = ?", issuer).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	secret := ""
	if len(row.ClientSecret) > 0 {
		plain, oerr := c.sealer.Open(string(row.ClientSecret))
		if oerr != nil {
			return nil, oerr
		}
		secret = string(plain)
	}
	return &registeredClient{Issuer: row.Issuer, ClientID: row.ClientID, ClientSecret: secret}, nil
}

// put inserts the client; a conflict on the issuer key is a no-op (a concurrent
// registration already won), so the caller re-reads the canonical row.
func (c *clientStore) put(ctx context.Context, rc registeredClient, registrationURI string) error {
	var sealed []byte
	if rc.ClientSecret != "" {
		s, err := c.sealer.Seal([]byte(rc.ClientSecret))
		if err != nil {
			return err
		}
		sealed = []byte(s)
	}
	now := time.Now().UTC()
	return c.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&models.OAuthClient{
		Issuer:          rc.Issuer,
		ClientID:        rc.ClientID,
		ClientSecret:    sealed,
		RegistrationURI: registrationURI,
		CreatedAt:       now,
		UpdatedAt:       now,
	}).Error
}
