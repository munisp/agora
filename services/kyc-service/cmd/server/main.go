// kyc-service: consent-gated BVN/NIN resolution with tenant-RLS audit and
// com.opendesk.kyc.Resolved CloudEvents (SPEC-W12 §5). Layout mirrors
// booking-service (chi + pgx store bootstrap + Dapr sidecar publish).
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

	"github.com/opendesk/kyc-service/internal/config"
	"github.com/opendesk/kyc-service/internal/daprc"
	"github.com/opendesk/kyc-service/internal/httpapi"
	"github.com/opendesk/kyc-service/internal/store"
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

	st, err := store.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	daprClient := daprc.New(cfg.DaprHost, cfg.DaprHTTPPort)

	// Resolver: live provider by default (SPEC-W34 GF8 — KYC_MOCK defaults
	// off); the deterministic mock is explicit opt-in for dev only and is
	// flagged loudly at startup because it auto-verifies fabricated IDs.
	var resolver httpapi.Resolver = httpapi.MockResolver{}
	if !cfg.Mock {
		if cfg.ProviderURL == "" {
			return fmt.Errorf("KYC_MOCK=0 requires KYC_PROVIDER_URL (no live provider configured)")
		}
		resolver = httpapi.NewLiveResolver(cfg.ProviderURL, cfg.ProviderAPIKey, cfg.ResolveTimeout)
		logger.Info("live KYC provider configured", zap.String("base", cfg.ProviderURL))
	} else {
		logger.Error("CRITICAL: MOCK KYC — NOT FOR PRODUCTION: KYC_MOCK=1 auto-verifies ANY all-digits BVN/NIN (len>=10) without a real provider; set KYC_MOCK=0 with KYC_PROVIDER_URL for production",
			zap.String("kyc_mock", "1"))
	}

	deps := httpapi.Deps{
		Store:       st,
		Consent:     httpapi.NewConsentClient(daprClient, cfg.IdentityAppID, cfg.IdentityBaseURL, cfg.IdentityInternalToken),
		Resolver:    resolver,
		Events:      daprClient,
		PubSub:      cfg.PubSubName,
		EventsTopic: cfg.KYCEventsTopic,
		Logger:      logger,
	}

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           httpapi.NewRouter(deps),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("kyc-service listening", zap.Int("port", cfg.Port))
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
