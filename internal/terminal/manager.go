package terminal

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/log"
	"golang.org/x/term"

	"github.com/srl-labs/clab-api-server/internal/models"
)

const (
	DefaultCleanupTick        = time.Minute
	DefaultSessionTTL         = time.Hour
	DefaultIdleTimeout        = 15 * time.Minute
	DefaultMaxSessionsPerUser = 128
	DefaultTerminalCols       = 120
	DefaultTerminalRows       = 36
	DefaultTelnetPort         = 5000

	// NTG-505: a resumable session lives on this long after its client dropped, and keeps this much output
	// to replay to the next client.
	ParkTimeout = 10 * time.Minute
	RingBytes   = 64 * 1024
)

var (
	ErrSessionNotFound   = errors.New("terminal session not found")
	ErrSessionForbidden  = errors.New("terminal session not owned by user")
	ErrSessionAttached   = errors.New("terminal session already has an active client")
	ErrTooManySessions   = errors.New("too many terminal sessions for user")
	ErrSessionExited     = errors.New("terminal session has already exited")
	ErrTakenOver         = errors.New("terminal session taken over by another client")
	ErrDetached          = errors.New("terminal stream detached")
	validSSHUsernameExpr = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_-]{0,31}$`)
)

var defaultShellCommandsByKind = map[string][]string{
	"nokia_srlinux": {"sr_cli"},
	"cisco_xrd":     {"/pkg/bin/xr_cli.sh"},
}

var defaultSSHUserByKind = map[string]string{
	"nokia_srlinux": "admin",
	"nokia_sros":    "admin",
	"cisco_xrd":     "clab",
	"cisco_xr9vk":   "clab",
	"arista_ceos":   "admin",
	"juniper_crpd":  "root",
}

type CreateSessionOptions struct {
	Username      string
	LabName       string
	NodeName      string
	ContainerID   string
	ContainerIP   string
	ContainerKind string
	Runtime       string
	Protocol      models.TerminalProtocol
	Cols          int
	Rows          int
	SSHUsername   string
	TelnetPort    int
	// ResumeKey (NTG-505): see models.TerminalSessionRequest. Honoured only for ssh to a linux node.
	ResumeKey string
}

type Manager struct {
	mu          sync.RWMutex
	sessions    map[string]*Session
	pending     map[string]int
	byKey       map[string]*Session // resumable sessions, one per (user, viewer, lab, node)
	parkTimeout time.Duration
	cleanupTick time.Duration
	sessionTTL  time.Duration
	idleTimeout time.Duration
	maxPerUser  int
	stopCh      chan struct{}
	stoppedCh   chan struct{}
}

type Session struct {
	manager *Manager

	mu           sync.RWMutex
	id           string
	username     string
	labName      string
	nodeName     string
	protocol     models.TerminalProtocol
	createdAt    time.Time
	expiresAt    time.Time
	lastActivity time.Time
	state        string
	lastError    string
	exitCode     *int
	attached     bool

	// NTG-505, resumable sessions only. pump is the only reader of the backend; cur is the attached stream.
	resumable   bool
	key         string
	containerID string
	parkedAt    time.Time
	ring        []byte
	trimmed     bool
	cur         *Stream
	gen         int
	pumpDone    bool
	cols, rows  int

	term   backend
	doneCh chan struct{}
}

func NewManager(cleanupTick, sessionTTL, idleTimeout time.Duration, maxPerUser int) *Manager {
	if cleanupTick <= 0 {
		cleanupTick = DefaultCleanupTick
	}
	if sessionTTL <= 0 {
		sessionTTL = DefaultSessionTTL
	}
	if idleTimeout <= 0 {
		idleTimeout = DefaultIdleTimeout
	}
	if maxPerUser <= 0 {
		maxPerUser = DefaultMaxSessionsPerUser
	}

	m := &Manager{
		sessions:    make(map[string]*Session),
		pending:     make(map[string]int),
		byKey:       make(map[string]*Session),
		parkTimeout: ParkTimeout,
		cleanupTick: cleanupTick,
		sessionTTL:  sessionTTL,
		idleTimeout: idleTimeout,
		maxPerUser:  maxPerUser,
		stopCh:      make(chan struct{}),
		stoppedCh:   make(chan struct{}),
	}

	go m.runCleanup()
	return m
}

func (m *Manager) Shutdown() {
	close(m.stopCh)
	<-m.stoppedCh

	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
	}
	m.mu.Unlock()

	for _, session := range sessions {
		session.Terminate("server shutdown")
		_ = session.WaitForExit(2 * time.Second)
	}
}

func (m *Manager) CreateSession(opts CreateSessionOptions) (*models.TerminalSessionInfo, error) {
	protocol, err := normalizeProtocol(opts.Protocol)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(opts.Username) == "" {
		return nil, fmt.Errorf("username is required")
	}
	if strings.TrimSpace(opts.LabName) == "" {
		return nil, fmt.Errorf("lab name is required")
	}
	if strings.TrimSpace(opts.NodeName) == "" {
		return nil, fmt.Errorf("node name is required")
	}
	if strings.TrimSpace(opts.ContainerID) == "" {
		return nil, fmt.Errorf("container ID is required")
	}
	if protocol == models.TerminalProtocolSSH && strings.TrimSpace(opts.ContainerIP) == "" {
		return nil, fmt.Errorf("container management IP is required for SSH")
	}

	cols, rows := sanitizeTerminalSize(opts.Cols, opts.Rows)
	resumable := opts.ResumeKey != "" && protocol == models.TerminalProtocolSSH && strings.TrimSpace(opts.ContainerKind) == "linux"
	key := ""
	if resumable {
		key = strings.Join([]string{strings.TrimSpace(opts.Username), opts.ResumeKey, strings.TrimSpace(opts.LabName), strings.TrimSpace(opts.NodeName), "ssh"}, "\x00")
		if info := m.reattach(key, opts.ContainerID); info != nil {
			return info, nil
		}
	}
	sessionID := newSessionID()
	command, err := resolveLaunchCommand(protocol, sessionID, opts)
	if err != nil {
		return nil, err
	}

	username := strings.TrimSpace(opts.Username)
	counted := m.countsTowardUserCap(protocol)
	if counted {
		if err := m.reserveUserSessionCapacity(username); err != nil {
			return nil, err
		}
	}

	now := time.Now().UTC()
	session := &Session{
		manager:      m,
		id:           sessionID,
		username:     username,
		labName:      strings.TrimSpace(opts.LabName),
		nodeName:     strings.TrimSpace(opts.NodeName),
		protocol:     protocol,
		createdAt:    now,
		expiresAt:    now.Add(m.sessionTTL),
		lastActivity: now,
		state:        "ready",
		doneCh:       make(chan struct{}),
		resumable:    resumable,
		key:          key,
		containerID:  opts.ContainerID,
		parkedAt:     now,
		cols:         cols,
		rows:         rows,
	}

	term, err := startBackend(protocol, command, opts, cols, rows)
	if err != nil {
		if counted {
			m.releaseUserSessionCapacity(username)
		}
		return nil, fmt.Errorf("failed to start terminal session: %w", err)
	}
	session.term = term

	m.mu.Lock()
	if counted {
		m.releaseUserSessionCapacityLocked(session.username)
	}
	var stale *Session
	if resumable {
		// Two fresh requests at once (pop-out): the first one registered wins.
		if holder := m.byKey[key]; holder != nil {
			if holder.live(opts.ContainerID) {
				m.mu.Unlock()
				term.Hangup()
				term.Kill()
				go func() { _, _ = term.Wait() }()
				if info := m.reattach(key, opts.ContainerID); info != nil {
					return info, nil
				}
				return nil, ErrSessionExited
			}
			stale = holder
		}
		m.byKey[key] = session
	}
	m.sessions[session.id] = session
	m.mu.Unlock()
	if stale != nil {
		stale.Terminate("replaced by a new session")
	}

	go session.waitForProcess()
	if resumable {
		go session.pump()
	}

	log.Infof(
		"terminal session created: user=%s lab=%s node=%s protocol=%s session=%s",
		session.username,
		session.labName,
		session.nodeName,
		session.protocol,
		session.id,
	)

	info := session.Snapshot()
	return &info, nil
}

// live: ready, backend still producing output, and still on this container.
func (s *Session) live(containerID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state == "ready" && !s.pumpDone && s.containerID == containerID
}

// reattach returns the running resumable session for key, or nil. A stale one (other container, not
// ready) is terminated.
func (m *Manager) reattach(key, containerID string) *models.TerminalSessionInfo {
	m.mu.RLock()
	holder := m.byKey[key]
	m.mu.RUnlock()
	if holder == nil {
		return nil
	}
	if !holder.live(containerID) {
		holder.Terminate("replaced by a new session")
		return nil
	}
	info := holder.Snapshot()
	info.Reattached = true
	log.Infof("terminal session reattached: user=%s lab=%s node=%s session=%s", info.Username, info.LabName, info.NodeName, info.SessionID)
	return &info
}

// CloseResumable ends the resumable sessions of a lab's nodes (all nodes when none are given): a stopped,
// removed or recreated node must not keep a parked shell.
func (m *Manager) CloseResumable(lab string, nodes ...string) {
	var doomed []*Session
	m.mu.RLock()
	for _, s := range m.byKey {
		if s.labName != lab {
			continue
		}
		if len(nodes) > 0 {
			match := false
			for _, n := range nodes {
				if n == s.nodeName {
					match = true
				}
			}
			if !match {
				continue
			}
		}
		doomed = append(doomed, s)
	}
	m.mu.RUnlock()
	for _, s := range doomed {
		s.Terminate("node or lab stopped")
	}
}

// rawPTY: `exec -i` leaves the host pty cooked (the runtime CLI only sets raw mode for -t), which would
// echo input, turn Ctrl-C into SIGINT for the CLI and \n into \r\n. Console bytes must reach IOS as sent.
func rawPTY(f *os.File) error {
	_, err := term.MakeRaw(int(f.Fd()))
	return err
}

// countsTowardUserCap: the grader's control sessions are bounded by its worker count, and must still
// open when students hold every terminal the cap allows (NTG-254).
func (m *Manager) countsTowardUserCap(protocol models.TerminalProtocol) bool {
	return protocol != models.TerminalProtocolConsoleControl
}

func (m *Manager) reserveUserSessionCapacity(username string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.countSessionsForUserLocked(username)+m.pending[username] >= m.maxPerUser {
		return ErrTooManySessions
	}
	m.pending[username]++
	return nil
}

func (m *Manager) releaseUserSessionCapacity(username string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.releaseUserSessionCapacityLocked(username)
}

func (m *Manager) releaseUserSessionCapacityLocked(username string) {
	if m.pending[username] <= 1 {
		delete(m.pending, username)
		return
	}
	m.pending[username]--
}

func (m *Manager) GetSessionInfo(sessionID, username string, allowAll bool) (*models.TerminalSessionInfo, error) {
	session, err := m.lookupSession(sessionID, username, allowAll)
	if err != nil {
		return nil, err
	}
	info := session.Snapshot()
	return &info, nil
}

func (m *Manager) TerminateSession(sessionID, username string, allowAll bool) error {
	session, err := m.lookupSession(sessionID, username, allowAll)
	if err != nil {
		return err
	}
	session.Terminate("session terminated")
	return nil
}

func (m *Manager) BeginStream(sessionID, username string, allowAll bool) (*Session, *Stream, error) {
	session, err := m.lookupSession(sessionID, username, allowAll)
	if err != nil {
		return nil, nil, err
	}
	stream, err := session.beginStream()
	if err != nil {
		return nil, nil, err
	}
	return session, stream, nil
}

func (m *Manager) lookupSession(sessionID, username string, allowAll bool) (*Session, error) {
	m.mu.RLock()
	session := m.sessions[sessionID]
	m.mu.RUnlock()
	if session == nil {
		return nil, ErrSessionNotFound
	}

	session.mu.RLock()
	owner := session.username
	session.mu.RUnlock()
	if !allowAll && owner != username {
		return nil, ErrSessionForbidden
	}
	return session, nil
}

func (m *Manager) countSessionsForUserLocked(username string) int {
	count := 0
	for _, session := range m.sessions {
		session.mu.RLock()
		owner := session.username
		state := session.state
		session.mu.RUnlock()
		if owner == username && state != "closed" && m.countsTowardUserCap(session.protocol) {
			count++
		}
	}
	return count
}

func (m *Manager) runCleanup() {
	ticker := time.NewTicker(m.cleanupTick)
	defer func() {
		ticker.Stop()
		close(m.stoppedCh)
	}()

	for {
		select {
		case <-ticker.C:
			m.cleanupExpiredSessions()
		case <-m.stopCh:
			return
		}
	}
}

func (m *Manager) cleanupExpiredSessions() {
	now := time.Now().UTC()

	m.mu.RLock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
	}
	m.mu.RUnlock()

	for _, session := range sessions {
		session.mu.RLock()
		state := session.state
		attached := session.attached
		expiresAt := session.expiresAt
		lastActivity := session.lastActivity
		resumable := session.resumable
		parkedAt := session.parkedAt
		session.mu.RUnlock()

		if now.After(expiresAt) {
			session.Terminate("session TTL expired")
			continue
		}
		if resumable {
			if state == "ready" && !attached && now.Sub(parkedAt) > m.parkTimeout {
				session.terminate("parked session expired", func() bool {
					return session.cur == nil && session.parkedAt.Equal(parkedAt)
				})
			}
		} else if !attached && state == "ready" && now.Sub(lastActivity) > m.idleTimeout {
			session.Terminate("session idle timeout")
			continue
		}
		if state == "closed" && now.Sub(lastActivity) > m.cleanupTick {
			m.mu.Lock()
			if current := m.sessions[session.id]; current == session {
				delete(m.sessions, session.id)
			}
			m.mu.Unlock()
		}
	}
}

// Stream is one client's view of a session. A non-resumable session's stream reads the backend directly;
// a resumable one's reads what pump forwards, so a client can leave and another take over.
type Stream struct {
	s          *Session
	r          *io.PipeReader
	w          *io.PipeWriter
	Replay     []byte
	Reattached bool
}

func (st *Stream) Read(p []byte) (int, error) {
	if st.r != nil {
		return st.r.Read(p)
	}
	return st.s.Read(p)
}

func (s *Session) beginStream() (*Stream, error) {
	s.mu.Lock()
	if s.state != "ready" || s.pumpDone {
		s.mu.Unlock()
		return nil, ErrSessionExited
	}
	if !s.resumable {
		if s.attached {
			s.mu.Unlock()
			return nil, ErrSessionAttached
		}
		s.attached = true
		s.lastActivity = time.Now().UTC()
		s.mu.Unlock()
		return &Stream{s: s}, nil
	}

	prev := s.cur
	s.gen++
	r, w := io.Pipe()
	replay := append([]byte(nil), s.ring...)
	if s.trimmed {
		if i := bytes.IndexByte(replay, '\n'); i >= 0 {
			replay = replay[i+1:]
		}
	}
	st := &Stream{s: s, r: r, w: w, Replay: replay, Reattached: s.gen > 1}
	s.cur = st
	s.attached = true
	s.parkedAt = time.Time{}
	s.lastActivity = time.Now().UTC()
	s.mu.Unlock()

	if prev != nil { // closing the writer is what reaches the reader: a closed reader would report ErrClosedPipe
		_ = prev.w.CloseWithError(ErrTakenOver)
	}
	return st, nil
}

// pump is the only reader of a resumable session's backend: it keeps the ring and forwards to the
// attached stream, if any.
func (s *Session) pump() {
	buf := make([]byte, 4096)
	for {
		n, err := s.term.Read(buf)
		if n > 0 {
			s.mu.Lock()
			s.ring = append(s.ring, buf[:n]...)
			if len(s.ring) > RingBytes {
				s.ring = append([]byte(nil), s.ring[len(s.ring)-RingBytes:]...)
				s.trimmed = true
			}
			s.lastActivity = time.Now().UTC()
			var w *io.PipeWriter
			if s.cur != nil {
				w = s.cur.w
			}
			s.mu.Unlock()
			if w != nil {
				// A write error means the stream detached; the data stays in the ring.
				_, _ = w.Write(buf[:n])
			}
		}
		if err != nil {
			s.mu.Lock()
			s.pumpDone = true
			if s.cur != nil {
				_ = s.cur.w.Close()
			}
			s.mu.Unlock()
			return
		}
	}
}

// EndStream ends the client's use of the stream (the handler's defer).
func (s *Session) EndStream(st *Stream) {
	if st.r != nil {
		s.Drop(st, "stream ended")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attached = false
	s.lastActivity = time.Now().UTC()
}

// Drop is a client leaving without asking to close: a non-resumable session ends, a resumable one parks.
func (s *Session) Drop(st *Stream, reason string) {
	if st.r == nil {
		s.Terminate(reason)
		return
	}
	_ = st.w.CloseWithError(ErrDetached)
	s.mu.Lock()
	if s.cur != st || s.state != "ready" {
		s.mu.Unlock()
		return
	}
	s.cur = nil
	s.attached = false
	s.parkedAt = time.Now().UTC()
	s.mu.Unlock()
	log.Infof("terminal session parked: user=%s lab=%s node=%s session=%s reason=%s", s.username, s.labName, s.nodeName, s.id, reason)
}

// Close is the client's deliberate close: only the stream that holds the session may end it.
func (s *Session) Close(st *Stream, reason string) {
	s.mu.RLock()
	stale := s.resumable && s.cur != st
	s.mu.RUnlock()
	if !stale {
		s.Terminate(reason)
	}
}

func (s *Session) Read(p []byte) (int, error) {
	if s.isClosed() {
		return 0, io.EOF
	}
	n, err := s.term.Read(p)
	if n > 0 {
		s.markActivity()
	}
	return n, err
}

func (s *Session) isCurrent(st *Stream) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.resumable || s.cur == st
}

func (s *Session) WriteInput(st *Stream, input string) error {
	if s.isClosed() {
		return io.EOF
	}
	if !s.isCurrent(st) {
		return ErrDetached
	}
	s.markActivity()
	_, err := s.term.Write([]byte(input))
	return err
}

func (s *Session) Resize(st *Stream, cols, rows int) error {
	if s.isClosed() {
		return io.EOF
	}
	if !s.isCurrent(st) {
		return ErrDetached
	}
	cols, rows = sanitizeTerminalSize(cols, rows)
	s.mu.Lock()
	s.cols, s.rows = cols, rows
	s.lastActivity = time.Now().UTC()
	s.mu.Unlock()
	return s.term.Resize(cols, rows)
}

// Redraw makes a full-screen program repaint: a resize to the same size sends no SIGWINCH, so shrink by
// one row first and restore the stored size (which a browser resize in between may have changed).
func (s *Session) Redraw() {
	s.mu.RLock()
	cols, rows := s.cols, s.rows
	s.mu.RUnlock()
	_ = s.term.Resize(cols, rows-1)
	time.AfterFunc(150*time.Millisecond, func() {
		if s.isClosed() {
			return
		}
		s.mu.RLock()
		cols, rows := s.cols, s.rows
		s.mu.RUnlock()
		_ = s.term.Resize(cols, rows)
	})
}

func (s *Session) isClosed() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state == "closed"
}

func (s *Session) markActivity() {
	s.mu.Lock()
	s.lastActivity = time.Now().UTC()
	s.mu.Unlock()
}

func (s *Session) Snapshot() models.TerminalSessionInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return models.TerminalSessionInfo{
		SessionID:    s.id,
		Username:     s.username,
		LabName:      s.labName,
		NodeName:     s.nodeName,
		Protocol:     s.protocol,
		State:        s.state,
		CreatedAt:    s.createdAt,
		ExpiresAt:    s.expiresAt,
		LastActivity: s.lastActivity,
		ExitCode:     s.exitCode,
		Error:        s.lastError,
	}
}

func (s *Session) waitForProcess() {
	defer close(s.doneCh)

	exitCode, err := s.term.Wait()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		err = nil // a non-zero exit is reported through exitCode, as before
	}

	s.mu.Lock()
	if s.exitCode == nil {
		s.exitCode = &exitCode
	}
	// A session being terminated keeps the reason Terminate recorded.
	if err != nil && !errors.Is(err, os.ErrClosed) && s.state != "closing" {
		s.lastError = err.Error()
	}
	s.state = "closed"
	s.attached = false
	s.lastActivity = time.Now().UTC()
	s.mu.Unlock()

	if s.resumable {
		s.manager.mu.Lock()
		if s.manager.byKey[s.key] == s {
			delete(s.manager.byKey, s.key)
		}
		s.manager.mu.Unlock()
	}

	// After the state is closed: reading a D-state process's environ can block, and the
	// session must not sit in the user's cap meanwhile.
	if s.protocol == models.TerminalProtocolShell || s.protocol == models.TerminalProtocolTelnet {
		killSessionProcesses(s.id)
	}

	log.Infof(
		"terminal session closed: user=%s lab=%s node=%s protocol=%s session=%s exitCode=%d",
		s.username,
		s.labName,
		s.nodeName,
		s.protocol,
		s.id,
		exitCode,
	)
}

func (s *Session) WaitForExit(timeout time.Duration) error {
	select {
	case <-s.doneCh:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("timed out waiting for terminal session to exit")
	}
}

func (s *Session) Terminate(reason string) { s.terminate(reason, nil) }

// terminate: guard, when set, runs under s.mu and can veto (a reattach may have raced the caller).
func (s *Session) terminate(reason string, guard func() bool) {
	s.mu.Lock()
	if s.state == "closed" || (guard != nil && !guard()) {
		s.mu.Unlock()
		return
	}
	s.state = "closing"
	s.lastError = reason
	s.lastActivity = time.Now().UTC()
	term := s.term
	s.mu.Unlock()

	term.Hangup()
	time.AfterFunc(750*time.Millisecond, term.Kill)
}

func sanitizeTerminalSize(cols, rows int) (int, int) {
	if cols < 40 || cols > 400 {
		cols = DefaultTerminalCols
	}
	if rows < 12 || rows > 200 {
		rows = DefaultTerminalRows
	}
	return cols, rows
}

func normalizeProtocol(protocol models.TerminalProtocol) (models.TerminalProtocol, error) {
	switch models.TerminalProtocol(strings.ToLower(strings.TrimSpace(string(protocol)))) {
	case models.TerminalProtocolSSH:
		return models.TerminalProtocolSSH, nil
	case models.TerminalProtocolShell:
		return models.TerminalProtocolShell, nil
	case models.TerminalProtocolTelnet:
		return models.TerminalProtocolTelnet, nil
	case models.TerminalProtocolConsole:
		return models.TerminalProtocolConsole, nil
	case models.TerminalProtocolConsoleControl:
		return models.TerminalProtocolConsoleControl, nil
	default:
		return "", fmt.Errorf("unsupported terminal protocol %q", protocol)
	}
}

// LegacySSHOptions (NTG-194): Cisco IOL 15 only offers SHA-1 kex and ssh-rsa host keys, which
// OpenSSH 9 leaves out by default. "+" only adds them, so newer nodes negotiate as before.
var LegacySSHOptions = []string{
	"-o", "KexAlgorithms=+diffie-hellman-group14-sha1,diffie-hellman-group-exchange-sha1",
	"-o", "HostKeyAlgorithms=+ssh-rsa",
}

func resolveLaunchCommand(protocol models.TerminalProtocol, sessionID string, opts CreateSessionOptions) ([]string, error) {
	switch protocol {
	case models.TerminalProtocolSSH:
		ip := stripCIDRSuffix(opts.ContainerIP)
		if ip == "" {
			return nil, fmt.Errorf("container management IP is required for SSH")
		}
		username := resolveSSHUsername(opts.ContainerKind, opts.SSHUsername)
		return append(append([]string{
			"ssh",
		}, LegacySSHOptions...),
			"-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null",
			"-o", "GlobalKnownHostsFile=/dev/null",
			"-o", "LogLevel=ERROR",
			"-o", "ServerAliveInterval=30",
			"-o", "ServerAliveCountMax=3",
			fmt.Sprintf("%s@%s", username, ip),
		), nil
	case models.TerminalProtocolShell:
		command := resolveShellCommand(opts.ContainerKind)
		return append([]string{resolveRuntime(opts.Runtime), "exec", "-it", "-e", sessionEnvKey + "=" + sessionID, opts.ContainerID}, command...), nil
	case models.TerminalProtocolTelnet:
		port := opts.TelnetPort
		if port <= 0 || port > 65535 {
			port = DefaultTelnetPort
		}
		return []string{
			resolveRuntime(opts.Runtime),
			"exec",
			"-it",
			"-e", sessionEnvKey + "=" + sessionID,
			opts.ContainerID,
			"telnet",
			"127.0.0.1",
			fmt.Sprintf("%d", port),
		}, nil
	case models.TerminalProtocolConsole, models.TerminalProtocolConsoleControl:
		// NTG-254: the IOL image's relay serves con0 over unix sockets inside the container.
		if strings.TrimSpace(opts.ContainerKind) != "cisco_iol" {
			// The wording is what writeTerminalManagerError maps to 400.
			return nil, fmt.Errorf("unsupported terminal protocol %q for kind %q (cisco_iol only)", protocol, opts.ContainerKind)
		}
		socket := "console"
		if protocol == models.TerminalProtocolConsoleControl {
			socket = "control"
		}
		// -i without -t: with a tty, docker keeps the exec'd attach running after its client is killed,
		// so a closed grader session would hold the console lock forever. Without one, stdin EOF ends it.
		return []string{resolveRuntime(opts.Runtime), "exec", "-i", opts.ContainerID, "/console-attach", socket}, nil
	default:
		return nil, fmt.Errorf("unsupported terminal protocol %q", protocol)
	}
}

func resolveShellCommand(kind string) []string {
	if command, ok := defaultShellCommandsByKind[strings.TrimSpace(kind)]; ok && len(command) > 0 {
		return append([]string(nil), command...)
	}
	return []string{"sh"}
}

func resolveSSHUsername(kind, requested string) string {
	if trimmed := strings.TrimSpace(requested); validSSHUsernameExpr.MatchString(trimmed) {
		return trimmed
	}
	if username, ok := defaultSSHUserByKind[strings.TrimSpace(kind)]; ok && username != "" {
		return username
	}
	return "admin"
}

func resolveRuntime(runtime string) string {
	if trimmed := strings.TrimSpace(runtime); trimmed != "" {
		return trimmed
	}
	return "docker"
}

func stripCIDRSuffix(ip string) string {
	trimmed := strings.TrimSpace(ip)
	if trimmed == "" {
		return ""
	}
	if slash := strings.IndexByte(trimmed, '/'); slash >= 0 {
		return trimmed[:slash]
	}
	return trimmed
}

func minimalCommandEnv() []string {
	return []string{
		"HOME=/tmp",
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"TERM=xterm-256color",
	}
}

func newSessionID() string {
	var buffer [16]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		panic(fmt.Errorf("failed to generate terminal session ID: %w", err))
	}
	return hex.EncodeToString(buffer[:])
}

// sessionEnvKey marks every process a shell/telnet session starts in the container.
const sessionEnvKey = "NG_TERM_SESSION"

// killSessionProcesses hangs up the processes a shell/telnet session left in its container.
// docker only closes an exec's stdin on client disconnect without a tty, so a -t shell never
// gets EOF (NTG-256); children such as ping inherit the marker env and get hung up too.
// ponytail: works because the API server runs on the host as root and sees container processes
// in the host /proc; inside a container it silently does nothing. Accepted limits: SIGHUP only, so
// nohup'd, trapped, or `env -u`'d processes survive (like with ssh); a session terminated right at
// creation can miss an exec that has not started yet; a pid can be reused between the read and the
// kill (upgrade path: pidfd_open + pidfd_send_signal).
func killSessionProcesses(sessionID string) {
	marker := []byte("\x00" + sessionEnvKey + "=" + sessionID + "\x00")
	environs, _ := filepath.Glob("/proc/[0-9]*/environ")
	for _, path := range environs {
		data, err := os.ReadFile(path)
		if err != nil || !bytes.Contains(append([]byte{0}, data...), marker) {
			continue
		}
		pid, err := strconv.Atoi(filepath.Base(filepath.Dir(path)))
		if err == nil {
			_ = syscall.Kill(pid, syscall.SIGHUP)
		}
	}
}
