# License OU Scoping — Design

**Goal:** Let the license sync restrict which Google Workspace users and ChromeOS devices are tracked as license seats to one or more Org Units (and their sub-OUs), so a deployment can match its Snipe-IT LDAP import scope (e.g. `/Students` and below) instead of importing license holders from the whole organization.

**Status:** Approved design (2026-06-18). Branch: `feat/license-ou-scoping` (off `main`).

## Background / Problem

The `licenses sync` command imports two kinds of licenses into Snipe-IT:

- **Chrome** (ChromeOS device upgrade licenses): one seat per device asset. Devices are loaded via `loadDevices`, which already passes the global `org_unit_path` to the Directory ChromeOS devices list, so Chrome seats are *already* OU-scoped when `org_unit_path` is set (the API includes sub-OUs).
- **Workspace** (user subscriptions): one seat per assigned user. Assignments come from the Enterprise License Manager API as `(SKU, user email)` pairs with **no OU information**.

Workspace licenses are therefore only *implicitly* scoped: `SyncWorkspace` keeps an assignment only if the user email matches a Snipe-IT user, and the deployment's LDAP imports only `/Students`. Two problems follow:

1. **Noise / wasted work:** every out-of-scope user is processed and then dropped (a real run logged `skipped=2980`).
2. **Incorrect seating (correctness bug):** the email matcher has a *local-part fallback* — `alice@staff.example.com` can match Snipe's `alice@students.example.com` under a different domain. A staff user sharing a username with an in-scope student can be wrongly given a seat, inflating cost.

There is no way to scope Workspace licenses explicitly by OU.

## Design

### Configuration

Add a license-scope setting that **defaults to the global `org_unit_path`** and can be **overridden** per the license sync. Applies to **both** Chrome and Workspace.

```yaml
org_unit_path: /Students          # existing global (scopes the device/asset sync)
licenses:
  org_unit_paths:                 # NEW (optional); overrides the global for licenses
    - /Students
    - /Faculty/Adjuncts
```

Effective scope resolution (computed once in `runLicensesSync`):

1. If `licenses.org_unit_paths` is non-empty → use it.
2. Else if global `org_unit_path` is non-empty → use `[org_unit_path]`.
3. Else → empty → **no OU filtering** (current behavior; fully backward compatible).

### New data source — Directory users with OUs

Workspace assignments lack OUs, so we need an `email → orgUnitPath` map. Add to the `google` package:

- `func (c *Client) ListAllUsers(ctx context.Context) ([]User, error)` — paginates the Admin SDK Directory `Users.list` (customer `my_customer`), requesting only `primaryEmail` and `orgUnitPath` fields, including suspended users (a suspended user can still hold a license). Returns `[]google.User{Email, OrgUnitPath}`.
- `SerializeUsers` / `DeserializeUsers` and a `cmd` `loadUsers` helper that caches to `users.json` under `cache_dir`, gated by `use_cache` — mirroring the existing `loadDevices` / `loadAssignments` pattern exactly. Users are only loaded when OU filtering is active.

We reject a per-OU `Users.list(query="orgUnitPath='/Students'")` approach: the Admin SDK orgUnitPath query is exact-match and does **not** include sub-OUs, so it cannot satisfy "and below."

### OU matching

A small pure helper (in `config`, alongside the other config logic):

```go
// InScope reports whether ou falls under any of scopes. Empty scopes => true (no filter).
// A scope matches its exact OU and any descendant, on a path-segment boundary:
//   "/Students" matches "/Students" and "/Students/HS", but NOT "/StudentsClub".
func InScope(ou string, scopes []string) bool
```

Rules: empty `scopes` → `true`; otherwise `true` if for some `s`, `ou == s` || `strings.HasPrefix(ou, s+"/")`. Comparison is case-sensitive (Google OU paths are case-stable).

### Wiring (`cmd/licenses.go`) — engines stay OU-agnostic

Filtering happens in `runLicensesSync`, where the data is already loaded, so `SyncWorkspace`, `SyncChrome`, and `Reconcile` are **unchanged** — they simply receive in-scope data.

