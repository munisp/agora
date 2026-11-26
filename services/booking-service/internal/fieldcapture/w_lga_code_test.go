package fieldcapture

import "testing"

// 2026-11-21: the field app's gazetteer LGA code rides the lead (CAC LGA view).
func TestLgaCodeOf(t *testing.T) {
	ok := lgaCodeOf(LeadCapturePayload{LgaCode: " ng-lagos-ikeja ", LGA: "Ikeja"})
	if ok == nil || *ok != "NG-LAGOS-IKEJA" || *lgaNameOf(LeadCapturePayload{LgaCode: "NG-LAGOS-IKEJA", LGA: "Ikeja"}) != "Ikeja" {
		t.Fatalf("valid code not kept: %v", ok)
	}
	for _, bad := range []string{"", "Ikeja", "NG LAGOS", "ng-lagos-ikeja'; drop"} {
		if lgaCodeOf(LeadCapturePayload{LgaCode: bad}) != nil {
			t.Fatalf("bad code %q accepted", bad)
		}
	}
	if lgaNameOf(LeadCapturePayload{LGA: "Ikeja"}) != nil {
		t.Fatal("a name without a code must not be stored")
	}
}
