package terminal

import (
	"os"
	"os/exec"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/srl-labs/clab-api-server/internal/models"
)

func newTestManager(t *testing.T, maxPerUser int) *Manager {
	t.Helper()
	m := NewManager(time.Hour, time.Hour, time.Hour, maxPerUser)
	t.Cleanup(func() {
		close(m.stopCh)
		<-m.stoppedCh
	})
	return m
}

func TestResolveLaunchCommandShellAllowlist(t *testing.T) {
	command, err := resolveLaunchCommand(models.TerminalProtocolShell, "sid-1", CreateSessionOptions{
		Runtime:       "docker",
		ContainerID:   "cid-123",
		ContainerKind: "nokia_srlinux",
	})
	if err != nil {
		t.Fatalf("resolveLaunchCommand returned unexpected error: %v", err)
	}

	expected := []string{"docker", "exec", "-it", "-e", "NG_TERM_SESSION=sid-1", "cid-123", "sr_cli"}
	if strings.Join(command, " ") != strings.Join(expected, " ") {
		t.Fatalf("unexpected shell command: got %v want %v", command, expected)
	}
}

func TestResolveLaunchCommandFallbackShell(t *testing.T) {
	command, err := resolveLaunchCommand(models.TerminalProtocolShell, "sid-1", CreateSessionOptions{
		Runtime:       "podman",
		ContainerID:   "cid-456",
		ContainerKind: "unknown-kind",
	})
	if err != nil {
		t.Fatalf("resolveLaunchCommand returned unexpected error: %v", err)
	}

	expected := []string{"podman", "exec", "-it", "-e", "NG_TERM_SESSION=sid-1", "cid-456", "sh"}
	if strings.Join(command, " ") != strings.Join(expected, " ") {
		t.Fatalf("unexpected fallback shell command: got %v want %v", command, expected)
	}
}

func TestResolveLaunchCommandSSHUsesDirectArgv(t *testing.T) {
	command, err := resolveLaunchCommand(models.TerminalProtocolSSH, "sid-1", CreateSessionOptions{
		ContainerIP:   "172.20.20.2/24",
		ContainerKind: "cisco_xrd",
	})
	if err != nil {
		t.Fatalf("resolveLaunchCommand returned unexpected error: %v", err)
	}

	if len(command) == 0 || command[0] != "ssh" {
		t.Fatalf("expected ssh binary, got %v", command)
	}
	for _, disallowed := range []string{"sh", "bash", "-lc", "-c"} {
		for _, part := range command {
			if part == disallowed {
				t.Fatalf("unexpected shell wrapper token %q in %v", disallowed, command)
			}
		}
	}
	if last := command[len(command)-1]; last != "clab@172.20.20.2" {
		t.Fatalf("unexpected ssh target %q", last)
	}
}

func TestResolveLaunchCommandTelnetUsesDirectArgv(t *testing.T) {
	command, err := resolveLaunchCommand(models.TerminalProtocolTelnet, "sid-1", CreateSessionOptions{
		Runtime:     "docker",
		ContainerID: "cid-789",
		TelnetPort:  7001,
	})
	if err != nil {
		t.Fatalf("resolveLaunchCommand returned unexpected error: %v", err)
	}

	expected := []string{"docker", "exec", "-it", "-e", "NG_TERM_SESSION=sid-1", "cid-789", "telnet", "127.0.0.1", "7001"}
	if strings.Join(command, " ") != strings.Join(expected, " ") {
		t.Fatalf("unexpected telnet command: got %v want %v", command, expected)
	}
	for _, disallowed := range []string{"ssh", "sh", "bash", "-lc", "-c"} {
		for _, part := range command {
			if part == disallowed {
				t.Fatalf("unexpected shell/ssh wrapper token %q in %v", disallowed, command)
			}
		}
	}
}

