package twentyc

import (
	"testing"

	"github.com/google/uuid"
)

func TestRouterSendsRoutedTenantToItsOwnInstance(t *testing.T) {
	def := New("http://shared:3000", "k1", 60)
	own := New("http://leadway:3000", "k2", 60)
	leadway := uuid.MustParse("bb69e5fd-aa86-4d4b-9d86-4cfe5c33ae52")
	other := uuid.MustParse("11111111-2222-3333-4444-555555555555")

	r := NewRouter(def)
	r.Add([]string{leadway.String(), "Leadway-Pensure"}, "ws-leadway", own)

	if got := r.ForTenant(&leadway); got != own {
		t.Fatalf("leadway by id: got default, want own instance")
	}
	if got := r.ForTenant(&other); got != def {
		t.Fatalf("other tenant: want default")
	}
	if got := r.ForTenant(nil); got != def {
		t.Fatalf("no tenant: want default")
	}
	if got := r.ForTenantKey("leadway-pensure"); got != own {
		t.Fatalf("leadway by slug (case-insensitive): want own instance")
	}
	if got := r.ForTenantKey(""); got != def {
		t.Fatalf("empty key: want default")
	}
	if got := r.ForWorkspace("ws-leadway"); got != own {
		t.Fatalf("webhook from leadway workspace: want own instance")
	}
	if got := r.ForWorkspace("ws-unknown"); got != def {
		t.Fatalf("unknown workspace: want default")
	}
	if all := r.All(); len(all) != 2 || all[0] != def || all[1] != own {
		t.Fatalf("All: want [default, own], got %d clients", len(all))
	}
	var nilRouter *Router
	if nilRouter.ForTenant(&leadway) != nil || nilRouter.All() != nil {
		t.Fatalf("nil router must be safe")
	}
}
