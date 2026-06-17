package sync

import (
	"testing"
	"time"

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

func baseCfg() *config.Config {
	cfg := &config.Config{}
	cfg.SnipeIT.DefaultStatusID = 1
	cfg.SnipeIT.DefaultCategoryID = 2
	cfg.Sync.AssetTag.Template = "{annotatedAssetId}"
	cfg.Sync.FieldMapping = map[string]config.FieldMappingEntry{
		"_snipeit_chrome_status_1": {Path: "status"},
	}
	return cfg
}

func TestSyncDeviceCreatesWhenAbsent(t *testing.T) {
	stub := &stubSnipe{bySerial: map[string][]snipe.Asset{}}
	e := New(baseCfg(), stub, logrus.New())
	if err := e.Warm(); err != nil {
		t.Fatal(err)
	}
	e.SyncDevice(dev(t, &admin.ChromeOsDevice{
		SerialNumber: "S1", Status: "ACTIVE", Model: "Acer Chromebook 311", AnnotatedAssetId: "CG-1",
	}))
	if len(stub.created) != 1 {
		t.Fatalf("created %d assets, want 1", len(stub.created))
	}
	a := stub.created[0]
	if a.Serial != "S1" || a.AssetTag != "CG-1" || a.StatusID != 1 {
		t.Errorf("created asset = %+v", a)
	}
	if a.CustomFields["_snipeit_chrome_status_1"] != "ACTIVE" {
		t.Errorf("custom field not mapped: %+v", a.CustomFields)
	}
}

func TestSyncDeviceUpdatesWhenPresent(t *testing.T) {
	stub := &stubSnipe{bySerial: map[string][]snipe.Asset{
		"S1": {{ID: 7, Serial: "S1", StatusID: 1, CustomFields: map[string]string{}}},
	}}
	cfg := baseCfg()
	cfg.Sync.Force = true // skip freshness gate
	e := New(cfg, stub, logrus.New())
	if err := e.Warm(); err != nil {
		t.Fatal(err)
	}
	e.SyncDevice(dev(t, &admin.ChromeOsDevice{SerialNumber: "S1", Status: "DISABLED", Model: "Acer Chromebook 311"}))
	if len(stub.created) != 0 {
		t.Fatalf("should not create, created=%d", len(stub.created))
	}
	a, ok := stub.patched[7]
	if !ok {
		t.Fatal("expected PatchAsset(7, ...)")
	}
	if a.CustomFields["_snipeit_chrome_status_1"] != "DISABLED" {
		t.Errorf("patch custom fields = %+v", a.CustomFields)
	}
}

func TestSyncDeviceUpdateOnlySkipsCreate(t *testing.T) {
	stub := &stubSnipe{bySerial: map[string][]snipe.Asset{}}
	cfg := baseCfg()
	cfg.Sync.UpdateOnly = true
	e := New(cfg, stub, logrus.New())
	_ = e.Warm()
	e.SyncDevice(dev(t, &admin.ChromeOsDevice{SerialNumber: "S9", Status: "ACTIVE"}))
	if len(stub.created) != 0 {
		t.Errorf("update_only must not create, created=%d", len(stub.created))
	}
}

func TestSyncDeviceSkipsEmptySerial(t *testing.T) {
	stub := &stubSnipe{bySerial: map[string][]snipe.Asset{}}
	e := New(baseCfg(), stub, logrus.New())
	_ = e.Warm()
	e.SyncDevice(dev(t, &admin.ChromeOsDevice{SerialNumber: ""}))
	if len(stub.created) != 0 || e.stats.Total != 1 || e.stats.Skipped != 1 {
		t.Errorf("empty serial should be skipped: created=%d stats=%+v", len(stub.created), e.stats)
	}
}

func TestSyncDeviceFreshnessSkip(t *testing.T) {
	stub := &stubSnipe{bySerial: map[string][]snipe.Asset{
		"S1": {{
			ID:           7,
			Serial:       "S1",
			StatusID:     1,
			CustomFields: map[string]string{},
			UpdatedAt:    time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		}},
	}}
	cfg := baseCfg() // Force defaults false
	e := New(cfg, stub, logrus.New())
	if err := e.Warm(); err != nil {
		t.Fatal(err)
	}
	// LastSync 2024-01-01 is older than asset UpdatedAt 2025-01-01 → freshness skip
	e.SyncDevice(dev(t, &admin.ChromeOsDevice{
		SerialNumber: "S1",
		Status:       "ACTIVE",
		Model:        "Acer Chromebook 311",
		LastSync:     "2024-01-01T00:00:00Z",
	}))
	if len(stub.patched) != 0 {
		t.Errorf("expected no PatchAsset call, got patched=%v", stub.patched)
	}
	if e.stats.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", e.stats.Skipped)
	}
	if e.stats.Updated != 0 {
		t.Errorf("Updated = %d, want 0", e.stats.Updated)
	}
}

