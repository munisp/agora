package campaignstudio

// Route registration per the SPEC-W19 anti-collision architecture: the
// package exposes RegisterRoutes(r chi.Router, d *Deps, mw ...) and the
// INTEGRATOR wires it (no server.go/main.go/config.go edits by the
// builder). Suggested wiring (mirroring the devices/geo adapters):
//
//	studioStore, err := campaignstudio.DialStore(ctx, cfg.StudioDatabaseURL) // falls back to DATABASE_URL
//	var studioStarter campaignstudio.SendStarter
//	if tc != nil {
//	    studioStarter = campaignstudio.TemporalStarter{Client: tc.Underlying(), TaskQueue: cfg.TemporalTaskQueue}
//	    campaignstudio.RegisterWorker(w, &campaignstudio.SendActivities{Store: studioStore, Logger: logger})
//	}
//	campaignstudio.RegisterRoutes(r, &campaignstudio.Deps{
//	    Store:             studioStore,
//	    Logger:            logger,
//	    Starter:           studioStarter, // nil → send steps defer
//	    TenantFromContext: tenantFrom,    // httpapi's existing helper
//	    RequireRead:       s.require("view_analytics"),
//	    RequireWrite:      s.require("manage_bookings"),
//	    UsageTopic:        cfg.UsageEventsTopic,
//	    EventsTopic:       campaignstudio.DefaultEventsTopic,
//	    StepBatchSize:     cfg.StudioStepBatch, // 0 → DefaultStepBatch (200)
//	})
//	// + appgate entitlement gating with app_id "campaign-studio"

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/opendesk/booking-service/internal/bookingops"
	"go.uber.org/zap"
)

// Deps carries everything RegisterRoutes needs (integrator-wired).
type Deps struct {
	// Store is required; RegisterRoutes is a no-op without it (partial
	// deployments keep the rest of the API intact).
	Store *Store
	Log   *zap.Logger
	// Starter dispatches send batches to the StudioSendWorkflow. nil →
	// send steps defer (sends_deferred) instead of erroring.
	Starter SendStarter
	// TenantFromContext resolves the caller's tenant (httpapi's tenantFrom
	// helper). nil → zero tenant (only useful in tests).
	TenantFromContext func(ctx context.Context) bookingops.TenantInfo
	// RequireRead / RequireWrite wrap the per-route handlers with the
	// existing perms middleware (view_analytics / manage_bookings). nil →
	// no extra gating (the integrator SHOULD wire both).
	RequireRead  func(http.Handler) http.Handler
	RequireWrite func(http.Handler) http.Handler
	// UsageTopic is opendesk.usage.events (empty disables the
	// journey_enrolled meter).
	UsageTopic string
	// EventsTopic is opendesk.studio.events.v1 (empty disables lifecycle
	// events; DefaultEventsTopic is the contract value).
	EventsTopic string
	// StepBatchSize caps enrollments advanced per step call (<=0 →
	// DefaultStepBatch).
	StepBatchSize int
}

type handlerFunc func(*Handlers, http.ResponseWriter, *http.Request, bookingops.TenantInfo)

// RegisterRoutes mounts /v1/studio/* on r. The variadic middlewares apply
// to the WHOLE group (integrator: appgate entitlement gating for app_id
// "campaign-studio" goes here); per-route perms come from
// Deps.RequireRead / Deps.RequireWrite.
func RegisterRoutes(r chi.Router, d *Deps, mw ...func(http.Handler) http.Handler) {
	if d == nil || d.Store == nil {
		return
	}
	h := &Handlers{
		Store:       d.Store,
		Log:         d.Log,
		Starter:     d.Starter,
		UsageTopic:  d.UsageTopic,
		EventsTopic: d.EventsTopic,
		StepBatch:   d.StepBatchSize,
	}
	tenantOf := d.TenantFromContext
	if tenantOf == nil {
		tenantOf = func(context.Context) bookingops.TenantInfo { return bookingops.TenantInfo{} }
	}
	adapt := func(perm func(http.Handler) http.Handler, fn handlerFunc) http.Handler {
		inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fn(h, w, r, tenantOf(r.Context()))
		})
		if perm != nil {
			return perm(inner)
		}
		return inner
	}

	r.Route("/v1/studio", func(r chi.Router) {
		r.Use(mw...)
		r.Method(http.MethodGet, "/segments", adapt(d.RequireRead, (*Handlers).ListSegments))
		r.Method(http.MethodPost, "/segments", adapt(d.RequireWrite, (*Handlers).CreateSegment))
		r.Method(http.MethodPatch, "/segments/{id}", adapt(d.RequireWrite, (*Handlers).PatchSegment))
		r.Method(http.MethodPost, "/segments/{id}/count", adapt(d.RequireRead, (*Handlers).CountSegment))

		r.Method(http.MethodGet, "/journeys", adapt(d.RequireRead, (*Handlers).ListJourneys))
		r.Method(http.MethodPost, "/journeys", adapt(d.RequireWrite, (*Handlers).CreateJourney))
		r.Method(http.MethodGet, "/journeys/{id}", adapt(d.RequireRead, (*Handlers).GetJourney))
		r.Method(http.MethodPatch, "/journeys/{id}", adapt(d.RequireWrite, (*Handlers).PatchJourney))
		r.Method(http.MethodPost, "/journeys/{id}/enroll", adapt(d.RequireWrite, (*Handlers).Enroll))
		r.Method(http.MethodPost, "/journeys/{id}/step", adapt(d.RequireWrite, (*Handlers).Step))
		r.Method(http.MethodGet, "/journeys/{id}/stats", adapt(d.RequireRead, (*Handlers).Stats))
	})
}
