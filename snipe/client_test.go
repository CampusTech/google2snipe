package snipe

import (
	"errors"
	"testing"

	"github.com/sirupsen/logrus"
)

func TestDryRunBlocksCreate(t *testing.T) {
	c, err := New("https://snipe.invalid", "key", true /*dryRun*/, false, logrus.New())
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.CreateAsset(Asset{Serial: "X1", ModelID: 1, StatusID: 1})
	if !errors.Is(err, ErrDryRun) {
		t.Fatalf("CreateAsset in dry-run = %v, want ErrDryRun", err)
	}
}
