# Sync Performance (Concurrency + Bulk Index) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Cut the cold device sync from ~3h (serial, round-trip-bound) to a fraction by adding a bounded worker pool, a one-time bulk asset index, checkout-at-create, and 429-aware backoff — applied to `sync` (full) and the asset-lookup path of `licenses sync` (bulk index).

**Architecture:** Add `ListAllAssets` + a 429 retry wrapper to the `snipe` client. Build a `serial→asset` index once at `Warm` and use it instead of per-device `GetAssetBySerial`. Make the `sync` engine concurrency-safe (per-worker stats merged at the end; a mutex around the lazily-created model/manufacturer caches; the warmed user/status maps are already read-only) and run `SyncAll` through a bounded worker pool sized by config. Fold checkout into asset creation for new deployable devices.

**Tech Stack:** Go 1.26.4, `github.com/michellepellon/go-snipeit`, `github.com/sirupsen/logrus`, `github.com/spf13/cobra`, stdlib `sync`/`net/http`.

## Global Constraints

- **Module path:** `github.com/CampusTech/google2snipe`; Go 1.26.4.
- **Branch:** `perf/sync-concurrency` (off `main` @ the PR-1 merge).
- **Concurrency default = 8**, configurable via `sync.concurrency` (config) and `--concurrency` (flag). `concurrency <= 1` ⇒ serial (preserves current behavior).
- **Concurrency safety is mandatory.** Every task that can run under the pool MUST be verified with `go test -race ./...`. The data races the audit found: `e.stats` int counters (no mutex) and the `e.models` / `e.manufacturers` maps (written by lazy create). `userIndex`/`deployableStatuses` are read-only after `Warm` — do not add locks for them.
- **429 backoff** honors the `Retry-After` header when present, else exponential backoff (base 500ms, ×2, cap 30s), max 6 attempts; logs each retry at Warn.
- **No behavior change at concurrency=1 or with an empty/old config** (default applies). Dry-run, `--use-cache`, status/checkout semantics all unchanged.
- **Lint/format:** `gofmt`/`goimports` clean; `go vet ./...` clean; `go mod tidy` idempotent before each commit.
- **Commit author** `Robbie Trencheny <robbie@campus.edu>`; end every commit body with `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.

## File / Responsibility Map

- `snipe/client.go` — add `ListAllAssets()` (paginated) and a central `retry429` wrapper applied to mutating + lookup calls.
- `config/config.go` — add `SyncConfig.Concurrency int` (default 8).
- `cmd/sync.go` — add `--concurrency` flag, OR it into `cfg.Sync.Concurrency`.
- `sync/engine.go` — add `ListAllAssets` to the `SnipeClient` interface; build `assetIndex map[string]snipe.Asset` in `Warm`; use it in the per-device path; checkout-at-create; per-worker stats + `mu` around model/manuf caches; bounded worker pool in `SyncAll`.
- `sync/engine_test.go` (and `sync/stub_test.go`) — stub gains `ListAllAssets`; add the concurrency/-race test.
- `cmd/licenses.go` — build the bulk `serial→asset` index once and use it in `assetIDBySerial`.
- `README.md`, `settings.example.yaml` — document `--concurrency`/`sync.concurrency` + perf notes.

---

## Shared Type Reference (defined across tasks — do not redefine)

```go
// snipe package (Task 1): a bulk lister, mirroring ListAllModels/ListAllUsers.
func (c *Client) ListAllAssets() ([]Asset, error)

// snipe package (Task 2): central retry honoring 429/Retry-After.
func (c *Client) retry429(op string, fn func() (*http.Response, error)) error

// sync package (Task 4): SnipeClient interface gains
ListAllAssets() ([]snipe.Asset, error)
```

---

## Task 1: snipe.ListAllAssets (bulk asset list)

**Files:**
- Modify: `snipe/client.go`
- Test: `snipe/client_test.go` (create if absent)

**Interfaces:**
- Produces: `(*Client) ListAllAssets() ([]Asset, error)` — all hardware assets, paginated (limit 500), mapped via the existing `fromSnipeAsset`.

- [ ] **Step 1: Write the failing test** (httptest server returning two pages of `/hardware`)

```go
package snipe

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
)

