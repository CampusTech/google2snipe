package licensesync

import (
	"errors"
	"sync"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/CampusTech/google2snipe/snipe"
)

// stubLC is an in-memory LicenseClient. Its mutators are guarded by mu so the engine's
// concurrent seat checkin/checkout calls are exercised cleanly under `go test -race`.
type stubLC struct {
	mu       sync.Mutex
	lic      snipe.License
	seats    []snipe.LicenseSeat
	nextSeat int
}

func (s *stubLC) EnsureLicense(spec snipe.LicenseSpec) (snipe.License, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lic.ID == 0 {
		s.lic = snipe.License{ID: 1, Name: spec.Name, Seats: spec.Seats}
	}
	return s.lic, nil
}
func (s *stubLC) EnsureSeats(licenseID, total int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.seats) < total {
		s.nextSeat++
		s.seats = append(s.seats, snipe.LicenseSeat{ID: s.nextSeat})
	}
	s.lic.Seats = len(s.seats)
	return nil
}
func (s *stubLC) ListSeats(licenseID int) ([]snipe.LicenseSeat, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]snipe.LicenseSeat(nil), s.seats...), nil
}

// setSeat assigns (or clears) a seat's holder. Caller must hold s.mu.
func (s *stubLC) setSeat(seatID, userID, assetID int) {
	for i := range s.seats {
		if s.seats[i].ID == seatID {
			s.seats[i].AssignedUserID, s.seats[i].AssignedAssetID = userID, assetID
		}
	}
}
func (s *stubLC) CheckoutSeatToUser(licenseID, seatID, userID int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setSeat(seatID, userID, 0)
	return nil
}
func (s *stubLC) CheckoutSeatToAsset(licenseID, seatID, assetID int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setSeat(seatID, 0, assetID)
	return nil
}
func (s *stubLC) CheckinSeat(licenseID, seatID int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setSeat(seatID, 0, 0)
	return nil
}

func TestReconcileConcurrentCheckoutNoRace(t *testing.T) {
	const n = 300
	stub := &stubLC{lic: snipe.License{ID: 1, Name: "Big", Seats: n}, nextSeat: n}
	for i := 1; i <= n; i++ {
		stub.seats = append(stub.seats, snipe.LicenseSeat{ID: i})
	}
	e := New(stub, logrus.New(), WithConcurrency(8))
	targets := make([]Target, n)
	for i := range targets {
		targets[i] = Target{IsUser: false, ID: 1000 + i}
	}
	st, err := e.Reconcile(snipe.LicenseSpec{Name: "Big", Reassignable: false, Seats: n}, targets)
	if err != nil {
		t.Fatal(err)
	}
	if st.CheckedOut != n {
		t.Fatalf("CheckedOut = %d, want %d", st.CheckedOut, n)
	}
	heldBy := map[int]int{} // assetID -> seats held
	assigned := 0
	for _, s := range stub.seats {
		if s.AssignedAssetID != 0 {
			heldBy[s.AssignedAssetID]++
			assigned++
		}
	}
	if assigned != n {
		t.Fatalf("assigned %d seats, want %d (every device seated exactly once, no double-use)", assigned, n)
	}
	for aid, c := range heldBy {
		if c != 1 {
			t.Fatalf("asset %d holds %d seats, want exactly 1", aid, c)
		}
	}
}

