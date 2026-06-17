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
