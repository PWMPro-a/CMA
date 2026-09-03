package containeropsimage

import "testing"

func TestAllowedCustomImageRequiresKnownRole(t *testing.T) {
	custom := "registry.example.test/team/cpa:2026.09"
	for _, role := range []string{"cpa", "cpamp", "agent"} {
		if !Allowed(role, custom, true) {
			t.Fatalf("custom image should be allowed for role %q with opt-in", role)
		}
		if Allowed(role, custom, false) {
			t.Fatalf("custom image should be blocked for role %q without opt-in", role)
		}
	}
	if Allowed("unknown", custom, true) {
		t.Fatal("custom image should be blocked for an unknown role")
	}
}

func TestAllowedPinnedAndLegacyCPAImages(t *testing.T) {
	for _, image := range []string{DefaultCPAImage, LegacyCPARepository + ":latest", CPARepository + "@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"} {
		if !Allowed("cpa", image, false) {
			t.Fatalf("expected CPA image to be allowed: %s", image)
		}
	}
}
