# License Cost Sync Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Sync Google Workspace user subscriptions and ChromeOS device upgrade licenses into Snipe-IT as first-class Licenses with per-seat cost, plus an interactive `licenses setup` that discovers them and quizzes for prices — so Snipe-IT can attribute ongoing cost per user.

**Architecture:** A new `licenses` cobra command group (`setup`, `sync`) over: a hand-rolled `snipe` License/seat client (go-snipeit has none), a `google/licensing.go` client (`licensing/v1`), and a `licensesync` reconcile engine. Every license — Workspace (seat per user) and ChromeOS upgrade (seat per device asset) — is a Snipe-IT License; `reassignable: false` models perpetual upgrades. Reuses the existing device cache, Snipe user list, and `GetAssetBySerial`.

**Tech Stack:** Go 1.26.4, `google.golang.org/api/licensing/v1`, `golang.org/x/oauth2/google`, raw `net/http` for Snipe licenses, `github.com/spf13/cobra`, `github.com/sirupsen/logrus`, `gopkg.in/yaml.v3`, `github.com/tidwall/gjson` (already present).

**Spec:** `docs/superpowers/specs/2026-06-17-license-cost-sync-design.md` (read it first).

## Global Constraints

- **Module path:** `github.com/CampusTech/google2snipe`; Go 1.26.4.
- **Everything is a Snipe-IT License with seats** — no Consumables. Perpetual Chrome upgrades → `reassignable: false`; recurring (fixed-term/annual) + Workspace → `reassignable: true`.
- **Classification rule:** `deviceLicenseType` containing `FixedTerm`, or equal to `enterpriseUpgrade`/`kioskUpgrade` → recurring (reassignable); everything else (incl. `*Perpetual`, bundled `education`/`enterprise`, deprecated `educationUpgrade`) → perpetual (non-reassignable). Config may override per type. `deviceLicenseTypeUnspecified`/empty is skipped.
- **Costs are config-provided** per seat (Google exposes none). Unmapped SKU/type → cost `0`. Stamped onto the Snipe License `purchase_cost`.
- **Reconcile:** recurring licenses fully reconcile (check stale seats in); perpetual licenses are additive (never reclaim). Idempotent (seat carries the user/asset id).
- **Matching:** Workspace user by email (lowercased, local-part fallback) against the Snipe user list; Chrome device by `serialNumber` via `GetAssetBySerial`. Unmatched → log + skip, never auto-create.
- **No Google-side writes**; read-only on Google, seat checkouts only on Snipe.
- **New DWD scope:** `https://www.googleapis.com/auth/apps.licensing` (Workspace part) — the operator must add it to the SA's domain-wide delegation before the Workspace phase works.
- **Dry-run** enforced before every Snipe mutation. `--dry-run`/`--use-cache` on `licenses sync`.
- **Lint/format:** `gofmt`/`goimports` clean; `go vet ./...` clean; `go mod tidy` idempotent before each commit.
- **Commit author** `Robbie Trencheny <robbie@campus.edu>`; end every commit body with `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.

---

## Shared Type Reference (defined across tasks — do not redefine)

```go
// package config (Task 1)
type LicensesConfig struct {
    Enabled                  bool                           `yaml:"enabled"`
    DefaultLicenseCategoryID int                            `yaml:"default_license_category_id"`
    Chrome                   map[string]ChromeLicenseConfig `yaml:"chrome"` // keyed by deviceLicenseType
    Workspace                WorkspaceLicenseConfig         `yaml:"workspace"`
}
type ChromeLicenseConfig struct {
    Name         string  `yaml:"name"`
    Cost         float64 `yaml:"cost"`
    Reassignable *bool   `yaml:"reassignable"` // nil => inferred from the type
    TermMonths   int     `yaml:"term_months"`  // optional; recurring => sets expiry
}
type WorkspaceLicenseConfig struct {
    CustomerID string             `yaml:"customer_id"` // Licensing API customer (domain or id); "" => derive from impersonate_subject domain
    Products   []string           `yaml:"products"`
    SKUCosts   map[string]float64 `yaml:"sku_costs"`
}

// package snipe (Tasks 2-4)
type LicenseSpec struct {
    Name           string
    CostPerSeat    float64
    CategoryID     int
    Reassignable   bool
    Seats          int    // minimum seats to ensure
    ExpirationDate string // "YYYY-MM-DD" or "" for none
}
type License struct {
    ID    int
    Name  string
    Seats int
}
type LicenseSeat struct {
    ID              int
    AssignedUserID  int // 0 if not assigned to a user
    AssignedAssetID int // 0 if not assigned to an asset
}

// package licensesync (Task 5)
type Target struct { // a desired seat-holder
    IsUser bool
    ID     int // user id or asset id
}
type LicenseClient interface { // satisfied by *snipe.LicenseClient
    EnsureLicense(spec snipe.LicenseSpec) (snipe.License, error)
    EnsureSeats(licenseID, total int) error
    ListSeats(licenseID int) ([]snipe.LicenseSeat, error)
    CheckoutSeatToUser(seatID, userID int) error
    CheckoutSeatToAsset(seatID, assetID int) error
    CheckinSeat(seatID int) error
}

// package google (Task 8)
type LicenseAssignment struct {
    UserEmail   string
    ProductID   string
    SKUID       string
    SKUName     string
}
```

---

## Task 1: Config — `licenses` section, classification helper, validation

**Files:**
- Modify: `config/config.go`
- Test: `config/config_test.go`

**Interfaces:**
- Produces: the `LicensesConfig`/`ChromeLicenseConfig`/`WorkspaceLicenseConfig` structs (Shared Type Reference); `config.ChromePerpetual(deviceLicenseType string) bool`; `Licenses LicensesConfig` field on `Config`; validation that requires `default_license_category_id` when `licenses.enabled`.

- [ ] **Step 1: Write the failing test** — append to `config/config_test.go`

```go
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
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./config/ -run 'TestChromePerpetual|TestLicensesValidation' -v`
Expected: FAIL — `ChromePerpetual` undefined.

- [ ] **Step 3: Add the structs, field, helper, and validation to `config/config.go`**

Add the three structs from the Shared Type Reference. Add to `Config`: `Licenses LicensesConfig \`yaml:"licenses"\``. Add the helper:

```go
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
```

In `Validate()`, before `return nil`, add:

```go
	if c.Licenses.Enabled && c.Licenses.DefaultLicenseCategoryID == 0 {
		return fmt.Errorf("licenses.default_license_category_id is required when licenses.enabled")
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./config/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w . && go vet ./...
git add config/
git -c user.name="Robbie Trencheny" -c user.email="robbie@campus.edu" commit -m "feat(config): licenses section + perpetual classification

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: Snipe LicenseClient — raw-HTTP scaffolding, dry-run, ListLicenses

**Files:**
- Create: `snipe/licenses.go`
- Test: `snipe/licenses_test.go`

**Interfaces:**
- Consumes: nothing (self-contained raw HTTP).
- Produces: `snipe.NewLicenseClient(url, apiKey string, dryRun bool, logger *logrus.Logger) *LicenseClient`; the `LicenseSpec`/`License`/`LicenseSeat` types; `(*LicenseClient) ListLicenses() ([]License, error)`; reuses `snipe.ErrDryRun`.

- [ ] **Step 1: Verify the Snipe-IT licenses API shape live (no code yet)**

The exact JSON matters and go-snipeit has no models for it. Run these against the test instance and record the field names you see:

```bash
TOKEN=$(grep '^API_TOKEN=' ~/Repos/Campus/IT/Google2Snipe-IT/.env | cut -d= -f2-)
BASE=https://campus-students.snipe-it.io/api/v1
# list shape (note: rows[].id, name, seats, free_seats_count, reassignable, category, purchase_cost)
curl -s -H "Authorization: Bearer $TOKEN" -H "Accept: application/json" "$BASE/licenses?limit=1" | python3 -m json.tool
```
Expected: `{ "total": ..., "rows": [...] }`. Confirm a license row exposes `id`, `name`, `seats`, `reassignable`, `category` (or `category_id`), `purchase_cost`. Use these exact keys in the structs below; adjust if the instance differs.

- [ ] **Step 2: Write the failing test** `snipe/licenses_test.go`

```go
package snipe

