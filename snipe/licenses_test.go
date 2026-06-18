package snipe

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

func TestRetryAfterDurationParsesAndClamps(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"", 0, false},
		{"5", 5 * time.Second, true},
		{"0", 0, true},               // present zero => retry immediately (distinct from absent)
		{"999999", maxBackoff, true}, // must clamp to the cap, not sleep for days
		{"-3", 0, false},
		{"banana", 0, false}, // HTTP-date / garbage => treated as absent
	}
	for _, c := range cases {
		h := http.Header{}
		h.Set("Retry-After", c.in)
		got, ok := retryAfterDuration(h)
		if got != c.want || ok != c.ok {
			t.Errorf("retryAfterDuration(%q) = (%v, %v), want (%v, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestLicenseClientRetriesThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"status":"success"}`))
	}))
	defer srv.Close()
	c := NewLicenseClient(srv.URL, "k", false, logrus.New())
	// CheckoutSeatToAsset issues a single PATCH through do(); it must ride out the 429s.
	if err := c.CheckoutSeatToAsset(context.Background(), 1, 2, 3); err != nil {
		t.Fatalf("CheckoutSeatToAsset should retry past 429s and succeed, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("server saw %d calls, want 3 (2 retries then success)", got)
	}
}

func TestLicenseClientGivesUpAfterPersistent429(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := NewLicenseClient(srv.URL, "k", false, logrus.New())
	err := c.CheckoutSeatToAsset(context.Background(), 1, 2, 3)
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("want a 429 error after exhausting retries, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got < 2 {
		t.Fatalf("server saw %d calls, want multiple retry attempts", got)
	}
}

// TestLicenseClientRetriesDroppedConnection reproduces the real backfill failure: a seat
// PATCH hit "dial tcp ...: connect: connection refused" and aborted the whole run. A
// transient network error on an idempotent request must be retried, not surfaced.
func TestLicenseClientRetriesDroppedConnection(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			// Drop the connection without responding (a client-side transport error,
			// the same class as connection refused/reset).
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("test server does not support hijacking")
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Fatalf("hijack: %v", err)
			}
			_ = conn.Close()
			return
		}
		_, _ = w.Write([]byte(`{"status":"success"}`))
	}))
	defer srv.Close()
	c := NewLicenseClient(srv.URL, "k", false, logrus.New())
	// CheckoutSeatToAsset issues a PATCH (idempotent); the dropped first connection must be
	// retried and the second attempt must succeed.
	if err := c.CheckoutSeatToAsset(context.Background(), 3, 3045, 999); err != nil {
		t.Fatalf("CheckoutSeatToAsset should retry the dropped connection and succeed, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("server saw %d calls, want 2 (1 dropped, 1 success)", got)
	}
}

func TestLicenseClientRetries5xxOnGet(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 2 {
			w.WriteHeader(http.StatusBadGateway) // 502
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total":0,"rows":[]}`))
	}))
	defer srv.Close()
	c := NewLicenseClient(srv.URL, "k", false, logrus.New())
	if _, err := c.ListLicenses(context.Background()); err != nil {
		t.Fatalf("ListLicenses should retry a 5xx and succeed, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("server saw %d calls, want 2 (1 retry then success)", got)
	}
}

func TestLicenseClientDryRunSentinel(t *testing.T) {
	c := NewLicenseClient("https://snipe.invalid", "key", true /*dryRun*/, logrus.New())
	// EnsureSeats is a pure mutator: in dry-run it must return ErrDryRun before any HTTP.
	if err := c.EnsureSeats(context.Background(), 1, 5); !errors.Is(err, ErrDryRun) {
		t.Fatalf("EnsureSeats dry-run = %v, want ErrDryRun", err)
	}
}

