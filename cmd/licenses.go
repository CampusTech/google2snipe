package cmd

import (
	"fmt"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/CampusTech/google2snipe/config"
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
		if err != nil || len(assets) != 1 {
			return 0, false
		}
		return assets[0].ID, true
	}
	if err := engine.SyncChrome(cfg.Licenses, devs, assetIDBySerial); err != nil {
		return err
	}
	licLog.Warn("license sync complete")
	return nil
}
