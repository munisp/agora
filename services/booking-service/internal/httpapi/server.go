// Package httpapi wires the chi router, tenant/auth middleware and REST
// handlers for booking-service.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/appgate"
	"github.com/opendesk/booking-service/internal/bookingops"
	"github.com/opendesk/booking-service/internal/cache"
	"github.com/opendesk/booking-service/internal/campaignstudio"
	"github.com/opendesk/booking-service/internal/crm360"
	"github.com/opendesk/booking-service/internal/daprc"
	"github.com/opendesk/booking-service/internal/devices"
	"github.com/opendesk/booking-service/internal/fieldcapture"
	"github.com/opendesk/booking-service/internal/geo"
	"github.com/opendesk/booking-service/internal/helpdesk"
	"github.com/opendesk/booking-service/internal/incidents"
	"github.com/opendesk/booking-service/internal/leads"
	"github.com/opendesk/booking-service/internal/lending"
	"github.com/opendesk/booking-service/internal/loyalty"
	"github.com/opendesk/booking-service/internal/permify"
	"github.com/opendesk/booking-service/internal/referrals"
	"github.com/opendesk/booking-service/internal/socialpub"
	"github.com/opendesk/booking-service/internal/store"
	"github.com/opendesk/booking-service/internal/surveys"
	"github.com/opendesk/booking-service/internal/workforce"
	"github.com/opendesk/booking-service/internal/workorders"
	"go.uber.org/zap"
)

// Authz outage policies (AUTHZ_OUTAGE_POLICY, Wave 5 #5).
const (
	// AuthzFailClosed denies requests when Permify is unreachable (default,
	// production-safe).
	AuthzFailClosed = "fail_closed"
	// AuthzFailOpen allows requests when Permify is unreachable, logging
	// CRITICAL — a dev convenience, never for production.
	AuthzFailOpen = "fail_open"
)

// EventPublisher abstracts CloudEvent publishing (daprc.Client satisfies it)
// so portal-code delivery is stubbed in tests.
type EventPublisher interface {
	PublishEvent(ctx context.Context, pubsub, topic string, data any) error
}

