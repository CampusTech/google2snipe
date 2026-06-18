# License OU Scoping Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Restrict the license sync to Google Workspace users and ChromeOS devices under configured Org Units (and their sub-OUs), defaulting to the global `org_unit_path` and overridable per the license sync.

**Architecture:** A pure `config.InScope` matcher + `config.EffectiveLicenseScopes` resolver decide the scope. A new `google.Client.ListAllUsers` (cached) supplies the `email → orgUnitPath` map the Workspace path needs (license assignments carry no OU). Filtering happens in `cmd/licenses.go` on the already-loaded `assignments` and `devs`, so `SyncWorkspace`/`SyncChrome`/`Reconcile` stay OU-agnostic.

**Tech Stack:** Go 1.26, `google.golang.org/api/admin/directory/v1` (Admin SDK), logrus, gopkg.in/yaml.v3, `go test` (stdlib testing).

## Global Constraints

- Go 1.26; format with `gofmt`; `golangci-lint run` clean (the pre-existing `snipe/client.go` QF1008 about `sa.User.User.ID` is out of scope — leave it); `go test -race ./...` green.
- Test strings use `example.com` only — never `campus.edu` / `campusgroup.co`.
- Commit author `Robbie Trencheny <robbie@campus.edu>` via `git -c user.name="Robbie Trencheny" -c user.email="robbie@campus.edu" commit`; commit-message body ends with the trailer line `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.
- Backward compatible: with no `licenses.org_unit_paths` and no global `google.org_unit_path`, `EffectiveLicenseScopes` returns `nil` → no user fetch, no filtering — behavior identical to today.
- `Client.ListAllUsers` needs the `https://www.googleapis.com/auth/admin.directory.user.readonly` OAuth scope, granted both in `google.scopes` config and via domain-wide delegation in the Google Admin console. Document this; do not silently default it.
- Branch: `feat/license-ou-scoping` (already off `main`; the design spec is at `docs/superpowers/specs/2026-06-18-license-ou-scoping-design.md`).

---

## File Structure

- **Create `config/scoping.go`** — `EffectiveLicenseScopes(*Config) []string` and `InScope(ou string, scopes []string) bool`. Pure, no deps beyond `strings`.
- **Create `config/scoping_test.go`** — table tests for both.
- **Modify `config/config.go`** — add `OrgUnitPaths []string` to `LicensesConfig`.
- **Create `google/users.go`** — `User{Email, OrgUnitPath}`, `userFromAdmin`, `Client.ListAllUsers(ctx)`, `SerializeUsers`/`DeserializeUsers`.
- **Create `google/users_test.go`** — `userFromAdmin` mapping + serialize round-trip.
- **Modify `cmd/licenses.go`** — `loadUsers` helper; pure `inScopeAssignments`/`inScopeDevices`; wire scope resolution + filtering into the Chrome and Workspace blocks.
- **Create `cmd/licenses_ou_test.go`** — tests for `inScopeAssignments`/`inScopeDevices` (introduces the first `cmd` test file).
- **Modify `README.md`** — document `licenses.org_unit_paths` and the user-readonly scope prerequisite.

---

### Task 1: Config scope resolver + matcher

**Files:**
- Create: `config/scoping.go`
- Create: `config/scoping_test.go`
- Modify: `config/config.go` (the `LicensesConfig` struct — add `OrgUnitPaths`)

**Interfaces:**
- Consumes: `config.Config` (`cfg.Licenses.OrgUnitPaths`, `cfg.Google.OrgUnitPath`).
- Produces: `config.EffectiveLicenseScopes(cfg *Config) []string`; `config.InScope(ou string, scopes []string) bool`. Later tasks call both.

- [ ] **Step 1: Add the config field**

In `config/config.go`, the `LicensesConfig` struct currently is:

```go
type LicensesConfig struct {
	Enabled                  bool                           `yaml:"enabled"`
	DefaultLicenseCategoryID int                            `yaml:"default_license_category_id"`
	Chrome                   map[string]ChromeLicenseConfig `yaml:"chrome"`
	Workspace                WorkspaceLicenseConfig         `yaml:"workspace"`
}
```

Add the `OrgUnitPaths` field:

