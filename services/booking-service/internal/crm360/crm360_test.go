package crm360

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Tag grammar: lowercase, 1..40 chars, [a-z0-9-_] (SPEC-W20 Agent A).
func TestValidateTag(t *testing.T) {
	valid := []string{"vip", "a", "x9", "gold-tier", "under_score", strings.Repeat("a", 40)}
	for _, tag := range valid {
		if err := ValidateTag(tag); err != nil {
			t.Errorf("ValidateTag(%q) = %v, want nil", tag, err)
		}
	}
	invalid := []string{
		"", "VIP", "has space", "dot.tag", "emoji🚀", "slash/tag",
		strings.Repeat("a", 41), "+234", "tag!",
	}
	for _, tag := range invalid {
		if err := ValidateTag(tag); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("ValidateTag(%q) = %v, want ErrInvalidInput", tag, err)
		}
	}
}

// NormalizeTag trims and lowercases; validation then decides.
func TestNormalizeTag(t *testing.T) {
	if got := NormalizeTag("  VIP "); got != "vip" {
		t.Fatalf("NormalizeTag = %q, want vip", got)
	}
	if got := NormalizeTag("\tGold-Tier\n"); got != "gold-tier" {
		t.Fatalf("NormalizeTag = %q, want gold-tier", got)
	}
	if err := ValidateTag(NormalizeTag(" VIP ")); err != nil {
		t.Fatalf("normalized VIP should validate: %v", err)
	}
}

// Note.Validate: body required + bounded, author bounded, ids required.
func TestNoteValidate(t *testing.T) {
	base := Note{TenantID: uuid.New(), ContactID: uuid.New(), Body: "hello", Author: "agent-1"}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid note: %v", err)
	}

	noBody := base
	noBody.Body = "   "
	if err := noBody.Validate(); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty body = %v, want ErrInvalidInput", err)
	}

	longBody := base
	longBody.Body = strings.Repeat("x", maxNoteBodyBytes+1)
	if err := longBody.Validate(); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("oversize body = %v, want ErrInvalidInput", err)
	}

	longAuthor := base
	longAuthor.Author = strings.Repeat("a", maxAuthorBytes+1)
	if err := longAuthor.Validate(); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("oversize author = %v, want ErrInvalidInput", err)
	}

	noTenant := base
	noTenant.TenantID = uuid.Nil
	if err := noTenant.Validate(); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("nil tenant = %v, want ErrInvalidInput", err)
	}

	noContact := base
	noContact.ContactID = uuid.Nil
	if err := noContact.Validate(); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("nil contact = %v, want ErrInvalidInput", err)
	}

	// Validate trims the body in place.
	trim := Note{TenantID: uuid.New(), ContactID: uuid.New(), Body: "  hi  "}
	if err := trim.Validate(); err != nil || trim.Body != "hi" {
		t.Fatalf("trim: %q, %v", trim.Body, err)
	}
}

// truncate cuts on a rune boundary and ellipsizes.
func TestTruncate(t *testing.T) {
	short := "short"
	if got := truncate(short, 120); got != short {
		t.Fatalf("short passthrough = %q", got)
	}
	long := strings.Repeat("ab", 200)
	got := truncate(long, 120)
	// Content is capped at max-1 bytes, then a 3-byte ellipsis is appended.
	if len(got) > 120-1+len("…") || !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated = %q (len %d)", got, len(got))
	}
	// Multibyte content must not split a rune.
	mb := strings.Repeat("é", 200) // 2 bytes per rune
	got = truncate(mb, 121)
	if !strings.HasSuffix(got, "…") || strings.ContainsRune(got, '�') {
		t.Fatalf("multibyte truncate broken: %q", got)
	}
}