// Deps bundles server dependencies.
type Deps struct {
	Store         *store.Store
	Ops           *bookingops.Service
	Resolver      *bookingops.TenantResolver
	Authz         permify.Authorizer
	AuthzDisabled bool // dev escape hatch (AUTHZ_DISABLED=true)
	// AuthzOutagePolicy decides what happens when the Permify check itself
	// errors: AuthzFailClosed (default) or AuthzFailOpen.
	AuthzOutagePolicy string
	Dapr              *daprc.Client
	IdentityAppID     string
	Gdpr              GdprStarter  // may be nil when Temporal is unreachable
	Cache             *cache.Cache // availability cache; nil disables caching
	Logger            *zap.Logger

	// Customer portal (Wave 5 #7).
	PortalSecret       string         // HMAC secret for portal JWTs (PORTAL_SECRET)
	PubSubName         string         // Dapr pubsub component for the notifications outbox
	NotificationsTopic string         // opendesk.notifications.outbox
	Publisher          EventPublisher // nil → Dapr client is used
	// TenantBySlug resolves tenant context for portal reschedule (timezone).
	// nil → Resolver.BySlug is used.
	TenantBySlug func(ctx context.Context, slug string) (bookingops.TenantInfo, error)
	// Geo serves the SPEC-W8 geospatial endpoints (locations, service
	// areas, geo campaigns). Nil → those routes answer 503.
	Geo *geo.Handlers
	// Incidents serves the SPEC-W11 Part B incident endpoints (list/detail,
	// dispatch, endpoint CRUD, ingest). Nil → those routes answer 503.
	Incidents *incidents.Service
	// Leads serves the SPEC-W13 CAC endpoints (leads, promo codes +
	// redeem, campaigns + spend, internal spend-sum). Nil → 503.
	Leads *leads.Service
	// Referrals serves the SPEC-W14 referral & commission endpoints
	// (referrals CRUD + verify, rules CRUD, ledger, balances). Nil → 503.
	Referrals *referrals.Service
	// Payouts serves GET /v1/payouts (SPEC-W14 Agent B: the payout queue
	// for Agent C's admin page). Nil → 503.
	Payouts *referrals.PayoutStore
	// Devices serves the SPEC-W16 push device-token endpoints (contract §1:
	// /v1/devices + GET /internal/devices?contact_id=). Nil → 503.
	Devices *devices.Handlers
	// FieldCapture serves POST /v1/field/capture (SPEC-W16 contract §4:
	// the offline-queue flush endpoint). Nil → 503.
	FieldCapture *fieldcapture.Handlers
	// AppGate (SPEC-W18 Agent D, contract §4): the app entitlement gate
	// middleware. Nil → no route is gated. When constructed with
	// APP_GATE_ENABLED=false (the DEFAULT) it is a pure pass-through —
	// production behavior is UNCHANGED unless explicitly opted in.
	// Reference wiring: /v1/leads is gated behind app_id "cac"
	// (docs/app-developer-guide.md).
	AppGate *appgate.Gate

	// SPEC-W19 integrator (additive): the four enterprise app packages.
	// Each Deps bundle is built in cmd/server/main.go (store + topics); the
	// tenant/user accessors and permission middleware are attached here in
	// NewRouter (they read this package's private context keys). Nil → the
	// app's routes are not registered (partial deployments stay intact).
	// Helpdesk serves /v1/helpdesk (appgate app_id "helpdesk").
	Helpdesk *helpdesk.Deps
	// Workorders serves /v1/field-service (appgate app_id "field-service").
	Workorders *workorders.Deps
	// Loyalty serves /v1/loyalty (appgate app_id "loyalty-wallet").
	Loyalty *loyalty.Deps
	// Studio serves /v1/studio (appgate app_id "campaign-studio").
	Studio *campaignstudio.Deps

	// SPEC-W20 integrator (additive): the four batch-2 enterprise app
	// packages. Same wiring posture as W19 — each Deps bundle is built in
	// cmd/server/main.go (store + topics); the tenant/user accessors and
	// permission middleware are attached here in NewRouter. Nil → the
	// app's routes are not registered (partial deployments stay intact).
	// CRM360 serves /v1/crm (appgate app_id "crm-360").
	CRM360 *crm360.Deps
	// Surveys serves /v1/surveys (appgate app_id "surveys-voc"). The
	// package itself registers POST /v1/surveys/respond OUTSIDE the gated
	// group (public token-resolved submit path — see surveys/handlers.go).
	Surveys *surveys.Deps
	// Lending serves /v1/lending (appgate app_id "lending").
	Lending *lending.Deps
	// Workforce serves /v1/workforce (appgate app_id "workforce").
	Workforce *workforce.Deps

	// SPEC-W21 integrator (additive): the social-publisher app package.
	// Same wiring posture as W19/W20 — the Deps bundle is built in
	// cmd/server/main.go (store + topics + provider mocks); the tenant
	// accessor and permission middleware are attached here in NewRouter.
	// Nil → the app's routes are not registered (partial deployments stay
	// intact).
	// Social serves /v1/social (appgate app_id "social-publisher").
	Social *socialpub.Deps
}

type ctxKey string

const (
	ctxTenant ctxKey = "tenant"
	ctxUser   ctxKey = "user"
)