func TestListAllAssetsPaginates(t *testing.T) {
	page1 := `{"total":2,"rows":[{"id":1,"asset_tag":"A1","serial":"S1"}]}`
	page2 := `{"total":2,"rows":[{"id":2,"asset_tag":"A2","serial":"S2"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.RawQuery, "offset=500") {
			_, _ = w.Write([]byte(page2))
			return
		}
		_, _ = w.Write([]byte(page1))
	}))
	defer srv.Close()
	c, err := New(srv.URL, "k", false, false, logrus.New())
	if err != nil {
		t.Fatal(err)
	}
	assets, err := c.ListAllAssets()
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 2 || assets[0].Serial != "S1" || assets[1].Serial != "S2" {
		t.Fatalf("paging failed: %+v", assets)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./snipe/ -run TestListAllAssetsPaginates -v`
Expected: FAIL — `ListAllAssets` undefined.

- [ ] **Step 3: Implement `ListAllAssets`** in `snipe/client.go`

Mirror the existing `ListAllModels` pagination loop (limit 500, offset, stop when `len(out) >= total` or a page is empty). Use the go-snipeit assets list call the package already uses in `Ping` (`c.sc.Assets.ListContext(ctx, &snipeit.ListOptions{Limit: 500, Offset: offset})`), map each row via the existing `fromSnipeAsset`, and accumulate. Confirm the exact go-snipeit list method + response field names against `Ping` (snipe/client.go:140) and `ListAllModels` (snipe/client.go:308) before writing — match their idiom.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./snipe/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w . && go vet ./...
git add snipe/client.go snipe/client_test.go
git -c user.name="Robbie Trencheny" -c user.email="robbie@campus.edu" commit -m "feat(snipe): ListAllAssets (paginated bulk asset list)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: snipe 429-aware retry/backoff

**Files:**
- Modify: `snipe/client.go`
- Test: `snipe/client_test.go`

**Interfaces:**
- Produces: `(*Client) retry429(op string, fn func() (*http.Response, error)) error` — runs `fn`; if the returned `*http.Response` is a 429, sleeps `Retry-After` (or exponential backoff: 500ms×2ⁿ capped 30s), and retries (max 6 attempts), logging each retry at Warn. Wraps the go-snipeit call in every mutating + lookup method (`CreateAsset`, `PatchAsset`, `CheckoutAssetToUser`, `CheckinAsset`, `CreateModel`, `CreateManufacturer`, `GetAssetBySerial`, `ListAllAssets`, and the other `ListAll*`).

- [ ] **Step 1: Write the failing test** (server 429s once with `Retry-After: 0`, then 200)

```go
func TestCreateAssetRetriesOn429(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if atomic.AddInt32(&n, 1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"status":"error","messages":"rate limited"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","payload":{"id":7,"asset_tag":"A","serial":"S"}}`))
	}))
	defer srv.Close()
	c, err := New(srv.URL, "k", false, false, logrus.New())
	if err != nil {
		t.Fatal(err)
	}
	a, err := c.CreateAsset(Asset{Serial: "S", ModelID: 1, StatusID: 1})
	if err != nil {
		t.Fatalf("expected success after retry, got %v", err)
	}
	if a.ID != 7 {
		t.Fatalf("asset id = %d, want 7", a.ID)
	}
	if atomic.LoadInt32(&n) < 2 {
		t.Fatalf("expected a retry (>=2 requests), got %d", n)
	}
}
```
(Add `sync/atomic` to the test imports.)

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./snipe/ -run TestCreateAssetRetriesOn429 -v`
Expected: FAIL — without retry, the first 429 surfaces as an error (or the call returns the error).

- [ ] **Step 3: Implement `retry429` + wrap the calls**

```go
func (c *Client) retry429(op string, fn func() (*http.Response, error)) error {
	const maxAttempts = 6
	backoff := 500 * time.Millisecond
	for attempt := 1; ; attempt++ {
		resp, err := fn()
		if resp == nil || resp.StatusCode != http.StatusTooManyRequests {
			return err
		}
		if attempt >= maxAttempts {
			return fmt.Errorf("%s: still rate-limited after %d attempts: %w", op, maxAttempts, err)
		}
		wait := backoff
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, perr := strconv.Atoi(strings.TrimSpace(ra)); perr == nil {
				wait = time.Duration(secs) * time.Second
			}
		}
		c.logger.WithFields(logrus.Fields{"op": op, "attempt": attempt, "wait": wait.String()}).
			Warn("snipe 429; backing off")
		time.Sleep(wait)
		if backoff *= 2; backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}
```

Then wrap each go-snipeit call. Pattern (CreateAsset shown — apply the same to PatchAsset/CheckoutAssetToUser/CheckinAsset/CreateModel/CreateManufacturer/GetAssetBySerial/ListAll*):