func TestReconcileConcurrentCheckinNoRace(t *testing.T) {
	const n = 300
	// Reassignable license with all n seats held by stale users (none desired): every seat
	// is reclaimed concurrently.
	stub := &stubLC{lic: snipe.License{ID: 1, Name: "WS", Seats: n}, nextSeat: n}
	for i := 1; i <= n; i++ {
		stub.seats = append(stub.seats, snipe.LicenseSeat{ID: i, AssignedUserID: 90000 + i})
	}
	e := New(stub, logrus.New(), WithConcurrency(8))
	st, err := e.Reconcile(snipe.LicenseSpec{Name: "WS", Reassignable: true, Seats: n}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.CheckedIn != n {
		t.Fatalf("CheckedIn = %d, want %d", st.CheckedIn, n)
	}
	for _, s := range stub.seats {
		if s.AssignedUserID != 0 {
			t.Fatalf("seat %d still held by user %d, want all checked in", s.ID, s.AssignedUserID)
		}
	}
}

func TestReconcileReassignableCheckoutAndCheckin(t *testing.T) {
	stub := &stubLC{}
	// pre-seed: license with one seat already assigned to user 99 (stale)
	stub.lic = snipe.License{ID: 1, Name: "WS Plus", Seats: 1}
	stub.seats = []snipe.LicenseSeat{{ID: 1, AssignedUserID: 99}}
	stub.nextSeat = 1
	e := New(stub, logrus.New())
	// desired: users 10 and 20 (not 99)
	st, err := e.Reconcile(snipe.LicenseSpec{Name: "WS Plus", Reassignable: true, Seats: 1},
		[]Target{{IsUser: true, ID: 10}, {IsUser: true, ID: 20}})
	if err != nil {
		t.Fatal(err)
	}
	assigned := map[int]bool{}
	for _, s := range stub.seats {
		if s.AssignedUserID != 0 {
			assigned[s.AssignedUserID] = true
		}
	}
	if !assigned[10] || !assigned[20] || assigned[99] {
		t.Errorf("want {10,20} assigned, 99 checked in; got %v", assigned)
	}
	if st.CheckedOut != 2 || st.CheckedIn != 1 {
		t.Errorf("stats = %+v, want CheckedOut=2 CheckedIn=1", st)
	}
}

func TestReconcilePerpetualAdditiveNoCheckin(t *testing.T) {
	stub := &stubLC{lic: snipe.License{ID: 1, Name: "Chrome Perp", Seats: 1}, nextSeat: 1,
		seats: []snipe.LicenseSeat{{ID: 1, AssignedAssetID: 99}}} // stale asset 99
	e := New(stub, logrus.New())
	st, err := e.Reconcile(snipe.LicenseSpec{Name: "Chrome Perp", Reassignable: false, Seats: 1},
		[]Target{{IsUser: false, ID: 10}})
	if err != nil {
		t.Fatal(err)
	}
	// perpetual: asset 10 checked out, stale asset 99 NOT checked in
	stale := false
	for _, s := range stub.seats {
		if s.AssignedAssetID == 99 {
			stale = true
		}
	}
	if !stale {
		t.Error("perpetual license must NOT check in stale seats")
	}
	if st.CheckedIn != 0 {
		t.Errorf("perpetual CheckedIn = %d, want 0", st.CheckedIn)
	}
}

// dryStub: license already EXISTS; all mutators are dry-run no-ops (ErrDryRun).
type dryStub struct{ seats []snipe.LicenseSeat }

func (d *dryStub) EnsureLicense(spec snipe.LicenseSpec) (snipe.License, error) {
	return snipe.License{ID: 1, Name: spec.Name, Seats: len(d.seats)}, nil
}
func (d *dryStub) EnsureSeats(int, int) error                 { return snipe.ErrDryRun }
func (d *dryStub) ListSeats(int) ([]snipe.LicenseSeat, error) { return d.seats, nil }
func (d *dryStub) CheckoutSeatToUser(int, int, int) error     { return snipe.ErrDryRun }
func (d *dryStub) CheckoutSeatToAsset(int, int, int) error    { return snipe.ErrDryRun }
func (d *dryStub) CheckinSeat(int, int) error                 { return snipe.ErrDryRun }

// createDryStub: license does NOT exist; EnsureLicense itself returns ErrDryRun.
type createDryStub struct{}

func (createDryStub) EnsureLicense(snipe.LicenseSpec) (snipe.License, error) {
	return snipe.License{}, snipe.ErrDryRun
}
func (createDryStub) EnsureSeats(int, int) error                 { return snipe.ErrDryRun }
func (createDryStub) ListSeats(int) ([]snipe.LicenseSeat, error) { return nil, nil }
func (createDryStub) CheckoutSeatToUser(int, int, int) error     { return snipe.ErrDryRun }
func (createDryStub) CheckoutSeatToAsset(int, int, int) error    { return snipe.ErrDryRun }
func (createDryStub) CheckinSeat(int, int) error                 { return snipe.ErrDryRun }

func TestReconcileDryRunCountsWithoutError(t *testing.T) {
	d := &dryStub{seats: []snipe.LicenseSeat{{ID: 1}}} // one existing free seat
	st, err := New(d, logrus.New()).Reconcile(
		snipe.LicenseSpec{Name: "X", Reassignable: true, Seats: 1},
		[]Target{{IsUser: true, ID: 10}, {IsUser: true, ID: 20}})
	if err != nil {
		t.Fatalf("dry-run must not error: %v", err)
	}
	if st.CheckedOut != 2 {
		t.Errorf("CheckedOut = %d, want 2", st.CheckedOut)
	}
}

func TestReconcileDryRunCreate(t *testing.T) {
	st, err := New(createDryStub{}, logrus.New()).Reconcile(
		snipe.LicenseSpec{Name: "X", Reassignable: false, Seats: 0},
		[]Target{{IsUser: false, ID: 1}, {IsUser: false, ID: 2}})
	if err != nil {
		t.Fatalf("dry-run create must not error: %v", err)
	}
	if st.CheckedOut != 2 {
		t.Errorf("CheckedOut = %d, want 2", st.CheckedOut)
	}
}

func TestReconcileDedupsDuplicateHolders(t *testing.T) {
	stub := &stubLC{}
	e := New(stub, logrus.New())
	st, err := e.Reconcile(snipe.LicenseSpec{Name: "X", Reassignable: true, Seats: 1},
		[]Target{{IsUser: true, ID: 10}, {IsUser: true, ID: 10}}) // same holder twice
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, s := range stub.seats {
		if s.AssignedUserID == 10 {
			count++
		}
	}
	if count != 1 {
		t.Errorf("user 10 seated %d times, want 1", count)
	}
	if st.CheckedOut != 1 {
		t.Errorf("CheckedOut = %d, want 1", st.CheckedOut)
	}
}

func TestReconcileReclaimsDuplicateSeats(t *testing.T) {
	stub := &stubLC{
		lic:      snipe.License{ID: 1, Name: "X", Seats: 2},
		seats:    []snipe.LicenseSeat{{ID: 1, AssignedUserID: 10}, {ID: 2, AssignedUserID: 10}},
		nextSeat: 2,
	}
	e := New(stub, logrus.New())
	st, err := e.Reconcile(snipe.LicenseSpec{Name: "X", Reassignable: true, Seats: 2},
		[]Target{{IsUser: true, ID: 10}})
	if err != nil {
		t.Fatal(err)
	}
	held := 0
	for _, s := range stub.seats {
		if s.AssignedUserID == 10 {
			held++
		}
	}
	if held != 1 {
		t.Errorf("user 10 holds %d seats, want 1 (duplicate reclaimed)", held)
	}
	if st.CheckedIn != 1 {
		t.Errorf("CheckedIn = %d, want 1", st.CheckedIn)
	}
}

type failCheckoutStub struct{ stubLC }

func (s *failCheckoutStub) CheckoutSeatToUser(licenseID, seatID, userID int) error {
	return errors.New("boom")
}

func TestReconcileReturnsErrorOnRealCheckoutFailure(t *testing.T) {
	e := New(&failCheckoutStub{}, logrus.New())
	_, err := e.Reconcile(snipe.LicenseSpec{Name: "X", Reassignable: true, Seats: 1},
		[]Target{{IsUser: true, ID: 10}})
	if err == nil {
		t.Fatal("expected reconcile to return the real checkout failure, got nil")
	}
	if err.Error() != "boom" {
		t.Fatalf("expected the checkout error %q to propagate, got %v", "boom", err)
	}
}
