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
		raw, _, err := c.do(http.MethodGet, fmt.Sprintf("/licenses?limit=%d&offset=%d", limit, offset), nil)
		if err != nil {
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

// EnsureLicense finds a license by name or creates it. On create it sets the
// category, seats, cost, reassignable flag, and (optional) expiration.
func (c *LicenseClient) EnsureLicense(spec LicenseSpec) (License, error) {
	existing, err := c.ListLicenses()
	if err != nil {
		return License{}, err
	}
	for _, l := range existing {
		if strings.EqualFold(l.Name, spec.Name) {
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
	raw, _, err := c.do(http.MethodPost, "/licenses", body)
	if err != nil {
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
	_ = json.Unmarshal(r.Payload, &p)
	return License{ID: p.ID, Name: p.Name, Seats: p.Seats}, nil
}

// EnsureSeats grows the license's seat total to at least total.
func (c *LicenseClient) EnsureSeats(licenseID, total int) error {
	if c.dryRun {
		return ErrDryRun
	}
	raw, _, err := c.do(http.MethodPatch, fmt.Sprintf("/licenses/%d", licenseID), map[string]any{"seats": total})
	if err != nil {
		return err
	}
	var r snipeResp
	_ = json.Unmarshal(raw, &r)
	if r.Status != "success" {
		return fmt.Errorf("growing license %d seats to %d: %s", licenseID, total, string(r.Messages))
	}
	return nil
}
