# google2snipe — Design Spec

**Date:** 2026-06-17
**Status:** Approved (design phase)
**Module path:** `github.com/CampusTech/google2snipe`

## Summary

`google2snipe` syncs **ChromeOS devices** from the **Google Admin SDK Directory API**
into **Snipe-IT**, following the architecture and feature set of the existing
`fleet2snipe` tool. It is a faithful port: reuse `fleet2snipe`'s package layout,
`snipe/` go-snipeit wrapper, `config/` machinery, and the gjson field-mapping +
transforms engine; swap the Fleet REST client for Google's official
`google.golang.org/api/admin/directory/v1` SDK; and add ChromeOS-specific behavior
(status→label mapping, OrgUnit custom field, annotatedUser→recent checkout).

Scope is **ChromeOS devices only**. No other Google device types are synced.

## Goals

- Full reconciliation sweep (`sync`) suitable for cron, plus single-device sync.
- Idempotent `setup` that creates ChromeOS custom fields in Snipe-IT and merges
  the resulting `db_column_name`s back into `settings.yaml`.
- Configurable field mapping (gjson paths + transforms) into Snipe-IT custom fields.
- Status-label mapping, OrgUnit capture, and optional checkout-to-user.
- `--dry-run`, `--debug`, local response caching, structured logging — full parity
  with `fleet2snipe`'s cross-cutting concerns.

## Non-Goals (dropped vs fleet2snipe)

- **`serve` webhook** — Google Admin SDK has no simple ChromeOS device-change
  webhook. A near-real-time path would require the Reports/audit API + Pub/Sub
  (a much larger effort with marginal benefit over a frequent cron `sync`). A
  frequent cron `sync` is the supported real-time-ish mechanism.
- **Device images (appledb.dev)** — no clean image source for ChromeOS models.
  An optional per-model image map MAY be added later but is out of scope for v1.
- **Policy / query / label mapping** — Fleet-specific concepts with no Google
  analog. Omitted.

## Architecture

Package layout mirrors `fleet2snipe`:

```
google2snipe/
├── main.go                 # version injection → cmd.Execute()
├── cmd/
│   ├── root.go             # cobra root, config load, logrus setup (per-package loggers)
│   ├── sync.go             # full sweep + single-device (--serial / --device-id)
│   ├── setup.go            # idempotent ChromeOS custom-field creation + config merge
│   └── test.go             # connectivity check (Directory API + Snipe ping)
├── config/                 # YAML structs + validation + MergeFieldMapping (ported)
│   ├── config.go
│   └── config_test.go
├── google/                 # official admin/directory/v1 client wrapper
│   ├── client.go           # auth (SA key + DWD / ADC), ListAllChromeOSDevices, GetByID, paging, retry
│   └── types.go            # helpers around directory.ChromeOsDevice (+ raw JSON for gjson)
├── snipe/                  # go-snipeit wrapper — reused ~verbatim
│   └── client.go
├── sync/                   # core engine
│   ├── engine.go
│   └── engine_test.go
├── settings.example.yaml
├── Dockerfile              # distroless, CMD sync
├── README.md
└── LICENSE                 # MIT
```

**Reuse map:** `snipe/` ~100% reusable; `config/` mostly reusable (auth fields
differ); `sync/engine.go` ~90% reusable (swap data source + ChromeOS specifics);
`cmd/root.go` logging setup reusable. New: `google/` replaces `fleetapi/`.

## Source: Google Admin SDK Directory API

- **SDK:** `google.golang.org/api/admin/directory/v1` (official), with
  `golang.org/x/oauth2/google` and `google.golang.org/api/option` for auth.
- **Auth (primary):** Service-account JSON key (path from config, or
  `GOOGLE_APPLICATION_CREDENTIALS`) with **domain-wide delegation**. Build a JWT
  config with scope `admin.directory.device.chromeos.readonly` and
  `Subject = <admin email to impersonate>` (config: `google.impersonate_subject`).
- **Auth (fallback):** Application Default Credentials (Workload Identity on
  Cloud Run/GKE) when no key file is provided; still requires DWD + subject.
