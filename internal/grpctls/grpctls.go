// Package grpctls provides mTLS configuration for gRPC connections between
// the control plane and workers. Both sides authenticate using certificates
// signed by a shared CA.
//
// Configuration via environment variables:
//
//	OPENSANDBOX_GRPC_TLS_CA   — path to CA certificate (PEM)
//	OPENSANDBOX_GRPC_TLS_CERT — path to own certificate (PEM)
//	OPENSANDBOX_GRPC_TLS_KEY  — path to own private key (PEM)
//
// If any of these are unset, TLS is disabled and insecure credentials are used.
package grpctls

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// ServerCredentials returns gRPC server transport credentials.
// If TLS is not configured, returns insecure credentials.
func ServerCredentials() (credentials.TransportCredentials, error) {
	caPath, certPath, keyPath := paths()
	if caPath == "" || certPath == "" || keyPath == "" {
		return insecure.NewCredentials(), nil
	}

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("grpctls: load server cert: %w", err)
	}

	caPool, err := loadCAPool(caPath)
	if err != nil {
		return nil, err
	}

	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    caPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	}

	return credentials.NewTLS(cfg), nil
}

// ClientCredentials returns gRPC client transport credentials.
// If TLS is not configured, returns insecure credentials.
func ClientCredentials() (credentials.TransportCredentials, error) {
	caPath, certPath, keyPath := paths()
	if caPath == "" || certPath == "" || keyPath == "" {
		return insecure.NewCredentials(), nil
	}

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("grpctls: load client cert: %w", err)
	}

	caPool, err := loadCAPool(caPath)
	if err != nil {
		return nil, err
	}

	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caPool,
		MinVersion:   tls.VersionTLS13,
	}

	return credentials.NewTLS(cfg), nil
}

// Enabled returns true if all three TLS env vars are set.
func Enabled() bool {
	ca, cert, key := paths()
	return ca != "" && cert != "" && key != ""
}

func paths() (ca, cert, key string) {
	return os.Getenv("OPENSANDBOX_GRPC_TLS_CA"),
		os.Getenv("OPENSANDBOX_GRPC_TLS_CERT"),
		os.Getenv("OPENSANDBOX_GRPC_TLS_KEY")
}

func loadCAPool(caPath string) (*x509.CertPool, error) {
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("grpctls: read CA cert: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("grpctls: CA cert %s contains no valid certificates", caPath)
	}
	return pool, nil
}
