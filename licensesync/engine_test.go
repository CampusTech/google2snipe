package licensesync

import (
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/CampusTech/google2snipe/snipe"
)

// stubLC is an in-memory LicenseClient.
type stubLC struct {
	lic      snipe.License
	seats    []snipe.LicenseSeat
	nextSeat int
}

func (s *stubLC) EnsureLicense(spec snipe.LicenseSpec) (snipe.License, error) {
	if s.lic.ID == 0 {
		s.lic = snipe.License{ID: 1, Name: spec.Name, Seats: spec.Seats}
	}
	return s.lic, nil
}
func (s *stubLC) EnsureSeats(licenseID, total int) error {
	for len(s.seats) < total {
		s.nextSeat++
		s.seats = append(s.seats, snipe.LicenseSeat{ID: s.nextSeat})
	}
	s.lic.Seats = len(s.seats)
	return nil
}
func (s *stubLC) ListSeats(licenseID int) ([]snipe.LicenseSeat, error) { return s.seats, nil }
func (s *stubLC) setUser(seatID, uid int) {
	for i := range s.seats {
		if s.seats[i].ID == seatID {
			s.seats[i].AssignedUserID, s.seats[i].AssignedAssetID = uid, 0
		}
	}
}
func (s *stubLC) CheckoutSeatToUser(licenseID, seatID, userID int) error {
	s.setUser(seatID, userID)
	return nil
}
func (s *stubLC) CheckoutSeatToAsset(licenseID, seatID, assetID int) error {
	for i := range s.seats {
		if s.seats[i].ID == seatID {
			s.seats[i].AssignedAssetID, s.seats[i].AssignedUserID = assetID, 0
		}
	}
	return nil
}
func (s *stubLC) CheckinSeat(licenseID, seatID int) error {
	for i := range s.seats {
		if s.seats[i].ID == seatID {
			s.seats[i].AssignedUserID, s.seats[i].AssignedAssetID = 0, 0
		}
	}
	return nil
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
