package leads

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/store"
)

// 2026-11-21: lga_code/lga_name appear in the funnel event only when known.
func TestFunnelEventLgaCode(t *testing.T) {
	code, name := "NG-LAGOS-IKEJA", "Ikeja"
	l := &store.Lead{ID: uuid.New(), TenantID: uuid.New(), ChannelOfFirstTouch: "field", LgaCode: &code, LgaName: &name}
	m := NewFunnelEvent(l, EventLeadCreated, time.Now()).Map()
	if m["lga_code"] != code || m["lga_name"] != name {
		t.Fatalf("lga fields missing: %v", m)
	}
	m = NewFunnelEvent(&store.Lead{ID: uuid.New(), TenantID: uuid.New()}, EventLeadCreated, time.Now()).Map()
	if _, ok := m["lga_code"]; ok {
		t.Fatal("lga_code must be absent when unknown")
	}
}
