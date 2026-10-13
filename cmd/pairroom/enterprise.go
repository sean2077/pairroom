package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sean2077/pairroom/internal/enterpriseauth"
	"github.com/sean2077/pairroom/internal/enterpriseservice"
	"github.com/sean2077/pairroom/internal/sqlstore"
)

func runEnterprise(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		fmt.Println("Usage: pairroom enterprise init --config FILE --bootstrap-subject SUBJECT")
		fmt.Println("       pairroom enterprise serve --config FILE")
		fmt.Println("       pairroom enterprise login|status|refresh|logout --service-origin HTTPS_ORIGIN --deployment ID")
		fmt.Println("Initialization explicitly enrolls the selected administrator; serving never bootstraps an administrator.")
		return nil
	}
	if args[0] == "login" || args[0] == "status" || args[0] == "refresh" || args[0] == "logout" {
		return runEnterpriseClient(args)
	}
	if args[0] != "init" && args[0] != "serve" {
		return errors.New("unsupported enterprise command; use enterprise help")
	}
	flags := flag.NewFlagSet("pairroom enterprise "+args[0], flag.ContinueOnError)
	configPath := flags.String("config", "", "operator JSON configuration; secret values are environment references")
	var bootstrap *string
	if args[0] == "init" {
		bootstrap = flags.String("bootstrap-subject", "", "exact OIDC subject explicitly selected as initial administrator")
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected enterprise command arguments")
	}
	if bootstrap != nil && (*bootstrap == "" || len(*bootstrap) > 1024 || !utf8.ValidString(*bootstrap) || strings.ContainsFunc(*bootstrap, unicode.IsControl)) {
		return errors.New("enterprise init requires an exact valid --bootstrap-subject")
	}
	cfg, err := loadEnterpriseConfig(*configPath)
	if err != nil {
		return err
	}
	dsn, err := enterpriseSecretFromEnv(cfg.DatabaseDSNEnv, true)
	if err != nil {
		return err
	}
	storeConfig := sqlstore.Config{DSN: dsn, Schema: cfg.DatabaseSchema, DeploymentID: cfg.DeploymentID, RequireSessionAuth: true}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if bootstrap != nil {
		return initializeEnterprise(ctx, cfg, storeConfig, *bootstrap)
	}
	return serveEnterprise(ctx, cfg, storeConfig)
}

func initializeEnterprise(parent context.Context, cfg enterpriseFileConfig, storeConfig sqlstore.Config, subject string) error {
	ctx, cancel := context.WithTimeout(parent, time.Minute)
	defer cancel()
	identity := sqlstore.IdentityConfig{
		Connections: []sqlstore.TrustConnectionConfig{{ConnectionID: cfg.OIDC.ConnectionID, Issuer: cfg.OIDC.Issuer}},
		Bootstrap:   enterpriseauth.VerifiedIdentity{ConnectionID: cfg.OIDC.ConnectionID, Issuer: cfg.OIDC.Issuer, Subject: subject},
	}
	for _, admitted := range cfg.OIDC.AdmittedSubjects {
		identity.Admitted = append(identity.Admitted, enterpriseauth.VerifiedIdentity{ConnectionID: cfg.OIDC.ConnectionID, Issuer: cfg.OIDC.Issuer, Subject: admitted})
	}
	if err := sqlstore.ValidateIdentityConfig(identity); err != nil {
		return errors.New("enterprise identity configuration is invalid")
	}
	if err := sqlstore.InitializeDeployment(ctx, storeConfig, cfg.OrganizationID); err != nil {
		return errors.New("enterprise database initialization failed; inspect the configured schema and connection")
	}
	store, err := sqlstore.Open(ctx, storeConfig)
	if err != nil {
		return errors.New("enterprise database ownership could not be acquired")
	}
	defer store.Close()
	result, err := store.InitializeIdentity(ctx, identity)
	if err != nil || result.Outcome != sqlstore.OutcomeCommitted && result.Outcome != sqlstore.OutcomeReplayed {
		return errors.New("enterprise identity initialization was not confirmed; no application session was issued")
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
		DeploymentID string           `json:"deployment_id"`
		Outcome      sqlstore.Outcome `json:"outcome"`
	}{cfg.DeploymentID, result.Outcome})
}