func TestSyncDeviceCheckoutSyncReassigns(t *testing.T) {
	cfg := baseCfg()
	cfg.Sync.Force = true
	cfg.Sync.Checkout = config.CheckoutConfig{
		Enabled: true, UseAnnotatedUser: true, FallbackToRecent: false,
		MatchField: "email", Mode: "sync",
	}
	stub := &stubSnipe{
		bySerial: map[string][]snipe.Asset{
			"S1": {{ID: 7, Serial: "S1", StatusID: 1, AssignedToID: 10, CustomFields: map[string]string{}}},
		},
		users: []snipe.User{{ID: 20, Email: "new@example.com"}},
	}
	e := New(cfg, stub, logrus.New())
	if err := e.Warm(); err != nil {
		t.Fatal(err)
	}
	e.SyncDevice(dev(t, &admin.ChromeOsDevice{
		SerialNumber: "S1", Status: "ACTIVE", Model: "Acer Chromebook 311",
		AnnotatedUser: "new@example.com",
	}))

	// Checkin must have been called for asset 7 before checkout
	if len(stub.checkins) != 1 || stub.checkins[0] != 7 {
		t.Errorf("expected checkin of asset 7, got checkins=%v", stub.checkins)
	}
	// Checkout must target the new user (20)
	if stub.checkouts == nil || stub.checkouts[7] != 20 {
		t.Errorf("expected checkout asset 7 -> user 20, got checkouts=%v", stub.checkouts)
	}
}

func TestSyncDeviceAssignModeNoReassign(t *testing.T) {
	cfg := baseCfg()
	cfg.Sync.Force = true
	cfg.Sync.Checkout = config.CheckoutConfig{
		Enabled: true, UseAnnotatedUser: true, FallbackToRecent: false,
		MatchField: "email", Mode: "assign",
	}
	stub := &stubSnipe{
		bySerial: map[string][]snipe.Asset{
			"S1": {{ID: 7, Serial: "S1", StatusID: 1, AssignedToID: 10, CustomFields: map[string]string{}}},
		},
		users: []snipe.User{{ID: 20, Email: "new@example.com"}},
	}
	e := New(cfg, stub, logrus.New())
	if err := e.Warm(); err != nil {
		t.Fatal(err)
	}
	e.SyncDevice(dev(t, &admin.ChromeOsDevice{
		SerialNumber: "S1", Status: "ACTIVE", Model: "Acer Chromebook 311",
		AnnotatedUser: "new@example.com",
	}))

	// assign mode must not checkin or checkout when already assigned
	if len(stub.checkins) != 0 {
		t.Errorf("assign mode must not checkin, got checkins=%v", stub.checkins)
	}
	if len(stub.checkouts) != 0 {
		t.Errorf("assign mode must not checkout when already assigned, got checkouts=%v", stub.checkouts)
	}
}

func TestSyncDeviceDryRunNoMutators(t *testing.T) {
	cfg := baseCfg()
	cfg.Sync.DryRun = true
	stub := &stubSnipe{bySerial: map[string][]snipe.Asset{}}
	e := New(cfg, stub, logrus.New())
	if err := e.Warm(); err != nil {
		t.Fatal(err)
	}
	e.SyncDevice(dev(t, &admin.ChromeOsDevice{
		SerialNumber:     "S2",
		Status:           "ACTIVE",
		Model:            "Acer Chromebook 311",
		AnnotatedAssetId: "CG-2",
	}))
	if e.stats.Created != 1 {
		t.Errorf("Created = %d, want 1", e.stats.Created)
	}
	if len(stub.created) != 0 {
		t.Errorf("dry-run must not call CreateAsset, got created=%v", stub.created)
	}
}
