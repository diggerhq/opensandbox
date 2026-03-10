package certmanager

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/opensandbox/opensandbox/internal/dns"
	"golang.org/x/crypto/acme"
)

// Config holds the configuration for the cert manager.
type Config struct {
	// Domain to obtain a wildcard cert for, e.g. "workers.opencomputer.dev"
	// The cert will cover "*.workers.opencomputer.dev"
	Domain string

	// Route53 hosted zone ID for DNS-01 challenges
	HostedZoneID string

	// S3 storage for the cert
	S3Bucket string
	S3Prefix string // default "certs/wildcard/"
	S3Region string

	// AWS credentials (empty = use IAM role)
	AccessKeyID    string
	SecretAccessKey string

	// Let's Encrypt account email
	ACMEEmail string

	// ACME directory URL (empty = Let's Encrypt production)
	ACMEDirectory string
}

// CertManager handles obtaining and renewing wildcard TLS certificates
// via Let's Encrypt DNS-01 challenges using Route53.
type CertManager struct {
	cfg      Config
	s3Client *s3.Client
	dns      *dns.Route53Client
	stop     chan struct{}
}

// NewCertManager creates a new certificate manager.
func NewCertManager(cfg Config) (*CertManager, error) {
	if cfg.S3Prefix == "" {
		cfg.S3Prefix = "certs/wildcard/"
	}
	if cfg.ACMEDirectory == "" {
		cfg.ACMEDirectory = "https://acme-v02.api.letsencrypt.org/directory"
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

	dnsClient, err := dns.NewRoute53Client(dns.Route53Config{
		HostedZoneID:   cfg.HostedZoneID,
		Region:         cfg.S3Region,
		AccessKeyID:    cfg.AccessKeyID,
		SecretAccessKey: cfg.SecretAccessKey,
	})
	if err != nil {
		return nil, fmt.Errorf("create Route53 client: %w", err)
	}

	return &CertManager{
		cfg:      cfg,
		s3Client: s3.NewFromConfig(awsCfg),
		dns:      dnsClient,
		stop:     make(chan struct{}),
	}, nil
}

// ObtainOrRenew checks the existing cert in S3 and renews if needed.
// If no cert exists or the cert expires within 30 days, obtains a new one.
func (m *CertManager) ObtainOrRenew(ctx context.Context) error {
	certPEM, _, err := m.GetCertFromS3(ctx)
	if err == nil {
		block, _ := pem.Decode(certPEM)
		if block != nil {
			cert, parseErr := x509.ParseCertificate(block.Bytes)
			if parseErr == nil && time.Until(cert.NotAfter) > 30*24*time.Hour {
				log.Printf("certmanager: existing cert valid until %s, no renewal needed", cert.NotAfter.Format(time.RFC3339))
				return nil
			}
			if parseErr == nil {
				log.Printf("certmanager: cert expires %s, renewing...", cert.NotAfter.Format(time.RFC3339))
			}
		}
	} else {
		log.Printf("certmanager: no existing cert in S3, obtaining new one...")
	}

	return m.obtain(ctx)
}

// obtain performs the ACME DNS-01 flow to get a wildcard cert.
func (m *CertManager) obtain(ctx context.Context) error {
	accountKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate account key: %w", err)
	}

	client := &acme.Client{
		Key:          accountKey,
		DirectoryURL: m.cfg.ACMEDirectory,
	}

	acct := &acme.Account{Contact: []string{"mailto:" + m.cfg.ACMEEmail}}
	if _, err := client.Register(ctx, acct, acme.AcceptTOS); err != nil {
		return fmt.Errorf("register ACME account: %w", err)
	}

	wildcardDomain := "*." + m.cfg.Domain
	order, err := client.AuthorizeOrder(ctx, acme.DomainIDs(wildcardDomain))
	if err != nil {
		return fmt.Errorf("authorize order: %w", err)
	}

	// Process DNS-01 challenges
	for _, authzURL := range order.AuthzURLs {
		authz, err := client.GetAuthorization(ctx, authzURL)
		if err != nil {
			return fmt.Errorf("get authorization: %w", err)
		}

		var dns01 *acme.Challenge
		for _, ch := range authz.Challenges {
			if ch.Type == "dns-01" {
				dns01 = ch
				break
			}
		}
		if dns01 == nil {
			return fmt.Errorf("no dns-01 challenge found for %s", authz.Identifier.Value)
		}

		txtValue, err := client.DNS01ChallengeRecord(dns01.Token)
		if err != nil {
			return fmt.Errorf("compute DNS-01 record: %w", err)
		}

		txtName := "_acme-challenge." + m.cfg.Domain
		if err := m.dns.UpsertTXTRecord(ctx, txtName, txtValue, 60); err != nil {
			return fmt.Errorf("create TXT record: %w", err)
		}

		log.Printf("certmanager: waiting for DNS propagation of %s...", txtName)
		time.Sleep(30 * time.Second)

		if _, err := client.Accept(ctx, dns01); err != nil {
			_ = m.dns.DeleteTXTRecord(ctx, txtName, txtValue)
			return fmt.Errorf("accept challenge: %w", err)
		}

		if _, err := client.WaitAuthorization(ctx, authzURL); err != nil {
			_ = m.dns.DeleteTXTRecord(ctx, txtName, txtValue)
			return fmt.Errorf("wait authorization: %w", err)
		}

		_ = m.dns.DeleteTXTRecord(ctx, txtName, txtValue)
	}

	// Generate cert key and CSR
	certKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate cert key: %w", err)
	}

	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		DNSNames: []string{wildcardDomain, m.cfg.Domain},
	}, certKey)
	if err != nil {
		return fmt.Errorf("create CSR: %w", err)
	}

	der, _, err := client.CreateOrderCert(ctx, order.FinalizeURL, csr, true)
	if err != nil {
		return fmt.Errorf("create order cert: %w", err)
	}

	// Encode cert chain
	var certBuf bytes.Buffer
	for _, d := range der {
		pem.Encode(&certBuf, &pem.Block{Type: "CERTIFICATE", Bytes: d})
	}

	// Encode key
	keyDER, err := x509.MarshalECPrivateKey(certKey)
	if err != nil {
		return fmt.Errorf("marshal key: %w", err)
	}
	var keyBuf bytes.Buffer
	pem.Encode(&keyBuf, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	// Upload to S3
	if err := m.uploadToS3(ctx, m.cfg.S3Prefix+"cert.pem", certBuf.Bytes()); err != nil {
		return fmt.Errorf("upload cert to S3: %w", err)
	}
	if err := m.uploadToS3(ctx, m.cfg.S3Prefix+"key.pem", keyBuf.Bytes()); err != nil {
		return fmt.Errorf("upload key to S3: %w", err)
	}

	log.Printf("certmanager: wildcard cert for *.%s obtained and uploaded to S3", m.cfg.Domain)
	return nil
}