```go
type LicensesConfig struct {
	Enabled                  bool                           `yaml:"enabled"`
	DefaultLicenseCategoryID int                            `yaml:"default_license_category_id"`
	OrgUnitPaths             []string                       `yaml:"org_unit_paths"`
	Chrome                   map[string]ChromeLicenseConfig `yaml:"chrome"`
	Workspace                WorkspaceLicenseConfig         `yaml:"workspace"`
}
```

- [ ] **Step 2: Write the failing tests** (`config/scoping_test.go`)

```go
package config

import (
	"reflect"
	"testing"
)

func TestInScope(t *testing.T) {
	cases := []struct {
		name   string
		ou     string
		scopes []string
		want   bool
	}{
		{"empty scopes means no filter", "/Anything", nil, true},
		{"exact match", "/Students", []string{"/Students"}, true},
		{"one level under", "/Students/HS", []string{"/Students"}, true},
		{"deep multi-level with space", "/Students/Online/Fall 2024", []string{"/Students"}, true},
		{"sibling prefix is not a descendant", "/StudentsClub", []string{"/Students"}, false},
		{"unrelated ou", "/Faculty", []string{"/Students"}, false},
		{"second scope matches", "/Faculty/Adjuncts", []string{"/Students", "/Faculty"}, true},
		{"root scope matches all", "/Students/HS", []string{"/"}, true},
		{"empty ou not in a real scope", "", []string{"/Students"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := InScope(c.ou, c.scopes); got != c.want {
				t.Fatalf("InScope(%q, %v) = %v, want %v", c.ou, c.scopes, got, c.want)
			}
		})
	}
}

func TestEffectiveLicenseScopes(t *testing.T) {
	override := &Config{Licenses: LicensesConfig{OrgUnitPaths: []string{"/Students", "/Faculty"}}, Google: GoogleConfig{OrgUnitPath: "/Everyone"}}
	if got := EffectiveLicenseScopes(override); !reflect.DeepEqual(got, []string{"/Students", "/Faculty"}) {
		t.Fatalf("override: got %v", got)
	}
	fallback := &Config{Google: GoogleConfig{OrgUnitPath: "/Students"}}
	if got := EffectiveLicenseScopes(fallback); !reflect.DeepEqual(got, []string{"/Students"}) {
		t.Fatalf("fallback: got %v", got)
	}
	none := &Config{}
	if got := EffectiveLicenseScopes(none); got != nil {
		t.Fatalf("none: got %v, want nil", got)
	}
}
```

- [ ] **Step 3: Run the tests to confirm they fail**

Run: `go test ./config/ -run 'TestInScope|TestEffectiveLicenseScopes'`
Expected: FAIL — `undefined: InScope` / `undefined: EffectiveLicenseScopes`.

- [ ] **Step 4: Implement** (`config/scoping.go`)

```go
package config

import "strings"

// EffectiveLicenseScopes resolves the Org Unit paths that scope the license sync:
// the licenses-specific list if set, otherwise the global org_unit_path (as a
// single-element list), otherwise nil (no OU filtering).
func EffectiveLicenseScopes(cfg *Config) []string {
	if len(cfg.Licenses.OrgUnitPaths) > 0 {
		return cfg.Licenses.OrgUnitPaths
	}
	if cfg.Google.OrgUnitPath != "" {
		return []string{cfg.Google.OrgUnitPath}
	}
	return nil
}

// InScope reports whether the org unit path ou falls under any of scopes. An empty
// scopes list means no filtering (everything is in scope). A scope matches its own
// OU exactly and any descendant on a path-segment boundary, so "/Students" matches
// "/Students" and "/Students/Online/Fall 2024" but not "/StudentsClub". The root
// scope "/" matches every OU; to scope to everything else, leave the list empty.
func InScope(ou string, scopes []string) bool {
	if len(scopes) == 0 {
		return true
	}
	for _, s := range scopes {
		if s == "/" || ou == s || strings.HasPrefix(ou, s+"/") {
			return true
		}
	}
	return false
}
```

- [ ] **Step 5: Run the tests to confirm they pass**

