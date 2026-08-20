package snipe

import (
	"context"
	"fmt"
	"strings"
	"time"

	snipeit "github.com/michellepellon/go-snipeit"
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

// LicenseClient manages Snipe-IT licenses and their seats.
//
// It borrows the shared *Client's go-snipeit connection, so license traffic is
// paced by the same rate limiter and retried by the same policy as asset
// traffic — the two used to run on separate HTTP clients and each spend the
// instance's request budget without knowing about the other.
type LicenseClient struct {
	sc     *snipeit.Client
	dryRun bool
	log    *logrus.Logger
}

// NewLicenseClient returns a license client that shares c's connection,
// rate limiter, and dry-run setting.
func NewLicenseClient(c *Client) *LicenseClient {
	return &LicenseClient{sc: c.sc, dryRun: c.dryRun, log: c.logger}
}

// apiErr turns a non-success envelope into an error carrying the API's message,
// so a validation/permission failure is not lost as a bare "not success".
func apiErr(what string, r snipeit.Response) error {
	return fmt.Errorf("%s: %s", what, r.Message.String())
}

// EnsureLicenseCategory finds a Snipe-IT category of type "license" by name
// (case-insensitive) or creates it, returning its id.
func (c *LicenseClient) EnsureLicenseCategory(ctx context.Context, name string) (int, error) {
	offset := 0
	const limit = 100
	for {
		page, _, err := c.sc.Categories.ListContext(ctx, &snipeit.ListOptions{Limit: limit, Offset: offset})
		if err != nil {
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
	created, _, err := c.sc.Categories.CreateContext(ctx, snipeit.Category{
		CommonFields: snipeit.CommonFields{Name: name},
		CategoryType: "license",
	})
	if err != nil {
		return 0, fmt.Errorf("creating license category %q: %w", name, err)
	}
	if created.Status != "success" {
		return 0, apiErr(fmt.Sprintf("creating license category %q", name), created.Response)
	}
	return created.Payload.ID, nil
}

// ListLicenses returns all licenses (paginated).
func (c *LicenseClient) ListLicenses(ctx context.Context) ([]License, error) {
	var out []License
	offset := 0
	const limit = 100
	for {
		page, _, err := c.sc.Licenses.ListContext(ctx, &snipeit.ListOptions{Limit: limit, Offset: offset})
		if err != nil {
			return nil, fmt.Errorf("listing licenses: %w", err)
		}
		for _, l := range page.Rows {
			out = append(out, License{ID: l.ID, Name: l.Name, Seats: l.Seats})
		}
		if len(page.Rows) == 0 || len(out) >= page.Total {
			break
		}
		offset += limit
	}
	return out, nil
}

// toSnipeLicense renders a spec as the license body Snipe-IT expects. seats is
// passed separately because create and update bound it differently.
func toSnipeLicense(spec LicenseSpec, seats int) snipeit.License {
	l := snipeit.License{
		CommonFields: snipeit.CommonFields{Name: spec.Name},
		Seats:        seats,
		CategoryID:   spec.CategoryID,
		Reassignable: snipeit.FlexBool(spec.Reassignable),
		PurchaseCost: fmt.Sprintf("%.2f", spec.CostPerSeat),
	}
	// A nil date leaves the stored expiration alone; a zero one clears it, which
	// is what an emptied config value must do.
	if spec.ExpirationDate != "" {
		if t, err := time.Parse("2006-01-02", spec.ExpirationDate); err == nil {
			l.ExpirationDate = &snipeit.SnipeTime{Time: t}
		}
	} else {
		l.ExpirationDate = &snipeit.SnipeTime{}
	}
	return l
}

// updateLicense PATCHes the mutable fields of an existing license so config changes
// (cost, category, reassignable, expiration) propagate on re-sync. config is source of truth.
func (c *LicenseClient) updateLicense(ctx context.Context, id int, spec LicenseSpec) error {
	// Seats are grown by EnsureSeats in bounded steps; leave them out here.
	resp, _, err := c.sc.Licenses.UpdateContext(ctx, id, toSnipeLicense(spec, 0))
	if err != nil {
		return fmt.Errorf("updating license %d: %w", id, err)
	}
	if resp.Status != "success" {
		return apiErr(fmt.Sprintf("updating license %d", id), resp.Response)
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
	// On create the license has 0 seats, so Snipe-IT's limit_change rule bounds the
	// seats field to 1..maxSeatsPerChange. Clamp here; EnsureSeats grows the rest in steps.
	seats := min(max(spec.Seats, 1), maxSeatsPerChange)
	created, _, err := c.sc.Licenses.CreateContext(ctx, toSnipeLicense(spec, seats))
	if err != nil {
		return License{}, fmt.Errorf("creating license %q: %w", spec.Name, err)
	}
	if created.Status != "success" {
		return License{}, apiErr(fmt.Sprintf("creating license %q", spec.Name), created.Response)
	}
	p := created.Payload
	return License{ID: p.ID, Name: p.Name, Seats: p.Seats}, nil
}

// ListSeats returns the license's seats and their current assignment.
func (c *LicenseClient) ListSeats(ctx context.Context, licenseID int) ([]LicenseSeat, error) {
	var out []LicenseSeat
	offset := 0
	const limit = 500
	for {
		page, _, err := c.sc.Licenses.ListSeatsContext(ctx, licenseID, &snipeit.ListOptions{Limit: limit, Offset: offset})
		if err != nil {
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

func (c *LicenseClient) CheckoutSeatToUser(ctx context.Context, licenseID, seatID, userID int) error {
	if c.dryRun {
		return ErrDryRun
	}
	resp, _, err := c.sc.Licenses.CheckoutSeatToUserContext(ctx, licenseID, seatID, userID)
	return seatResult(err, resp, licenseID, seatID)
}

func (c *LicenseClient) CheckoutSeatToAsset(ctx context.Context, licenseID, seatID, assetID int) error {
	if c.dryRun {
		return ErrDryRun
	}
	resp, _, err := c.sc.Licenses.CheckoutSeatToAssetContext(ctx, licenseID, seatID, assetID)
	return seatResult(err, resp, licenseID, seatID)
}

func (c *LicenseClient) CheckinSeat(ctx context.Context, licenseID, seatID int) error {
	if c.dryRun {
		return ErrDryRun
	}
	resp, _, err := c.sc.Licenses.CheckinSeatContext(ctx, licenseID, seatID)
	return seatResult(err, resp, licenseID, seatID)
}

// seatResult folds a seat call's transport error and API envelope into one error.
func seatResult(err error, resp *snipeit.Response, licenseID, seatID int) error {
	what := fmt.Sprintf("seat %d on license %d", seatID, licenseID)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if resp.Status != "success" {
		return apiErr(what, *resp)
	}
	return nil
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
	what := fmt.Sprintf("growing license %d seats to %d", licenseID, seats)
	resp, _, err := c.sc.Licenses.UpdateContext(ctx, licenseID, snipeit.License{Seats: seats})
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if resp.Status != "success" {
		return apiErr(what, resp.Response)
	}
	return nil
}

// licenseSeatCount returns a license's current seat total.
func (c *LicenseClient) licenseSeatCount(ctx context.Context, licenseID int) (int, error) {
	lic, _, err := c.sc.Licenses.GetContext(ctx, licenseID)
	if err != nil {
		return 0, fmt.Errorf("reading license %d: %w", licenseID, err)
	}
	return lic.Seats, nil
}
