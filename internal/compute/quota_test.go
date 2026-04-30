package compute

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsAzureQuotaErr(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		isQuota bool
	}{
		{"nil", nil, false},
		{"unrelated", errors.New("network timeout"), false},
		{"QuotaExceeded code", errors.New("ERROR CODE: QuotaExceeded\noperation failed"), true},
		{"OperationNotAllowed", errors.New("OperationNotAllowed: regional cores"), true},
		{"SkuNotAvailable", errors.New("SkuNotAvailable: Standard_D16ads_v7 in eastus2"), true},
		{"AllocationFailed", errors.New("AllocationFailed: capacity unavailable"), true},
		{"ZonalAllocationFailed", errors.New("ZonalAllocationFailed in zone 1"), true},
		{"OverconstrainedAllocationRequest", errors.New("OverconstrainedAllocationRequest"), true},
		{"approved quota text", errors.New("operation results in exceeding approved quota"), true},
		{"regional cores phrasing", errors.New("Operation could not be completed as it results in exceeding approved Total Regional Cores quota"), true},
		{"wrapped quota", fmt.Errorf("azure: create VM failed: %w", errors.New("QuotaExceeded")), true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isAzureQuotaErr(tc.err)
			if got != tc.isQuota {
				t.Fatalf("isAzureQuotaErr(%v) = %v, want %v", tc.err, got, tc.isQuota)
			}
		})
	}
}

func TestIsEC2QuotaErr(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		isQuota bool
	}{
		{"nil", nil, false},
		{"unrelated", errors.New("connection reset"), false},
		{"VcpuLimitExceeded", errors.New("VcpuLimitExceeded: max 32 in region"), true},
		{"InstanceLimitExceeded", errors.New("InstanceLimitExceeded"), true},
		{"InsufficientInstanceCapacity", errors.New("InsufficientInstanceCapacity in us-east-1a"), true},
		{"MaxSpotInstanceCountExceeded", errors.New("MaxSpotInstanceCountExceeded"), true},
		{"Unsupported in AZ", errors.New("Unsupported: c7gd.metal not offered in this AZ"), true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isEC2QuotaErr(tc.err)
			if got != tc.isQuota {
				t.Fatalf("isEC2QuotaErr(%v) = %v, want %v", tc.err, got, tc.isQuota)
			}
		})
	}
}

func TestWrapAzureCreateErrTagsQuota(t *testing.T) {
	quotaSrc := errors.New("AllocationFailed: no capacity")
	wrapped := wrapAzureCreateErr(quotaSrc, "azure: create VM foo failed: %w", quotaSrc)
	if !errors.Is(wrapped, ErrQuotaExceeded) {
		t.Fatalf("expected wrapped quota error to match ErrQuotaExceeded, got %v", wrapped)
	}
	// The original error chain must remain reachable so callers can still log
	// the underlying provider message.
	if !errors.Is(wrapped, quotaSrc) {
		t.Fatalf("expected wrapped error to preserve original chain, got %v", wrapped)
	}
}

func TestWrapAzureCreateErrPassesThroughNonQuota(t *testing.T) {
	src := errors.New("network unreachable")
	wrapped := wrapAzureCreateErr(src, "azure: create VM foo failed: %w", src)
	if errors.Is(wrapped, ErrQuotaExceeded) {
		t.Fatalf("non-quota error should not be tagged as ErrQuotaExceeded: %v", wrapped)
	}
	if !errors.Is(wrapped, src) {
		t.Fatalf("expected wrapped error to preserve original chain, got %v", wrapped)
	}
}

func TestWrapEC2CreateErrTagsQuota(t *testing.T) {
	src := errors.New("VcpuLimitExceeded: max 32 in us-east-1")
	wrapped := wrapEC2CreateErr(src, "ec2: RunInstances failed: %w", src)
	if !errors.Is(wrapped, ErrQuotaExceeded) {
		t.Fatalf("expected wrapped quota error to match ErrQuotaExceeded, got %v", wrapped)
	}
	if !errors.Is(wrapped, src) {
		t.Fatalf("expected wrapped error to preserve original chain, got %v", wrapped)
	}
}
