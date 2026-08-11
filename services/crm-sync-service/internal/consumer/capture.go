// Capture consumer (SPEC-W38 F3): opendesk.conversation.captures ->
// Twenty note with the captured fields on the matched person.
//
// CaptureExtracted is emitted by conversation-service (app/capture.py)
// after a post-call LLM extraction pass against the agent's capture
// schema. This handler mirrors the syncer.go note style (CreateNote +
// /rest/noteTargets link) and its lookup logic: resolve the Twenty person
// from a phone/e-mail found among the captured fields (Twenty lookup
// first, then the sync_map contact_phone fallback written at booking
// sync), exactly like the call-summary note paths.
//
// Dedupe: sync_map kind=capture_note keyed by record_id — a redelivered
// CaptureExtracted for the same capture_records row never creates a
// second note.
//
// Skip+ack (documented) cases — none of these are errors worth a retry:
//   - the event carries no record_id: permanent (poison payload);
//   - the captured data has no phone/e-mail field: no person can be
//     resolved and an orphaned note would be dropped into the tenant's CRM;
//   - the contact refs resolve to no Twenty person (lookup + sync_map
//     miss): the contact was never synced; the note is best-effort.
package consumer

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/opendesk/crm-sync-service/internal/events"
	"github.com/opendesk/crm-sync-service/internal/syncmap"
	"github.com/opendesk/crm-sync-service/internal/twentyc"
	"go.uber.org/zap"
)

// KindCaptureNote maps capture record id -> Twenty note id for the
// captured-fields note (SPEC-W38 F3). Written on note creation so
// redeliveries of the same CaptureExtracted dedupe instead of duplicating.
const KindCaptureNote = "capture_note"

// HandleCapture processes opendesk.conversation.captures (own consumer
// group `crm-sync-capture`, so offsets are independent of the shared
// `crm-sync` group on the other topics).
func (s *Syncer) HandleCapture(ctx context.Context, evt events.CloudEvent) error {
	if evt.Type != events.TypeCaptureExtracted {
		s.Log.Debug("ignoring captures event", zap.String("type", evt.Type))
		return nil
	}
	d, err := events.DataAs[events.CaptureExtractedData](evt)
	if err != nil {
		return permanent(err)
	}
	if d.RecordID == "" {
		return permanent(fmt.Errorf("CaptureExtracted missing record_id"))
	}
	tenantUUID := parseUUID(evt.TenantID)

	// Dedupe via sync_map keyed by record_id (at-least-once delivery).
	m, err := s.Map.Get(ctx, KindCaptureNote, d.RecordID, tenantUUID)
	if err != nil && !errors.Is(err, syncmap.ErrNotFound) {
		return fmt.Errorf("lookup capture_note mapping: %w", err)
	}
	if err == nil && m.TwentyID != "" {
		s.Log.Debug("capture note already created; skipping",
			zap.String("record_id", d.RecordID))
		return nil
	}
	if len(d.Data) == 0 {
		s.Log.Debug("CaptureExtracted with empty data; skipping note",
			zap.String("record_id", d.RecordID))
		return nil
	}

	phone, email := captureContactRefs(d.Data)
	if phone == "" && email == "" {
		s.Log.Debug("captured fields carry no phone/email; skipping note",
			zap.String("record_id", d.RecordID),
			zap.String("conversation_id", d.ConversationID))
		return nil
	}
	personID, err := s.resolvePersonForCapture(ctx, phone, email, tenantUUID)
	if err != nil {
		return fmt.Errorf("resolve person for capture note: %w", err)
	}
	if personID == "" {
		s.Log.Info("no Twenty person for captured contact; capture note skipped",
			zap.String("record_id", d.RecordID))
		return nil
	}
	noteID, err := s.Twenty.CreateNote(ctx, twentyc.CaptureNoteTitle,
		twentyc.CaptureNote(d), personID)
	if err != nil {
		return fmt.Errorf("create capture note: %w", err)
	}
	// A mapping failure is returned (retried) rather than swallowed: losing
	// it risks a duplicate note on redelivery, which is worse.
	if err := s.Map.Put(ctx, KindCaptureNote, d.RecordID, noteID, tenantUUID); err != nil {
		return fmt.Errorf("record capture_note mapping: %w", err)
	}
	s.Log.Info("capture note added",
		zap.String("person_id", personID),
		zap.String("note_id", noteID),
		zap.String("record_id", d.RecordID),
		zap.String("conversation_id", d.ConversationID))
	return nil
}

// resolvePersonForCapture resolves the caller's Person from the captured
// contact refs: Twenty people lookup first (existing FindPerson), then the
// sync_map contact_phone mapping written at booking sync — the same
// two-step lookup as resolvePersonForCall.
func (s *Syncer) resolvePersonForCapture(ctx context.Context, phone, email string, tenantUUID *uuid.UUID) (string, error) {
	personID, err := s.findPersonForNote(ctx, phone, email)
	if err != nil {
		return "", err
	}
	if personID != "" || phone == "" {
		return personID, nil
	}
	m, err := s.Map.Get(ctx, syncmap.KindContactPhone, phone, tenantUUID)
	if errors.Is(err, syncmap.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return m.TwentyID, nil
}

// captureContactRefs picks the first phone and e-mail values out of the
// captured fields: any string-valued key containing "phone" or "email"
// (case-insensitive). Keys are scanned in sorted order for determinism.
func captureContactRefs(data map[string]any) (phone, email string) {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v, ok := data[k].(string)
		if !ok || strings.TrimSpace(v) == "" {
			continue
		}
		lk := strings.ToLower(k)
		switch {
		case phone == "" && strings.Contains(lk, "phone"):
			phone = strings.TrimSpace(v)
		case email == "" && strings.Contains(lk, "email"):
			email = strings.TrimSpace(v)
		}
	}
	return phone, email
}
