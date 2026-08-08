package incusclient

import "regexp"

// incusNamePattern matches Incus instance/profile name rules: 1-63 chars,
// alphanumeric with optional internal hyphens, no leading/trailing hyphen.
var incusNamePattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

// ValidIncusName reports whether name is a valid Incus instance/profile name.
// It is the single source of truth for the name rules used by supervisor child
// names and sandbox names (previously duplicated in two packages).
func ValidIncusName(name string) bool {
	return incusNamePattern.MatchString(name)
}
