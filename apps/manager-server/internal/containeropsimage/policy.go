// Package containeropsimage centralizes the image defaults and validation used
// by the manager and cpamp-agent deployment paths.
package containeropsimage

import (
	"regexp"
	"strings"
)

const (
	// DefaultCPAImage is the pinned CPA build that contains the storefront
	// license gate. Keep the tag immutable for a release series.
	DefaultCPAImage = "ghcr.io/abc124774961/cli-proxy-api-cpa:v7.2.148-cpa.3"
	// DefaultCPAMPImage is the manager/agent image used by a clean stack.
	DefaultCPAMPImage = "seakee/cpa-manager-plus:latest"

	CPARepository       = "ghcr.io/abc124774961/cli-proxy-api-cpa"
	LegacyCPARepository = "seakee/cli-proxy-api"
	CPAMPRepository     = "seakee/cpa-manager-plus"
)

var (
	tagPattern    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-fA-F]{64}$`)
)

// DefaultForRole returns the pinned image used when a caller does not supply
// an image explicitly.
func DefaultForRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "cpa":
		return DefaultCPAImage
	case "cpamp", "agent":
		return DefaultCPAMPImage
	default:
		return ""
	}
}

// Repository strips a tag or digest and returns the image repository.
func Repository(image string) string {
	reference := strings.TrimSpace(image)
	if at := strings.IndexByte(reference, '@'); at > 0 {
		reference = reference[:at]
	}
	lastSlash := strings.LastIndexByte(reference, '/')
	if colon := strings.LastIndexByte(reference, ':'); colon > lastSlash {
		reference = reference[:colon]
	}
	return reference
}

// ValidReference performs a conservative Docker image reference check. It is
// intentionally independent of a registry network request so it can be used
// before any image is pulled.
func ValidReference(raw string) bool {
	image := strings.TrimSpace(raw)
	if image == "" || image != raw || len(image) > 512 || strings.ContainsAny(image, "\t\r\n ") {
		return false
	}
	if strings.Contains(image, "://") || strings.Count(image, "@") > 1 {
		return false
	}

	name := image
	if at := strings.IndexByte(image, '@'); at >= 0 {
		name = image[:at]
		if !digestPattern.MatchString(image[at+1:]) {
			return false
		}
	}

	lastSlash := strings.LastIndexByte(name, '/')
	if colon := strings.LastIndexByte(name, ':'); colon > lastSlash {
		if !tagPattern.MatchString(name[colon+1:]) {
			return false
		}
		name = name[:colon]
	}
	if name == "" || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.Contains(name, "//") {
		return false
	}
	for _, component := range strings.Split(name, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".") || strings.HasPrefix(component, "-") || strings.HasSuffix(component, "-") {
			return false
		}
		for _, r := range component {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' || r == ':' {
				continue
			}
			return false
		}
	}
	return true
}

// Allowed reports whether an image may be used for a role. The pinned CPA
// repository and the legacy CPA repository remain accepted for existing
// installations. Any other repository requires an explicit custom-image
// opt-in from the caller.
func Allowed(role, image string, allowCustom bool) bool {
	if !ValidReference(image) {
		return false
	}
	repository := Repository(image)
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "cpa":
		if repository == CPARepository || repository == LegacyCPARepository {
			return true
		}
	case "cpamp", "agent":
		if repository == CPAMPRepository {
			return true
		}
	default:
		// Custom-image opt-in applies only to the three managed stack roles;
		// accepting arbitrary roles would bypass manifest role validation.
		return false
	}
	return allowCustom
}
