package entity

// Credential is a short-lived credential for the user-computer channel.
type Credential struct {
	Credential          string `json:"credential"`
	AccountScope        string `json:"account_scope,omitempty"`
	Backend             string `json:"backend,omitempty"`
	MachineID           string `json:"machine_id,omitempty"`
	ExpiresAtMS         int64  `json:"expires_at_ms"`
	ServerAuthoritative bool   `json:"server_authoritative"`
}
