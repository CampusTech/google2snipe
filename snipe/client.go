// Package snipe wraps the go-snipeit library with dry-run enforcement, a
// token-bucket rate limiter, and the convenience methods used by the
// google2snipe sync engine and setup command.
//
// It is ported from CampusTech/fleet2snipe's snipe/client.go and adapted to a
// data-source-agnostic public surface built on plain local types (Asset, Model,
// Manufacturer, User, FieldDef) so the sync engine can depend on stable
// signatures without importing go-snipeit's models directly.
package snipe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	snipeit "github.com/michellepellon/go-snipeit"
	"github.com/sirupsen/logrus"
)

// ErrDryRun is returned by every mutating method when the client is in dry-run
// mode. It is a sentinel so callers can detect it with errors.Is.
var ErrDryRun = errors.New("dry-run: mutation skipped")

// Asset is a data-source-agnostic view of a Snipe-IT hardware asset.
type Asset struct {
	ID           int
	AssetTag     string
	Serial       string
	Name         string
	ModelID      int
	StatusID     int
	AssignedToID int
	CustomFields map[string]string // db_column_name -> value
	UpdatedAt    time.Time
}

// Model is a data-source-agnostic view of a Snipe-IT asset model.
type Model struct {
	ID             int
	ManufacturerID int
	CategoryID     int
	FieldsetID     int
	Name           string
	ModelNumber    string
}

// Manufacturer is a data-source-agnostic view of a Snipe-IT manufacturer.
type Manufacturer struct {
	ID   int
	Name string
}

// User is a data-source-agnostic view of a Snipe-IT user.
type User struct {
	ID          int
	Username    string
	Email       string
	EmployeeNum string
}

// FieldDef defines a custom field to ensure exists in Snipe-IT.
type FieldDef struct {
	Name    string
	Element string   // text, textarea, radio, listbox, checkbox
	Format  string   // ANY, DATE, BOOLEAN, MAC, IP, NUMERIC, EMAIL, URL
	Values  []string // for radio/listbox fields
}

// Client wraps go-snipeit with dry-run enforcement, optional rate limiting, and
// an injected logger.
type Client struct {
	sc     *snipeit.Client
	dryRun bool
	logger *logrus.Logger
}

// Compile-time assertion that *Client implements the method set the sync engine
// (Task 7) defines as its SnipeClient interface. Declared locally to avoid an
// import cycle with the sync package.
var _ interface {
	GetAssetBySerial(serial string) ([]Asset, error)
	CreateAsset(a Asset) (Asset, error)
	PatchAsset(id int, a Asset) (Asset, error)
	CheckoutAssetToUser(assetID, userID int) error
	CheckinAsset(assetID int) error
	ListAllModels() ([]Model, error)
	CreateModel(m Model) (Model, error)
	ListAllManufacturers() ([]Manufacturer, error)
	CreateManufacturer(name string) (Manufacturer, error)
	ListAllUsers() ([]User, error)
	SetupFields(fieldsetIDs []int, fields []FieldDef) (map[string]string, error)
	Ping() (string, error)
} = (*Client)(nil)

// snipeLogger adapts the injected logrus logger to go-snipeit's Logger
// interface for debug request/response tracing.
type snipeLogger struct {
	logger *logrus.Logger
}

func (l *snipeLogger) LogRequest(method, url string, body []byte) {
	l.logger.WithFields(logrus.Fields{"method": method, "url": url}).Debug("snipe-it request")
}

func (l *snipeLogger) LogResponse(method, url string, statusCode int, body []byte) {
	l.logger.WithFields(logrus.Fields{"method": method, "url": url, "status": statusCode}).Debug("snipe-it response")
}