// StartRenewalLoop runs a background goroutine that checks cert renewal every 12 hours.
func (m *CertManager) StartRenewalLoop(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(12 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := m.ObtainOrRenew(ctx); err != nil {
					log.Printf("certmanager: renewal check failed: %v", err)
				}
			case <-m.stop:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
}

// Stop stops the renewal loop.
func (m *CertManager) Stop() {
	close(m.stop)
}

// GetCertFromS3 downloads the cert and key PEM from S3.
func (m *CertManager) GetCertFromS3(ctx context.Context) (certPEM, keyPEM []byte, err error) {
	certPEM, err = m.downloadFromS3(ctx, m.cfg.S3Prefix+"cert.pem")
	if err != nil {
		return nil, nil, fmt.Errorf("download cert: %w", err)
	}
	keyPEM, err = m.downloadFromS3(ctx, m.cfg.S3Prefix+"key.pem")
	if err != nil {
		return nil, nil, fmt.Errorf("download key: %w", err)
	}
	return certPEM, keyPEM, nil
}

func (m *CertManager) uploadToS3(ctx context.Context, key string, data []byte) error {
	_, err := m.s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &m.cfg.S3Bucket,
		Key:    &key,
		Body:   bytes.NewReader(data),
	})
	return err
}

func (m *CertManager) downloadFromS3(ctx context.Context, key string) ([]byte, error) {
	resp, err := m.s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: &m.cfg.S3Bucket,
		Key:    &key,
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}
