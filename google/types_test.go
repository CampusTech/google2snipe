package google

import (
	"testing"

	"github.com/tidwall/gjson"
	admin "google.golang.org/api/admin/directory/v1"
)

func TestWrapDevicePopulatesRawForGjson(t *testing.T) {
	d := &admin.ChromeOsDevice{
		SerialNumber: "ABC123",
		Status:       "ACTIVE",
		OrgUnitPath:  "/Students/Grade5",
		RecentUsers: []*admin.ChromeOsDeviceRecentUsers{
			{Type: "USER_TYPE_MANAGED", Email: "kid@school.edu"},
		},
	}
	dev, err := wrapDevice(d)
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(dev.Raw, "serialNumber").String(); got != "ABC123" {
		t.Errorf("serialNumber via gjson = %q", got)
	}
	if got := gjson.GetBytes(dev.Raw, "recentUsers.0.email").String(); got != "kid@school.edu" {
		t.Errorf("recentUsers.0.email via gjson = %q", got)
	}
	if got := gjson.GetBytes(dev.Raw, "orgUnitPath").String(); got != "/Students/Grade5" {
		t.Errorf("orgUnitPath via gjson = %q", got)
	}
}

func TestSerializeRoundTripRestoresRaw(t *testing.T) {
	in := []Device{}
	d, _ := wrapDevice(&admin.ChromeOsDevice{SerialNumber: "S1", Model: "Acer Chromebook 311"})
	in = append(in, d)
	data, err := SerializeDevices(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := DeserializeDevices(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].SerialNumber != "S1" {
		t.Fatalf("round trip lost device: %+v", out)
	}
	if got := gjson.GetBytes(out[0].Raw, "model").String(); got != "Acer Chromebook 311" {
		t.Errorf("raw not restored after deserialize: %q", got)
	}
}