// New constructs a wrapped go-snipeit client. It does NOT make any network
// call: go-snipeit's NewClientWithOptions only validates and parses the URL.
//
// When rateLimit is true, a token-bucket limiter of 2 req/s with burst 5 is
// applied — the same default used by the other CampusTech 2snipe tools.
// When dryRun is true, every mutating method returns ErrDryRun before any HTTP
// request is made.
func New(url, apiKey string, dryRun, rateLimit bool, logger *logrus.Logger) (*Client, error) {
	if logger == nil {
		logger = logrus.New()
	}
	baseURL := strings.TrimRight(url, "/")

	opts := &snipeit.ClientOptions{Logger: &snipeLogger{logger: logger}}
	if rateLimit {
		opts.RateLimiter = snipeit.NewTokenBucketRateLimiter(2, 5)
	}

	sc, err := snipeit.NewClientWithOptions(baseURL, apiKey, opts)
	if err != nil {
		return nil, fmt.Errorf("creating snipe-it client: %w", err)
	}
	return &Client{sc: sc, dryRun: dryRun, logger: logger}, nil
}

// Ping fetches one record to verify the API key works and returns a short
// status string. Used by the connectivity-check command.
func (c *Client) Ping() (string, error) {
	resp, _, err := c.sc.Assets.ListContext(context.Background(), &snipeit.ListOptions{Limit: 1})
	if err != nil {
		return "", fmt.Errorf("ping: %w", err)
	}
	return fmt.Sprintf("ok (%d assets reachable)", resp.Total), nil
}

// GetAssetBySerial looks up assets by serial. Snipe's /byserial endpoint does a
// partial search, so this filters to exact case-insensitive matches.
func (c *Client) GetAssetBySerial(serial string) ([]Asset, error) {
	resp, _, err := c.sc.Assets.GetAssetBySerialContext(context.Background(), serial)
	if err != nil {
		return nil, fmt.Errorf("looking up serial %s: %w", serial, err)
	}
	var out []Asset
	for _, a := range resp.Rows {
		if strings.EqualFold(a.Serial, serial) {
			out = append(out, fromSnipeAsset(a))
		}
	}
	return out, nil
}

// CreateAsset creates a hardware asset and returns it with its assigned ID and
// asset tag.
// CreateAsset creates an asset. On custom-field validation errors (Snipe
// rejecting a field as "not available on this Asset Model's fieldset", or as
// invalid), it strips the rejected fields and retries the create once so the
// asset still lands with the fields that do fit.
func (c *Client) CreateAsset(a Asset) (Asset, error) {
	if c.dryRun {
		return Asset{}, ErrDryRun
	}
	sa := toSnipeAsset(a)
	resp, _, err := c.sc.Assets.CreateContext(context.Background(), sa)
	if err != nil {
		return Asset{}, fmt.Errorf("creating asset: %w", err)
	}
	if resp.Status == "success" {
		return fromSnipeAsset(resp.Payload), nil
	}

	rejected, reason := invalidFieldErrors(resp.Message.String())
	if len(rejected) > 0 && sa.CustomFields != nil {
		c.logger.WithFields(logrus.Fields{
			"serial":      a.Serial,
			"model_id":    sa.Model.ID,
			"fieldset_id": sa.Model.FieldsetID,
			"fields":      rejected,
			"reason":      reason,
		}).Warn("Snipe-IT rejected custom fields — retrying without them. Run 'google2snipe setup' to fix the fieldset.")
		cleaned := make(map[string]string, len(sa.CustomFields))
		for k, v := range sa.CustomFields {
			cleaned[k] = v
		}
		for _, k := range rejected {
			delete(cleaned, k)
		}
		sa.CustomFields = cleaned
		resp, _, err = c.sc.Assets.CreateContext(context.Background(), sa)
		if err != nil {
			return Asset{}, fmt.Errorf("creating asset (retry): %w", err)
		}
		if resp.Status != "success" {
			return Asset{}, fmt.Errorf("creating asset failed: %s", resp.Message.String())
		}
		return fromSnipeAsset(resp.Payload), nil
	}
	return Asset{}, fmt.Errorf("creating asset failed: %s", resp.Message.String())
}

