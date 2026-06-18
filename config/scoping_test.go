package config

import (
	"reflect"
	"testing"
)

func TestInScope(t *testing.T) {
	cases := []struct {
		name   string
		ou     string
		scopes []string
		want   bool
	}{
		{"empty scopes means no filter", "/Anything", nil, true},
		{"exact match", "/Students", []string{"/Students"}, true},
		{"one level under", "/Students/HS", []string{"/Students"}, true},
		{"deep multi-level with space", "/Students/Online/Fall 2024", []string{"/Students"}, true},
		{"sibling prefix is not a descendant", "/StudentsClub", []string{"/Students"}, false},
		{"unrelated ou", "/Faculty", []string{"/Students"}, false},
		{"second scope matches", "/Faculty/Adjuncts", []string{"/Students", "/Faculty"}, true},
		{"root scope matches all", "/Students/HS", []string{"/"}, true},
		{"empty ou not in a real scope", "", []string{"/Students"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := InScope(c.ou, c.scopes); got != c.want {
				t.Fatalf("InScope(%q, %v) = %v, want %v", c.ou, c.scopes, got, c.want)
			}
		})
	}
}

func TestEffectiveLicenseScopes(t *testing.T) {
	override := &Config{Licenses: LicensesConfig{OrgUnitPaths: []string{"/Students", "/Faculty"}}, Google: GoogleConfig{OrgUnitPath: "/Everyone"}}
	if got := EffectiveLicenseScopes(override); !reflect.DeepEqual(got, []string{"/Students", "/Faculty"}) {
		t.Fatalf("override: got %v", got)
	}
	fallback := &Config{Google: GoogleConfig{OrgUnitPath: "/Students"}}
	if got := EffectiveLicenseScopes(fallback); !reflect.DeepEqual(got, []string{"/Students"}) {
		t.Fatalf("fallback: got %v", got)
	}
	none := &Config{}
	if got := EffectiveLicenseScopes(none); got != nil {
		t.Fatalf("none: got %v, want nil", got)
	}
}
