package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "settings.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDefaultsAndBareFieldMapping(t *testing.T) {
	p := writeTemp(t, `
google:
  credentials_file: /tmp/sa.json
  impersonate_subject: admin@example.com
snipe_it:
  url: https://snipe.example.com
  api_key: abc
  default_status_id: 1
  default_category_id: 2
sync:
  field_mapping:
    _snipeit_chrome_serial_1: serialNumber
    _snipeit_chrome_ram_2:
      path: systemRamTotal
      transform: bytes_to_gb
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Google.CustomerID != "my_customer" {
		t.Errorf("customer_id default = %q, want my_customer", cfg.Google.CustomerID)
	}
	if cfg.Google.Projection != "full" {
		t.Errorf("projection default = %q, want full", cfg.Google.Projection)
	}
	if got := cfg.Sync.FieldMapping["_snipeit_chrome_serial_1"]; got.Path != "serialNumber" || got.Transform != "" {
		t.Errorf("bare mapping = %+v, want {serialNumber }", got)
	}
	if got := cfg.Sync.FieldMapping["_snipeit_chrome_ram_2"]; got.Path != "systemRamTotal" || got.Transform != "bytes_to_gb" {
		t.Errorf("object mapping = %+v", got)
	}
}

func TestValidateRejectsUnknownTransform(t *testing.T) {
	p := writeTemp(t, `
google: {credentials_file: /tmp/sa.json, impersonate_subject: a@b.com}
snipe_it: {url: https://x, api_key: k, default_status_id: 1, default_category_id: 2}
sync:
  field_mapping:
    _snipeit_x_1: {path: model, transform: not_a_transform}
`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for unknown transform")
	}
}

func TestValidateRequiresSnipeAndGoogle(t *testing.T) {
	p := writeTemp(t, `
google: {impersonate_subject: a@b.com}
snipe_it: {url: https://x, api_key: k, default_status_id: 1, default_category_id: 2}
`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error: missing credentials_file and GOOGLE_APPLICATION_CREDENTIALS")
	}
}
