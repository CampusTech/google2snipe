package snipe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
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
//
// It retries on rate-limiting and transient failures (honoring Retry-After, then an
// exponential backoff, both capped at maxBackoff). A 429 is always retried — it was
// rate-limited and never processed. A 5xx or transient network error is retried only for
// idempotent methods. CONTRACT: every PATCH caller in this package MUST send an
// absolute/idempotent body (updateLicense, patchSeat, EnsureSeats all do); do not add a
// relative-mutation PATCH without making do() skip retries for it. POST (license/category
// create) is never retried on 5xx/network errors so a create that may have succeeded is not
// replayed.
func (c *LicenseClient) do(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var bodyBytes []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		bodyBytes = b
	}
	idempotent := method != http.MethodPost
	const maxAttempts = 6
	backoff := 500 * time.Millisecond

	for attempt := 1; ; attempt++ {
		var rdr io.Reader
		if bodyBytes != nil {
			rdr = bytes.NewReader(bodyBytes)
		}
		// The caller-supplied ctx (rooted at a signal.NotifyContext in cmd) lets a Ctrl-C
		// abort the in-flight request and any backoff sleep instead of hard-killing the run.
		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+"/api/v1"+path, rdr)
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Content-Type", "application/json")
		c.log.WithFields(logrus.Fields{"method": method, "path": path, "attempt": attempt}).Debug("snipe license request")

		var (
			data   []byte
			status int
			wait   time.Duration
			retry  bool
			reason string
		)
		resp, err := c.http.Do(req)
		if err != nil {
			// A cancelled/deadline-exceeded ctx is not a transient failure — abort
			// immediately rather than retrying a request the caller no longer wants.
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, 0, err
			}
			if !idempotent || attempt >= maxAttempts {
				return nil, 0, fmt.Errorf("%s %s: after %d attempt(s): %w", method, path, attempt, err)
			}
			retry, wait, reason = true, backoff, err.Error()
		} else {
			var rerr error
			data, rerr = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if rerr != nil {
				return data, resp.StatusCode, rerr
			}
			status = resp.StatusCode
			if (status == http.StatusTooManyRequests || (status >= 500 && idempotent)) && attempt < maxAttempts {
				retry, wait, reason = true, backoff, fmt.Sprintf("HTTP %d", status)
				if d, ok := retryAfterDuration(resp.Header); ok {
					wait = d
				}
			}
		}
		if !retry {
			return data, status, nil
		}
		c.log.WithFields(logrus.Fields{"method": method, "path": path, "attempt": attempt, "wait": wait.String(), "reason": reason}).
			Warn("snipe license request failed (429/5xx/transient); backing off")
		// Cancel-aware backoff: a Ctrl-C (SIGINT/SIGTERM) cancels ctx so we abort the
		// sleep promptly instead of waiting out the full Retry-After/exponential wait.
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-time.After(wait):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// maxBackoff caps both the exponential retry backoff and any server-provided Retry-After, so
// a single large (or malformed-but-numeric) Retry-After can't stall the whole sync for hours.
const maxBackoff = 30 * time.Second

// retryAfterDuration parses a Retry-After header given in whole seconds (delta-seconds form
// only; an HTTP-date value is treated as absent and falls back to exponential backoff). The
// bool reports whether a valid value was present (a present "0" means retry immediately,
// distinct from no header at all). The returned wait is clamped to maxBackoff.
func retryAfterDuration(h http.Header) (time.Duration, bool) {
	ra := strings.TrimSpace(h.Get("Retry-After"))
	if ra == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(ra); err == nil && secs >= 0 {
		d := time.Duration(secs) * time.Second
		if d > maxBackoff {
			d = maxBackoff
		}
		return d, true
	}
	return 0, false
}

