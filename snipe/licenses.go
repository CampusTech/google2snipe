package snipe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// LicenseSpec / License / LicenseSeat are the Shared Type Reference types (NOT in
// the brief body — define them here exactly as shown; later tasks compile against them).
type LicenseSpec struct {
	Name           string
	CostPerSeat    float64
	CategoryID     int
	Reassignable   bool
	Seats          int
	ExpirationDate string // "YYYY-MM-DD" or ""
}
type License struct {
	ID    int
	Name  string
	Seats int
}
type LicenseSeat struct {
	ID              int
	AssignedUserID  int // 0 if not assigned to a user
	AssignedAssetID int // 0 if not assigned to an asset
}

// LicenseClient talks to Snipe-IT licenses/seats directly (go-snipeit has no support).
type LicenseClient struct {
	baseURL string
	apiKey  string
	dryRun  bool
	http    *http.Client
	log     *logrus.Logger
}

func NewLicenseClient(url, apiKey string, dryRun bool, logger *logrus.Logger) *LicenseClient {
	if logger == nil {
		logger = logrus.New()
	}
	return &LicenseClient{
		baseURL: strings.TrimRight(url, "/"),
		apiKey:  apiKey,
		dryRun:  dryRun,
		http:    &http.Client{Timeout: 30 * time.Second},
		log:     logger,
	}
}

// snipeResp is the {status, messages, payload} envelope returned by mutating endpoints.
type snipeResp struct {
	Status   string          `json:"status"`
	Messages json.RawMessage `json:"messages"`
	Payload  json.RawMessage `json:"payload"`
}

// check2xx returns a descriptive error for non-2xx responses, including the body,
// so an auth/rate-limit/validation failure (401/429/422) is not lost as an opaque
// JSON-unmarshal error.
func check2xx(status int, raw []byte, what string) error {
	if status < 200 || status >= 300 {
		return fmt.Errorf("%s: HTTP %d: %s", what, status, strings.TrimSpace(string(raw)))
	}
	return nil
}

// do issues an authenticated request to /api/v1<path> and returns the raw body, so
// list ({total,rows}) and mutation ({status,payload}) callers each decode what they need.
// Mutating callers must check c.dryRun BEFORE calling do.
func (c *LicenseClient) do(method, path string, body any) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.baseURL+"/api/v1"+path, rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	c.log.WithFields(logrus.Fields{"method": method, "path": path}).Debug("snipe license request")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return data, resp.StatusCode, nil
}

