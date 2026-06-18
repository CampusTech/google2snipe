package snipe

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
)

func TestDryRunBlocksCreate(t *testing.T) {
	c, err := New("https://snipe.invalid", "key", true /*dryRun*/, false, logrus.New())
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.CreateAsset(Asset{Serial: "X1", ModelID: 1, StatusID: 1})
	if !errors.Is(err, ErrDryRun) {
		t.Fatalf("CreateAsset in dry-run = %v, want ErrDryRun", err)
	}
}

func TestListAllAssetsPaginates(t *testing.T) {
	page1 := `{"total":2,"rows":[{"id":1,"asset_tag":"A1","serial":"S1"}]}`
	page2 := `{"total":2,"rows":[{"id":2,"asset_tag":"A2","serial":"S2"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.RawQuery, "offset=500") {
			_, _ = w.Write([]byte(page2))
			return
		}
		_, _ = w.Write([]byte(page1))
	}))
	defer srv.Close()
	c, err := New(srv.URL, "k", false, false, logrus.New())
	if err != nil {
		t.Fatal(err)
	}
	assets, err := c.ListAllAssets()
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 2 || assets[0].Serial != "S1" || assets[1].Serial != "S2" {
		t.Fatalf("paging failed: %+v", assets)
	}
}