Run: `go test ./config/ -run 'TestInScope|TestEffectiveLicenseScopes' -v`
Expected: PASS (all sub-tests).

- [ ] **Step 6: Commit**

```bash
gofmt -w config/scoping.go config/scoping_test.go config/config.go
go build ./... && go test ./config/...
git add config/scoping.go config/scoping_test.go config/config.go
git -c user.name="Robbie Trencheny" -c user.email="robbie@campus.edu" commit -m "feat(config): add license OU scope resolver and matcher

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: Google Directory users source

**Files:**
- Create: `google/users.go`
- Create: `google/users_test.go`

**Interfaces:**
- Consumes: existing `google.Client` (`c.svc *admin.Service`, `c.customerID`, `c.log`); `admin "google.golang.org/api/admin/directory/v1"`.
- Produces: `google.User{Email, OrgUnitPath string}`; `(*Client).ListAllUsers(ctx context.Context) ([]User, error)`; `google.SerializeUsers([]User) ([]byte, error)`; `google.DeserializeUsers([]byte) ([]User, error)`. Task 3 uses all four.

- [ ] **Step 1: Write the failing tests** (`google/users_test.go`)

```go
package google

import (
	"reflect"
	"testing"

	admin "google.golang.org/api/admin/directory/v1"
)

func TestUserFromAdmin(t *testing.T) {
	got := userFromAdmin(&admin.User{PrimaryEmail: "alice@example.com", OrgUnitPath: "/Students/HS"})
	want := User{Email: "alice@example.com", OrgUnitPath: "/Students/HS"}
	if got != want {
		t.Fatalf("userFromAdmin = %+v, want %+v", got, want)
	}
}

func TestSerializeDeserializeUsersRoundTrip(t *testing.T) {
	in := []User{
		{Email: "a@example.com", OrgUnitPath: "/Students"},
		{Email: "b@example.com", OrgUnitPath: "/Faculty/Adjuncts"},
	}
	data, err := SerializeUsers(in)
	if err != nil {
		t.Fatalf("SerializeUsers: %v", err)
	}
	out, err := DeserializeUsers(data)
	if err != nil {
		t.Fatalf("DeserializeUsers: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round-trip = %+v, want %+v", out, in)
	}
}
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test ./google/ -run 'TestUserFromAdmin|TestSerializeDeserializeUsersRoundTrip'`
Expected: FAIL — `undefined: userFromAdmin` / `undefined: User` / `undefined: SerializeUsers`.

- [ ] **Step 3: Implement** (`google/users.go`)

```go
package google

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sirupsen/logrus"
	admin "google.golang.org/api/admin/directory/v1"
)

// User is a Directory user reduced to the fields the license OU filter needs.
type User struct {
	Email       string `json:"email"`
	OrgUnitPath string `json:"org_unit_path"`
}

// userFromAdmin reduces an Admin SDK user to the fields we keep.
func userFromAdmin(u *admin.User) User {
	return User{Email: u.PrimaryEmail, OrgUnitPath: u.OrgUnitPath}
}

// ListAllUsers pages through every Directory user for the customer, returning each
// user's primary email and org unit path. It requests only those two fields and
// includes suspended users (a suspended account can still hold a license). Requires
// the admin.directory.user.readonly scope (config google.scopes + DWD grant).
func (c *Client) ListAllUsers(ctx context.Context) ([]User, error) {
	customer := c.customerID
	if customer == "" {
		customer = "my_customer"
	}
	var out []User
	pageToken := ""
	for {
		call := c.svc.Users.List().
			Customer(customer).
			MaxResults(500).
			Fields("nextPageToken,users(primaryEmail,orgUnitPath)").
			Context(ctx)
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		resp, err := call.Do()
		if err != nil {
			return nil, fmt.Errorf("list directory users (needs admin.directory.user.readonly scope): %w", err)
		}
		for _, u := range resp.Users {
			out = append(out, userFromAdmin(u))
		}
		c.log.WithFields(logrus.Fields{"page": len(resp.Users), "total": len(out)}).Debug("listed directory users page")
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}
	return out, nil
}

// SerializeUsers marshals users to indented JSON for caching.
func SerializeUsers(users []User) ([]byte, error) {
	return json.MarshalIndent(users, "", "  ")
}

