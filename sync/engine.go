package sync

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"

	"github.com/CampusTech/google2snipe/config"
	"github.com/CampusTech/google2snipe/google"
	"github.com/CampusTech/google2snipe/snipe"
)

// SnipeClient is the subset of the snipe wrapper the engine depends on. Every
// method takes a context so a Ctrl-C (SIGINT/SIGTERM) cancels in-flight requests
// and aborts retry backoff instead of hard-killing the process mid-sync.
type SnipeClient interface {
	GetAssetBySerial(ctx context.Context, serial string) ([]snipe.Asset, error)
	ListAllAssets(ctx context.Context) ([]snipe.Asset, error)
	CreateAsset(ctx context.Context, a snipe.Asset) (snipe.Asset, error)
	PatchAsset(ctx context.Context, id int, a snipe.Asset) (snipe.Asset, error)
	CheckoutAssetToUser(ctx context.Context, assetID, userID int) error
	CheckinAsset(ctx context.Context, assetID int) error
	ListAllModels(ctx context.Context) ([]snipe.Model, error)
	CreateModel(ctx context.Context, m snipe.Model) (snipe.Model, error)
	ListAllManufacturers(ctx context.Context) ([]snipe.Manufacturer, error)
	CreateManufacturer(ctx context.Context, name string) (snipe.Manufacturer, error)
	ListAllUsers(ctx context.Context) ([]snipe.User, error)
	ListAllStatusLabels(ctx context.Context) ([]snipe.StatusLabel, error)
}

// Stats accumulates per-run counters.
type Stats struct{ Total, Created, Updated, Skipped, Errors int }

// add sums each counter field of o into s.
func (s *Stats) add(o Stats) {
	s.Total += o.Total
	s.Created += o.Created
	s.Updated += o.Updated
	s.Skipped += o.Skipped
	s.Errors += o.Errors
}

// Engine reconciles ChromeOS devices into Snipe-IT.
type Engine struct {
	cfg   *config.Config
	snipe SnipeClient
	log   *logrus.Logger

	mu                 sync.Mutex                    // guards models and manufacturers
	models             map[string]snipe.Model        // keyed by model name
	manufacturers      map[string]snipe.Manufacturer // keyed by lowercased name
	userIndex          map[string]int                // keyed by lowercased match-field value
	deployableStatuses map[int]bool                  // Snipe status-label IDs that allow checkout
	assetIndex         map[string]snipe.Asset        // keyed by strings.ToLower(serial)
	stats              Stats
}