```go
func (c *Client) CreateAsset(a Asset) (Asset, error) {
	if c.dryRun {
		return Asset{}, ErrDryRun
	}
	sa := toSnipeAsset(a)
	var resp *snipeit.AssetCreateResponse // use the actual go-snipeit return type
	err := c.retry429("create asset", func() (*http.Response, error) {
		r, httpResp, e := c.sc.Assets.CreateContext(context.Background(), sa)
		resp = r
		return httpResp, e
	})
	// ... keep the existing strip-and-retry custom-field handling + fromSnipeAsset mapping ...
}
```
IMPORTANT: confirm the exact go-snipeit return types/method names before writing (read the current method bodies). Keep ALL existing per-method behavior (dry-run guard, the CreateAsset custom-field strip-and-retry, the CheckinAsset 2xx-parse-tolerance) — only wrap the network call in `retry429`. Add imports `strconv`, `time` if missing.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./snipe/ -v`
Expected: PASS (retry test + all existing snipe tests).

- [ ] **Step 5: Commit**

```bash
gofmt -w . && go vet ./...
git add snipe/client.go snipe/client_test.go
git -c user.name="Robbie Trencheny" -c user.email="robbie@campus.edu" commit -m "feat(snipe): 429-aware retry/backoff on Snipe API calls

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 3: config + cmd concurrency setting

**Files:**
- Modify: `config/config.go`, `cmd/sync.go`
- Test: `config/config_test.go`

**Interfaces:**
- Produces: `SyncConfig.Concurrency int` (yaml `concurrency`, default 8 via applyDefaults when 0); `--concurrency` flag on `syncCmd` ORed into `cfg.Sync.Concurrency`.

- [ ] **Step 1: Write the failing test** — append to `config/config_test.go`

```go
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
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./config/ -run TestConcurrencyDefaults -v`
Expected: FAIL — field/default missing.

- [ ] **Step 3: Add the field + default + flag**

In `config/config.go`: add `Concurrency int \`yaml:"concurrency"\`` to `SyncConfig`; in `applyDefaults` set `if c.Sync.Concurrency == 0 { c.Sync.Concurrency = 8 }`.
In `cmd/sync.go`: add a package var `syncConcurrency int`, register `syncCmd.Flags().IntVar(&syncConcurrency, "concurrency", 0, "parallel workers for the Snipe sync (0 = use config default 8)")`, and after `config.Load` add `if syncConcurrency > 0 { cfg.Sync.Concurrency = syncConcurrency }`.

- [ ] **Step 4: Run tests + help**

Run: `go test ./config/ -v && go build ./... && go run . sync --help | grep concurrency`
Expected: PASS; help shows `--concurrency`.

- [ ] **Step 5: Commit**