// TestEnsureLicenseClampsCreateSeats reproduces the production failure: creating a license
// with > 10000 seats is rejected by Snipe-IT's limit_change rule (on create, current seats
// is 0, so the bound is 1..10000). The create must clamp seats to the per-change limit.
func TestEnsureLicenseClampsCreateSeats(t *testing.T) {
	createSeats := -1.0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet { // ListLicenses -> empty so a create is attempted
			if r.URL.Path != "/api/v1/licenses" {
				t.Errorf("list path = %q, want /api/v1/licenses", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"total":0,"rows":[]}`))
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/licenses" {
			t.Errorf("create request = %s %q, want POST /api/v1/licenses", r.Method, r.URL.Path)
		}
		var body map[string]any // POST /licenses
		_ = json.NewDecoder(r.Body).Decode(&body)
		if s, ok := body["seats"].(float64); ok {
			createSeats = s
		}
		_, _ = w.Write([]byte(`{"status":"success","payload":{"id":7,"name":"Big","seats":10000}}`))
	}))
	defer srv.Close()
	c := NewLicenseClient(srv.URL, "k", false, logrus.New())
	if _, err := c.EnsureLicense(context.Background(), LicenseSpec{Name: "Big", CategoryID: 1, Seats: 13000}); err != nil {
		t.Fatalf("EnsureLicense: %v", err)
	}
	if createSeats != 10000 {
		t.Fatalf("create seats = %v, want 10000 (clamped to Snipe's per-change limit)", createSeats)
	}
}

// TestEnsureSeatsStepsPastChangeLimit verifies a grow larger than the per-change limit is
// applied in <=10000 increments rather than one oversized PATCH that Snipe-IT would reject.
func TestEnsureSeatsStepsPastChangeLimit(t *testing.T) {
	var patched []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet { // GET /licenses/{id} -> current seat total
			if r.URL.Path != "/api/v1/licenses/7" {
				t.Errorf("get path = %q, want /api/v1/licenses/7", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":7,"seats":10000}`))
			return
		}
		if r.Method != http.MethodPatch || r.URL.Path != "/api/v1/licenses/7" {
			t.Errorf("grow request = %s %q, want PATCH /api/v1/licenses/7", r.Method, r.URL.Path)
		}
		var body map[string]any // PATCH /licenses/{id}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if s, ok := body["seats"].(float64); ok {
			patched = append(patched, int(s))
		}
		_, _ = w.Write([]byte(`{"status":"success"}`))
	}))
	defer srv.Close()
	c := NewLicenseClient(srv.URL, "k", false, logrus.New())
	if err := c.EnsureSeats(context.Background(), 7, 25000); err != nil {
		t.Fatalf("EnsureSeats: %v", err)
	}
	want := []int{20000, 25000} // 10000 -> 20000 (+10000) -> 25000 (+5000)
	if len(patched) != len(want) || patched[0] != want[0] || patched[1] != want[1] {
		t.Fatalf("patched seat steps = %v, want %v", patched, want)
	}
}

func TestEnsureLicenseSurfacesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(422)
			_, _ = w.Write([]byte(`{"status":"error","messages":"bad"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total":0,"rows":[]}`)) // empty list so create is attempted
	}))
	defer srv.Close()
	c := NewLicenseClient(srv.URL, "key", false /*not dry-run*/, logrus.New())
	_, err := c.EnsureLicense(context.Background(), LicenseSpec{Name: "X", CategoryID: 1, Seats: 1})
	if err == nil || !strings.Contains(err.Error(), "HTTP 422") {
		t.Fatalf("want HTTP 422 error, got %v", err)
	}
}

func TestSeatMutatorsDryRun(t *testing.T) {
	c := NewLicenseClient("https://snipe.invalid", "k", true /*dryRun*/, logrus.New())
	if err := c.CheckoutSeatToUser(context.Background(), 1, 2, 3); !errors.Is(err, ErrDryRun) {
		t.Fatalf("CheckoutSeatToUser = %v", err)
	}
	if err := c.CheckoutSeatToAsset(context.Background(), 1, 2, 3); !errors.Is(err, ErrDryRun) {
		t.Fatalf("CheckoutSeatToAsset = %v", err)
	}
	if err := c.CheckinSeat(context.Background(), 1, 2); !errors.Is(err, ErrDryRun) {
		t.Fatalf("CheckinSeat = %v", err)
	}
}