// DeserializeUsers reads cached JSON back into users.
func DeserializeUsers(data []byte) ([]User, error) {
	var users []User
	return users, json.Unmarshal(data, &users)
}
```

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `go build ./google/ && go test ./google/ -run 'TestUserFromAdmin|TestSerializeDeserializeUsersRoundTrip' -v`
Expected: PASS. (`ListAllUsers`'s live pagination has no unit test — it mirrors the established, untested `ListAllChromeOSDevices` pattern; its field mapping is covered by `TestUserFromAdmin`.)

- [ ] **Step 5: Commit**

```bash
gofmt -w google/users.go google/users_test.go
go build ./... && go test ./google/...
git add google/users.go google/users_test.go
git -c user.name="Robbie Trencheny" -c user.email="robbie@campus.edu" commit -m "feat(google): list Directory users with org unit paths (cached)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: Wire OU filtering into the license command

**Files:**
- Modify: `cmd/licenses.go` (add `loadUsers`, `inScopeAssignments`, `inScopeDevices`; filter in the Chrome block ~lines 66-94 and the Workspace block ~lines 96-145)
- Create: `cmd/licenses_ou_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `config.EffectiveLicenseScopes`, `config.InScope` (Task 1); `google.User`, `(*Client).ListAllUsers`, `google.SerializeUsers`, `google.DeserializeUsers` (Task 2); existing `google.LicenseAssignment{UserEmail}`, `google.Device` (embeds `*admin.ChromeOsDevice` with `OrgUnitPath`), the existing `loadDevices`/`loadAssignments` cache pattern.
- Produces: filtered `assignments`/`devs` passed to the unchanged `SyncChrome`/`SyncWorkspace`.

- [ ] **Step 1: Write the failing tests** (`cmd/licenses_ou_test.go`)

```go
package cmd

import (
	"testing"

	admin "google.golang.org/api/admin/directory/v1"

	"github.com/CampusTech/google2snipe/google"
)

func dev(serial, ou string) google.Device {
	return google.Device{ChromeOsDevice: &admin.ChromeOsDevice{SerialNumber: serial, OrgUnitPath: ou}}
}

func TestInScopeDevices(t *testing.T) {
	devs := []google.Device{
		dev("A", "/Students"),
		dev("B", "/Students/HS"),
		dev("C", "/Faculty"),
		dev("D", ""),
	}
	got := inScopeDevices(devs, []string{"/Students"})
	if len(got) != 2 || got[0].SerialNumber != "A" || got[1].SerialNumber != "B" {
		t.Fatalf("kept %d devices: %+v, want A and B", len(got), serials(got))
	}
	// Empty scopes keep everything.
	if len(inScopeDevices(devs, nil)) != 4 {
		t.Fatalf("empty scopes should keep all 4")
	}
}

func serials(devs []google.Device) []string {
	out := make([]string, len(devs))
	for i, d := range devs {
		out[i] = d.SerialNumber
	}
	return out
}

func TestInScopeAssignments(t *testing.T) {
	asg := []google.LicenseAssignment{
		{UserEmail: "alice@example.com"},   // /Students -> kept
		{UserEmail: "BOB@example.com"},     // /Faculty -> dropped (case-insensitive lookup)
		{UserEmail: "ghost@example.com"},   // absent from map -> dropped
	}
	ouByEmail := map[string]string{
		"alice@example.com": "/Students/Online",
		"bob@example.com":   "/Faculty",
	}
	got := inScopeAssignments(asg, ouByEmail, []string{"/Students"})
	if len(got) != 1 || got[0].UserEmail != "alice@example.com" {
		t.Fatalf("kept %d assignments: %+v, want only alice", len(got), got)
	}
	// Empty scopes keep everything.
	if len(inScopeAssignments(asg, ouByEmail, nil)) != 3 {
		t.Fatalf("empty scopes should keep all 3")
	}
}
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test ./cmd/ -run 'TestInScopeDevices|TestInScopeAssignments'`
Expected: FAIL — `undefined: inScopeDevices` / `undefined: inScopeAssignments`.

- [ ] **Step 3: Add the pure filter helpers** (in `cmd/licenses.go`, after `runLicensesSync`)

```go
// inScopeDevices keeps only devices whose OrgUnitPath falls under scopes. Empty
// scopes keep everything (config.InScope handles that).
func inScopeDevices(devs []google.Device, scopes []string) []google.Device {
	out := devs[:0:0]
	for _, d := range devs {
		ou := ""
		if d.ChromeOsDevice != nil {
			ou = d.OrgUnitPath
		}
		if config.InScope(ou, scopes) {
			out = append(out, d)
		}
	}
	return out
}

