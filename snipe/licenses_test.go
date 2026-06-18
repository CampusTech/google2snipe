package snipe

import (
	"errors"
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
