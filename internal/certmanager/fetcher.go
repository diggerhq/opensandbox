package certmanager

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// FetcherConfig holds the configuration for the worker-side cert fetcher.
type FetcherConfig struct {
	S3Bucket       string
	S3Prefix       string // default "certs/wildcard/"
	S3Region       string
	AccessKeyID    string
	SecretAccessKey string

	// Local paths to write cert files (for debugging/inspection)
	LocalCertDir string // e.g. "/data/sandboxes/tls/"
}

// CertFetcher downloads the shared wildcard cert from S3 and provides
// a hot-swappable tls.Certificate via GetCertificate().
type CertFetcher struct {
	cfg      FetcherConfig
	s3Client *s3.Client
	cert     atomic.Pointer[tls.Certificate]
	expiry   atomic.Pointer[time.Time]
	renewer  *CertManager // optional: fallback renewal if server is down
	stop     chan struct{}
}

// NewCertFetcher creates a new cert fetcher for workers.
func NewCertFetcher(cfg FetcherConfig) (*CertFetcher, error) {
	if cfg.S3Prefix == "" {
		cfg.S3Prefix = "certs/wildcard/"
	}

	var opts []func(*awsconfig.LoadOptions) error
	if cfg.S3Region != "" {
		opts = append(opts, awsconfig.WithRegion(cfg.S3Region))
	}
	if cfg.AccessKeyID != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), opts...)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}

	return &CertFetcher{
		cfg:      cfg,
		s3Client: s3.NewFromConfig(awsCfg),
		stop:     make(chan struct{}),
	}, nil
}

// SetRenewer sets an optional CertManager that the fetcher can use to renew
// the cert if it's close to expiry and the server hasn't renewed it.
// This provides redundancy — any worker can renew if the server is down.
func (f *CertFetcher) SetRenewer(cm *CertManager) {
	f.renewer = cm
}

// FetchAndStore downloads the cert from S3, optionally writes to local disk,
// and stores the parsed tls.Certificate for use by GetCertificate.
// Retries up to 3 times with exponential backoff on failure.
func (f *CertFetcher) FetchAndStore(ctx context.Context) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * 5 * time.Second
			log.Printf("certfetcher: retry %d/%d in %s...", attempt+1, 3, backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		err := f.fetchOnce(ctx)
		if err == nil {
			return nil
		}
		lastErr = err
		log.Printf("certfetcher: fetch attempt %d failed: %v", attempt+1, err)
	}
	return fmt.Errorf("all fetch attempts failed: %w", lastErr)
}

func (f *CertFetcher) fetchOnce(ctx context.Context) error {
	certPEM, err := f.downloadFromS3(ctx, f.cfg.S3Prefix+"cert.pem")
	if err != nil {
		return fmt.Errorf("download cert from S3: %w", err)
	}
	keyPEM, err := f.downloadFromS3(ctx, f.cfg.S3Prefix+"key.pem")
	if err != nil {
		return fmt.Errorf("download key from S3: %w", err)
	}

	// Parse the certificate
	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("parse TLS cert: %w", err)
	}

	// Extract expiry from the leaf certificate
	if len(tlsCert.Certificate) > 0 {
		leaf, parseErr := x509.ParseCertificate(tlsCert.Certificate[0])
		if parseErr == nil {
			f.expiry.Store(&leaf.NotAfter)
			log.Printf("certfetcher: cert expires %s (in %s)",
				leaf.NotAfter.Format(time.RFC3339),
				time.Until(leaf.NotAfter).Round(time.Hour))
		}
	}

	// Store atomically for hot-swap
	f.cert.Store(&tlsCert)

	// Optionally write to local disk
	if f.cfg.LocalCertDir != "" {
		if err := os.MkdirAll(f.cfg.LocalCertDir, 0700); err != nil {
			log.Printf("certfetcher: warning: can't create cert dir: %v", err)
		} else {
			_ = os.WriteFile(filepath.Join(f.cfg.LocalCertDir, "cert.pem"), certPEM, 0600)
			_ = os.WriteFile(filepath.Join(f.cfg.LocalCertDir, "key.pem"), keyPEM, 0600)
		}
	}

	log.Printf("certfetcher: cert loaded from S3 (%d bytes cert, %d bytes key)", len(certPEM), len(keyPEM))
	return nil
}

// StartRefreshLoop runs a background goroutine that re-fetches the cert every hour.
// If the cert is within 7 days of expiry and a renewer is configured, it attempts
// renewal directly (fallback for when the server is down).
func (f *CertFetcher) StartRefreshLoop(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(12 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				// Check if cert is critically close to expiry
				if f.renewer != nil {
					if exp := f.expiry.Load(); exp != nil && time.Until(*exp) < 7*24*time.Hour {
						log.Printf("certfetcher: cert expires in %s, attempting fallback renewal", time.Until(*exp).Round(time.Hour))
						if err := f.renewer.ObtainOrRenew(ctx); err != nil {
							log.Printf("certfetcher: fallback renewal failed: %v", err)
						} else {
							log.Printf("certfetcher: fallback renewal succeeded")
						}
					}
				}

				// Always re-fetch from S3 (picks up server-renewed or self-renewed cert)
				if err := f.FetchAndStore(ctx); err != nil {
					log.Printf("certfetcher: refresh failed: %v", err)
				}
			case <-f.stop:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
}

// Stop stops the refresh loop.
func (f *CertFetcher) Stop() {
	close(f.stop)
}

// GetCertificate implements the tls.Config.GetCertificate callback.
// Returns the current cert from the atomic pointer — never blocks TLS handshakes.
func (f *CertFetcher) GetCertificate(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
	cert := f.cert.Load()
	if cert == nil {
		return nil, fmt.Errorf("no TLS certificate available")
	}
	return cert, nil
}

// CertExpiry returns the expiry time of the currently loaded certificate.
// Returns zero time if no cert is loaded.
func (f *CertFetcher) CertExpiry() time.Time {
	if exp := f.expiry.Load(); exp != nil {
		return *exp
	}
	return time.Time{}
}

// HasValidCert returns true if a cert is loaded and not expired.
func (f *CertFetcher) HasValidCert() bool {
	exp := f.expiry.Load()
	return exp != nil && time.Now().Before(*exp)
}

func (f *CertFetcher) downloadFromS3(ctx context.Context, key string) ([]byte, error) {
	resp, err := f.s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: &f.cfg.S3Bucket,
		Key:    &key,
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}