```bash
gofmt -w . && go vet ./...
git add config/config.go config/config_test.go cmd/sync.go
git -c user.name="Robbie Trencheny" -c user.email="robbie@campus.edu" commit -m "feat(config): sync.concurrency setting (default 8) + --concurrency flag

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 4: engine bulk serial→asset index (replaces per-device lookup)

**Files:**
- Modify: `sync/engine.go`, `sync/stub_test.go`
- Test: `sync/engine_test.go`

**Interfaces:**
- Consumes: `snipe.ListAllAssets` (Task 1).
- Produces: `SnipeClient` interface gains `ListAllAssets() ([]snipe.Asset, error)`; `Warm` populates `e.assetIndex map[string]snipe.Asset` (key = `strings.ToLower(serial)`); the per-device path looks up `e.assetIndex` instead of calling `GetAssetBySerial`.

- [ ] **Step 1: Add `ListAllAssets` to the stub** `sync/stub_test.go`

```go
func (s *stubSnipe) ListAllAssets() ([]snipe.Asset, error) {
	var out []snipe.Asset
	for _, list := range s.bySerial {
		out = append(out, list...)
	}
	return out, nil
}
```

- [ ] **Step 2: Write the failing test** (asset already present ⇒ engine updates, never calls per-device lookup)

```go
func TestSyncUsesAssetIndexForExisting(t *testing.T) {
	stub := &stubSnipe{
		bySerial: map[string][]snipe.Asset{"ABC": {{ID: 9, Serial: "ABC", ModelID: 1, StatusID: 2}}},
		statusLabels: []snipe.StatusLabel{{ID: 2, Name: "Deployable", Type: "deployable"}},
	}
	e := New(testConfig(), stub, logrus.New()) // adapt to the engine's actual constructor
	if err := e.Warm(); err != nil {
		t.Fatal(err)
	}
	dev := devWithSerialStatus(t, "ABC", "ACTIVE") // helper producing a google.Device
	e.SyncDevice(dev)
	if len(stub.created) != 0 {
		t.Errorf("expected update via index, but a create happened: %+v", stub.created)
	}
	if _, ok := stub.patched[9]; !ok {
		t.Errorf("expected asset 9 to be patched via the index")
	}
}
```
(Adapt `New(...)`/`testConfig()`/`devWith...` to the engine's actual constructor + existing test helpers — read `sync/engine_test.go` first and reuse what's there.)

- [ ] **Step 3: Run it to verify it fails**

Run: `go test ./sync/ -run TestSyncUsesAssetIndex -v`
Expected: FAIL — index not built / still calls GetAssetBySerial.

- [ ] **Step 4: Implement the index**

- Add `ListAllAssets() ([]snipe.Asset, error)` to the `SnipeClient` interface in `sync/engine.go`.
- Add `assetIndex map[string]snipe.Asset` to the `Engine` struct.
- In `Warm()`, after the existing warms, call `ListAllAssets()` and build `e.assetIndex` keyed by `strings.ToLower(asset.Serial)` (skip empty serials). Log the count at Info.
- In the per-device path (where it currently calls `e.snipe.GetAssetBySerial(serial)`), replace the lookup with an index read: `existing, ok := e.assetIndex[strings.ToLower(serial)]`. Preserve the existing create-vs-update / freshness / multi-match logic — the index holds exactly one asset per serial (the bulk list is deduped by serial; if Snipe somehow has duplicate serials, keep the first and the existing "multiple assets share this serial" warning can move to the Warm-time index build).
- Keep `GetAssetBySerial` in the snipe client (still used elsewhere / as a fallback) — just stop calling it per-device here.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./sync/ -v`
Expected: PASS (new test + all existing engine tests; the stub now serves the index).

- [ ] **Step 6: Commit**

```bash
gofmt -w . && go vet ./...
git add sync/engine.go sync/stub_test.go sync/engine_test.go
git -c user.name="Robbie Trencheny" -c user.email="robbie@campus.edu" commit -m "perf(sync): build a serial->asset index at warm, drop per-device lookups

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 5: engine checkout-at-create

**Files:**
- Modify: `sync/engine.go`
- Test: `sync/engine_test.go`

**Interfaces:**
- Consumes: `toSnipeAsset` mapping `Asset.AssignedToID` into the create payload (confirmed at snipe/client.go:563-565).
- Produces: for a NEW deployable device with a resolved checkout user, the create sets `Asset.AssignedToID` so the asset is created already-assigned; the separate post-create checkout call is skipped for that device.

- [ ] **Step 1: Write the failing test** (new device + deployable + checkout user ⇒ created already assigned, no separate checkout call)

```go
func TestNewDeviceCheckedOutAtCreate(t *testing.T) {
	stub := &stubSnipe{
		bySerial:     map[string][]snipe.Asset{},
		users:        []snipe.User{{ID: 55, Email: "u@example.com"}},
		statusLabels: []snipe.StatusLabel{{ID: 2, Name: "Deployable", Type: "deployable"}},
	}
	e := New(testConfigWithCheckout(), stub, logrus.New())
	if err := e.Warm(); err != nil {
		t.Fatal(err)
	}
	e.SyncDevice(devWithRecentUser(t, "NEW1", "ACTIVE", "u@example.com"))
	if len(stub.created) != 1 || stub.created[0].AssignedToID != 55 {
		t.Fatalf("expected create with AssignedToID=55, got %+v", stub.created)
	}
	if len(stub.checkouts) != 0 {
		t.Errorf("expected no separate checkout call, got %v", stub.checkouts)
	}
}
```
(Adapt config/helpers to the engine's actual checkout config + test helpers.)

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./sync/ -run TestNewDeviceCheckedOutAtCreate -v`
Expected: FAIL — create has AssignedToID=0 and a separate checkout fires.

- [ ] **Step 3: Implement checkout-at-create**

