package twentyc

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/opendesk/crm-sync-service/internal/events"
)

func TestTenantDomain(t *testing.T) {
	if got := TenantDomain("acme-salon"); got != "acme-salon.opendesk.local" {
		t.Fatalf("TenantDomain = %q", got)
	}
}

func TestCompanyFromTenant(t *testing.T) {
	c := CompanyFromTenant(events.TenantProvisionedData{
		TenantID: "t-1", Slug: "acme-salon", Name: "Acme Salon", Plan: "free",
	})
	if c.Name != "Acme Salon" {
		t.Fatalf("Name = %q", c.Name)
	}
	if c.DomainName.PrimaryLinkURL != "https://acme-salon.opendesk.local" {
		t.Fatalf("DomainName = %+v", c.DomainName)
	}
}

func TestSplitName(t *testing.T) {
	cases := []struct{ in, first, last string }{
		{"Jane Doe", "Jane", "Doe"},
		{"Jane", "Jane", ""},
		{"Jane Mary Doe", "Jane", "Mary Doe"},
		{"  ", "Unknown", ""},
		{"  Bob  Smith ", "Bob", "Smith"},
	}
	for _, c := range cases {
		got := SplitName(c.in)
		if got.FirstName != c.first || got.LastName != c.last {
			t.Errorf("SplitName(%q) = %+v, want {%q %q}", c.in, got, c.first, c.last)
		}
	}
}

func TestPersonFromContact(t *testing.T) {
	p := PersonFromContact("Jane Doe", "jane@example.com", "+1555000111")
	if p.Name.FirstName != "Jane" || p.Name.LastName != "Doe" {
		t.Fatalf("Name = %+v", p.Name)
	}
	if p.Emails == nil || p.Emails.PrimaryEmail != "jane@example.com" {
		t.Fatalf("Emails = %+v", p.Emails)
	}
	if p.Phones == nil || p.Phones.PrimaryPhoneNumber != "+1555000111" {
		t.Fatalf("Phones = %+v", p.Phones)
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["emails"].(map[string]any)["primaryEmail"]; !ok {
		t.Fatalf("marshalled person missing emails.primaryEmail: %s", b)
	}
}

func TestPersonFromContactOmitsEmptyChannels(t *testing.T) {
	p := PersonFromContact("Jane", "", "")
	b, _ := json.Marshal(p)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if _, ok := m["emails"]; ok {
		t.Fatalf("emails should be omitted when empty: %s", b)
	}
	if _, ok := m["phones"]; ok {
		t.Fatalf("phones should be omitted when empty: %s", b)
	}
}

func TestTaskTitle(t *testing.T) {
	ts := time.Date(2026, 3, 1, 15, 30, 0, 0, time.FixedZone("X", 3600))
	got := TaskTitle("Haircut", ts)
	want := "Haircut — Sun 1 Mar 2026, 3:30 pm (Lagos)"
	if got != want {
		t.Fatalf("TaskTitle = %q, want %q", got, want)
	}
	if got := TaskTitle("", ts); got != "Appointment — Sun 1 Mar 2026, 3:30 pm (Lagos)" {
		t.Fatalf("TaskTitle empty offering = %q", got)
	}
}

func TestTaskFromBooking(t *testing.T) {
	d := events.BookingData{
		BookingID:    "b-1",
		OfferingName: "Consultation",
		ContactName:  "Jane Doe",
		StartsAt:     time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
		Status:       "pending",
		Source:       "voice",
		PriceCents:   5000,
		Currency:     "USD",
	}
	task := TaskFromBooking(d)
	if task.Title != "Consultation — Sun 1 Mar 2026, 11:00 am (Lagos)" {
		t.Fatalf("Title = %q", task.Title)
	}
	if task.DueAt != "2026-03-01T10:00:00Z" {
		t.Fatalf("DueAt = %q", task.DueAt)
	}
	if task.Status != "TODO" {
		t.Fatalf("Status = %q", task.Status)
	}
	if task.Body == "" {
		t.Fatal("Body should summarize the booking")
	}
}

func TestCancelNote(t *testing.T) {
	if got := CancelNote("sick"); got != "Booking cancelled: sick" {
		t.Fatalf("CancelNote = %q", got)
	}
	if got := CancelNote(""); got != "Booking cancelled: no reason given" {
		t.Fatalf("CancelNote empty = %q", got)
	}
}

func TestBookingTaskBodyIsPlainLanguage(t *testing.T) {
	got := BookingTaskBody("confirmed", "Ngozi Okafor", "voice", 0, "NGN")
	want := "Confirmed booking for Ngozi Okafor, made on a call with the receptionist. Free consultation."
	if got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if b := BookingTaskBody("no_show", "", "web", 250000, "ngn"); b != "No show booking for a customer, made on the booking page. Price: NGN 2500.00." {
		t.Fatalf("body = %q", b)
	}
}
