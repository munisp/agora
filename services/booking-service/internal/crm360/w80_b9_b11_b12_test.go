package crm360

// SPEC-W80 B9 + B11 + B12:
//   - B9: CreateNote / AddTag validate the contact exists IN-TENANT (404
//     for dangling or cross-tenant contact ids — no orphan rows).
//   - B11: AddTag's fresh-vs-replay decision rides ON CONFLICT DO NOTHING
//     RowsAffected (store returns added=true|false); TagAdded fires only
//     on a real attach.
//   - B12: DELETE /v1/crm/notes/{id} — tenant-scoped delete, 404
//     cross-tenant, NoteDeleted event on a real delete.

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/bookingops"
)

// B11 store-level: both branches of the RowsAffected decision.
func TestAddTagRowsAffectedBranches(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	contactID := addContact(t, st, tenantID, "Ada", "+234801", "ada@example.com")

	added, err := st.AddTag(ctx, tenantID, contactID, "vip")
	if err != nil || !added {
		t.Fatalf("first AddTag = (%v, %v), want (true, nil)", added, err)
	}
	added, err = st.AddTag(ctx, tenantID, contactID, "vip")
	if err != nil || added {
		t.Fatalf("replay AddTag = (%v, %v), want (false, nil) — no event-worthy fresh attach", added, err)
	}
}

// B9: note/tag writes against a dangling or CROSS-TENANT contact id are
// 404 — never silent orphan rows.
func TestB9ContactExistenceValidation(t *testing.T) {
	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme"}
	r, st := testRouter(t, tenant)
	otherTenant := uuid.New()
	// A contact that exists only in ANOTHER tenant.
	crossID := addContact(t, st, otherTenant, "Grace", "+234802", "grace@example.com")
	dangling := uuid.New()

	for _, contactID := range []uuid.UUID{dangling, crossID} {
		base := "/v1/crm/contacts/" + contactID.String()
		rec := do(t, r, tenant.Slug, http.MethodPost, base+"/notes", `{"body":"hello"}`)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("create note on %s = %d (%s), want 404", contactID, rec.Code, rec.Body.String())
		}
		rec = do(t, r, tenant.Slug, http.MethodPost, base+"/tags", `{"tag":"vip"}`)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("add tag on %s = %d (%s), want 404", contactID, rec.Code, rec.Body.String())
		}
	}

	// Sanity: an in-tenant contact still accepts both writes.
	own := addContact(t, st, tenant.ID, "Ada", "+234801", "ada@example.com")
	base := "/v1/crm/contacts/" + own.String()
	if rec := do(t, r, tenant.Slug, http.MethodPost, base+"/notes", `{"body":"hello"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create note in-tenant = %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := do(t, r, tenant.Slug, http.MethodPost, base+"/tags", `{"tag":"vip"}`); rec.Code != http.StatusOK {
		t.Fatalf("add tag in-tenant = %d (%s)", rec.Code, rec.Body.String())
	}
}

// B11 handler-level: TagAdded fires exactly once across a fresh add +
// replay (the atomic RowsAffected decision, not a check-then-act probe).
func TestB11TagAddedEventOnceAcrossReplay(t *testing.T) {
	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme"}
	r, st := testRouter(t, tenant)
	contactID := addContact(t, st, tenant.ID, "Ada", "+234801", "ada@example.com")
	base := "/v1/crm/contacts/" + contactID.String()

	if rec := do(t, r, tenant.Slug, http.MethodPost, base+"/tags", `{"tag":"vip"}`); rec.Code != http.StatusOK {
		t.Fatalf("add tag = %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := do(t, r, tenant.Slug, http.MethodPost, base+"/tags", `{"tag":"vip"}`); rec.Code != http.StatusOK {
		t.Fatalf("replay tag = %d (%s)", rec.Code, rec.Body.String())
	}
	events := outboxEvents(t, st)
	if len(events) != 1 || events[0] != DefaultEventsTopic+" "+EventTypeTagAdded {
		t.Fatalf("outbox events = %v, want exactly one TagAdded", events)
	}
}

// B12: DELETE /v1/crm/notes/{id} — happy path + unknown-id 404 +
// NoteDeleted event; second delete 404s.
func TestB12DeleteNote(t *testing.T) {
	tenant := bookingops.TenantInfo{ID: uuid.New(), Slug: "acme"}
	r, st := testRouter(t, tenant)
	contactID := addContact(t, st, tenant.ID, "Ada", "+234801", "ada@example.com")

	rec := do(t, r, tenant.Slug, http.MethodPost, "/v1/crm/contacts/"+contactID.String()+"/notes",
		`{"body":"to be deleted"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create note = %d (%s)", rec.Code, rec.Body.String())
	}
	created := decodeEnv[struct {
		Note Note `json:"note"`
	}](t, rec)

	// Unknown id → 404 (the tenant-scoped cross-tenant delete — RLS makes
	// it indistinguishable — is covered store-level below).
	rec = do(t, r, tenant.Slug, http.MethodDelete, "/v1/crm/notes/"+uuid.NewString(), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete unknown note = %d, want 404", rec.Code)
	}

	// Happy path: 204, the note is gone, NoteDeleted emitted.
	rec = do(t, r, tenant.Slug, http.MethodDelete, "/v1/crm/notes/"+created.Note.ID.String(), "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete note = %d (%s), want 204", rec.Code, rec.Body.String())
	}
	if _, err := st.GetNote(context.Background(), tenant.ID, created.Note.ID); err != ErrNotFound {
		t.Fatalf("GetNote after delete = %v, want ErrNotFound", err)
	}
	// Second delete → 404.
	rec = do(t, r, tenant.Slug, http.MethodDelete, "/v1/crm/notes/"+created.Note.ID.String(), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("re-delete = %d, want 404", rec.Code)
	}
	// Events: NoteCreated + NoteDeleted exactly once each.
	events := outboxEvents(t, st)
	wantCreated, wantDeleted := DefaultEventsTopic+" "+EventTypeNoteCreated, DefaultEventsTopic+" "+EventTypeNoteDeleted
	if len(events) != 2 || events[0] != wantCreated || events[1] != wantDeleted {
		t.Fatalf("outbox events = %v, want [%s %s]", events, wantCreated, wantDeleted)
	}
}

// B12 store-level: DeleteNote is tenant-scoped — deleting with the WRONG
// tenant id is ErrNotFound and the row survives.
func TestB12DeleteNoteTenantScopedStore(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantA, tenantB := uuid.New(), uuid.New()
	contactA := addContact(t, st, tenantA, "Ada", "+234801", "ada@example.com")

	n := Note{TenantID: tenantA, ContactID: contactA, Body: "scoped"}
	if err := st.CreateNote(ctx, &n); err != nil {
		t.Fatalf("create note: %v", err)
	}
	if err := st.DeleteNote(ctx, tenantB, n.ID); err != ErrNotFound {
		t.Fatalf("cross-tenant DeleteNote = %v, want ErrNotFound", err)
	}
	if _, err := st.GetNote(ctx, tenantA, n.ID); err != nil {
		t.Fatalf("note must survive the cross-tenant delete: %v", err)
	}
	if err := st.DeleteNote(ctx, tenantA, n.ID); err != nil {
		t.Fatalf("DeleteNote = %v", err)
	}
	if _, err := st.GetNote(ctx, tenantA, n.ID); err != ErrNotFound {
		t.Fatalf("GetNote after delete = %v, want ErrNotFound", err)
	}
}