```
scopes := effectiveLicenseScopes(cfg)
if len(scopes) > 0 {
    users := loadUsers(...)                  // email -> orgUnitPath (cached)
    ouByEmail := index(users)                // map[strings.ToLower(email)]orgUnitPath
    // Workspace: keep assignments whose user OU is in scope
    assignments = filter(assignments, a => InScope(ouByEmail[lower(a.UserEmail)], scopes))
    // Chrome: keep devices whose device OU is in scope
    devs = filter(devs, d => InScope(d.OrgUnitPath, scopes))
    log.Warn("OU scope", scopes, "kept", keptAsg, "of", totalAsg, "assignments;", keptDev, "of", totalDev, "devices")
}
```

- The Workspace filter runs **before** the existing email→Snipe-user matching, so out-of-scope users never reach the local-part fallback.
- A license-holder email **absent** from the directory map (alias-only, recently deleted, or external) resolves to an empty OU → **out of scope → excluded**, logged at debug. (Alias matching is a documented limitation; assignment emails are normally the primary email.)

## Components & Interfaces

- **`config/config.go`** — `LicensesConfig.OrgUnitPaths []string` (`yaml:"org_unit_paths"`); `InScope(ou string, scopes []string) bool`; (optionally) an `effectiveLicenseScopes(*Config) []string` helper.
- **`google/client.go` + `google/types.go`** — `google.User{Email, OrgUnitPath string}`; `Client.ListAllUsers(ctx)`; `SerializeUsers` / `DeserializeUsers`.
- **`cmd/licenses.go`** — `loadUsers(ctx, cfg, log)` (cached); scope resolution + filtering of `assignments` and `devs` before the existing Chrome/Workspace calls.
- **Engines (`licensesync/*`)** — unchanged.

## Data flow

```
runLicensesSync
  ├─ scopes = effectiveLicenseScopes(cfg)
  ├─ if scopes:
  │     loadUsers (cache|API) ──► ouByEmail
  │     assignments := keep(InScope(ouByEmail[email]))      ── Workspace
  │     devs        := keep(InScope(device.OrgUnitPath))    ── Chrome
  ├─ SyncChrome(cfg, devs, assetIDBySerial)        # in-scope devices
  └─ SyncWorkspace(cfg, assignments, userIDByEmail)# in-scope assignments
```

## Error handling / edge cases

- **No scopes** → skip user load and all filtering (back-compat, zero overhead).
- **`ListAllUsers` fails** → propagate the error (abort the run) only when OU filtering is active; otherwise it is never called.
- **License with zero in-scope holders** → its SKU/type never enters the reconcile map → not created (existing behavior).
- **Chrome scope vs device scope:** devices are loaded once with the global `org_unit_path`, so a license scope *broader* than the global device scope can only re-filter what was loaded (a license scope narrower-or-equal works fully). Documented; the typical case (global unset or equal to the license scope) is exact.
- **Cache staleness:** `users.json` honors `use_cache` like the other caches; a stale OU map is the user's explicit cache choice.

## Testing

- `InScope` table tests: empty scopes (all pass); exact match; descendant match; segment-boundary rejection (`/StudentsClub` vs `/Students`); multiple scopes; root-ish edge cases.
- Scope resolution: override wins; falls back to global; empty when neither set.
- Assignment filtering: in/out-of-scope users, an email missing from the map (excluded), case-insensitive email match.
- Device filtering: in/out-of-scope `OrgUnitPath`.
- `google.ListAllUsers` parsing + `Serialize/DeserializeUsers` round-trip (mock/fixture, no live API).

## Out of scope

- Threading a real context (handled separately by `perf/context-cancellation`).
- Alias-email resolution for assignments.
- Per-SKU or per-license-type OU scoping (single scope set governs the whole license sync).

## Backward compatibility

With no `licenses.org_unit_paths` and no global `org_unit_path`, behavior is identical to today (no user fetch, no filtering). Chrome remains scoped by `org_unit_path` exactly as before.