// NewRouter builds the chi router with all routes.
func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(60 * time.Second))

	if d.TenantBySlug == nil && d.Resolver != nil {
		d.TenantBySlug = d.Resolver.BySlug
	}
	s := &server{d: d, portalLimiter: newPortalRateLimiter(), promoLimiter: newPortalRateLimiter()}

	// SPEC-W18 Agent D (additive): the entitlement gate prefers the tenant
	// resolved by tenantMiddleware (covers the JWT-claim path where no
	// X-Tenant-Slug header is present), falling back to the raw header.
	if s.d.AppGate != nil {
		s.d.AppGate.SetTenantSlugFunc(func(r *http.Request) string {
			if t := tenantFrom(r.Context()); t.Slug != "" {
				return t.Slug
			}
			return r.Header.Get("X-Tenant-Slug")
		})
	}

	r.Get("/healthz", s.healthz)

	// Tenant-scoped management API (SPEC §8: tenant from JWT claim or
	// X-Tenant-Slug header, validated by middleware).
	r.Route("/v1", func(r chi.Router) {
		r.Use(s.tenantMiddleware)
		r.Route("/offerings", func(r chi.Router) {
			r.Get("/", s.listOfferings)
			r.With(s.require("manage_catalog")).Post("/", s.createOffering)
			r.Get("/{id}", s.getOffering)
			r.With(s.require("manage_catalog")).Put("/{id}", s.updateOffering)
			r.With(s.require("manage_catalog")).Delete("/{id}", s.deleteOffering)
		})
		r.Route("/team-members", func(r chi.Router) {
			r.Get("/", s.listTeamMembers)
			r.With(s.require("manage_catalog")).Post("/", s.createTeamMember)
			r.Get("/{id}", s.getTeamMember)
			r.With(s.require("manage_catalog")).Put("/{id}", s.updateTeamMember)
			r.With(s.require("manage_catalog")).Delete("/{id}", s.deleteTeamMember)
			r.With(s.require("manage_catalog")).Put("/{id}/availability", s.putAvailability)
			r.Get("/{id}/availability", s.getAvailabilityRules)
		})
		r.Get("/availability", s.getAvailability)
		// Wave 5 #4: ranked slot suggestions minimizing calendar fragmentation.
		r.Get("/availability/optimize", s.getOptimizedAvailability)
		r.Get("/site", s.getSite)
		r.With(s.require("manage_catalog")).Put("/site", s.updateSite)
		r.Route("/contacts", func(r chi.Router) {
			r.Get("/", s.listContacts)
			r.With(s.require("manage_bookings")).Post("/", s.createContact)
			r.Get("/{id}", s.getContact)
			r.With(s.require("manage_bookings")).Put("/{id}", s.updateContact)
			r.With(s.require("manage_bookings")).Delete("/{id}", s.deleteContact)
			// SPEC-W8 A2: contact location upsert (lat/lng or geocoded address).
			r.With(s.require("manage_bookings")).Put("/{id}/location", s.geoHandler((*geo.Handlers).PutContactLocation))
		})
		// SPEC-W8 A2 geospatial endpoints (BFF: /api/bookings/v1/...).
		r.Get("/locations/summary", s.geoHandler((*geo.Handlers).LocationsSummary))
		r.Route("/service-areas", func(r chi.Router) {
			r.Get("/", s.geoHandler((*geo.Handlers).ListServiceAreas))
			r.With(s.require("manage_bookings")).Post("/", s.geoHandler((*geo.Handlers).CreateServiceArea))
			r.With(s.require("manage_bookings")).Delete("/{id}", s.geoHandler((*geo.Handlers).DeleteServiceArea))
		})
		r.Route("/geo", func(r chi.Router) {
			r.With(s.require("manage_bookings")).Post("/audience/preview", s.geoHandler((*geo.Handlers).AudiencePreview))
			r.With(s.require("manage_bookings")).Post("/campaigns", s.geoHandler((*geo.Handlers).CreateGeoCampaign))
			r.Get("/campaigns", s.geoHandler((*geo.Handlers).ListGeoCampaigns))
			r.Get("/campaigns/{id}", s.geoHandler((*geo.Handlers).GetGeoCampaign))
		})
		r.Route("/bookings", func(r chi.Router) {
			r.With(s.require("manage_bookings")).Post("/", s.createBooking)
			r.Get("/", s.listBookings)
			r.Get("/{id}", s.getBooking)
			r.With(s.require("manage_bookings")).Post("/{id}/reschedule", s.rescheduleBooking)
			r.With(s.require("manage_bookings")).Post("/{id}/cancel", s.cancelBooking)
		})
		r.Route("/waitlist", func(r chi.Router) {
			r.With(s.require("manage_bookings")).Post("/", s.createWaitlistEntry)
			r.Get("/", s.listWaitlist)
			// Claim is token-authorized (capability), not permify-guarded —
			// the claimant is an end customer following the backfill link.
			r.Post("/{id}/claim", s.claimWaitlist)
		})
		// GDPR privacy endpoints (SPEC-W3 §2 innovation 13) — restored
		// additively; handlers live in privacy.go.
		r.Route("/privacy", func(r chi.Router) {
			r.With(s.require("manage_bookings")).Post("/export", s.gdprExport)
			r.With(s.require("manage_bookings")).Post("/erase", s.gdprErase)
		})
		// Incidents API (SPEC-W11 Part B §3): admin list/detail, manual
		// dispatch and dispatch-endpoint CRUD (manage_bookings).
		r.Route("/incidents", func(r chi.Router) {
			r.Get("/", s.listIncidents)
			r.Get("/{id}", s.getIncident)
			r.With(s.require("manage_bookings")).Post("/{id}/dispatch", s.dispatchIncident)
			r.With(s.require("manage_bookings")).Post("/dispatch-endpoints", s.createDispatchEndpoint)
			r.Get("/dispatch-endpoints", s.listDispatchEndpoints)
			r.With(s.require("manage_bookings")).Delete("/dispatch-endpoints", s.deleteDispatchEndpoint)
		})
		// Leads + CAC (SPEC-W13 Agent A): manage_bookings for mutations,
		// view_analytics for reads (docs/security/roles.md).
		// SPEC-W18 Agent D (additive): reference wiring of the app
		// entitlement gate — this route group belongs to catalog app "cac"
		// and is gated against identity's /internal/entitlements/check when
		// APP_GATE_ENABLED=true. With the default (false) the gate is a pure
		// pass-through: production behavior is UNCHANGED unless opted in.
		r.Route("/leads", func(r chi.Router) {
			if s.d.AppGate != nil {
				r.Use(s.d.AppGate.Middleware("cac"))
			}
			r.With(s.require("view_analytics")).Get("/", s.listLeads)
			r.With(s.require("manage_bookings")).Post("/", s.createLead)
			r.With(s.require("view_analytics")).Get("/{id}", s.getLead)
			r.With(s.require("manage_bookings")).Post("/{id}/status", s.transitionLead)
		})
		r.Route("/promo", func(r chi.Router) {
			r.With(s.require("manage_bookings")).Post("/", s.createPromoCode)
			r.With(s.require("view_analytics")).Get("/", s.listPromoCodes)
		})
		r.Route("/campaigns", func(r chi.Router) {
			r.With(s.require("view_analytics")).Get("/", s.listCampaigns)
			r.With(s.require("manage_bookings")).Post("/", s.createCampaign)
			r.With(s.require("manage_bookings")).Post("/{id}/spend", s.recordCampaignSpend)
		})
		// Referrals + commissions (SPEC-W14 Agent A): manage_bookings for
		// mutations, view_analytics for reads (same posture as /leads).
		r.Route("/referrals", func(r chi.Router) {
			r.With(s.require("view_analytics")).Get("/", s.listReferrals)
			r.With(s.require("manage_bookings")).Post("/", s.createReferral)
			r.With(s.require("view_analytics")).Get("/{id}", s.getReferral)
			r.With(s.require("manage_bookings")).Post("/{id}/verify", s.verifyReferral)
			r.With(s.require("manage_bookings")).Post("/{id}/reject", s.rejectReferral)
		})
		r.Route("/commissions", func(r chi.Router) {
			r.With(s.require("view_analytics")).Get("/rules", s.listCommissionRules)
			r.With(s.require("manage_bookings")).Post("/rules", s.createCommissionRule)
			r.With(s.require("manage_bookings")).Put("/rules/{id}", s.updateCommissionRule)
			r.With(s.require("manage_bookings")).Delete("/rules/{id}", s.deleteCommissionRule)
			r.With(s.require("view_analytics")).Get("/ledger", s.listCommissionLedger)
			r.With(s.require("view_analytics")).Get("/balance/{beneficiary}", s.commissionBalance)
		})
		// SPEC-W14 Agent B (additive): the payout queue read by Agent C's
		// payouts page — view_analytics like the other commission reads.
		r.With(s.require("view_analytics")).Get("/payouts", s.listPayouts)
		// SPEC-W16 Agent B (additive): push device tokens (contract §1).
		// Register/unregister are manage_bookings (least-privileged
		// existing write perm — staff register from the field app; there
		// is no lighter write permission in the Permify schema); the
		// inventory read is view_analytics like the other CAC reads.
		r.Route("/devices", func(r chi.Router) {
			r.With(s.require("manage_bookings")).Post("/", s.devicesHandler((*devices.Handlers).Register))
			r.With(s.require("manage_bookings")).Delete("/{token}", s.devicesHandler((*devices.Handlers).Unregister))
			r.With(s.require("view_analytics")).Get("/", s.devicesHandler((*devices.Handlers).List))
		})
		// SPEC-W16 Agent B (additive): field PWA / mobile offline-queue
		// flush (contract §4). manage_bookings — same posture as the lead
		// capture + contact-location writes it performs.
		r.With(s.require("manage_bookings")).Post("/field/capture", s.fieldCaptureHandler)
		// SPEC-W19 integrator (additive): loyalty self-mounts "/loyalty"
		// INSIDE this /v1 group (chi panics on a second Mount("/v1"), and
		// this way the group inherits the tenant middleware for free);
		// per-route perms come from Deps.Require, the appgate gate for
		// app_id "loyalty-wallet" wraps the whole group.
		if s.d.Loyalty != nil {
			s.d.Loyalty.TenantFromContext = tenantFrom
			s.d.Loyalty.Require = s.require
			loyalty.RegisterRoutes(r, s.d.Loyalty, s.appGateChain("loyalty-wallet")...)
		}
	})

	// SPEC-W19 integrator — BEGIN (additive): the remaining three enterprise
	// app route groups (loyalty is wired inside the /v1 group above). Each
	// package self-mounts its /v1-prefixed routes; the chain is
	// tenantMiddleware (resolves ctxTenant/ctxUser the packages read via the
	// accessors below) → appgate entitlement gate (pass-through unless
	// APP_GATE_ENABLED=true) → perms (GET/HEAD → view_analytics, writes →
	// manage_bookings, per SPEC-W19 contract §3). Nil Deps → not registered.
	if s.d.Helpdesk != nil {
		s.d.Helpdesk.TenantFromContext = func(ctx context.Context) (bookingops.TenantInfo, bool) {
			t := tenantFrom(ctx)
			return t, t.ID != uuid.Nil
		}
		s.d.Helpdesk.UserFromContext = userFrom
		mw := append([]func(http.Handler) http.Handler{s.tenantMiddleware},
			s.appGateChain("helpdesk", s.requireReadWrite())...)
		helpdesk.RegisterRoutes(r, s.d.Helpdesk, mw...)
	}
	if s.d.Workorders != nil {
		// The package resolves the tenant itself first (its own tenant
		// middleware wraps the integrator chain — RegisterRoutes contract);
		// httpapi's tenantMiddleware then runs ahead of the appgate/perms
		// middleware so the slug extractor and require() see
		// ctxTenant/ctxUser. Both resolutions ride the resolver cache.
		mw := append([]func(http.Handler) http.Handler{s.tenantMiddleware},
			s.appGateChain("field-service", s.requireReadWrite())...)
		workorders.RegisterRoutes(r, s.d.Workorders, mw...)
	}
	if s.d.Studio != nil {
		s.d.Studio.TenantFromContext = tenantFrom
		s.d.Studio.RequireRead = s.require("view_analytics")
		s.d.Studio.RequireWrite = s.require("manage_bookings")
		mw := append([]func(http.Handler) http.Handler{s.tenantMiddleware},
			s.appGateChain("campaign-studio")...)
		campaignstudio.RegisterRoutes(r, s.d.Studio, mw...)
	}
	// SPEC-W19 integrator — END

	// SPEC-W20 integrator — BEGIN (additive): the four batch-2 enterprise
	// app route groups. Identical posture to W19: each package resolves
	// the tenant itself first (its own tenant middleware wraps the
	// integrator chain), then httpapi's tenantMiddleware runs ahead of the
	// appgate/perms middleware so the slug extractor and require() see
	// ctxTenant/ctxUser. Perms are method-aware via requireReadWrite
	// (GET/HEAD → view_analytics, writes → manage_bookings, SPEC-W20
	// contract §3); the appgate gate is a pass-through unless
	// APP_GATE_ENABLED=true. Nil Deps → not registered.
	if s.d.CRM360 != nil {
		s.d.CRM360.UserFromContext = userFrom
		mw := append([]func(http.Handler) http.Handler{s.tenantMiddleware},
			s.appGateChain("crm-360", s.requireReadWrite())...)
		crm360.RegisterRoutes(r, s.d.CRM360, mw...)
	}
	if s.d.Surveys != nil {
		// POST /v1/surveys/respond is registered by the PACKAGE OUTSIDE
		// this gated group — public submit path, no tenant header, no JWT,
		// no appgate (the loud comment in surveys/handlers.go; the invite
		// token resolves the tenant via the invite_token_access RLS
		// policy). Only the tenant-scoped group gets the chain below.
		mw := append([]func(http.Handler) http.Handler{s.tenantMiddleware},
			s.appGateChain("surveys-voc", s.requireReadWrite())...)
		surveys.RegisterRoutes(r, s.d.Surveys, mw...)
	}
	if s.d.Lending != nil {
		s.d.Lending.UserFromContext = userFrom
		mw := append([]func(http.Handler) http.Handler{s.tenantMiddleware},
			s.appGateChain("lending", s.requireReadWrite())...)
		lending.RegisterRoutes(r, s.d.Lending, mw...)
	}
	if s.d.Workforce != nil {
		s.d.Workforce.UserFromContext = userFrom
		mw := append([]func(http.Handler) http.Handler{s.tenantMiddleware},
			s.appGateChain("workforce", s.requireReadWrite())...)
		workforce.RegisterRoutes(r, s.d.Workforce, mw...)
	}
	// SPEC-W20 integrator — END

	// SPEC-W21 integrator — BEGIN (additive): the social-publisher route
	// group. Same posture as W19 helpdesk (the package reads the tenant
	// via the accessor below): httpapi's tenantMiddleware runs first
	// (resolves ctxTenant/ctxUser), then the appgate entitlement gate
	// (pass-through unless APP_GATE_ENABLED=true), then method-aware
	// perms (GET/HEAD → view_analytics, writes → manage_bookings). Nil
	// Deps → not registered.
	if s.d.Social != nil {
		s.d.Social.TenantFromContext = func(ctx context.Context) (bookingops.TenantInfo, bool) {
			t := tenantFrom(ctx)
			return t, t.ID != uuid.Nil
		}
		mw := append([]func(http.Handler) http.Handler{s.tenantMiddleware},
			s.appGateChain("social-publisher", s.requireReadWrite())...)
		socialpub.RegisterRoutes(r, s.d.Social, mw...)
	}
	// SPEC-W21 integrator — END

	// Public promo redemption (SPEC-W13 §6): rate-limited, idempotent per
	// code+phone. No tenant middleware — the unguessable code resolves the
	// owning tenant server-side (public site-slug resolution pattern).
	r.Post("/v1/promo/redeem", s.redeemPromo)

	// IoT/webhook incident ingest (SPEC-W11 Part B §6): invoked
	// service-to-service via Dapr by the messaging-gateway, which already
	// authenticated the caller (per-tenant shared secret) — hence no tenant
	// middleware here; the body carries tenant_id / tenant_slug.
	r.Post("/v1/incidents/ingest", s.ingestIncident)

	// Delivery-ledger update (SPEC-W11 Part B §4): notification-worker's
	// UpdateWebhookDelivery activity (payload type "incident") records each
	// attempt outcome here via Dapr service invocation.
	r.Post("/internal/incidents/deliveries/{id}", s.updateIncidentDelivery)

	// Public booking page endpoints — no auth; the site slug resolves the
	// tenant server-side, so cross-tenant access is impossible by construction.
	r.Route("/public/sites/{slug}", func(r chi.Router) {
		// Widget/page shell endpoints (no auth, published sites only).
		r.Get("/", s.publicSite)
		r.Get("/offerings", s.publicOfferings)
		r.Get("/context", s.publicContext)
		r.Get("/availability", s.publicAvailability)
		r.Post("/bookings", s.publicCreateBooking)
		// Customer self-service portal (Wave 5 #7): magic-code login. The
		// authenticated half lives under /portal (portal JWT middleware).
		r.Post("/portal/request", s.portalRequestCode)
		r.Post("/portal/verify", s.portalVerifyCode)
	})

	// Customer self-service portal, contact-scoped via the portal JWT
	// issued by /public/sites/{slug}/portal/verify (no Keycloak account —
	// APISIX must expose /api/bookings/portal/* without openid-connect).
	r.Route("/portal", func(r chi.Router) {
		r.Use(s.portalMiddleware)
		r.Get("/bookings", s.portalListBookings)
		r.Post("/bookings/{id}/reschedule", s.portalRescheduleBooking)
		r.Post("/bookings/{id}/cancel", s.portalCancelBooking)
	})

	// Temporal activity endpoints invoked by the saga workers via Dapr
	// service invocation (SPEC §6).
	r.Route("/activities", func(r chi.Router) {
		r.Post("/reserve-slot", s.activityReserveSlot)
		r.Post("/confirm-booking", s.activityConfirmBooking)
		r.Post("/release-slot", s.activityReleaseSlot)
		r.Post("/mark-no-show", s.activityMarkNoShow)
	})

	// Internal endpoints invoked by other services via Dapr (e.g. the
	// TenantOnboardingWorkflow seeds the default public site).
	r.Post("/internal/sites", s.createSiteInternal)

	// Reverse CRM sync endpoints (Twenty -> OpenDesk, SPEC-CRM §B), invoked
	// by crm-sync-service via Dapr service invocation. Tenant resolution is
	// the usual X-Tenant-Slug middleware; no Permify guard (internal only).
	r.Route("/internal/contacts", func(r chi.Router) {
		r.Use(s.tenantMiddleware)
		r.Post("/upsert", s.upsertContactInternal)
		r.Get("/", s.lookupContactInternal)
	})
	r.Route("/internal/bookings", func(r chi.Router) {
		r.Use(s.tenantMiddleware)
		r.Post("/{id}/crm-note", s.addBookingCRMNoteInternal)
	})

	// Internal spend-sum (SPEC-W13 §4/§5): analytics-service (Agent B)
	// invokes this via Dapr to join campaign spend into the CAC rollups.
	// Tenant resolution is the usual X-Tenant-Slug middleware; no Permify
	// guard (internal only).
	r.Route("/internal/campaigns", func(r chi.Router) {
		r.Use(s.tenantMiddleware)
		r.Get("/{id}/spend-sum", s.campaignSpendSum)
	})

	// Internal device-token lookup (SPEC-W16 contract §1): the
	// notification-worker's SendPushNotification activity invokes this via
	// Dapr to fan out to a contact's devices. Tenant resolution is the
	// usual X-Tenant-Slug middleware; no Permify guard (internal only).
	// Response shape is frozen for Agent A.
	r.Route("/internal/devices", func(r chi.Router) {
		r.Use(s.tenantMiddleware)
		r.Get("/", s.devicesHandler((*devices.Handlers).ListInternal))
	})

	return r
}

