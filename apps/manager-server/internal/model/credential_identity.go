package model

// CredentialIdentity identifies one credential for lifecycle cleanup after
// the corresponding auth file has been deleted from CPA.
type CredentialIdentity struct {
	AuthFileName    string
	AuthIndex       string
	Provider        string
	AccountSnapshot string
	AccountID       string
}
