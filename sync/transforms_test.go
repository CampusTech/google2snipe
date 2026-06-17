package sync

import (
	"testing"

	"github.com/tidwall/gjson"
)

func res(json, path string) gjson.Result { return gjson.Get(json, path) }

func TestTransforms(t *testing.T) {
	cases := []struct {
		name, json, path, transform, want string
	}{
		{"plain string", `{"a":"hi"}`, "a", "", "hi"},
		{"int as string field", `{"ram":"8589934592"}`, "ram", "bytes_to_gb", "8.59"},
		{"int number", `{"ram":8589934592}`, "ram", "bytes_to_gb", "8.59"},
		{"zero bytes empty", `{"ram":0}`, "ram", "bytes_to_gb", ""},
		{"missing empty", `{}`, "nope", "bytes_to_gb", ""},
		{"mac colons", `{"m":"a4bb6d123456"}`, "m", "mac_colons", "a4:bb:6d:12:34:56"},
		{"mac already sep", `{"m":"A4-BB-6D-12-34-56"}`, "m", "mac_colons", "a4:bb:6d:12:34:56"},
		{"mac bad length", `{"m":"xyz"}`, "m", "mac_colons", ""},
		{"bool yes", `{"b":true}`, "b", "bool_yes_no", "Yes"},
		{"bool no", `{"b":false}`, "b", "bool_yes_no", "No"},
		{"upper", `{"s":"flex"}`, "s", "uppercase", "FLEX"},
		{"array joined", `{"u":[{"email":"a@x"},{"email":"b@y"}]}`, "u.#.email", "", "a@x, b@y"},
		{"number int form", `{"n":5}`, "n", "", "5"},
		{"date_only from rfc3339 millis", `{"t":"2024-05-01T12:00:00.000Z"}`, "t", "date_only", "2024-05-01"},
		{"date_only from bare date", `{"t":"2020-02-19"}`, "t", "date_only", "2020-02-19"},
		{"datetime from rfc3339 millis", `{"t":"2024-05-01T12:00:00.000Z"}`, "t", "datetime", "2024-05-01 12:00:00"},
		{"date unparseable empty", `{"t":"not a date"}`, "t", "date_only", ""},
		{"date missing empty", `{}`, "nope", "date_only", ""},
		{"bad string bytes empty", `{"ram":"not-a-number"}`, "ram", "bytes_to_gb", ""},
		{"mac dashes", `{"m":"a4bb6d123456"}`, "m", "mac_dashes", "a4-bb-6d-12-34-56"},
		{"comma thousands", `{"n":1234567}`, "n", "comma_thousands", "1,234,567"},
		{"comma thousands negative", `{"n":-1234}`, "n", "comma_thousands", "-1,234"},
		{"unix to iso", `{"t":1609459200}`, "t", "unix_to_iso", "2021-01-01 00:00:00"},
		{"unix to iso string", `{"t":"1609459200"}`, "t", "unix_to_iso", "2021-01-01 00:00:00"},
		{"unix to iso zero empty", `{"t":0}`, "t", "unix_to_iso", ""},
		{"lowercase", `{"s":"FLEX"}`, "s", "lowercase", "flex"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := transformValue(res(c.json, c.path), c.transform)
			if got != c.want {
				t.Errorf("transformValue(%s,%q) = %q, want %q", c.path, c.transform, got, c.want)
			}
		})
	}
}
