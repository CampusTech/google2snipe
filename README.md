# google2snipe

Sync **ChromeOS device** inventory from the [Google Admin SDK Directory API](https://developers.google.com/workspace/admin/directory/reference/rest/v1/chromeosdevices) into [Snipe-IT](https://snipeitapp.com). Written in Go.

A sibling of [`fleet2snipe`](https://github.com/CampusTech/fleet2snipe) and the wider `*2snipe` family (inspired by [`grokability/jamf2snipe`](https://github.com/grokability/jamf2snipe)) — same download → cache → map → reconcile pattern, but sourced from Google Workspace's ChromeOS device directory. Scope is **ChromeOS only**: Chromebooks, Chromeboxes, Chromebases, and ChromeOS Flex installs that show up under `Devices → Chrome devices` in the Admin console.

## What you get

- **One binary, three subcommands** — `sync` (full reconciliation, run from cron), `setup` (idempotent custom-field creation), `test` (connectivity check). There is no webhook listener: Google has no ChromeOS device-change push, so a frequent cron `sync` is the reconciliation loop.
- **gjson field mapping over the *entire* ChromeOsDevice schema.** The official SDK struct is marshalled back to JSON, so any documented field — including deeply nested arrays like `recentUsers.#.email`, `diskVolumeReports.0.volumeInfo.0.storageFree`, or `cpuInfo.0.logicalCpus.0.cStates.0.sessionDuration` — is addressable with full gjson syntax plus optional value transforms.
- **ChromeOS lifecycle status → Snipe-IT status labels.** A configurable map turns `DEPROVISIONED`/`DISABLED`/`ACTIVE`/… into your Snipe-IT status-label IDs, and unlike most importers it **keeps status in sync on existing assets**.
- **Idempotent `setup`** that creates a 33-field ChromeOS baseline in Snipe-IT (with the right field *formats* — `DATE`, `IP`, `MAC`, `NUMERIC`), associates them with your fieldset, and writes the resulting `field_mapping` back into `settings.yaml` (comments preserved).
- **Checkout to the assigned user** — `annotatedUser` first, falling back to the most-recent **managed** login user (domain-restricted), with correct check-in-then-checkout reassignment.
- **Service-account auth with domain-wide delegation**, via the official `google.golang.org/api/admin/directory/v1` SDK.
- **`--dry-run`** gated at every mutation (enforced in both the engine *and* the Snipe-IT client), local **cache** for offline dev (`--use-cache`), single-device sync (`--serial` / `--device-id`), structured **logrus** logging.
- **Concurrent sync with auto-backoff** — devices are processed by a bounded worker pool (`--concurrency`, default 8). The Snipe-IT client automatically retries 429 and transient 5xx responses (honoring `Retry-After`), so it self-throttles rather than failing under concurrency. Set `--concurrency 1` for serial behaviour.
- **Custom-field rejection retry** — if Snipe-IT rejects a field for being outside the model's fieldset, the offending keys are stripped and the PATCH is retried so the rest of the update still lands.
- **Distroless Dockerfile** included.

## Quick start

```sh
go build ./...

cp settings.example.yaml settings.yaml
$EDITOR settings.yaml                   # fill in Google/Snipe credentials + IDs

./google2snipe test                     # verify connectivity to both APIs
./google2snipe setup                    # create the ChromeOS custom fields in Snipe-IT
./google2snipe sync --dry-run --verbose # preview
./google2snipe sync                     # do it
```

Run `sync` on a cron (every 15–60 min is typical) as your authoritative reconciliation loop.

## Authentication

ChromeOS device data lives behind the Admin SDK Directory API, which requires **domain-wide delegation** (DWD): a service account that impersonates a Workspace admin.

### Google (one-time setup)

1. **Create a service account** in a Google Cloud project (IAM & Admin → Service Accounts) and download its **JSON key**. Note the service account's **OAuth 2 client ID** (a long number on the service account's details page).
2. **Enable the Admin SDK API** for that project (APIs & Services → Library → "Admin SDK API" → Enable).
3. **Grant domain-wide delegation** in the **Google Admin console**: Security → Access and data control → **API controls** → **Domain-wide delegation** → **Add new**. Enter the service account's client ID and this exact scope (read-only is all `google2snipe` needs):

   ```
   https://www.googleapis.com/auth/admin.directory.device.chromeos.readonly
   ```

4. Pick an **admin user to impersonate** — any account with the *Mobile and endpoint management* (or super-admin) privilege. This becomes `google.impersonate_subject`.

### Snipe-IT

Account → **Manage API Keys** → **Create New Token**. This becomes `snipe_it.api_key`.

### Wiring it up

Set credentials via `settings.yaml` or env vars:

| Env var | Config key |
|---|---|
| `GOOGLE_APPLICATION_CREDENTIALS` | `google.credentials_file` |
| `GOOGLE_IMPERSONATE_SUBJECT` | `google.impersonate_subject` |
| `GOOGLE_CUSTOMER_ID` | `google.customer_id` (default `my_customer`) |
| `SNIPE_URL` | `snipe_it.url` |
| `SNIPE_API_KEY` | `snipe_it.api_key` |

`customer_id` defaults to `my_customer`, which resolves to the impersonated admin's own Workspace account — leave it unless you administer multiple customers.

## The `sync` command

```sh
./google2snipe sync                          # full sweep of every ChromeOS device
./google2snipe sync --force --verbose        # ignore the freshness check
./google2snipe sync --serial 5CD1234ABC      # one device by serial number
./google2snipe sync --device-id <google-id>  # one device by Google deviceId
./google2snipe sync --update-only            # never create new assets, only update
./google2snipe sync --use-cache              # replay devices + Snipe users from .cache/ (no list re-fetch)
./google2snipe sync --projection basic       # opt down from the default FULL projection
./google2snipe sync --concurrency 16         # raise worker count (auto-backoff handles 429s)
./google2snipe sync --concurrency 1          # serial mode (equivalent to serial flag)
```

### Concurrency and performance

`sync` processes devices with a bounded worker pool. The default concurrency is **8** and can be tuned via `--concurrency` or `sync.concurrency` in `settings.yaml`.

**First ("cold") run is the slow one.** Every device is new, so every one triggers a Snipe-IT asset create. Subsequent runs skip devices whose `lastSync` hasn't advanced past Snipe-IT's `updated_at` (the freshness check), so they finish much faster.

**Safe to raise `--concurrency`.** All Snipe-IT writes go through automatic retry with exponential backoff. 429 rate-limit responses honor the `Retry-After` header; transient 5xx errors back off and retry transparently. The client self-throttles rather than failing, so increasing concurrency degrades gracefully under load.

**Bulk asset index.** Both `sync` and `licenses sync` load all existing Snipe-IT assets once at startup and build an in-memory serial→asset index. This replaces one API lookup per device with a single paginated list call, which is particularly significant on large inventories.

Persistent flags (all subcommands): `--config <path>` (default `settings.yaml`), `-v/--verbose` (info), `-d/--debug` (debug), `--log-file <path>`, `--log-format text|json`. The default log level is **warn**, so a plain `sync` prints only the run summary and problems; add `--verbose` to see per-device decisions.

Restricting scope from the server side (cheaper than syncing everything and filtering):

```yaml
google:
  org_unit_path: /Students/Chromebooks   # only this OU subtree
  query: "user:jdoe"                      # Directory API search query (see the
                                          # chromeosdevices.list `query` reference)
```

## Projection: why FULL by default

The Directory API has two projections. `google.projection` defaults to **`full`** because the lightweight `basic` projection omits the report arrays *and* `recentUsers` — and the checkout fallback depends on `recentUsers`. FULL costs **no extra API quota** (quota is per request, not per field); it only means larger responses and a larger `.cache/devices.json`.

`basic` is retained as an opt-down for large fleets that map only basic fields. If you map a FULL-only path (`recentUsers`, `diskSpaceUsage`, `cpuInfo`, `tpmVersionInfo`, `osUpdateStatus`, the various `*Reports[]`, …) while `projection: basic` is set, config load prints a warning naming the offending field.

## How fields get populated

`field_mapping` is the single source that feeds Snipe-IT custom fields. Values that resolve empty are **skipped** — `google2snipe` never overwrites Snipe-IT data with `""`. It is auto-populated by `setup`, but you can hand-edit it freely. Each entry is either a bare gjson path or an object with `path` + optional `transform`; both forms coexist:

```yaml
sync:
  field_mapping:
    _snipeit_chrome_serial_1: serialNumber                 # bare string — path only
    _snipeit_chrome_ou_2: orgUnitPath
    _snipeit_chrome_update_3: osUpdateStatus.state         # nested object
    _snipeit_chrome_recent_4: recentUsers.#.email          # array → comma-joined

    _snipeit_chrome_ram_5:                                 # object form — adds a transform
      path: systemRamTotal
      transform: bytes_to_gb                               # "8589934592" → "8.59"
    _snipeit_chrome_mac_6:
      path: macAddress
      transform: mac_colons                                # "a4bb6d123456" → "a4:bb:6d:12:34:56"
```

Full [gjson](https://github.com/tidwall/gjson) syntax (array indexing, `#` iteration, `#(...)` queries, modifiers) works on `path`. Arrays render as a comma-separated list of their non-empty elements.

> **int64-as-string:** Google encodes 64-bit integers (`systemRamTotal`, `diskSpaceUsage.*`, `diskVolumeReports[].volumeInfo[].storage*`) as JSON *strings*. gjson and the byte transforms read string-or-number transparently, so `bytes_to_gb` works on them unchanged.

### Transforms

Transforms standardise units and rendering before a value lands in Snipe-IT. Unknown transform names are **rejected at config load** with an error naming both the bad transform and the field that used it.

| Category | Name | Input | Output |
|---|---|---|---|
| Unit | `bytes_to_gb` | int64 bytes (or numeric string) | decimal GB (`bytes / 10⁹`), 2 dp |
| | `bytes_to_gib` | int64 bytes | binary GiB (`bytes / 2³⁰`), 2 dp |
| | `bytes_to_mb` | int64 bytes | decimal MB (`bytes / 10⁶`), 2 dp |
| | `bytes_to_tb` | int64 bytes | decimal TB (`bytes / 10¹²`), 2 dp |
| Date | `date_only` | RFC3339 / `YYYY-MM-DD` | `YYYY-MM-DD` (UTC) |
| | `datetime` | RFC3339 / timestamp | `YYYY-MM-DD HH:MM:SS` (UTC) |
| | `unix_to_iso` | int64 seconds-since-epoch | `YYYY-MM-DD HH:MM:SS` (UTC) |
| String | `uppercase` / `lowercase` | any string | `strings.ToUpper` / `ToLower` |
| | `mac_colons` | any MAC-ish string | `aa:bb:cc:dd:ee:ff` (lowercase) |
| | `mac_dashes` | any MAC-ish string | `aa-bb-cc-dd-ee-ff` (lowercase) |
| Display | `comma_thousands` | integer (or numeric string) | `1,234,567` |
| | `bool_yes_no` | bool / numeric / string | `Yes` / `No`; `""` for unknown |

**Empty-on-no-data rule:** zero, missing, and unparseable values resolve to `""` for the unit, date, and `unix_to_iso` transforms — so a device that hasn't reported a value yet leaves the Snipe-IT field untouched rather than writing a placeholder. The cosmetic transforms (`comma_thousands`, case) let a legitimate `0` or empty string pass through.

**MAC normaliser:** strips every non-hex character, then regroups into byte pairs with the chosen separator — colon, dash, dot, and run-on formats all converge. Anything that doesn't reduce to exactly 12 hex characters returns `""`. Google reports MACs separator-less, so the `mac_colons`/`mac_dashes` transform is what makes them validate against Snipe-IT's `MAC` field format.

**Dates** are normalised to Snipe-IT-friendly forms. Snipe-IT's `DATE` field format validates via PHP `strtotime`, which accepts RFC3339, but normalising with `date_only`/`datetime` keeps the stored value clean and sortable and sidesteps any fractional-second edge case.

## The `setup` field set

`google2snipe setup` is **idempotent** and safe to re-run. It creates / updates a baseline of `Chrome: …` custom fields in Snipe-IT, associates them with your `custom_fieldset_id`, and rewrites `sync.field_mapping` in `settings.yaml` with the resulting `db_column_name`s. The 33-field default set, with the Snipe-IT field format each is created with:

| Field | gjson path | Transform | Format |
|---|---|---|---|
| Chrome: Serial | `serialNumber` | | ANY |
| Chrome: Device ID | `deviceId` | | ANY |
| Chrome: Model | `model` | | ANY |
| Chrome: OS Type | `chromeOsType` | | ANY |
| Chrome: OS Version | `osVersion` | | ANY |
| Chrome: Platform Version | `platformVersion` | | ANY |
| Chrome: Firmware Version | `firmwareVersion` | | ANY |
| Chrome: OS Compliance | `osVersionCompliance` | | ANY |
| Chrome: OS Update State | `osUpdateStatus.state` | | ANY |
| Chrome: Status | `status` | | ANY |
| Chrome: Org Unit Path | `orgUnitPath` | | ANY |
| Chrome: Annotated User | `annotatedUser` | | ANY |
| Chrome: Annotated Asset ID | `annotatedAssetId` | | ANY |
| Chrome: Annotated Location | `annotatedLocation` | | ANY |
| Chrome: Boot Mode | `bootMode` | | ANY |
| Chrome: MAC | `macAddress` | `mac_colons` | MAC |
| Chrome: Ethernet MAC | `ethernetMacAddress` | `mac_colons` | MAC |
| Chrome: Last Known IP | `lastKnownNetwork.0.ipAddress` | | IP |
| Chrome: CPU Model | `cpuInfo.0.model` | | ANY |
| Chrome: System RAM (GB) | `systemRamTotal` | `bytes_to_gb` | NUMERIC |
| Chrome: Disk Capacity (GB) | `diskSpaceUsage.capacityBytes` | `bytes_to_gb` | NUMERIC |
| Chrome: Disk Used (GB) | `diskSpaceUsage.usedBytes` | `bytes_to_gb` | NUMERIC |
| Chrome: License Type | `deviceLicenseType` | | ANY |
| Chrome: Manufacture Date | `manufactureDate` | `date_only` | DATE |
| Chrome: Order Number | `orderNumber` | | ANY |
| Chrome: Auto-Update Through | `autoUpdateThrough` | `date_only` | DATE |
| Chrome: Support End Date | `supportEndDate` | `date_only` | DATE |
| Chrome: First Enrollment | `firstEnrollmentTime` | `date_only` | DATE |
| Chrome: Last Enrollment | `lastEnrollmentTime` | `date_only` | DATE |
| Chrome: Last Sync | `lastSync` | `datetime` | ANY |
| Chrome: TPM Spec Level | `tpmVersionInfo.specLevel` | | ANY |
| Chrome: Notes | `notes` | | ANY |
| Chrome: Recent Users | `recentUsers.#.email` | | ANY |

`Chrome: Auto-Update Through` is the live AUE date (`autoUpdateThrough`); the deprecated `autoUpdateExpiration` is intentionally not used. The report-derived fields (RAM/disk/CPU, recent users, network, TPM, update state) require `projection: full` to populate — see above.

Other useful fields (`meid`, `lastKnownNetwork.0.wanIpAddress`, `dockMacAddress`, `cpuInfo.0.architecture`, `tpmVersionInfo.family`, `willAutoRenew` with `bool_yes_no`, `deprovisionReason`, …) aren't created by default but can be mapped by hand. Transient telemetry arrays (`activeTimeRanges`, `cpuStatusReports`, `systemRamFreeReports`, `deviceFiles`, …) are deliberately left out as static custom fields, though they remain mappable.

**Manual prerequisites in Snipe-IT** (one time):

1. Create a fieldset → `snipe_it.custom_fieldset_id` (required by `setup`).
2. Create a status label for new assets → `snipe_it.default_status_id`.
3. Create a model category for ChromeOS → `snipe_it.default_category_id`.

Manufacturers are auto-created from the model's vendor token (see Operating notes) — leave them blank, or pre-map them with `manufacturer_ids`.

## Status mapping

```yaml
snipe_it:
  default_status_id: 2          # for new assets and any unmapped status
  status_map:
    ACTIVE: 2
    DEPROVISIONED: 4
    DISABLED: 4
```

ChromeOS reports a lifecycle status (`ACTIVE`, `DEPROVISIONED`, `DISABLED`, `INACTIVE`, `PROVISIONED`, …). `status_map` translates each to a Snipe-IT status-label ID; anything unmapped falls back to `default_status_id`. The mapped status is applied to **existing** assets on every sync, so a device that gets deprovisioned in the Admin console flips to your "Archived"/"Broken" label automatically. The raw value is also written verbatim to the `Chrome: Status` custom field.

## Org units

ChromeOS devices live in Google **org units** (`orgUnitPath`). `setup` maps `orgUnitPath` into the `Chrome: Org Unit Path` custom field; Snipe-IT locations are left untouched. If you'd rather drive Snipe-IT locations or categories from OUs, map `orgUnitPath` to a custom field and build a view/automation on the Snipe-IT side, or add your own mapping entry.

## Checkout to the assigned user

Disabled by default. When enabled, `google2snipe` checks the asset out to a Snipe-IT user derived from the device:

```yaml
sync:
  checkout:
    enabled: true
    use_annotated_user: true        # primary: the admin-set annotatedUser
    fallback_to_recent: true        # else the most-recent managed login user
    recent_user_domain: example.com # only count recent users at this domain
    match_field: email              # snipe field: email | username | employee_num
    mode: assign                    # assign | sync | force
```

- **Source order:** the admin-set `annotatedUser` is used first. If it's empty and `fallback_to_recent` is on, the first `recentUsers[]` entry that is **managed** (`USER_TYPE_MANAGED`) and — when `recent_user_domain` is set — whose email matches `@<domain>` is used. The domain filter keeps personal/guest logins from being assigned. (`recentUsers` requires `projection: full`.)
- **`match_field`** is the Snipe-IT user field to look the value up against; matching is case-insensitive and falls back to the email local-part. All Snipe-IT users are loaded once at warm time and indexed for O(1) lookups.
- **`mode`:** `assign` only checks out when the asset is unassigned; `sync` / `force` also **reassign** when the user differs — and because Snipe-IT refuses to overwrite an existing assignment, the asset is checked **in** first, then back out to the new user.
- A Google user with no Snipe-IT counterpart is logged and skipped — `google2snipe` never auto-creates users.

## Operating notes

- **Match key:** `serialNumber`, case-insensitive. Devices with no serial are skipped. Two Snipe-IT assets sharing a serial → flagged and skipped to avoid clobbering the wrong record.
- **Freshness check:** a device whose `lastSync` (or `lastEnrollmentTime`) predates Snipe-IT's `updated_at` is skipped for field updates. Use `--force` (or `sync.force: true`) to ignore. Checkout reconciliation still runs on a freshness-skipped device, so assignment stays correct even when field data is stale.
- **Asset tag:** template-driven. `sync.asset_tag.template` is a string with `{gjson.path}` placeholders (default `"{annotatedAssetId}"`, e.g. `"CG-{serialNumber}"`). An empty render asks Snipe-IT to auto-assign.
- **Names:** off by default. Set `sync.set_name: true` with an optional `sync.name_template` (default `"{annotatedAssetId}"`, falling back to the serial) to sync the asset name.
- **Model & manufacturer:** the Snipe-IT model is auto-created from the `model` string. ChromeOS has no separate vendor field, so the manufacturer is derived from the **first token** of the model (e.g. `Lenovo` from `Lenovo 300e Chromebook`), resolved against `snipe_it.manufacturer_ids` (lowercased vendor → ID), auto-created if absent, or `snipe_it.default_manufacturer_id` as a fallback.
- **Custom-field rejection retry:** if Snipe-IT rejects fields with "not available on this Asset Model's fieldset", the bad keys are stripped and the PATCH is retried once so the rest applies. Re-run `setup` to fix the underlying fieldset.
- **Cache:** every fetch writes `.cache/devices.json` (ChromeOS devices) and `.cache/users.json` (the Snipe-IT user list used for checkout matching); `--use-cache` replays both without re-paginating the APIs (device raw JSON is restored so gjson mapping still works). Models and manufacturers are always fetched fresh, since they're created during syncs.
- **Rate limiting:** Snipe-IT writes go through a token-bucket limiter (`sync.rate_limit: true`).

## Configuration reference

See [`settings.example.yaml`](settings.example.yaml) for a fully-commented template covering every key. The top-level shape:

```yaml
google:      # credentials_file, impersonate_subject, customer_id, projection, org_unit_path, query
snipe_it:    # url, api_key, default_status_id, default_category_id, default_manufacturer_id,
             # custom_fieldset_id, status_map, manufacturer_ids
sync:        # dry_run, force, rate_limit, concurrency (default 8; 1=serial), update_only, use_cache,
             # cache_dir, set_name, name_template, asset_tag.template, field_mapping (managed by setup),
             # checkout {...}
licenses:    # enabled, default_license_category_id, chrome {...}, workspace {...}
```

## License cost sync

`google2snipe` can attribute the ongoing cost of your Google Workspace subscriptions and ChromeOS device upgrade licenses by syncing them into Snipe-IT as cost-bearing **Licenses** with seats.

- **Workspace licenses** — each active Workspace SKU becomes a Snipe-IT License with one seat per assigned user. Stale seats are checked in and fresh ones created each run (fully reconciled).
- **Chrome upgrade licenses** — each `deviceLicenseType` found on your ChromeOS devices becomes a Snipe-IT License with one seat per device.

### Modeling: perpetual vs. recurring

All licenses are represented as Snipe-IT Licenses with seats. How seats are managed depends on the license term:

| Type | `reassignable` | Reconcile behavior |
|---|---|---|
| Perpetual Chrome upgrade (e.g. `educationUpgradePerpetual`) | `false` | **Additive only** — seats are created when a device is seen and never reclaimed. Perpetual licenses are one-time purchases tied to a device for its lifetime. |
| Recurring Chrome upgrade (fixed-term / annual) | `true` | **Fully reconciled** — stale seats are checked in, fresh ones created. Optional expiry date set from the term. |
| All Workspace SKUs | `true` | **Fully reconciled** — stale seats checked in, fresh ones created each sync. |

### Prerequisites

These three steps are required before enabling the license sync:

1. **Add the `apps.licensing` DWD scope** to your service account. In the Google Admin console go to Security → Access and data control → API controls → Domain-wide delegation, find your service account's client ID, and add:

   ```
   https://www.googleapis.com/auth/apps.licensing
   ```

   This scope is required for the Workspace license discovery and seat reconciliation calls.

2. **Enable the Enterprise License Manager API** in the Google Cloud project that owns the service account:

   ```sh
   gcloud services enable licensing.googleapis.com --project=<your-project>
   ```

   Without this, all license assignment calls fail with `SERVICE_DISABLED`. The tool surfaces this as a clear error message.

3. **Provide a Snipe-IT license category.** When you run `google2snipe licenses setup` it will prompt for a category id. Leave the prompt blank and the command will offer to create a new category for you (defaulting to the name "Software Licenses") — just press Enter twice to accept the defaults. If you prefer to manage categories yourself, create one in Snipe-IT under Settings → Categories (type: License), note the ID, and enter it at the prompt (or set it directly in `settings.yaml`):

   ```yaml
   licenses:
     enabled: true
     default_license_category_id: 7   # your license category ID
   ```

### A note on pricing

Google exposes no pricing API — not even the Reseller or Cloud Channel APIs surface per-seat costs for direct customers. Costs are captured once, interactively, via `google2snipe licenses setup` and stored in `settings.yaml`. Update them there when your subscription pricing changes.

License costs are recorded as the **per-seat** price on each Snipe-IT License (so a user's assigned license seat reflects that user's per-seat cost); the License's total-cost field therefore reflects the per-seat unit, not the full subscription total.

### Commands

#### `google2snipe licenses setup` — discover and price

Run this once (and again when your subscriptions change) to build the `licenses:` config block:

```sh
./google2snipe licenses setup
```

This command is **interactive**:

1. Reads all ChromeOS devices (from the Google Admin API, or the local cache when `sync.use_cache: true` is set in the config) and collects every distinct `deviceLicenseType` found.
2. Probes the supported Workspace products via the Enterprise License Manager API and collects the assignments it can access.
3. For each discovered Chrome upgrade type and Workspace SKU, prompts you for the per-seat cost in USD.
4. Writes the resulting `licenses:` block into `settings.yaml` (comments preserved) and sets `licenses.enabled: true`.

After running setup, review the written config and then run `sync`.

#### `google2snipe licenses sync` — reconcile

```sh
./google2snipe licenses sync                # full reconciliation
./google2snipe licenses sync --dry-run      # preview without mutating Snipe-IT
./google2snipe licenses sync --use-cache    # replay devices/users/assignments from .cache/
```

- **Chrome:** iterates every synced device, creates or updates the corresponding Snipe-IT License seat (keyed by asset serial → Snipe asset ID). Perpetual upgrades are additive; recurring upgrades are reconciled.
- **Workspace:** fetches live license assignments from the Enterprise License Manager API (or `.cache/license_assignments.json` with `--use-cache`), matches users by email to Snipe-IT users, and reconciles seats per SKU. Users with no matching Snipe-IT account are logged and skipped — no users are auto-created.

`--dry-run` is gated at every Snipe-IT write (same enforcement as the `sync` command). Run from cron alongside `google2snipe sync`.

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

## Docker

```sh
docker build -t google2snipe .

# one-shot sync (cron / Cloud Run job / Kubernetes CronJob)
docker run --rm \
  -e GOOGLE_APPLICATION_CREDENTIALS=/sa.json \
  -e GOOGLE_IMPERSONATE_SUBJECT=admin@example.com \
  -e SNIPE_URL=https://snipe.example.com \
  -e SNIPE_API_KEY=... \
  -v $(pwd)/sa.json:/sa.json:ro \
  -v $(pwd)/settings.yaml:/app/settings.yaml:ro \
  google2snipe sync
```

The image is multi-stage and runs on `gcr.io/distroless/static-debian12:nonroot`; the default command is `sync`.

## Differences from fleet2snipe

`google2snipe` follows fleet2snipe's architecture and shares its `setup`/transform/dry-run/cache machinery, but the source shapes the feature set:

- **No `serve` webhook** — Google has no ChromeOS device-change push, so a frequent cron `sync` is the loop. **No device images** (no clean ChromeOS image source).
- **No policy / saved-query / label mapping** — those are Fleet/osquery concepts with no Google analog. ChromeOS exposes its richness as nested device fields instead, so `field_mapping` (gjson) is the single mapping source.
- **Added:** ChromeOS **status → status-label mapping** (kept in sync on existing assets), org-unit capture, `projection` control, and the `annotatedUser → recent-user` checkout model with a domain allow-list.

## License

MIT
