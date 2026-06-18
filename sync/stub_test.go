package sync

import (
	"encoding/json"
	"sync"
	"testing"

	admin "google.golang.org/api/admin/directory/v1"

	"github.com/CampusTech/google2snipe/snipe"
)

func mustJSON(t *testing.T, d *admin.ChromeOsDevice) []byte {
	t.Helper()
	b, err := json.Marshal([]*admin.ChromeOsDevice{d})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// stubSnipe is an in-memory SnipeClient for engine tests.
type stubSnipe struct {
	mu           sync.Mutex
	bySerial     map[string][]snipe.Asset
	created      []snipe.Asset
	patched      map[int]snipe.Asset
	checkouts    map[int]int // assetID -> userID
	checkins     []int       // assetIDs that were checked in
	models       []snipe.Model
	manufs       []snipe.Manufacturer
	users        []snipe.User
	statusLabels []snipe.StatusLabel
	nextID       int
}

func (s *stubSnipe) GetAssetBySerial(serial string) ([]snipe.Asset, error) {
	return s.bySerial[serial], nil
}
func (s *stubSnipe) CreateAsset(a snipe.Asset) (snipe.Asset, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	a.ID = s.nextID
	s.created = append(s.created, a)
	return a, nil
}
func (s *stubSnipe) PatchAsset(id int, a snipe.Asset) (snipe.Asset, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.patched == nil {
		s.patched = map[int]snipe.Asset{}
	}
	a.ID = id
	s.patched[id] = a
	return a, nil
}
func (s *stubSnipe) CheckoutAssetToUser(assetID, userID int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.checkouts == nil {
		s.checkouts = map[int]int{}
	}
	s.checkouts[assetID] = userID
	return nil
}
func (s *stubSnipe) CheckinAsset(assetID int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkins = append(s.checkins, assetID)
	return nil
}
func (s *stubSnipe) ListAllModels() ([]snipe.Model, error) { return s.models, nil }
func (s *stubSnipe) CreateModel(m snipe.Model) (snipe.Model, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	m.ID = s.nextID
	s.models = append(s.models, m)
	return m, nil
}
func (s *stubSnipe) ListAllManufacturers() ([]snipe.Manufacturer, error) { return s.manufs, nil }
func (s *stubSnipe) CreateManufacturer(name string) (snipe.Manufacturer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	m := snipe.Manufacturer{ID: s.nextID, Name: name}
	s.manufs = append(s.manufs, m)
	return m, nil
}
func (s *stubSnipe) ListAllUsers() ([]snipe.User, error) { return s.users, nil }
func (s *stubSnipe) ListAllStatusLabels() ([]snipe.StatusLabel, error) {
	return s.statusLabels, nil
}
func (s *stubSnipe) ListAllAssets() ([]snipe.Asset, error) {
	var out []snipe.Asset
	for _, list := range s.bySerial {
		out = append(out, list...)
	}
	return out, nil
}
