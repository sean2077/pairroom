package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/sqlstore"
)

func enterpriseTestCertificate(t *testing.T, hostname string, before, after time.Time) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: hostname}, DNSNames: []string{hostname}, NotBefore: before, NotAfter: after, BasicConstraintsValid: true, IsCA: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir()
	certPath, keyPath := filepath.Join(path, "server.pem"), filepath.Join(path, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func TestEnterpriseTLSRefusesInvalidIdentityBeforeDatabase(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name, hostname string
		before, after  time.Time
	}{
		{"wrong host", "other.example.test", now.Add(-time.Hour), now.Add(time.Hour)},
		{"expired", "pairroom.example.test", now.Add(-2 * time.Hour), now.Add(-time.Hour)},
		{"not yet valid", "pairroom.example.test", now.Add(time.Hour), now.Add(2 * time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testEnterpriseConfig()
			cfg.TLSCertificateFile, cfg.TLSKeyFile = enterpriseTestCertificate(t, tc.hostname, tc.before, tc.after)
			// The real serve entrypoint must reject TLS before attempting even
			// to parse the deliberately invalid database connection string.
			err := serveEnterprise(context.Background(), cfg, sqlstore.Config{DSN: "invalid DSN private-test-value"})
			if err == nil || err.Error() != "enterprise TLS certificate does not cover the public origin or its validity window" {
				t.Fatalf("TLS rejection did not precede database open: %v", err)
			}
		})
	}
	cfg := testEnterpriseConfig()
	cfg.TLSCertificateFile, cfg.TLSKeyFile = enterpriseTestCertificate(t, "pairroom.example.test", now.Add(-time.Hour), now.Add(time.Hour))
	if cert, err := loadEnterpriseCertificate(cfg); err != nil || len(cert.Certificate) != 1 || cert.PrivateKey == nil {
		t.Fatalf("valid configured certificate rejected: %v", err)
	}
	_, unrelatedKey := enterpriseTestCertificate(t, "pairroom.example.test", now.Add(-time.Hour), now.Add(time.Hour))
	cfg.TLSKeyFile = unrelatedKey
	if _, err := loadEnterpriseCertificate(cfg); err == nil || strings.Contains(err.Error(), unrelatedKey) {
		t.Fatal("mismatched key accepted or private path exposed")
	}
}

func TestEnterprisePrivateCAIsExplicitAndBounded(t *testing.T) {
	cfg := testEnterpriseConfig()
	if roots, err := cfg.providerRoots(); err != nil || roots != nil {
		t.Fatal("unconfigured private CA changed system trust selection")
	}
	now := time.Now()
	certPath, _ := enterpriseTestCertificate(t, "idp.example.test", now.Add(-time.Hour), now.Add(time.Hour))
	cfg.OIDC.CACertificateFile = certPath
	roots, err := cfg.providerRoots()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(data)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cert.Verify(x509.VerifyOptions{DNSName: "idp.example.test", Roots: roots}); err != nil {
		t.Fatalf("explicit private root was not added: %v", err)
	}
	for _, data := range [][]byte{[]byte("invalid private-test-value"), []byte(strings.Repeat("x", (1<<20)+1))} {
		path := filepath.Join(t.TempDir(), "invalid-ca.pem")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		cfg.OIDC.CACertificateFile = path
		if _, err := cfg.providerRoots(); err == nil || strings.Contains(err.Error(), "private-test-value") || strings.Contains(err.Error(), path) {
			t.Fatal("invalid private CA accepted or echoed")
		}
	}
}

func TestEnterpriseSharedIdentityAndProviderValidationPrecedeDatabase(t *testing.T) {
	cfg := testEnterpriseConfig()
	cfg.OIDC.Issuer += "/" + strings.Repeat("x", 2048)
	err := initializeEnterprise(context.Background(), cfg, sqlstore.Config{DSN: "invalid DSN private-test-value"}, "administrator")
	if err == nil || err.Error() != "enterprise identity configuration is invalid" {
		t.Fatalf("shared identity validation happened after initialization: %v", err)
	}
	cfg = testEnterpriseConfig()
	now := time.Now()
	cfg.TLSCertificateFile, cfg.TLSKeyFile = enterpriseTestCertificate(t, "pairroom.example.test", now.Add(-time.Hour), now.Add(time.Hour))
	cfg.OIDC.ClientSecretEnv = ""
	cfg.OIDC.AllowedOrigins = nil
	err = serveEnterprise(context.Background(), cfg, sqlstore.Config{DSN: "invalid DSN private-test-value"})
	if err == nil || err.Error() != "enterprise OIDC provider configuration or discovery is unavailable" {
		t.Fatalf("shared provider validation happened after database open: %v", err)
	}
}
