package snipe

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sirupsen/logrus"
)

func TestDryRunBlocksCreate(t *testing.T) {
	c, err := New("https://snipe.invalid", "key", true /*dryRun*/, "dedicated", logrus.New())
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.CreateAsset(context.Background(), Asset{Serial: "X1", ModelID: 1, StatusID: 1})
	if !errors.Is(err, ErrDryRun) {
		t.Fatalf("CreateAsset in dry-run = %v, want ErrDryRun", err)
	}
}

func TestCreateAssetRetriesOn429(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if atomic.AddInt32(&n, 1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"status":"error","messages":"rate limited"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","payload":{"id":7,"asset_tag":"A","serial":"S"}}`))
	}))
	defer srv.Close()
	c, err := New(srv.URL, "k", false, "dedicated", logrus.New())
	if err != nil {
		t.Fatal(err)
	}
	a, err := c.CreateAsset(context.Background(), Asset{Serial: "S", ModelID: 1, StatusID: 1})
	if err != nil {
		t.Fatalf("expected success after retry, got %v", err)
	}
	if a.ID != 7 {
		t.Fatalf("asset id = %d, want 7", a.ID)
	}
	if atomic.LoadInt32(&n) < 2 {
		t.Fatalf("expected a retry (>=2 requests), got %d", n)
	}
}

// A create that fails with a 5xx may already have landed server-side, so it
// must NOT be replayed — a retry would risk a duplicate asset. Reads and
// absolute updates are still retried.
func TestCreateAssetNotRetriedOn5xx(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"status":"error","messages":"unavailable"}`))
	}))
	defer srv.Close()
	c, err := New(srv.URL, "k", false, "dedicated", logrus.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateAsset(context.Background(), Asset{Serial: "S", ModelID: 1, StatusID: 1}); err == nil {
		t.Fatal("expected the 503 to surface instead of being retried")
	}
	if got := atomic.LoadInt32(&n); got != 1 {
		t.Fatalf("POST sent %d times, want 1 (a create must not be replayed after a 5xx)", got)
	}
}

// A PATCH carries an absolute update here, so replaying it is safe and a
// transient 5xx should not fail the sync.
func TestPatchAssetRetriesOn5xx(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if atomic.AddInt32(&n, 1) == 1 {
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"status":"error","messages":"unavailable"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","payload":{"id":8,"asset_tag":"A","serial":"S"}}`))
	}))
	defer srv.Close()
	c, err := New(srv.URL, "k", false, "dedicated", logrus.New())
	if err != nil {
		t.Fatal(err)
	}
	a, err := c.PatchAsset(context.Background(), 8, Asset{StatusID: 2})
	if err != nil {
		t.Fatalf("expected success after 503 retry, got %v", err)
	}
	if a.ID != 8 {
		t.Fatalf("asset id = %d, want 8", a.ID)
	}
	if atomic.LoadInt32(&n) < 2 {
		t.Fatalf("expected a retry on 503 (>=2 requests), got %d", n)
	}
}

// The plan name selects the pace; an unknown one is a config error, not a
// silently unlimited client.
func TestNewRejectsUnknownRatePlan(t *testing.T) {
	if _, err := New("https://snipe.invalid", "k", true, "enterprise", logrus.New()); err == nil {
		t.Fatal("expected an unknown rate limit plan to be rejected")
	}
	for _, plan := range []string{"", "basic", "small_business", "dedicated"} {
		if _, err := New("https://snipe.invalid", "k", true, plan, logrus.New()); err != nil {
			t.Errorf("plan %q: %v", plan, err)
		}
	}
}

func TestListAllAssetsPaginates(t *testing.T) {
	page1 := `{"total":2,"rows":[{"id":1,"asset_tag":"A1","serial":"S1"}]}`
	page2 := `{"total":2,"rows":[{"id":2,"asset_tag":"A2","serial":"S2"}]}`
	// Archived assets are hidden from the default listing and must be fetched
	// with ?status=Archived — otherwise the sync re-creates them.
	archived1 := `{"total":2,"rows":[{"id":3,"asset_tag":"A3","serial":"S3"}]}`
	archived2 := `{"total":2,"rows":[{"id":4,"asset_tag":"A4","serial":"S4"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Query().Get("status") == "Archived" && strings.Contains(r.URL.RawQuery, "offset=500"):
			_, _ = w.Write([]byte(archived2))
		case r.URL.Query().Get("status") == "Archived":
			_, _ = w.Write([]byte(archived1))
		case strings.Contains(r.URL.RawQuery, "offset=500"):
			_, _ = w.Write([]byte(page2))
		default:
			_, _ = w.Write([]byte(page1))
		}
	}))
	defer srv.Close()
	c, err := New(srv.URL, "k", false, "dedicated", logrus.New())
	if err != nil {
		t.Fatal(err)
	}
	assets, err := c.ListAllAssets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 4 || assets[0].Serial != "S1" || assets[1].Serial != "S2" ||
		assets[2].Serial != "S3" || assets[3].Serial != "S4" {
		t.Fatalf("paging failed: %+v", assets)
	}
}