// ListLicenses returns all licenses (paginated).
func (c *LicenseClient) ListLicenses() ([]License, error) {
	var out []License
	offset := 0
	const limit = 100
	for {
		raw, status, err := c.do(http.MethodGet, fmt.Sprintf("/licenses?limit=%d&offset=%d", limit, offset), nil)
		if err != nil {
			return nil, err
		}
		if err := check2xx(status, raw, "listing licenses"); err != nil {
			return nil, err
		}
		var page struct {
			Total int `json:"total"`
			Rows  []struct {
				ID    int    `json:"id"`
				Name  string `json:"name"`
				Seats int    `json:"seats"`
			} `json:"rows"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, fmt.Errorf("listing licenses: %w", err)
		}
		for _, r := range page.Rows {
			out = append(out, License{ID: r.ID, Name: r.Name, Seats: r.Seats})
		}
		if len(page.Rows) == 0 || len(out) >= page.Total {
			break
		}
		offset += limit
	}
	return out, nil
}

// updateLicense PATCHes the mutable fields of an existing license so config changes
// (cost, category, reassignable, expiration) propagate on re-sync. config is source of truth.
func (c *LicenseClient) updateLicense(id int, spec LicenseSpec) error {
	body := map[string]any{
		"purchase_cost": spec.CostPerSeat,
		"category_id":   spec.CategoryID,
		"reassignable":  spec.Reassignable,
	}
	if spec.ExpirationDate != "" {
		body["expiration_date"] = spec.ExpirationDate
	} else {
		body["expiration_date"] = nil
	}
	raw, status, err := c.do(http.MethodPatch, fmt.Sprintf("/licenses/%d", id), body)
	if err != nil {
		return err
	}
	if err := check2xx(status, raw, fmt.Sprintf("updating license %d", id)); err != nil {
		return err
	}
	var r snipeResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("updating license %d: %w", id, err)
	}
	if r.Status != "success" {
		return fmt.Errorf("updating license %d: %s", id, string(r.Messages))
	}
	return nil
}

// EnsureLicense finds a license by name or creates it. On create it sets the
// category, seats, cost, reassignable flag, and (optional) expiration.
func (c *LicenseClient) EnsureLicense(spec LicenseSpec) (License, error) {
	existing, err := c.ListLicenses()
	if err != nil {
		return License{}, err
	}
	for _, l := range existing {
		if strings.EqualFold(l.Name, spec.Name) {
			if !c.dryRun {
				if err := c.updateLicense(l.ID, spec); err != nil {
					return License{}, err
				}
			}
			return l, nil
		}
	}
	if c.dryRun {
		return License{}, ErrDryRun
	}
	body := map[string]any{
		"name":          spec.Name,
		"seats":         max(spec.Seats, 1),
		"category_id":   spec.CategoryID,
		"reassignable":  spec.Reassignable,
		"purchase_cost": spec.CostPerSeat,
	}
	if spec.ExpirationDate != "" {
		body["expiration_date"] = spec.ExpirationDate
	}
	raw, status, err := c.do(http.MethodPost, "/licenses", body)
	if err != nil {
		return License{}, err
	}
	if err := check2xx(status, raw, fmt.Sprintf("creating license %q", spec.Name)); err != nil {
		return License{}, err
	}
	var r snipeResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return License{}, fmt.Errorf("creating license %q: %w", spec.Name, err)
	}
	if r.Status != "success" {
		return License{}, fmt.Errorf("creating license %q: %s", spec.Name, string(r.Messages))
	}
	var p struct {
		ID    int    `json:"id"`
		Name  string `json:"name"`
		Seats int    `json:"seats"`
	}
	if err := json.Unmarshal(r.Payload, &p); err != nil {
		return License{}, fmt.Errorf("parsing created license %q: %w", spec.Name, err)
	}
	return License{ID: p.ID, Name: p.Name, Seats: p.Seats}, nil
}

// ListSeats returns the license's seats and their current assignment.
func (c *LicenseClient) ListSeats(licenseID int) ([]LicenseSeat, error) {
	var out []LicenseSeat
	offset := 0
	const limit = 100
	for {
		raw, status, err := c.do(http.MethodGet, fmt.Sprintf("/licenses/%d/seats?limit=%d&offset=%d", licenseID, limit, offset), nil)
		if err != nil {
			return nil, err
		}
		if err := check2xx(status, raw, fmt.Sprintf("listing seats for license %d", licenseID)); err != nil {
			return nil, err
		}
		var page struct {
			Total int `json:"total"`
			Rows  []struct {
				ID           int `json:"id"`
				AssignedUser *struct {
					ID int `json:"id"`
				} `json:"assigned_user"`
				AssignedAsset *struct {
					ID int `json:"id"`
				} `json:"assigned_asset"`
			} `json:"rows"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, fmt.Errorf("listing seats for license %d: %w", licenseID, err)
		}
		for _, s := range page.Rows {
			seat := LicenseSeat{ID: s.ID}
			if s.AssignedUser != nil {
				seat.AssignedUserID = s.AssignedUser.ID
			}
			if s.AssignedAsset != nil {
				seat.AssignedAssetID = s.AssignedAsset.ID
			}
			out = append(out, seat)
		}
		if len(page.Rows) == 0 || len(out) >= page.Total {
			break
		}
		offset += limit
	}
	return out, nil
}

func (c *LicenseClient) patchSeat(licenseID, seatID int, body map[string]any) error {
	raw, status, err := c.do(http.MethodPatch, fmt.Sprintf("/licenses/%d/seats/%d", licenseID, seatID), body)
	if err != nil {
		return err
	}
	if err := check2xx(status, raw, fmt.Sprintf("seat %d on license %d", seatID, licenseID)); err != nil {
		return err
	}
	var r snipeResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("seat %d on license %d: %w", seatID, licenseID, err)
	}
	if r.Status != "success" {
		return fmt.Errorf("seat %d on license %d: %s", seatID, licenseID, string(r.Messages))
	}
	return nil
}

func (c *LicenseClient) CheckoutSeatToUser(licenseID, seatID, userID int) error {
	if c.dryRun {
		return ErrDryRun
	}
	return c.patchSeat(licenseID, seatID, map[string]any{"assigned_to": userID})
}
func (c *LicenseClient) CheckoutSeatToAsset(licenseID, seatID, assetID int) error {
	if c.dryRun {
		return ErrDryRun
	}
	return c.patchSeat(licenseID, seatID, map[string]any{"asset_id": assetID})
}
func (c *LicenseClient) CheckinSeat(licenseID, seatID int) error {
	if c.dryRun {
		return ErrDryRun
	}
	return c.patchSeat(licenseID, seatID, map[string]any{"assigned_to": nil, "asset_id": nil})
}

// EnsureSeats grows the license's seat total to at least total.
func (c *LicenseClient) EnsureSeats(licenseID, total int) error {
	if c.dryRun {
		return ErrDryRun
	}
	raw, status, err := c.do(http.MethodPatch, fmt.Sprintf("/licenses/%d", licenseID), map[string]any{"seats": total})
	if err != nil {
		return err
	}
	if err := check2xx(status, raw, fmt.Sprintf("growing license %d seats to %d", licenseID, total)); err != nil {
		return err
	}
	var r snipeResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("growing license %d seats to %d: %w", licenseID, total, err)
	}
	if r.Status != "success" {
		return fmt.Errorf("growing license %d seats to %d: %s", licenseID, total, string(r.Messages))
	}
	return nil
}
