package licensesync

import (
	"time"

	"github.com/CampusTech/google2snipe/config"
	"github.com/CampusTech/google2snipe/google"
	"github.com/CampusTech/google2snipe/snipe"
)

// SyncChrome reconciles ChromeOS device upgrade licenses: one Snipe License per
// configured deviceLicenseType, a seat per device asset. Perpetual types are
// non-reassignable (additive); recurring types reconcile + expire.
func (e *Engine) SyncChrome(cfg config.LicensesConfig, devices []google.Device, assetIDBySerial func(string) (int, bool)) error {
	// group device assets by deviceLicenseType
	byType := map[string][]Target{}
	skipped := 0
	for _, d := range devices {
		lt := d.DeviceLicenseType
		if lt == "" || lt == "deviceLicenseTypeUnspecified" {
			continue
		}
		if _, configured := cfg.Chrome[lt]; !configured {
			continue
		}
		assetID, ok := assetIDBySerial(d.SerialNumber)
		if !ok {
			e.log.WithField("serial", d.SerialNumber).Debug("device not yet a Snipe asset; skipping license seat")
			skipped++
			continue
		}
		byType[lt] = append(byType[lt], Target{IsUser: false, ID: assetID})
	}
	if skipped > 0 {
		e.log.WithField("skipped", skipped).Warn("chrome: devices skipped (no matching Snipe asset yet)")
	}
	for lt, targets := range byType {
		cc := cfg.Chrome[lt]
		reassignable := !config.ChromePerpetual(lt)
		if cc.Reassignable != nil {
			reassignable = *cc.Reassignable
		}
		spec := snipe.LicenseSpec{
			Name:         cc.Name,
			CostPerSeat:  cc.Cost,
			CategoryID:   cfg.DefaultLicenseCategoryID,
			Reassignable: reassignable,
			Seats:        len(targets),
		}
		if reassignable && cc.TermMonths > 0 {
			spec.ExpirationDate = time.Now().UTC().AddDate(0, cc.TermMonths, 0).Format("2006-01-02")
		}
		st, err := e.Reconcile(spec, targets)
		if err != nil {
			return err
		}
		e.log.WithField("license", cc.Name).WithField("checked_out", st.CheckedOut).
			WithField("checked_in", st.CheckedIn).Warn("chrome license reconciled")
	}
	return nil
}
