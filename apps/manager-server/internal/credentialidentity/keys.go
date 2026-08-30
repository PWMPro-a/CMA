package credentialidentity

import (
	"strings"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/model"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/usageidentity"
)

// AccountKeys returns the account-history keys that can have been generated
// for one credential identity across current and legacy writers. File-scoped
// keys are preferred so deleting one member of a shared account never removes
// a sibling credential's history.
func AccountKeys(identity model.CredentialIdentity) []string {
	fileName := strings.TrimSpace(identity.AuthFileName)
	provider := strings.TrimSpace(identity.Provider)
	index := strings.TrimSpace(identity.AuthIndex)
	accountID := strings.TrimSpace(identity.AccountID)
	accountSnapshot := strings.TrimSpace(identity.AccountSnapshot)
	keys := make([]string, 0, 5)
	seen := make(map[string]struct{}, 5)
	appendKey := func(fields usageidentity.Fields) {
		key, ok := usageidentity.AccountKey(fields)
		if !ok || key == "" {
			return
		}
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	if fileName != "" && index != "" {
		appendKey(usageidentity.Fields{AuthFileSnapshot: fileName, AuthIndex: index, AuthProviderSnapshot: provider})
	}
	if fileName != "" && accountID != "" {
		appendKey(usageidentity.Fields{AuthFileSnapshot: fileName, AuthProjectIDSnapshot: accountID, AuthProviderSnapshot: provider})
	}
	if fileName != "" && accountSnapshot != "" {
		appendKey(usageidentity.Fields{AuthFileSnapshot: fileName, AccountSnapshot: accountSnapshot, AuthProviderSnapshot: provider})
	}
	if fileName != "" && index == "" && accountID == "" && accountSnapshot == "" {
		appendKey(usageidentity.Fields{AuthFileSnapshot: fileName, AuthProviderSnapshot: provider})
	}
	if index != "" {
		appendKey(usageidentity.Fields{AuthIndex: index, AuthProviderSnapshot: provider})
	}
	if accountID != "" {
		appendKey(usageidentity.Fields{AuthProjectIDSnapshot: accountID, AuthProviderSnapshot: provider})
	}
	if accountSnapshot != "" {
		appendKey(usageidentity.Fields{AccountSnapshot: accountSnapshot, AuthProviderSnapshot: provider})
	}
	return keys
}