// inScopeAssignments keeps only assignments whose user's OU falls under scopes.
// ouByEmail is keyed by lowercased email; an email absent from it (alias-only,
// deleted, or external) resolves to "" and is dropped unless scopes is empty.
func inScopeAssignments(asg []google.LicenseAssignment, ouByEmail map[string]string, scopes []string) []google.LicenseAssignment {
	out := asg[:0:0]
	for _, a := range asg {
		if config.InScope(ouByEmail[strings.ToLower(a.UserEmail)], scopes) {
			out = append(out, a)
		}
	}
	return out
}
```

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `go test ./cmd/ -run 'TestInScopeDevices|TestInScopeAssignments' -v`
Expected: PASS.

- [ ] **Step 5: Add the `loadUsers` cache helper** (in `cmd/licenses.go`, after `loadAssignments`)

```go
// loadUsers returns Directory users (email + org unit path) for OU filtering,
// from cache when sync.use_cache is set, else fetched and cached. Mirrors
// loadAssignments. Only called when OU filtering is active.
func loadUsers(ctx context.Context, cfg *config.Config, logger *logrus.Logger) ([]google.User, error) {
	path := filepath.Join(cfg.Sync.CacheDir, "workspace_users.json")
	if cfg.Sync.UseCache {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read cache: %w", err)
		}
		return google.DeserializeUsers(data)
	}
	gc, err := google.New(cfg.Google, logger)
	if err != nil {
		return nil, err
	}
	users, err := gc.ListAllUsers(ctx)
	if err != nil {
		return nil, err
	}
	if data, err := google.SerializeUsers(users); err == nil {
		_ = os.MkdirAll(cfg.Sync.CacheDir, 0o755)
		_ = os.WriteFile(path, data, 0o644)
	}
	return users, nil
}
```

(If `os`, `filepath`, `fmt`, `logrus`, `strings`, `config`, `google` are not already imported in `cmd/licenses.go`, they will be after the wiring below; run `goimports`/`gofmt` and let `go build` confirm.)

- [ ] **Step 6: Resolve scopes once and filter the Chrome block**

In `runLicensesSync`, immediately after `engine := licensesync.New(...)` (currently line 61), add:

```go
	scopes := config.EffectiveLicenseScopes(cfg)
```

Inside the Chrome block, right after `devs, err := loadDevices(cmd.Context(), cfg)` returns (currently ~line 70, before the `allAssets` fetch), add:

```go
		if len(scopes) > 0 {
			before := len(devs)
			devs = inScopeDevices(devs, scopes)
			licLog.WithFields(logrus.Fields{"scopes": scopes, "kept": len(devs), "of": before}).
				Info("OU-scoped Chrome devices")
		}
```

- [ ] **Step 7: Filter the Workspace block**

Inside the Workspace block, right after `asg, err := loadAssignments(cmd.Context(), cfg, licLog)` returns (currently ~line 100, before the `users` fetch), add:

```go
		if len(scopes) > 0 {
			wsUsers, uerr := loadUsers(cmd.Context(), cfg, googleLog)
			if uerr != nil {
				return uerr
			}
			ouByEmail := make(map[string]string, len(wsUsers))
			for _, u := range wsUsers {
				if u.Email != "" {
					ouByEmail[strings.ToLower(u.Email)] = u.OrgUnitPath
				}
			}
			before := len(asg)
			asg = inScopeAssignments(asg, ouByEmail, scopes)
			licLog.WithFields(logrus.Fields{"scopes": scopes, "kept": len(asg), "of": before}).
				Info("OU-scoped Workspace assignments")
		}
