// Package devices implements SPEC-W16 Agent B (contract §1): push device
// tokens for the admin + field apps (Expo mobile and the PWAs). Tokens are
// registered by the client after the push-permission grant and consumed by
// the notification-worker's SendPushNotification activity via the internal
// Dapr-invoked endpoint GET /internal/devices?contact_id= (Agent A codes TO
// that contract — its response shape is frozen).
//
// Persistence mirrors the W13 leads idiom (idempotent bootstrap DDL with
// RLS enabled + forced, tenant_isolation policy, every tenant query inside
// withTenant) packaged like the W14 PayoutStore: a small dedicated pool
// dialed from DATABASE_URL because the shared store.Store does not expose
// its pool.
package devices

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Platform values (contract §1 enum).
const (
	PlatformAndroid = "android"
	PlatformIOS     = "ios"
	PlatformWeb     = "web"
)

// App values (contract §1 enum): the admin app vs the field app (field mode
// of the same Expo binary counts as "field").
const (
	AppAdmin = "admin"
	AppField = "field"
)

// ErrInvalidInput marks deterministic validation failures (400 at the API).
var ErrInvalidInput = errors.New("invalid device input")

// DeviceToken mirrors booking.device_tokens (contract §1). One row per
// (tenant_id, token): re-registering the same token refreshes
// contact_id/platform/app and stamps last_seen_at (upsert semantics — the
// mobile client re-registers on every token refresh).
type DeviceToken struct {
	TenantID   uuid.UUID  `json:"tenant_id"`
	ContactID  *uuid.UUID `json:"contact_id"` // null when the device is not linked to a contact
	Token      string     `json:"token"`
	Platform   string     `json:"platform"` // android | ios | web
	App        string     `json:"app"`      // admin | field
	CreatedAt  time.Time  `json:"created_at"`
	LastSeenAt time.Time  `json:"last_seen_at"`
}

// maxTokenLen bounds the token column (FCM registration tokens and web-push
// endpoint URLs both stay well under 4 KiB).
const maxTokenLen = 4096

// ValidatePlatform / ValidateApp enforce the contract §1 enums.
func ValidatePlatform(p string) error {
	switch p {
	case PlatformAndroid, PlatformIOS, PlatformWeb:
		return nil
	}
	return fmt.Errorf("%w: platform %q (want android|ios|web)", ErrInvalidInput, p)
}

func ValidateApp(a string) error {
	switch a {
	case AppAdmin, AppField:
		return nil
	}
	return fmt.Errorf("%w: app %q (want admin|field)", ErrInvalidInput, a)
}

// Validate checks the minimal field set required for persistence.
func Validate(d *DeviceToken) error {
	if d.TenantID == uuid.Nil {
		return fmt.Errorf("%w: tenant_id is required", ErrInvalidInput)
	}
	d.Token = strings.TrimSpace(d.Token)
	if d.Token == "" {
		return fmt.Errorf("%w: token is required", ErrInvalidInput)
	}
	if len(d.Token) > maxTokenLen {
		return fmt.Errorf("%w: token exceeds %d bytes", ErrInvalidInput, maxTokenLen)
	}
	if err := ValidatePlatform(d.Platform); err != nil {
		return err
	}
	return ValidateApp(d.App)
}
