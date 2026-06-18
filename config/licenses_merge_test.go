package config

import (
	"os"
	"testing"
)

func TestMergeLicensesWritesBlock(t *testing.T) {
	p := writeTemp(t, "sync:\n  set_name: false\n")
	in := LicensesConfig{
		Enabled:                  true,
		DefaultLicenseCategoryID: 7,
		Chrome: map[string]ChromeLicenseConfig{
			"educationUpgradePerpetual": {Name: "Chrome EDU Perpetual", Cost: 38},
		},
		Workspace: WorkspaceLicenseConfig{
			Products: []string{"Google-Apps"},
			SKUCosts: map[string]float64{"1010310008": 5},
		},
	}
	if err := MergeLicenses(p, in); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	var c Config
	if err := yamlUnmarshal(data, &c); err != nil {
		t.Fatalf("reload: %v\n%s", err, data)
	}
	if !c.Licenses.Enabled || c.Licenses.DefaultLicenseCategoryID != 7 {
		t.Errorf("licenses block missing:\n%s", data)
	}
	if c.Licenses.Chrome["educationUpgradePerpetual"].Cost != 38 {
		t.Errorf("chrome cost missing:\n%s", data)
	}
	if c.Licenses.Workspace.SKUCosts["1010310008"] != 5 {
		t.Errorf("workspace sku cost missing:\n%s", data)
	}
}
