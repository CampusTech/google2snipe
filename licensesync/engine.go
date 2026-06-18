package licensesync

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/sirupsen/logrus"

	"github.com/CampusTech/google2snipe/snipe"
)

// LicenseClient is the subset of the Snipe license client the engine needs.
// Seat methods take licenseID FIRST (matches *snipe.LicenseClient). Each method
// takes a context so a Ctrl-C (SIGINT/SIGTERM) cancels in-flight requests and
// aborts retry backoff instead of hard-killing the process mid-reconcile.
type LicenseClient interface {
	EnsureLicense(ctx context.Context, spec snipe.LicenseSpec) (snipe.License, error)
	EnsureSeats(ctx context.Context, licenseID, total int) error
	ListSeats(ctx context.Context, licenseID int) ([]snipe.LicenseSeat, error)
	CheckoutSeatToUser(ctx context.Context, licenseID, seatID, userID int) error
	CheckoutSeatToAsset(ctx context.Context, licenseID, seatID, assetID int) error
	CheckinSeat(ctx context.Context, licenseID, seatID int) error
}

// Target is a desired seat-holder (a user or an asset).
type Target struct {
	IsUser bool
	ID     int
}

// Stats summarizes a reconcile pass.
type Stats struct{ CheckedOut, CheckedIn, AlreadyOK int }

type Engine struct {
	lc          LicenseClient
	log         *logrus.Logger
	concurrency int
}

// Option configures an Engine.
type Option func(*Engine)

// WithConcurrency bounds how many seat checkin/checkout API calls run at once during a
// Reconcile. Values < 1 are treated as 1 (fully serial).
func WithConcurrency(n int) Option { return func(e *Engine) { e.concurrency = n } }

func New(lc LicenseClient, logger *logrus.Logger, opts ...Option) *Engine {
	if logger == nil {
		logger = logrus.New()
	}
	e := &Engine{lc: lc, log: logger, concurrency: 1}
	for _, o := range opts {
		o(e)
	}
	if e.concurrency < 1 {
		e.concurrency = 1
	}
	return e
}

func isDryRun(err error) bool { return errors.Is(err, snipe.ErrDryRun) }

// parallelFor runs fn(0..n-1) across at most `workers` goroutines and blocks until all
// complete. fn must be safe for concurrent calls across distinct i; callers here give each
// i its own result slot and a distinct seat so there is no shared mutable state.
func parallelFor(n, workers int, fn func(i int)) {
	if n <= 0 {
		return
	}
	if workers < 1 {
		workers = 1
	}
	if workers > n {
		workers = n
	}
	if workers == 1 {
		for i := 0; i < n; i++ {
			fn(i)
		}
		return
	}
	var wg sync.WaitGroup
	jobs := make(chan int)
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for i := range jobs {
				fn(i)
			}
		}()
	}
	for i := 0; i < n; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
}

