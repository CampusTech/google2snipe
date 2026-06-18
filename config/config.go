package config

import (
	"fmt"
	"log"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the top-level YAML config. See Shared Type Reference in the plan.
type Config struct {
	Google   GoogleConfig   `yaml:"google"`
	SnipeIT  SnipeITConfig  `yaml:"snipe_it"`
	Sync     SyncConfig     `yaml:"sync"`
	Licenses LicensesConfig `yaml:"licenses"`
}

type GoogleConfig struct {
	CredentialsFile    string   `yaml:"credentials_file"`
	ImpersonateSubject string   `yaml:"impersonate_subject"`
	CustomerID         string   `yaml:"customer_id"`
	Projection         string   `yaml:"projection"`
	OrgUnitPath        string   `yaml:"org_unit_path"`
	Query              string   `yaml:"query"`
	Scopes             []string `yaml:"scopes"`
}

type SnipeITConfig struct {
	URL                   string         `yaml:"url"`
	APIKey                string         `yaml:"api_key"`
	DefaultStatusID       int            `yaml:"default_status_id"`
	DefaultCategoryID     int            `yaml:"default_category_id"`
	DefaultManufacturerID int            `yaml:"default_manufacturer_id"`
	CustomFieldsetID      int            `yaml:"custom_fieldset_id"`
	StatusMap             map[string]int `yaml:"status_map"`
	ManufacturerIDs       map[string]int `yaml:"manufacturer_ids"`
}

type SyncConfig struct {
	DryRun           bool                         `yaml:"dry_run"`
	Force            bool                         `yaml:"force"`
	RateLimit        bool                         `yaml:"rate_limit"`
	UpdateOnly       bool                         `yaml:"update_only"`
	UseCache         bool                         `yaml:"use_cache"`
	CacheDir         string                       `yaml:"cache_dir"`
	SetName          bool                         `yaml:"set_name"`
	NameTemplate     string                       `yaml:"name_template"`
	StripModelVendor bool                         `yaml:"strip_model_vendor"`
	AssetTag         AssetTagConfig               `yaml:"asset_tag"`
	FieldMapping     map[string]FieldMappingEntry `yaml:"field_mapping"`
	Checkout         CheckoutConfig               `yaml:"checkout"`
	Concurrency      int                          `yaml:"concurrency"`
}

type AssetTagConfig struct {
	Template string `yaml:"template"`
}

// FieldMappingEntry accepts either a bare string (path only) or a
// {path, transform} mapping in YAML.
type FieldMappingEntry struct {
	Path      string `yaml:"path"`
	Transform string `yaml:"transform"`
}

func (e *FieldMappingEntry) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		e.Path = value.Value
		return nil
	}
	type raw FieldMappingEntry
	var r raw
	if err := value.Decode(&r); err != nil {
		return err
	}
	*e = FieldMappingEntry(r)
	return nil
}

type CheckoutConfig struct {
	Enabled          bool   `yaml:"enabled"`
	UseAnnotatedUser bool   `yaml:"use_annotated_user"`
	FallbackToRecent bool   `yaml:"fallback_to_recent"`
	RecentUserDomain string `yaml:"recent_user_domain"`
	MatchField       string `yaml:"match_field"`
	Mode             string `yaml:"mode"`
}

type LicensesConfig struct {
	Enabled                  bool                           `yaml:"enabled"`
	DefaultLicenseCategoryID int                            `yaml:"default_license_category_id"`
	Chrome                   map[string]ChromeLicenseConfig `yaml:"chrome"`
	Workspace                WorkspaceLicenseConfig         `yaml:"workspace"`
}

type ChromeLicenseConfig struct {
	Name         string  `yaml:"name"`
	Cost         float64 `yaml:"cost"`
	Reassignable *bool   `yaml:"reassignable"`
	TermMonths   int     `yaml:"term_months"`
}

type WorkspaceLicenseConfig struct {
	CustomerID string             `yaml:"customer_id"`
	Products   []string           `yaml:"products"`
	SKUCosts   map[string]float64 `yaml:"sku_costs"`
}

// ChromePerpetual reports whether a ChromeOS deviceLicenseType is a perpetual
// (non-reassignable) upgrade. Recurring = fixed-term or annual.
func ChromePerpetual(deviceLicenseType string) bool {
	if strings.Contains(strings.ToLower(deviceLicenseType), "fixedterm") {
		return false
	}
	switch deviceLicenseType {
	case "enterpriseUpgrade", "kioskUpgrade": // deprecated-annual / kiosk-annual
		return false
	}
	return true
}

// KnownTransforms is the set of transform names accepted in field_mapping.
var KnownTransforms = map[string]bool{
	"": true, "bytes_to_gb": true, "bytes_to_gib": true, "bytes_to_mb": true,
	"bytes_to_tb": true, "mac_colons": true, "mac_dashes": true,
	"bool_yes_no": true, "uppercase": true, "lowercase": true,
	"comma_thousands": true, "unix_to_iso": true,
	"date_only": true, "datetime": true,
}

