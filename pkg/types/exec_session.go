package types

// ExecSessionCreateRequest is the request body for creating an exec session.
type ExecSessionCreateRequest struct {
	Command               string            `json:"cmd"`
	Args                  []string          `json:"args,omitempty"`
	Env                   map[string]string `json:"envs,omitempty"`
	Cwd                   string            `json:"cwd,omitempty"`
	Timeout               int               `json:"timeout,omitempty"`
	MaxRunAfterDisconnect int               `json:"maxRunAfterDisconnect,omitempty"`
}

// ExecSessionInfo is the response body for exec session metadata.
type ExecSessionInfo struct {
	SessionID       string   `json:"sessionID"`
	SandboxID       string   `json:"sandboxID"`
	Command         string   `json:"command"`
	Args            []string `json:"args"`
	Running         bool     `json:"running"`
	ExitCode        *int     `json:"exitCode,omitempty"`
	StartedAt       string   `json:"startedAt"`
	AttachedClients int      `json:"attachedClients"`
}
