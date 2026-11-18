// Event -> Twenty object mapping (pure functions, unit-tested).
package twentyc

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
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
	PrimaryPhoneNumber      string `json:"primaryPhoneNumber"`
	PrimaryPhoneCountryCode string `json:"primaryPhoneCountryCode,omitempty"`
	PrimaryPhoneCallingCode string `json:"primaryPhoneCallingCode,omitempty"`
}

// NationalPhone splits a phone as callers give it into Twenty's parts. Twenty rejects a local number such as
// "08065553108" with 400 "Provided phone number is invalid", so every booking/call failed to reach the CRM.
// Nigerian numbers (0XXXXXXXXXX, 234XXXXXXXXXX, +234XXXXXXXXXX) become national "XXXXXXXXXX" + NG/+234;
// anything else is passed through unchanged with no country.
func NationalPhone(phone string) (national, country, calling string) {
	raw := strings.TrimSpace(phone)
	var b strings.Builder
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	d := b.String()
	switch {
	case strings.HasPrefix(d, "234") && len(d) == 13:
		return d[3:], "NG", "+234"
	case strings.HasPrefix(d, "0") && len(d) == 11:
		return d[1:], "NG", "+234"
	}
	return raw, "", ""
}

// PersonFromContact maps booking contact fields -> Person upsert body.
// The contact name is split on the first space (first/last).
func PersonFromContact(name, email, phone string) PersonUpsert {
	p := PersonUpsert{Name: SplitName(name)}
	if email != "" {
		p.Emails = &Emails{PrimaryEmail: email}
	}
	if phone != "" && plausiblePhone(phone) {
		national, country, calling := NationalPhone(phone)
		p.Phones = &Phones{PrimaryPhoneNumber: national, PrimaryPhoneCountryCode: country, PrimaryPhoneCallingCode: calling}
	}
	return p
}

// plausiblePhone: a Nigerian number NationalPhone recognises, or an international "+" number of 8-15 digits. Anything
// else (a voice-call mishearing like "080065553108") is left off the Person: Twenty rejects the whole create with 400,
// which dead-lettered the event and lost the contributor, task and call summary.
func plausiblePhone(phone string) bool {
	if _, country, _ := NationalPhone(phone); country != "" {
		return true
	}
	raw := strings.TrimSpace(phone)
	digits := 0
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			digits++
		}
	}
	return strings.HasPrefix(raw, "+") && digits >= 8 && digits <= 15
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

// TaskTitle renders "{offering} — Tue 24 Nov 2026, 9:00 am (Lagos)" in the business's time zone (env
// CRM_TASK_TIMEZONE, default Africa/Lagos). It used to be "… appointment at 2026-11-24T08:00:00Z": a 9 am Lagos visit
// read as 08:00 to the business. Tasks are matched by booking id (syncmap) and the machine time lives in dueAt, so the
// title is only for people.
func TaskTitle(offering string, startsAt time.Time) string {
	if offering == "" {
		offering = "Appointment"
	}
	tzName := os.Getenv("CRM_TASK_TIMEZONE")
	if tzName == "" {
		tzName = "Africa/Lagos"
	}
	loc, err := time.LoadLocation(tzName)
	if err != nil {
		loc = time.UTC
	}
	local := startsAt.In(loc)
	city := tzName
	if i := strings.LastIndex(tzName, "/"); i >= 0 {
		city = strings.ReplaceAll(tzName[i+1:], "_", " ")
	}
	return fmt.Sprintf("%s — %s, %s (%s)", offering, local.Format("Mon 2 Jan 2006"),
		strings.ToLower(local.Format("3:04 PM")), city)
}

// RichText is Twenty's rich-text field (v1.x renamed tasks'/notes' "body" to "bodyV2"; the old field is rejected with
// 400 "Field metadata for field body is missing"). Twenty converts the markdown to its own format.
type RichText struct {
	Markdown string `json:"markdown"`
}

// TaskCreate is the body for creating a Twenty Task.
type TaskCreate struct {
	Title  string    `json:"title"`
	Body   string    `json:"-"`
	BodyV2 *RichText `json:"bodyV2,omitempty"`
	DueAt  string    `json:"dueAt,omitempty"`
	Status string    `json:"status,omitempty"`
}

// MarshalJSON sends Body as Twenty's bodyV2 rich text.
func (t TaskCreate) MarshalJSON() ([]byte, error) {
	type plain TaskCreate
	p := plain(t)
	if p.BodyV2 == nil && p.Body != "" {
		p.BodyV2 = &RichText{Markdown: p.Body}
	}
	return json.Marshal(p)
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

// CallSummaryNoteTitle is the Note title for the call summary written when a
// call ends (SessionEnded / CallQualityEnriched on the conversation topics).
const CallSummaryNoteTitle = "📞 Call summary"

// CallSummaryNote renders the call summary a business owner reads in the CRM:
// the plain-language summary (from callsummary, may be empty) followed by the
// facts that matter to the business, e.g.
//
//	The caller wanted to open a retirement savings account. The receptionist
//	booked an RSA Registration call for Tuesday 24 November at 9:00 am.
//
//	- Call length: 5 min 42 s
//	- Phone confirmed: 0806 555 3108
//	- Appointment booked: Yes
//	- Passed to a person: No
//
// Engineering signals (turn counts, tool names, latencies, speech-engine call
// counts) stay out of the CRM: they live in the call-quality metrics.
func CallSummaryNote(q events.CallQuality, summary string) string {
	var b strings.Builder
	if s := strings.TrimSpace(summary); s != "" {
		b.WriteString(s)
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "- Call length: %s\n", callLength(q.DurationS))
	if p := strings.TrimSpace(q.ConfirmedPhone); p != "" {
		fmt.Fprintf(&b, "- Phone confirmed: %s\n", spacedPhone(p))
	}
	booked := "No"
	switch {
	case q.ToolCalls["book_appointment"] > 0:
		booked = "Yes"
	case q.ToolCalls["reschedule_appointment"] > 0:
		booked = "Rescheduled"
	case q.ToolCalls["cancel_appointment"] > 0:
		booked = "Cancelled"
	}
	fmt.Fprintf(&b, "- Appointment booked: %s\n", booked)
	fmt.Fprintf(&b, "- Passed to a person: %s", strings.Title(yesNo(q.Escalated))) //nolint:staticcheck // ASCII yes/no
	if q.AvgSentiment != nil {
		mood := "Neutral"
		if *q.AvgSentiment >= 0.25 {
			mood = "Positive"
		} else if *q.AvgSentiment <= -0.25 {
			mood = "Negative"
		}
		fmt.Fprintf(&b, "\n- Caller's mood: %s", mood)
	}
	return b.String()
}

func callLength(sec float64) string {
	n := int(math.Round(sec))
	if n < 60 {
		return fmt.Sprintf("%d s", n)
	}
	return fmt.Sprintf("%d min %d s", n/60, n%60)
}

// spacedPhone writes an 11-digit Nigerian mobile as 0806 555 3108; other
// formats are returned as given.
func spacedPhone(p string) string {
	if len(p) == 11 && strings.HasPrefix(p, "0") && strings.Trim(p, "0123456789") == "" {
		return p[:4] + " " + p[4:7] + " " + p[7:]
	}
	return p
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
