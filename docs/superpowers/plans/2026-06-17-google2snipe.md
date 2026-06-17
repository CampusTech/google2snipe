# google2snipe Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a Go CLI that syncs ChromeOS devices from the Google Admin SDK Directory API into Snipe-IT, mirroring fleet2snipe's architecture and feature set.

**Architecture:** Cobra command tree (`sync`, `setup`, `test`) over four packages: `config/` (YAML + validation), `google/` (official `admin/directory/v1` SDK wrapper that lists/gets ChromeOS devices and retains raw JSON for gjson), `snipe/` (go-snipeit wrapper, ported from fleet2snipe), and `sync/` (the reconciliation engine — field mapping, transforms, status mapping, checkout). The engine matches assets by serial, upserts them, maps device fields into Snipe custom fields via gjson paths + transforms, maps ChromeOS lifecycle status to Snipe status labels, and optionally checks out to the assigned/recent user.

**Tech Stack:** Go 1.26.4, `google.golang.org/api/admin/directory/v1`, `golang.org/x/oauth2/google`, `google.golang.org/api/option`, `github.com/michellepellon/go-snipeit`, `github.com/spf13/cobra`, `github.com/sirupsen/logrus`, `github.com/tidwall/gjson`, `gopkg.in/yaml.v3`.

**Reference implementation:** fleet2snipe lives at `/Users/robbiet480/go/src/github.com/CampusTech/fleet2snipe`. Referred to below as `$FLEET`. Read its files when a task says "port from $FLEET" — the `snipe/` package and config YAML-merge logic are data-source-agnostic and should be copied with the minimal adaptations shown.

**Spec:** `docs/superpowers/specs/2026-06-17-google2snipe-design.md` (read it first).

## Global Constraints

- **Module path:** `github.com/CampusTech/google2snipe`
- **Go version:** 1.26.4 (`go 1.26.4` in go.mod)
- **Scope:** ChromeOS devices ONLY. No other Google device types.
- **Projection:** default `full` (required for `recentUsers` + report arrays); `basic` is an opt-down.
- **Customer ID:** default `my_customer`.
- **Match key:** `serialNumber`, case-insensitive. 0→create, 1→update, >1→skip+warn.
- **Empty values are never written** to Snipe custom fields (gjson miss / failed transform → `""` → skipped).
- **Dry-run** must be enforced in `snipe/` before every mutation.
- **Logging:** logrus, structured fields (never string interpolation), per-package loggers; levels via `--verbose`/`--debug`; `text`/`json` formats.
- **Auth:** service-account JSON key (config `credentials_file` or `GOOGLE_APPLICATION_CREDENTIALS`) + domain-wide delegation impersonating `impersonate_subject`. Scope `admin.AdminDirectoryDeviceChromeosReadonlyScope`.
- **License:** MIT (already added). `.gitignore` already added.
- **Commit style:** end commit messages with `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`. Commit author `Robbie Trencheny <robbie@campus.edu>`.
- **Lint/format:** `gofmt`/`goimports` clean; `go vet ./...` clean before each commit.

---

## Shared Type Reference (defined across tasks — do not redefine)

These are the cross-package contracts. The defining task is noted; later tasks consume them.

```go
// package config (Task 2)
type Config struct {
    Google  GoogleConfig  `yaml:"google"`
    SnipeIT SnipeITConfig `yaml:"snipe_it"`
    Sync    SyncConfig    `yaml:"sync"`
}
type GoogleConfig struct {
    CredentialsFile    string `yaml:"credentials_file"`
    ImpersonateSubject string `yaml:"impersonate_subject"`
    CustomerID         string `yaml:"customer_id"`
    Projection         string `yaml:"projection"`   // "full" | "basic"
    OrgUnitPath        string `yaml:"org_unit_path"`
    Query              string `yaml:"query"`
}
type SnipeITConfig struct {
    URL                   string         `yaml:"url"`
    APIKey                string         `yaml:"api_key"`
    DefaultStatusID       int            `yaml:"default_status_id"`
    DefaultCategoryID     int            `yaml:"default_category_id"`
    DefaultManufacturerID int            `yaml:"default_manufacturer_id"`
    CustomFieldsetID      int            `yaml:"custom_fieldset_id"`
    StatusMap             map[string]int `yaml:"status_map"`       // ChromeOS status -> status label id
    ManufacturerIDs       map[string]int `yaml:"manufacturer_ids"` // vendor (lowercased) -> id
}
type SyncConfig struct {
    DryRun       bool                         `yaml:"dry_run"`
    Force        bool                         `yaml:"force"`
    RateLimit    bool                         `yaml:"rate_limit"`
    UpdateOnly   bool                         `yaml:"update_only"`
    UseCache     bool                         `yaml:"use_cache"`
    CacheDir     string                       `yaml:"cache_dir"`
    SetName      bool                         `yaml:"set_name"`
    NameTemplate string                       `yaml:"name_template"`
    AssetTag     AssetTagConfig               `yaml:"asset_tag"`
    FieldMapping map[string]FieldMappingEntry `yaml:"field_mapping"`
    Checkout     CheckoutConfig               `yaml:"checkout"`
}
type AssetTagConfig struct {
    Template string `yaml:"template"`
}
type FieldMappingEntry struct {
    Path      string `yaml:"path"`
    Transform string `yaml:"transform"`
}   // custom UnmarshalYAML: a bare scalar string sets Path only
type CheckoutConfig struct {
    Enabled          bool   `yaml:"enabled"`
    UseAnnotatedUser bool   `yaml:"use_annotated_user"`
    FallbackToRecent bool   `yaml:"fallback_to_recent"`
    RecentUserDomain string `yaml:"recent_user_domain"`
    MatchField       string `yaml:"match_field"` // "email"|"username"|"employee_num"
    Mode             string `yaml:"mode"`        // "assign"|"sync"|"force"
}

// package google (Task 3)
type Device struct {
    *admin.ChromeOsDevice            // embedded; admin = google.golang.org/api/admin/directory/v1
    Raw json.RawMessage             // json.Marshal of the device, for gjson
}

// package snipe (Task 5)
type Asset struct {
    ID           int
    AssetTag     string
    Serial       string
    Name         string
    ModelID      int
    StatusID     int
    AssignedToID int
    CustomFields map[string]string  // db_column_name -> value
    UpdatedAt    time.Time
}
type Model struct {
    ID, ManufacturerID, CategoryID, FieldsetID int
    Name, ModelNumber                          string
}
type Manufacturer struct { ID int; Name string }
type User struct { ID int; Username, Email, EmployeeNum string }
type FieldDef struct {
    Name, Element, Format string
    Values                []string
}

// package sync (Task 7) — engine depends on this interface, not the concrete client
type SnipeClient interface {
    GetAssetBySerial(serial string) ([]snipe.Asset, error)
    CreateAsset(a snipe.Asset) (snipe.Asset, error)
    PatchAsset(id int, a snipe.Asset) (snipe.Asset, error)
    CheckoutAssetToUser(assetID, userID int) error
    CheckinAsset(assetID int) error
    ListAllModels() ([]snipe.Model, error)
    CreateModel(m snipe.Model) (snipe.Model, error)
    ListAllManufacturers() ([]snipe.Manufacturer, error)
    CreateManufacturer(name string) (snipe.Manufacturer, error)
    ListAllUsers() ([]snipe.User, error)
}
```

---

## Task 1: Project bootstrap (module, main, root command)

**Files:**
- Create: `go.mod`
- Create: `main.go`
- Create: `cmd/root.go`
- Create: `cmd/logging.go`

**Interfaces:**
- Produces: `cmd.Execute() error`; `cmd.rootCmd *cobra.Command`; package-level `logrus` loggers `cmd.log` and exported setters `cmd.SetLogLevel`, `cmd.SetLogFormat`, `cmd.SetLogOutput`. The config file path persistent flag value `cmd.cfgFile`.

- [ ] **Step 1: Initialize the module**

Run:
```bash
cd /Users/robbiet480/go/src/github.com/CampusTech/google2snipe
go mod init github.com/CampusTech/google2snipe
go get github.com/spf13/cobra@latest
go get github.com/sirupsen/logrus@latest
```
Expected: `go.mod` created with module path. Edit the `go` line to `go 1.26.4` (and remove any `toolchain` line `go mod init` may have added, or set it to `toolchain go1.26.4`). Then `cobra` + `logrus` added.

- [ ] **Step 2: Write `main.go`**

```go
package main

import "github.com/CampusTech/google2snipe/cmd"

// version is injected at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	cmd.Version = version
	cmd.Execute()
}
```

- [ ] **Step 3: Write `cmd/logging.go`** (per-package logger registry, ported pattern from `$FLEET/cmd/root.go`)

```go
package cmd

import (
	"io"

	"github.com/sirupsen/logrus"
)

// loggers registered by each package via RegisterLogger so global flags
// (--verbose/--debug/--log-format/--log-file) propagate everywhere.
var loggers []*logrus.Logger

// RegisterLogger records a logger for global level/format/output propagation.
func RegisterLogger(l *logrus.Logger) { loggers = append(loggers, l) }

func SetLogLevel(level logrus.Level) {
	for _, l := range loggers {
		l.SetLevel(level)
	}
}

func SetLogFormat(f logrus.Formatter) {
	for _, l := range loggers {
		l.SetFormatter(f)
	}
}

func SetLogOutput(w io.Writer) {
	for _, l := range loggers {
		l.SetOutput(w)
	}
}
```

- [ ] **Step 4: Write `cmd/root.go`**