type server struct {
	d             Deps
	portalLimiter *portalRateLimiter
	promoLimiter  *portalRateLimiter // SPEC-W13 §6: public promo redeem guard
}

func (s *server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.d.Store.Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "db unreachable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// tenantMiddleware resolves the tenant from the X-Tenant-Slug header (or JWT
// tenant claim) via identity-service and enforces JWT tenant membership when
// the token carries the tenant_slugs claim (SPEC §8).
func (s *server) tenantMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims := parseBearerClaims(r.Header.Get("Authorization"))

		slug := r.Header.Get("X-Tenant-Slug")
		if slug == "" {
			slug = claims.firstTenant()
		}
		if slug == "" {
			writeError(w, http.StatusBadRequest, "X-Tenant-Slug header (or JWT tenant_slugs claim) is required")
			return
		}
		// If the token enumerates tenants, the requested one must be among them.
		if len(claims.TenantSlugs) > 0 && !claims.hasTenant(slug) {
			writeError(w, http.StatusForbidden, "token not entitled to tenant "+slug)
			return
		}
		tenant, err := s.d.Resolver.BySlug(r.Context(), slug)
		if err != nil {
			s.d.Logger.Warn("tenant resolution failed", zap.String("slug", slug), zap.Error(err))
			writeError(w, http.StatusNotFound, "tenant not found")
			return
		}
		ctx := context.WithValue(r.Context(), ctxTenant, tenant)
		ctx = context.WithValue(ctx, ctxUser, claims.Sub)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// require returns middleware enforcing a Permify permission
// (manage_catalog / manage_bookings) on organization:{tenantID} for the JWT
// subject, per SPEC §8.
func (s *server) require(permission string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if s.d.AuthzDisabled {
				next.ServeHTTP(w, r)
				return
			}
			tenant := tenantFrom(r.Context())
			userID := userFrom(r.Context())
			if userID == "" {
				writeError(w, http.StatusUnauthorized, "authenticated subject required (JWT sub or X-User-Id)")
				return
			}
			allowed, err := s.d.Authz.Check(r.Context(), tenant.ID.String(),
				"user:"+userID, permission, "organization:"+tenant.ID.String())
			if err != nil {
				if s.d.AuthzOutagePolicy == AuthzFailOpen {
					s.d.Logger.Error("CRITICAL: permify unreachable, allowing request (AUTHZ_OUTAGE_POLICY=fail_open) — dev only",
						zap.String("tenant_id", tenant.ID.String()), zap.String("user", userID),
						zap.String("permission", permission), zap.Error(err))
					next.ServeHTTP(w, r)
					return
				}
				s.d.Logger.Error("permify check failed", zap.Error(err))
				writeError(w, http.StatusBadGateway, "authorization service error")
				return
			}
			if !allowed {
				writeError(w, http.StatusForbidden, "missing permission "+permission)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// requireReadWrite (SPEC-W19 integrator) dispatches the perms middleware by
// method: GET/HEAD → view_analytics, everything else → manage_bookings
// (contract §3). Used by app packages whose RegisterRoutes applies the
// integrator middleware group-wide over mixed-method routes.
func (s *server) requireReadWrite() func(http.Handler) http.Handler {
	read := s.require("view_analytics")
	write := s.require("manage_bookings")
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				read(next).ServeHTTP(w, r)
				return
			}
			write(next).ServeHTTP(w, r)
		})
	}
}

