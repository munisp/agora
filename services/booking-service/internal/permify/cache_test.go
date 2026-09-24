package permify

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// fakeAuthorizer records calls and answers from a scripted result.
type fakeAuthorizer struct {
	calls atomic.Int32
	allow bool
	err   error
}

func (f *fakeAuthorizer) Check(ctx context.Context, tenantID, subject, permission, resource string) (bool, error) {
	f.calls.Add(1)
	return f.allow, f.err
}

// Positive decisions are cached for the TTL (one backend call for repeats).
func TestCachedAuthorizerCachesPositives(t *testing.T) {
	inner := &fakeAuthorizer{allow: true}
	c := NewCachedAuthorizer(inner, time.Minute)
	for i := 0; i < 5; i++ {
		ok, err := c.Check(context.Background(), "t1", "user:u1", "manage_bookings", "organization:t1")
		if err != nil || !ok {
			t.Fatalf("check %d = %v, %v", i, ok, err)
		}
	}
	if n := inner.calls.Load(); n != 1 {
		t.Fatalf("backend calls = %d, want 1 (positive cached)", n)
	}
	// A different key input (permission) must NOT hit the cached entry.
	if _, err := c.Check(context.Background(), "t1", "user:u1", "view_analytics", "organization:t1"); err != nil {
		t.Fatal(err)
	}
	if n := inner.calls.Load(); n != 2 {
		t.Fatalf("backend calls = %d, want 2 (key includes permission)", n)
	}
}

// Negative decisions are NEVER cached (fail-closed: revocation re-checks).
func TestCachedAuthorizerNeverCachesNegatives(t *testing.T) {
	inner := &fakeAuthorizer{allow: false}
	c := NewCachedAuthorizer(inner, time.Minute)
	for i := 0; i < 3; i++ {
		ok, err := c.Check(context.Background(), "t1", "user:u1", "manage_bookings", "organization:t1")
		if err != nil || ok {
			t.Fatalf("check %d = %v, %v", i, ok, err)
		}
	}
	if n := inner.calls.Load(); n != 3 {
		t.Fatalf("backend calls = %d, want 3 (negatives uncached)", n)
	}
}

// Backend errors are NEVER cached and propagate (outage policy decides).
func TestCachedAuthorizerNeverCachesErrors(t *testing.T) {
	inner := &fakeAuthorizer{err: errors.New("permify down")}
	c := NewCachedAuthorizer(inner, time.Minute)
	for i := 0; i < 2; i++ {
		if _, err := c.Check(context.Background(), "t1", "user:u1", "p", "r"); err == nil {
			t.Fatalf("check %d: want error", i)
		}
	}
	if n := inner.calls.Load(); n != 2 {
		t.Fatalf("backend calls = %d, want 2 (errors uncached)", n)
	}
	// Recovery: a subsequent allow is served and cached.
	inner.err = nil
	inner.allow = true
	ok, err := c.Check(context.Background(), "t1", "user:u1", "p", "r")
	if err != nil || !ok {
		t.Fatalf("recovery check = %v, %v", ok, err)
	}
}

// An expired positive re-hits the backend.
func TestCachedAuthorizerExpiry(t *testing.T) {
	inner := &fakeAuthorizer{allow: true}
	c := NewCachedAuthorizer(inner, 20*time.Millisecond)
	if _, err := c.Check(context.Background(), "t1", "user:u1", "p", "r"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if _, err := c.Check(context.Background(), "t1", "user:u1", "p", "r"); err != nil {
		t.Fatal(err)
	}
	if n := inner.calls.Load(); n != 2 {
		t.Fatalf("backend calls = %d, want 2 (expired entry re-fetched)", n)
	}
}
