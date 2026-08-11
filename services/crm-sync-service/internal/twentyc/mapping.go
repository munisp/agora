// Event -> Twenty object mapping (pure functions, unit-tested).
package twentyc

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/opendesk/crm-sync-service/internal/events"
)

// TenantDomain is the synthetic domainName for a tenant's Twenty Company:
// slug + "." + tenantDomainSuffix (SPEC-CRM §B).
func TenantDomain(slug string) string {
	return slug + "." + tenantDomainSuffix
}

// CompanyUpsert is the body for creating/updating a Twenty Company from a
// TenantProvisioned event.
type CompanyUpsert struct {
	Name       string `json:"name"`
	DomainName Links  `json:"domainName"`
}

// Links is Twenty's LINKS field shape (v1.x).
type Links struct {
	PrimaryLinkURL string `json:"primaryLinkUrl"`
}

// CompanyFromTenant maps TenantProvisioned -> Company upsert body.
func CompanyFromTenant(d events.TenantProvisionedData) CompanyUpsert {
	return CompanyUpsert{
		Name:       d.Name,
		DomainName: Links{PrimaryLinkURL: "https://" + TenantDomain(d.Slug)},
	}
}

// PersonUpsert is the body for creating/updating a Twenty Person.
type PersonUpsert struct {
	Name   FullName `json:"name"`
	Emails *Emails  `json:"emails,omitempty"`
	Phones *Phones  `json:"phones,omitempty"`
}

// FullName is Twenty's FULL_NAME field shape.
type FullName struct {
	FirstName string `json:"firstName"`
	LastName  string `json:"lastName"`
}

// Emails is Twenty's EMAILS field shape.
type Emails struct {
	PrimaryEmail string `json:"primaryEmail"`
}

// Phones is Twenty's PHONES field shape.
type Phones struct {
	PrimaryPhoneNumber string `json:"primaryPhoneNumber"`
}

// PersonFromContact maps booking contact fields -> Person upsert body.
// The contact name is split on the first space (first/last).
func PersonFromContact(name, email, phone string) PersonUpsert {
	p := PersonUpsert{Name: SplitName(name)}
	if email != "" {
		p.Emails = &Emails{PrimaryEmail: email}
	}
	if phone != "" {
		p.Phones = &Phones{PrimaryPhoneNumber: phone}
	}
	return p
}

// SplitName splits a display name into first/last at the first space.
func SplitName(name string) FullName {
	name = strings.TrimSpace(name)
	if name == "" {
		return FullName{FirstName: "Unknown"}
	}
	first, last, found := strings.Cut(name, " ")
	if !found {
		return FullName{FirstName: name}
	}
	return FullName{FirstName: first, LastName: strings.TrimSpace(last)}
}

// TaskTitle renders "{offering} appointment at {starts_at}" (SPEC-CRM §B).
// starts_at is rendered RFC3339 in UTC for stable, parseable titles.
func TaskTitle(offering string, startsAt time.Time) string {
	if offering == "" {
		offering = "Appointment"
	}
	return fmt.Sprintf("%s appointment at %s", offering, startsAt.UTC().Format(time.RFC3339))
}

// TaskCreate is the body for creating a Twenty Task.
type TaskCreate struct {
	Title  string `json:"title"`
	Body   string `json:"body,omitempty"`
	DueAt  string `json:"dueAt,omitempty"`
	Status string `json:"status,omitempty"`
}

// TaskFromBooking maps a booking event -> Task create body linked to a person.
func TaskFromBooking(d events.BookingData) TaskCreate {
	return TaskCreate{
		Title: TaskTitle(d.OfferingName, d.StartsAt),
		Body: fmt.Sprintf("OpenDesk booking %s (%s) — contact %s, source %s, price %d %s.",
			d.BookingID, d.Status, d.ContactName, d.Source, d.PriceCents, d.Currency),
		DueAt:  FormatTime(d.StartsAt),
		Status: "TODO",
	}
}

// CancelNote renders the note appended to a task body on BookingCancelled.
func CancelNote(reason string) string {
	if reason == "" {
		reason = "no reason given"
	}
	return "Booking cancelled in OpenDesk: " + reason
}

// AIBookingNote is the Note body for AI-receptionist bookings (SPEC-CRM §B).
const AIBookingNote = "Booked via AI receptionist"

// CallSummaryNoteTitle is the Note title for AI call-quality summaries
// (enriched SessionEnded events on opendesk.conversation.events).
const CallSummaryNoteTitle = "📞 AI call summary"