func serveEnterprise(ctx context.Context, cfg enterpriseFileConfig, storeConfig sqlstore.Config) error {
	// Validate file-backed TLS before opening persistent state. A proxy mode has
	// already required explicit peer prefixes and a canonical HTTPS origin.
	certificate, err := loadEnterpriseCertificate(cfg)
	if err != nil {
		return err
	}
	roots, err := cfg.providerRoots()
	if err != nil {
		return err
	}
	secret, err := enterpriseSecretFromEnv(cfg.OIDC.ClientSecretEnv, false)
	if err != nil {
		return err
	}
	providerConfig := cfg.providerConfig(secret)
	providerConfig.Outbound.RootCAs = roots
	provider, err := enterpriseauth.NewRecoveringProvider(ctx, providerConfig)
	if err != nil {
		return errors.New("enterprise OIDC provider configuration or discovery is unavailable")
	}
	defer provider.Close()
	store, err := sqlstore.Open(ctx, storeConfig)
	if err != nil {
		return errors.New("enterprise database or exclusive ownership is unavailable")
	}
	defer store.Close()
	if err := store.CheckIdentityConfiguration(ctx, cfg.OrganizationID, cfg.OIDC.ConnectionID, cfg.OIDC.Issuer); err != nil {
		return errors.New("enterprise identity configuration is not initialized or does not match; run the explicit operator initialization first")
	}
	if _, err := store.RecoverDeliveries(ctx); err != nil {
		return errors.New("enterprise startup recovery is not complete; no public listener was opened")
	}
	transactions, err := enterpriseauth.NewMemoryStore(cfg.PendingLoginLimit)
	if err != nil {
		return errors.New("enterprise login transaction store configuration is invalid")
	}
	handler, err := enterpriseservice.New(cfg.httpConfig(), provider, transactions, store)
	if err != nil {
		return errors.New("enterprise HTTP policy or authenticated backend configuration is invalid")
	}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return errors.New("enterprise listen address is unavailable")
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: time.Minute, WriteTimeout: 65 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 32 << 10}
	if cfg.TLSCertificateFile != "" {
		server.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}
	}
	serveErrors := make(chan error, 1)
	go func() {
		if server.TLSConfig != nil {
			serveErrors <- server.ServeTLS(listener, "", "")
		} else {
			serveErrors <- server.Serve(listener)
		}
	}()
	fmt.Println("PairRoom enterprise service ready.")
	select {
	case err := <-serveErrors:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return errors.New("enterprise HTTP listener stopped unexpectedly")
		}
		return nil
	case <-ctx.Done():
		// Shutdown drains accepted HTTP work before releasing the deployment
		// lease. Vendor sessions and endpoint processes are never controlled.
		shutdown, cancel := context.WithTimeout(context.Background(), 70*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return errors.New("enterprise HTTP drain did not finish within its budget")
		}
		return nil
	}
}

func loadEnterpriseCertificate(cfg enterpriseFileConfig) (tls.Certificate, error) {
	if cfg.TLSCertificateFile == "" {
		return tls.Certificate{}, nil
	}
	certificate, err := tls.LoadX509KeyPair(cfg.TLSCertificateFile, cfg.TLSKeyFile)
	if err != nil {
		return tls.Certificate{}, errors.New("enterprise TLS certificate or key is unavailable or invalid")
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	origin, originErr := url.Parse(cfg.PublicOrigin)
	now := time.Now()
	if err != nil || originErr != nil || origin.Hostname() == "" || leaf.VerifyHostname(origin.Hostname()) != nil || now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return tls.Certificate{}, errors.New("enterprise TLS certificate does not cover the public origin or its validity window")
	}
	return certificate, nil
}
