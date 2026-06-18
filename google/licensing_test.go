package google

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sirupsen/logrus"
	licensing "google.golang.org/api/licensing/v1"
	"google.golang.org/api/option"
)

func TestListAssignmentsPaginates(t *testing.T) {
	page1 := `{"items":[{"userId":"a@x.edu","productId":"Google-Apps","skuId":"1010310008","skuName":"Education Plus"}],"nextPageToken":"tok"}`
	page2 := `{"items":[{"userId":"b@x.edu","productId":"Google-Apps","skuId":"1010310008","skuName":"Education Plus"}]}`
	mux := http.NewServeMux()
	mux.HandleFunc("/apps/licensing/v1/product/Google-Apps/users", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("pageToken") == "tok" {
			_, _ = w.Write([]byte(page2))
			return
		}
		_, _ = w.Write([]byte(page1))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	svc, err := licensing.NewService(context.Background(), option.WithoutAuthentication(), option.WithEndpoint(srv.URL+"/"))
	if err != nil {
		t.Fatal(err)
	}
	c := &LicensingClient{svc: svc, customerID: "x.edu", log: logrus.New()}
	got, err := c.ListAssignments(context.Background(), []string{"Google-Apps"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].UserEmail != "a@x.edu" || got[1].SKUID != "1010310008" {
		t.Fatalf("paging/parse failed: %+v", got)
	}
}
