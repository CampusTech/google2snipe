package cmd

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/CampusTech/google2snipe/config"
	"github.com/CampusTech/google2snipe/google"
)

// candidateProducts is the known Licensing-API product catalog probed at setup.
var candidateProducts = []string{
	"Google-Apps", "101031", "101034", "101037", "101047", "101001", "101005",
	"Google-Vault", "101033", "101038", "101054", "101039", "101040", "101035", "101052",
}

var licensesSetupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Discover license types in use and quiz for per-seat costs, then write config",
	RunE:  runLicensesSetup,
}

func init() { licensesCmd.AddCommand(licensesSetupCmd) }

func runLicensesSetup(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadForSetup(cfgFile)
	if err != nil {
		return err
	}
	out := config.LicensesConfig{
		Enabled:                  true,
		DefaultLicenseCategoryID: cfg.Licenses.DefaultLicenseCategoryID,
		Chrome:                   map[string]config.ChromeLicenseConfig{},
		Workspace:                config.WorkspaceLicenseConfig{SKUCosts: map[string]float64{}},
	}
	in := bufio.NewReader(os.Stdin)
	askCost := func(label string) float64 {
		fmt.Printf("  %s\n    cost per seat (USD, blank=0): ", label)
		line, _ := in.ReadString('\n')
		v, _ := strconv.ParseFloat(strings.TrimSpace(line), 64)
		return v
	}

	// 1) license category id
	if out.DefaultLicenseCategoryID == 0 {
		fmt.Print("Snipe-IT license category id (default_license_category_id): ")
		line, _ := in.ReadString('\n')
		out.DefaultLicenseCategoryID, _ = strconv.Atoi(strings.TrimSpace(line))
	}

	// 2) Chrome upgrade types from devices
	devs, err := loadDevices(cmd.Context(), cfg)
	if err != nil {
		return err
	}
	chromeCounts := map[string]int{}
	for _, d := range devs {
		if d.DeviceLicenseType != "" && d.DeviceLicenseType != "deviceLicenseTypeUnspecified" {
			chromeCounts[d.DeviceLicenseType]++
		}
	}
	for _, t := range sortedKeys(chromeCounts) {
		kind := "recurring"
		if config.ChromePerpetual(t) {
			kind = "perpetual"
		}
		name := fmt.Sprintf("Chrome Upgrade (%s)", t)
		cost := askCost(fmt.Sprintf("%s  [%s · %d devices · %s]", name, kind, chromeCounts[t], t))
		out.Chrome[t] = config.ChromeLicenseConfig{Name: name, Cost: cost}
	}

	// 3) Workspace SKUs from the Licensing API
	gl, err := google.NewLicensingClient(cfg.Google, cfg.Licenses.Workspace.CustomerID, licLog)
	if err != nil {
		return err
	}
	asg, err := gl.ListAssignments(cmd.Context(), candidateProducts)
	if err != nil {
		return err
	}
	type sk struct {
		name    string
		product string
		count   int
	}
	skus := map[string]*sk{}
	prodSet := map[string]bool{}
	for _, a := range asg {
		prodSet[a.ProductID] = true
		s := skus[a.SKUID]
		if s == nil {
			s = &sk{name: a.SKUName, product: a.ProductID}
			skus[a.SKUID] = s
		}
		s.count++
	}
	for _, id := range sortedKeys2(skus) {
		s := skus[id]
		cost := askCost(fmt.Sprintf("%s  [license · %d users · SKU %s]", s.name, s.count, id))
		out.Workspace.SKUCosts[id] = cost
	}
	for p := range prodSet {
		out.Workspace.Products = append(out.Workspace.Products, p)
	}
	sort.Strings(out.Workspace.Products)

	// 4) write config
	if err := config.MergeLicenses(cfgFile, out); err != nil {
		return err
	}
	fmt.Printf("\nWrote licenses config: %d Chrome type(s), %d Workspace SKU(s) into %s\n",
		len(out.Chrome), len(out.Workspace.SKUCosts), cfgFile)
	return nil
}

func sortedKeys(m map[string]int) []string {
	var k []string
	for s := range m {
		k = append(k, s)
	}
	sort.Strings(k)
	return k
}

func sortedKeys2[V any](m map[string]V) []string {
	var k []string
	for s := range m {
		k = append(k, s)
	}
	sort.Strings(k)
	return k
}
