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

// TestEnvOverrideCredentials verifies GOOGLE_APPLICATION_CREDENTIALS is used when
// credentials_file is absent from YAML, but that YAML wins when both are set.
func TestEnvOverrideCredentials(t *testing.T) {
	baseYAML := `
google:
  impersonate_subject: a@b.com
snipe_it:
  url: https://snipe.example.com
  api_key: abc
  default_status_id: 1
  default_category_id: 2
`
	// Case 1: env var only — no credentials_file in YAML.
	t.Run("env_only", func(t *testing.T) {
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/env/sa.json")
		p := writeTemp(t, baseYAML)
		cfg, err := Load(p)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Google.CredentialsFile != "/env/sa.json" {
			t.Errorf("CredentialsFile = %q, want /env/sa.json", cfg.Google.CredentialsFile)
		}
	})

	// Case 2: YAML credentials_file set AND env var set — YAML wins.
	t.Run("yaml_wins_over_env", func(t *testing.T) {
		t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/env/sa.json")
		p := writeTemp(t, `
google:
  credentials_file: /yaml/sa.json
  impersonate_subject: a@b.com
snipe_it:
  url: https://snipe.example.com
  api_key: abc
  default_status_id: 1
  default_category_id: 2
`)
		cfg, err := Load(p)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Google.CredentialsFile != "/yaml/sa.json" {
			t.Errorf("CredentialsFile = %q, want /yaml/sa.json (YAML should win)", cfg.Google.CredentialsFile)
		}
	})
}

// TestCheckoutEnumRejection verifies that invalid mode and match_field values are rejected.
func TestCheckoutEnumRejection(t *testing.T) {
	base := `
google:
  credentials_file: /tmp/sa.json
  impersonate_subject: a@b.com
snipe_it:
  url: https://snipe.example.com
  api_key: abc
  default_status_id: 1
  default_category_id: 2
`
	t.Run("bad_mode", func(t *testing.T) {
		p := writeTemp(t, base+`
sync:
  checkout:
    mode: bogus
`)
		if _, err := Load(p); err == nil {
			t.Fatal("expected error for invalid checkout.mode")
		}
	})

	t.Run("bad_match_field", func(t *testing.T) {
		p := writeTemp(t, base+`
sync:
  checkout:
    match_field: notafield
`)
		if _, err := Load(p); err == nil {
			t.Fatal("expected error for invalid checkout.match_field")
		}
	})
}

// TestFullOnlyPathsWarningNotError verifies that a field_mapping with a FullOnly path
// under projection=basic is a warning (Load succeeds) and the mapping is present.
func TestFullOnlyPathsWarningNotError(t *testing.T) {
	p := writeTemp(t, `
google:
  credentials_file: /tmp/sa.json
  impersonate_subject: a@b.com
  projection: basic
snipe_it:
  url: https://snipe.example.com
  api_key: abc
  default_status_id: 1
  default_category_id: 2
sync:
  field_mapping:
    _snipeit_recent_user_1: recentUsers.0.email
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load returned error (expected only a warning): %v", err)
	}
	got, ok := cfg.Sync.FieldMapping["_snipeit_recent_user_1"]
	if !ok {
		t.Fatal("field_mapping entry missing after Load")
	}
	if got.Path != "recentUsers.0.email" {
		t.Errorf("mapping path = %q, want recentUsers.0.email", got.Path)
	}
}
