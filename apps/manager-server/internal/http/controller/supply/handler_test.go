package supply

import "testing"

func TestRetryRecoveryImportID(t *testing.T) {
	recoveryID, ok := retryRecoveryImportID("/v0/management/supply/recoveries/recovery-123/retry-import")
	if !ok || recoveryID != "recovery-123" {
		t.Fatalf("retryRecoveryImportID = %q, %v", recoveryID, ok)
	}
	for _, path := range []string{
		"/v0/management/supply/recoveries/recovery-123/claim",
		"/v0/management/supply/recoveries/a/b/retry-import",
		"/v0/management/supply/recoveries//retry-import",
	} {
		if recoveryID, ok := retryRecoveryImportID(path); ok {
			t.Fatalf("retryRecoveryImportID(%q) = %q, true", path, recoveryID)
		}
	}
}
