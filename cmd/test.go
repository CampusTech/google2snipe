package cmd

import (
	"github.com/spf13/cobra"

	"github.com/CampusTech/google2snipe/config"
	"github.com/CampusTech/google2snipe/google"
	"github.com/CampusTech/google2snipe/snipe"
)

var testCmd = &cobra.Command{
	Use:   "test",
	Short: "Verify connectivity to the Google Admin SDK and Snipe-IT",
	RunE:  runTest,
}

func init() { rootCmd.AddCommand(testCmd) }

func runTest(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return err
	}
	gc, err := google.New(cfg.Google, googleLog)
	if err != nil {
		return err
	}
	customer, err := gc.About(cmd.Context())
	if err != nil {
		return err
	}
	googleLog.WithField("customer_id", customer).Warn("google admin sdk: OK")

	sc, err := snipe.New(cfg.SnipeIT.URL, cfg.SnipeIT.APIKey, true, cfg.Sync.RateLimit, snipeLog)
	if err != nil {
		return err
	}
	ver, err := sc.Ping()
	if err != nil {
		return err
	}
	snipeLog.WithField("version", ver).Warn("snipe-it: OK")
	return nil
}