// FullOnlyPaths are gjson path prefixes only populated under projection=full.
var FullOnlyPaths = map[string]bool{
	"recentUsers": true, "activeTimeRanges": true, "cpuStatusReports": true,
	"cpuInfo": true, "diskVolumeReports": true, "systemRamFreeReports": true,
	"deviceFiles": true, "screenshotFiles": true, "lastKnownNetwork": true,
	"backlightInfo": true, "fanInfo": true, "bluetoothAdapterInfo": true,
	"diskSpaceUsage": true, "tpmVersionInfo": true,
}

// loadConfig reads, applies env overrides + defaults, and validates a config file.
// It does NOT check for the licenses category requirement.
func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.applyEnv()
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Load reads, validates, and returns the config. It additionally requires a
// license category id once license cost sync is enabled.
func Load(path string) (*Config, error) {
	c, err := loadConfig(path)
	if err != nil {
		return nil, err
	}
	if c.Licenses.Enabled && c.Licenses.DefaultLicenseCategoryID == 0 {
		return nil, fmt.Errorf("licenses.default_license_category_id is required when licenses.enabled")
	}
	return c, nil
}

// LoadForSetup is like Load but tolerates licenses.enabled without a category id,
// so `licenses setup` can run before that id is configured.
func LoadForSetup(path string) (*Config, error) {
	return loadConfig(path)
}

func (c *Config) applyEnv() {
	if v := os.Getenv("SNIPE_URL"); v != "" {
		c.SnipeIT.URL = v
	}
	if v := os.Getenv("SNIPE_API_KEY"); v != "" {
		c.SnipeIT.APIKey = v
	}
	if v := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"); v != "" && c.Google.CredentialsFile == "" {
		c.Google.CredentialsFile = v
	}
	if v := os.Getenv("GOOGLE_IMPERSONATE_SUBJECT"); v != "" {
		c.Google.ImpersonateSubject = v
	}
	if v := os.Getenv("GOOGLE_CUSTOMER_ID"); v != "" {
		c.Google.CustomerID = v
	}
}

func (c *Config) applyDefaults() {
	if c.Google.CustomerID == "" {
		c.Google.CustomerID = "my_customer"
	}
	if c.Google.Projection == "" {
		c.Google.Projection = "full"
	}
	c.Google.Projection = strings.ToLower(c.Google.Projection)
	if len(c.Google.Scopes) == 0 {
		c.Google.Scopes = []string{"https://www.googleapis.com/auth/admin.directory.device.chromeos.readonly"}
	}
	if c.Sync.CacheDir == "" {
		c.Sync.CacheDir = ".cache"
	}
	if c.Sync.AssetTag.Template == "" {
		c.Sync.AssetTag.Template = "{annotatedAssetId}"
	}
	if c.Sync.Checkout.MatchField == "" {
		c.Sync.Checkout.MatchField = "email"
	}
	if c.Sync.Checkout.Mode == "" {
		c.Sync.Checkout.Mode = "assign"
	}
	if c.Sync.Concurrency == 0 {
		c.Sync.Concurrency = 8
	}
}

// Validate fails fast on missing required fields and bad enum values.
func (c *Config) Validate() error {
	if c.Google.CredentialsFile == "" {
		return fmt.Errorf("google.credentials_file (or GOOGLE_APPLICATION_CREDENTIALS) is required")
	}
	if c.Google.ImpersonateSubject == "" {
		return fmt.Errorf("google.impersonate_subject is required for domain-wide delegation")
	}
	if c.Google.Projection != "full" && c.Google.Projection != "basic" {
		return fmt.Errorf("google.projection must be full or basic, got %q", c.Google.Projection)
	}
	if c.SnipeIT.URL == "" {
		return fmt.Errorf("snipe_it.url is required")
	}
	if c.SnipeIT.APIKey == "" {
		return fmt.Errorf("snipe_it.api_key is required")
	}
	if c.SnipeIT.DefaultStatusID == 0 {
		return fmt.Errorf("snipe_it.default_status_id is required")
	}
	if c.SnipeIT.DefaultCategoryID == 0 {
		return fmt.Errorf("snipe_it.default_category_id is required")
	}
	for col, e := range c.Sync.FieldMapping {
		if e.Path == "" {
			return fmt.Errorf("field_mapping[%s]: empty path", col)
		}
		if !KnownTransforms[e.Transform] {
			return fmt.Errorf("field_mapping[%s]: unknown transform %q", col, e.Transform)
		}
		if c.Google.Projection == "basic" {
			prefix := e.Path
			if i := strings.IndexAny(prefix, ".#"); i >= 0 {
				prefix = prefix[:i]
			}
			if FullOnlyPaths[prefix] {
				log.Printf("warning: field_mapping[%s] path %q requires projection=full but projection=basic", col, e.Path)
			}
		}
	}
	switch c.Sync.Checkout.MatchField {
	case "email", "username", "employee_num":
	default:
		return fmt.Errorf("checkout.match_field must be email|username|employee_num, got %q", c.Sync.Checkout.MatchField)
	}
	switch c.Sync.Checkout.Mode {
	case "assign", "sync", "force":
	default:
		return fmt.Errorf("checkout.mode must be assign|sync|force, got %q", c.Sync.Checkout.Mode)
	}
	return nil
}