// PatchAsset partially updates an asset. On custom-field validation errors
// (Snipe rejecting a field as "not available on this Asset Model's fieldset",
// or as invalid), it strips the rejected fields and retries the PATCH once so
// the rest of the update still applies.
func (c *Client) PatchAsset(id int, a Asset) (Asset, error) {
	if c.dryRun {
		return Asset{}, ErrDryRun
	}
	sa := toSnipeAsset(a)
	resp, _, err := c.sc.Assets.PatchContext(context.Background(), id, sa)
	if err != nil {
		return Asset{}, fmt.Errorf("updating asset %d: %w", id, err)
	}
	if resp.Status == "success" {
		return fromSnipeAsset(resp.Payload), nil
	}

	rejected, reason := invalidFieldErrors(resp.Message.String())
	if len(rejected) > 0 && sa.CustomFields != nil {
		c.logger.WithFields(logrus.Fields{
			"asset_id":    id,
			"model_id":    sa.Model.ID,
			"fieldset_id": sa.Model.FieldsetID,
			"fields":      rejected,
			"reason":      reason,
		}).Warn("Snipe-IT rejected custom fields — retrying without them. Run 'google2snipe setup' to fix the fieldset.")
		cleaned := make(map[string]string, len(sa.CustomFields))
		for k, v := range sa.CustomFields {
			cleaned[k] = v
		}
		for _, k := range rejected {
			delete(cleaned, k)
		}
		sa.CustomFields = cleaned
		resp, _, err = c.sc.Assets.PatchContext(context.Background(), id, sa)
		if err != nil {
			return Asset{}, fmt.Errorf("updating asset %d (retry): %w", id, err)
		}
		if resp.Status != "success" {
			return Asset{}, fmt.Errorf("updating asset %d failed: %s", id, resp.Message.String())
		}
		return fromSnipeAsset(resp.Payload), nil
	}
	return Asset{}, fmt.Errorf("updating asset %d failed: %s", id, resp.Message.String())
}

// CheckoutAssetToUser checks an asset out to a Snipe-IT user. Errors if the
// asset is already checked out — call CheckinAsset first if reassigning.
func (c *Client) CheckoutAssetToUser(assetID, userID int) error {
	if c.dryRun {
		return ErrDryRun
	}
	body := map[string]any{
		"checkout_to_type": "user",
		"assigned_user":    userID,
	}
	resp, _, err := c.sc.Assets.CheckoutContext(context.Background(), assetID, body)
	if err != nil {
		return fmt.Errorf("checking out asset %d to user %d: %w", assetID, userID, err)
	}
	if resp.Status != "success" {
		return fmt.Errorf("checking out asset %d to user %d: %s", assetID, userID, resp.Message.String())
	}
	return nil
}

// CheckinAsset returns a checked-out asset back to its base state. Safe to call
// on an asset that isn't currently checked out (Snipe-IT returns a soft error
// which we treat as success).
func (c *Client) CheckinAsset(assetID int) error {
	if c.dryRun {
		return ErrDryRun
	}
	resp, _, err := c.sc.Assets.CheckinContext(context.Background(), assetID, map[string]any{})
	if err != nil {
		return fmt.Errorf("checking in asset %d: %w", assetID, err)
	}
	if resp.Status != "success" {
		// "That asset is not checked out to anyone" is fine — we wanted it
		// unassigned and it already is.
		if strings.Contains(strings.ToLower(resp.Message.String()), "not checked out") {
			return nil
		}
		return fmt.Errorf("checking in asset %d: %s", assetID, resp.Message.String())
	}
	return nil
}

// ListAllModels pages through every model in Snipe-IT.
func (c *Client) ListAllModels() ([]Model, error) {
	var out []Model
	offset := 0
	const limit = 500
	for {
		resp, _, err := c.sc.Models.ListContext(context.Background(), &snipeit.ListOptions{Limit: limit, Offset: offset})
		if err != nil {
			return nil, fmt.Errorf("listing models: %w", err)
		}
		for _, m := range resp.Rows {
			out = append(out, fromSnipeModel(m))
		}
		if len(out) >= resp.Total {
			break
		}
		offset += limit
	}
	return out, nil
}

// CreateModel creates a new asset model.
func (c *Client) CreateModel(m Model) (Model, error) {
	if c.dryRun {
		return Model{}, ErrDryRun
	}
	resp, _, err := c.sc.Models.CreateContext(context.Background(), toSnipeModel(m))
	if err != nil {
		return Model{}, fmt.Errorf("creating model: %w", err)
	}
	if resp.Status != "success" {
		return Model{}, fmt.Errorf("creating model failed: %s", resp.Message.String())
	}
	return fromSnipeModel(resp.Payload), nil
}

