package channel

// SPEC-W46 PERF-10 tests: USSD menu cache — TTL hit, expiry refetch,
// stale-on-error fallback, error propagation with no entry.

import (
	"context"
	"errors"
	"testing"
	"time"
)

type countingMenus struct {
	calls int
	menu  []USSDMenuItem
	err   error
}

func (f *countingMenus) USSDMenu(_ context.Context, _ string) ([]USSDMenuItem, error) {
	f.calls++
	return f.menu, f.err
}

func TestUSSDMenuCacheFreshHit(t *testing.T) {
	inner := &countingMenus{menu: []USSDMenuItem{{Key: "1", Label: "Balance", Action: "balance"}}}
	c := NewCachedUSSDMenuFetcher(inner, 0)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		menu, err := c.USSDMenu(ctx, "tenant-a")
		if err != nil {
			t.Fatalf("USSDMenu: %v", err)
		}
		if len(menu) != 1 || menu[0].Key != "1" {
			t.Fatalf("unexpected menu: %+v", menu)
		}
	}
	if inner.calls != 1 {
		t.Fatalf("fresh entries must not refetch: calls=%d", inner.calls)
	}
	// A nil menu (pass-through tenant) is a valid cached resolution too.
	innerNil := &countingMenus{}
	cNil := NewCachedUSSDMenuFetcher(innerNil, 0)
	for i := 0; i < 2; i++ {
		menu, err := cNil.USSDMenu(ctx, "tenant-b")
		if err != nil || menu != nil {
			t.Fatalf("nil-menu tenant: menu=%v err=%v", menu, err)
		}
	}
	if innerNil.calls != 1 {
		t.Fatalf("nil menu must be cached: calls=%d", innerNil.calls)
	}
}

func TestUSSDMenuCacheExpiryRefetch(t *testing.T) {
	inner := &countingMenus{menu: []USSDMenuItem{{Key: "1", Label: "L", Action: "a"}}}
	c := NewCachedUSSDMenuFetcher(inner, time.Minute)
	now := time.Now()
	c.SetClock(func() time.Time { return now })
	ctx := context.Background()
	if _, err := c.USSDMenu(ctx, "t"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute) // past TTL
	if _, err := c.USSDMenu(ctx, "t"); err != nil {
		t.Fatal(err)
	}
	if inner.calls != 2 {
		t.Fatalf("expired entry must refetch: calls=%d", inner.calls)
	}
}

func TestUSSDMenuCacheStaleOnError(t *testing.T) {
	good := []USSDMenuItem{{Key: "1", Label: "L", Action: "a"}}
	inner := &countingMenus{menu: good}
	c := NewCachedUSSDMenuFetcher(inner, time.Minute)
	now := time.Now()
	c.SetClock(func() time.Time { return now })
	ctx := context.Background()
	if _, err := c.USSDMenu(ctx, "t"); err != nil {
		t.Fatal(err)
	}
	// TTL expired + upstream down → stale entry served, no error.
	now = now.Add(time.Hour)
	inner.err = errors.New("identity down")
	menu, err := c.USSDMenu(ctx, "t")
	if err != nil {
		t.Fatalf("stale-on-error must not surface the error: %v", err)
	}
	if len(menu) != 1 {
		t.Fatalf("stale menu expected, got %+v", menu)
	}
}

func TestUSSDMenuCacheErrorWithoutEntry(t *testing.T) {
	inner := &countingMenus{err: errors.New("identity down")}
	c := NewCachedUSSDMenuFetcher(inner, 0)
	if _, err := c.USSDMenu(context.Background(), "never-seen"); err == nil {
		t.Fatal("with no cached entry the error must propagate (pass-through fallback upstream)")
	}
}