// appGateChain (SPEC-W19 integrator) returns the appgate entitlement
// middleware for appID followed by any extra middleware — an empty slice
// prefix when the gate is not configured (nil Deps.AppGate keeps behavior
// identical to pre-W18; with APP_GATE_ENABLED=false the middleware itself
// is a pass-through).
func (s *server) appGateChain(appID string, extra ...func(http.Handler) http.Handler) []func(http.Handler) http.Handler {
	var chain []func(http.Handler) http.Handler
	if s.d.AppGate != nil {
		chain = append(chain, s.d.AppGate.Middleware(appID))
	}
	return append(chain, extra...)
}

func tenantFrom(ctx context.Context) bookingops.TenantInfo {
	t, _ := ctx.Value(ctxTenant).(bookingops.TenantInfo)
	return t
}

// geoHandler adapts a geo.Handlers method to http.HandlerFunc, injecting
// the tenant context. Answers 503 when geo is not configured (Deps.Geo
// nil) so partial deployments keep the rest of the API intact.
func (s *server) geoHandler(fn func(*geo.Handlers, http.ResponseWriter, *http.Request, bookingops.TenantInfo)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.d.Geo == nil {
			writeError(w, http.StatusServiceUnavailable, "geo features unavailable")
			return
		}
		fn(s.d.Geo, w, r, tenantFrom(r.Context()))
	}
}