In the CREATE branch of the per-device path: after resolving the asset fields, resolve the checkout user (reuse the existing checkout-user resolution + the deployable-status guard). If a target user id is resolved AND the device's status is deployable, set `asset.AssignedToID = userID` BEFORE `CreateAsset`, and mark that this device was checked out at create so the later `applyCheckout` step is skipped for it. The UPDATE path and all existing checkout semantics (sync/force reassign, checkin-before-reassign) are unchanged — only the new-asset create path folds the checkout in.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./sync/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w . && go vet ./...
git add sync/engine.go sync/engine_test.go
git -c user.name="Robbie Trencheny" -c user.email="robbie@campus.edu" commit -m "perf(sync): check new deployable assets out at create time

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 6: engine concurrency (worker pool + race-safe state)

**Files:**
- Modify: `sync/engine.go`
- Test: `sync/engine_test.go`

**Interfaces:**
- Consumes: `cfg.Sync.Concurrency` (Task 3); the read-only `assetIndex` (Task 4).
- Produces: `SyncAll` runs devices through a bounded worker pool of size `max(1, cfg.Sync.Concurrency)`; stats are accumulated per worker and merged (no shared-counter race); `e.models`/`e.manufacturers` lazy-create is guarded by `e.mu sync.Mutex`. Behavior at concurrency=1 is identical to today.

- [ ] **Step 1: Write the failing race test** (many devices, concurrency>1, run under -race)

```go
func TestSyncAllConcurrentNoRace(t *testing.T) {
	stub := &stubSnipe{
		bySerial:     map[string][]snipe.Asset{},
		statusLabels: []snipe.StatusLabel{{ID: 2, Name: "Deployable", Type: "deployable"}},
	}
	cfg := testConfig()
	cfg.Sync.Concurrency = 8
	e := New(cfg, stub, logrus.New())
	if err := e.Warm(); err != nil {
		t.Fatal(err)
	}
	var devs []google.Device
	for i := 0; i < 200; i++ {
		devs = append(devs, devWithSerialStatus(t, fmt.Sprintf("S%03d", i), "ACTIVE"))
	}
	st := e.SyncAll(devs)
	if st.Created != 200 {
		t.Fatalf("Created = %d, want 200", st.Created)
	}
}
```
(NOTE: the stub's `created`/`checkouts`/`patched` slices/maps are themselves written concurrently by the workers — make the stub append under its own mutex so the TEST harness isn't the race. Add a `sync.Mutex` to `stubSnipe` guarding `created`/`patched`/`checkouts`/`checkins` and the model/manuf create lists. This keeps `-race` focused on the engine.)

- [ ] **Step 2: Run it under -race to verify it fails**

Run: `go test ./sync/ -run TestSyncAllConcurrent -race -v`
Expected: FAIL — `-race` reports a data race on `e.stats` and/or the model/manuf maps (or wrong `Created` count from lost increments).

- [ ] **Step 3: Implement the pool + race-safe state**

1. Add `mu sync.Mutex` to the `Engine` struct (guards `models`/`manufacturers` only).
2. In `ensureModel` and `ensureManufacturer`: wrap the check-create-insert in `e.mu.Lock()` / `defer e.mu.Unlock()` (a missing model is created at most once; concurrent callers serialize on the rare create, hit the cache afterward).
3. Extract the per-device body into `func (e *Engine) syncDevice(dev google.Device, st *Stats)` that writes counters to `*st` (replace every `e.stats.X++` with `st.X++`). Keep the public `func (e *Engine) SyncDevice(dev google.Device)` as a thin wrapper: `e.syncDevice(dev, &e.stats)` (single-threaded callers/tests unchanged).
4. Rewrite `SyncAll`:

```go
func (e *Engine) SyncAll(devs []google.Device) Stats {
	workers := e.cfg.Sync.Concurrency
	if workers < 1 {
		workers = 1
	}
	jobs := make(chan google.Device)
	partials := make([]Stats, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for dev := range jobs {
				e.syncDevice(dev, &partials[idx])
			}
		}(w)
	}
	for _, d := range devs {
		jobs <- d
	}
	close(jobs)
	wg.Wait()
	for _, p := range partials {
		e.stats.add(p) // sum each counter field; add a small Stats.add(Stats) helper
	}
	return e.stats
}
```
Add a `func (s *Stats) add(o Stats)` that sums each counter field. (The progress log `every 50` can be dropped or emitted from a shared atomic counter — keep it simple: drop the per-50 line, since per-license/summary logs already exist; or use `atomic.Int64`. Do NOT reintroduce an unguarded shared int.)

- [ ] **Step 4: Run under -race to verify it passes**

Run: `go test ./sync/ -race -v`
Expected: PASS, no race reports. Also run the whole suite under race: `go test -race ./...`.

- [ ] **Step 5: Commit**