// ListAllManufacturers pages through every manufacturer.
func (c *Client) ListAllManufacturers() ([]Manufacturer, error) {
	var out []Manufacturer
	offset := 0
	const limit = 500
	for {
		resp, _, err := c.sc.Manufacturers.ListContext(context.Background(), &snipeit.ListOptions{Limit: limit, Offset: offset})
		if err != nil {
			return nil, fmt.Errorf("listing manufacturers: %w", err)
		}
		for _, m := range resp.Rows {
			out = append(out, Manufacturer{ID: m.ID, Name: m.Name})
		}
		if len(out) >= resp.Total {
			break
		}
		offset += limit
	}
	return out, nil
}

// CreateManufacturer creates a manufacturer by name.
func (c *Client) CreateManufacturer(name string) (Manufacturer, error) {
	if c.dryRun {
		return Manufacturer{}, ErrDryRun
	}
	m := snipeit.Manufacturer{}
	m.Name = name
	resp, _, err := c.sc.Manufacturers.CreateContext(context.Background(), m)
	if err != nil {
		return Manufacturer{}, fmt.Errorf("creating manufacturer: %w", err)
	}
	if resp.Status != "success" {
		return Manufacturer{}, fmt.Errorf("creating manufacturer failed: %s", resp.Message.String())
	}
	return Manufacturer{ID: resp.Payload.ID, Name: resp.Payload.Name}, nil
}

// ListAllUsers pages through every Snipe-IT user.
func (c *Client) ListAllUsers() ([]User, error) {
	var out []User
	offset := 0
	const limit = 500
	for {
		resp, _, err := c.sc.Users.ListContext(context.Background(), &snipeit.ListOptions{Limit: limit, Offset: offset})
		if err != nil {
			return nil, fmt.Errorf("listing users: %w", err)
		}
		for _, u := range resp.Rows {
			out = append(out, User{
				ID:          u.ID,
				Username:    u.Username,
				Email:       u.Email,
				EmployeeNum: u.Employee,
			})
		}
		if len(out) >= resp.Total {
			break
		}
		offset += limit
	}
	return out, nil
}

// SetupFields creates/updates the listed custom fields and associates each one
// with every fieldset in fieldsetIDs. It returns a map of field Name ->
// db_column_name. Snipe-IT fields have a single global db_column_name no matter
// how many fieldsets reference them, so multi-fieldset support is purely an
// additional Associate call per fieldset.
func (c *Client) SetupFields(fieldsetIDs []int, fields []FieldDef) (map[string]string, error) {
	if c.dryRun {
		return nil, ErrDryRun
	}
	existing, _, err := c.sc.Fields.List(nil)
	if err != nil {
		return nil, fmt.Errorf("listing existing fields: %w", err)
	}
	byName := make(map[string]snipeit.Field, len(existing.Rows))
	for _, f := range existing.Rows {
		byName[f.Name] = f
	}

	out := make(map[string]string, len(fields))
	for _, f := range fields {
		field := snipeit.Field{}
		field.Name = f.Name
		field.Element = f.Element
		field.Format = f.Format
		field.FieldValues = strings.Join(f.Values, "\r\n")

		var fieldID int
		var dbColumn string

		if ex, ok := byName[f.Name]; ok {
			resp, _, err := c.sc.Fields.Update(ex.ID, field)
			if err != nil {
				return out, fmt.Errorf("updating field %q: %w", f.Name, err)
			}
			if resp.Status != "success" {
				return out, fmt.Errorf("updating field %q: %s", f.Name, resp.Message.String())
			}
			fieldID = resp.Payload.ID
			dbColumn = resp.Payload.DBColumnName
			if dbColumn == "" {
				dbColumn = ex.DBColumnName
			}
		} else {
			resp, _, err := c.sc.Fields.Create(field)
			if err != nil {
				return out, fmt.Errorf("creating field %q: %w", f.Name, err)
			}
			if resp.Status != "success" {
				return out, fmt.Errorf("creating field %q: %s", f.Name, resp.Message.String())
			}
			fieldID = resp.Payload.ID
			dbColumn = resp.Payload.DBColumnName
		}

		out[f.Name] = dbColumn

		for _, fsID := range fieldsetIDs {
			if fsID <= 0 {
				continue
			}
			if _, err := c.sc.Fields.Associate(fieldID, fsID); err != nil {
				return out, fmt.Errorf("associating %q with fieldset %d: %w", f.Name, fsID, err)
			}
		}
	}

	// Snipe-IT sometimes returns a blank db_column_name on update — refetch.
	missing := false
	for _, v := range out {
		if v == "" {
			missing = true
			break
		}
	}
	if missing {
		if refresh, _, err := c.sc.Fields.List(nil); err == nil {
			lut := make(map[string]string, len(refresh.Rows))
			for _, f := range refresh.Rows {
				lut[f.Name] = f.DBColumnName
			}
			for name, dbCol := range out {
				if dbCol == "" {
					if col, ok := lut[name]; ok && col != "" {
						out[name] = col
					}
				}
			}
		}
	}

	return out, nil
}