func TestResolveLaunchCommandTelnetDefaultsPort(t *testing.T) {
	command, err := resolveLaunchCommand(models.TerminalProtocolTelnet, "sid-1", CreateSessionOptions{
		Runtime:     "podman",
		ContainerID: "cid-999",
		TelnetPort:  0,
	})
	if err != nil {
		t.Fatalf("resolveLaunchCommand returned unexpected error: %v", err)
	}

	expected := []string{"podman", "exec", "-it", "-e", "NG_TERM_SESSION=sid-1", "cid-999", "telnet", "127.0.0.1", "5000"}
	if strings.Join(command, " ") != strings.Join(expected, " ") {
		t.Fatalf("unexpected default telnet command: got %v want %v", command, expected)
	}
}

func TestSanitizeTerminalSizeClampsInvalidValues(t *testing.T) {
	cols, rows := sanitizeTerminalSize(0, 500)
	if cols != DefaultTerminalCols {
		t.Fatalf("unexpected cols: got %d want %d", cols, DefaultTerminalCols)
	}
	if rows != DefaultTerminalRows {
		t.Fatalf("unexpected rows: got %d want %d", rows, DefaultTerminalRows)
	}
}

func TestNewManagerDefaultsMaxSessionsPerUser(t *testing.T) {
	m := newTestManager(t, 0)
	if m.maxPerUser != DefaultMaxSessionsPerUser {
		t.Fatalf("unexpected maxPerUser: got %d want %d", m.maxPerUser, DefaultMaxSessionsPerUser)
	}
	if m.maxPerUser != 128 {
		t.Fatalf("unexpected default session limit: got %d want 128", m.maxPerUser)
	}
}

func TestNewManagerUsesConfiguredMaxSessionsPerUser(t *testing.T) {
	m := newTestManager(t, 150)
	if m.maxPerUser != 150 {
		t.Fatalf("unexpected maxPerUser: got %d want 150", m.maxPerUser)
	}
}

func TestReserveUserSessionCapacityRejectsAtConfiguredLimit(t *testing.T) {
	m := newTestManager(t, 2)
	m.sessions["active"] = &Session{username: "alice", state: "ready"}

	if err := m.reserveUserSessionCapacity("alice"); err != nil {
		t.Fatalf("reserve below limit returned unexpected error: %v", err)
	}
	if err := m.reserveUserSessionCapacity("alice"); err != ErrTooManySessions {
		t.Fatalf("reserve at limit returned %v, want %v", err, ErrTooManySessions)
	}
}

func TestReserveUserSessionCapacityIgnoresClosedSessions(t *testing.T) {
	m := newTestManager(t, 1)
	m.sessions["closed"] = &Session{username: "alice", state: "closed"}

	if err := m.reserveUserSessionCapacity("alice"); err != nil {
		t.Fatalf("reserve with only closed sessions returned unexpected error: %v", err)
	}
}

// NTG-194: IOL 15 only offers SHA-1 kex and ssh-rsa host keys, which OpenSSH 9 leaves out by
// default; the terminal must not depend on a host ssh_config entry for them.
func TestResolveLaunchCommandSSHAddsLegacyAlgorithms(t *testing.T) {
	command, err := resolveLaunchCommand(models.TerminalProtocolSSH, "sid-1", CreateSessionOptions{
		ContainerIP:   "172.20.15.2/24",
		ContainerKind: "cisco_iol",
	})
	if err != nil {
		t.Fatalf("resolveLaunchCommand returned unexpected error: %v", err)
	}
	joined := strings.Join(command, " ")
	for _, want := range []string{
		"-o KexAlgorithms=+diffie-hellman-group14-sha1,diffie-hellman-group-exchange-sha1",
		"-o HostKeyAlgorithms=+ssh-rsa",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("ssh argv %q lacks %q", joined, want)
		}
	}
	if last := command[len(command)-1]; last != "admin@172.20.15.2" {
		t.Fatalf("unexpected ssh target %q", last)
	}
}

