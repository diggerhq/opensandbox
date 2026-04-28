package billing

import (
	"strings"
	"testing"

	"github.com/opensandbox/opensandbox/internal/db"
)

// meterEventNameFor pins the (event_type, memory_mb) → Stripe meter
// routing. Overage is flat — memory_mb is preserved on the outbox row
// for analytics but ignored at ship time so a 1 GB and 64 GB sandbox
// hit the same meter.

func newSenderForTest() *BillableEventsSender {
	stripe := &StripeClient{
		ReservedMeterEventName:    "sandbox_compute_sandbox_reserved",
		OverageMeterEventName:     "sandbox_compute_sandbox_overage",
		DiskOverageMeterEventName: "sandbox_compute_sandbox_disk_overage",
	}
	return &BillableEventsSender{stripe: stripe}
}

func TestSender_meterEventName_reservedFlat(t *testing.T) {
	s := newSenderForTest()
	got, err := s.meterEventNameFor(db.BillableEventReservedUsage, 0)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "sandbox_compute_sandbox_reserved" {
		t.Errorf("got %q", got)
	}
}

func TestSender_meterEventName_overageFlatAcrossTiers(t *testing.T) {
	s := newSenderForTest()
	for _, tier := range []int{1024, 4096, 8192, 16384, 32768, 65536} {
		got, err := s.meterEventNameFor(db.BillableEventOverageUsage, tier)
		if err != nil {
			t.Fatalf("tier %d: err: %v", tier, err)
		}
		if got != "sandbox_compute_sandbox_overage" {
			t.Errorf("tier %d: got %q, want flat overage meter", tier, got)
		}
	}
}

func TestSender_meterEventName_diskOverage(t *testing.T) {
	s := newSenderForTest()
	got, err := s.meterEventNameFor(db.BillableEventDiskOverageUsage, 0)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "sandbox_compute_sandbox_disk_overage" {
		t.Errorf("got %q", got)
	}
}

func TestSender_meterEventName_unknownTypeRejected(t *testing.T) {
	s := newSenderForTest()
	_, err := s.meterEventNameFor("totally_made_up", 0)
	if err == nil || !strings.Contains(err.Error(), "unknown event_type") {
		t.Errorf("expected unknown event_type error, got %v", err)
	}
}

func TestSender_meterEventName_missingProvisionRejected(t *testing.T) {
	// Empty stripe client — no meters provisioned. Sender must error
	// loudly rather than ship to an empty event_name.
	s := &BillableEventsSender{stripe: &StripeClient{}}
	cases := []struct {
		eventType string
		want      string
	}{
		{db.BillableEventReservedUsage, "reserved meter not provisioned"},
		{db.BillableEventOverageUsage, "overage meter not provisioned"},
		{db.BillableEventDiskOverageUsage, "disk overage meter not provisioned"},
	}
	for _, tc := range cases {
		_, err := s.meterEventNameFor(tc.eventType, 0)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("event_type=%s: expected %q, got %v", tc.eventType, tc.want, err)
		}
	}
}
