package licensesync

import (
	"github.com/CampusTech/google2snipe/config"
	"github.com/CampusTech/google2snipe/google"
	"github.com/CampusTech/google2snipe/snipe"
)

// SyncWorkspace reconciles Workspace user subscriptions: one Snipe License per
// SKU (reassignable), a seat per assigned user.
func (e *Engine) SyncWorkspace(cfg config.LicensesConfig, assignments []google.LicenseAssignment, userIDByEmail func(string) (int, bool)) error {
	type skuInfo struct {
		name    string
		targets []Target
	}
	bySKU := map[string]*skuInfo{}
	for _, a := range assignments {
		uid, ok := userIDByEmail(a.UserEmail)
		if !ok {
			e.log.WithField("email", a.UserEmail).Debug("no Snipe user; skipping license seat")
			continue
		}
		si := bySKU[a.SKUID]
		if si == nil {
			si = &skuInfo{name: a.SKUName}
			bySKU[a.SKUID] = si
		}
		if si.name == "" {
			si.name = a.SKUName
		}
		si.targets = append(si.targets, Target{IsUser: true, ID: uid})
	}
	for skuID, si := range bySKU {
		name := si.name
		if name == "" {
			name = "Workspace SKU " + skuID
		}
		spec := snipe.LicenseSpec{
			Name:         name,
			CostPerSeat:  cfg.Workspace.SKUCosts[skuID], // 0 if unmapped
			CategoryID:   cfg.DefaultLicenseCategoryID,
			Reassignable: true,
			Seats:        len(si.targets),
		}
		st, err := e.Reconcile(spec, si.targets)
		if err != nil {
			return err
		}
		e.log.WithField("license", name).WithField("checked_out", st.CheckedOut).
			WithField("checked_in", st.CheckedIn).Info("workspace license reconciled")
	}
	return nil
}
