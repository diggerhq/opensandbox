package secretsproxy

import (
	"container/list"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	maxCacheSize  = 10000
	leafCacheTTL  = 1 * time.Hour
	leafCertValid = 24 * time.Hour
	caValidYears  = 10
)

// CA holds a certificate authority used to sign ephemeral per-host TLS
// certificates for the HTTPS MITM secrets proxy.
type CA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte

	mu    sync.Mutex
	cache map[string]*cachedCert
	lru   *list.List
}

type cachedCert struct {
	cert      *tls.Certificate
	expiresAt time.Time
	element   *list.Element
	hostname  string
}

// LoadOrCreateCA loads the CA cert+key from dir, or generates a new one.
// The CA persists across worker restarts so sandboxes with the cert baked
// in continue to trust it after a reboot.
func LoadOrCreateCA(dir string) (*CA, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create CA dir: %w", err)
	}

	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")

	// Try loading existing CA
	certPEM, err := os.ReadFile(certPath)
	if err == nil {
		keyPEM, err := os.ReadFile(keyPath)
		if err == nil {
			ca, err := parseCA(certPEM, keyPEM)
			if err == nil {
				return ca, nil
			}
		}
	}

	// Generate new CA
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}

	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "OpenSandbox Proxy CA",
			Organization: []string{"OpenSandbox"},
		},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(caValidYears * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create CA cert: %w", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal CA key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := os.WriteFile(certPath, certPEM, 0644); err != nil {
		return nil, fmt.Errorf("write CA cert: %w", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		return nil, fmt.Errorf("write CA key: %w", err)
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("parse CA cert: %w", err)
	}

	return &CA{
		cert:    cert,
		key:     key,
		certPEM: certPEM,
		cache:   make(map[string]*cachedCert),
		lru:     list.New(),
	}, nil
}

// CertPEM returns the CA certificate PEM for injection into sandboxes.
func (ca *CA) CertPEM() []byte {
	return ca.certPEM
}

// SignHost returns a TLS certificate signed by this CA for the given hostname.
// Results are cached with TTL and LRU eviction.
func (ca *CA) SignHost(hostname string) (*tls.Certificate, error) {
	ca.mu.Lock()
	defer ca.mu.Unlock()

	// Check cache
	if entry, ok := ca.cache[hostname]; ok {
		if time.Now().Before(entry.expiresAt) {
			ca.lru.MoveToFront(entry.element)
			return entry.cert, nil
		}
		// Expired — remove
		ca.lru.Remove(entry.element)
		delete(ca.cache, hostname)
	}

	// Generate leaf cert (ECDSA P-256 for speed)
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate leaf key: %w", err)
	}

	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: hostname},
		DNSNames:     []string{hostname},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(leafCertValid),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &leafKey.PublicKey, ca.key)
	if err != nil {
		return nil, fmt.Errorf("sign leaf cert: %w", err)
	}

	tlsCert := &tls.Certificate{
		Certificate: [][]byte{certDER, ca.cert.Raw},
		PrivateKey:  leafKey,
	}

	// Evict if at capacity
	for ca.lru.Len() >= maxCacheSize {
		oldest := ca.lru.Back()
		if oldest == nil {
			break
		}
		evicted := oldest.Value.(*cachedCert)
		ca.lru.Remove(oldest)
		delete(ca.cache, evicted.hostname)
	}

	// Cache
	entry := &cachedCert{
		cert:      tlsCert,
		expiresAt: time.Now().Add(leafCacheTTL),
		hostname:  hostname,
	}
	entry.element = ca.lru.PushFront(entry)
	ca.cache[hostname] = entry

	return tlsCert, nil
}

func parseCA(certPEM, keyPEM []byte) (*CA, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, fmt.Errorf("no PEM cert block")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, err
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, fmt.Errorf("no PEM key block")
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, err
	}
	return &CA{
		cert:    cert,
		key:     key,
		certPEM: certPEM,
		cache:   make(map[string]*cachedCert),
		lru:     list.New(),
	}, nil
}