func TestResolveLaunchCommandConsoleExecsAttach(t *testing.T) {
	opts := CreateSessionOptions{ContainerID: "cid", ContainerKind: "cisco_iol", Runtime: "docker"}
	for protocol, socket := range map[models.TerminalProtocol]string{
		models.TerminalProtocolConsole:        "console",
		models.TerminalProtocolConsoleControl: "control",
	} {
		got, err := resolveLaunchCommand(protocol, "sid-1", opts)
		if err != nil {
			t.Fatal(err)
		}
		// -i without -t: a closed session must end the attach inside the container too (stdin EOF).
		// With a tty, docker leaves the exec'd process running after the client dies (NTG-254 G1.3).
		want := []string{"docker", "exec", "-i", "cid", "/console-attach", socket}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %v want %v", protocol, got, want)
		}
	}
}

func TestRawPTYPassesBytesUntouched(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer ptmx.Close()
	defer tty.Close()
	if err := rawPTY(ptmx); err != nil {
		t.Fatal(err)
	}
	tio, err := unix.IoctlGetTermios(int(tty.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	if tio.Lflag&(unix.ICANON|unix.ECHO|unix.ISIG) != 0 || tio.Oflag&unix.OPOST != 0 || tio.Iflag&unix.ICRNL != 0 {
		t.Fatalf("pty still cooks console bytes: lflag=%#x oflag=%#x iflag=%#x", tio.Lflag, tio.Oflag, tio.Iflag)
	}
}

func TestResolveLaunchCommandConsoleOnlyForIOL(t *testing.T) {
	_, err := resolveLaunchCommand(models.TerminalProtocolConsole, "sid-1", CreateSessionOptions{ContainerID: "cid", ContainerKind: "linux"})
	// writeTerminalManagerError maps this text to 400, not 500.
	if err == nil || !strings.Contains(err.Error(), "unsupported terminal protocol") {
		t.Fatalf("console on a linux node must be refused as an unsupported protocol, got %v", err)
	}
}

func TestNormalizeProtocolAcceptsConsole(t *testing.T) {
	for _, p := range []string{"console", "CONSOLE", " console-control "} {
		if _, err := normalizeProtocol(models.TerminalProtocol(p)); err != nil {
			t.Errorf("%q: %v", p, err)
		}
	}
}

func TestConsoleControlSkipsUserCap(t *testing.T) {
	m := newTestManager(t, 1)
	if !m.countsTowardUserCap(models.TerminalProtocolConsole) {
		t.Error("student console sessions must count toward the cap")
	}
	if m.countsTowardUserCap(models.TerminalProtocolConsoleControl) {
		t.Error("grader control sessions must not count toward the cap")
	}
}

func TestCountSessionsForUserIgnoresConsoleControl(t *testing.T) {
	m := newTestManager(t, 1)
	m.sessions["ctl1"] = &Session{username: "grader", state: "ready", protocol: models.TerminalProtocolConsoleControl}
	m.sessions["ctl2"] = &Session{username: "grader", state: "ready", protocol: models.TerminalProtocolConsoleControl}
	if err := m.reserveUserSessionCapacity("grader"); err != nil {
		t.Fatalf("control sessions must not fill the cap: %v", err)
	}
	m.releaseUserSessionCapacity("grader")
	m.sessions["con1"] = &Session{username: "grader", state: "ready", protocol: models.TerminalProtocolConsole}
	if err := m.reserveUserSessionCapacity("grader"); err != ErrTooManySessions {
		t.Fatalf("a student console session must fill the cap, got %v", err)
	}
}

// NTG-256: docker never closes a -t exec's stdin when its client dies, so the shell inside
// would outlive the session; the marker env lets waitForProcess hang it up.
func TestKillSessionProcessesHangsUpMarked(t *testing.T) {
	start := func(sid string) *exec.Cmd {
		cmd := exec.Command("sleep", "30")
		cmd.Env = append(os.Environ(), "NG_TERM_SESSION="+sid)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		return cmd
	}
	marked, other := start("sid-1"), start("sid-2")
	exited := make(chan struct{})
	go func() { _ = marked.Wait(); close(exited) }()

	killSessionProcesses("sid-1")

	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("process marked sid-1 still running 2 s after killSessionProcesses")
	}
	if err := other.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("process marked sid-2 must survive, got %v", err)
	}
}
