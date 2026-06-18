# License Cost Sync — Design Spec

**Date:** 2026-06-17
**Status:** Approved (design phase)
**Subsystem of:** google2snipe (separate from the device sync)

## Summary

Add a subsystem that syncs **assigned Google licenses into Snipe-IT as first-class
Licenses with per-seat cost**, so Snipe-IT can attribute an ongoing cost to every
user and device:

- **Google Workspace user subscriptions** (per user, via the Enterprise License
  Manager API) → a Snipe License per SKU, a seat checked out to each licensed user.
- **ChromeOS device upgrade licenses** (per device, from `deviceLicenseType` which
  the device sync already pulls) → a Snipe License per upgrade type, a seat checked
  out to each device asset.

The primary goal is **cost attribution per user**: each Snipe License carries a
configured per-seat cost, and Snipe's per-user reporting then rolls up
(Workspace seats on the user) + (the user's Chromebook's upgrade-license seat,
via the device's checkout).

A companion interactive command, **`licenses setup`**, discovers every license
type in use across the account, asks the operator for the price of each, and
writes the `licenses:` config block.

## Goals

- Represent both license families as Snipe-IT **Licenses with seats** (one
  mechanism), with a configured per-seat cost.
- **Cost rolls up per user** with no extra reporting glue beyond Snipe's native
  license/asset-to-user association.
- **Idempotent + reconciled**: re-running is safe; seats are checked **in** when an
  assignment disappears (offboarded user, wiped/reassigned device) — except where a
  license is non-reassignable (perpetual), where the binding is permanent by design.
- **Self-configuring**: `licenses setup` discovers what exists and only asks for
  prices.

## Non-Goals

- No Consumable objects. (Snipe consumables check out to a user only, can't link to
  a device, aren't reclaimable, and aren't idempotent per-device — a poor fit. A
  Snipe License with `reassignable: false` models a perpetual/one-time license
  correctly instead.)
- No attempt to read prices from Google (the APIs don't expose cost). Costs are
  config-provided.
- No license *provisioning* (we never assign/revoke Google licenses) — read-only on
  the Google side, write-only of seat checkouts on the Snipe side.

## License taxonomy

### ChromeOS device upgrades (`deviceLicenseType`)

The Directory API returns `deviceLicenseType` in camelCase. Each maps to one Snipe
License; `reassignable` is derived from whether the type is perpetual.

| `deviceLicenseType` (API JSON) | Meaning | Snipe License `reassignable` | Expiry |
|---|---|---|---|
| `educationUpgradePerpetual` | standalone perpetual EDU upgrade | **false** | none |
| `enterpriseUpgradePerpetual` | standalone perpetual ENT upgrade | **false** | none |
| `educationUpgrade` *(deprecated)* | perpetual standalone EDU upgrade | **false** | none |
| `education` | **bundled** perpetual EDU upgrade (free w/ device) | **false** | none |
| `enterprise` | **bundled** perpetual ENT upgrade (free w/ device) | **false** | none |
| `educationUpgradeFixedTerm` | standalone fixed-term EDU upgrade | true | optional |
| `enterpriseUpgradeFixedTerm` | standalone fixed-term ENT upgrade | true | optional |
| `enterpriseUpgrade` *(deprecated)* | annual standalone ENT upgrade | true | optional |
| `kioskUpgrade` | annual Kiosk upgrade | true | optional |
| `deviceLicenseTypeUnspecified` / empty | unknown / none | — (skipped) | — |

**Classification rule:** the type name contains `FixedTerm`, or is `enterpriseUpgrade`
/ `kioskUpgrade` (annual) → **recurring** (`reassignable: true`); otherwise →
**perpetual** (`reassignable: false`). Config may override per type.
**Bundled** types (`education`, `enterprise`) are perpetual; their default cost is
`0` (the upgrade came with the hardware) but is configurable.

Today's fleet is **100% `educationUpgradePerpetual`** (9,839 devices; 688 have no
upgrade), i.e. one non-reassignable License with a seat per device. The build
handles every type so it stays correct as the mix changes.

### Workspace user subscriptions (Enterprise License Manager API)

The Licensing API (`licensing/v1`) lists license assignments per **product**, keyed
by **user**. There is no "list all products" call, so the tool ships the known
product catalog and probes each. Each assignment returns `productId`, `skuId`,
`productName`, `skuName`, `userId` (email).

**Candidate products probed during `licenses setup`** (skipped on 403 / empty):

```
Google-Apps   101031   101034   101037   101047   101001   101005
Google-Vault  101033   101038   101054   101039   101040   101035   101052
```

(EDU accounts typically populate `Google-Apps`, `101031`, `101034` archived-user,
`101037` Teaching & Learning, `101047` Gemini for Education; Cloud Identity / Vault /
Voice as applicable.) Notable EDU SKUs: `Google-Apps-For-Education` /
`1010070001` (Fundamentals), `1010310008` (Education Plus), `1010310005` (Education
Standard), `1010370001` (Teaching & Learning Upgrade), `1010340007` (Fundamentals –
Archived User). `licenses setup` records only the SKUs actually assigned; `licenses
sync` iterates the configured product list.

## Snipe-IT modeling

Everything is a Snipe-IT **License** with **seats**:

- Each License: `name`, `category_id` (a license category), `purchase_cost` =
  configured per-seat cost, `seats` (auto-grown to cover assignees), `reassignable`
  (per the taxonomy), optional `expiration_date` (recurring types).
- A **seat** is checked out to either a **user** (Workspace) or an **asset**
  (ChromeOS upgrade). A device's upgrade-license seat → the **device asset**, which
  is itself checked out to the student, so cost rolls up to the user transitively.
- **Cost-per-user** is then native Snipe data: licenses assigned to the user +
  assets (with their license seats) assigned to the user.

## Commands (a `licenses` command group)

A new cobra group keeps these distinct from the device `sync`/`setup`/`test`.

### `google2snipe licenses setup` — interactive, one-time

1. **Discover Chrome upgrade types** from device data (cache or fetch): distinct
   `deviceLicenseType` + device counts.
2. **Discover Workspace SKUs**: probe each candidate product via
   `licenseAssignments.listForProduct`; collect distinct `(productId, skuId,
   skuName)` + user counts.
3. **Auto-classify** each (perpetual vs recurring; `reassignable`).
4. **Quiz** the operator per license, showing kind + count, reading a price from
   stdin (blank = 0):
   ```
   Chrome Education Upgrade (Perpetual)   [perpetual · 9,839 devices]
     cost per seat (USD, blank=0): 38
   Google Workspace for Education Plus    [recurring · 4,212 users · SKU 1010310008]
     cost per seat/yr (USD, blank=0): 5
   ```
5. **Optionally create** the Snipe-IT **license category** if missing (or accept an
   existing `default_license_category_id`).
6. **Write the `licenses:` block** into `settings.yaml` (names, productIds/SKUs,
   inferred kinds, costs, category id) using the same comment-preserving YAML merge
   as the device `setup` (`config.MergeFieldMapping`'s machinery, generalized).

### `google2snipe licenses sync` — the reconcile (`--dry-run`, `--use-cache`)

For each configured license, build the **desired seat-holder set** and **reconcile**
against Snipe's current seats: check out the missing, check in the stale.

1. **Chrome upgrades** (device data): group devices by `deviceLicenseType`. Desired
   seat-holders = the **device assets** (matched by serial). For **reassignable:false**
   (perpetual) licenses, only check **out** new seats (never reclaim — Snipe enforces
   this too). For **reassignable:true** (recurring), full reconcile (check stale seats
   in) + set expiry.
2. **Workspace** (Licensing API): `userEmail → [SKU]`. Per SKU, desired seat-holders
   = the **Snipe users** (matched by email). Full reconcile.

## Architecture

```
licenses subsystem
├── google/licensing.go        # licensing/v1 client: ListAssignmentsForProduct,
│                              #   over candidate products -> userEmail -> [SKU{id,name}]
├── snipe/licenses.go          # hand-rolled (go-snipeit has none):
│                              #   ListLicenses, EnsureLicense(name,cost,cat,reassignable,seats,expiry),
│                              #   GrowSeats, ListSeats, CheckoutSeatToUser/ToAsset, CheckinSeat
├── licensesync/engine.go      # reconcile: build desired sets, diff vs current seats
├── cmd/licenses.go            # `licenses` parent + `setup` + `sync` subcommands
└── config: licenses{...}      # new section (see schema)
```

The device `sync`, `google` (device client), and `snipe` asset/user methods are
reused unchanged. The licenses subsystem reuses: the device cache
(`.cache/devices.json`) for Chrome upgrades, the cached Snipe user list
(`.cache/users.json`) for Workspace matching, and `GetAssetBySerial` for device
matching.

### Google licensing client (`google/licensing.go`)

- New DWD scope `https://www.googleapis.com/auth/apps.licensing` (read-only use).
- `licensing/v1` via `google.golang.org/api/licensing/v1`, built on the same
  service-account + impersonation + debug-transport plumbing as the directory
  client.
- `licenseAssignments.listForProduct(productId, customerId)` paginated; skip a
  product on 403/404 (not enabled for the customer). Cache to
  `.cache/license_assignments.json`.

### Snipe license/seat client (`snipe/licenses.go`)

Hand-rolled raw-HTTP against the Snipe API (go-snipeit has no Licenses support);
reuses the configured URL + token. Methods:
`ListLicenses`, `EnsureLicense(spec)` (create if absent by name; set cost / category
/ reassignable / seats / expiry), `EnsureSeats(licenseID, n)` (PATCH the seat total
up so Snipe materializes seat rows), `ListSeats(licenseID)` (id + assigned user/asset),
`CheckoutSeat(seatID, toUserID|toAssetID)`, `CheckinSeat(seatID)`. Dry-run enforced.
Worth contributing upstream to go-snipeit later.

## Cost model

- Google exposes no prices → config provides a **per-seat cost** per upgrade type
  and per Workspace SKU. Stamped onto the Snipe License `purchase_cost`.
- Per-user cost = Snipe-native rollup of (Workspace license seats on the user) +
  (the user's assigned Chromebook's upgrade-license seat). Recurring costs are
  per-year per seat (operator's chosen basis); perpetual is the one-time per-seat
  cost.
- Unmapped SKUs/types default to cost `0` (still tracked as a License, just $0).

## Config schema (`licenses:` — written by `licenses setup`)

```yaml
licenses:
  enabled: true
  default_license_category_id: 7        # REQUIRED (Snipe license category)

  # ChromeOS device upgrades, keyed by deviceLicenseType (camelCase). kind/
  # reassignable inferred from the type; cost is per seat.
  chrome:
    educationUpgradePerpetual:
      name: "Chrome Education Upgrade (Perpetual)"
      cost: 38.00
      # reassignable: false   # inferred (perpetual)
    educationUpgradeFixedTerm:
      name: "Chrome Education Upgrade (Fixed-term)"
      cost: 8.00
      # reassignable: true; term_months: 12  (optional, sets expiry)

  # Workspace user subscriptions. `products` is the discovered subset of the
  # candidate catalog; sku_costs is per-seat $/yr (unmapped SKU => 0).
  workspace:
    products: ["Google-Apps", "101031", "101034", "101037"]
    sku_costs:
      "Google-Apps-For-Education": 0.00     # Fundamentals (free)
      "1010310008": 5.00                    # Education Plus
      "1010310005": 3.00                    # Education Standard
      "1010370001": 4.00                    # Teaching & Learning Upgrade
```

## Reconciliation semantics

- **Recurring licenses** (Workspace, Chrome fixed-term/annual): per License, diff the
  desired seat-holder set against current seats → check out missing, **check in
  stale** (offboarded user / device that lost or changed the license). Keeps
  cost-per-user current.
- **Perpetual licenses** (`reassignable: false`): additive only — check out a seat to
  each device that has the upgrade; never reclaim (Snipe disallows it and it's
  semantically correct: the perpetual license is consumed and bound to the device).
  Idempotent because the seat carries the device's asset id (skip devices that
  already hold a seat).
- **Seat growth**: before checkout, if assignees exceed the License's seat total,
  grow `seats` so Snipe materializes more seat rows.
- **No Google-side writes** ever.

## Matching

- **Workspace**: assignment `userId` (email) → Snipe user via the existing
  email-indexed user list (lowercased, local-part fallback). A Google user with no
  Snipe counterpart is logged and skipped (never auto-created), as elsewhere.
- **Chrome**: device `serialNumber` → Snipe asset via `GetAssetBySerial` (the device
  must already be synced as an asset by `sync`). Devices not yet in Snipe are logged
  and skipped.

## Caching

`--use-cache` serves devices (`.cache/devices.json`), Snipe users
(`.cache/users.json`), and Workspace license assignments
(`.cache/license_assignments.json`) from disk. License/seat reads from Snipe are
always fresh (they're being mutated).

## Prerequisites

1. Add DWD scope `https://www.googleapis.com/auth/apps.licensing` to the
   service-account's domain-wide-delegation grant (Workspace part).
2. A Snipe-IT **license category** → `default_license_category_id` (manual, like the
   asset `default_status_id`/`default_category_id`; `licenses setup` can create it).

## Build phasing (for the implementation plan)

1. **Snipe license/seat client** (`snipe/licenses.go`) — create/ensure license,
   grow/list seats, checkout-to-user, checkout-to-asset, checkin. Verified against
   the live API with direct calls first.
2. **`licenses` command group + reconcile engine skeleton** — config schema,
   matching, dry-run.
3. **Chrome upgrade sync** (no new Google API) — perpetual (reassignable=false,
   seat per device, additive) covering the current fleet; recurring path included.
4. **`licenses setup`** — Chrome-type discovery + quiz + config write.
5. **Workspace sync** — `google/licensing.go` (apps.licensing scope), Licensing-API
   discovery, License-per-SKU, seats to users, full reconcile; extend `licenses
   setup` to probe Workspace products.

## Open defaults (approved)

- Perpetual Chrome upgrades modeled as non-reassignable Licenses (not Consumables).
- Costs are config-provided per-seat; unmapped → $0.
- `licenses sync` is a separate command from the device `sync` (whole-fleet view is
  required for reconcile).
- Recurring licenses fully reconcile (check seats in); perpetual licenses are
  additive (never reclaimed).
