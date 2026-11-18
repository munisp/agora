package twentyc

import "testing"

func TestNationalPhoneNigerianFormats(t *testing.T) {
	for _, in := range []string{"08065553108", "0806 555 3108", "+2348065553108", "2348065553108", "+234 806-555-3108"} {
		n, c, cc := NationalPhone(in)
		if n != "8065553108" || c != "NG" || cc != "+234" {
			t.Fatalf("%q -> %q %q %q", in, n, c, cc)
		}
	}
	if n, c, _ := NationalPhone("+14155550100"); n != "+14155550100" || c != "" {
		t.Fatalf("non-Nigerian number changed: %q %q", n, c)
	}
}

func TestPersonFromContactSendsTwentyPhoneParts(t *testing.T) {
	p := PersonFromContact("Emeka Okafor", "", "08065553108")
	if p.Phones == nil || p.Phones.PrimaryPhoneNumber != "8065553108" || p.Phones.PrimaryPhoneCallingCode != "+234" {
		t.Fatalf("phones = %+v", p.Phones)
	}
	if got := PersonPhoneFilter("0806 555 3108"); got != PersonPhoneFilter("+2348065553108") {
		t.Fatalf("lookup differs by input format: %q", got)
	}
}