```go
package cmd

import (
	"fmt"
	"io"
	"os"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

// Version is set from main.
var Version = "dev"

var (
	cfgFile   string
	verbose   bool
	debug     bool
	logFile   string
	logFormat string

	log = logrus.New()
)

var rootCmd = &cobra.Command{
	Use:           "google2snipe",
	Short:         "Sync ChromeOS devices from the Google Admin SDK into Snipe-IT",
	SilenceUsage:  true,
	SilenceErrors: true,
	Version:       Version,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		level := logrus.WarnLevel
		if verbose {
			level = logrus.InfoLevel
		}
		if debug {
			level = logrus.DebugLevel
		}
		SetLogLevel(level)

		switch logFormat {
		case "json":
			SetLogFormat(&logrus.JSONFormatter{})
		default:
			SetLogFormat(&logrus.TextFormatter{FullTimestamp: true})
		}

		var out io.Writer = os.Stderr
		if logFile != "" {
			f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				return fmt.Errorf("open log file: %w", err)
			}
			out = io.MultiWriter(os.Stderr, f)
		}
		SetLogOutput(out)
		return nil
	},
}

func init() {
	RegisterLogger(log)
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "settings.yaml", "config file path")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "info-level logging")
	rootCmd.PersistentFlags().BoolVarP(&debug, "debug", "d", false, "debug-level logging")
	rootCmd.PersistentFlags().StringVar(&logFile, "log-file", "", "also append logs to this file")
	rootCmd.PersistentFlags().StringVar(&logFormat, "log-format", "text", "log format: text|json")
}

// Execute runs the root command.
func Execute() {
	rootCmd.Version = Version
	if err := rootCmd.Execute(); err != nil {
		log.WithError(err).Error("command failed")
		os.Exit(1)
	}
}
```

- [ ] **Step 5: Verify it builds and runs**

Run:
```bash
go build ./... && go run . --help && go run . --version
```
Expected: build succeeds; `--help` prints usage with `--config/--verbose/--debug/--log-file/--log-format`; `--version` prints `google2snipe version dev`.

- [ ] **Step 6: Commit**

```bash
gofmt -w . && go vet ./...
git add go.mod go.sum main.go cmd/
git -c user.name="Robbie Trencheny" -c user.email="robbie@campus.edu" commit -m "feat: bootstrap module, root command, logging registry

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: Config package (structs, load, validate)

**Files:**
- Create: `config/config.go`
- Create: `config/config_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `config.Load(path string) (*Config, error)` (reads YAML, applies env overrides + defaults, validates); the structs in the Shared Type Reference; `config.KnownTransforms map[string]bool`; `config.FullOnlyPaths map[string]bool`; method `(*Config) Validate() error`.

- [ ] **Step 1: Write the failing test** `config/config_test.go`

```go
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
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./config/ -run TestLoad -v`
Expected: FAIL — `config.Load` undefined.

- [ ] **Step 3: Write `config/config.go`**

```go
package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the top-level YAML config. See Shared Type Reference in the plan.
type Config struct {
	Google  GoogleConfig  `yaml:"google"`
	SnipeIT SnipeITConfig `yaml:"snipe_it"`
	Sync    SyncConfig    `yaml:"sync"`
}

type GoogleConfig struct {
	CredentialsFile    string `yaml:"credentials_file"`
	ImpersonateSubject string `yaml:"impersonate_subject"`
	CustomerID         string `yaml:"customer_id"`
	Projection         string `yaml:"projection"`
	OrgUnitPath        string `yaml:"org_unit_path"`
	Query              string `yaml:"query"`
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
	DryRun       bool                         `yaml:"dry_run"`
	Force        bool                         `yaml:"force"`
	RateLimit    bool                         `yaml:"rate_limit"`
	UpdateOnly   bool                         `yaml:"update_only"`
	UseCache     bool                         `yaml:"use_cache"`
	CacheDir     string                       `yaml:"cache_dir"`
	SetName      bool                         `yaml:"set_name"`
	NameTemplate string                       `yaml:"name_template"`
	AssetTag     AssetTagConfig               `yaml:"asset_tag"`
	FieldMapping map[string]FieldMappingEntry `yaml:"field_mapping"`
	Checkout     CheckoutConfig               `yaml:"checkout"`
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

// Load reads, applies env overrides + defaults, and validates a config file.
func Load(path string) (*Config, error) {
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
	if c.SnipeIT.URL == "" || c.SnipeIT.APIKey == "" {
		return fmt.Errorf("snipe_it.url and snipe_it.api_key are required")
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
```

