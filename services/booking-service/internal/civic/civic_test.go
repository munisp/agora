package civic

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opendesk/booking-service/internal/store"
)

// SPEC-W32 WS-A validation matrix (public report body).
func TestReportInputValidateMatrix(t *testing.T) {
	f := 6.5244
	g := 3.3792
	ok := ReportInput{CategorySlug: "roads", Description: "Deep pothole at the junction blocking one lane"}
	cases := []struct {
		name    string
		mutate  func(*ReportInput)
		wantErr bool
	}{
		{"valid minimal", nil, false},
		{"valid full", func(in *ReportInput) {
			in.Lat, in.Lon = &f, &g
			in.ReporterPhoneE164 = "+2348012345678"
			in.PhotoURL = "https://example.org/p.jpg"
			in.LocationText = "Ikeja"
		}, false},
		{"missing category", func(in *ReportInput) { in.CategorySlug = "" }, true},
		{"description too short", func(in *ReportInput) { in.Description = "too short" }, true},
		{"description too long", func(in *ReportInput) { in.Description = strings.Repeat("x", DescriptionMaxLen+1) }, true},
		{"description boundary min", func(in *ReportInput) { in.Description = strings.Repeat("x", DescriptionMinLen) }, false},
		{"description boundary max", func(in *ReportInput) { in.Description = strings.Repeat("x", DescriptionMaxLen) }, false},
		{"lat without lon", func(in *ReportInput) { in.Lat = &f }, true},
		{"lon without lat", func(in *ReportInput) { in.Lon = &g }, true},
		{"lat out of range", func(in *ReportInput) { bad := 91.0; in.Lat, in.Lon = &bad, &g }, true},
		{"lon out of range", func(in *ReportInput) { bad := -181.0; in.Lat, in.Lon = &f, &bad }, true},
		{"phone not e164", func(in *ReportInput) { in.ReporterPhoneE164 = "08012345678" }, true},
		{"phone too short", func(in *ReportInput) { in.ReporterPhoneE164 = "+23480" }, true},
		{"wants_updates without phone", func(in *ReportInput) {
			tr := true
			in.WantsUpdates = &tr
		}, true},
		{"photo_url not absolute", func(in *ReportInput) { in.PhotoURL = "ftp://x/y.jpg" }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := ok
			if tc.mutate != nil {
				tc.mutate(&in)
			}
			err := in.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("want validation error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr && err != nil && !strings.Contains(err.Error(), ErrInvalidInput.Error()) {
				t.Fatalf("error must wrap ErrInvalidInput: %v", err)
			}
		})
	}
}

func TestWantsStatusUpdates(t *testing.T) {
	tr, fl := true, false
	if !(ReportInput{WantsUpdates: &tr}).WantsStatusUpdates() {
		t.Fatal("explicit true must win")
	}
	if (ReportInput{WantsUpdates: &fl, ReporterPhoneE164: "+2348012345678"}).WantsStatusUpdates() {
		t.Fatal("explicit false must win even with phone")
	}
	if !(ReportInput{ReporterPhoneE164: "+2348012345678"}).WantsStatusUpdates() {
		t.Fatal("default with phone must be true")
	}
	if (ReportInput{}).WantsStatusUpdates() {
		t.Fatal("default without phone must be false")
	}
}

// Ref format GOV-{LGA}-{WARD}-YYYY-{seq6} (SPEC-W32 §2).
func TestFormatRef(t *testing.T) {
	got := FormatRef("ikeja", "ward-03", 2026, 42)
	if got != "GOV-IKEJA-WARD03-2026-000042" {
		t.Fatalf("ref = %q", got)
	}
	// Fallbacks for absent lga/ward.
	if got := FormatRef("", "", 2026, 7); got != "GOV-GEN-00-2026-000007" {
		t.Fatalf("fallback ref = %q", got)
	}
	// Long components are truncated to 8 chars; seq wraps at 6 digits.
	if got := FormatRef("VeryLongLGACode", "w", 2026, 1234567); got != "GOV-VERYLONG-W-2026-234567" {
		t.Fatalf("truncated ref = %q", got)
	}
}