// invalidFieldErrors parses a Snipe-IT validation error message and returns the
// custom-field keys that should be stripped on retry, plus a short reason.
func invalidFieldErrors(msg string) ([]string, string) {
	var errs map[string][]string
	if err := json.Unmarshal([]byte(msg), &errs); err != nil {
		return nil, ""
	}
	var rejected []string
	reason := ""
fieldLoop:
	for key, msgs := range errs {
		for _, m := range msgs {
			switch {
			case strings.Contains(m, "not available on this Asset Model's fieldset"):
				rejected = append(rejected, key)
				reason = "fieldset missing"
				continue fieldLoop
			case strings.Contains(m, "is invalid."):
				rejected = append(rejected, key)
				reason = "invalid field value"
				continue fieldLoop
			}
		}
	}
	return rejected, reason
}

// toSnipeAsset translates the local Asset into go-snipeit's Asset for writes.
func toSnipeAsset(a Asset) snipeit.Asset {
	var sa snipeit.Asset
	sa.ID = a.ID
	sa.Name = a.Name
	sa.AssetTag = a.AssetTag
	sa.Serial = a.Serial
	sa.Model.ID = a.ModelID
	sa.StatusLabel.ID = a.StatusID
	if a.AssignedToID != 0 {
		sa.User = &snipeit.FlexUser{}
		sa.User.User.ID = a.AssignedToID
	}
	if len(a.CustomFields) > 0 {
		sa.CustomFields = a.CustomFields
	}
	return sa
}

// fromSnipeAsset translates a go-snipeit Asset into the local Asset.
func fromSnipeAsset(sa snipeit.Asset) Asset {
	a := Asset{
		ID:           sa.ID,
		AssetTag:     sa.AssetTag,
		Serial:       sa.Serial,
		Name:         sa.Name,
		ModelID:      sa.Model.ID,
		StatusID:     sa.StatusLabel.ID,
		CustomFields: sa.CustomFields,
	}
	if sa.User != nil {
		a.AssignedToID = sa.User.User.ID
	}
	if sa.UpdatedAt != nil {
		a.UpdatedAt = sa.UpdatedAt.Time
	}
	return a
}

// toSnipeModel translates the local Model into go-snipeit's Model for writes.
func toSnipeModel(m Model) snipeit.Model {
	var sm snipeit.Model
	sm.ID = m.ID
	sm.Name = m.Name
	sm.ModelNumber = m.ModelNumber
	sm.Manufacturer.ID = m.ManufacturerID
	sm.Category.ID = m.CategoryID
	sm.FieldsetID = m.FieldsetID
	return sm
}

// fromSnipeModel translates a go-snipeit Model into the local Model.
func fromSnipeModel(sm snipeit.Model) Model {
	return Model{
		ID:             sm.ID,
		ManufacturerID: sm.Manufacturer.ID,
		CategoryID:     sm.Category.ID,
		FieldsetID:     sm.FieldsetID,
		Name:           sm.Name,
		ModelNumber:    sm.ModelNumber,
	}
}
