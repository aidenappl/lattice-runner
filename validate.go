package main

// validContainerName checks that a container name is safe to use.
// Allows alphanumeric, hyphens, underscores, dots, and forward slashes (for compose prefixes).
func validContainerName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '/') {
			return false
		}
	}
	return true
}
