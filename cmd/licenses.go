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
	engine := licensesync.New(lc, licLog, licensesync.WithConcurrency(cfg.Sync.Concurrency))

	// Chrome license sync needs the device list (cache or fetch) and a
	// serial→asset index. Workspace-only configs skip all of it so they don't
	// hard-depend on listing every hardware asset in Snipe-IT.
	if len(cfg.Licenses.Chrome) > 0 {
		devs, err := loadDevices(cmd.Context(), cfg)
		if err != nil {
			return err
		}
		allAssets, err := sc.ListAllAssets(cmd.Context())
		if err != nil {
			return err
		}
		assetIdx := make(map[string]int, len(allAssets))
		for _, a := range allAssets {
			if a.Serial == "" {
				continue
			}
			key := strings.ToLower(a.Serial)
			if _, dup := assetIdx[key]; dup {
				licLog.WithField("serial", a.Serial).Warn("duplicate serial in Snipe-IT; keeping first (ambiguous asset)")
				continue
			}
			assetIdx[key] = a.ID
		}
		assetIDBySerial := func(serial string) (int, bool) {
			id, ok := assetIdx[strings.ToLower(serial)]
			return id, ok
		}
		if err := engine.SyncChrome(cmd.Context(), cfg.Licenses, devs, assetIDBySerial); err != nil {
			return err
		}
	}

	if len(cfg.Licenses.Workspace.Products) > 0 {
		asg, err := loadAssignments(cmd.Context(), cfg, licLog)
		if err != nil {
			return err
		}
		users, err := newCachingSnipe(sc, cfg.Sync.UseCache, cfg.Sync.CacheDir, snipeLog).ListAllUsers(cmd.Context())
		if err != nil {
			return err
		}
		// Index Snipe users by full lowercased email, plus an unambiguous local-part
		// (before-@) index so a Workspace user can still match a Snipe user under a
		// different domain (e.g. alice@students.example.com -> alice@example.com). Local parts
		// shared by more than one distinct user are marked ambiguous and never matched.
		idx := map[string]int{}
		localPart := map[string]int{}
		ambiguous := map[string]bool{}
		for _, u := range users {
			if u.Email == "" {
				continue
			}
			e := strings.ToLower(u.Email)
			idx[e] = u.ID
			if i := strings.IndexByte(e, '@'); i > 0 {
				lp := e[:i]
				if prev, seen := localPart[lp]; seen && prev != u.ID {
					ambiguous[lp] = true
				} else {
					localPart[lp] = u.ID
				}
			}
		}
		userIDByEmail := func(email string) (int, bool) {
			e := strings.ToLower(email)
			if id, ok := idx[e]; ok {
				return id, true
			}
			if i := strings.IndexByte(e, '@'); i > 0 {
				lp := e[:i]
				if !ambiguous[lp] {
					if id, ok := localPart[lp]; ok {
						return id, true
					}
				}
			}
			return 0, false
		}
		if err := engine.SyncWorkspace(cmd.Context(), cfg.Licenses, asg, userIDByEmail); err != nil {
			return err
		}
	}

	licLog.Warn("license sync complete")
	return nil
}

func loadAssignments(ctx context.Context, cfg *config.Config, logger *logrus.Logger) ([]google.LicenseAssignment, error) {
	path := filepath.Join(cfg.Sync.CacheDir, "license_assignments.json")
	if cfg.Sync.UseCache {
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			return google.DeserializeAssignments(data)
		case os.IsNotExist(err):
			// expected on first run; fall through to a live fetch
		default:
			logger.WithError(err).WithField("path", path).Warn("license assignments cache unreadable; fetching live")
		}
	}
	gl, err := google.NewLicensingClient(cfg.Google, cfg.Licenses.Workspace.CustomerID, logger)
	if err != nil {
		return nil, err
	}
	asg, err := gl.ListAssignments(ctx, cfg.Licenses.Workspace.Products)
	if err != nil {
		return nil, err
	}
	if data, err := google.SerializeAssignments(asg); err == nil {
		_ = os.MkdirAll(cfg.Sync.CacheDir, 0o755)
		_ = os.WriteFile(path, data, 0o600)
	}
	return asg, nil
}