// Reconcile ensures the license exists and its seats match the desired holders.
// Reassignable licenses check stale seats IN first (freeing them for reuse) before
// checking out new holders; non-reassignable (perpetual) licenses are additive and
// never reclaim seats. In dry-run, mutating client methods return snipe.ErrDryRun;
// Reconcile then logs the intended change and counts it without aborting.
func (e *Engine) Reconcile(ctx context.Context, spec snipe.LicenseSpec, desired []Target) (Stats, error) {
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
	lic, err := e.lc.EnsureLicense(ctx, spec)
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

	seats, err := e.lc.ListSeats(ctx, lic.ID)
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
	workers := e.concurrency

	// 1) Reassignable: reclaim stale holders' seats, and reclaim duplicate seats of a
	//    wanted holder (keep exactly one), freeing them for reuse. Perpetual never reclaims.
	//    Choosing which seats to reclaim is pure bookkeeping; the check-in API calls then run
	//    concurrently. A failed check-in only keeps that seat out of the reuse pool — it never
	//    changes whether a *desired* holder is already seated, because reclaim only touches
	//    unwanted holders and a wanted holder's surplus duplicates (it always keeps one).
	var toCheckin []int
	if spec.Reassignable {
		for uid, seatIDs := range curUser {
			if !wantUser[uid] {
				toCheckin = append(toCheckin, seatIDs...)
			} else if len(seatIDs) > 1 {
				toCheckin = append(toCheckin, seatIDs[1:]...)
			}
		}
		for aid, seatIDs := range curAsset {
			if !wantAsset[aid] {
				toCheckin = append(toCheckin, seatIDs...)
			} else if len(seatIDs) > 1 {
				toCheckin = append(toCheckin, seatIDs[1:]...)
			}
		}
	}
	if len(toCheckin) > 0 {
		freed := make([]bool, len(toCheckin))
		cerrs := make([]error, len(toCheckin))
		parallelFor(len(toCheckin), workers, func(i int) {
			// Stop issuing new seat PATCHes promptly once ctx is cancelled (Ctrl-C);
			// the slot stays zero-valued so it's neither counted nor errored.
			if ctx.Err() != nil {
				return
			}
			// Worker invariant: write ONLY this i's slots (freed[i]/cerrs[i]). st.CheckedIn,
			// free, and firstErr are aggregated single-threaded after parallelFor returns.
			seatID := toCheckin[i]
			// A dry-run check-in returns ErrDryRun; treat it as a (pretended) freed seat so
			// the dry-run reports the same intended reuse a real run would.
			if err := e.lc.CheckinSeat(ctx, lic.ID, seatID); err != nil && !isDryRun(err) {
				e.log.WithError(err).WithField("seat", seatID).Warn("seat checkin failed")
				cerrs[i] = err
				return
			}
			freed[i] = true
		})
		for i, ok := range freed {
			if ok {
				st.CheckedIn++
				free = append(free, toCheckin[i])
			}
		}
		for _, er := range cerrs {
			if er != nil {
				recordErr(er)
			}
		}
	}

	// 2) Determine holders that still need a seat. A desired holder that currently holds at
	//    least one seat is already OK (reclaim above only removed unwanted holders / surplus
	//    duplicates, so a wanted holder that held a seat still holds one).
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
		switch err := e.lc.EnsureSeats(ctx, lic.ID, newTotal); {
		case err == nil:
			seats2, lerr := e.lc.ListSeats(ctx, lic.ID)
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

	// 4) Check out holders that need a seat. Pre-pair each holder with a distinct free seat
	//    so concurrent workers never contend for the same seat, then run the checkouts in
	//    parallel. Holders beyond the free-seat supply are handled afterward (dry-run growth
	//    counts them as intended; otherwise each is a "no free seat" error).
	k := len(need)
	if len(free) < k {
		k = len(free)
	}
	if k > 0 {
		seatFor := make([]int, k)
		copy(seatFor, free[:k])
		coOK := make([]bool, k)
		coErr := make([]error, k)
		parallelFor(k, workers, func(i int) {
			// Stop issuing new seat PATCHes promptly once ctx is cancelled (Ctrl-C);
			// the slot stays zero-valued so it's neither counted nor errored.
			if ctx.Err() != nil {
				return
			}
			// Worker invariant: write ONLY this i's slots (coOK[i]/coErr[i]) and read its
			// pre-assigned seatFor[i]. st.CheckedOut/firstErr are aggregated after the barrier.
			t := need[i]
			seatID := seatFor[i]
			var cerr error
			if t.IsUser {
				cerr = e.lc.CheckoutSeatToUser(ctx, lic.ID, seatID, t.ID)
			} else {
				cerr = e.lc.CheckoutSeatToAsset(ctx, lic.ID, seatID, t.ID)
			}
			if cerr != nil && !isDryRun(cerr) {
				e.log.WithError(cerr).WithField("holder", t.ID).Warn("seat checkout failed")
				coErr[i] = cerr
				return
			}
			coOK[i] = true
		})
		for i := 0; i < k; i++ {
			if coOK[i] {
				st.CheckedOut++
			}
			if coErr[i] != nil {
				recordErr(coErr[i])
			}
		}
	}
	for _, t := range need[k:] {
		if growthDryRun {
			e.log.WithField("license", lic.Name).WithField("holder", t.ID).
				Warn("[dry-run] would add a seat and check out holder")
			st.CheckedOut++
		} else {
			e.log.WithField("license", lic.Name).WithField("holder", t.ID).
				Warn("no free seat available; checkout skipped")
			recordErr(fmt.Errorf("no free seat available for holder %d on license %q", t.ID, lic.Name))
		}
	}

	return st, firstErr
}
