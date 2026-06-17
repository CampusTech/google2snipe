package sync

import (
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// transformValue renders a gjson result to a string per the named transform.
// Unit/date transforms return "" for missing/zero/unparseable input so the
// engine never writes a meaningless value.
func transformValue(r gjson.Result, transform string) string {
	switch transform {
	case "":
		return stringifyGJSON(r)
	case "bytes_to_gb":
		return bytesTo(r, 1e9)
	case "bytes_to_gib":
		return bytesTo(r, 1<<30)
	case "bytes_to_mb":
		return bytesTo(r, 1e6)
	case "bytes_to_tb":
		return bytesTo(r, 1e12)
	case "mac_colons":
		return normalizeMAC(r.String(), ":")
	case "mac_dashes":
		return normalizeMAC(r.String(), "-")
	case "bool_yes_no":
		return boolYesNo(r)
	case "uppercase":
		s := stringifyGJSON(r)
		if s == "" {
			return ""
		}
		return strings.ToUpper(s)
	case "lowercase":
		s := stringifyGJSON(r)
		if s == "" {
			return ""
		}
		return strings.ToLower(s)
	case "comma_thousands":
		return commaThousands(r)
	case "unix_to_iso":
		return unixToISO(r)
	case "date_only":
		return formatDate(r, "2006-01-02")
	case "datetime":
		return formatDate(r, "2006-01-02 15:04:05")
	default:
		return stringifyGJSON(r)
	}
}

func numeric(r gjson.Result) (float64, bool) {
	switch r.Type {
	case gjson.Number:
		return r.Num, true
	case gjson.String:
		f, err := strconv.ParseFloat(strings.TrimSpace(r.String()), 64)
		if err != nil {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}

func bytesTo(r gjson.Result, div float64) string {
	n, ok := numeric(r)
	if !ok || n == 0 {
		return ""
	}
	v := n / div
	rounded := math.Round(v*100) / 100
	return strconv.FormatFloat(rounded, 'f', -1, 64)
}

func normalizeMAC(s, sep string) string {
	var hex []rune
	for _, c := range strings.ToLower(s) {
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			hex = append(hex, c)
		}
	}
	if len(hex) != 12 {
		return ""
	}
	parts := make([]string, 0, 6)
	for i := 0; i < 12; i += 2 {
		parts = append(parts, string(hex[i:i+2]))
	}
	return strings.Join(parts, sep)
}

func boolYesNo(r gjson.Result) string {
	switch r.Type {
	case gjson.True:
		return "Yes"
	case gjson.False:
		return "No"
	case gjson.Number:
		if r.Num != 0 {
			return "Yes"
		}
		return "No"
	case gjson.String:
		switch strings.ToLower(strings.TrimSpace(r.String())) {
		case "true", "yes", "1":
			return "Yes"
		case "false", "no", "0":
			return "No"
		}
	}
	return ""
}

func commaThousands(r gjson.Result) string {
	n, ok := numeric(r)
	if !ok {
		return ""
	}
	neg := n < 0
	i := int64(math.Abs(n))
	s := strconv.FormatInt(i, 10)
	var b strings.Builder
	for idx, ch := range s {
		if idx > 0 && (len(s)-idx)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(ch)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func unixToISO(r gjson.Result) string {
	var sec int64
	switch r.Type {
	case gjson.Number:
		sec = int64(r.Num)
	case gjson.String:
		n, err := strconv.ParseInt(strings.TrimSpace(r.String()), 10, 64)
		if err != nil {
			return ""
		}
		sec = n
	default:
		return ""
	}
	if sec == 0 {
		return ""
	}
	return time.Unix(sec, 0).UTC().Format("2006-01-02 15:04:05")
}

var dateLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05Z0700",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

// formatDate parses a flexible date/timestamp string (RFC3339 with or without
// fractional seconds, "YYYY-MM-DD HH:MM:SS", or a bare "YYYY-MM-DD") and
// reformats it to layout in UTC. Returns "" for empty/unparseable input.
func formatDate(r gjson.Result, layout string) string {
	s := strings.TrimSpace(r.String())
	if s == "" {
		return ""
	}
	for _, l := range dateLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC().Format(layout)
		}
	}
	return ""
}

// stringifyGJSON renders a gjson result to a string; arrays become a
// comma-separated list of their non-empty elements.
func stringifyGJSON(r gjson.Result) string {
	if !r.Exists() {
		return ""
	}
	switch r.Type {
	case gjson.Null:
		return ""
	case gjson.True:
		return "true"
	case gjson.False:
		return "false"
	case gjson.Number:
		if r.Num == math.Trunc(r.Num) {
			return strconv.FormatInt(int64(r.Num), 10)
		}
		return strconv.FormatFloat(r.Num, 'f', -1, 64)
	case gjson.String:
		return r.String()
	case gjson.JSON:
		if r.IsArray() {
			var parts []string
			r.ForEach(func(_, v gjson.Result) bool {
				if s := stringifyGJSON(v); s != "" {
					parts = append(parts, s)
				}
				return true
			})
			return strings.Join(parts, ", ")
		}
		return r.String()
	}
	return ""
}