```

(Use `googleLog` — the package-level google logger used by `loadDevices`; confirm its name in `cmd/` via `grep -n googleLog cmd/*.go`. If it is named differently, use that name.)

- [ ] **Step 8: Build, vet, race, lint**

Run:
```bash
gofmt -w cmd/licenses.go cmd/licenses_ou_test.go
go build ./... && go vet ./... && go test -race ./...
golangci-lint run ./cmd/... ./config/... ./google/...
```
Expected: build/vet/test all pass; golangci-lint reports no new issues (the pre-existing `snipe/client.go` QF1008 is unrelated and not in these packages).

- [ ] **Step 9: Document in README**

In `README.md`, find the licenses configuration section (search for `licenses:` or `org_unit_path`). Add documentation for the new key and the scope prerequisite:

````markdown
#### Scoping licenses to Org Units

By default the license sync covers every Workspace user / ChromeOS device the
API returns. To restrict it to specific Org Units (and their sub-OUs), set:

```yaml
licenses:
  org_unit_paths:
    - /Students          # matches /Students and everything under it
```

If `licenses.org_unit_paths` is omitted, the global `google.org_unit_path` is
used; if that is also unset, no OU filtering is applied. Matching is by path
segment, so `/Students` matches `/Students/Online/Fall 2024` but not
`/StudentsClub`.

**Workspace OU scoping requires the `admin.directory.user.readonly` scope** (to
read each user's Org Unit). Add it to `google.scopes` **and** grant it to the
service account in the Google Admin console (Security → API controls →
Domain-wide delegation):

```yaml
google:
  scopes:
    - https://www.googleapis.com/auth/admin.directory.device.chromeos.readonly
    - https://www.googleapis.com/auth/admin.directory.user.readonly
```
````

- [ ] **Step 10: Commit**

```bash
git add cmd/licenses.go cmd/licenses_ou_test.go README.md
git -c user.name="Robbie Trencheny" -c user.email="robbie@campus.edu" commit -m "feat(licenses): scope Workspace + Chrome license sync to configured OUs

Resolve the effective OU scopes (licenses.org_unit_paths, else the global
org_unit_path), and when set, drop Workspace assignments whose user OU is out of
scope and Chrome devices whose OrgUnitPath is out of scope, before reconciling.
Workspace user OUs come from a new cached Directory Users.list sweep. Engines
stay OU-agnostic. No filtering when no scope is configured.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Self-Review

**1. Spec coverage:**
- Config `licenses.org_unit_paths` + global default + empty=off → Task 1 (`OrgUnitPaths`, `EffectiveLicenseScopes`). ✓
- `email → orgUnitPath` via cached `Users.list` → Task 2 (`ListAllUsers`, serialize) + Task 3 (`loadUsers`). ✓
- `InScope` segment-boundary matcher → Task 1. ✓
- Filter Workspace assignments + Chrome devices in `cmd`, engines unchanged → Task 3. ✓
- Edge cases: empty scopes no-op (Task 1 `InScope`/`EffectiveLicenseScopes`, tested); email absent from map → dropped (Task 3 `inScopeAssignments`, tested); license with zero in-scope holders skipped (unchanged engine behavior — fewer targets → SKU/type never enters the reconcile map). ✓
- Scope prerequisite documented → Task 3 Step 9 + Global Constraints. ✓
- Out of scope (context threading, alias resolution, per-SKU scoping) → not added. ✓

**2. Placeholder scan:** No TBD/TODO; every code step has complete code. The only "confirm the name" notes (`googleLog`, import presence) are concrete grep instructions, not deferred logic.

**3. Type consistency:** `User{Email, OrgUnitPath}` defined in Task 2, consumed identically in Task 3 (`u.Email`, `u.OrgUnitPath`). `InScope(ou string, scopes []string) bool` and `EffectiveLicenseScopes(*Config) []string` defined in Task 1, called with matching signatures in Task 3. `ListAllUsers(ctx) ([]User, error)`, `SerializeUsers`/`DeserializeUsers` defined in Task 2, used in `loadUsers` (Task 3). `inScopeDevices(devs, scopes)` / `inScopeAssignments(asg, ouByEmail, scopes)` defined and tested in Task 3.
