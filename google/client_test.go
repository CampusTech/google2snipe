package google

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sirupsen/logrus"
	admin "google.golang.org/api/admin/directory/v1"
	"google.golang.org/api/option"
)

// newTestClient builds a Client whose admin.Service points at a fake server.
func newTestClient(t *testing.T, srvURL string) *Client {
	t.Helper()
	svc, err := admin.NewService(context.Background(),
		option.WithoutAuthentication(),
		option.WithEndpoint(srvURL),
	)
	if err != nil {
		t.Fatal(err)
	}
	return &Client{svc: svc, customerID: "my_customer", projection: "FULL", log: logrus.New()}
}

func TestListAllChromeOSDevicesPaginates(t *testing.T) {
	page1 := `{"chromeosdevices":[{"deviceId":"d1","serialNumber":"S1"}],"nextPageToken":"tok"}`
	page2 := `{"chromeosdevices":[{"deviceId":"d2","serialNumber":"S2"}]}`
	mux := http.NewServeMux()
	mux.HandleFunc("/admin/directory/v1/customer/my_customer/devices/chromeos", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("pageToken") == "tok" {
			_, _ = w.Write([]byte(page2))
			return
		}
		_, _ = w.Write([]byte(page1))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv.URL+"/")
	devs, err := c.ListAllChromeOSDevices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 2 || devs[0].SerialNumber != "S1" || devs[1].SerialNumber != "S2" {
		t.Fatalf("paging failed: got %d devices %+v", len(devs), devs)
	}
	if string(devs[0].Raw) == "" {
		t.Error("Raw not populated")
	}
}

func TestListAllChromeOSDevicesFilters(t *testing.T) {
	wantOrgUnit := "/Engineering"
	wantQuery := "os:Chrome"

	var gotOrgUnit, gotQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("/admin/directory/v1/customer/my_customer/devices/chromeos", func(w http.ResponseWriter, r *http.Request) {
		gotOrgUnit = r.URL.Query().Get("orgUnitPath")
		gotQuery = r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"chromeosdevices":[]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv.URL+"/")
	c.orgUnit = wantOrgUnit
	c.query = wantQuery

	if _, err := c.ListAllChromeOSDevices(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotOrgUnit != wantOrgUnit {
		t.Errorf("orgUnitPath: got %q, want %q", gotOrgUnit, wantOrgUnit)
	}
	if gotQuery != wantQuery {
		t.Errorf("query: got %q, want %q", gotQuery, wantQuery)
	}
}
