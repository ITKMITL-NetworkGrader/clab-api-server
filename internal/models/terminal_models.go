package models

import "time"

type TerminalProtocol string

const (
	TerminalProtocolSSH     TerminalProtocol = "ssh"
	TerminalProtocolShell   TerminalProtocol = "shell"
	TerminalProtocolTelnet  TerminalProtocol = "telnet"
	TerminalProtocolConsole TerminalProtocol = "console"
	// TerminalProtocolConsoleControl is the grader's: it holds the IOL console's write lock (NTG-254).
	TerminalProtocolConsoleControl TerminalProtocol = "console-control"
)

type TerminalSessionRequest struct {
	// Protocol selects the terminal transport: ssh, shell, telnet, console, or console-control.
	Protocol TerminalProtocol `json:"protocol" binding:"required"`
	// Cols is the requested terminal width in characters.
	Cols int `json:"cols"`
	// Rows is the requested terminal height in characters.
	Rows int `json:"rows"`
	// SSHUsername optionally overrides the SSH username for protocol=ssh.
	SSHUsername string `json:"sshUsername,omitempty"`
	// TelnetPort optionally overrides the telnet destination port for protocol=telnet.
	TelnetPort int `json:"telnetPort,omitempty"`
	// ResumeKey (NTG-505) makes a Linux ssh session resumable: the same key asks for the same node's shell
	// again after a dropped client (10 minutes). Identifies the person at the browser; 1-64 of A-Za-z0-9._@-.
	ResumeKey string `json:"resumeKey,omitempty"`
}

type TerminalSessionResponse struct {
	SessionID string           `json:"sessionId"`
	Protocol  TerminalProtocol `json:"protocol"`
	State     string           `json:"state"`
	CreatedAt time.Time        `json:"createdAt"`
	ExpiresAt time.Time        `json:"expiresAt"`
}

type TerminalSessionInfo struct {
	SessionID    string           `json:"sessionId"`
	Username     string           `json:"username"`
	LabName      string           `json:"labName"`
	NodeName     string           `json:"nodeName"`
	Protocol     TerminalProtocol `json:"protocol"`
	State        string           `json:"state"`
	CreatedAt    time.Time        `json:"createdAt"`
	ExpiresAt    time.Time        `json:"expiresAt"`
	LastActivity time.Time        `json:"lastActivity"`
	ExitCode     *int             `json:"exitCode,omitempty"`
	Error        string           `json:"error,omitempty"`
	// Reattached is true when the request returned an already running resumable session.
	Reattached bool `json:"reattached,omitempty"`
}
