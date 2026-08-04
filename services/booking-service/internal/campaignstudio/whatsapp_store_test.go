package campaignstudio

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// SPEC-W21 Agent A: step-execution enqueue — AdvanceDue queues whatsapp
// sends with the template payload intact (embedded-postgres store test).

func TestAdvanceDueWhatsApp(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenantID := uuid.New()
	now := time.Now().UTC()

	contactID := seedContact(t, st, tenantID, "Ada Lovelace", "+2348011111111", "ada@example.com", "twenty")
	noPhoneID := seedContact(t, st, tenantID, "No Phone", "", "np@example.com", "field")

	steps := Steps{
		{Type: StepSend, Kind: KindWhatsApp,
			TemplateName: "vote_reminder", Language: "en_US",
			Params: []string{"{name}", "Ward 3"}},
	}
	j := mkActiveJourney(t, st, tenantID, steps)
	if _, _, err := st.Enroll(ctx, tenantID, j.ID, []uuid.UUID{contactID, noPhoneID}); err != nil {
		t.Fatalf("enroll: %v", err)
	}

	// Dispatch disabled → deferred, nothing queued.
	res, err := st.AdvanceDue(ctx, tenantID, j, now, 100, false)
	if err != nil {
		t.Fatalf("advance (no dispatch): %v", err)
	}
	if !res.SendsDeferred || len(res.Sends) != 0 {
		t.Fatalf("no-dispatch = %+v, want sends_deferred with no queue", res)
	}

	// Dispatch → Ada queued with the full whatsapp payload; NoPhone skipped
	// (missing phone, same discipline as sms); both complete (last step).
	res, err = st.AdvanceDue(ctx, tenantID, j, now, 100, true)
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if len(res.Sends) != 1 || res.Skipped != 1 || res.Completed != 2 {
		t.Fatalf("advance = %+v, want 1 send 1 skip 2 completed", res)
	}
	qs := res.Sends[0]
	if qs.Kind != KindWhatsApp || qs.Phone != "+2348011111111" ||
		qs.TemplateName != "vote_reminder" || qs.Language != "en_US" ||
		len(qs.Params) != 2 || qs.Params[1] != "Ward 3" {
		t.Fatalf("queued whatsapp send mismatch: %+v", qs)
	}
	if qs.ContactID != contactID {
		t.Fatalf("queued contact = %s, want %s", qs.ContactID, contactID)
	}
}