import (
	"errors"
	"testing"

	"github.com/sirupsen/logrus"
)

func TestLicenseClientDryRunSentinel(t *testing.T) {
	c := NewLicenseClient("https://snipe.invalid", "key", true /*dryRun*/, logrus.New())
	// EnsureLicense is a mutator; in dry-run it must not dial and must return ErrDryRun.
	_, err := c.EnsureLicense(LicenseSpec{Name: "X", CategoryID: 1, Seats: 1})
	if !errors.Is(err, ErrDryRun) {
		t.Fatalf("EnsureLicense dry-run = %v, want ErrDryRun", err)
	}
}
```

(EnsureLicense is implemented in Task 3; this test compiles once Task 3 adds it. For Task 2, stub `EnsureLicense` to `return License{}, ErrDryRun` when dryRun and `panic("task 3")` otherwise — replaced in Task 3. Keep the test.)

- [ ] **Step 3: Run it to verify it fails**

Run: `go test ./snipe/ -run TestLicenseClientDryRun -v`
Expected: FAIL — `NewLicenseClient` undefined.

- [ ] **Step 4: Write `snipe/licenses.go`** (raw HTTP scaffolding + ListLicenses)

```go
package snipe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// LicenseClient talks to the Snipe-IT licenses/seats endpoints directly, because
// go-snipeit has no support for them. It is separate from the go-snipeit-backed
// asset Client.
type LicenseClient struct {
	baseURL string
	apiKey  string
	dryRun  bool
	http    *http.Client
	log     *logrus.Logger
}

func NewLicenseClient(url, apiKey string, dryRun bool, logger *logrus.Logger) *LicenseClient {
	if logger == nil {
		logger = logrus.New()
	}
	return &LicenseClient{
		baseURL: strings.TrimRight(url, "/"),
		apiKey:  apiKey,
		dryRun:  dryRun,
		http:    &http.Client{Timeout: 30 * time.Second},
		log:     logger,
	}
}

// snipeResp is the common {status, messages, payload} envelope.
type snipeResp struct {
	Status   string          `json:"status"`
	Messages json.RawMessage `json:"messages"`
	Payload  json.RawMessage `json:"payload"`
}

// do issues an authenticated request to /api/v1<path>. For mutating methods the
// caller must check dryRun first. out (if non-nil) receives the decoded payload.
func (c *LicenseClient) do(method, path string, body any) (*snipeResp, *http.Response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.baseURL+"/api/v1"+path, rdr)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	c.log.WithFields(logrus.Fields{"method": method, "path": path}).Debug("snipe license request")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	var r snipeResp
	data, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(data, &r) // some endpoints (list) have no status; tolerate
	return &r, resp, nil
}

// ListLicenses returns all licenses (paginated).
func (c *LicenseClient) ListLicenses() ([]License, error) {
	var out []License
	offset := 0
	const limit = 100
	for {
		_, resp, err := c.do(http.MethodGet, fmt.Sprintf("/licenses?limit=%d&offset=%d", limit, offset), nil)
		if err != nil {
			return nil, err
		}
		var page struct {
			Total int `json:"total"`
			Rows  []struct {
				ID    int    `json:"id"`
				Name  string `json:"name"`
				Seats int    `json:"seats"`
			} `json:"rows"`
		}
		data, _ := io.ReadAll(resp.Body) // body already consumed in do(); re-issue properly below
		_ = data
		_ = page
		break // replaced below
	}
	return out, nil
}
```

NOTE TO IMPLEMENTER: `do()` consumes the body for the envelope; for list endpoints the page (total+rows) IS the body, not an envelope `payload`. Refactor `do` to return the raw `[]byte` body so both list (`{total,rows}`) and mutation (`{status,payload}`) callers can decode what they need. Concretely: change `do` to `func (c *LicenseClient) do(method, path string, body any) (raw []byte, status int, err error)`, then `ListLicenses` unmarshals `raw` into `{total, rows}`, and mutators unmarshal into `snipeResp`. Implement that shape (verified against Step 1's output) so `ListLicenses` returns real rows.

- [ ] **Step 5: Add the stub `EnsureLicense` (replaced in Task 3) and run the test**

Add to `snipe/licenses.go`:
```go
func (c *LicenseClient) EnsureLicense(spec LicenseSpec) (License, error) {
	if c.dryRun {
		return License{}, ErrDryRun
	}
	panic("implemented in Task 3")
}
```

Run: `go build ./... && go test ./snipe/ -run TestLicenseClientDryRun -v`
Expected: PASS (dry-run returns ErrDryRun without dialing).

- [ ] **Step 6: Commit**

```bash
gofmt -w . && go vet ./... && go mod tidy
git add snipe/licenses.go snipe/licenses_test.go
git -c user.name="Robbie Trencheny" -c user.email="robbie@campus.edu" commit -m "feat(snipe): license client scaffolding + ListLicenses

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 3: Snipe LicenseClient — EnsureLicense + EnsureSeats

**Files:**
- Modify: `snipe/licenses.go`

**Interfaces:**
- Consumes: Task 2 `do`/`ListLicenses`.
- Produces: `(*LicenseClient) EnsureLicense(spec LicenseSpec) (License, error)` (create-or-find by name; on create set name/category/seats/cost/reassignable/expiry); `(*LicenseClient) EnsureSeats(licenseID, total int) error` (PATCH the seat total up if below `total`).

- [ ] **Step 1: Verify create + seats-grow shape live**

```bash
TOKEN=$(grep '^API_TOKEN=' ~/Repos/Campus/IT/Google2Snipe-IT/.env | cut -d= -f2-)
BASE=https://campus-students.snipe-it.io/api/v1
# Create a throwaway license (delete after) to learn the create body + payload shape
curl -s -X POST -H "Authorization: Bearer $TOKEN" -H "Accept: application/json" -H "Content-Type: application/json" \
  -d '{"name":"ZZ Test License","seats":1,"category_id":7,"reassignable":false,"purchase_cost":1.00}' "$BASE/licenses" | python3 -m json.tool
```
Confirm: `status: success`, `payload.id`, and that `seats`/`reassignable`/`category_id`/`purchase_cost` are accepted (use category id 7 or whatever exists — create a license category first if none). Confirm increasing seats works via `PATCH /licenses/{id}` `{"seats": N}`. Delete the throwaway license afterward.

- [ ] **Step 2: Replace the stub `EnsureLicense` and add `EnsureSeats`**

```go
// EnsureLicense finds a license by name or creates it. On create it sets the
// category, seats, cost, reassignable flag, and (optional) expiration.
func (c *LicenseClient) EnsureLicense(spec LicenseSpec) (License, error) {
	existing, err := c.ListLicenses()
	if err != nil {
		return License{}, err
	}
	for _, l := range existing {
		if strings.EqualFold(l.Name, spec.Name) {
			return l, nil
		}
	}
	if c.dryRun {
		return License{}, ErrDryRun
	}
	body := map[string]any{
		"name":          spec.Name,
		"seats":         max(spec.Seats, 1),
		"category_id":   spec.CategoryID,
		"reassignable":  spec.Reassignable,
		"purchase_cost": spec.CostPerSeat,
	}
	if spec.ExpirationDate != "" {
		body["expiration_date"] = spec.ExpirationDate
	}
	raw, _, err := c.do(http.MethodPost, "/licenses", body)
	if err != nil {
		return License{}, err
	}
	var r snipeResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return License{}, fmt.Errorf("creating license %q: %w", spec.Name, err)
	}
	if r.Status != "success" {
		return License{}, fmt.Errorf("creating license %q: %s", spec.Name, string(r.Messages))
	}
	var p struct {
		ID    int    `json:"id"`
		Name  string `json:"name"`
		Seats int    `json:"seats"`
	}
	_ = json.Unmarshal(r.Payload, &p)
	return License{ID: p.ID, Name: p.Name, Seats: p.Seats}, nil
}

// EnsureSeats grows the license's seat total to at least total.
func (c *LicenseClient) EnsureSeats(licenseID, total int) error {
	if c.dryRun {
		return ErrDryRun
	}
	raw, _, err := c.do(http.MethodPatch, fmt.Sprintf("/licenses/%d", licenseID), map[string]any{"seats": total})
	if err != nil {
		return err
	}
	var r snipeResp
	_ = json.Unmarshal(raw, &r)
	if r.Status != "success" {
		return fmt.Errorf("growing license %d seats to %d: %s", licenseID, total, string(r.Messages))
	}
	return nil
}
```