Add at top of file the small logger used by Validate's warning:
```go
import "log"
```
(Replace the `import` block above to include `"log"` alongside the others. The warning uses the std `log` package to avoid a cmd import cycle.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./config/ -v`
Expected: PASS (all three tests).

- [ ] **Step 5: Commit**

```bash
gofmt -w . && go vet ./...
git add config/
git -c user.name="Robbie Trencheny" -c user.email="robbie@campus.edu" commit -m "feat(config): YAML load, env overrides, defaults, validation

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 3: Google client — types and device parsing

**Files:**
- Create: `google/types.go`
- Create: `google/types_test.go`

**Interfaces:**
- Consumes: `admin "google.golang.org/api/admin/directory/v1"`.
- Produces: `google.Device` (embeds `*admin.ChromeOsDevice`, adds `Raw json.RawMessage`); `google.wrapDevice(*admin.ChromeOsDevice) (Device, error)`; `google.SerializeDevices([]Device) ([]byte, error)`; `google.DeserializeDevices([]byte) ([]Device, error)`.

- [ ] **Step 1: Add the dependency**

Run:
```bash
go get google.golang.org/api/admin/directory/v1@latest
go get google.golang.org/api/option@latest
go get golang.org/x/oauth2/google@latest
```
Expected: modules added to go.mod.

- [ ] **Step 2: Write the failing test** `google/types_test.go`

```go
package google

import (
	"testing"

	"github.com/tidwall/gjson"
	admin "google.golang.org/api/admin/directory/v1"
)

func TestWrapDevicePopulatesRawForGjson(t *testing.T) {
	d := &admin.ChromeOsDevice{
		SerialNumber: "ABC123",
		Status:       "ACTIVE",
		OrgUnitPath:  "/Students/Grade5",
		RecentUsers: []*admin.ChromeOsDeviceRecentUsers{
			{Type: "USER_TYPE_MANAGED", Email: "kid@school.edu"},
		},
	}
	dev, err := wrapDevice(d)
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(dev.Raw, "serialNumber").String(); got != "ABC123" {
		t.Errorf("serialNumber via gjson = %q", got)
	}
	if got := gjson.GetBytes(dev.Raw, "recentUsers.0.email").String(); got != "kid@school.edu" {
		t.Errorf("recentUsers.0.email via gjson = %q", got)
	}
	if got := gjson.GetBytes(dev.Raw, "orgUnitPath").String(); got != "/Students/Grade5" {
		t.Errorf("orgUnitPath via gjson = %q", got)
	}
}

func TestSerializeRoundTripRestoresRaw(t *testing.T) {
	in := []Device{}
	d, _ := wrapDevice(&admin.ChromeOsDevice{SerialNumber: "S1", Model: "Acer Chromebook 311"})
	in = append(in, d)
	data, err := SerializeDevices(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := DeserializeDevices(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].SerialNumber != "S1" {
		t.Fatalf("round trip lost device: %+v", out)
	}
	if got := gjson.GetBytes(out[0].Raw, "model").String(); got != "Acer Chromebook 311" {
		t.Errorf("raw not restored after deserialize: %q", got)
	}
}
```

- [ ] **Step 3: Run it to verify it fails**

Run: `go test ./google/ -run TestWrap -v`
Expected: FAIL — `wrapDevice` undefined.

- [ ] **Step 4: Write `google/types.go`**

```go
package google

import (
	"encoding/json"

	admin "google.golang.org/api/admin/directory/v1"
)

// Device wraps an admin.ChromeOsDevice and retains its JSON form so the sync
// engine can address any field (including deeply nested arrays) via gjson.
type Device struct {
	*admin.ChromeOsDevice
	Raw json.RawMessage `json:"-"`
}

// wrapDevice marshals the SDK struct to JSON and stores it as Raw.
func wrapDevice(d *admin.ChromeOsDevice) (Device, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return Device{}, err
	}
	return Device{ChromeOsDevice: d, Raw: raw}, nil
}

// SerializeDevices writes devices (with their underlying SDK struct) to JSON for caching.
func SerializeDevices(devs []Device) ([]byte, error) {
	bare := make([]*admin.ChromeOsDevice, len(devs))
	for i, d := range devs {
		bare[i] = d.ChromeOsDevice
	}
	return json.MarshalIndent(bare, "", "  ")
}

// DeserializeDevices reads cached JSON back into Devices, restoring Raw.
func DeserializeDevices(data []byte) ([]Device, error) {
	var bare []*admin.ChromeOsDevice
	if err := json.Unmarshal(data, &bare); err != nil {
		return nil, err
	}
	out := make([]Device, 0, len(bare))
	for _, d := range bare {
		dev, err := wrapDevice(d)
		if err != nil {
			return nil, err
		}
		out = append(out, dev)
	}
	return out, nil
}
```

Run `go get github.com/tidwall/gjson@latest` if not yet present (needed by the test).

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./google/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
gofmt -w . && go vet ./...
git add google/ go.mod go.sum
git -c user.name="Robbie Trencheny" -c user.email="robbie@campus.edu" commit -m "feat(google): Device type with raw JSON + cache serialization

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 4: Google client — auth, list, get, paging

**Files:**
- Create: `google/client.go`
- Create: `google/client_test.go`

**Interfaces:**
- Consumes: `config.GoogleConfig`; `google.Device`, `google.wrapDevice`.
- Produces: `google.New(cfg config.GoogleConfig, logger *logrus.Logger) (*Client, error)`; `(*Client) ListAllChromeOSDevices(ctx) ([]Device, error)`; `(*Client) GetDevice(ctx, deviceID string) (Device, error)`; `(*Client) About(ctx) (string, error)` (returns customer ID / smoke check).

- [ ] **Step 1: Write the failing test** `google/client_test.go` (uses httptest to serve the Directory API list response — no real auth)

```go
package google

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sirupsen/logrus"
	admin "google.golang.org/api/admin/directory/v1"
	"google.golang.org/api/option"
)

// newTestClient builds a Client whose admin.Service points at a fake server.
func newTestClient(t *testing.T, srvURL string) *Client {
	t.Helper()
	svc, err := admin.NewService(context.Background(),
		option.WithoutAuthentication(),
		option.WithEndpoint(srvURL),
	)
	if err != nil {
		t.Fatal(err)
	}
	return &Client{svc: svc, customerID: "my_customer", projection: "FULL", log: logrus.New()}
}

func TestListAllChromeOSDevicesPaginates(t *testing.T) {
	page1 := `{"chromeosdevices":[{"deviceId":"d1","serialNumber":"S1"}],"nextPageToken":"tok"}`
	page2 := `{"chromeosdevices":[{"deviceId":"d2","serialNumber":"S2"}]}`
	mux := http.NewServeMux()
	mux.HandleFunc("/admin/directory/v1/customer/my_customer/devices/chromeos", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("pageToken") == "tok" {
			_, _ = w.Write([]byte(page2))
			return
		}
		_, _ = w.Write([]byte(page1))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv.URL+"/")
	devs, err := c.ListAllChromeOSDevices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 2 || devs[0].SerialNumber != "S1" || devs[1].SerialNumber != "S2" {
		t.Fatalf("paging failed: got %d devices %+v", len(devs), devs)
	}
	if string(devs[0].Raw) == "" {
		t.Error("Raw not populated")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./google/ -run TestListAll -v`
Expected: FAIL — `Client` / `ListAllChromeOSDevices` undefined.

- [ ] **Step 3: Write `google/client.go`**

```go
package google

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/sirupsen/logrus"
	"golang.org/x/oauth2/google"
	admin "google.golang.org/api/admin/directory/v1"
	"google.golang.org/api/option"

	cfgpkg "github.com/CampusTech/google2snipe/config"
)

// Client wraps the Admin SDK Directory service for ChromeOS devices.
type Client struct {
	svc        *admin.Service
	customerID string
	projection string // "FULL" | "BASIC"
	orgUnit    string
	query      string
	log        *logrus.Logger
}

// New builds an authenticated Client using a service-account key with
// domain-wide delegation impersonating cfg.ImpersonateSubject.
func New(cfg cfgpkg.GoogleConfig, logger *logrus.Logger) (*Client, error) {
	if logger == nil {
		logger = logrus.New()
	}
	keyData, err := os.ReadFile(cfg.CredentialsFile)
	if err != nil {
		return nil, fmt.Errorf("read credentials_file: %w", err)
	}
	jwtCfg, err := google.JWTConfigFromJSON(keyData, admin.AdminDirectoryDeviceChromeosReadonlyScope)
	if err != nil {
		return nil, fmt.Errorf("parse service account key: %w", err)
	}
	jwtCfg.Subject = cfg.ImpersonateSubject

	ctx := context.Background()
	svc, err := admin.NewService(ctx, option.WithTokenSource(jwtCfg.TokenSource(ctx)))
	if err != nil {
		return nil, fmt.Errorf("create directory service: %w", err)
	}
	return &Client{
		svc:        svc,
		customerID: cfg.CustomerID,
		projection: strings.ToUpper(cfg.Projection),
		orgUnit:    cfg.OrgUnitPath,
		query:      cfg.Query,
		log:        logger,
	}, nil
}

// ListAllChromeOSDevices pages through every ChromeOS device for the customer.
func (c *Client) ListAllChromeOSDevices(ctx context.Context) ([]Device, error) {
	var out []Device
	pageToken := ""
	for {
		call := c.svc.Chromeosdevices.List(c.customerID).
			Projection(c.projection).
			MaxResults(200).
			Context(ctx)
		if c.orgUnit != "" {
			call = call.OrgUnitPath(c.orgUnit)
		}
		if c.query != "" {
			call = call.Query(c.query)
		}
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		resp, err := call.Do()
		if err != nil {
			return nil, fmt.Errorf("list chromeos devices: %w", err)
		}
		for _, d := range resp.Chromeosdevices {
			dev, err := wrapDevice(d)
			if err != nil {
				return nil, err
			}
			out = append(out, dev)
		}
		c.log.WithField("count", len(out)).Debug("listed chromeos devices page")
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}
	return out, nil
}

// GetDevice fetches a single ChromeOS device by its Google deviceId.
func (c *Client) GetDevice(ctx context.Context, deviceID string) (Device, error) {
	d, err := c.svc.Chromeosdevices.Get(c.customerID, deviceID).
		Projection(c.projection).Context(ctx).Do()
	if err != nil {
		return Device{}, fmt.Errorf("get chromeos device %s: %w", deviceID, err)
	}
	return wrapDevice(d)
}

// About is a lightweight connectivity check: lists a single device page.
func (c *Client) About(ctx context.Context) (string, error) {
	_, err := c.svc.Chromeosdevices.List(c.customerID).
		Projection("BASIC").MaxResults(1).Context(ctx).Do()
	if err != nil {
		return "", err
	}
	return c.customerID, nil
}
```

Note for the implementer: the test constructs `Client` with unexported fields directly (`svc`, `customerID`, `projection`, `log`) — keep those field names exactly as written so the test compiles.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./google/ -v`
Expected: PASS (paging test + Task 3 tests).

- [ ] **Step 5: Register the logger and commit**

Add to `google/client.go` a package logger so cmd can propagate levels: not needed (logger is injected by cmd). Commit:
```bash
gofmt -w . && go vet ./...
git add google/ go.mod go.sum
git -c user.name="Robbie Trencheny" -c user.email="robbie@campus.edu" commit -m "feat(google): authenticated client with list/get/paging

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 5: Snipe-IT wrapper (port from fleet2snipe)

**Files:**
- Create: `snipe/client.go`
- Test: `snipe/client_test.go` (dry-run enforcement only — no live API)

**Interfaces:**
- Consumes: `github.com/michellepellon/go-snipeit`.
- Produces: the `snipe` types in the Shared Type Reference and a `*snipe.Client` implementing the `sync.SnipeClient` interface, plus `SetupFields(fieldsetIDs []int, fields []FieldDef) (map[string]string, error)` and `Ping() (string, error)`. Constructor `snipe.New(url, apiKey string, dryRun, rateLimit bool, logger *logrus.Logger) (*Client, error)`.

- [ ] **Step 1: Add dependency and read the reference**

Run:
```bash
go get github.com/michellepellon/go-snipeit@latest
```
Read `$FLEET/snipe/client.go` in full. It is data-source-agnostic.

- [ ] **Step 2: Port the file**

Copy `$FLEET/snipe/client.go` into `snipe/client.go`. Apply exactly these adaptations:
1. Keep the package name `snipe`.
2. Ensure the exported surface matches the Shared Type Reference: types `Asset`, `Model`, `Manufacturer`, `User`, `FieldDef`; methods `GetAssetBySerial`, `CreateAsset`, `PatchAsset`, `CheckoutAssetToUser`, `CheckinAsset`, `ListAllModels`, `CreateModel`, `ListAllManufacturers`, `CreateManufacturer`, `ListAllUsers`, `SetupFields`, `Ping`. If fleet2snipe's method names differ, add thin wrapper methods with these exact names/signatures (the engine and setup command depend on them).
3. Constructor signature must be `New(url, apiKey string, dryRun, rateLimit bool, logger *logrus.Logger) (*Client, error)`.
4. `Asset.CustomFields` is `map[string]string` keyed by `db_column_name`.
5. Preserve dry-run enforcement (`ErrDryRun`), token-bucket rate limiting, and the custom-field-rejection strip-and-retry in `PatchAsset`.
6. Register no global logger; the logger is injected via `New`.

If any go-snipeit symbol referenced by fleet2snipe is unavailable in the version resolved here, pin the same version fleet2snipe uses (check `$FLEET/go.mod`) via `go get github.com/michellepellon/go-snipeit@<that-version>`.

- [ ] **Step 3: Write the dry-run test** `snipe/client_test.go`

```go
package snipe

import (
	"errors"
	"testing"

	"github.com/sirupsen/logrus"
)

func TestDryRunBlocksCreate(t *testing.T) {
	c, err := New("https://snipe.invalid", "key", true /*dryRun*/, false, logrus.New())
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.CreateAsset(Asset{Serial: "X1", ModelID: 1, StatusID: 1})
	if !errors.Is(err, ErrDryRun) {
		t.Fatalf("CreateAsset in dry-run = %v, want ErrDryRun", err)
	}
}
```

- [ ] **Step 4: Run it**

Run: `go test ./snipe/ -run TestDryRun -v`
Expected: PASS (no network call because dry-run short-circuits before the HTTP request). If the port performs setup that hits the network in `New`, adjust `New` to defer client construction or accept the invalid URL without dialing — `New` must not make network calls.

- [ ] **Step 5: Commit**

```bash
gofmt -w . && go vet ./...
git add snipe/ go.mod go.sum
git -c user.name="Robbie Trencheny" -c user.email="robbie@campus.edu" commit -m "feat(snipe): port go-snipeit wrapper from fleet2snipe

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 6: Transforms

**Files:**
- Create: `sync/transforms.go`
- Create: `sync/transforms_test.go`

**Interfaces:**
- Consumes: `github.com/tidwall/gjson`.
- Produces: `sync.transformValue(r gjson.Result, transform string) string`; `sync.stringifyGJSON(r gjson.Result) string`.

- [ ] **Step 1: Write the failing test** `sync/transforms_test.go`

```go
package sync

import (
	"testing"

	"github.com/tidwall/gjson"
)

func res(json, path string) gjson.Result { return gjson.Get(json, path) }

func TestTransforms(t *testing.T) {
	cases := []struct {
		name, json, path, transform, want string
	}{
		{"plain string", `{"a":"hi"}`, "a", "", "hi"},
		{"int as string field", `{"ram":"8589934592"}`, "ram", "bytes_to_gb", "8.59"},
		{"int number", `{"ram":8589934592}`, "ram", "bytes_to_gb", "8.59"},
		{"zero bytes empty", `{"ram":0}`, "ram", "bytes_to_gb", ""},
		{"missing empty", `{}`, "nope", "bytes_to_gb", ""},
		{"mac colons", `{"m":"a4bb6d123456"}`, "m", "mac_colons", "a4:bb:6d:12:34:56"},
		{"mac already sep", `{"m":"A4-BB-6D-12-34-56"}`, "m", "mac_colons", "a4:bb:6d:12:34:56"},
		{"mac bad length", `{"m":"xyz"}`, "m", "mac_colons", ""},
		{"bool yes", `{"b":true}`, "b", "bool_yes_no", "Yes"},
		{"bool no", `{"b":false}`, "b", "bool_yes_no", "No"},
		{"upper", `{"s":"flex"}`, "s", "uppercase", "FLEX"},
		{"array joined", `{"u":[{"email":"a@x"},{"email":"b@y"}]}`, "u.#.email", "", "a@x, b@y"},
		{"number int form", `{"n":5}`, "n", "", "5"},
		{"date_only from rfc3339 millis", `{"t":"2024-05-01T12:00:00.000Z"}`, "t", "date_only", "2024-05-01"},
		{"date_only from bare date", `{"t":"2020-02-19"}`, "t", "date_only", "2020-02-19"},
		{"datetime from rfc3339 millis", `{"t":"2024-05-01T12:00:00.000Z"}`, "t", "datetime", "2024-05-01 12:00:00"},
		{"date unparseable empty", `{"t":"not a date"}`, "t", "date_only", ""},
		{"date missing empty", `{}`, "nope", "date_only", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := transformValue(res(c.json, c.path), c.transform)
			if got != c.want {
				t.Errorf("transformValue(%s,%q) = %q, want %q", c.path, c.transform, got, c.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./sync/ -run TestTransforms -v`
Expected: FAIL — `transformValue` undefined.

- [ ] **Step 3: Write `sync/transforms.go`**

```go
package sync

import (
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// transformValue renders a gjson result to a string per the named transform.
// Unit/date transforms return "" for missing/zero/unparseable input so the
// engine never writes a meaningless value.
func transformValue(r gjson.Result, transform string) string {
	switch transform {
	case "":
		return stringifyGJSON(r)
	case "bytes_to_gb":
		return bytesTo(r, 1e9)
	case "bytes_to_gib":
		return bytesTo(r, 1<<30)
	case "bytes_to_mb":
		return bytesTo(r, 1e6)
	case "bytes_to_tb":
		return bytesTo(r, 1e12)
	case "mac_colons":
		return normalizeMAC(r.String(), ":")
	case "mac_dashes":
		return normalizeMAC(r.String(), "-")
	case "bool_yes_no":
		return boolYesNo(r)
	case "uppercase":
		s := stringifyGJSON(r)
		if s == "" {
			return ""
		}
		return strings.ToUpper(s)
	case "lowercase":
		s := stringifyGJSON(r)
		if s == "" {
			return ""
		}
		return strings.ToLower(s)
	case "comma_thousands":
		return commaThousands(r)
	case "unix_to_iso":
		return unixToISO(r)
	case "date_only":
		return formatDate(r, "2006-01-02")
	case "datetime":
		return formatDate(r, "2006-01-02 15:04:05")
	default:
		return stringifyGJSON(r)
	}
}

func numeric(r gjson.Result) (float64, bool) {
	switch r.Type {
	case gjson.Number:
		return r.Num, true
	case gjson.String:
		f, err := strconv.ParseFloat(strings.TrimSpace(r.String()), 64)
		if err != nil {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}

func bytesTo(r gjson.Result, div float64) string {
	n, ok := numeric(r)
	if !ok || n == 0 {
		return ""
	}
	v := n / div
	rounded := math.Round(v*100) / 100
	return strconv.FormatFloat(rounded, 'f', -1, 64)
}

func normalizeMAC(s, sep string) string {
	var hex []rune
	for _, c := range strings.ToLower(s) {
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			hex = append(hex, c)
		}
	}
	if len(hex) != 12 {
		return ""
	}
	parts := make([]string, 0, 6)
	for i := 0; i < 12; i += 2 {
		parts = append(parts, string(hex[i:i+2]))
	}
	return strings.Join(parts, sep)
}

func boolYesNo(r gjson.Result) string {
	switch r.Type {
	case gjson.True:
		return "Yes"
	case gjson.False:
		return "No"
	case gjson.Number:
		if r.Num != 0 {
			return "Yes"
		}
		return "No"
	case gjson.String:
		switch strings.ToLower(strings.TrimSpace(r.String())) {
		case "true", "yes", "1":
			return "Yes"
		case "false", "no", "0":
			return "No"
		}
	}
	return ""
}

func commaThousands(r gjson.Result) string {
	n, ok := numeric(r)
	if !ok {
		return ""
	}
	neg := n < 0
	i := int64(math.Abs(n))
	s := strconv.FormatInt(i, 10)
	var b strings.Builder
	for idx, ch := range s {
		if idx > 0 && (len(s)-idx)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(ch)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func unixToISO(r gjson.Result) string {
	var sec int64
	switch r.Type {
	case gjson.Number:
		sec = int64(r.Num)
	case gjson.String:
		n, err := strconv.ParseInt(strings.TrimSpace(r.String()), 10, 64)
		if err != nil {
			return ""
		}
		sec = n
	default:
		return ""
	}
	if sec == 0 {
		return ""
	}
	return time.Unix(sec, 0).UTC().Format("2006-01-02 15:04:05")
}

var dateLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05Z0700",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

// formatDate parses a flexible date/timestamp string (RFC3339 with or without
// fractional seconds, "YYYY-MM-DD HH:MM:SS", or a bare "YYYY-MM-DD") and
// reformats it to layout in UTC. Returns "" for empty/unparseable input.
func formatDate(r gjson.Result, layout string) string {
	s := strings.TrimSpace(r.String())
	if s == "" {
		return ""
	}
	for _, l := range dateLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC().Format(layout)
		}
	}
	return ""
}

// stringifyGJSON renders a gjson result to a string; arrays become a
// comma-separated list of their non-empty elements.
func stringifyGJSON(r gjson.Result) string {
	if !r.Exists() {
		return ""
	}
	switch r.Type {
	case gjson.Null:
		return ""
	case gjson.True:
		return "true"
	case gjson.False:
		return "false"
	case gjson.Number:
		if r.Num == math.Trunc(r.Num) {
			return strconv.FormatInt(int64(r.Num), 10)
		}
		return strconv.FormatFloat(r.Num, 'f', -1, 64)
	case gjson.String:
		return r.String()
	case gjson.JSON:
		if r.IsArray() {
			var parts []string
			r.ForEach(func(_, v gjson.Result) bool {
				if s := stringifyGJSON(v); s != "" {
					parts = append(parts, s)
				}
				return true
			})
			return strings.Join(parts, ", ")
		}
		return r.String()
	}
	return ""
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./sync/ -run TestTransforms -v`
Expected: PASS (all sub-cases). Note: `8589934592 / 1e9 = 8.589934592` → rounds to `8.59`.

- [ ] **Step 5: Commit**

```bash
gofmt -w . && go vet ./...
git add sync/transforms.go sync/transforms_test.go
git -c user.name="Robbie Trencheny" -c user.email="robbie@campus.edu" commit -m "feat(sync): value transforms and gjson stringification

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 7: Engine — field mapping, status, asset tag

**Files:**
- Create: `sync/engine.go`
- Create: `sync/engine_test.go`

**Interfaces:**
- Consumes: `config.Config`, `google.Device`, `snipe.*`, `transformValue`.
- Produces: `sync.SnipeClient` interface (Shared Type Reference); `sync.Engine` struct; `sync.New(cfg *config.Config, sc SnipeClient, logger *logrus.Logger) *Engine`; methods `(*Engine) applyMapping(google.Device) map[string]string`, `(*Engine) statusID(google.Device) int`, `(*Engine) assetTag(google.Device) string`. `Engine` fields used by later tasks: `cfg`, `snipe`, `log`, `stats Stats`, `models map[string]snipe.Model`, `manufacturers map[string]snipe.Manufacturer`, `userIndex map[string]int`. `Stats{Total,Created,Updated,Skipped,Errors int}`.

- [ ] **Step 1: Write the failing test** `sync/engine_test.go`

```go
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
	d := dev(t, &admin.ChromeOsDevice{SerialNumber: "S1", SystemRamTotal: "8000000000"})
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
```

Add a shared test helper file `sync/stub_test.go`:

```go
package sync

import (
	"encoding/json"
	"testing"

	admin "google.golang.org/api/admin/directory/v1"

	"github.com/CampusTech/google2snipe/snipe"
)

func mustJSON(t *testing.T, d *admin.ChromeOsDevice) []byte {
	t.Helper()
	b, err := json.Marshal([]*admin.ChromeOsDevice{d})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// stubSnipe is an in-memory SnipeClient for engine tests.
type stubSnipe struct {
	bySerial    map[string][]snipe.Asset
	created     []snipe.Asset
	patched     map[int]snipe.Asset
	checkouts   map[int]int // assetID -> userID
	models      []snipe.Model
	manufs      []snipe.Manufacturer
	users       []snipe.User
	nextID      int
}

func (s *stubSnipe) GetAssetBySerial(serial string) ([]snipe.Asset, error) {
	return s.bySerial[serial], nil
}
func (s *stubSnipe) CreateAsset(a snipe.Asset) (snipe.Asset, error) {
	s.nextID++
	a.ID = s.nextID
	s.created = append(s.created, a)
	return a, nil
}
func (s *stubSnipe) PatchAsset(id int, a snipe.Asset) (snipe.Asset, error) {
	if s.patched == nil {
		s.patched = map[int]snipe.Asset{}
	}
	a.ID = id
	s.patched[id] = a
	return a, nil
}
func (s *stubSnipe) CheckoutAssetToUser(assetID, userID int) error {
	if s.checkouts == nil {
		s.checkouts = map[int]int{}
	}
	s.checkouts[assetID] = userID
	return nil
}
func (s *stubSnipe) CheckinAsset(assetID int) error               { return nil }
func (s *stubSnipe) ListAllModels() ([]snipe.Model, error)        { return s.models, nil }
func (s *stubSnipe) CreateModel(m snipe.Model) (snipe.Model, error) {
	s.nextID++
	m.ID = s.nextID
	s.models = append(s.models, m)
	return m, nil
}
func (s *stubSnipe) ListAllManufacturers() ([]snipe.Manufacturer, error) { return s.manufs, nil }
func (s *stubSnipe) CreateManufacturer(name string) (snipe.Manufacturer, error) {
	s.nextID++
	m := snipe.Manufacturer{ID: s.nextID, Name: name}
	s.manufs = append(s.manufs, m)
	return m, nil
}
func (s *stubSnipe) ListAllUsers() ([]snipe.User, error) { return s.users, nil }
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./sync/ -run 'TestApplyMapping|TestStatusID|TestAssetTag' -v`
Expected: FAIL — `Engine`/`New` undefined.

- [ ] **Step 3: Write `sync/engine.go`** (engine core; create/update added in Task 9)

```go
package sync

import (
	"regexp"
	"strings"

	"github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"

	"github.com/CampusTech/google2snipe/config"
	"github.com/CampusTech/google2snipe/google"
	"github.com/CampusTech/google2snipe/snipe"
)

// SnipeClient is the subset of the snipe wrapper the engine depends on.
type SnipeClient interface {
	GetAssetBySerial(serial string) ([]snipe.Asset, error)
	CreateAsset(a snipe.Asset) (snipe.Asset, error)
	PatchAsset(id int, a snipe.Asset) (snipe.Asset, error)
	CheckoutAssetToUser(assetID, userID int) error
	CheckinAsset(assetID int) error
	ListAllModels() ([]snipe.Model, error)
	CreateModel(m snipe.Model) (snipe.Model, error)
	ListAllManufacturers() ([]snipe.Manufacturer, error)
	CreateManufacturer(name string) (snipe.Manufacturer, error)
	ListAllUsers() ([]snipe.User, error)
}

// Stats accumulates per-run counters.
type Stats struct{ Total, Created, Updated, Skipped, Errors int }

// Engine reconciles ChromeOS devices into Snipe-IT.
type Engine struct {
	cfg   *config.Config
	snipe SnipeClient
	log   *logrus.Logger

	models        map[string]snipe.Model        // keyed by model name
	manufacturers map[string]snipe.Manufacturer // keyed by lowercased name
	userIndex     map[string]int                // keyed by lowercased match-field value
	stats         Stats
}

// New builds an Engine.
func New(cfg *config.Config, sc SnipeClient, logger *logrus.Logger) *Engine {
	if logger == nil {
		logger = logrus.New()
	}
	return &Engine{
		cfg:           cfg,
		snipe:         sc,
		log:           logger,
		models:        map[string]snipe.Model{},
		manufacturers: map[string]snipe.Manufacturer{},
		userIndex:     map[string]int{},
	}
}

// applyMapping resolves configured field_mapping entries against the device JSON.
func (e *Engine) applyMapping(dev google.Device) map[string]string {
	out := map[string]string{}
	for col, entry := range e.cfg.Sync.FieldMapping {
		r := gjson.GetBytes(dev.Raw, entry.Path)
		if v := transformValue(r, entry.Transform); v != "" {
			out[col] = v
		}
	}
	return out
}

// statusID maps ChromeOS lifecycle status to a Snipe status label, falling
// back to the configured default.
func (e *Engine) statusID(dev google.Device) int {
	if id, ok := e.cfg.SnipeIT.StatusMap[dev.Status]; ok && id != 0 {
		return id
	}
	return e.cfg.SnipeIT.DefaultStatusID
}

var tagPlaceholder = regexp.MustCompile(`\{([^}]+)\}`)

// assetTag renders the configured template against the device; empty template
// or all-empty placeholders yield "" (Snipe auto-assigns).
func (e *Engine) assetTag(dev google.Device) string {
	tmpl := e.cfg.Sync.AssetTag.Template
	if tmpl == "" {
		return ""
	}
	out := tagPlaceholder.ReplaceAllStringFunc(tmpl, func(m string) string {
		path := m[1 : len(m)-1]
		return gjson.GetBytes(dev.Raw, path).String()
	})
	return strings.TrimSpace(out)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./sync/ -v`
Expected: PASS (transforms + mapping/status/tag). `8000000000/1e9 = 8` → `"8"`.

- [ ] **Step 5: Commit**

```bash
gofmt -w . && go vet ./...
git add sync/engine.go sync/engine_test.go sync/stub_test.go
git -c user.name="Robbie Trencheny" -c user.email="robbie@campus.edu" commit -m "feat(sync): engine core — field mapping, status, asset tag

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 8: Engine — model/manufacturer, user index, checkout resolution

**Files:**
- Modify: `sync/engine.go`
- Modify: `sync/engine_test.go`

**Interfaces:**
- Consumes: Task 7 `Engine`.
- Produces: `(*Engine) Warm() error`; `(*Engine) ensureModel(google.Device) (int, error)`; `(*Engine) ensureManufacturer(google.Device) (int, error)`; `(*Engine) resolveCheckoutUser(google.Device) (int, bool)`.

- [ ] **Step 1: Write the failing test** (append to `sync/engine_test.go`)

```go
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
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./sync/ -run 'TestResolveCheckout|TestEnsureManufacturer' -v`
Expected: FAIL — `Warm`/`resolveCheckoutUser`/`ensureManufacturer` undefined.

- [ ] **Step 3: Append to `sync/engine.go`**

```go
// Warm preloads models, manufacturers, and users into in-memory indexes.
func (e *Engine) Warm() error {
	models, err := e.snipe.ListAllModels()
	if err != nil {
		return err
	}
	for _, m := range models {
		e.models[m.Name] = m
	}
	manufs, err := e.snipe.ListAllManufacturers()
	if err != nil {
		return err
	}
	for _, m := range manufs {
		e.manufacturers[strings.ToLower(m.Name)] = m
	}
	users, err := e.snipe.ListAllUsers()
	if err != nil {
		return err
	}
	for _, u := range users {
		key := userKey(u, e.cfg.Sync.Checkout.MatchField)
		if key != "" {
			e.userIndex[strings.ToLower(key)] = u.ID
		}
	}
	e.log.WithFields(logrus.Fields{
		"models": len(e.models), "manufacturers": len(e.manufacturers), "users": len(e.userIndex),
	}).Info("warmed snipe-it caches")
	return nil
}

func userKey(u snipe.User, matchField string) string {
	switch matchField {
	case "username":
		return u.Username
	case "employee_num":
		return u.EmployeeNum
	default:
		return u.Email
	}
}

// ensureManufacturer resolves (or creates) a Snipe manufacturer from the
// device's model vendor (first token of the model string).
func (e *Engine) ensureManufacturer(dev google.Device) (int, error) {
	vendor := modelVendor(dev.Model)
	if vendor == "" {
		return e.cfg.SnipeIT.DefaultManufacturerID, nil
	}
	if id, ok := e.cfg.SnipeIT.ManufacturerIDs[strings.ToLower(vendor)]; ok && id != 0 {
		return id, nil
	}
	if m, ok := e.manufacturers[strings.ToLower(vendor)]; ok {
		return m.ID, nil
	}
	m, err := e.snipe.CreateManufacturer(vendor)
	if err != nil {
		return 0, err
	}
	e.manufacturers[strings.ToLower(vendor)] = m
	return m.ID, nil
}

func modelVendor(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	return strings.Fields(model)[0]
}

// ensureModel resolves (or creates) a Snipe model from the device model name.
func (e *Engine) ensureModel(dev google.Device) (int, error) {
	name := strings.TrimSpace(dev.Model)
	if name == "" {
		name = "Unknown ChromeOS Device"
	}
	if m, ok := e.models[name]; ok {
		return m.ID, nil
	}
	manufID, err := e.ensureManufacturer(dev)
	if err != nil {
		return 0, err
	}
	m, err := e.snipe.CreateModel(snipe.Model{
		Name:           name,
		ManufacturerID: manufID,
		CategoryID:     e.cfg.SnipeIT.DefaultCategoryID,
		FieldsetID:     e.cfg.SnipeIT.CustomFieldsetID,
	})
	if err != nil {
		return 0, err
	}
	e.models[name] = m
	return m.ID, nil
}

// resolveCheckoutUser picks the Snipe user ID to check the device out to,
// per the checkout config. Returns ok=false when checkout is disabled or no
// matching user is found.
func (e *Engine) resolveCheckoutUser(dev google.Device) (int, bool) {
	co := e.cfg.Sync.Checkout
	if !co.Enabled {
		return 0, false
	}
	var candidate string
	if co.UseAnnotatedUser && dev.AnnotatedUser != "" {
		candidate = dev.AnnotatedUser
	} else if co.FallbackToRecent {
		for _, ru := range dev.RecentUsers {
			if ru.Email == "" {
				continue
			}
			if ru.Type != "" && ru.Type != "USER_TYPE_MANAGED" {
				continue
			}
			if co.RecentUserDomain != "" &&
				!strings.HasSuffix(strings.ToLower(ru.Email), "@"+strings.ToLower(co.RecentUserDomain)) {
				continue
			}
			candidate = ru.Email
			break
		}
	}
	if candidate == "" {
		return 0, false
	}
	return e.lookupUser(candidate)
}

func (e *Engine) lookupUser(email string) (int, bool) {
	key := strings.ToLower(strings.TrimSpace(email))
	if id, ok := e.userIndex[key]; ok {
		return id, true
	}
	if i := strings.IndexByte(key, '@'); i > 0 {
		if id, ok := e.userIndex[key[:i]]; ok {
			return id, true
		}
	}
	return 0, false
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./sync/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w . && go vet ./...
git add sync/engine.go sync/engine_test.go
git -c user.name="Robbie Trencheny" -c user.email="robbie@campus.edu" commit -m "feat(sync): warm caches, model/manufacturer, checkout resolution

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 9: Engine — create, update, SyncDevice, SyncAll

**Files:**
- Modify: `sync/engine.go`
- Modify: `sync/engine_test.go`

**Interfaces:**
- Consumes: Tasks 7–8 `Engine`.
- Produces: `(*Engine) SyncDevice(dev google.Device)`; `(*Engine) SyncAll(devs []google.Device) Stats`; `(*Engine) StatsSnapshot() Stats`.

- [ ] **Step 1: Write the failing test** (append to `sync/engine_test.go`)

```go
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
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./sync/ -run TestSyncDevice -v`
Expected: FAIL — `SyncDevice` undefined.

- [ ] **Step 3: Append to `sync/engine.go`**

```go
import (
	"time"
)

// SyncAll reconciles every device and returns run statistics.
func (e *Engine) SyncAll(devs []google.Device) Stats {
	for i, d := range devs {
		e.SyncDevice(d)
		if (i+1)%50 == 0 {
			e.log.WithField("processed", i+1).Info("syncing")
		}
	}
	e.log.WithFields(logrus.Fields{
		"total": e.stats.Total, "created": e.stats.Created, "updated": e.stats.Updated,
		"skipped": e.stats.Skipped, "errors": e.stats.Errors,
	}).Info("sync complete")
	return e.stats
}

// StatsSnapshot returns a copy of the current counters.
func (e *Engine) StatsSnapshot() Stats { return e.stats }

// SyncDevice reconciles a single device into Snipe-IT.
func (e *Engine) SyncDevice(dev google.Device) {
	e.stats.Total++
	serial := strings.TrimSpace(dev.SerialNumber)
	if serial == "" {
		e.log.WithField("device_id", dev.DeviceId).Debug("skipping device with empty serial")
		e.stats.Skipped++
		return
	}
	l := e.log.WithField("serial", serial)

	existing, err := e.snipe.GetAssetBySerial(serial)
	if err != nil {
		l.WithError(err).Error("snipe lookup failed")
		e.stats.Errors++
		return
	}
	switch len(existing) {
	case 0:
		if e.cfg.Sync.UpdateOnly {
			l.Debug("update-only: skipping create")
			e.stats.Skipped++
			return
		}
		e.create(dev, l)
	case 1:
		e.update(dev, existing[0], l)
	default:
		l.WithField("matches", len(existing)).Warn("multiple assets share this serial; skipping")
		e.stats.Skipped++
	}
}

func (e *Engine) create(dev google.Device, l *logrus.Entry) {
	modelID, err := e.ensureModel(dev)
	if err != nil {
		l.WithError(err).Error("ensure model failed")
		e.stats.Errors++
		return
	}
	asset := snipe.Asset{
		Serial:       dev.SerialNumber,
		AssetTag:     e.assetTag(dev),
		ModelID:      modelID,
		StatusID:     e.statusID(dev),
		CustomFields: e.applyMapping(dev),
	}
	if e.cfg.Sync.SetName {
		asset.Name = e.renderName(dev)
	}
	if e.cfg.Sync.DryRun {
		l.Info("[DRY RUN] would create asset")
		e.stats.Created++
		return
	}
	created, err := e.snipe.CreateAsset(asset)
	if err != nil {
		l.WithError(err).Error("create asset failed")
		e.stats.Errors++
		return
	}
	l.WithField("snipe_id", created.ID).Info("created asset")
	e.applyCheckout(dev, created, l)
	e.stats.Created++
}

func (e *Engine) update(dev google.Device, existing snipe.Asset, l *logrus.Entry) {
	if !e.cfg.Sync.Force && deviceOlderThan(dev, existing.UpdatedAt) {
		l.Debug("snipe record newer than device; skipping field update")
		e.applyCheckout(dev, existing, l)
		e.stats.Skipped++
		return
	}
	modelID, err := e.ensureModel(dev)
	if err != nil {
		l.WithError(err).Error("ensure model failed")
		e.stats.Errors++
		return
	}
	patch := snipe.Asset{
		ModelID:      modelID,
		StatusID:     e.statusID(dev),
		CustomFields: e.applyMapping(dev),
	}
	if e.cfg.Sync.SetName {
		patch.Name = e.renderName(dev)
	}
	if e.cfg.Sync.DryRun {
		l.WithField("snipe_id", existing.ID).Info("[DRY RUN] would update asset")
		e.stats.Updated++
		return
	}
	if _, err := e.snipe.PatchAsset(existing.ID, patch); err != nil {
		l.WithError(err).Error("update asset failed")
		e.stats.Errors++
		return
	}
	l.WithField("snipe_id", existing.ID).Info("updated asset")
	e.applyCheckout(dev, existing, l)
	e.stats.Updated++
}

func (e *Engine) applyCheckout(dev google.Device, asset snipe.Asset, l *logrus.Entry) {
	userID, ok := e.resolveCheckoutUser(dev)
	if !ok {
		return
	}
	switch e.cfg.Sync.Checkout.Mode {
	case "assign":
		if asset.AssignedToID != 0 {
			return // already assigned; don't override
		}
	case "sync", "force":
		if asset.AssignedToID == userID {
			return // already correct
		}
	}
	if e.cfg.Sync.DryRun {
		l.WithField("user_id", userID).Info("[DRY RUN] would check out asset")
		return
	}
	if err := e.snipe.CheckoutAssetToUser(asset.ID, userID); err != nil {
		l.WithError(err).Warn("checkout failed")
		return
	}
	l.WithField("user_id", userID).Info("checked out asset")
}

func (e *Engine) renderName(dev google.Device) string {
	tmpl := e.cfg.Sync.NameTemplate
	if tmpl == "" {
		tmpl = "{annotatedAssetId}"
	}
	out := tagPlaceholder.ReplaceAllStringFunc(tmpl, func(m string) string {
		return gjson.GetBytes(dev.Raw, m[1:len(m)-1]).String()
	})
	out = strings.TrimSpace(out)
	if out == "" {
		out = dev.SerialNumber
	}
	return out
}

// deviceOlderThan reports whether the device's last sync/enrollment predates t.
func deviceOlderThan(dev google.Device, t time.Time) bool {
	if t.IsZero() {
		return false
	}
	ts := dev.LastSync
	if ts == "" {
		ts = dev.LastEnrollmentTime
	}
	if ts == "" {
		return false
	}
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return false
	}
	return parsed.Before(t)
}
```

Merge the `time` import into the existing import block at the top of `engine.go` (do not add a second `import` statement).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./sync/ -v`
Expected: PASS (all engine tests).

- [ ] **Step 5: Commit**

```bash
gofmt -w . && go vet ./...
git add sync/engine.go sync/engine_test.go
git -c user.name="Robbie Trencheny" -c user.email="robbie@campus.edu" commit -m "feat(sync): create/update/checkout reconciliation with freshness gate

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 10: `sync` command (wiring, flags, cache, single-device)

**Files:**
- Create: `cmd/sync.go`

**Interfaces:**
- Consumes: `config.Load`, `google.New`, `snipe.New`, `sync.New`.
- Produces: `sync` cobra subcommand registered on `rootCmd`.

- [ ] **Step 1: Write `cmd/sync.go`**

```go
package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/CampusTech/google2snipe/config"
	"github.com/CampusTech/google2snipe/google"
	"github.com/CampusTech/google2snipe/snipe"
	syncpkg "github.com/CampusTech/google2snipe/sync"
)

var (
	syncDryRun     bool
	syncForce      bool
	syncSerial     string
	syncDeviceID   string
	syncUpdateOnly bool
	syncUseCache   bool
	syncProjection string

	googleLog = logrus.New()
	snipeLog  = logrus.New()
	syncLog   = logrus.New()
)

var syncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Reconcile ChromeOS devices into Snipe-IT",
	RunE:  runSync,
}

func init() {
	RegisterLogger(googleLog)
	RegisterLogger(snipeLog)
	RegisterLogger(syncLog)
	syncCmd.Flags().BoolVar(&syncDryRun, "dry-run", false, "simulate without mutating Snipe-IT")
	syncCmd.Flags().BoolVar(&syncForce, "force", false, "ignore freshness checks")
	syncCmd.Flags().StringVar(&syncSerial, "serial", "", "sync only the device with this serial")
	syncCmd.Flags().StringVar(&syncDeviceID, "device-id", "", "sync only the device with this Google deviceId")
	syncCmd.Flags().BoolVar(&syncUpdateOnly, "update-only", false, "never create, only update")
	syncCmd.Flags().BoolVar(&syncUseCache, "use-cache", false, "read devices from local cache instead of the API")
	syncCmd.Flags().StringVar(&syncProjection, "projection", "", "override projection: full|basic")
	rootCmd.AddCommand(syncCmd)
}

func runSync(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return err
	}
	// flag overrides
	cfg.Sync.DryRun = cfg.Sync.DryRun || syncDryRun
	cfg.Sync.Force = cfg.Sync.Force || syncForce
	cfg.Sync.UpdateOnly = cfg.Sync.UpdateOnly || syncUpdateOnly
	cfg.Sync.UseCache = cfg.Sync.UseCache || syncUseCache
	if syncSerial != "" || syncDeviceID != "" {
		cfg.Sync.Force = true
	}
	if syncProjection != "" {
		cfg.Google.Projection = syncProjection
	}

	sc, err := snipe.New(cfg.SnipeIT.URL, cfg.SnipeIT.APIKey, cfg.Sync.DryRun, cfg.Sync.RateLimit, snipeLog)
	if err != nil {
		return err
	}
	engine := syncpkg.New(cfg, sc, syncLog)
	if err := engine.Warm(); err != nil {
		return fmt.Errorf("warm caches: %w", err)
	}

	devs, err := loadDevices(cmd.Context(), cfg)
	if err != nil {
		return err
	}
	if syncSerial != "" {
		devs = filterSerial(devs, syncSerial)
	}
	engine.SyncAll(devs)
	stats := engine.StatsSnapshot()
	syncLog.WithFields(logrus.Fields{
		"total": stats.Total, "created": stats.Created, "updated": stats.Updated,
		"skipped": stats.Skipped, "errors": stats.Errors,
	}).Warn("done")
	if stats.Errors > 0 {
		return fmt.Errorf("%d device(s) failed to sync", stats.Errors)
	}
	return nil
}

func loadDevices(ctx context.Context, cfg *config.Config) ([]google.Device, error) {
	cachePath := filepath.Join(cfg.Sync.CacheDir, "devices.json")
	if cfg.Sync.UseCache {
		data, err := os.ReadFile(cachePath)
		if err != nil {
			return nil, fmt.Errorf("read cache: %w", err)
		}
		return google.DeserializeDevices(data)
	}

	gc, err := google.New(cfg.Google, googleLog)
	if err != nil {
		return nil, err
	}
	if syncDeviceID != "" {
		d, err := gc.GetDevice(ctx, syncDeviceID)
		if err != nil {
			return nil, err
		}
		return []google.Device{d}, nil
	}
	devs, err := gc.ListAllChromeOSDevices(ctx)
	if err != nil {
		return nil, err
	}
	if data, err := google.SerializeDevices(devs); err == nil {
		_ = os.MkdirAll(cfg.Sync.CacheDir, 0o755)
		_ = os.WriteFile(cachePath, data, 0o644)
	}
	return devs, nil
}

func filterSerial(devs []google.Device, serial string) []google.Device {
	var out []google.Device
	for _, d := range devs {
		if d.SerialNumber == serial {
			out = append(out, d)
		}
	}
	return out
}
```

- [ ] **Step 2: Verify it builds**

Run: `go build ./... && go run . sync --help`
Expected: build succeeds; help shows all sync flags.

- [ ] **Step 3: Commit**

```bash
gofmt -w . && go vet ./...
git add cmd/sync.go
git -c user.name="Robbie Trencheny" -c user.email="robbie@campus.edu" commit -m "feat(cmd): sync command with cache, single-device, flag overrides

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 11: `setup` command (ChromeOS field defs, SetupFields, config merge)

**Files:**
- Create: `cmd/setup.go`
- Create: `cmd/setup_fields.go`
- Create: `config/merge.go`
- Create: `config/merge_test.go`

**Interfaces:**
- Consumes: `snipe.SetupFields`, `snipe.FieldDef`.
- Produces: `setup` cobra subcommand; `cmd.chromeFieldDefs() ([]snipe.FieldDef, map[string]config.FieldMappingEntry)`; `config.MergeFieldMapping(path string, entries map[string]FieldMappingEntry) error`.

- [ ] **Step 1: Port the YAML merge** — read `$FLEET/config/config.go` `MergeFieldMapping` and copy it into `config/merge.go`, adapting the function signature to `MergeFieldMapping(path string, entries map[string]FieldMappingEntry) error` where `entries` maps `db_column_name -> FieldMappingEntry`. It must: load the YAML as a `yaml.Node`, find/create `sync.field_mapping`, set each `db_column_name` key to either a bare scalar (path only) or a `{path, transform}` map, preserve comments, and write back.

- [ ] **Step 2: Write the failing test** `config/merge_test.go`

```go
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
```

Add helper `yamlUnmarshal` to `config/merge.go`:
```go
func yamlUnmarshal(data []byte, v any) error { return yaml.Unmarshal(data, v) }
```

- [ ] **Step 3: Run it to verify it fails**

Run: `go test ./config/ -run TestMergeFieldMapping -v`
Expected: FAIL — `MergeFieldMapping` undefined.

- [ ] **Step 4: Implement `config/merge.go`** (port as described in Step 1). Run the test until PASS.

Run: `go test ./config/ -v`
Expected: PASS.

- [ ] **Step 5: Write `cmd/setup_fields.go`** (the core 33-field set + mappings from the spec)

```go
package cmd

import (
	"github.com/CampusTech/google2snipe/config"
	"github.com/CampusTech/google2snipe/snipe"
)

type fieldSpec struct {
	name      string
	element   string // text|listbox|radio|checkbox
	format    string // ANY|NUMERIC|IP|MAC|URL|DATE|BOOLEAN
	path      string
	transform string
}

// coreFields is the default ChromeOS custom-field set created by `setup`.
var coreFields = []fieldSpec{
	{"Chrome: Serial", "text", "ANY", "serialNumber", ""},
	{"Chrome: Device ID", "text", "ANY", "deviceId", ""},
	{"Chrome: Model", "text", "ANY", "model", ""},
	{"Chrome: OS Type", "text", "ANY", "chromeOsType", ""},
	{"Chrome: OS Version", "text", "ANY", "osVersion", ""},
	{"Chrome: Platform Version", "text", "ANY", "platformVersion", ""},
	{"Chrome: Firmware Version", "text", "ANY", "firmwareVersion", ""},
	{"Chrome: OS Compliance", "text", "ANY", "osVersionCompliance", ""},
	{"Chrome: OS Update State", "text", "ANY", "osUpdateStatus.state", ""},
	{"Chrome: Status", "text", "ANY", "status", ""},
	{"Chrome: Org Unit Path", "text", "ANY", "orgUnitPath", ""},
	{"Chrome: Annotated User", "text", "ANY", "annotatedUser", ""},
	{"Chrome: Annotated Asset ID", "text", "ANY", "annotatedAssetId", ""},
	{"Chrome: Annotated Location", "text", "ANY", "annotatedLocation", ""},
	{"Chrome: Boot Mode", "text", "ANY", "bootMode", ""},
	{"Chrome: MAC", "text", "MAC", "macAddress", "mac_colons"},
	{"Chrome: Ethernet MAC", "text", "MAC", "ethernetMacAddress", "mac_colons"},
	{"Chrome: Last Known IP", "text", "IP", "lastKnownNetwork.0.ipAddress", ""},
	{"Chrome: CPU Model", "text", "ANY", "cpuInfo.0.model", ""},
	{"Chrome: System RAM (GB)", "text", "NUMERIC", "systemRamTotal", "bytes_to_gb"},
	{"Chrome: Disk Capacity (GB)", "text", "NUMERIC", "diskSpaceUsage.capacityBytes", "bytes_to_gb"},
	{"Chrome: Disk Used (GB)", "text", "NUMERIC", "diskSpaceUsage.usedBytes", "bytes_to_gb"},
	{"Chrome: License Type", "text", "ANY", "deviceLicenseType", ""},
	{"Chrome: Manufacture Date", "text", "DATE", "manufactureDate", "date_only"},
	{"Chrome: Order Number", "text", "ANY", "orderNumber", ""},
	{"Chrome: Auto-Update Through", "text", "DATE", "autoUpdateThrough", "date_only"},
	{"Chrome: Support End Date", "text", "DATE", "supportEndDate", "date_only"},
	{"Chrome: First Enrollment", "text", "DATE", "firstEnrollmentTime", "date_only"},
	{"Chrome: Last Enrollment", "text", "DATE", "lastEnrollmentTime", "date_only"},
	{"Chrome: Last Sync", "text", "ANY", "lastSync", "datetime"},
	{"Chrome: TPM Spec Level", "text", "ANY", "tpmVersionInfo.specLevel", ""},
	{"Chrome: Notes", "text", "ANY", "notes", ""},
	{"Chrome: Recent Users", "text", "ANY", "recentUsers.#.email", ""},
}

// chromeFieldDefs returns the FieldDefs to create and the field-name -> mapping
// (the engine fills db_column_name after creation).
func chromeFieldDefs() ([]snipe.FieldDef, map[string]config.FieldMappingEntry) {
	defs := make([]snipe.FieldDef, 0, len(coreFields))
	pathByName := make(map[string]config.FieldMappingEntry, len(coreFields))
	for _, f := range coreFields {
		defs = append(defs, snipe.FieldDef{Name: f.name, Element: f.element, Format: f.format})
		pathByName[f.name] = config.FieldMappingEntry{Path: f.path, Transform: f.transform}
	}
	return defs, pathByName
}
```

- [ ] **Step 6: Write `cmd/setup.go`**

```go
package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/CampusTech/google2snipe/config"
	"github.com/CampusTech/google2snipe/snipe"
)

var setupDryRun bool

var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Create ChromeOS custom fields in Snipe-IT and merge mappings into config",
	RunE:  runSetup,
}

func init() {
	setupCmd.Flags().BoolVar(&setupDryRun, "dry-run", false, "simulate without creating fields")
	rootCmd.AddCommand(setupCmd)
}

func runSetup(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return err
	}
	sc, err := snipe.New(cfg.SnipeIT.URL, cfg.SnipeIT.APIKey, setupDryRun, cfg.Sync.RateLimit, snipeLog)
	if err != nil {
		return err
	}
	defs, pathByName := chromeFieldDefs()

	fieldsetIDs := []int{}
	if cfg.SnipeIT.CustomFieldsetID != 0 {
		fieldsetIDs = append(fieldsetIDs, cfg.SnipeIT.CustomFieldsetID)
	}
	if len(fieldsetIDs) == 0 {
		return fmt.Errorf("snipe_it.custom_fieldset_id is required for setup")
	}

	dbColByName, err := sc.SetupFields(fieldsetIDs, defs)
	if err != nil {
		return err
	}
	if setupDryRun {
		snipeLog.WithField("fields", len(defs)).Warn("[DRY RUN] would create/update fields and merge config")
		return nil
	}

	merge := map[string]config.FieldMappingEntry{}
	for name, entry := range pathByName {
		dbCol := dbColByName[name]
		if dbCol == "" {
			snipeLog.WithField("field", name).Warn("no db_column_name returned; skipping mapping")
			continue
		}
		merge[dbCol] = entry
	}
	if err := config.MergeFieldMapping(cfgFile, merge); err != nil {
		return fmt.Errorf("merge field mapping: %w", err)
	}
	snipeLog.WithField("fields", len(merge)).Warn("setup complete; field_mapping merged into config")
	return nil
}
```

- [ ] **Step 7: Verify build + tests**

Run: `go build ./... && go test ./... && go run . setup --help`
Expected: build + all tests pass; setup help prints.

- [ ] **Step 8: Commit**

```bash
gofmt -w . && go vet ./...
git add cmd/setup.go cmd/setup_fields.go config/merge.go config/merge_test.go
git -c user.name="Robbie Trencheny" -c user.email="robbie@campus.edu" commit -m "feat(cmd): setup command — ChromeOS field set + config merge

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 12: `test` command (connectivity)

**Files:**
- Create: `cmd/test.go`

**Interfaces:**
- Consumes: `google.New`/`About`, `snipe.New`/`Ping`.
- Produces: `test` cobra subcommand.

- [ ] **Step 1: Write `cmd/test.go`**

```go
package cmd

import (
	"github.com/spf13/cobra"

	"github.com/CampusTech/google2snipe/config"
	"github.com/CampusTech/google2snipe/google"
	"github.com/CampusTech/google2snipe/snipe"
)

var testCmd = &cobra.Command{
	Use:   "test",
	Short: "Verify connectivity to the Google Admin SDK and Snipe-IT",
	RunE:  runTest,
}

func init() { rootCmd.AddCommand(testCmd) }

func runTest(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return err
	}
	gc, err := google.New(cfg.Google, googleLog)
	if err != nil {
		return err
	}
	customer, err := gc.About(cmd.Context())
	if err != nil {
		return err
	}
	googleLog.WithField("customer_id", customer).Warn("google admin sdk: OK")

	sc, err := snipe.New(cfg.SnipeIT.URL, cfg.SnipeIT.APIKey, true, cfg.Sync.RateLimit, snipeLog)
	if err != nil {
		return err
	}
	ver, err := sc.Ping()
	if err != nil {
		return err
	}
	snipeLog.WithField("version", ver).Warn("snipe-it: OK")
	return nil
}
```

- [ ] **Step 2: Verify build**

Run: `go build ./... && go run . test --help`
Expected: build succeeds; help prints.

- [ ] **Step 3: Commit**

```bash
gofmt -w . && go vet ./...
git add cmd/test.go
git -c user.name="Robbie Trencheny" -c user.email="robbie@campus.edu" commit -m "feat(cmd): test command for API connectivity

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 13: Docs, example config, Dockerfile

**Files:**
- Create: `settings.example.yaml`
- Create: `README.md`
- Create: `Dockerfile`

**Interfaces:** none (project deliverables).

- [ ] **Step 1: Write `settings.example.yaml`**

```yaml
# google2snipe — sync ChromeOS devices from the Google Admin SDK into Snipe-IT.
# Copy to settings.yaml and fill in. Secrets can also come from env:
#   SNIPE_URL, SNIPE_API_KEY, GOOGLE_APPLICATION_CREDENTIALS,
#   GOOGLE_IMPERSONATE_SUBJECT, GOOGLE_CUSTOMER_ID

google:
  # Service-account JSON key with domain-wide delegation. Grant the SA the
  # scope https://www.googleapis.com/auth/admin.directory.device.chromeos.readonly
  # in Admin Console > Security > API controls > Domain-wide delegation.
  credentials_file: /path/to/service-account.json
  # An admin user to impersonate (DWD). Required.
  impersonate_subject: admin@campusgroup.co
  customer_id: my_customer      # default; resolves to your account's customer
  projection: full              # full (default) | basic. FULL is needed for
                                # recentUsers (checkout fallback) + report fields.
  org_unit_path: ""             # optional: only sync this OU subtree
  query: ""                     # optional: Directory API search query

snipe_it:
  url: https://snipe.campusgroup.co
  api_key: REPLACE_ME
  default_status_id: 2          # status for new/unmapped assets (REQUIRED)
  default_category_id: 5        # ChromeOS category (REQUIRED)
  default_manufacturer_id: 0    # fallback when vendor can't be derived from model
  custom_fieldset_id: 3         # fieldset for ChromeOS models (REQUIRED for setup)
  # Map ChromeOS lifecycle status -> Snipe status label IDs.
  status_map:
    ACTIVE: 2
    DEPROVISIONED: 4
    DISABLED: 4
  # Map model-vendor (lowercased first token of `model`) -> manufacturer ID.
  manufacturer_ids:
    lenovo: 10
    acer: 11
    hp: 12
    dell: 13
    asus: 14
    google: 15

sync:
  dry_run: false
  rate_limit: true              # token-bucket limit on Snipe-IT writes
  update_only: false
  set_name: false
  name_template: "{annotatedAssetId}"
  cache_dir: .cache
  asset_tag:
    template: "{annotatedAssetId}"   # empty render -> Snipe auto-assigns
  # field_mapping is populated by `google2snipe setup`. Example shape:
  #   _snipeit_chrome_serial_1: serialNumber
  #   _snipeit_chrome_ram_2: {path: systemRamTotal, transform: bytes_to_gb}
  field_mapping: {}
  checkout:
    enabled: false
    use_annotated_user: true
    fallback_to_recent: true
    recent_user_domain: campusgroup.co  # only count recent users at this domain
    match_field: email          # email | username | employee_num
    mode: assign                # assign | sync | force

# --- Optional / opt-in fields (not created by `setup`; add to field_mapping
#     manually if you want them) ---
#   _snipeit_chrome_meid_X: meid                       # cellular models only
#   _snipeit_chrome_wan_ip_X: lastKnownNetwork.0.wanIpAddress
#   _snipeit_chrome_dock_mac_X: {path: dockMacAddress, transform: mac_colons}
#   _snipeit_chrome_will_renew_X: {path: willAutoRenew, transform: bool_yes_no}
#   _snipeit_chrome_deprovision_reason_X: deprovisionReason
```

- [ ] **Step 2: Write `README.md`** (features + quick start)

````markdown
# google2snipe

Sync **ChromeOS devices** from the Google Admin SDK Directory API into
[Snipe-IT](https://snipeitapp.com/). A sibling of `fleet2snipe`.

## Features

- Full reconciliation sweep (`sync`) for cron, plus single-device sync
  (`--serial` / `--device-id`).
- Idempotent `setup` that creates a 33-field ChromeOS custom-field set in
  Snipe-IT and merges the resulting mappings into your config.
- Configurable field mapping via gjson paths + transforms over the full
  ChromeOsDevice schema (any nested/array field).
- ChromeOS lifecycle status → Snipe status label mapping.
- Optional checkout to the assigned user (`annotatedUser`), falling back to the
  most-recent managed login user (domain-restricted).
- `--dry-run`, `--debug`, local response cache (`--use-cache`), structured logs.

## Authentication

Create a Google Cloud service account, enable the Admin SDK API, and grant it
**domain-wide delegation** for scope
`https://www.googleapis.com/auth/admin.directory.device.chromeos.readonly`
(Admin Console → Security → API controls → Domain-wide delegation). Download the
JSON key and set `google.credentials_file` + `google.impersonate_subject` (an
admin to impersonate).

## Quick start

```bash
go build .
cp settings.example.yaml settings.yaml
$EDITOR settings.yaml          # creds + Snipe IDs
./google2snipe test            # verify connectivity
./google2snipe setup           # create custom fields, merge mappings
./google2snipe sync --dry-run --verbose
./google2snipe sync            # do it
```

Run `./google2snipe sync` from cron (e.g. every 15 min). Projection defaults to
`full`.

## License

MIT
````

- [ ] **Step 3: Write `Dockerfile`**

```dockerfile
FROM golang:1.26.4-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=docker
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=${VERSION}" -o /out/google2snipe .

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/google2snipe /usr/local/bin/google2snipe
USER nonroot
ENTRYPOINT ["google2snipe"]
CMD ["sync"]
```

- [ ] **Step 4: Final verification**

Run:
```bash
go build ./... && go test ./... && go vet ./...
./google2snipe --help
```
Expected: clean build, all tests pass, help lists `sync`, `setup`, `test`.

- [ ] **Step 5: Commit**

```bash
gofmt -w .
git add settings.example.yaml README.md Dockerfile
git -c user.name="Robbie Trencheny" -c user.email="robbie@campus.edu" commit -m "docs: example config, README, Dockerfile

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
git push origin main
```

---

## Self-Review Notes (author checklist — completed)

**Spec coverage:**
- Auth (SA key + DWD, env fallback) → Task 4 + config Task 2. ✓
- Source list/get/paging/projection/OU/query filters/cache → Task 4 + Task 10. ✓
- Match by serial, upsert, >1 skip → Task 9. ✓
- Asset tag template (default `{annotatedAssetId}`) → Task 7. ✓
- Model auto-create; manufacturer from model first-token + map + default → Task 8. ✓
- Status map + default; status updated on existing → Task 7 + Task 9. ✓
- OU custom field only → via field_mapping (`orgUnitPath`), Task 11 core set. ✓
- Checkout annotatedUser→recent (domain-filtered, managed) + match_field + mode → Task 8 + Task 9. ✓
- gjson field_mapping + transforms; empty skipped; FULL-only warning; int64-as-string → Task 6 + Task 2. ✓
- setup 33-field core set with formats + config merge → Task 11. ✓
- Commands/flags → Tasks 10–12. ✓
- logrus, dry-run, rate-limit, cache, Docker, README, MIT → Tasks 1,5,10,13. ✓
- Dropped: serve, images, policy/query/label mapping. ✓ (absent by design)

**Type consistency:** `SnipeClient` interface (Task 7) matches `stubSnipe` (Task 7) and the ported `snipe.Client` surface (Task 5). `snipe.Asset.CustomFields` is `map[string]string` everywhere. `config.FieldMappingEntry{Path,Transform}` used consistently. `google.Device` embeds `*admin.ChromeOsDevice` so `.SerialNumber`, `.Status`, `.Model`, `.AnnotatedUser`, `.RecentUsers`, `.LastSync`, `.LastEnrollmentTime`, `.DeviceId` resolve via the SDK struct.

**Known port risks (flagged for implementer):** Task 5 (go-snipeit wrapper) and Task 11 Step 1 (YAML merge) are ports from fleet2snipe — read those source files and match the exact method/format behavior. If go-snipeit's `field create` API names the format/element parameters differently, adapt inside `SetupFields` without changing its signature.
