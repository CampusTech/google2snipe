package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/CampusTech/google2snipe/config"
	"github.com/CampusTech/google2snipe/google"
	"github.com/CampusTech/google2snipe/licensesync"
	"github.com/CampusTech/google2snipe/snipe"
)

var (
	licDryRun   bool
	licUseCache bool
	licLog      = logrus.New()
)

var licensesCmd = &cobra.Command{
	Use:   "licenses",
	Short: "Sync Google licenses into Snipe-IT as cost-bearing Licenses",
}

var licensesSyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Reconcile Google licenses into Snipe-IT license seats",
	RunE:  runLicensesSync,
}

func init() {
	RegisterLogger(licLog)
	licensesSyncCmd.Flags().BoolVar(&licDryRun, "dry-run", false, "simulate without mutating Snipe-IT")
	licensesSyncCmd.Flags().BoolVar(&licUseCache, "use-cache", false, "read devices/users from local cache")
	licensesCmd.AddCommand(licensesSyncCmd)
	rootCmd.AddCommand(licensesCmd)
}

func runLicensesSync(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return err
	}
	if !cfg.Licenses.Enabled {
		return fmt.Errorf("licenses.enabled is false; run 'google2snipe licenses setup' first")
	}
	cfg.Sync.DryRun = cfg.Sync.DryRun || licDryRun
	cfg.Sync.UseCache = cfg.Sync.UseCache || licUseCache

	// asset lookups via the existing go-snipeit-backed client
	sc, err := snipe.New(cfg.SnipeIT.URL, cfg.SnipeIT.APIKey, cfg.Sync.DryRun, cfg.Sync.RateLimit, snipeLog)
	if err != nil {
		return err
	}
	lc := snipe.NewLicenseClient(cfg.SnipeIT.URL, cfg.SnipeIT.APIKey, cfg.Sync.DryRun, licLog)
	engine := licensesync.New(lc, licLog)

	// devices (cache or fetch) — reuse the sync command's loader
	devs, err := loadDevices(cmd.Context(), cfg)
	if err != nil {
		return err
	}
	assetIDBySerial := func(serial string) (int, bool) {
		assets, err := sc.GetAssetBySerial(serial)
		if err != nil {
			licLog.WithError(err).WithField("serial", serial).Debug("asset lookup failed; skipping license seat")
			return 0, false
		}
		if len(assets) != 1 {
			if len(assets) > 1 {
				licLog.WithField("serial", serial).WithField("matches", len(assets)).
					Warn("duplicate serial in Snipe-IT; skipping license seat (ambiguous asset)")
			}
			return 0, false
		}
		return assets[0].ID, true
	}
	if err := engine.SyncChrome(cfg.Licenses, devs, assetIDBySerial); err != nil {
		return err
	}

	if len(cfg.Licenses.Workspace.Products) > 0 {
		gl, err := google.NewLicensingClient(cfg.Google, cfg.Licenses.Workspace.CustomerID, licLog)
		if err != nil {
			return err
		}
		asg, err := loadAssignments(cmd.Context(), cfg, gl)
		if err != nil {
			return err
		}
		// reuse the sync engine's Warm user index via a fresh engine, or load users directly:
		users, err := newCachingSnipe(sc, cfg.Sync.UseCache, cfg.Sync.CacheDir, snipeLog).ListAllUsers()
		if err != nil {
			return err
		}
		idx := map[string]int{}
		for _, u := range users {
			if u.Email != "" {
				idx[strings.ToLower(u.Email)] = u.ID
			}
		}
		userIDByEmail := func(email string) (int, bool) {
			id, ok := idx[strings.ToLower(email)]
			if !ok {
				if i := strings.IndexByte(strings.ToLower(email), '@'); i > 0 {
					id, ok = idx[strings.ToLower(email)[:i]]
				}
			}
			return id, ok
		}
		if err := engine.SyncWorkspace(cfg.Licenses, asg, userIDByEmail); err != nil {
			return err
		}
	}

	licLog.Warn("license sync complete")
	return nil
}

func loadAssignments(ctx context.Context, cfg *config.Config, gl *google.LicensingClient) ([]google.LicenseAssignment, error) {
	path := filepath.Join(cfg.Sync.CacheDir, "license_assignments.json")
	if cfg.Sync.UseCache {
		if data, err := os.ReadFile(path); err == nil {
			return google.DeserializeAssignments(data)
		}
	}
	asg, err := gl.ListAssignments(ctx, cfg.Licenses.Workspace.Products)
	if err != nil {
		return nil, err
	}
	if data, err := google.SerializeAssignments(asg); err == nil {
		_ = os.MkdirAll(cfg.Sync.CacheDir, 0o755)
		_ = os.WriteFile(path, data, 0o644)
	}
	return asg, nil
}