(`max` is the Go 1.21+ builtin. The `do` signature is the refactored `(raw []byte, status int, err error)` from Task 2.)

- [ ] **Step 3: Build + the Task 2 dry-run test still passes**

Run: `go build ./... && go test ./snipe/ -run TestLicenseClientDryRun -v`
Expected: PASS.

- [ ] **Step 4: Live smoke (manual, optional but recommended)** — run a tiny Go scratch or curl to confirm EnsureLicense creates "ZZ Smoke" then is found (idempotent) on a second call; delete it after. Record the result in the commit message.

- [ ] **Step 5: Commit**

```bash
gofmt -w . && go vet ./...
git add snipe/licenses.go
git -c user.name="Robbie Trencheny" -c user.email="robbie@campus.edu" commit -m "feat(snipe): EnsureLicense + EnsureSeats

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 4: Snipe LicenseClient — seats: list, checkout (user/asset), checkin

**Files:**
- Modify: `snipe/licenses.go`

**Interfaces:**
- Produces: `(*LicenseClient) ListSeats(licenseID int) ([]LicenseSeat, error)`; `CheckoutSeatToUser(seatID, userID int) error`; `CheckoutSeatToAsset(seatID, assetID int) error`; `CheckinSeat(seatID int) error`.

- [ ] **Step 1: Verify the seats shape live**

```bash
TOKEN=$(grep '^API_TOKEN=' ~/Repos/Campus/IT/Google2Snipe-IT/.env | cut -d= -f2-)
BASE=https://campus-students.snipe-it.io/api/v1
# Using the ZZ Test License id from Task 3:
curl -s -H "Authorization: Bearer $TOKEN" -H "Accept: application/json" "$BASE/licenses/<id>/seats" | python3 -m json.tool
```
Confirm each seat row exposes its `id` and its current assignment (`assigned_user`/`assigned_to` and `asset_id`/`assigned_asset`). Confirm the checkout shape: `PATCH /licenses/{license}/seats/{seat}` with `{"assigned_to": <userId>}` (user) or `{"asset_id": <assetId>}` (asset); check-in = `{"assigned_to": null, "asset_id": null}`. Use the exact keys you observe.

- [ ] **Step 2: Add the seat methods to `snipe/licenses.go`**

```go
// ListSeats returns the license's seats and their current assignment.
func (c *LicenseClient) ListSeats(licenseID int) ([]LicenseSeat, error) {
	var out []LicenseSeat
	offset := 0
	const limit = 100
	for {
		raw, _, err := c.do(http.MethodGet, fmt.Sprintf("/licenses/%d/seats?limit=%d&offset=%d", licenseID, limit, offset), nil)
		if err != nil {
			return nil, err
		}
		var page struct {
			Total int `json:"total"`
			Rows  []struct {
				ID           int `json:"id"`
				AssignedUser *struct {
					ID int `json:"id"`
				} `json:"assigned_user"`
				AssignedAsset *struct {
					ID int `json:"id"`
				} `json:"assigned_asset"`
			} `json:"rows"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, fmt.Errorf("listing seats for license %d: %w", licenseID, err)
		}
		for _, s := range page.Rows {
			seat := LicenseSeat{ID: s.ID}
			if s.AssignedUser != nil {
				seat.AssignedUserID = s.AssignedUser.ID
			}
			if s.AssignedAsset != nil {
				seat.AssignedAssetID = s.AssignedAsset.ID
			}
			out = append(out, seat)
		}
		if len(out) >= page.Total {
			break
		}
		offset += limit
	}
	return out, nil
}

func (c *LicenseClient) patchSeat(licenseID, seatID int, body map[string]any) error {
	raw, _, err := c.do(http.MethodPatch, fmt.Sprintf("/licenses/%d/seats/%d", licenseID, seatID), body)
	if err != nil {
		return err
	}
	var r snipeResp
	_ = json.Unmarshal(raw, &r)
	if r.Status != "success" {
		return fmt.Errorf("seat %d on license %d: %s", seatID, licenseID, string(r.Messages))
	}
	return nil
}

func (c *LicenseClient) CheckoutSeatToUser(licenseID, seatID, userID int) error {
	if c.dryRun {
		return ErrDryRun
	}
	return c.patchSeat(licenseID, seatID, map[string]any{"assigned_to": userID})
}
func (c *LicenseClient) CheckoutSeatToAsset(licenseID, seatID, assetID int) error {
	if c.dryRun {
		return ErrDryRun
	}
	return c.patchSeat(licenseID, seatID, map[string]any{"asset_id": assetID})
}
func (c *LicenseClient) CheckinSeat(licenseID, seatID int) error {
	if c.dryRun {
		return ErrDryRun
	}
	return c.patchSeat(licenseID, seatID, map[string]any{"assigned_to": nil, "asset_id": nil})
}
```

NOTE: the seat checkout/checkin need the `licenseID` (the seat path is `/licenses/{license}/seats/{seat}`). Update the Shared Type Reference's `LicenseClient` interface methods to take `(licenseID, seatID, ...)`. The engine (Task 5) holds the license id from `EnsureLicense`, so it can pass it.

- [ ] **Step 3: Build**

Run: `go build ./... && go vet ./...`
Expected: clean.

- [ ] **Step 4: Live smoke (recommended)** — on the ZZ Test License: grow to 1 seat, list seats, checkout the seat to a known user id, list again (assigned), checkin, list again (free). Delete the ZZ license after. Record in commit.

- [ ] **Step 5: Commit**

```bash
gofmt -w .
git add snipe/licenses.go
git -c user.name="Robbie Trencheny" -c user.email="robbie@campus.edu" commit -m "feat(snipe): license seats — list/checkout(user|asset)/checkin

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 5: licensesync engine — reconcile algorithm

**Files:**
- Create: `licensesync/engine.go`
- Test: `licensesync/engine_test.go`

**Interfaces:**
- Consumes: `snipe.LicenseSpec`/`License`/`LicenseSeat`.
- Produces: `licensesync.LicenseClient` interface (Shared Type Reference, with seat methods taking `licenseID`); `licensesync.Target`; `licensesync.Engine`; `New(lc LicenseClient, logger *logrus.Logger) *Engine`; `(*Engine) Reconcile(spec snipe.LicenseSpec, desired []Target) (Stats, error)` where `Stats{CheckedOut, CheckedIn, AlreadyOK int}`.

- [ ] **Step 1: Write the failing test** `licensesync/engine_test.go`

```go
package licensesync

import (
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/CampusTech/google2snipe/snipe"
)

// stubLC is an in-memory LicenseClient.
type stubLC struct {
	lic       snipe.License
	seats     []snipe.LicenseSeat
	nextSeat  int
}

func (s *stubLC) EnsureLicense(spec snipe.LicenseSpec) (snipe.License, error) {
	if s.lic.ID == 0 {
		s.lic = snipe.License{ID: 1, Name: spec.Name, Seats: spec.Seats}
	}
	return s.lic, nil
}
func (s *stubLC) EnsureSeats(licenseID, total int) error {
	for len(s.seats) < total {
		s.nextSeat++
		s.seats = append(s.seats, snipe.LicenseSeat{ID: s.nextSeat})
	}
	s.lic.Seats = len(s.seats)
	return nil
}
func (s *stubLC) ListSeats(licenseID int) ([]snipe.LicenseSeat, error) { return s.seats, nil }
func (s *stubLC) setUser(seatID, uid int) {
	for i := range s.seats {
		if s.seats[i].ID == seatID {
			s.seats[i].AssignedUserID, s.seats[i].AssignedAssetID = uid, 0
		}
	}
}
func (s *stubLC) CheckoutSeatToUser(licenseID, seatID, userID int) error  { s.setUser(seatID, userID); return nil }
func (s *stubLC) CheckoutSeatToAsset(licenseID, seatID, assetID int) error { for i := range s.seats { if s.seats[i].ID==seatID { s.seats[i].AssignedAssetID, s.seats[i].AssignedUserID = assetID, 0 } }; return nil }
func (s *stubLC) CheckinSeat(licenseID, seatID int) error { for i := range s.seats { if s.seats[i].ID==seatID { s.seats[i].AssignedUserID, s.seats[i].AssignedAssetID = 0, 0 } }; return nil }

func TestReconcileReassignableCheckoutAndCheckin(t *testing.T) {
	stub := &stubLC{}
	// pre-seed: license with one seat already assigned to user 99 (stale)
	stub.lic = snipe.License{ID: 1, Name: "WS Plus", Seats: 1}
	stub.seats = []snipe.LicenseSeat{{ID: 1, AssignedUserID: 99}}
	stub.nextSeat = 1
	e := New(stub, logrus.New())
	// desired: users 10 and 20 (not 99)
	st, err := e.Reconcile(snipe.LicenseSpec{Name: "WS Plus", Reassignable: true, Seats: 1},
		[]Target{{IsUser: true, ID: 10}, {IsUser: true, ID: 20}})
	if err != nil {
		t.Fatal(err)
	}
	assigned := map[int]bool{}
	for _, s := range stub.seats {
		if s.AssignedUserID != 0 {
			assigned[s.AssignedUserID] = true
		}
	}
	if !assigned[10] || !assigned[20] || assigned[99] {
		t.Errorf("want {10,20} assigned, 99 checked in; got %v", assigned)
	}
	if st.CheckedOut != 2 || st.CheckedIn != 1 {
		t.Errorf("stats = %+v, want CheckedOut=2 CheckedIn=1", st)
	}
}

func TestReconcilePerpetualAdditiveNoCheckin(t *testing.T) {
	stub := &stubLC{lic: snipe.License{ID: 1, Name: "Chrome Perp", Seats: 1}, nextSeat: 1,
		seats: []snipe.LicenseSeat{{ID: 1, AssignedAssetID: 99}}} // stale asset 99
	e := New(stub, logrus.New())
	st, err := e.Reconcile(snipe.LicenseSpec{Name: "Chrome Perp", Reassignable: false, Seats: 1},
		[]Target{{IsUser: false, ID: 10}})
	if err != nil {
		t.Fatal(err)
	}
	// perpetual: asset 10 checked out, stale asset 99 NOT checked in
	stale := false
	for _, s := range stub.seats {
		if s.AssignedAssetID == 99 {
			stale = true
		}
	}
	if !stale {
		t.Error("perpetual license must NOT check in stale seats")
	}
	if st.CheckedIn != 0 {
		t.Errorf("perpetual CheckedIn = %d, want 0", st.CheckedIn)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./licensesync/ -v`
Expected: FAIL — package/`New` undefined.

- [ ] **Step 3: Write `licensesync/engine.go`**

```go
package licensesync

import (
	"github.com/sirupsen/logrus"

	"github.com/CampusTech/google2snipe/snipe"
)

type LicenseClient interface {
	EnsureLicense(spec snipe.LicenseSpec) (snipe.License, error)
	EnsureSeats(licenseID, total int) error
	ListSeats(licenseID int) ([]snipe.LicenseSeat, error)
	CheckoutSeatToUser(licenseID, seatID, userID int) error
	CheckoutSeatToAsset(licenseID, seatID, assetID int) error
	CheckinSeat(licenseID, seatID int) error
}

type Target struct {
	IsUser bool
	ID     int
}

type Stats struct{ CheckedOut, CheckedIn, AlreadyOK int }

type Engine struct {
	lc  LicenseClient
	log *logrus.Logger
}

func New(lc LicenseClient, logger *logrus.Logger) *Engine {
	if logger == nil {
		logger = logrus.New()
	}
	return &Engine{lc: lc, log: logger}
}

// Reconcile ensures the license exists and its seats match the desired holders.
// Reassignable licenses check stale seats in; non-reassignable (perpetual) are
// additive only.
func (e *Engine) Reconcile(spec snipe.LicenseSpec, desired []Target) (Stats, error) {
	var st Stats
	lic, err := e.lc.EnsureLicense(spec)
	if err != nil {
		return st, err
	}
	if len(desired) > lic.Seats {
		if err := e.lc.EnsureSeats(lic.ID, len(desired)); err != nil {
			return st, err
		}
	}
	seats, err := e.lc.ListSeats(lic.ID)
	if err != nil {
		return st, err
	}
	curUser := map[int]int{}  // userID -> seatID
	curAsset := map[int]int{} // assetID -> seatID
	var free []int
	for _, s := range seats {
		switch {
		case s.AssignedUserID != 0:
			curUser[s.AssignedUserID] = s.ID
		case s.AssignedAssetID != 0:
			curAsset[s.AssignedAssetID] = s.ID
		default:
			free = append(free, s.ID)
		}
	}
	wantUser := map[int]bool{}
	wantAsset := map[int]bool{}
	for _, t := range desired {
		if t.IsUser {
			wantUser[t.ID] = true
		} else {
			wantAsset[t.ID] = true
		}
	}
	popFree := func() (int, bool) {
		if len(free) == 0 {
			return 0, false
		}
		id := free[0]
		free = free[1:]
		return id, true
	}
	for _, t := range desired {
		if t.IsUser {
			if curUser[t.ID] != 0 {
				st.AlreadyOK++
				continue
			}
			seatID, ok := popFree()
			if !ok {
				e.log.WithField("license", lic.Name).Warn("no free seat available; grow failed?")
				continue
			}
			if err := e.lc.CheckoutSeatToUser(lic.ID, seatID, t.ID); err != nil {
				e.log.WithError(err).WithField("user_id", t.ID).Warn("seat checkout failed")
				continue
			}
			st.CheckedOut++
		} else {
			if curAsset[t.ID] != 0 {
				st.AlreadyOK++
				continue
			}
			seatID, ok := popFree()
			if !ok {
				e.log.WithField("license", lic.Name).Warn("no free seat available; grow failed?")
				continue
			}
			if err := e.lc.CheckoutSeatToAsset(lic.ID, seatID, t.ID); err != nil {
				e.log.WithError(err).WithField("asset_id", t.ID).Warn("seat checkout failed")
				continue
			}
			st.CheckedOut++
		}
	}
	if spec.Reassignable {
		for uid, seatID := range curUser {
			if !wantUser[uid] {
				if err := e.lc.CheckinSeat(lic.ID, seatID); err != nil {
					e.log.WithError(err).Warn("seat checkin failed")
					continue
				}
				st.CheckedIn++
			}
		}
		for aid, seatID := range curAsset {
			if !wantAsset[aid] {
				if err := e.lc.CheckinSeat(lic.ID, seatID); err != nil {
					e.log.WithError(err).Warn("seat checkin failed")
					continue
				}
				st.CheckedIn++
			}
		}
	}
	return st, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./licensesync/ -v`
Expected: PASS (both reconcile tests).

- [ ] **Step 5: Commit**

```bash
gofmt -w . && go vet ./...
git add licensesync/
git -c user.name="Robbie Trencheny" -c user.email="robbie@campus.edu" commit -m "feat(licensesync): seat reconcile engine (reassignable vs perpetual)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 6: Chrome upgrade → license targets

**Files:**
- Create: `licensesync/chrome.go`
- Test: `licensesync/chrome_test.go`

**Interfaces:**
- Consumes: `config.LicensesConfig`, `config.ChromePerpetual`, `google.Device`, the `snipe` asset lookup (passed as a func), `licensesync.Engine`.
- Produces: `(*Engine) SyncChrome(cfg config.LicensesConfig, devices []google.Device, assetIDBySerial func(string) (int, bool)) error` — groups devices by `deviceLicenseType`, builds a `snipe.LicenseSpec` per configured type, desired = device asset targets, calls `Reconcile`.

- [ ] **Step 1: Write the failing test** `licensesync/chrome_test.go`

```go
package licensesync

import (
	"testing"

	"github.com/sirupsen/logrus"
	admin "google.golang.org/api/admin/directory/v1"

	"github.com/CampusTech/google2snipe/config"
	"github.com/CampusTech/google2snipe/google"
)

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
```

Add the `mustJSONChrome` helper to `licensesync/chrome_test.go`:
```go
import "encoding/json"
func mustJSONChrome(t *testing.T, d *admin.ChromeOsDevice) []byte {
	t.Helper()
	b, err := json.Marshal([]*admin.ChromeOsDevice{d})
	if err != nil {
		t.Fatal(err)
	}
	return b
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./licensesync/ -run TestSyncChrome -v`
Expected: FAIL — `SyncChrome` undefined.

- [ ] **Step 3: Write `licensesync/chrome.go`**

```go
package licensesync

import (
	"time"

	"github.com/CampusTech/google2snipe/config"
	"github.com/CampusTech/google2snipe/google"
	"github.com/CampusTech/google2snipe/snipe"
)

// SyncChrome reconciles ChromeOS device upgrade licenses: one Snipe License per
// configured deviceLicenseType, a seat per device asset. Perpetual types are
// non-reassignable (additive); recurring types reconcile + expire.
func (e *Engine) SyncChrome(cfg config.LicensesConfig, devices []google.Device, assetIDBySerial func(string) (int, bool)) error {
	// group device assets by deviceLicenseType
	byType := map[string][]Target{}
	for _, d := range devices {
		lt := d.DeviceLicenseType
		if lt == "" || lt == "deviceLicenseTypeUnspecified" {
			continue
		}
		if _, configured := cfg.Chrome[lt]; !configured {
			continue
		}
		assetID, ok := assetIDBySerial(d.SerialNumber)
		if !ok {
			e.log.WithField("serial", d.SerialNumber).Debug("device not yet a Snipe asset; skipping license seat")
			continue
		}
		byType[lt] = append(byType[lt], Target{IsUser: false, ID: assetID})
	}
	for lt, targets := range byType {
		cc := cfg.Chrome[lt]
		reassignable := !config.ChromePerpetual(lt)
		if cc.Reassignable != nil {
			reassignable = *cc.Reassignable
		}
		spec := snipe.LicenseSpec{
			Name:         cc.Name,
			CostPerSeat:  cc.Cost,
			CategoryID:   cfg.DefaultLicenseCategoryID,
			Reassignable: reassignable,
			Seats:        len(targets),
		}
		if reassignable && cc.TermMonths > 0 {
			spec.ExpirationDate = time.Now().UTC().AddDate(0, cc.TermMonths, 0).Format("2006-01-02")
		}
		st, err := e.Reconcile(spec, targets)
		if err != nil {
			return err
		}
		e.log.WithField("license", cc.Name).WithField("checked_out", st.CheckedOut).
			WithField("checked_in", st.CheckedIn).Info("chrome license reconciled")
	}
	return nil
}
```

NOTE: `time.Now()` is fine here (this is the real CLI, not a workflow script).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./licensesync/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w . && go vet ./...
git add licensesync/chrome.go licensesync/chrome_test.go
git -c user.name="Robbie Trencheny" -c user.email="robbie@campus.edu" commit -m "feat(licensesync): ChromeOS upgrade license reconcile

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 7: `licenses` command group + `licenses sync` (Chrome only, for now)

**Files:**
- Create: `cmd/licenses.go`

**Interfaces:**
- Consumes: `config.Load`, `snipe.NewLicenseClient`, `licensesync.New`/`SyncChrome`, the existing `google.New`/device load + `snipe.New`/`GetAssetBySerial`.
- Produces: `licenses` parent command with a `sync` subcommand, registered on `rootCmd`.

- [ ] **Step 1: Write `cmd/licenses.go`**

```go
package cmd

import (
	"fmt"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/CampusTech/google2snipe/config"
	"github.com/CampusTech/google2snipe/google"
	"github.com/CampusTech/google2snipe/licensesync"
	"github.com/CampusTech/google2snipe/snipe"
)

var (
	licDryRun   bool
	licUseCache bool
	licLog      = logrus.New()
)

var licensesCmd = &cobra.Command{
	Use:   "licenses",
	Short: "Sync Google licenses into Snipe-IT as cost-bearing Licenses",
}

var licensesSyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Reconcile Google licenses into Snipe-IT license seats",
	RunE:  runLicensesSync,
}

func init() {
	RegisterLogger(licLog)
	licensesSyncCmd.Flags().BoolVar(&licDryRun, "dry-run", false, "simulate without mutating Snipe-IT")
	licensesSyncCmd.Flags().BoolVar(&licUseCache, "use-cache", false, "read devices/users from local cache")
	licensesCmd.AddCommand(licensesSyncCmd)
	rootCmd.AddCommand(licensesCmd)
}

func runLicensesSync(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return err
	}
	if !cfg.Licenses.Enabled {
		return fmt.Errorf("licenses.enabled is false; run 'google2snipe licenses setup' first")
	}
	cfg.Sync.DryRun = cfg.Sync.DryRun || licDryRun
	cfg.Sync.UseCache = cfg.Sync.UseCache || licUseCache

	// asset lookups via the existing go-snipeit-backed client
	sc, err := snipe.New(cfg.SnipeIT.URL, cfg.SnipeIT.APIKey, cfg.Sync.DryRun, cfg.Sync.RateLimit, snipeLog)
	if err != nil {
		return err
	}
	lc := snipe.NewLicenseClient(cfg.SnipeIT.URL, cfg.SnipeIT.APIKey, cfg.Sync.DryRun, licLog)
	engine := licensesync.New(lc, licLog)

	// devices (cache or fetch) — reuse the sync command's loader
	devs, err := loadDevices(cmd.Context(), cfg)
	if err != nil {
		return err
	}
	assetIDBySerial := func(serial string) (int, bool) {
		assets, err := sc.GetAssetBySerial(serial)
		if err != nil || len(assets) != 1 {
			return 0, false
		}
		return assets[0].ID, true
	}
	if err := engine.SyncChrome(cfg.Licenses, devs, assetIDBySerial); err != nil {
		return err
	}
	licLog.Warn("license sync complete")
	return nil
}
```

NOTE: `loadDevices` is defined in `cmd/sync.go` (Task 10 of the device plan). It's in the same `cmd` package, so it's reachable. `assetIDBySerial` does one Snipe lookup per device — acceptable; a future optimization could bulk-load assets.

- [ ] **Step 2: Verify build + help**

Run: `go build ./... && go run . licenses sync --help && go run . licenses --help`
Expected: build succeeds; help shows the `licenses` group and the `sync` subcommand with `--dry-run`/`--use-cache`.

- [ ] **Step 3: Commit**

```bash
gofmt -w . && go vet ./...
git add cmd/licenses.go
git -c user.name="Robbie Trencheny" -c user.email="robbie@campus.edu" commit -m "feat(cmd): licenses group + licenses sync (chrome)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 8: Google Licensing client (`apps.licensing`)

**Files:**
- Create: `google/licensing.go`
- Test: `google/licensing_test.go`

**Interfaces:**
- Consumes: `config.GoogleConfig` (credentials, impersonate subject), `config.WorkspaceLicenseConfig`.
- Produces: `google.NewLicensingClient(cfg config.GoogleConfig, customerID string, logger) (*LicensingClient, error)`; `(*LicensingClient) ListAssignments(ctx, products []string) ([]LicenseAssignment, error)` (paginated across products; skip a product on 403/404); the `LicenseAssignment` type. Cache helpers `SerializeAssignments`/`DeserializeAssignments`.

- [ ] **Step 1: Add the dependency + verify the scope/customer live (after the operator adds the DWD scope)**

```bash
go get google.golang.org/api/licensing/v1@latest
```
The Licensing API's `customerId` is the **domain or unique customer id**, NOT `my_customer`. Once `…/auth/apps.licensing` is authorized in DWD, confirm a call works (use the impersonated user's domain, e.g. derive from `impersonate_subject`):
```bash
# (pseudo — implement equivalently in Go) listForProduct Google-Apps for the customer domain.
```
If you cannot run it yet (scope not added), proceed with the httptest-based unit test below and leave the live check for the operator.

- [ ] **Step 2: Write the failing test** `google/licensing_test.go` (httptest, no real auth)

```go
package google

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sirupsen/logrus"
	licensing "google.golang.org/api/licensing/v1"
	"google.golang.org/api/option"
)

func TestListAssignmentsPaginates(t *testing.T) {
	page1 := `{"items":[{"userId":"a@x.edu","productId":"Google-Apps","skuId":"1010310008","skuName":"Education Plus"}],"nextPageToken":"tok"}`
	page2 := `{"items":[{"userId":"b@x.edu","productId":"Google-Apps","skuId":"1010310008","skuName":"Education Plus"}]}`
	mux := http.NewServeMux()
	mux.HandleFunc("/apps/licensing/v1/product/Google-Apps/users", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("pageToken") == "tok" {
			_, _ = w.Write([]byte(page2))
			return
		}
		_, _ = w.Write([]byte(page1))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	svc, err := licensing.NewService(context.Background(), option.WithoutAuthentication(), option.WithEndpoint(srv.URL+"/"))
	if err != nil {
		t.Fatal(err)
	}
	c := &LicensingClient{svc: svc, customerID: "x.edu", log: logrus.New()}
	got, err := c.ListAssignments(context.Background(), []string{"Google-Apps"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].UserEmail != "a@x.edu" || got[1].SKUID != "1010310008" {
		t.Fatalf("paging/parse failed: %+v", got)
	}
}
```

(Confirm the path `/apps/licensing/v1/product/{productId}/users` against the SDK's actual request path when you wire it; adjust the mux handler to match.)

- [ ] **Step 3: Run it to verify it fails**

Run: `go test ./google/ -run TestListAssignments -v`
Expected: FAIL — `LicensingClient` undefined.

- [ ] **Step 4: Write `google/licensing.go`**

```go
package google

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/sirupsen/logrus"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/googleapi"
	licensing "google.golang.org/api/licensing/v1"
	"google.golang.org/api/option"

	cfgpkg "github.com/CampusTech/google2snipe/config"
)

type LicenseAssignment struct {
	UserEmail string
	ProductID string
	SKUID     string
	SKUName   string
}

type LicensingClient struct {
	svc        *licensing.Service
	customerID string
	log        *logrus.Logger
}

// NewLicensingClient builds a licensing/v1 client via the SA + DWD, reusing the
// debug transport. customerID is the Workspace customer domain or unique id;
// "" derives the domain from the impersonation subject.
func NewLicensingClient(cfg cfgpkg.GoogleConfig, customerID string, logger *logrus.Logger) (*LicensingClient, error) {
	if logger == nil {
		logger = logrus.New()
	}
	if customerID == "" {
		if at := strings.LastIndex(cfg.ImpersonateSubject, "@"); at >= 0 {
			customerID = cfg.ImpersonateSubject[at+1:]
		}
	}
	keyData, err := os.ReadFile(cfg.CredentialsFile)
	if err != nil {
		return nil, fmt.Errorf("read credentials_file: %w", err)
	}
	jwtCfg, err := google.JWTConfigFromJSON(keyData, licensing.AppsLicensingScope)
	if err != nil {
		return nil, fmt.Errorf("parse service account key: %w", err)
	}
	jwtCfg.Subject = cfg.ImpersonateSubject
	ctx := context.Background()
	httpClient := jwtCfg.Client(ctx)
	httpClient.Transport = &debugTransport{base: httpClient.Transport, log: logger}
	svc, err := licensing.NewService(ctx, option.WithHTTPClient(httpClient))
	if err != nil {
		return nil, fmt.Errorf("create licensing service: %w", err)
	}
	return &LicensingClient{svc: svc, customerID: customerID, log: logger}, nil
}

// ListAssignments pages through every license assignment for each product,
// skipping products the customer isn't entitled to (403/404).
func (c *LicensingClient) ListAssignments(ctx context.Context, products []string) ([]LicenseAssignment, error) {
	var out []LicenseAssignment
	for _, product := range products {
		pageToken := ""
		for {
			call := c.svc.LicenseAssignments.ListForProduct(product, c.customerID).MaxResults(1000).Context(ctx)
			if pageToken != "" {
				call = call.PageToken(pageToken)
			}
			resp, err := call.Do()
			if err != nil {
				var gerr *googleapi.Error
				if errors.As(err, &gerr) && (gerr.Code == 403 || gerr.Code == 404) {
					c.log.WithField("product", product).Debug("skipping product (not entitled)")
					break
				}
				return nil, fmt.Errorf("list assignments for %s: %w", product, err)
			}
			for _, a := range resp.Items {
				out = append(out, LicenseAssignment{
					UserEmail: a.UserId, ProductID: a.ProductId, SKUID: a.SkuId, SKUName: a.SkuName,
				})
			}
			if resp.NextPageToken == "" {
				break
			}
			pageToken = resp.NextPageToken
		}
	}
	return out, nil
}

// SerializeAssignments / DeserializeAssignments cache the assignment list.
func SerializeAssignments(a []LicenseAssignment) ([]byte, error) { return json.MarshalIndent(a, "", "  ") }
func DeserializeAssignments(data []byte) ([]LicenseAssignment, error) {
	var a []LicenseAssignment
	return a, json.Unmarshal(data, &a)
}
```

Run `go get google.golang.org/api@latest` if `licensing` import is unresolved. The test constructs `LicensingClient` with unexported fields (`svc`, `customerID`, `log`) — keep those exact names.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./google/ -v`
Expected: PASS (licensing + existing google tests). Adjust the test's mux path to match the SDK's real request path if needed.

- [ ] **Step 6: Commit**

```bash
gofmt -w . && go vet ./... && go mod tidy
git add google/licensing.go google/licensing_test.go go.mod go.sum
git -c user.name="Robbie Trencheny" -c user.email="robbie@campus.edu" commit -m "feat(google): licensing/v1 client for Workspace license assignments

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 9: Workspace license reconcile + wire into `licenses sync`

**Files:**
- Create: `licensesync/workspace.go`
- Test: `licensesync/workspace_test.go`
- Modify: `cmd/licenses.go`

**Interfaces:**
- Consumes: `config.LicensesConfig`, `google.LicenseAssignment`, the Snipe user index (passed as `userIDByEmail func(string)(int,bool)`), `licensesync.Engine`.
- Produces: `(*Engine) SyncWorkspace(cfg config.LicensesConfig, assignments []google.LicenseAssignment, userIDByEmail func(string)(int,bool)) error` — one Snipe License per SKU (reassignable), seat per user, full reconcile.

- [ ] **Step 1: Write the failing test** `licensesync/workspace_test.go`

```go
package licensesync

import (
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/CampusTech/google2snipe/config"
	"github.com/CampusTech/google2snipe/google"
)

func TestSyncWorkspacePerSKUSeatPerUser(t *testing.T) {
	stub := &stubLC{}
	e := New(stub, logrus.New())
	cfg := config.LicensesConfig{
		Enabled: true, DefaultLicenseCategoryID: 7,
		Workspace: config.WorkspaceLicenseConfig{SKUCosts: map[string]float64{"1010310008": 5}},
	}
	asg := []google.LicenseAssignment{
		{UserEmail: "a@x.edu", SKUID: "1010310008", SKUName: "Education Plus"},
		{UserEmail: "b@x.edu", SKUID: "1010310008", SKUName: "Education Plus"},
	}
	uid := map[string]int{"a@x.edu": 10, "b@x.edu": 20}
	err := e.SyncWorkspace(cfg, asg, func(email string) (int, bool) { id, ok := uid[email]; return id, ok })
	if err != nil {
		t.Fatal(err)
	}
	users := map[int]bool{}
	for _, s := range stub.seats {
		if s.AssignedUserID != 0 {
			users[s.AssignedUserID] = true
		}
	}
	if !users[10] || !users[20] {
		t.Errorf("want users 10,20 seated; got %v", users)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./licensesync/ -run TestSyncWorkspace -v`
Expected: FAIL — `SyncWorkspace` undefined.

- [ ] **Step 3: Write `licensesync/workspace.go`**

```go
package licensesync

import (
	"github.com/CampusTech/google2snipe/config"
	"github.com/CampusTech/google2snipe/google"
	"github.com/CampusTech/google2snipe/snipe"
)

// SyncWorkspace reconciles Workspace user subscriptions: one Snipe License per
// SKU (reassignable), a seat per assigned user.
func (e *Engine) SyncWorkspace(cfg config.LicensesConfig, assignments []google.LicenseAssignment, userIDByEmail func(string) (int, bool)) error {
	type skuInfo struct {
		name    string
		targets []Target
	}
	bySKU := map[string]*skuInfo{}
	for _, a := range assignments {
		uid, ok := userIDByEmail(a.UserEmail)
		if !ok {
			e.log.WithField("email", a.UserEmail).Debug("no Snipe user; skipping license seat")
			continue
		}
		si := bySKU[a.SKUID]
		if si == nil {
			si = &skuInfo{name: a.SKUName}
			bySKU[a.SKUID] = si
		}
		if si.name == "" {
			si.name = a.SKUName
		}
		si.targets = append(si.targets, Target{IsUser: true, ID: uid})
	}
	for skuID, si := range bySKU {
		name := si.name
		if name == "" {
			name = "Workspace SKU " + skuID
		}
		spec := snipe.LicenseSpec{
			Name:         name,
			CostPerSeat:  cfg.Workspace.SKUCosts[skuID], // 0 if unmapped
			CategoryID:   cfg.DefaultLicenseCategoryID,
			Reassignable: true,
			Seats:        len(si.targets),
		}
		st, err := e.Reconcile(spec, si.targets)
		if err != nil {
			return err
		}
		e.log.WithField("license", name).WithField("checked_out", st.CheckedOut).
			WithField("checked_in", st.CheckedIn).Info("workspace license reconciled")
	}
	return nil
}
```

- [ ] **Step 4: Wire into `cmd/licenses.go runLicensesSync`** — after `SyncChrome`, add:

```go
	if len(cfg.Licenses.Workspace.Products) > 0 {
		gl, err := google.NewLicensingClient(cfg.Google, cfg.Licenses.Workspace.CustomerID, licLog)
		if err != nil {
			return err
		}
		asg, err := loadAssignments(cmd.Context(), cfg, gl)
		if err != nil {
			return err
		}
		// reuse the sync engine's Warm user index via a fresh engine, or load users directly:
		users, err := newCachingSnipe(sc, cfg.Sync.UseCache, cfg.Sync.CacheDir, snipeLog).ListAllUsers()
		if err != nil {
			return err
		}
		idx := map[string]int{}
		for _, u := range users {
			if u.Email != "" {
				idx[strings.ToLower(u.Email)] = u.ID
			}
		}
		userIDByEmail := func(email string) (int, bool) {
			id, ok := idx[strings.ToLower(email)]
			if !ok {
				if i := strings.IndexByte(strings.ToLower(email), '@'); i > 0 {
					id, ok = idx[strings.ToLower(email)[:i]]
				}
			}
			return id, ok
		}
		if err := engine.SyncWorkspace(cfg.Licenses, asg, userIDByEmail); err != nil {
			return err
		}
	}
```

Add a `loadAssignments` helper in `cmd/licenses.go`:
```go
func loadAssignments(ctx context.Context, cfg *config.Config, gl *google.LicensingClient) ([]google.LicenseAssignment, error) {
	path := filepath.Join(cfg.Sync.CacheDir, "license_assignments.json")
	if cfg.Sync.UseCache {
		if data, err := os.ReadFile(path); err == nil {
			return google.DeserializeAssignments(data)
		}
	}
	asg, err := gl.ListAssignments(ctx, cfg.Licenses.Workspace.Products)
	if err != nil {
		return nil, err
	}
	if data, err := google.SerializeAssignments(asg); err == nil {
		_ = os.MkdirAll(cfg.Sync.CacheDir, 0o755)
		_ = os.WriteFile(path, data, 0o644)
	}
	return asg, nil
}
```
Add imports `context`, `os`, `path/filepath`, `strings` to `cmd/licenses.go`.

- [ ] **Step 5: Run tests + build**

Run: `go test ./licensesync/... && go build ./... && go vet ./...`
Expected: PASS + clean.

- [ ] **Step 6: Commit**

```bash
gofmt -w . && go mod tidy
git add licensesync/workspace.go licensesync/workspace_test.go cmd/licenses.go
git -c user.name="Robbie Trencheny" -c user.email="robbie@campus.edu" commit -m "feat(licensesync): Workspace license reconcile + wire into licenses sync

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 10: `licenses setup` — discovery, quiz, config write

**Files:**
- Create: `cmd/license_setup.go`
- Create: `config/licenses_merge.go`
- Test: `config/licenses_merge_test.go`

**Interfaces:**
- Consumes: device load (Chrome types), `google.NewLicensingClient`/`ListAssignments` (Workspace SKUs), `config.ChromePerpetual`, the Snipe license client (optional category create).
- Produces: `licenses setup` subcommand; `config.MergeLicenses(path string, lic config.LicensesConfig) error` (writes/merges the `licenses:` block, preserving comments).

- [ ] **Step 1: Write the failing test for the config writer** `config/licenses_merge_test.go`

```go
package config

import (
	"os"
	"testing"
)

func TestMergeLicensesWritesBlock(t *testing.T) {
	p := writeTemp(t, "sync:\n  set_name: false\n")
	in := LicensesConfig{
		Enabled:                  true,
		DefaultLicenseCategoryID: 7,
		Chrome: map[string]ChromeLicenseConfig{
			"educationUpgradePerpetual": {Name: "Chrome EDU Perpetual", Cost: 38},
		},
		Workspace: WorkspaceLicenseConfig{
			Products: []string{"Google-Apps"},
			SKUCosts: map[string]float64{"1010310008": 5},
		},
	}
	if err := MergeLicenses(p, in); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	var c Config
	if err := yamlUnmarshal(data, &c); err != nil {
		t.Fatalf("reload: %v\n%s", err, data)
	}
	if !c.Licenses.Enabled || c.Licenses.DefaultLicenseCategoryID != 7 {
		t.Errorf("licenses block missing:\n%s", data)
	}
	if c.Licenses.Chrome["educationUpgradePerpetual"].Cost != 38 {
		t.Errorf("chrome cost missing:\n%s", data)
	}
	if c.Licenses.Workspace.SKUCosts["1010310008"] != 5 {
		t.Errorf("workspace sku cost missing:\n%s", data)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./config/ -run TestMergeLicenses -v`
Expected: FAIL — `MergeLicenses` undefined.

- [ ] **Step 3: Implement `config/licenses_merge.go`**

Write `MergeLicenses(path string, lic LicensesConfig) error` using `yaml.Node`: load the file, find or create the top-level `licenses` key, and set its value node by marshaling `lic` to a `yaml.Node` (`var n yaml.Node; n.Encode(lic)`), then write the file back preserving other content/comments. (Mirror the node-manipulation approach already used by `MergeFieldMapping` in `config/merge.go` — load doc, find/replace the mapping key, `yaml.Marshal` back. Reuse `yamlUnmarshal` for the test.)

- [ ] **Step 4: Run the writer test**

Run: `go test ./config/ -v`
Expected: PASS.

- [ ] **Step 5: Implement `cmd/license_setup.go`** (interactive)

```go
package cmd

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/CampusTech/google2snipe/config"
	"github.com/CampusTech/google2snipe/google"
)

// candidateProducts is the known Licensing-API product catalog probed at setup.
var candidateProducts = []string{
	"Google-Apps", "101031", "101034", "101037", "101047", "101001", "101005",
	"Google-Vault", "101033", "101038", "101054", "101039", "101040", "101035", "101052",
}

var licensesSetupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Discover license types in use and quiz for per-seat costs, then write config",
	RunE:  runLicensesSetup,
}

func init() { licensesCmd.AddCommand(licensesSetupCmd) }

func runLicensesSetup(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		// Load validates; allow setup to run even if licenses not yet enabled by
		// loading laxly. If Load fails ONLY due to the licenses category guard,
		// proceed; otherwise return. (Implementer: add a Load variant or set a
		// temporary category so Load passes; simplest is to require the operator
		// to have the other required fields set, which they do post device-setup.)
		return err
	}
	out := config.LicensesConfig{
		Enabled:                  true,
		DefaultLicenseCategoryID: cfg.Licenses.DefaultLicenseCategoryID,
		Chrome:                   map[string]config.ChromeLicenseConfig{},
		Workspace:                config.WorkspaceLicenseConfig{SKUCosts: map[string]float64{}},
	}
	in := bufio.NewReader(os.Stdin)
	askCost := func(label string) float64 {
		fmt.Printf("  %s\n    cost per seat (USD, blank=0): ", label)
		line, _ := in.ReadString('\n')
		v, _ := strconv.ParseFloat(strings.TrimSpace(line), 64)
		return v
	}

	// 1) license category id
	if out.DefaultLicenseCategoryID == 0 {
		fmt.Print("Snipe-IT license category id (default_license_category_id): ")
		line, _ := in.ReadString('\n')
		out.DefaultLicenseCategoryID, _ = strconv.Atoi(strings.TrimSpace(line))
	}

	// 2) Chrome upgrade types from devices
	devs, err := loadDevices(cmd.Context(), cfg)
	if err != nil {
		return err
	}
	chromeCounts := map[string]int{}
	for _, d := range devs {
		if d.DeviceLicenseType != "" && d.DeviceLicenseType != "deviceLicenseTypeUnspecified" {
			chromeCounts[d.DeviceLicenseType]++
		}
	}
	for _, t := range sortedKeys(chromeCounts) {
		kind := "recurring"
		if config.ChromePerpetual(t) {
			kind = "perpetual"
		}
		name := fmt.Sprintf("Chrome Upgrade (%s)", t)
		cost := askCost(fmt.Sprintf("%s  [%s · %d devices · %s]", name, kind, chromeCounts[t], t))
		out.Chrome[t] = config.ChromeLicenseConfig{Name: name, Cost: cost}
	}

	// 3) Workspace SKUs from the Licensing API
	gl, err := google.NewLicensingClient(cfg.Google, cfg.Licenses.Workspace.CustomerID, licLog)
	if err != nil {
		return err
	}
	asg, err := gl.ListAssignments(cmd.Context(), candidateProducts)
	if err != nil {
		return err
	}
	type sk struct{ name, product string; count int }
	skus := map[string]*sk{}
	prodSet := map[string]bool{}
	for _, a := range asg {
		prodSet[a.ProductID] = true
		s := skus[a.SKUID]
		if s == nil {
			s = &sk{name: a.SKUName, product: a.ProductID}
			skus[a.SKUID] = s
		}
		s.count++
	}
	for _, id := range sortedKeys2(skus) {
		s := skus[id]
		cost := askCost(fmt.Sprintf("%s  [license · %d users · SKU %s]", s.name, s.count, id))
		out.Workspace.SKUCosts[id] = cost
	}
	for p := range prodSet {
		out.Workspace.Products = append(out.Workspace.Products, p)
	}
	sort.Strings(out.Workspace.Products)

	// 4) write config
	if err := config.MergeLicenses(cfgFile, out); err != nil {
		return err
	}
	fmt.Printf("\nWrote licenses config: %d Chrome type(s), %d Workspace SKU(s) into %s\n",
		len(out.Chrome), len(out.Workspace.SKUCosts), cfgFile)
	return nil
}

func sortedKeys(m map[string]int) []string {
	var k []string
	for s := range m {
		k = append(k, s)
	}
	sort.Strings(k)
	return k
}
```

Add `sortedKeys2` for the `map[string]*sk` (same shape). NOTE: `licenses setup` reads from stdin, so it's interactive (run in a real terminal). If `Load` rejects because `licenses.enabled` is set but the category is unset, handle by treating setup as the bootstrap that fills it — the implementer should make `Load` tolerant for `setup` (e.g. a `LoadForSetup` that skips the licenses-category guard) OR document that the operator sets `licenses: {enabled: true, default_license_category_id: N}` first. Pick the `LoadForSetup` variant (skip only the licenses-category validation) and use it here.

- [ ] **Step 6: Build + help**

Run: `go build ./... && go run . licenses setup --help`
Expected: build succeeds; help prints. (Full interactive run requires the `apps.licensing` scope + a terminal.)

- [ ] **Step 7: Commit**

```bash
gofmt -w . && go vet ./...
git add cmd/license_setup.go config/licenses_merge.go config/licenses_merge_test.go
git -c user.name="Robbie Trencheny" -c user.email="robbie@campus.edu" commit -m "feat(cmd): licenses setup — discover + quiz + write config

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 11: Docs — settings.example.yaml + README

**Files:**
- Modify: `settings.example.yaml`
- Modify: `README.md`

**Interfaces:** none (docs).

- [ ] **Step 1: Add a `licenses:` block to `settings.example.yaml`**

```yaml
# Optional: license cost sync (run `google2snipe licenses setup` to populate).
licenses:
  enabled: false
  default_license_category_id: 0        # REQUIRED when enabled (Snipe license category)
  chrome:                               # keyed by deviceLicenseType; perpetual => non-reassignable
    educationUpgradePerpetual:
      name: "Chrome Education Upgrade (Perpetual)"
      cost: 0.00                        # one-time per device/seat
  workspace:
    customer_id: ""                     # Workspace domain or customer id ("" => derive from impersonate_subject)
    products: []                        # filled by `licenses setup`
    sku_costs: {}                       # SKU -> per-seat $/yr
```

- [ ] **Step 2: Add a "License cost sync" section to `README.md`**

Document: the goal (cost per user), the new DWD scope `…/auth/apps.licensing`, the license-category prereq, `google2snipe licenses setup` (interactive discovery + price quiz), `google2snipe licenses sync` (`--dry-run`/`--use-cache`), the perpetual-vs-recurring modeling (`reassignable`), and that perpetual upgrades are additive while recurring/Workspace reconcile.

- [ ] **Step 3: Final verification**

Run: `go build ./... && go test ./... && go vet ./... && test -z "$(gofmt -l .)"`
Expected: clean build, all tests pass, gofmt clean.

- [ ] **Step 4: Commit**

```bash
git add settings.example.yaml README.md
git -c user.name="Robbie Trencheny" -c user.email="robbie@campus.edu" commit -m "docs: license cost sync — example config + README

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
git push origin main
```

---

## Self-Review Notes (author checklist — completed)

**Spec coverage:**
- Workspace user licenses → License, seat per user, reconcile → Tasks 8, 9. ✓
- Chrome upgrade → License, seat per device; perpetual reassignable=false additive, recurring reconcile + expiry → Tasks 5, 6. ✓
- Taxonomy/classification (perpetual vs recurring) → Task 1 (`ChromePerpetual`). ✓
- Snipe Licenses + seats hand-rolled (go-snipeit gap) → Tasks 2–4. ✓
- Cost from config, per-seat, unmapped→0 → Tasks 1, 6, 9. ✓
- `licenses setup` (discover + quiz + write) → Task 10. ✓
- `licenses sync` reconcile (`--dry-run`/`--use-cache`) → Tasks 7, 9. ✓
- Reconcile semantics (recurring check-in, perpetual additive) → Task 5 (`Reconcile`), tested. ✓
- apps.licensing scope, license-category prereq → Tasks 8, 1, 11. ✓
- Caching (devices/users/license_assignments) → Tasks 7, 9. ✓
- Matching (email→user, serial→asset) → Tasks 6, 9. ✓

**Type consistency:** `LicenseClient` seat methods take `(licenseID, seatID, ...)` consistently in the snipe client (Task 4), the licensesync interface + stub (Task 5), and the engine calls (Task 5). `snipe.LicenseSpec/License/LicenseSeat` and `google.LicenseAssignment` used identically across tasks. `config.ChromePerpetual` used in Tasks 6 and 10.

**Known live-verification points (flagged in-task):** the Snipe licenses/seats JSON shapes (Tasks 2–4 lead with curl verification — adjust field keys to the instance) and the Licensing-API request path + `customerId` semantics (Task 8 — verify once the `apps.licensing` scope is authorized). These are external-API contracts that must be confirmed against the live services during implementation.