func TestListSeatsParsesAssignments(t *testing.T) {
	body := `{"total":3,"rows":[
		{"id":10,"assigned_user":{"id":555},"assigned_asset":null},
		{"id":11,"assigned_user":null,"assigned_asset":{"id":777}},
		{"id":12,"assigned_user":null,"assigned_asset":null}
	]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	c := NewLicenseClient(srv.URL, "k", false, logrus.New())
	seats, err := c.ListSeats(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if len(seats) != 3 {
		t.Fatalf("got %d seats", len(seats))
	}
	if seats[0].AssignedUserID != 555 || seats[0].AssignedAssetID != 0 {
		t.Errorf("seat0 = %+v", seats[0])
	}
	if seats[1].AssignedAssetID != 777 || seats[1].AssignedUserID != 0 {
		t.Errorf("seat1 = %+v", seats[1])
	}
	if seats[2].AssignedUserID != 0 || seats[2].AssignedAssetID != 0 {
		t.Errorf("seat2 (free) = %+v", seats[2])
	}
}

func TestEnsureLicenseDryRunSkipsCreate(t *testing.T) {
	var posted bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posted = true
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total":0,"rows":[]}`)) // empty license list
	}))
	defer srv.Close()
	c := NewLicenseClient(srv.URL, "key", true /*dryRun*/, logrus.New())
	_, err := c.EnsureLicense(context.Background(), LicenseSpec{Name: "X", CategoryID: 1, Seats: 1})
	if !errors.Is(err, ErrDryRun) {
		t.Fatalf("EnsureLicense dry-run = %v, want ErrDryRun", err)
	}
	if posted {
		t.Fatal("dry-run EnsureLicense must not POST")
	}
}

func TestEnsureLicenseCategoryCreates(t *testing.T) {
	var posted map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/categories", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &posted)
			_, _ = w.Write([]byte(`{"status":"success","payload":{"id":42}}`))
			return
		}
		_, _ = w.Write([]byte(`{"total":0,"rows":[]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := NewLicenseClient(srv.URL, "k", false, logrus.New())
	id, err := c.EnsureLicenseCategory(context.Background(), "Software Licenses")
	if err != nil {
		t.Fatal(err)
	}
	if id != 42 {
		t.Fatalf("id = %d, want 42", id)
	}
	if posted["category_type"] != "license" {
		t.Errorf("category_type = %v, want license", posted["category_type"])
	}
}

func TestEnsureLicenseCategoryFindsExisting(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/categories", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			t.Error("must not POST when a license category already exists")
		}
		w.Header().Set("Content-Type", "application/json")
		// Snipe-IT returns category_type title-cased ("License"/"Asset") — the find must match case-insensitively.
		_, _ = w.Write([]byte(`{"total":2,"rows":[{"id":3,"name":"Laptops","category_type":"Asset"},{"id":9,"name":"Software Licenses","category_type":"License"}]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := NewLicenseClient(srv.URL, "k", false, logrus.New())
	id, err := c.EnsureLicenseCategory(context.Background(), "software licenses") // case-insensitive
	if err != nil {
		t.Fatal(err)
	}
	if id != 9 {
		t.Fatalf("id = %d, want 9", id)
	}
}

func TestEnsureLicenseUpdatesExisting(t *testing.T) {
	var patched map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/licenses", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total":1,"rows":[{"id":7,"name":"X","seats":3}]}`))
	})
	mux.HandleFunc("/api/v1/licenses/7", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &patched)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","payload":{}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := NewLicenseClient(srv.URL, "k", false /*not dry-run*/, logrus.New())
	lic, err := c.EnsureLicense(context.Background(), LicenseSpec{Name: "X", CostPerSeat: 9.99, CategoryID: 2, Reassignable: true, Seats: 3})
	if err != nil {
		t.Fatal(err)
	}
	if lic.ID != 7 {
		t.Fatalf("want existing id 7, got %d", lic.ID)
	}
	if patched == nil {
		t.Fatal("existing license was not updated (no PATCH issued)")
	}
	if patched["purchase_cost"] != 9.99 {
		t.Errorf("purchase_cost = %v, want 9.99", patched["purchase_cost"])
	}
}

// TestLicenseClientCancelAbortsBackoff proves the cancellation contract: when the
// context is cancelled on the first 429 (with Retry-After: 1 it would otherwise sleep
// a full second before retrying), do()'s cancel-aware backoff returns promptly with
// context.Canceled instead of waiting out the backoff. The handler cancels the request's
// own context after answering the first hit so the in-flight retry sleep is interrupted.
func TestLicenseClientCancelAbortsBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		// Cancel the run on the first hit, then return a 429 with a 1s Retry-After. The
		// client would normally sleep ~1s before retrying; the cancel must cut it short.
		cancel()
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := NewLicenseClient(srv.URL, "k", false, logrus.New())

	start := time.Now()
	err := c.EnsureSeats(ctx, 1, 5) // first call (GET seat-count) rides the 429 retry path
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed >= 200*time.Millisecond {
		t.Fatalf("took %v, want < 200ms (cancel must abort the Retry-After backoff promptly)", elapsed)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("server saw %d calls, want exactly 1 (no retry after cancel)", got)
	}
}
