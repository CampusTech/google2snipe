package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/CampusTech/google2snipe/config"
	"github.com/CampusTech/google2snipe/google"
	"github.com/CampusTech/google2snipe/snipe"
	syncpkg "github.com/CampusTech/google2snipe/sync"
)

var (
	syncDryRun     bool
	syncForce      bool
	syncSerial     string
	syncDeviceID   string
	syncUpdateOnly bool
	syncUseCache   bool
	syncProjection string

	googleLog = logrus.New()
	snipeLog  = logrus.New()
	syncLog   = logrus.New()
)

var syncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Reconcile ChromeOS devices into Snipe-IT",
	RunE:  runSync,
}

func init() {
	RegisterLogger(googleLog)
	RegisterLogger(snipeLog)
	RegisterLogger(syncLog)
	syncCmd.Flags().BoolVar(&syncDryRun, "dry-run", false, "simulate without mutating Snipe-IT")
	syncCmd.Flags().BoolVar(&syncForce, "force", false, "ignore freshness checks")
	syncCmd.Flags().StringVar(&syncSerial, "serial", "", "sync only the device with this serial")
	syncCmd.Flags().StringVar(&syncDeviceID, "device-id", "", "sync only the device with this Google deviceId")
	syncCmd.Flags().BoolVar(&syncUpdateOnly, "update-only", false, "never create, only update")
	syncCmd.Flags().BoolVar(&syncUseCache, "use-cache", false, "read devices from local cache instead of the API")
	syncCmd.Flags().StringVar(&syncProjection, "projection", "", "override projection: full|basic")
	rootCmd.AddCommand(syncCmd)
}

func runSync(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return err
	}
	// flag overrides
	cfg.Sync.DryRun = cfg.Sync.DryRun || syncDryRun
	cfg.Sync.Force = cfg.Sync.Force || syncForce
	cfg.Sync.UpdateOnly = cfg.Sync.UpdateOnly || syncUpdateOnly
	cfg.Sync.UseCache = cfg.Sync.UseCache || syncUseCache
	if syncSerial != "" || syncDeviceID != "" {
		cfg.Sync.Force = true
	}
	if syncProjection != "" {
		cfg.Google.Projection = syncProjection
	}

	sc, err := snipe.New(cfg.SnipeIT.URL, cfg.SnipeIT.APIKey, cfg.Sync.DryRun, cfg.Sync.RateLimit, snipeLog)
	if err != nil {
		return err
	}
	engine := syncpkg.New(cfg, sc, syncLog)
	if err := engine.Warm(); err != nil {
		return fmt.Errorf("warm caches: %w", err)
	}

	devs, err := loadDevices(cmd.Context(), cfg)
	if err != nil {
		return err
	}
	if syncSerial != "" {
		devs = filterSerial(devs, syncSerial)
	}
	engine.SyncAll(devs)
	stats := engine.StatsSnapshot()
	syncLog.WithFields(logrus.Fields{
		"total": stats.Total, "created": stats.Created, "updated": stats.Updated,
		"skipped": stats.Skipped, "errors": stats.Errors,
	}).Warn("done")
	if stats.Errors > 0 {
		return fmt.Errorf("%d device(s) failed to sync", stats.Errors)
	}
	return nil
}

func loadDevices(ctx context.Context, cfg *config.Config) ([]google.Device, error) {
	cachePath := filepath.Join(cfg.Sync.CacheDir, "devices.json")
	if cfg.Sync.UseCache {
		data, err := os.ReadFile(cachePath)
		if err != nil {
			return nil, fmt.Errorf("read cache: %w", err)
		}
		return google.DeserializeDevices(data)
	}

	gc, err := google.New(cfg.Google, googleLog)
	if err != nil {
		return nil, err
	}
	if syncDeviceID != "" {
		d, err := gc.GetDevice(ctx, syncDeviceID)
		if err != nil {
			return nil, err
		}
		return []google.Device{d}, nil
	}
	devs, err := gc.ListAllChromeOSDevices(ctx)
	if err != nil {
		return nil, err
	}
	if data, err := google.SerializeDevices(devs); err == nil {
		_ = os.MkdirAll(cfg.Sync.CacheDir, 0o755)
		_ = os.WriteFile(cachePath, data, 0o644)
	}
	return devs, nil
}

func filterSerial(devs []google.Device, serial string) []google.Device {
	var out []google.Device
	for _, d := range devs {
		if d.SerialNumber == serial {
			out = append(out, d)
		}
	}
	return out
}
