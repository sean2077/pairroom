package lanshare

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"time"
)

// Identity is an owner-private key pair. Store it with mode 0600, never return
// it from UI/relay APIs or put it into command-line arguments.
type Identity struct {
	CertificatePEM string `json:"certificate_pem"`
	PrivateKeyPEM  string `json:"private_key_pem"`
}

func NewIdentity() (Identity, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Identity{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return Identity{}, err
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "PairRoom LAN"}, NotBefore: now.Add(-5 * time.Minute), NotAfter: now.AddDate(5, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, priv)
	if err != nil {
		return Identity{}, err
	}
	key, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return Identity{}, err
	}
	return Identity{CertificatePEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), PrivateKeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}))}, nil
}
func (i Identity) Certificate() (tls.Certificate, error) {
	return tls.X509KeyPair([]byte(i.CertificatePEM), []byte(i.PrivateKeyPEM))
}
func (i Identity) Fingerprint() (string, error) {
	c, err := i.Certificate()
	if err != nil {
		return "", err
	}
	cert, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return "", err
	}
	return Fingerprint(cert), nil
}
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}
func ValidFingerprint(value string) bool {
	b, err := hex.DecodeString(value)
	return err == nil && len(b) == sha256.Size && len(value) == sha256.Size*2
}

func NewClient(invite Invite, identity Identity) (*http.Client, error) {
	if err := invite.Validate(); err != nil {
		return nil, err
	}
	cert, err := identity.Certificate()
	if err != nil {
		return nil, err
	}
	config := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert},
		// The public invitation authenticates the exact SPKI. VerifyConnection
		// runs for full AND resumed handshakes; OS CAs and DNS names confer no
		// authority on this private self-signed transport.
		InsecureSkipVerify: true, //nolint:gosec // Pinned SPKI and validity checked below, including session resumption.
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) != 1 {
				return errors.New("unexpected LAN server certificate chain")
			}
			leaf := cs.PeerCertificates[0]
			pin := Fingerprint(leaf)
			now := time.Now()
			if subtle.ConstantTimeCompare([]byte(pin), []byte(invite.HostPin)) != 1 || now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
				return errors.New("LAN server identity does not match invitation")
			}
			return nil
		},
	}
	return &http.Client{Transport: &http.Transport{Proxy: nil, TLSClientConfig: config, MaxIdleConnsPerHost: 4, ResponseHeaderTimeout: 40 * time.Second}, Timeout: 45 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}

func ServerTLS(identity Identity) (*tls.Config, error) {
	cert, err := identity.Certificate()
	if err != nil {
		return nil, err
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, ClientAuth: tls.RequireAnyClientCert,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) != 1 {
				return errors.New("one room client certificate required")
			}
			leaf := cs.PeerCertificates[0]
			now := time.Now()
			if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
				return errors.New("expired LAN client identity")
			}
			return nil
		},
	}, nil
}
