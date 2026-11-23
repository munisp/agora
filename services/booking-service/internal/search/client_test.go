package search

// SPEC-W119 §5: client tests against an httptest fake OpenSearch —
// request shape, response parsing, error/status mapping, delete 404
// tolerance. No live cluster.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fakeOS(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL + "/"), srv
}

func TestSearchParsesHitsAndTotal(t *testing.T) {
	c, _ := fakeOS(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/bookings/_search" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"hits":{"total":{"value":2,"relation":"eq"},"hits":[
			{"_source":{"booking_id":"b1"}},
			{"_source":{"booking_id":"b2"}}]}}`))
	})
	hits, total, err := c.Search(context.Background(), "bookings", BuildBookingQuery("t", Params{Limit: 10}))
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if total != 2 || len(hits) != 2 {
		t.Fatalf("total=%d hits=%d", total, len(hits))
	}
	var doc struct {
		BookingID string `json:"booking_id"`
	}
	if err := json.Unmarshal(hits[0], &doc); err != nil || doc.BookingID != "b1" {
		t.Fatalf("hit0 = %v %v", doc, err)
	}
}

func TestSearchErrorStatusMapping(t *testing.T) {
	c, _ := fakeOS(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"cluster down"}`))
	})
	_, _, err := c.Search(context.Background(), "bookings", map[string]any{})
	if err == nil {
		t.Fatal("want error")
	}
	osErr, ok := err.(*Error)
	if !ok || osErr.Status != http.StatusBadGateway {
		t.Fatalf("err = %#v", err)
	}
	if !strings.Contains(osErr.Body, "cluster down") {
		t.Fatalf("body = %q", osErr.Body)
	}
}

func TestIndexDocPutsDocument(t *testing.T) {
	var gotPath, gotBody string
	c, _ := fakeOS(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.Method + " " + r.URL.Path
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = string(buf)
		_, _ = w.Write([]byte(`{"result":"updated"}`))
	})
	if err := c.IndexDoc(context.Background(), "contacts", "c1", map[string]any{"name": "Ada"}); err != nil {
		t.Fatalf("IndexDoc: %v", err)
	}
	if gotPath != "PUT /contacts/_doc/c1" {
		t.Fatalf("path = %s", gotPath)
	}
	if !strings.Contains(gotBody, `"name":"Ada"`) {
		t.Fatalf("body = %s", gotBody)
	}
}

func TestDeleteDocTolerates404(t *testing.T) {
	c, _ := fakeOS(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"result":"not_found"}`))
	})
	if err := c.DeleteDoc(context.Background(), "contacts", "gone"); err != nil {
		t.Fatalf("DeleteDoc(404) must be tolerated: %v", err)
	}
}

func TestPingHitsClusterHealth(t *testing.T) {
	var got string
	c, _ := fakeOS(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path
		_, _ = w.Write([]byte(`{"status":"green"}`))
	})
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got != "/_cluster/health" {
		t.Fatalf("path = %s", got)
	}
}