- **Customer ID:** `google.customer_id`, default `my_customer`.
- **List:** `Chromeosdevices.List(customerId)` paginated via `PageToken`.
  - `projection`: `BASIC` (default) or `FULL` (pulls reports/recentUsers; heavier).
    Config `google.projection` + `sync --projection` override.
  - Optional filters: `google.org_unit_path` and `google.query` (mirror
    fleet2snipe's team/platform filters).
- **Get single:** `Chromeosdevices.Get(customerId, deviceId)`.
- **Retry:** backoff on HTTP 429/5xx, respect `Retry-After`; no retry on 4xx.
- **Cache:** `.cache/devices.json` written after each fetch; `--use-cache` reads it
  for offline dev. Raw JSON retained per device for gjson field-mapping.

### ChromeOS device fields of interest

`serialNumber`, `deviceId`, `model`, `osVersion`, `platformVersion`,
`firmwareVersion`, `bootMode`, `status`, `annotatedAssetId`, `annotatedUser`,
`annotatedLocation`, `notes`, `orgUnitPath`, `macAddress`, `ethernetMacAddress`,
`lastSync`, `lastEnrollmentTime`, `autoUpdateExpiration`, `supportEndDate`,
`willAutoRenew`, `recentUsers[]` (`{type, email}`). Reports
(`cpuStatusReports[]`, `diskVolumeReports[]`, `systemRamFreeReports[]`, etc.)
require `projection=FULL`.

## Snipe-IT integration

Reuse `fleet2snipe`'s `snipe/` wrapper (go-snipeit): dry-run enforcement,
token-bucket rate limiting (2 req/s, burst 5), custom-field-rejection retry,
`GetAssetBySerial`, `CreateAsset`, `PatchAsset`, `CheckoutAssetToUser`,
`CheckinAsset`, model/manufacturer/user listing, `SetupFields`.

### Mapping rules

- **Match key:** `serialNumber`, case-insensitive exact match. `0`→create,
  `1`→update, `>1`→skip + warn.
- **Asset tag:** template (config `asset_tag.template`), default
  `{annotatedAssetId}`; if the rendered value is empty, fall back to auto-assign
  (let Snipe-IT generate). Per-OU/template overrides optional. `{gjson.path}`
  placeholders against device JSON.
- **Model:** `model` string; auto-create Snipe model if missing (cached by model).
- **Manufacturer:** derived from `model` (first token) via configurable
  `snipe_it.manufacturer_ids` map (vendor lowercased → ID); auto-create when
  absent; `snipe_it.default_manufacturer_id` fallback. (Directory API has no
  separate vendor field.)
- **Category:** single `snipe_it.default_category_id` (all ChromeOS); optional
  override map for form factors.
- **Status:** `snipe_it.status_map` (e.g. `ACTIVE→Deployed`,
  `DEPROVISIONED→Archived`, `DISABLED→Broken`, `INACTIVE/PROVISIONED→…`) →
  Snipe status-label IDs. New/unmapped assets use `snipe_it.default_status_id`.
  Raw `status` is also written to a custom field. Status IS updated on existing
  assets (unlike fleet2snipe, which never changes status).
- **OrgUnit:** `orgUnitPath` written to a custom field only; Snipe location
  untouched.
- **Name:** `sync.set_name` (default off) with a template source (e.g.
  `{annotatedAssetId}` or `{serialNumber}`).

### Checkout (opt-in, off by default)

Config `sync.checkout`:
```yaml
checkout:
  enabled: false
  use_annotated_user: true        # primary source: annotatedUser
  fallback_to_recent: true        # else first eligible recentUsers entry
  recent_user_domain: ""          # if set, only recent users at this domain qualify
  match_field: email              # email | username | employee_num
  mode: assign                    # assign | sync | force
```
Resolution: try `annotatedUser`; if empty and `fallback_to_recent`, pick the first
`recentUsers[]` entry of managed type whose email matches `recent_user_domain`
(any domain if unset). Match the Snipe-IT user by `match_field`. `mode` semantics
match fleet2snipe (`assign` = check out if currently unassigned; `sync` = keep in
sync; `force` = always enforce).

## Field-mapping engine (ported)

Keep the gjson `field_mapping` source (path + optional transform) against the
device JSON, and the reusable transforms: date/`unix_to_iso`/ISO passthrough,
`mac_colons`/`mac_dashes`, `bool_yes_no`, `uppercase`/`lowercase`,
`comma_thousands`, byte/GB conversions, etc. Add ChromeOS-handy transforms as
needed. Empty/missing/unparseable → `""` (never written) for unit/date transforms.

Per-key freshness: skip update when device `lastSync` (or
`lastEnrollmentTime`) is older than Snipe `updated_at`, unless `--force`.

Fleet-only sources (`policy_mapping`, `query_mapping`, `label_mapping`,
`labels_field`, `per_platform`) are **omitted** — no Google equivalent. (A single
implicit ChromeOS "platform" is assumed.)

## `setup` command

Idempotently create a standard ChromeOS field set in Snipe-IT and merge
`db_column_name`s back into `settings.yaml` (same machinery as fleet2snipe's
`MergeFieldMapping`). Default fields and their mappings:

| Field name | Path | Transform |
|---|---|---|
| Chrome: Serial | `serialNumber` | |
| Chrome: Device ID | `deviceId` | |
| Chrome: Model | `model` | |
| Chrome: OS Version | `osVersion` | |
| Chrome: Platform Version | `platformVersion` | |
| Chrome: Firmware Version | `firmwareVersion` | |
| Chrome: Status | `status` | |
| Chrome: Org Unit Path | `orgUnitPath` | |
| Chrome: Annotated User | `annotatedUser` | |
| Chrome: Annotated Asset ID | `annotatedAssetId` | |
| Chrome: Annotated Location | `annotatedLocation` | |
| Chrome: Boot Mode | `bootMode` | |
| Chrome: MAC | `macAddress` | `mac_colons` |
| Chrome: Ethernet MAC | `ethernetMacAddress` | `mac_colons` |
| Chrome: Last Sync | `lastSync` | |
| Chrome: Last Enrollment | `lastEnrollmentTime` | |
| Chrome: Auto-Update Expiration | `autoUpdateExpiration` | |
| Chrome: Support End Date | `supportEndDate` | |
| Chrome: Notes | `notes` | |
| Chrome: Recent Users | `recentUsers.#.email` | (joined) |

Manual prerequisites (Snipe-IT): ≥1 fieldset, ≥1 status label
(`default_status_id`), ≥1 category (`default_category_id`). Manufacturers
auto-created.

## Commands & flags

- `root`: `--config` (default `settings.yaml`), `--verbose`, `--debug`,
  `--log-file`, `--log-format text|json`. Env: `SNIPE_URL`, `SNIPE_API_KEY`,
  `GOOGLE_APPLICATION_CREDENTIALS`, `GOOGLE_IMPERSONATE_SUBJECT`, `GOOGLE_CUSTOMER_ID`.
- `sync`: `--dry-run`, `--force`, `--serial`, `--device-id`, `--update-only`,
  `--use-cache`, `--projection basic|full`.
- `setup`: `--dry-run`.
- `test`: connectivity + version for both APIs.

## Config (sketch)

```yaml
google:
  credentials_file: /path/to/sa.json   # or GOOGLE_APPLICATION_CREDENTIALS / ADC
  impersonate_subject: admin@campusgroup.co
  customer_id: my_customer
  projection: basic                     # basic | full
  org_unit_path: ""                     # optional filter
  query: ""                             # optional Directory API query

snipe_it:
  url: https://snipe.example.com
  api_key: ...
  default_status_id: 1
  default_category_id: 5
  default_manufacturer_id: 0
  custom_fieldset_id: 2
  status_map: { ACTIVE: 1, DEPROVISIONED: 3, DISABLED: 4 }
  manufacturer_ids: { lenovo: 10, acer: 11, hp: 12, dell: 13, asus: 14 }

sync:
  dry_run: false
  rate_limit: true
  set_name: false
  asset_tag: { template: "{annotatedAssetId}" }
  field_mapping: { _snipeit_chrome_status_6: status, ... }   # merged by `setup`
  checkout:
    enabled: false
    use_annotated_user: true
    fallback_to_recent: true
    recent_user_domain: ""
    match_field: email
    mode: assign
```

## Sync logic

```
SyncDevice(dev):
  serial := dev.SerialNumber; skip if empty
  asset := GetAssetBySerial(serial)
  0 matches → create   (skip if --update-only)
  1 match   → update
  >1        → skip + warn

create: ensureModel → ensureManufacturer → build asset
        (serial, model, status=status_map|default, asset_tag template,
         name?, custom fields from applyMapping) → CreateAsset → applyCheckout
update: freshness check (lastSync vs updated_at unless --force) →
        diff custom fields + status + model + name → PatchAsset (field-rejection
        retry) → applyCheckout
```

Processing is **serial** (no goroutines) — safe for Snipe-IT rate limits; mirrors
fleet2snipe. Per-device errors are logged and non-fatal; the sweep continues.

## Cross-cutting (parity)

logrus structured logging (per-package loggers, level via `--verbose`/`--debug`,
text/json formats); dry-run enforced in `snipe/`; Snipe token-bucket rate limit;
config validation fail-fast (including unknown transform names); `.cache` dir;
distroless multi-stage `Dockerfile` (`CGO_ENABLED=0`, version via ldflags, default
`CMD sync`); MIT `LICENSE`; `README.md` with quick-start
(`test` → `setup` → `sync --dry-run` → `sync`).

## Dependencies

- `google.golang.org/api` (admin/directory/v1, option)
- `golang.org/x/oauth2` (google JWT/DWD)
- `github.com/michellepellon/go-snipeit`
- `github.com/spf13/cobra`
- `github.com/sirupsen/logrus`
- `github.com/tidwall/gjson`
- `gopkg.in/yaml.v3`

## Testing

- `config/config_test.go`: loading, validation, `MergeFieldMapping`.
- `sync/engine_test.go`: field mapping, transforms, status mapping, checkout
  resolution (annotatedUser → recent w/ domain filter), asset-tag rendering.
- `google/` client: parsing/paging against fixture JSON; auth wiring smoke test.

## Open defaults (approved)

- Asset tag defaults to `annotatedAssetId` (auto-assign if empty).
- Sync runs serially.
- Status IS updated on existing assets (departure from fleet2snipe).
