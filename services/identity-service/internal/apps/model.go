// Package apps implements the OpenDesk app platform foundation (SPEC-W18 §1 /
// Agent A): the global platform_apps catalog, per-tenant provisioning state
// (tenant_apps, RLS tenant_isolation), the owner/admin-guarded registry REST
// API, lifecycle CloudEvents on opendesk.apps.lifecycle.v1 and the
// service-to-service entitlement check consumed by app backends.
package apps

import (
	"time"

	"github.com/google/uuid"
)

// Event type + topic contract (SPEC-W18 §1). The topic is configurable via
// APPS_LIFECYCLE_TOPIC; these are the defaults consumers subscribe to.
const (
	// ProvisionedEventType is published when a tenant first provisions an app.
	ProvisionedEventType = "com.opendesk.apps.AppProvisioned"
	// StatusChangedEventType is published on enable/disable/suspend transitions.
	StatusChangedEventType = "com.opendesk.apps.AppStatusChanged"
	// DefaultLifecycleTopic is the default Kafka topic for app lifecycle events.
	DefaultLifecycleTopic = "opendesk.apps.lifecycle.v1"
)

// AppStatus is the lifecycle state of a tenant's app provisioning row.
type AppStatus string

const (
	StatusEnabled   AppStatus = "enabled"
	StatusDisabled  AppStatus = "disabled"
	StatusSuspended AppStatus = "suspended"
	// StatusNotProvisioned is the virtual status of a catalog app with no
	// tenant_apps row (never stored — produced by the LEFT JOIN view).
	StatusNotProvisioned AppStatus = "not_provisioned"
)

// Valid reports whether s is a storable tenant_apps status.
func (s AppStatus) Valid() bool {
	switch s {
	case StatusEnabled, StatusDisabled, StatusSuspended:
		return true
	}
	return false
}

// PlatformApp mirrors the global platform_apps catalog row (SPEC-W18 §1,
// contract §3 fields). The catalog is platform-wide reference data.
type PlatformApp struct {
	AppID           string    `json:"app_id" yaml:"app_id"`
	Name            string    `json:"name" yaml:"name"`
	Version         string    `json:"version" yaml:"version"`
	Description     string    `json:"description" yaml:"description"`
	Category        string    `json:"category" yaml:"category"`
	Icon            string    `json:"icon" yaml:"icon"`
	NavRoute        string    `json:"nav_route" yaml:"nav_route"`
	RequiredPerms   []string  `json:"required_perms" yaml:"required_perms"`
	DefaultPlanTier string    `json:"default_plan_tier" yaml:"default_plan_tier"`
	BackendNote     string    `json:"backend_note" yaml:"backend_note"`
	CreatedAt       time.Time `json:"created_at" yaml:"-"`
}

// TenantApp mirrors a tenant_apps row: one tenant's provisioning state for
// one catalog app. Rows are never hard-deleted (DELETE soft-disables) so the
// provisioning audit trail survives.
type TenantApp struct {
	TenantID      uuid.UUID `json:"tenant_id"`
	AppID         string    `json:"app_id"`
	Status        AppStatus `json:"status"`
	Config        []byte    `json:"config"` // jsonb document, object-shaped
	ProvisionedAt time.Time `json:"provisioned_at"`
	ProvisionedBy string    `json:"provisioned_by"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// TenantAppView is the GET /v1/tenants/{slug}/apps row: the full catalog
// LEFT JOIN tenant_apps, so every catalog app appears with its tenant status
// (not_provisioned + empty config when there is no row yet).
type TenantAppView struct {
	PlatformApp
	Status        AppStatus  `json:"status"`
	Config        []byte     `json:"config"`
	ProvisionedAt *time.Time `json:"provisioned_at"`
	ProvisionedBy string     `json:"provisioned_by,omitempty"`
	UpdatedAt     *time.Time `json:"updated_at"`
}

// Entitlement reasons returned by GET /internal/entitlements/check
// (SPEC-W18 §1). Reason always mirrors the tenant's effective status;
// unknown apps are answered 404 {error} instead (callers treat as denied).
const (
	ReasonEnabled        = "enabled"
	ReasonDisabled       = "disabled"
	ReasonSuspended      = "suspended"
	ReasonNotProvisioned = "not_provisioned"
)
