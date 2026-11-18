package helpdesk

// SPEC-W80 B9 (helpdesk half): CreateTicket validates a caller-supplied
// contact_id exists IN-TENANT — dangling or cross-tenant ids are a 404,
// never an orphaned ticket link. Tickets without a contact_id are
// untouched (the field stays optional).

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/bookingops"
)

// w80ContactsDDL is the minimal contacts mirror (owned by the shared
// booking store in production) the B9 existence check reads.
const w80ContactsDDL = `
CREATE TABLE IF NOT EXISTS contacts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    name TEXT NOT NULL,
    phone TEXT,
    email TEXT,
    notes TEXT NOT NULL DEFAULT ''
);`

func TestB9CreateTicketValidatesContact(t *testing.T) {
	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme"}
	r, st := testRouter(t, tenant)
	if _, err := st.pool.Exec(context.Background(), w80ContactsDDL); err != nil {
		t.Fatalf("contacts mirror: %v", err)
	}

	contactID := uuid.New()
	if _, err := st.pool.Exec(context.Background(),
		`INSERT INTO contacts (id, tenant_id, name) VALUES ($1,$2,'Ada')`, contactID, tenant.ID); err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	crossContact := uuid.New()
	if _, err := st.pool.Exec(context.Background(),
		`INSERT INTO contacts (id, tenant_id, name) VALUES ($1,$2,'Grace')`, crossContact, uuid.New()); err != nil {
		t.Fatalf("seed cross-tenant contact: %v", err)
	}

	// In-tenant contact → 201.
	rec := do(t, r, http.MethodPost, "/v1/helpdesk/tickets",
		`{"subject":"POS offline","channel":"voice","contact_id":"`+contactID.String()+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create with in-tenant contact = %d (%s), want 201", rec.Code, rec.Body.String())
	}

	// Dangling contact id → 404.
	rec = do(t, r, http.MethodPost, "/v1/helpdesk/tickets",
		`{"subject":"POS offline","channel":"voice","contact_id":"`+uuid.NewString()+`"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("create with dangling contact = %d (%s), want 404", rec.Code, rec.Body.String())
	}

	// Cross-tenant contact id → 404 (indistinguishable from missing).
	rec = do(t, r, http.MethodPost, "/v1/helpdesk/tickets",
		`{"subject":"POS offline","channel":"voice","contact_id":"`+crossContact.String()+`"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("create with cross-tenant contact = %d (%s), want 404", rec.Code, rec.Body.String())
	}

	// No contact_id → unchanged posture (still 201).
	rec = do(t, r, http.MethodPost, "/v1/helpdesk/tickets",
		`{"subject":"No contact ticket","channel":"web"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create without contact = %d (%s), want 201", rec.Code, rec.Body.String())
	}
}
