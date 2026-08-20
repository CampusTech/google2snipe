package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
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

func TestChromePerpetualClassification(t *testing.T) {
	cases := map[string]bool{
		"educationUpgradePerpetual":  true,
		"enterpriseUpgradePerpetual": true,
		"educationUpgrade":           true, // deprecated standalone perpetual
		"education":                  true, // bundled perpetual
		"enterprise":                 true, // bundled perpetual
		"educationUpgradeFixedTerm":  false,
		"enterpriseUpgradeFixedTerm": false,
		"enterpriseUpgrade":          false, // deprecated annual
		"kioskUpgrade":               false, // annual
	}
	for typ, want := range cases {
		if got := ChromePerpetual(typ); got != want {
			t.Errorf("ChromePerpetual(%q) = %v, want %v", typ, got, want)
		}
	}
}

func TestLicensesValidationRequiresCategory(t *testing.T) {
	p := writeTemp(t, `
google: {credentials_file: /tmp/sa.json, impersonate_subject: a@b.com}
snipe_it: {url: https://x, api_key: k, default_status_id: 1, default_category_id: 2}
licenses:
  enabled: true
`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error: licenses.enabled requires default_license_category_id")
	}
}

func TestConcurrencyDefaultsToEight(t *testing.T) {
	p := writeTemp(t, `
google: {credentials_file: /tmp/sa.json, impersonate_subject: a@b.com}
snipe_it: {url: https://x, api_key: k, default_status_id: 1, default_category_id: 2}
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sync.Concurrency != 8 {
		t.Fatalf("Concurrency = %d, want 8 (default)", cfg.Sync.Concurrency)
	}
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

// The JWT is minted with exactly these scopes, so license sync's directory-user
// lookups 403 if the default set omits the user scope — regardless of what the
// domain-wide delegation grant allows.
func TestDefaultScopesCoverDirectoryUsers(t *testing.T) {
	c := &Config{}
	c.applyDefaults()
	want := map[string]bool{
		"https://www.googleapis.com/auth/admin.directory.device.chromeos.readonly": false,
		"https://www.googleapis.com/auth/admin.directory.user.readonly":            false,
	}
	for _, s := range c.Google.Scopes {
		if _, ok := want[s]; ok {
			want[s] = true
		}
	}
	for s, found := range want {
		if !found {
			t.Errorf("default scopes missing %s (got %v)", s, c.Google.Scopes)
		}
	}

	// An explicit list still wins, unchanged.
	custom := []string{"https://example.test/custom"}
	c = &Config{}
	c.Google.Scopes = custom
	c.applyDefaults()
	if !reflect.DeepEqual(c.Google.Scopes, custom) {
		t.Errorf("configured scopes = %v, want them left as %v", c.Google.Scopes, custom)
	}
}
// The plan name drives the client's pacing, and the pre-preset booleans must
// keep working for configs written before the presets existed.
func TestRateLimitSettingParsing(t *testing.T) {
	cases := map[string]RateLimitSetting{
		"basic":          RateLimitBasic,
		"small_business": RateLimitSmallBusiness,
		"small-business": RateLimitSmallBusiness,
		"Dedicated":      RateLimitDedicated,
		"true":           RateLimitSmallBusiness,
		"false":          RateLimitDedicated,
	}
	for in, want := range cases {
		var got struct {
			RateLimit RateLimitSetting `yaml:"rate_limit"`
		}
		if err := yaml.Unmarshal([]byte("rate_limit: "+in), &got); err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got.RateLimit != want {
			t.Errorf("rate_limit: %s parsed as %q, want %q", in, got.RateLimit, want)
		}
	}
}

func TestRateLimitSettingDefaultsAndValidates(t *testing.T) {
	c := &Config{}
	c.applyDefaults()
	if c.Sync.RateLimit != RateLimitSmallBusiness {
		t.Errorf("default rate_limit = %q, want small_business", c.Sync.RateLimit)
	}

	c = validConfigForRateLimit()
	c.Sync.RateLimit = "enterprise"
	if err := c.Validate(); err == nil {
		t.Error("an unknown plan name must fail validation rather than silently disabling limiting")
	}
}

// validConfigForRateLimit returns a config that passes Validate, so the test
// above fails only on the rate-limit field.
func validConfigForRateLimit() *Config {
	c := &Config{}
	c.Google.CredentialsFile = "creds.json"
	c.Google.ImpersonateSubject = "admin@example.com"
	c.SnipeIT.URL = "https://snipe.example.com"
	c.SnipeIT.APIKey = "key"
	c.applyDefaults()
	return c
}
