package licensesync

import (
	"errors"
	"fmt"

	"github.com/sirupsen/logrus"

	"github.com/CampusTech/google2snipe/snipe"
)

// LicenseClient is the subset of the Snipe license client the engine needs.
// Seat methods take licenseID FIRST (matches *snipe.LicenseClient).
type LicenseClient interface {
	EnsureLicense(spec snipe.LicenseSpec) (snipe.License, error)
	EnsureSeats(licenseID, total int) error
	ListSeats(licenseID int) ([]snipe.LicenseSeat, error)
	CheckoutSeatToUser(licenseID, seatID, userID int) error
	CheckoutSeatToAsset(licenseID, seatID, assetID int) error
	CheckinSeat(licenseID, seatID int) error
}

// Target is a desired seat-holder (a user or an asset).
type Target struct {
	IsUser bool
	ID     int
}

// Stats summarizes a reconcile pass.
type Stats struct{ CheckedOut, CheckedIn, AlreadyOK int }

type Engine struct {
	lc  LicenseClient
	log *logrus.Logger
}

func New(lc LicenseClient, logger *logrus.Logger) *Engine {
	if logger == nil {
		logger = logrus.New()
	}
	return &Engine{lc: lc, log: logger}
}

func isDryRun(err error) bool { return errors.Is(err, snipe.ErrDryRun) }

// Reconcile ensures the license exists and its seats match the desired holders.
// Reassignable licenses check stale seats IN first (freeing them for reuse) before
// checking out new holders; non-reassignable (perpetual) licenses are additive and
// never reclaim seats. In dry-run, mutating client methods return snipe.ErrDryRun;
// Reconcile then logs the intended change and counts it without aborting.
func (e *Engine) Reconcile(spec snipe.LicenseSpec, desired []Target) (Stats, error) {
	// Deduplicate desired holders so the same user/asset is never seated twice
	// (e.g. two Workspace emails resolving to one Snipe user via the local-part fallback).
	if len(desired) > 1 {
		seen := make(map[[2]int]bool, len(desired))
		deduped := desired[:0:0]
		for _, t := range desired {
			k := [2]int{0, t.ID}
			if t.IsUser {
				k[0] = 1
			}
			if seen[k] {
				continue
			}
			seen[k] = true
			deduped = append(deduped, t)
		}
		desired = deduped
	}

	var st Stats
	lic, err := e.lc.EnsureLicense(spec)
	if isDryRun(err) {
		e.log.WithField("license", spec.Name).WithField("would_seat", len(desired)).
			Warn("[dry-run] would create license and seat holders")
		st.CheckedOut = len(desired)
		return st, nil
	}
	if err != nil {
		return st, err
	}

	wantUser := map[int]bool{}
	wantAsset := map[int]bool{}
	for _, t := range desired {
		if t.IsUser {
			wantUser[t.ID] = true
		} else {
			wantAsset[t.ID] = true
		}
	}

	seats, err := e.lc.ListSeats(lic.ID)
	if err != nil {
		return st, err
	}
	curUser := map[int][]int{}  // userID -> seatIDs (handles pre-existing duplicate seats)
	curAsset := map[int][]int{} // assetID -> seatIDs
	var free []int
	for _, s := range seats {
		switch {
		case s.AssignedUserID != 0:
			curUser[s.AssignedUserID] = append(curUser[s.AssignedUserID], s.ID)
		case s.AssignedAssetID != 0:
			curAsset[s.AssignedAssetID] = append(curAsset[s.AssignedAssetID], s.ID)
		default:
			free = append(free, s.ID)
		}
	}

	var firstErr error
	recordErr := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	// checkin returns true if the seat is now free (real success or dry-run no-op).
	checkin := func(seatID int) bool {
		if err := e.lc.CheckinSeat(lic.ID, seatID); err != nil && !isDryRun(err) {
			e.log.WithError(err).WithField("seat", seatID).Warn("seat checkin failed")
			recordErr(err)
			return false
		}
		st.CheckedIn++
		return true
	}

	// 1) Reassignable: reclaim stale holders' seats, and reclaim duplicate seats of a
	//    wanted holder (keep exactly one), freeing them for reuse. Perpetual never reclaims.
	if spec.Reassignable {
		for uid, seatIDs := range curUser {
			if !wantUser[uid] {
				var kept []int
				for _, seatID := range seatIDs {
					if checkin(seatID) {
						free = append(free, seatID)
					} else {
						kept = append(kept, seatID)
					}
				}
				if len(kept) == 0 {
					delete(curUser, uid)
				} else {
					curUser[uid] = kept
				}
			} else if len(seatIDs) > 1 {
				for _, seatID := range seatIDs[1:] {
					if checkin(seatID) {
						free = append(free, seatID)
					}
				}
				curUser[uid] = seatIDs[:1]
			}
		}
		for aid, seatIDs := range curAsset {
			if !wantAsset[aid] {
				var kept []int
				for _, seatID := range seatIDs {
					if checkin(seatID) {
						free = append(free, seatID)
					} else {
						kept = append(kept, seatID)
					}
				}
				if len(kept) == 0 {
					delete(curAsset, aid)
				} else {
					curAsset[aid] = kept
				}
			} else if len(seatIDs) > 1 {
				for _, seatID := range seatIDs[1:] {
					if checkin(seatID) {
						free = append(free, seatID)
					}
				}
				curAsset[aid] = seatIDs[:1]
			}
		}
	}

	// 2) Determine holders that still need a seat.
	var need []Target
	for _, t := range desired {
		has := false
		if t.IsUser {
			has = len(curUser[t.ID]) > 0
		} else {
			has = len(curAsset[t.ID]) > 0
		}
		if has {
			st.AlreadyOK++
		} else {
			need = append(need, t)
		}
	}

	// 3) Grow seats if there aren't enough free ones, then re-list to learn new seat IDs.
	growthDryRun := false
	if len(need) > len(free) {
		newTotal := len(seats) + (len(need) - len(free))
		switch err := e.lc.EnsureSeats(lic.ID, newTotal); {
		case err == nil:
			seats2, lerr := e.lc.ListSeats(lic.ID)
			if lerr != nil {
				return st, lerr
			}
			free = free[:0]
			for _, s := range seats2 {
				if s.AssignedUserID == 0 && s.AssignedAssetID == 0 {
					free = append(free, s.ID)
				}
			}
		case isDryRun(err):
			growthDryRun = true
		default:
			return st, err
		}
	}

	// 4) Check out the holders that need a seat.
	for _, t := range need {
		if len(free) == 0 {
			if growthDryRun {
				e.log.WithField("license", lic.Name).WithField("holder", t.ID).
					Warn("[dry-run] would add a seat and check out holder")
				st.CheckedOut++
			} else {
				e.log.WithField("license", lic.Name).WithField("holder", t.ID).
					Warn("no free seat available; checkout skipped")
				recordErr(fmt.Errorf("no free seat available for holder %d on license %q", t.ID, lic.Name))
			}
			continue
		}
		seatID := free[0]
		free = free[1:]
		var cerr error
		if t.IsUser {
			cerr = e.lc.CheckoutSeatToUser(lic.ID, seatID, t.ID)
		} else {
			cerr = e.lc.CheckoutSeatToAsset(lic.ID, seatID, t.ID)
		}
		if cerr != nil && !isDryRun(cerr) {
			e.log.WithError(cerr).WithField("holder", t.ID).Warn("seat checkout failed")
			recordErr(cerr)
			continue
		}
		st.CheckedOut++
	}

	return st, firstErr
}
