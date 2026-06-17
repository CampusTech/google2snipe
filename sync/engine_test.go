package sync

import (
	"testing"

	"github.com/sirupsen/logrus"
	admin "google.golang.org/api/admin/directory/v1"

	"github.com/CampusTech/google2snipe/config"
	"github.com/CampusTech/google2snipe/google"
	"github.com/CampusTech/google2snipe/snipe"
)

func testEngine(t *testing.T, cfg *config.Config) *Engine {
	t.Helper()
	return New(cfg, &stubSnipe{}, logrus.New())
}

func dev(t *testing.T, d *admin.ChromeOsDevice) google.Device {
	t.Helper()
	devs, err := google.DeserializeDevices(mustJSON(t, d))
	if err != nil {
		t.Fatal(err)
	}
	return devs[0]
}

func TestApplyMappingSkipsEmpty(t *testing.T) {
	cfg := &config.Config{}
	cfg.Sync.FieldMapping = map[string]config.FieldMappingEntry{
		"_snipeit_serial_1": {Path: "serialNumber"},
		"_snipeit_ram_2":    {Path: "systemRamTotal", Transform: "bytes_to_gb"},
		"_snipeit_notes_3":  {Path: "notes"}, // empty -> skipped
	}
	e := testEngine(t, cfg)
	d := dev(t, &admin.ChromeOsDevice{SerialNumber: "S1", SystemRamTotal: int64(8000000000)})
	out := e.applyMapping(d)
	if out["_snipeit_serial_1"] != "S1" {
		t.Errorf("serial = %q", out["_snipeit_serial_1"])
	}
	if out["_snipeit_ram_2"] != "8" {
		t.Errorf("ram = %q, want 8", out["_snipeit_ram_2"])
	}
	if _, ok := out["_snipeit_notes_3"]; ok {
		t.Error("empty notes should be skipped")
	}
}

func TestStatusIDMapAndDefault(t *testing.T) {
	cfg := &config.Config{}
	cfg.SnipeIT.DefaultStatusID = 1
	cfg.SnipeIT.StatusMap = map[string]int{"DEPROVISIONED": 3}
	e := testEngine(t, cfg)
	if got := e.statusID(dev(t, &admin.ChromeOsDevice{Status: "DEPROVISIONED"})); got != 3 {
		t.Errorf("mapped status = %d, want 3", got)
	}
	if got := e.statusID(dev(t, &admin.ChromeOsDevice{Status: "ACTIVE"})); got != 1 {
		t.Errorf("unmapped status = %d, want default 1", got)
	}
	cfg.SnipeIT.StatusMap["ZERO_MAPPED"] = 0
	if got := e.statusID(dev(t, &admin.ChromeOsDevice{Status: "ZERO_MAPPED"})); got != 1 {
		t.Errorf("zero-mapped status should fall through to default 1, got %d", got)
	}
}

func TestAssetTagTemplate(t *testing.T) {
	cfg := &config.Config{}
	cfg.Sync.AssetTag.Template = "{annotatedAssetId}"
	e := testEngine(t, cfg)
	if got := e.assetTag(dev(t, &admin.ChromeOsDevice{AnnotatedAssetId: "CG-42"})); got != "CG-42" {
		t.Errorf("asset tag = %q, want CG-42", got)
	}
	if got := e.assetTag(dev(t, &admin.ChromeOsDevice{})); got != "" {
		t.Errorf("empty annotatedAssetId should yield empty tag, got %q", got)
	}
	cfg2 := &config.Config{} // AssetTag.Template stays ""
	e2 := testEngine(t, cfg2)
	if got := e2.assetTag(dev(t, &admin.ChromeOsDevice{AnnotatedAssetId: "CG-42"})); got != "" {
		t.Errorf("empty template should yield empty tag, got %q", got)
	}
}

func TestResolveCheckoutAnnotatedThenRecentWithDomain(t *testing.T) {
	cfg := &config.Config{}
	cfg.Sync.Checkout = config.CheckoutConfig{
		Enabled: true, UseAnnotatedUser: true, FallbackToRecent: true,
		RecentUserDomain: "school.edu", MatchField: "email", Mode: "assign",
	}
	stub := &stubSnipe{users: []snipe.User{
		{ID: 10, Email: "assigned@school.edu"},
		{ID: 20, Email: "kid@school.edu"},
	}}
	e := New(cfg, stub, logrus.New())
	if err := e.Warm(); err != nil {
		t.Fatal(err)
	}

	// annotatedUser present -> use it
	d1 := dev(t, &admin.ChromeOsDevice{AnnotatedUser: "assigned@school.edu"})
	if uid, ok := e.resolveCheckoutUser(d1); !ok || uid != 10 {
		t.Errorf("annotated -> uid=%d ok=%v, want 10,true", uid, ok)
	}

	// no annotatedUser -> first recent user in the allowed domain (skip guest)
	d2 := dev(t, &admin.ChromeOsDevice{RecentUsers: []*admin.ChromeOsDeviceRecentUsers{
		{Type: "USER_TYPE_UNMANAGED", Email: "guest@gmail.com"},
		{Type: "USER_TYPE_MANAGED", Email: "kid@school.edu"},
	}})
	if uid, ok := e.resolveCheckoutUser(d2); !ok || uid != 20 {
		t.Errorf("recent -> uid=%d ok=%v, want 20,true", uid, ok)
	}

	// recent user outside domain -> no match
	d3 := dev(t, &admin.ChromeOsDevice{RecentUsers: []*admin.ChromeOsDeviceRecentUsers{
		{Type: "USER_TYPE_MANAGED", Email: "someone@other.edu"},
	}})
	if _, ok := e.resolveCheckoutUser(d3); ok {
		t.Error("recent user outside domain should not match")
	}
}

func TestEnsureManufacturerFromModel(t *testing.T) {
	cfg := &config.Config{}
	cfg.SnipeIT.DefaultManufacturerID = 0
	stub := &stubSnipe{}
	e := New(cfg, stub, logrus.New())
	if err := e.Warm(); err != nil {
		t.Fatal(err)
	}
	id, err := e.ensureManufacturer(dev(t, &admin.ChromeOsDevice{Model: "Lenovo 300e Chromebook"}))
	if err != nil || id == 0 {
		t.Fatalf("ensureManufacturer = %d, %v", id, err)
	}
	if len(stub.manufs) != 1 || stub.manufs[0].Name != "Lenovo" {
		t.Errorf("created manufacturer = %+v, want name Lenovo", stub.manufs)
	}
}
