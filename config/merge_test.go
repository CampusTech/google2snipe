package config

import (
	"os"
	"strings"
	"testing"
)

func TestMergeFieldMappingWritesEntries(t *testing.T) {
	p := writeTemp(t, "sync:\n  set_name: false\n")
	err := MergeFieldMapping(p, map[string]FieldMappingEntry{
		"_snipeit_chrome_serial_1": {Path: "serialNumber"},
		"_snipeit_chrome_ram_2":    {Path: "systemRamTotal", Transform: "bytes_to_gb"},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(p)
	cfg, err := loadRaw(t, p)
	if err != nil {
		t.Fatalf("reload: %v\n%s", err, out)
	}
	if cfg.Sync.FieldMapping["_snipeit_chrome_serial_1"].Path != "serialNumber" {
		t.Errorf("serial mapping missing:\n%s", out)
	}
	if got := cfg.Sync.FieldMapping["_snipeit_chrome_ram_2"]; got.Transform != "bytes_to_gb" {
		t.Errorf("ram transform missing:\n%s", out)
	}
	if !strings.Contains(string(out), "field_mapping") {
		t.Errorf("field_mapping section not written:\n%s", out)
	}
}

// loadRaw parses without validation (config is intentionally incomplete here).
func loadRaw(t *testing.T, path string) (*Config, error) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	return &c, yamlUnmarshal(data, &c)
}
