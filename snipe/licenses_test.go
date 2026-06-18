package snipe

import (
	"errors"
	"testing"

	"github.com/sirupsen/logrus"
)

func TestLicenseClientDryRunSentinel(t *testing.T) {
	c := NewLicenseClient("https://snipe.invalid", "key", true /*dryRun*/, logrus.New())
	// EnsureLicense is a mutator; in dry-run it must not dial and must return ErrDryRun.
	_, err := c.EnsureLicense(LicenseSpec{Name: "X", CategoryID: 1, Seats: 1})
	if !errors.Is(err, ErrDryRun) {
		t.Fatalf("EnsureLicense dry-run = %v, want ErrDryRun", err)
	}
}
