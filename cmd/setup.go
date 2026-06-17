package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/CampusTech/google2snipe/config"
	"github.com/CampusTech/google2snipe/snipe"
)

var setupDryRun bool

var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Create ChromeOS custom fields in Snipe-IT and merge mappings into config",
	RunE:  runSetup,
}

func init() {
	setupCmd.Flags().BoolVar(&setupDryRun, "dry-run", false, "simulate without creating fields")
	rootCmd.AddCommand(setupCmd)
}

func runSetup(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return err
	}
	sc, err := snipe.New(cfg.SnipeIT.URL, cfg.SnipeIT.APIKey, setupDryRun, cfg.Sync.RateLimit, snipeLog)
	if err != nil {
		return err
	}
	defs, pathByName := chromeFieldDefs()

	fieldsetIDs := []int{}
	if cfg.SnipeIT.CustomFieldsetID != 0 {
		fieldsetIDs = append(fieldsetIDs, cfg.SnipeIT.CustomFieldsetID)
	}
	if len(fieldsetIDs) == 0 {
		return fmt.Errorf("snipe_it.custom_fieldset_id is required for setup")
	}

	dbColByName, err := sc.SetupFields(fieldsetIDs, defs)
	if err != nil {
		return err
	}
	if setupDryRun {
		snipeLog.WithField("fields", len(defs)).Warn("[DRY RUN] would create/update fields and merge config")
		return nil
	}

	merge := map[string]config.FieldMappingEntry{}
	for name, entry := range pathByName {
		dbCol := dbColByName[name]
		if dbCol == "" {
			snipeLog.WithField("field", name).Warn("no db_column_name returned; skipping mapping")
			continue
		}
		merge[dbCol] = entry
	}
	if err := config.MergeFieldMapping(cfgFile, merge); err != nil {
		return fmt.Errorf("merge field mapping: %w", err)
	}
	snipeLog.WithField("fields", len(merge)).Warn("setup complete; field_mapping merged into config")
	return nil
}