// CallSummaryNote renders the markdown-ish Note body for an enriched
// SessionEnded call-quality payload, e.g.:
//
//	📞 AI call summary — duration 95s, 6 turns, tools: book_appointment×1,
//	avg LLM 820ms (max 1400ms), escalated: no, fallback used: no
//
// Optional segments are omitted when the payload lacks them (no tool calls,
// no LLM latency samples, no avg sentiment). Sentiment is NOT produced by
// the voice runtime — per-turn sentiment/intent lives in the OpenSearch
// conversations index (conversation-service intel); the field exists only
// for future/event-external enrichment.
func CallSummaryNote(q events.CallQuality) string {
	parts := []string{
		fmt.Sprintf("duration %ds", int(math.Round(q.DurationS))),
		fmt.Sprintf("%d turns", q.TurnCount),
	}
	if len(q.ToolCalls) > 0 {
		names := make([]string, 0, len(q.ToolCalls))
		for name := range q.ToolCalls {
			names = append(names, name)
		}
		sort.Strings(names)
		tools := make([]string, 0, len(names))
		for _, name := range names {
			tools = append(tools, fmt.Sprintf("%s×%d", name, q.ToolCalls[name]))
		}
		parts = append(parts, "tools: "+strings.Join(tools, ", "))
	}
	if q.AvgLLMLatencyMs != nil {
		seg := fmt.Sprintf("avg LLM %dms", *q.AvgLLMLatencyMs)
		if q.MaxLLMLatencyMs != nil {
			seg += fmt.Sprintf(" (max %dms)", *q.MaxLLMLatencyMs)
		}
		parts = append(parts, seg)
	}
	if q.SttCalls > 0 || q.TtsCalls > 0 {
		parts = append(parts, fmt.Sprintf("stt %d calls, tts %d calls", q.SttCalls, q.TtsCalls))
	}
	parts = append(parts, "escalated: "+yesNo(q.Escalated))
	parts = append(parts, "fallback used: "+yesNo(q.LLMFallbackUsed))
	if q.AvgSentiment != nil {
		parts = append(parts, fmt.Sprintf("avg sentiment %.2f", *q.AvgSentiment))
	}
	return CallSummaryNoteTitle + " — " + strings.Join(parts, ", ")
}

// CaptureNoteTitle is the Note title for post-call captured fields
// (SPEC-W38 F3: CaptureExtracted events on opendesk.conversation.captures).
const CaptureNoteTitle = "📋 AI captured fields"

// CaptureNote renders the markdown-ish Note body for a CaptureExtracted
// payload, e.g.:
//
//	📋 AI captured fields — caller_name: Ada, day: Friday, party_size: 4
//
// Keys are sorted for a deterministic body; the capture schema's declared
// keys are what the LLM extraction kept, so every value renders with %v.
func CaptureNote(d events.CaptureExtractedData) string {
	keys := make([]string, 0, len(d.Data))
	for k := range d.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s: %v", k, d.Data[k]))
	}
	return CaptureNoteTitle + " — " + strings.Join(parts, ", ")
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// FormatTime renders a timestamp in Twenty's accepted RFC3339 form (UTC).
func FormatTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// ---------------- reverse-sync record shapes (Twenty -> OpenDesk) ----------------

// PersonRecord is the slice of a Twenty Person we read back on reverse sync.
type PersonRecord struct {
	ID        string   `json:"id"`
	Name      FullName `json:"name"`
	Emails    *Emails  `json:"emails"`
	Phones    *Phones  `json:"phones"`
	CompanyID string   `json:"companyId"`
}

// DisplayName renders "First Last", falling back to "Unknown" (mirrors
// SplitName's fallback so the booking contact always has a name).
func (p PersonRecord) DisplayName() string {
	n := strings.TrimSpace(strings.TrimSpace(p.Name.FirstName) + " " + strings.TrimSpace(p.Name.LastName))
	if n == "" {
		return "Unknown"
	}
	return n
}

// PrimaryEmail returns the person's primary e-mail ("" when unset).
func (p PersonRecord) PrimaryEmail() string {
	if p.Emails == nil {
		return ""
	}
	return p.Emails.PrimaryEmail
}

// PrimaryPhone returns the person's primary phone number ("" when unset).
func (p PersonRecord) PrimaryPhone() string {
	if p.Phones == nil {
		return ""
	}
	return p.Phones.PrimaryPhoneNumber
}

// CompanyRecord is the slice of a Twenty Company used for tenant resolution.
type CompanyRecord struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	DomainName Links  `json:"domainName"`
}

// TaskRecord is the slice of a Twenty Task used by the reverse sync.
type TaskRecord struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

// SlugFromTenantDomain extracts the tenant slug from a Twenty Company's
// domainName URL — the forward syncer writes companies with
// domainName "https://<slug>.opendesk.local" (TenantDomain). Returns
// ("", false) when the URL is not an OpenDesk tenant domain.
func SlugFromTenantDomain(primaryLinkURL string) (string, bool) {
	host := strings.TrimSpace(primaryLinkURL)
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	host, _, _ = strings.Cut(host, "/") // strip any path
	host = strings.TrimSuffix(host, "/")
	slug, ok := strings.CutSuffix(host, "."+tenantDomainSuffix)
	if !ok || slug == "" {
		return "", false
	}
	return slug, true
}

// tenantDomainSuffix is the synthetic parent domain for tenant companies.
const tenantDomainSuffix = "opendesk.local"