// New builds an Engine.
func New(cfg *config.Config, sc SnipeClient, logger *logrus.Logger) *Engine {
	if logger == nil {
		logger = logrus.New()
	}
	return &Engine{
		cfg:                cfg,
		snipe:              sc,
		log:                logger,
		models:             map[string]snipe.Model{},
		manufacturers:      map[string]snipe.Manufacturer{},
		userIndex:          map[string]int{},
		deployableStatuses: map[int]bool{},
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

// Warm preloads models, manufacturers, and users into in-memory indexes.
func (e *Engine) Warm(ctx context.Context) error {
	models, err := e.snipe.ListAllModels(ctx)
	if err != nil {
		return err
	}
	for _, m := range models {
		e.models[m.Name] = m
	}
	manufs, err := e.snipe.ListAllManufacturers(ctx)
	if err != nil {
		return err
	}
	for _, m := range manufs {
		e.manufacturers[strings.ToLower(m.Name)] = m
	}
	users, err := e.snipe.ListAllUsers(ctx)
	if err != nil {
		return err
	}
	for _, u := range users {
		key := userKey(u, e.cfg.Sync.Checkout.MatchField)
		if key != "" {
			e.userIndex[strings.ToLower(key)] = u.ID
		}
	}
	labels, err := e.snipe.ListAllStatusLabels(ctx)
	if err != nil {
		return err
	}
	for _, s := range labels {
		if strings.EqualFold(s.Type, "deployable") {
			e.deployableStatuses[s.ID] = true
		}
	}
	assets, err := e.snipe.ListAllAssets(ctx)
	if err != nil {
		return err
	}
	e.assetIndex = make(map[string]snipe.Asset, len(assets))
	for _, a := range assets {
		if a.Serial == "" {
			continue
		}
		key := strings.ToLower(a.Serial)
		if _, exists := e.assetIndex[key]; exists {
			e.log.WithField("serial", a.Serial).Warn("multiple assets share this serial; keeping first seen")
			continue
		}
		e.assetIndex[key] = a
	}
	e.log.WithFields(logrus.Fields{
		"models": len(e.models), "manufacturers": len(e.manufacturers),
		"users": len(e.userIndex), "deployable_statuses": len(e.deployableStatuses),
		"asset_index": len(e.assetIndex),
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
func (e *Engine) ensureManufacturer(ctx context.Context, dev google.Device) (int, error) {
	vendor := modelVendor(dev.Model)
	if vendor == "" {
		return e.cfg.SnipeIT.DefaultManufacturerID, nil
	}
	if id, ok := e.cfg.SnipeIT.ManufacturerIDs[strings.ToLower(vendor)]; ok && id != 0 {
		return id, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if m, ok := e.manufacturers[strings.ToLower(vendor)]; ok {
		return m.ID, nil
	}
	m, err := e.snipe.CreateManufacturer(ctx, vendor)
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
func (e *Engine) ensureModel(ctx context.Context, dev google.Device) (int, error) {
	name := strings.TrimSpace(dev.Model)
	if name == "" {
		name = "Unknown ChromeOS Device"
	}
	// Optionally drop the leading vendor token from the model name so it isn't
	// duplicated with the manufacturer (e.g. "HP Chromebook 14c" -> "Chromebook 14c").
	if e.cfg.Sync.StripModelVendor {
		if vendor := modelVendor(dev.Model); vendor != "" {
			if stripped := strings.TrimSpace(strings.TrimPrefix(name, vendor)); stripped != "" {
				name = stripped
			}
		}
	}
	e.mu.Lock()
	if m, ok := e.models[name]; ok {
		e.mu.Unlock()
		return m.ID, nil
	}
	e.mu.Unlock()

	manufID, err := e.ensureManufacturer(ctx, dev)
	if err != nil {
		return 0, err
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	// Re-check after acquiring the lock (another goroutine may have created it).
	if m, ok := e.models[name]; ok {
		return m.ID, nil
	}
	m, err := e.snipe.CreateModel(ctx, snipe.Model{
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

// SyncAll reconciles every device through a bounded worker pool and returns run
// statistics. It is not safe to call concurrently with itself or StatsSnapshot.
func (e *Engine) SyncAll(ctx context.Context, devs []google.Device) Stats {
	if err := ctx.Err(); err != nil {
		e.log.WithError(err).Info("sync cancelled before dispatch")
		return e.stats
	}
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
			for d := range jobs {
				e.syncDevice(ctx, d, &partials[idx])
			}
		}(w)
	}
	// Stop dispatching new work once ctx is cancelled (Ctrl-C): close the channel so
	// the workers drain whatever they already picked up and exit, instead of queueing
	// the rest of the device list.
	for _, d := range devs {
		// Explicit check first: a bare select can still pick the send when ctx.Done() is
		// also ready (both cases selectable), so a cancelled run could otherwise dispatch more.
		if ctx.Err() != nil {
			goto drain
		}
		select {
		case jobs <- d:
		case <-ctx.Done():
			goto drain
		}
	}
drain:
	close(jobs)
	wg.Wait()
	for _, p := range partials {
		e.stats.add(p)
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
func (e *Engine) SyncDevice(ctx context.Context, dev google.Device) {
	e.syncDevice(ctx, dev, &e.stats)
}

// syncDevice is the per-device implementation; counters are written to st.
func (e *Engine) syncDevice(ctx context.Context, dev google.Device, st *Stats) {
	st.Total++
	serial := strings.TrimSpace(dev.SerialNumber)
	if serial == "" {
		e.log.WithField("device_id", dev.DeviceId).Debug("skipping device with empty serial")
		st.Skipped++
		return
	}
	l := e.log.WithField("serial", serial)

	existing, ok := e.assetIndex[strings.ToLower(serial)]
	if !ok {
		if e.cfg.Sync.UpdateOnly {
			l.Debug("update-only: skipping create")
			st.Skipped++
			return
		}
		e.createDev(ctx, dev, l, st)
	} else {
		e.updateDev(ctx, dev, existing, l, st)
	}
}

// isContextErr reports whether err is a context cancellation/deadline, i.e. a graceful
// Ctrl-C shutdown rather than a real sync failure.
func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (e *Engine) createDev(ctx context.Context, dev google.Device, l *logrus.Entry, st *Stats) {
	if e.cfg.Sync.DryRun {
		l.WithField("model", dev.Model).Info("[DRY RUN] would create asset")
		st.Created++
		return
	}
	modelID, err := e.ensureModel(ctx, dev)
	if err != nil {
		if isContextErr(err) {
			l.WithError(err).Debug("cancelled during model resolution")
			st.Skipped++
			return
		}
		l.WithError(err).Error("ensure model failed")
		st.Errors++
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
	// Check the new asset out to its user AT CREATE time (one call instead of
	// create + checkout) when the status is deployable and a user resolves. The
	// guard mirrors applyCheckout's, so create and update pick the same user.
	checkedOutAtCreate := false
	if len(e.deployableStatuses) == 0 || e.deployableStatuses[asset.StatusID] {
		if userID, ok := e.resolveCheckoutUser(dev); ok {
			asset.AssignedToID = userID
			checkedOutAtCreate = true
		}
	}
	created, err := e.snipe.CreateAsset(ctx, asset)
	if err != nil {
		if isContextErr(err) {
			l.WithError(err).Debug("cancelled during create")
			st.Skipped++
			return
		}
		l.WithError(err).Error("create asset failed")
		st.Errors++
		return
	}
	l.WithField("snipe_id", created.ID).Info("created asset")
	if !checkedOutAtCreate {
		e.applyCheckout(ctx, dev, created, l)
	}
	st.Created++
}

func (e *Engine) updateDev(ctx context.Context, dev google.Device, existing snipe.Asset, l *logrus.Entry, st *Stats) {
	if !e.cfg.Sync.Force && deviceOlderThan(dev, existing.UpdatedAt) {
		l.Debug("snipe record newer than device; skipping field update")
		e.applyCheckout(ctx, dev, existing, l)
		st.Skipped++
		return
	}
	if e.cfg.Sync.DryRun {
		l.WithField("snipe_id", existing.ID).Info("[DRY RUN] would update asset")
		st.Updated++
		return
	}
	modelID, err := e.ensureModel(ctx, dev)
	if err != nil {
		if isContextErr(err) {
			l.WithError(err).Debug("cancelled during model resolution")
			st.Skipped++
			return
		}
		l.WithError(err).Error("ensure model failed")
		st.Errors++
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
	if _, err := e.snipe.PatchAsset(ctx, existing.ID, patch); err != nil {
		if isContextErr(err) {
			l.WithError(err).Debug("cancelled during update")
			st.Skipped++
			return
		}
		l.WithError(err).Error("update asset failed")
		st.Errors++
		return
	}
	l.WithField("snipe_id", existing.ID).Info("updated asset")
	e.applyCheckout(ctx, dev, existing, l)
	st.Updated++
}

func (e *Engine) applyCheckout(ctx context.Context, dev google.Device, asset snipe.Asset, l *logrus.Entry) {
	// Snipe-IT only checks out assets whose status is deployable; skip devices
	// whose mapped status isn't (e.g. DEPROVISIONED/DISABLED -> Archived) so we
	// don't attempt an impossible checkout. Only enforced when status-label
	// deployability is known.
	if len(e.deployableStatuses) > 0 && !e.deployableStatuses[e.statusID(dev)] {
		l.WithField("status", dev.Status).Debug("skipping checkout: non-deployable status")
		return
	}
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
	// For sync/force mode: if the asset is currently checked out to a different
	// user, check it in first so Snipe-IT will accept the reassignment.
	mode := e.cfg.Sync.Checkout.Mode
	if (mode == "sync" || mode == "force") && asset.AssignedToID != 0 && asset.AssignedToID != userID {
		if err := e.snipe.CheckinAsset(ctx, asset.ID); err != nil {
			l.WithError(err).Warn("checkin before reassign failed")
			return
		}
	}
	if err := e.snipe.CheckoutAssetToUser(ctx, asset.ID, userID); err != nil {
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
