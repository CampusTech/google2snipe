package google

import (
	"encoding/json"

	admin "google.golang.org/api/admin/directory/v1"
)

// Device wraps an admin.ChromeOsDevice and retains its JSON form so the sync
// engine can address any field (including deeply nested arrays) via gjson.
type Device struct {
	*admin.ChromeOsDevice
	Raw json.RawMessage `json:"-"`
}

// wrapDevice marshals the SDK struct to JSON and stores it as Raw.
func wrapDevice(d *admin.ChromeOsDevice) (Device, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return Device{}, err
	}
	return Device{ChromeOsDevice: d, Raw: raw}, nil
}

// SerializeDevices writes devices (with their underlying SDK struct) to JSON for caching.
func SerializeDevices(devs []Device) ([]byte, error) {
	bare := make([]*admin.ChromeOsDevice, len(devs))
	for i, d := range devs {
		bare[i] = d.ChromeOsDevice
	}
	return json.MarshalIndent(bare, "", "  ")
}

// DeserializeDevices reads cached JSON back into Devices, restoring Raw.
func DeserializeDevices(data []byte) ([]Device, error) {
	var bare []*admin.ChromeOsDevice
	if err := json.Unmarshal(data, &bare); err != nil {
		return nil, err
	}
	out := make([]Device, 0, len(bare))
	for _, d := range bare {
		dev, err := wrapDevice(d)
		if err != nil {
			return nil, err
		}
		out = append(out, dev)
	}
	return out, nil
}