func userFrom(ctx context.Context) string {
	u, _ := ctx.Value(ctxUser).(string)
	return u
}

// devicesHandler adapts a devices.Handlers method to http.HandlerFunc,
// injecting the tenant context (SPEC-W16 Agent B; same pattern as
// geoHandler). Answers 503 when the devices store is not configured.
func (s *server) devicesHandler(fn func(*devices.Handlers, http.ResponseWriter, *http.Request, bookingops.TenantInfo)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.d.Devices == nil {
			writeError(w, http.StatusServiceUnavailable, "devices unavailable")
			return
		}
		fn(s.d.Devices, w, r, tenantFrom(r.Context()))
	}
}

// fieldCaptureHandler adapts POST /v1/field/capture (SPEC-W16 Agent B).
// Answers 503 when the field-capture service is not configured.
func (s *server) fieldCaptureHandler(w http.ResponseWriter, r *http.Request) {
	if s.d.FieldCapture == nil {
		writeError(w, http.StatusServiceUnavailable, "field capture unavailable")
		return
	}
	s.d.FieldCapture.Capture(w, r, tenantFrom(r.Context()))
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *server) internal(w http.ResponseWriter, err error) {
	s.d.Logger.Error("internal error", zap.Error(err))
	writeError(w, http.StatusInternalServerError, "internal error")
}

// mapOpError converts bookingops/store sentinel errors to HTTP statuses.
func (s *server) mapOpError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "conflict")
	case errors.Is(err, bookingops.ErrPhoneRequired):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, bookingops.ErrSlotUnavailable):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, bookingops.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		s.internal(w, err)
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

// decodeOptionalJSON decodes a body that may be empty (e.g. POST with no payload).
func decodeOptionalJSON(r *http.Request, v any) error {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	return json.NewDecoder(r.Body).Decode(v)
}

func urlUUID(w http.ResponseWriter, r *http.Request, param string) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, param))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid "+param)
		return uuid.Nil, false
	}
	return id, true
}