// SLA math is plain wall-clock hours, correct across midnight/weekend
// (SPEC-W32 §4.2: no business calendar in v1).
func TestComputeSLA(t *testing.T) {
	// Friday 23:30 + 2h ack → Saturday 01:30 (midnight + week boundary).
	start := time.Date(2026, 8, 7, 23, 30, 0, 0, time.UTC) // Friday
	ack, resolve := ComputeSLA(start, store.CivicCategory{AckSLAHours: 2, ResolveSLAHours: 49})
	if ack == nil || ack.Weekday() != time.Saturday || ack.Hour() != 1 || ack.Minute() != 30 {
		t.Fatalf("ack due = %v, want Sat 01:30", ack)
	}
	if resolve == nil || !resolve.Equal(start.Add(49*time.Hour)) {
		t.Fatalf("resolve due = %v", resolve)
	}
	// Zero/negative SLA hours leave the clock unset.
	ack, resolve = ComputeSLA(start, store.CivicCategory{AckSLAHours: 0, ResolveSLAHours: -1})
	if ack != nil || resolve != nil {
		t.Fatalf("zero SLA must unset dues: %v %v", ack, resolve)
	}
}

// Routing precedence: ward-specific override > ward-less category rule >
// category default (SPEC-W32 §2).
func TestResolveMDAQueue(t *testing.T) {
	catID := uuid.New()
	cat := store.CivicCategory{ID: catID, MDAQueue: "mda-default"}
	rules := []store.CivicRoutingRule{
		{ID: uuid.New(), CategoryID: catID, Ward: "", MDAQueue: "mda-category-wide"},
		{ID: uuid.New(), CategoryID: catID, Ward: "Ward 3", MDAQueue: "mda-ward3"},
		{ID: uuid.New(), CategoryID: uuid.New(), Ward: "Ward 3", MDAQueue: "mda-other-cat"},
	}
	if got := ResolveMDAQueue(rules, cat, "Ward 3"); got != "mda-ward3" {
		t.Fatalf("ward override = %q", got)
	}
	// Ward match is case-insensitive.
	if got := ResolveMDAQueue(rules, cat, "ward 3"); got != "mda-ward3" {
		t.Fatalf("case-insensitive ward override = %q", got)
	}
	if got := ResolveMDAQueue(rules, cat, "Ward 9"); got != "mda-category-wide" {
		t.Fatalf("category-wide rule = %q", got)
	}
	if got := ResolveMDAQueue(nil, cat, "Ward 3"); got != "mda-default" {
		t.Fatalf("category default = %q", got)
	}
}

func TestHaversineM(t *testing.T) {
	// 1 degree of latitude ≈ 111.2 km.
	d := HaversineM(6.0, 3.0, 7.0, 3.0)
	if d < 110000 || d > 113000 {
		t.Fatalf("1° lat distance = %v m", d)
	}
	if d := HaversineM(6.5, 3.3, 6.5, 3.3); d != 0 {
		t.Fatalf("same point distance = %v", d)
	}
	// ~300m apart must fall inside the 500m duplicate radius.
	if d := HaversineM(6.5244, 3.3792, 6.5271, 3.3792); d >= DuplicateCandidateMaxM {
		t.Fatalf("nearby distance = %v, want < %v", d, DuplicateCandidateMaxM)
	}
}

// Duplicate candidates: geo ≤500m + same category + ±72h (SPEC-W32 WS-A).
func TestIsDuplicateCandidate(t *testing.T) {
	lat1, lon1 := 6.5244, 3.3792
	lat2 := 6.5271 // ~300m north
	far := 6.6000  // ~8.4km north
	catID := uuid.New()
	now := time.Now().UTC()
	base := store.CivicCase{ID: uuid.New(), CategoryID: catID, Lat: &lat1, Lon: &lon1, CreatedAt: now}
	mk := func(mut func(*store.CivicCase)) store.CivicCase {
		o := store.CivicCase{ID: uuid.New(), CategoryID: catID, Lat: &lat2, Lon: &lon1, CreatedAt: now.Add(-time.Hour)}
		if mut != nil {
			mut(&o)
		}
		return o
	}
	if !IsDuplicateCandidate(base, mk(nil), DuplicateCandidateMaxM) {
		t.Fatal("nearby same-category ±72h case must be a candidate")
	}
	if IsDuplicateCandidate(base, mk(func(o *store.CivicCase) { o.CategoryID = uuid.New() }), DuplicateCandidateMaxM) {
		t.Fatal("different category must not be a candidate")
	}
	if IsDuplicateCandidate(base, mk(func(o *store.CivicCase) { o.Lat = &far }), DuplicateCandidateMaxM) {
		t.Fatal(">500m away must not be a candidate")
	}
	if IsDuplicateCandidate(base, mk(func(o *store.CivicCase) { o.Lat = nil }), DuplicateCandidateMaxM) {
		t.Fatal("missing geo must not be a candidate")
	}
	if IsDuplicateCandidate(base, mk(func(o *store.CivicCase) { id := uuid.New(); o.MergedInto = &id }), DuplicateCandidateMaxM) {
		t.Fatal("already-merged case must not be a candidate")
	}
	if IsDuplicateCandidate(base, mk(func(o *store.CivicCase) { o.ID = base.ID }), DuplicateCandidateMaxM) {
		t.Fatal("the case itself must not be a candidate")
	}
	if IsDuplicateCandidate(base, mk(func(o *store.CivicCase) { o.CreatedAt = now.Add(-73 * time.Hour) }), DuplicateCandidateMaxM) {
		t.Fatal("outside ±72h must not be a candidate")
	}
	if !IsDuplicateCandidate(base, mk(func(o *store.CivicCase) { o.CreatedAt = now.Add(71 * time.Hour) }), DuplicateCandidateMaxM) {
		t.Fatal("inside +72h must be a candidate")
	}
}

