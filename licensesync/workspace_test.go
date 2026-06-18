package licensesync

import (
	"context"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/CampusTech/google2snipe/config"
	"github.com/CampusTech/google2snipe/google"
)

func TestSyncWorkspacePerSKUSeatPerUser(t *testing.T) {
	stub := &stubLC{}
	e := New(stub, logrus.New())
	cfg := config.LicensesConfig{
		Enabled: true, DefaultLicenseCategoryID: 7,
		Workspace: config.WorkspaceLicenseConfig{SKUCosts: map[string]float64{"1010310008": 5}},
	}
	asg := []google.LicenseAssignment{
		{UserEmail: "a@x.edu", SKUID: "1010310008", SKUName: "Education Plus"},
		{UserEmail: "b@x.edu", SKUID: "1010310008", SKUName: "Education Plus"},
	}
	uid := map[string]int{"a@x.edu": 10, "b@x.edu": 20}
	err := e.SyncWorkspace(context.Background(), cfg, asg, func(email string) (int, bool) { id, ok := uid[email]; return id, ok })
	if err != nil {
		t.Fatal(err)
	}
	users := map[int]bool{}
	for _, s := range stub.seats {
		if s.AssignedUserID != 0 {
			users[s.AssignedUserID] = true
		}
	}
	if !users[10] || !users[20] {
		t.Errorf("want users 10,20 seated; got %v", users)
	}
}
