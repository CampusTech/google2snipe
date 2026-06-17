package cmd

import (
	"github.com/CampusTech/google2snipe/config"
	"github.com/CampusTech/google2snipe/snipe"
)

type fieldSpec struct {
	name      string
	element   string // text|listbox|radio|checkbox
	format    string // ANY|NUMERIC|IP|MAC|URL|DATE|BOOLEAN
	path      string
	transform string
}

// coreFields is the default ChromeOS custom-field set created by `setup`.
var coreFields = []fieldSpec{
	{"Chrome: Serial", "text", "ANY", "serialNumber", ""},
	{"Chrome: Device ID", "text", "ANY", "deviceId", ""},
	{"Chrome: Model", "text", "ANY", "model", ""},
	{"Chrome: OS Type", "text", "ANY", "chromeOsType", ""},
	{"Chrome: OS Version", "text", "ANY", "osVersion", ""},
	{"Chrome: Platform Version", "text", "ANY", "platformVersion", ""},
	{"Chrome: Firmware Version", "text", "ANY", "firmwareVersion", ""},
	{"Chrome: OS Compliance", "text", "ANY", "osVersionCompliance", ""},
	{"Chrome: OS Update State", "text", "ANY", "osUpdateStatus.state", ""},
	{"Chrome: Status", "text", "ANY", "status", ""},
	{"Chrome: Org Unit Path", "text", "ANY", "orgUnitPath", ""},
	{"Chrome: Annotated User", "text", "ANY", "annotatedUser", ""},
	{"Chrome: Annotated Asset ID", "text", "ANY", "annotatedAssetId", ""},
	{"Chrome: Annotated Location", "text", "ANY", "annotatedLocation", ""},
	{"Chrome: Boot Mode", "text", "ANY", "bootMode", ""},
	{"Chrome: MAC", "text", "MAC", "macAddress", "mac_colons"},
	{"Chrome: Ethernet MAC", "text", "MAC", "ethernetMacAddress", "mac_colons"},
	{"Chrome: Last Known IP", "text", "IP", "lastKnownNetwork.0.ipAddress", ""},
	{"Chrome: CPU Model", "text", "ANY", "cpuInfo.0.model", ""},
	{"Chrome: System RAM (GB)", "text", "NUMERIC", "systemRamTotal", "bytes_to_gb"},
	{"Chrome: Disk Capacity (GB)", "text", "NUMERIC", "diskSpaceUsage.capacityBytes", "bytes_to_gb"},
	{"Chrome: Disk Used (GB)", "text", "NUMERIC", "diskSpaceUsage.usedBytes", "bytes_to_gb"},
	{"Chrome: License Type", "text", "ANY", "deviceLicenseType", ""},
	{"Chrome: Manufacture Date", "text", "DATE", "manufactureDate", "date_only"},
	{"Chrome: Order Number", "text", "ANY", "orderNumber", ""},
	{"Chrome: Auto-Update Through", "text", "DATE", "autoUpdateThrough", "date_only"},
	{"Chrome: Support End Date", "text", "DATE", "supportEndDate", "date_only"},
	{"Chrome: First Enrollment", "text", "DATE", "firstEnrollmentTime", "date_only"},
	{"Chrome: Last Enrollment", "text", "DATE", "lastEnrollmentTime", "date_only"},
	{"Chrome: Last Sync", "text", "ANY", "lastSync", "datetime"},
	{"Chrome: TPM Spec Level", "text", "ANY", "tpmVersionInfo.specLevel", ""},
	{"Chrome: Notes", "text", "ANY", "notes", ""},
	{"Chrome: Recent Users", "text", "ANY", "recentUsers.#.email", ""},
}

// chromeFieldDefs returns the FieldDefs to create and the field-name -> mapping
// (the engine fills db_column_name after creation).
func chromeFieldDefs() ([]snipe.FieldDef, map[string]config.FieldMappingEntry) {
	defs := make([]snipe.FieldDef, 0, len(coreFields))
	pathByName := make(map[string]config.FieldMappingEntry, len(coreFields))
	for _, f := range coreFields {
		defs = append(defs, snipe.FieldDef{Name: f.name, Element: f.element, Format: f.format})
		pathByName[f.name] = config.FieldMappingEntry{Path: f.path, Transform: f.transform}
	}
	return defs, pathByName
}
