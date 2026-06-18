package cmd

import (
	"testing"

	admin "google.golang.org/api/admin/directory/v1"

	"github.com/CampusTech/google2snipe/google"
)

func dev(serial, ou string) google.Device {
	return google.Device{ChromeOsDevice: &admin.ChromeOsDevice{SerialNumber: serial, OrgUnitPath: ou}}
}

func TestInScopeDevices(t *testing.T) {
	devs := []google.Device{
		dev("A", "/Students"),
		dev("B", "/Students/HS"),
		dev("C", "/Faculty"),
		dev("D", ""),
	}
	got := inScopeDevices(devs, []string{"/Students"})
	if len(got) != 2 || got[0].SerialNumber != "A" || got[1].SerialNumber != "B" {
		t.Fatalf("kept %d devices: %+v, want A and B", len(got), serials(got))
	}
	// Empty scopes keep everything.
	if len(inScopeDevices(devs, nil)) != 4 {
		t.Fatalf("empty scopes should keep all 4")
	}
}

func serials(devs []google.Device) []string {
	out := make([]string, len(devs))
	for i, d := range devs {
		out[i] = d.SerialNumber
	}
	return out
}

func TestInScopeAssignments(t *testing.T) {
	asg := []google.LicenseAssignment{
		{UserEmail: "alice@example.com"}, // /Students -> kept
		{UserEmail: "BOB@example.com"},   // /Faculty -> dropped (case-insensitive lookup)
		{UserEmail: "ghost@example.com"}, // absent from map -> dropped
	}
	ouByEmail := map[string]string{
		"alice@example.com": "/Students/Online",
		"bob@example.com":   "/Faculty",
	}
	got := inScopeAssignments(asg, ouByEmail, []string{"/Students"})
	if len(got) != 1 || got[0].UserEmail != "alice@example.com" {
		t.Fatalf("kept %d assignments: %+v, want only alice", len(got), got)
	}
	// Empty scopes keep everything.
	if len(inScopeAssignments(asg, ouByEmail, nil)) != 3 {
		t.Fatalf("empty scopes should keep all 3")
	}
}
