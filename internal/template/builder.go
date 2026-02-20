package template

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/opensandbox/opensandbox/internal/ecr"
	"github.com/opensandbox/opensandbox/internal/podman"
)

// Builder builds container images from Dockerfiles and pushes to ECR.
type Builder struct {
	podman    *podman.Client
	ecrConfig *ecr.Config // nil if ECR not configured
}

// NewBuilder creates a new template builder.
func NewBuilder(client *podman.Client, ecrConfig *ecr.Config) *Builder {
	return &Builder{
		podman:    client,
		ecrConfig: ecrConfig,
	}
}

// Build builds a template from Dockerfile content, tags it, and pushes to ECR.
// Returns the final ECR image reference and the build log.
func (b *Builder) Build(ctx context.Context, dockerfile, name, tag, ecrImageRef string) (string, string, error) {
	if tag == "" {
		tag = "latest"
	}

	localImage := fmt.Sprintf("localhost/opensandbox-template/%s:%s", name, tag)

	// Write Dockerfile to temp directory
	tmpDir, err := os.MkdirTemp("", "opensandbox-build-*")
	if err != nil {
		return "", "", fmt.Errorf("failed to create temp dir for build: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	dockerfilePath := filepath.Join(tmpDir, "Dockerfile")
	if err := os.WriteFile(dockerfilePath, []byte(dockerfile), 0644); err != nil {
		return "", "", fmt.Errorf("failed to write Dockerfile: %w", err)
	}

	// Build image with podman
	result, err := b.podman.Run(ctx, "build", "-t", localImage, "-f", dockerfilePath, tmpDir)
	if err != nil {
		return "", "", fmt.Errorf("failed to build template %s: %w", name, err)
	}
	if result.ExitCode != 0 {
		return "", "", fmt.Errorf("podman build failed (exit %d): %s", result.ExitCode, result.Stderr)
	}
	buildLog := result.Stdout + result.Stderr

	// If ECR is configured and an image ref was provided, tag and push
	if b.ecrConfig != nil && b.ecrConfig.IsConfigured() && ecrImageRef != "" {
		// Authenticate to ECR
		username, password, err := ecr.GetAuthToken(ctx, b.ecrConfig)
		if err != nil {
			return "", buildLog, fmt.Errorf("failed to get ECR auth: %w", err)
		}

		if err := b.podman.LoginRegistry(ctx, b.ecrConfig.Registry, username, password); err != nil {
			return "", buildLog, fmt.Errorf("failed to login to ECR: %w", err)
		}

		// Tag with ECR reference
		if err := b.podman.TagImage(ctx, localImage, ecrImageRef); err != nil {
			return "", buildLog, fmt.Errorf("failed to tag image for ECR: %w", err)
		}

		// Push to ECR
		log.Printf("template: pushing %s to ECR...", ecrImageRef)
		if err := b.podman.PushImage(ctx, ecrImageRef); err != nil {
			return "", buildLog, fmt.Errorf("failed to push image to ECR: %w", err)
		}
		log.Printf("template: push complete for %s", ecrImageRef)

		return ecrImageRef, buildLog, nil
	}

	// No ECR — return local image reference
	return localImage, buildLog, nil
}

// RefreshECRAuth authenticates podman to ECR. Call periodically (tokens expire after 12h).
func (b *Builder) RefreshECRAuth(ctx context.Context) error {
	if b.ecrConfig == nil || !b.ecrConfig.IsConfigured() {
		return nil
	}

	username, password, err := ecr.GetAuthToken(ctx, b.ecrConfig)
	if err != nil {
		return fmt.Errorf("failed to get ECR auth: %w", err)
	}

	return b.podman.LoginRegistry(ctx, b.ecrConfig.Registry, username, password)
}
