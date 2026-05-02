package secretsproxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// KVStore is a tiny KV-shaped interface a shared CA loader uses to
// publish/fetch the CA cert and key. Backed by Azure Key Vault, AWS Secrets
// Manager, or anything else that maps a string name to an opaque blob.
//
// Implementations must:
//   - Get returns ErrNotFound (or any error) when the name doesn't exist;
//     the caller treats both as "go generate".
//   - Set is allowed to overwrite. The shared-CA loader handles races by
//     re-reading after writing — last writer wins, then everyone converges
//     on what's actually in the store.
type KVStore interface {
	Get(ctx context.Context, name string) ([]byte, error)
	Set(ctx context.Context, name string, value []byte) error
}

// ErrNotFound is the canonical "this key doesn't exist" sentinel that KVStore
// implementations should return when a Get misses. Callers can check via
// errors.Is — though the loader treats any Get error as a miss anyway.
var ErrNotFound = errors.New("secretsproxy: KV key not found")

// LoadOrCreateSharedCA fetches the proxy CA from a region-scoped KV, or
// generates one and publishes it. Required for live migration: each worker
// in the region MUST present the same CA so a sandbox migrated between
// workers continues to trust the proxy's MITM TLS certs (the source's CA
// cert is baked into the guest's trust store — if the destination's proxy
// presents certs signed by a different CA, every outbound HTTPS call after
// migration fails with "authority and subject key identifier mismatch").
//
// The function is idempotent under concurrent worker startup: multiple
// workers booting at once may each generate locally, but after the
// re-fetch step they all converge on whatever cert ended up in KV.
//
// fallbackDir is mirrored from KV so an operator can inspect the cert with
// `openssl x509` and so the worker has a copy if KV becomes briefly
// unreachable later.
func LoadOrCreateSharedCA(ctx context.Context, kv KVStore, certName, keyName, fallbackDir string) (*CA, error) {
	if kv == nil {
		// No shared store configured — fall back to per-worker behavior.
		// Live migration of secrets-proxy-using sandboxes will TLS-fail
		// across worker boundaries, but at least single-worker setups
		// keep working. Production deployments should always pass a KV.
		return LoadOrCreateCA(fallbackDir)
	}
	if err := os.MkdirAll(fallbackDir, 0700); err != nil {
		return nil, fmt.Errorf("create CA dir: %w", err)
	}

	// 1. Try to fetch the existing shared CA.
	if certPEM, keyPEM, err := fetchSharedCA(ctx, kv, certName, keyName); err == nil {
		ca, parseErr := parseCA(certPEM, keyPEM)
		if parseErr != nil {
			// The KV value exists but is corrupted. Refusing here is
			// safer than regenerating (which would silently invalidate
			// every existing sandbox's trust store).
			return nil, fmt.Errorf("shared CA in KV is malformed: %w", parseErr)
		}
		if writeErr := mirrorCAToDir(fallbackDir, certPEM, keyPEM); writeErr != nil {
			// Non-fatal — we have a valid CA in memory.
			fmt.Fprintf(os.Stderr, "secretsproxy: mirror CA to %s failed: %v\n", fallbackDir, writeErr)
		}
		return ca, nil
	}

	// 2. KV miss — generate a fresh CA.
	certPEM, keyPEM, err := generateCAPEM()
	if err != nil {
		return nil, fmt.Errorf("generate shared CA: %w", err)
	}

	// 3. Publish to KV. If publish fails we continue with the locally-
	// generated CA so the worker can still serve sandboxes — but live
	// migration to other workers will still mismatch certs until the
	// publish succeeds on a future restart.
	pubCertErr := kv.Set(ctx, certName, certPEM)
	pubKeyErr := kv.Set(ctx, keyName, keyPEM)
	if pubCertErr != nil || pubKeyErr != nil {
		fmt.Fprintf(os.Stderr, "secretsproxy: publishing shared CA to KV failed (cert=%v key=%v) — using local CA only\n", pubCertErr, pubKeyErr)
		ca, parseErr := parseCA(certPEM, keyPEM)
		if parseErr != nil {
			return nil, fmt.Errorf("parse generated CA: %w", parseErr)
		}
		_ = mirrorCAToDir(fallbackDir, certPEM, keyPEM)
		return ca, nil
	}

	// 4. Race resolution: another worker may have published a CA between
	// our miss and our Set. Re-fetch and prefer whatever's actually in
	// the store as canonical. This converges all racing workers onto a
	// single CA without needing a distributed lock.
	if remoteCert, remoteKey, refetchErr := fetchSharedCA(ctx, kv, certName, keyName); refetchErr == nil {
		if !pemEqual(remoteCert, certPEM) || !pemEqual(remoteKey, keyPEM) {
			ca, parseErr := parseCA(remoteCert, remoteKey)
			if parseErr == nil {
				_ = mirrorCAToDir(fallbackDir, remoteCert, remoteKey)
				return ca, nil
			}
		}
	}

	ca, parseErr := parseCA(certPEM, keyPEM)
	if parseErr != nil {
		return nil, fmt.Errorf("parse generated CA: %w", parseErr)
	}
	_ = mirrorCAToDir(fallbackDir, certPEM, keyPEM)
	return ca, nil
}

func fetchSharedCA(ctx context.Context, kv KVStore, certName, keyName string) ([]byte, []byte, error) {
	certPEM, certErr := kv.Get(ctx, certName)
	if certErr != nil {
		return nil, nil, certErr
	}
	keyPEM, keyErr := kv.Get(ctx, keyName)
	if keyErr != nil {
		return nil, nil, keyErr
	}
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		return nil, nil, ErrNotFound
	}
	return certPEM, keyPEM, nil
}

func generateCAPEM() (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate key: %w", err)
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
		return nil, nil, fmt.Errorf("create cert: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal key: %w", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

var mirrorMu sync.Mutex

func mirrorCAToDir(dir string, certPEM, keyPEM []byte) error {
	mirrorMu.Lock()
	defer mirrorMu.Unlock()
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), certPEM, 0644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "ca.key"), keyPEM, 0600)
}

func pemEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
