package entity

// Machine identifies this computer to the server.
type Machine struct {
	MachineID string `json:"machine_id"`
	Label     string `json:"label"`
	// HostID is the HostID of the host that created MachineID. A different
	// value means the store was copied from another host.
	HostID string `json:"host_id,omitempty"`
}
