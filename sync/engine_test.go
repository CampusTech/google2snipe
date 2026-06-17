package sync

import (
	"testing"

	"github.com/sirupsen/logrus"
	admin "google.golang.org/api/admin/directory/v1"

	"github.com/CampusTech/google2snipe/config"
	"github.com/CampusTech/google2snipe/google"
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
	d := dev(t, &admin.ChromeOsDevice{SerialNumber: "S1", SystemRamTotal: 8000000000})
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
}
