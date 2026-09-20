// identity-service: tenant provisioning, Keycloak/Permify wiring and public
// tenant context for agent session injection (SPEC §7 identity schema).
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/opendesk/identity-service/internal/apps"
	"github.com/opendesk/identity-service/internal/billing"
	"github.com/opendesk/identity-service/internal/config"
	"github.com/opendesk/identity-service/internal/consent"
	"github.com/opendesk/identity-service/internal/daprc"
	"github.com/opendesk/identity-service/internal/httpapi"
	"github.com/opendesk/identity-service/internal/keycloak"
	"github.com/opendesk/identity-service/internal/packs"
	"github.com/opendesk/identity-service/internal/permify"
	"github.com/opendesk/identity-service/internal/store"
	"go.uber.org/zap"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	logger, err := zap.NewProduction()
	if err != nil {
		return err
	}
	defer logger.Sync() //nolint:errcheck

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// SPEC-W43 I-03: INTERNAL_DATABASE_URL (app_identity_internal member) is
	// the RLS-escape pool for tenants-table access; aliases DATABASE_URL when
	// unset (dev superuser bypasses RLS).
	st, err := store.New(ctx, cfg.DatabaseURL, cfg.InternalDatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	// Industry workflow packs (SPEC-CRM §C): loaded + validated at boot from
	// the mounted industries dir; an invalid pack file is fatal.
	registry, err := packs.Load(cfg.IndustriesDir)
	if err != nil {
		return fmt.Errorf("load industry packs: %w", err)
	}
	logger.Info("industry packs loaded",
		zap.String("dir", cfg.IndustriesDir), zap.Strings("packs", registry.IDs()))

	daprClient := daprc.New(cfg.DaprHost, cfg.DaprHTTPPort)

	// SPEC-W12 §4: NDPA consent registry (own store/pool with RLS-enforced
	// consents table; routes registered additively by httpapi).
	consentStore, err := consent.New(ctx, cfg.DatabaseURL, cfg.InternalDatabaseURL)
	if err != nil {
		return err
	}
	defer consentStore.Close()
	// SPEC-W43 I-04 / SPEC-W44 K4: durable erasure outbox relay — publishes
	// ErasureRequested (consent topic) + PrivacyEraseRequested
	// (opendesk.privacy.events) until acknowledged.
	consentRelay := &consent.Relay{
		Repo:         consentStore,
		Events:       daprClient,
		PubSub:       cfg.PubSubName,
		ConsentTopic: cfg.ConsentErasureTopic,
		PrivacyTopic: cfg.PrivacyEventsTopic,
		Logger:       logger,
	}
	go consentRelay.Run(ctx, cfg.ConsentRelayInterval)
	consents := &consent.Handler{
		Repo:         consentStore,
		Tenants:      st,
		Relay:        consentRelay,
		Events:       daprClient,
		PubSub:       cfg.PubSubName,
		ErasureTopic: cfg.ConsentErasureTopic,
		// SPEC-W44 F4 / V2-D3: erasure + consent-record read are gated —
		// K2 service token or K1 tenant-bound subject (dev escape logged).
		InternalToken:      cfg.InternalToken,
		TrustDirectTenancy: cfg.TrustDirectTenancy,
		// SPEC-W45 STK O13: data subjects may ALSO self-serve with a booking
		// portal JWT (contact+tenant bound); unset secret = path unavailable.
		PortalSecret: cfg.PortalSecret,
		Logger:       logger,
	}

	// SPEC-W18 §1/§3: app platform registry. The embedded catalog.yaml is
	// validated and upserted into platform_apps at boot (idempotent); a
	// malformed catalog is boot-fatal (packs.Load idiom).
	appsStore, err := apps.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer appsStore.Close()
	catalog, err := apps.LoadCatalog()
	if err != nil {
		return fmt.Errorf("load app catalog: %w", err)
	}
	n, err := appsStore.EnsureCatalog(ctx, catalog)
	if err != nil {
		return fmt.Errorf("upsert app catalog: %w", err)
	}
	logger.Info("app catalog upserted", zap.Int("apps", n))
	appsHandler := &apps.Handler{
		Repo:    appsStore,
		Tenants: st,
		Authz:   permify.NewHTTPClient(cfg.PermifyURL),
		Publisher: &apps.Publisher{
			Events: daprClient,
			PubSub: cfg.PubSubName,
			Topic:  cfg.AppsLifecycleTopic,
			Logger: logger,
		},
		Logger: logger,
	}

	kcClient := keycloak.New(cfg.KeycloakURL, cfg.KeycloakRealm, cfg.KeycloakClientID, cfg.KeycloakClientSecret)

	// SPEC-W45 K10: realm SMTP bootstrap — PATCH the realm smtpServer map from
	// KC_REALM_SMTP_* so Keycloak's own credentials e-mails (K8
	// execute-actions-email) fire. Fail-soft: the MemberInvited →
	// notification-worker rail (dapr SMTP binding) works regardless.
	if cfg.RealmSMTPHost != "" {
		smtpCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := kcClient.ApplyRealmSMTP(smtpCtx, keycloak.RealmSMTPConfig{
			Host:     cfg.RealmSMTPHost,
			Port:     cfg.RealmSMTPPort,
			From:     cfg.RealmSMTPFrom,
			User:     cfg.RealmSMTPUser,
			Password: cfg.RealmSMTPPassword,
		})
		cancel()
		if err != nil {
			logger.Warn("realm SMTP bootstrap failed (invite e-mail via notification-worker unaffected)",
				zap.String("host", cfg.RealmSMTPHost), zap.Error(err))
		} else {
			logger.Info("realm SMTP configured", zap.String("host", cfg.RealmSMTPHost), zap.String("from", cfg.RealmSMTPFrom))
		}
	} else {
		logger.Info("KC_REALM_SMTP_HOST unset — realm SMTP bootstrap skipped (invite e-mail via notification-worker rail)")
	}

	// SPEC-W45 K contract note: best-effort billing plan push. Disabled when
	// BILLING_URL is unset (dev default); token-less push fails closed at
	// the peer (logged loudly here, ERROR per failed push at request time).
	var billingClient *billing.Client
	if cfg.BillingURL != "" {
		billingClient = billing.New(cfg.BillingURL, cfg.BillingInternalToken)
		if cfg.BillingInternalToken == "" {
			logger.Warn("BILLING_URL set but BILLING_INTERNAL_TOKEN empty: billing plan push will fail closed (peer 503/401)")
		}
	} else {
		logger.Info("BILLING_URL unset — billing plan push disabled (TenantPlanChanged event remains the sync path)")
	}

	deps := httpapi.Deps{
		Store:             st,
		Keycloak:          kcClient,
		Permify:           permify.NewHTTPClient(cfg.PermifyURL),
		Dapr:              daprClient,
		PubSub:            cfg.PubSubName,
		Topic:             cfg.IdentityEventsTopic,
		NotificationAppID: cfg.NotificationAppID,
		Packs:             registry,
		Logger:            logger,
		Consents:          consents,
		Apps:              appsHandler,
		InternalToken:     cfg.InternalToken,
		PlatformAdmins:    cfg.PlatformAdmins,
		Billing:           billingClient,
	}

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           httpapi.NewRouter(deps),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("identity-service listening", zap.Int("port", cfg.Port))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
	case err := <-errCh:
		return err
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	logger.Info("shutting down")
	return srv.Shutdown(shutCtx)
}
