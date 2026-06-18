package licensesync

import (
	"encoding/json"
	"testing"

	"github.com/sirupsen/logrus"
	admin "google.golang.org/api/admin/directory/v1"

	"github.com/CampusTech/google2snipe/config"
	"github.com/CampusTech/google2snipe/google"
)

func mustJSONChrome(t *testing.T, d *admin.ChromeOsDevice) []byte {
	t.Helper()
	b, err := json.Marshal([]*admin.ChromeOsDevice{d})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func devWith(t *testing.T, serial, lic string) google.Device {
	t.Helper()
	d, err := google.DeserializeDevices(mustJSONChrome(t, &admin.ChromeOsDevice{SerialNumber: serial, DeviceLicenseType: lic}))
	if err != nil {
		t.Fatal(err)
	}
	return d[0]
}

func TestSyncChromePerpetualPerDeviceAsset(t *testing.T) {
	stub := &stubLC{}
	e := New(stub, logrus.New())
	cfg := config.LicensesConfig{
		Enabled:                  true,
		DefaultLicenseCategoryID: 7,
		Chrome: map[string]config.ChromeLicenseConfig{
			"educationUpgradePerpetual": {Name: "Chrome EDU Perpetual", Cost: 38},
		},
	}
	devs := []google.Device{
		devWith(t, "S1", "educationUpgradePerpetual"),
		devWith(t, "S2", "educationUpgradePerpetual"),
		devWith(t, "S3", ""), // no license -> skipped
	}
	assetID := map[string]int{"S1": 101, "S2": 102}
	err := e.SyncChrome(cfg, devs, func(serial string) (int, bool) { id, ok := assetID[serial]; return id, ok })
	if err != nil {
		t.Fatal(err)
	}
	assets := map[int]bool{}
	for _, s := range stub.seats {
		if s.AssignedAssetID != 0 {
			assets[s.AssignedAssetID] = true
		}
	}
	if !assets[101] || !assets[102] {
		t.Errorf("want assets 101,102 seated; got %v", assets)
	}
}
