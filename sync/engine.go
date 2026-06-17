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
