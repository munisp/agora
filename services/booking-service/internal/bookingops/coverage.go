package bookingops

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// SPEC-W52 Scope B: coverage-zone enforcement at PUBLIC booking create.
//
// Semantics (BOOKING_GEO_ENFORCE):
//   - enforce=false (default) → the gate is INERT; every create path is
//     byte-identical to pre-W52 behavior.
//   - enforce=true applies ONLY to the public site create path (the route
//     wrapper in httpapi/server.go marks the request context via
//     MarkPublicBookingCreate). The tenant API (POST /v1/bookings) and the
//     Kafka command consumer replay the same Ops.Create WITHOUT the marker
//     and are never gated — agent/voice bookings are deliberately exempt
//     this wave.
//   - The site's tenant has NO service_areas rows → pass. service_areas has
//     no active flag (areas are deleted, not deactivated), so every present
//     row is "active" by construction; a tenant that defines no zones
//     accepts bookings from anywhere (documented semantics, not a stub).
//   - The booking's contact has NO contact_locations point → pass (there is
//     nothing to locate the booking by; coverage enforcement never invents
//     a location).
//   - Point present + zones present → ST_Covers over the tenant's areas
//     (geography, WGS84, parameterized). No covering area →
//     ErrGeoOutsideCoverage, mapped to 422 {code:"GEO_OUTSIDE_COVERAGE"}.
//   - PostGIS absent (embedded/non-postgis installs) → the store's
//     requireGeo guard answers ErrPostGISUnavailable, which httpapi maps to
//     503 — the existing geoHandler degradation idiom, no fallback invented.
//
// RLS: every count query runs through the store's generic built-query
// executor (Store.AudienceCount — the internal/geo builder pattern: query
// text + args produced here, the store only executes inside withTenant), so
// tenant isolation holds at the database layer even if the $1 filter
// regressed.

// ErrGeoOutsideCoverage rejects a public booking whose contact location is
// not covered by any of the site's service areas (422
// GEO_OUTSIDE_COVERAGE).
var ErrGeoOutsideCoverage = errors.New("booking contact location is outside the site's service coverage")

// CoverageCounter is the single-int built-query executor seam
// (Store.AudienceCount satisfies it). It exists so the coverage decision
// matrix is unit-testable without PostGIS: the embedded Postgres of the
// store tests has no postgis extension, so a faked counter drives the
// enabled/inside/outside cases while the real store covers the
// PostGIS-absent 503 case.
type CoverageCounter interface {
	AudienceCount(ctx context.Context, tenantID uuid.UUID, q string, args []any) (int, error)
}

// publicBookingCreateKey marks the public site create path.
type publicBookingCreateKey struct{}

// MarkPublicBookingCreate tags the request context of the public site
// booking-create route (POST /public/sites/{slug}/bookings). Only creates
// carrying the marker are coverage-gated when GeoEnforce is on.
func MarkPublicBookingCreate(ctx context.Context) context.Context {
	return context.WithValue(ctx, publicBookingCreateKey{}, true)
}

// isPublicBookingCreate reports whether the context carries the public
// create marker.
func isPublicBookingCreate(ctx context.Context) bool {
	v, _ := ctx.Value(publicBookingCreateKey{}).(bool)
	return v
}

// Coverage-zone SQL builders. All geometry enters parameterized; tenant
// scoping is both a $1 filter AND the withTenant RLS context of the
// executor. Kept pure (no DB) so tests assert the text directly.

// buildServiceAreaCountQuery counts the tenant's service zones.
func buildServiceAreaCountQuery() (string, []any) {
	return `SELECT count(*) FROM service_areas WHERE tenant_id=$1`, nil
}

// buildContactPointCountQuery counts the contact's last-known points
// (contact_locations is PK (tenant_id, contact_id) → 0 or 1).
func buildContactPointCountQuery(contactID uuid.UUID) (string, []any) {
	return `SELECT count(*) FROM contact_locations WHERE tenant_id=$1 AND contact_id=$2`,
		[]any{contactID}
}

// buildCoveredCountQuery counts the tenant's zones covering the contact's
// point. ST_Covers(geography, geography) is an exact WGS84 containment
// test; the point is read from contact_locations, never from request input.
func buildCoveredCountQuery(contactID uuid.UUID) (string, []any) {
	return `SELECT count(*) FROM service_areas a
		WHERE a.tenant_id=$1 AND EXISTS (
		    SELECT 1 FROM contact_locations cl
		    WHERE cl.tenant_id=$1 AND cl.contact_id=$2 AND ST_Covers(a.geom, cl.geom))`,
		[]any{contactID}
}

// checkGeoCoverage enforces the coverage zone policy for public creates
// (see package comment above for the full semantics). tenantID is bound as
// $1 by the caller; the builders' args carry the contact id.
func (s *Service) checkGeoCoverage(ctx context.Context, tenantID, contactID uuid.UUID) error {
	if !s.GeoEnforce || !isPublicBookingCreate(ctx) {
		return nil
	}
	counter := s.Coverage
	if counter == nil {
		counter = s.Store
	}
	areasQ, areasArgs := buildServiceAreaCountQuery()
	areas, err := counter.AudienceCount(ctx, tenantID, areasQ, append([]any{tenantID}, areasArgs...))
	if err != nil {
		return err // ErrPostGISUnavailable → 503 at the handler (geoHandler idiom)
	}
	if areas == 0 {
		return nil // no zones defined → coverage accepts anywhere
	}
	pointQ, pointArgs := buildContactPointCountQuery(contactID)
	points, err := counter.AudienceCount(ctx, tenantID, pointQ, append([]any{tenantID}, pointArgs...))
	if err != nil {
		return err
	}
	if points == 0 {
		return nil // no fix for this contact → nothing to enforce against
	}
	coveredQ, coveredArgs := buildCoveredCountQuery(contactID)
	covered, err := counter.AudienceCount(ctx, tenantID, coveredQ, append([]any{tenantID}, coveredArgs...))
	if err != nil {
		return err
	}
	if covered == 0 {
		return ErrGeoOutsideCoverage
	}
	return nil
}
