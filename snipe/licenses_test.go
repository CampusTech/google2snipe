package snipe

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
)

func TestLicenseClientDryRunSentinel(t *testing.T) {
	c := NewLicenseClient("https://snipe.invalid", "key", true /*dryRun*/, logrus.New())
	// EnsureSeats is a pure mutator: in dry-run it must return ErrDryRun before any HTTP.
	if err := c.EnsureSeats(1, 5); !errors.Is(err, ErrDryRun) {
		t.Fatalf("EnsureSeats dry-run = %v, want ErrDryRun", err)
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
	_, err := c.EnsureLicense(LicenseSpec{Name: "X", CategoryID: 1, Seats: 1})
	if err == nil || !strings.Contains(err.Error(), "HTTP 422") {
		t.Fatalf("want HTTP 422 error, got %v", err)
	}
}

func TestSeatMutatorsDryRun(t *testing.T) {
	c := NewLicenseClient("https://snipe.invalid", "k", true /*dryRun*/, logrus.New())
	if err := c.CheckoutSeatToUser(1, 2, 3); !errors.Is(err, ErrDryRun) {
		t.Fatalf("CheckoutSeatToUser = %v", err)
	}
	if err := c.CheckoutSeatToAsset(1, 2, 3); !errors.Is(err, ErrDryRun) {
		t.Fatalf("CheckoutSeatToAsset = %v", err)
	}
	if err := c.CheckinSeat(1, 2); !errors.Is(err, ErrDryRun) {
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
	seats, err := c.ListSeats(42)
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
	_, err := c.EnsureLicense(LicenseSpec{Name: "X", CategoryID: 1, Seats: 1})
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
	id, err := c.EnsureLicenseCategory("Software Licenses")
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
	id, err := c.EnsureLicenseCategory("software licenses") // case-insensitive
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
	lic, err := c.EnsureLicense(LicenseSpec{Name: "X", CostPerSeat: 9.99, CategoryID: 2, Reassignable: true, Seats: 3})
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
