package sync

import (
	"regexp"
	"strings"
	"time"

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
	if e.cfg.Sync.DryRun {
		l.WithField("model", dev.Model).Info("[DRY RUN] would create asset")
		e.stats.Created++
		return
	}
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
	if e.cfg.Sync.DryRun {
		l.WithField("snipe_id", existing.ID).Info("[DRY RUN] would update asset")
		e.stats.Updated++
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
	// For sync/force mode: if the asset is currently checked out to a different
	// user, check it in first so Snipe-IT will accept the reassignment.
	mode := e.cfg.Sync.Checkout.Mode
	if (mode == "sync" || mode == "force") && asset.AssignedToID != 0 && asset.AssignedToID != userID {
		if err := e.snipe.CheckinAsset(asset.ID); err != nil {
			l.WithError(err).Warn("checkin before reassign failed")
			return
		}
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
