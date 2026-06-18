package config

import "strings"

// EffectiveLicenseScopes resolves the Org Unit paths that scope the license sync:
// the licenses-specific list if set, otherwise the global org_unit_path (as a
// single-element list), otherwise nil (no OU filtering).
func EffectiveLicenseScopes(cfg *Config) []string {
	if len(cfg.Licenses.OrgUnitPaths) > 0 {
		return cfg.Licenses.OrgUnitPaths
	}
	if cfg.Google.OrgUnitPath != "" {
		return []string{cfg.Google.OrgUnitPath}
	}
	return nil
}

// InScope reports whether the org unit path ou falls under any of scopes. An empty
// scopes list means no filtering (everything is in scope). A scope matches its own
// OU exactly and any descendant on a path-segment boundary, so "/Students" matches
// "/Students" and "/Students/Online/Fall 2024" but not "/StudentsClub". The root
// scope "/" matches every OU; to scope to everything else, leave the list empty.
//
// Each scope is trimmed of surrounding whitespace and a trailing slash; a blank
// (or whitespace-/slash-only) entry is skipped rather than treated as a wildcard,
// so a stray empty item in config can't silently disable filtering.
func InScope(ou string, scopes []string) bool {
	if len(scopes) == 0 {
		return true
	}
	for _, s := range scopes {
		s = strings.TrimSpace(s)
		if s != "/" {
			s = strings.TrimRight(s, "/")
		}
		if s == "" {
			continue
		}
		if s == "/" || ou == s || strings.HasPrefix(ou, s+"/") {
			return true
		}
	}
	return false
}