```bash
gofmt -w . && go vet ./...
git add sync/engine.go sync/engine_test.go
git -c user.name="Robbie Trencheny" -c user.email="robbie@campus.edu" commit -m "perf(sync): bounded worker pool with race-safe stats + model/manufacturer cache

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 7: licenses sync uses the bulk asset index

**Files:**
- Modify: `cmd/licenses.go`
- Test: build/run (the licensesync engine is already unit-tested via stubs; this is a wiring change)

**Interfaces:**
- Consumes: `snipe.ListAllAssets` (Task 1).
- Produces: `runLicensesSync` builds a `serial→assetID` map once via `sc.ListAllAssets()` and the `assetIDBySerial` closure reads it, instead of one `GetAssetBySerial` per device.

- [ ] **Step 1: Replace the per-device lookup in `cmd/licenses.go`**

Before the `SyncChrome` call, build the index:
```go
	assetIdx := map[string]int{}
	allAssets, err := sc.ListAllAssets()
	if err != nil {
		return err
	}
	for _, a := range allAssets {
		if a.Serial != "" {
			assetIdx[strings.ToLower(a.Serial)] = a.ID
		}
	}
	assetIDBySerial := func(serial string) (int, bool) {
		id, ok := assetIdx[strings.ToLower(serial)]
		return id, ok
	}
```
Remove the old closure that called `sc.GetAssetBySerial` per serial (and its multi-match warn — duplicate serials are now naturally collapsed by the map; if you want to keep the duplicate-serial signal, log it while building `assetIdx` when a key is overwritten). `strings` is already imported.

- [ ] **Step 2: Verify build + help + tests**

Run: `go build ./... && go test ./... && go run . licenses sync --help`
Expected: build + all tests pass; help unchanged.

- [ ] **Step 3: Commit**

```bash
gofmt -w . && go vet ./...
git add cmd/licenses.go
git -c user.name="Robbie Trencheny" -c user.email="robbie@campus.edu" commit -m "perf(licenses): build a serial->asset index once instead of per-device lookups

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 8: Docs

**Files:**
- Modify: `README.md`, `settings.example.yaml`

- [ ] **Step 1: settings.example.yaml** — add `concurrency: 8` under `sync:` with a comment ("parallel Snipe workers; 1 = serial").

- [ ] **Step 2: README.md** — document `--concurrency` / `sync.concurrency`, that the first cold sync is the slow one, the 429 backoff (safe to raise concurrency; it auto-throttles), and that both `sync` and `licenses sync` now bulk-load assets once.

- [ ] **Step 3: Final verification**

Run: `go build ./... && go test -race ./... && go vet ./... && test -z "$(gofmt -l .)"`
Expected: clean, all tests pass under `-race`.

- [ ] **Step 4: Commit + push + open PR**

```bash
git add README.md settings.example.yaml
git -c user.name="Robbie Trencheny" -c user.email="robbie@campus.edu" commit -m "docs: sync concurrency + bulk asset index

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
git push -u origin perf/sync-concurrency
```
(Controller opens the PR with `gh pr create` after the branch is green.)

---

## Self-Review Notes (author checklist — completed)

**Design coverage:** worker pool → Task 6; 429 backoff → Task 2; bulk asset index → Tasks 1+4 (sync) and Task 7 (licenses); checkout-at-create → Task 5; config/flag (default 8) → Task 3; docs → Task 8. ✓

**Concurrency-safety coverage:** the audit's three hazards each have a fix + a `-race` gate: `e.stats` → per-worker partials merged (Task 6); `e.models`/`e.manufacturers` → `e.mu` around lazy create (Task 6); read-only `userIndex`/`deployableStatuses` → untouched. Stub mutated by workers → guarded by a stub mutex (Task 6 Step 1 note). Task 6 + Task 8 both run `go test -race`. ✓

**Type consistency:** `ListAllAssets` signature identical in snipe (Task 1), the engine `SnipeClient` interface + stub (Task 4); `retry429(op, fn)` (Task 2) used by all wrapped methods; `Stats.add` + `syncDevice(dev, *Stats)` (Task 6); `cfg.Sync.Concurrency` (Task 3) consumed in Task 6. ✓

**Verification-anchored unknowns (flagged in-task):** exact go-snipeit list method + return types for `ListAllAssets`/`retry429` wrapping (Tasks 1–2 say "confirm against existing method bodies"); the engine's actual constructor/test-helper names (Tasks 4–6 say "adapt to existing"). These are local read-then-match, not external-API guesses.
