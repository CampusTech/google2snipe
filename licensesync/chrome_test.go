package licensesync

import (
	"encoding/json"
	"testing"

	"github.com/sirupsen/logrus"
	admin "google.golang.org/api/admin/directory/v1"

	"github.com/CampusTech/google2snipe/config"
	"github.com/CampusTech/google2snipe/google"
	"github.com/CampusTech/google2snipe/snipe"
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

// recordingLC records the LicenseSpec passed to EnsureLicense so a test can assert
// how SyncChrome translates a deviceLicenseType into the license's reassignable flag.
type recordingLC struct {
	stubLC
	specs []snipe.LicenseSpec
}

func (r *recordingLC) EnsureLicense(spec snipe.LicenseSpec) (snipe.License, error) {
	r.specs = append(r.specs, spec)
	return r.stubLC.EnsureLicense(spec)
}

// TestSyncChromeSetsReassignableFromType guards the central perpetual-vs-recurring
// semantic: a perpetual type must yield Reassignable=false, a fixed-term type true.
func TestSyncChromeSetsReassignableFromType(t *testing.T) {
	rec := &recordingLC{}
	e := New(rec, logrus.New())
	cfg := config.LicensesConfig{
		Enabled:                  true,
		DefaultLicenseCategoryID: 7,
		Chrome: map[string]config.ChromeLicenseConfig{
			"educationUpgradePerpetual":  {Name: "Perp"},      // perpetual  -> reassignable=false
			"enterpriseUpgradeFixedTerm": {Name: "Recurring"}, // fixed-term -> reassignable=true
		},
	}
	devs := []google.Device{
		devWith(t, "P1", "educationUpgradePerpetual"),
		devWith(t, "R1", "enterpriseUpgradeFixedTerm"),
	}
	asset := map[string]int{"P1": 201, "R1": 202}
	if err := e.SyncChrome(cfg, devs, func(s string) (int, bool) { id, ok := asset[s]; return id, ok }); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, s := range rec.specs {
		got[s.Name] = s.Reassignable
	}
	if len(rec.specs) != 2 {
		t.Fatalf("expected 2 EnsureLicense specs, got %d", len(rec.specs))
	}
	if v, ok := got["Perp"]; !ok || v != false {
		t.Errorf("perpetual license Reassignable = %v (present=%v), want false", v, ok)
	}
	if v, ok := got["Recurring"]; !ok || v != true {
		t.Errorf("fixed-term license Reassignable = %v (present=%v), want true", v, ok)
	}
}