// EnsureLicenseCategory finds a Snipe-IT category of type "license" by name
// (case-insensitive) or creates it, returning its id.
func (c *LicenseClient) EnsureLicenseCategory(ctx context.Context, name string) (int, error) {
	offset := 0
	const limit = 100
	for {
		raw, status, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/categories?limit=%d&offset=%d", limit, offset), nil)
		if err != nil {
			return 0, err
		}
		if err := check2xx(status, raw, "listing categories"); err != nil {
			return 0, err
		}
		var page struct {
			Total int `json:"total"`
			Rows  []struct {
				ID           int    `json:"id"`
				Name         string `json:"name"`
				CategoryType string `json:"category_type"`
			} `json:"rows"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return 0, fmt.Errorf("listing categories: %w", err)
		}
		for _, r := range page.Rows {
			if strings.EqualFold(r.CategoryType, "license") && strings.EqualFold(r.Name, name) {
				return r.ID, nil
			}
		}
		offset += len(page.Rows)
		if len(page.Rows) == 0 || offset >= page.Total {
			break
		}
	}
	if c.dryRun {
		return 0, ErrDryRun
	}
	raw, status, err := c.do(ctx, http.MethodPost, "/categories", map[string]any{"name": name, "category_type": "license"})
	if err != nil {
		return 0, err
	}
	if err := check2xx(status, raw, fmt.Sprintf("creating license category %q", name)); err != nil {
		return 0, err
	}
	var r snipeResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return 0, fmt.Errorf("creating license category %q: %w", name, err)
	}
	if r.Status != "success" {
		return 0, fmt.Errorf("creating license category %q: %s", name, string(r.Messages))
	}
	var p struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(r.Payload, &p); err != nil {
		return 0, fmt.Errorf("parsing created category %q: %w", name, err)
	}
	return p.ID, nil
}

// ListLicenses returns all licenses (paginated).
func (c *LicenseClient) ListLicenses(ctx context.Context) ([]License, error) {
	var out []License
	offset := 0
	const limit = 100
	for {
		raw, status, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/licenses?limit=%d&offset=%d", limit, offset), nil)
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
func (c *LicenseClient) updateLicense(ctx context.Context, id int, spec LicenseSpec) error {
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
	raw, status, err := c.do(ctx, http.MethodPatch, fmt.Sprintf("/licenses/%d", id), body)
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
func (c *LicenseClient) EnsureLicense(ctx context.Context, spec LicenseSpec) (License, error) {
	existing, err := c.ListLicenses(ctx)
	if err != nil {
		return License{}, err
	}
	for _, l := range existing {
		if strings.EqualFold(l.Name, spec.Name) {
			if !c.dryRun {
				if err := c.updateLicense(ctx, l.ID, spec); err != nil {
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
		"name": spec.Name,
		// On create the license has 0 seats, so Snipe-IT's limit_change rule bounds the
		// seats field to 1..maxSeatsPerChange. Clamp here; EnsureSeats grows the rest in steps.
		"seats":         min(max(spec.Seats, 1), maxSeatsPerChange),
		"category_id":   spec.CategoryID,
		"reassignable":  spec.Reassignable,
		"purchase_cost": spec.CostPerSeat,
	}
	if spec.ExpirationDate != "" {
		body["expiration_date"] = spec.ExpirationDate
	}
	raw, status, err := c.do(ctx, http.MethodPost, "/licenses", body)
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
func (c *LicenseClient) ListSeats(ctx context.Context, licenseID int) ([]LicenseSeat, error) {
	var out []LicenseSeat
	offset := 0
	const limit = 500
	for {
		raw, status, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/licenses/%d/seats?limit=%d&offset=%d", licenseID, limit, offset), nil)
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

func (c *LicenseClient) patchSeat(ctx context.Context, licenseID, seatID int, body map[string]any) error {
	raw, status, err := c.do(ctx, http.MethodPatch, fmt.Sprintf("/licenses/%d/seats/%d", licenseID, seatID), body)
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

func (c *LicenseClient) CheckoutSeatToUser(ctx context.Context, licenseID, seatID, userID int) error {
	if c.dryRun {
		return ErrDryRun
	}
	return c.patchSeat(ctx, licenseID, seatID, map[string]any{"assigned_to": userID})
}
func (c *LicenseClient) CheckoutSeatToAsset(ctx context.Context, licenseID, seatID, assetID int) error {
	if c.dryRun {
		return ErrDryRun
	}
	return c.patchSeat(ctx, licenseID, seatID, map[string]any{"asset_id": assetID})
}
func (c *LicenseClient) CheckinSeat(ctx context.Context, licenseID, seatID int) error {
	if c.dryRun {
		return ErrDryRun
	}
	return c.patchSeat(ctx, licenseID, seatID, map[string]any{"assigned_to": nil, "asset_id": nil})
}

// maxSeatsPerChange mirrors Snipe-IT's `limit_change:10000` rule on a license's seats field
// (app/Models/License.php prepareLimitChangeRule): a single create/update may change the seat
// count by at most this much relative to the license's CURRENT seat-record count. It is NOT
// an absolute cap on total seats — larger totals are reached by growing in repeated steps.
const maxSeatsPerChange = 10000

// EnsureSeats grows the license's seat total to at least total, stepping in increments no
// larger than maxSeatsPerChange so Snipe-IT's per-change limit never rejects the request.
// A license with, say, 25000 seats is reached as 10000 (create) -> 20000 -> 25000.
func (c *LicenseClient) EnsureSeats(ctx context.Context, licenseID, total int) error {
	if c.dryRun {
		return ErrDryRun
	}
	current, err := c.licenseSeatCount(ctx, licenseID)
	if err != nil {
		return err
	}
	for current < total {
		next := min(current+maxSeatsPerChange, total)
		if err := c.patchLicenseSeats(ctx, licenseID, next); err != nil {
			return err
		}
		current = next
	}
	return nil
}

// patchLicenseSeats sets the license's seat total in one request. The caller must keep each
// change within maxSeatsPerChange of the current seat count.
func (c *LicenseClient) patchLicenseSeats(ctx context.Context, licenseID, seats int) error {
	raw, status, err := c.do(ctx, http.MethodPatch, fmt.Sprintf("/licenses/%d", licenseID), map[string]any{"seats": seats})
	if err != nil {
		return err
	}
	if err := check2xx(status, raw, fmt.Sprintf("growing license %d seats to %d", licenseID, seats)); err != nil {
		return err
	}
	var r snipeResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("growing license %d seats to %d: %w", licenseID, seats, err)
	}
	if r.Status != "success" {
		return fmt.Errorf("growing license %d seats to %d: %s", licenseID, seats, string(r.Messages))
	}
	return nil
}

// licenseSeatCount returns a license's current seat total via GET /licenses/{id}, which Snipe
// returns as the bare license object.
func (c *LicenseClient) licenseSeatCount(ctx context.Context, licenseID int) (int, error) {
	raw, status, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/licenses/%d", licenseID), nil)
	if err != nil {
		return 0, err
	}
	if err := check2xx(status, raw, fmt.Sprintf("reading license %d", licenseID)); err != nil {
		return 0, err
	}
	var lic struct {
		Seats int `json:"seats"`
	}
	if err := json.Unmarshal(raw, &lic); err != nil {
		return 0, fmt.Errorf("reading license %d seats: %w", licenseID, err)
	}
	return lic.Seats, nil
}