func TestMaskPhone(t *testing.T) {
	got := MaskPhone("+2348012345678")
	if got == "+2348012345678" || !strings.HasPrefix(got, "+234") || !strings.HasSuffix(got, "78") {
		t.Fatalf("masked phone = %q", got)
	}
	if strings.Contains(got, "8012") {
		t.Fatalf("masked phone leaks middle digits: %q", got)
	}
	if MaskPhone("") != "" {
		t.Fatal("empty phone stays empty")
	}
	// Very short numbers still mask (never echo the raw value).
	if got := MaskPhone("+12"); strings.Contains(got, "12") && !strings.Contains(got, "*") {
		t.Fatalf("short phone not masked: %q", got)
	}
}

// Reporter masking by role (SPEC-W32 §4.4): anonymous reporters are masked
// for non-owner/admin operators on every operator view.
func TestMaskReporter(t *testing.T) {
	phone := "+2348012345678"
	name := "Adaeze Obi"
	mk := func() *store.CivicCase {
		p, n := phone, name
		return &store.CivicCase{Anonymous: true, ReporterPhoneE164: &p, ReporterName: &n}
	}
	c := mk()
	MaskReporter(c, false)
	if c.ReporterPhoneE164 == nil || *c.ReporterPhoneE164 == phone {
		t.Fatalf("phone not masked: %v", c.ReporterPhoneE164)
	}
	if c.ReporterName == nil || *c.ReporterName != "Anonymous" {
		t.Fatalf("name not masked: %v", c.ReporterName)
	}
	c = mk()
	MaskReporter(c, true)
	if *c.ReporterPhoneE164 != phone || *c.ReporterName != name {
		t.Fatal("owner/admin view must stay unmasked")
	}
	// Non-anonymous cases are never masked.
	c = mk()
	c.Anonymous = false
	MaskReporter(c, false)
	if *c.ReporterPhoneE164 != phone {
		t.Fatal("non-anonymous reporter must not be masked")
	}
}

func TestCanViewReporterRole(t *testing.T) {
	if !CanViewReporterRole([]string{"staff", "owner"}) {
		t.Fatal("owner must see reporter")
	}
	if !CanViewReporterRole([]string{"admin"}) {
		t.Fatal("admin must see reporter")
	}
	if CanViewReporterRole([]string{"staff", "viewer", "analyst"}) {
		t.Fatal("staff/viewer/analyst must NOT see reporter")
	}
	if CanViewReporterRole(nil) {
		t.Fatal("no roles must NOT see reporter")
	}
}

// In-memory throttler: sliding windows, independent keys, expiry.
func TestThrottler(t *testing.T) {
	th := NewThrottler()
	now := time.Now()
	th.SetClock(func() time.Time { return now })
	for i := 0; i < 10; i++ {
		if !th.Allow("ip:1.2.3.4", 10, time.Hour) {
			t.Fatalf("hit %d must be allowed", i+1)
		}
	}
	if th.Allow("ip:1.2.3.4", 10, time.Hour) {
		t.Fatal("11th hit within the hour must be rejected")
	}
	// A different key is independent.
	if !th.Allow("ip:5.6.7.8", 10, time.Hour) {
		t.Fatal("distinct key must be allowed")
	}
	// Window expiry frees capacity.
	now = now.Add(61 * time.Minute)
	if !th.Allow("ip:1.2.3.4", 10, time.Hour) {
		t.Fatal("after the window lapses hits must be allowed again")
	}
	// limit<=0 disables the check.
	if !th.Allow("ip:1.2.3.4", 0, time.Hour) {
		t.Fatal("disabled limit must allow")
	}
}
