package types

// AgentSessionCreateRequest is the request body for creating an agent session.
type AgentSessionCreateRequest struct {
	Prompt         string                 `json:"prompt,omitempty"`
	Model          string                 `json:"model,omitempty"`
	SystemPrompt   string                 `json:"systemPrompt,omitempty"`
	AllowedTools   []string               `json:"allowedTools,omitempty"`
	PermissionMode string                 `json:"permissionMode,omitempty"`
	MaxTurns       int                    `json:"maxTurns,omitempty"`
	Cwd            string                 `json:"cwd,omitempty"`
	McpServers     map[string]interface{} `json:"mcpServers,omitempty"`
	Resume         string                 `json:"resume,omitempty"`
}

// AgentSessionInfo is the response body for agent session metadata.
type AgentSessionInfo struct {
	SessionID      string `json:"sessionID"`
	SandboxID      string `json:"sandboxID"`
	Running        bool   `json:"running"`
	StartedAt      string `json:"startedAt"`
	ClaudeSessionID string `json:"claudeSessionID,omitempty"`
}

// AgentPromptRequest is the request body for sending a prompt to an agent session.
type AgentPromptRequest struct {
	Text string `json:"text"`
}
