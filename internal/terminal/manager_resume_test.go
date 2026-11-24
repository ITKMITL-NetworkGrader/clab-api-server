package terminal

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/srl-labs/clab-api-server/internal/models"
)

// fakeSSH puts an `ssh` that is just cat first on PATH, so a resumable ssh session runs on a real pty.
func fakeSSH(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/ssh", []byte("#!/bin/sh\nexec cat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

func resumeOpts(key, node string) CreateSessionOptions {
	return CreateSessionOptions{
		Username: "svc", LabName: "lab", NodeName: node, ContainerID: "cid-" + node,
		ContainerIP: "10.0.0.2", ContainerKind: "linux", Protocol: models.TerminalProtocolSSH, ResumeKey: key,
	}
}

func mustCreate(t *testing.T, m *Manager, opts CreateSessionOptions) (*Session, *models.TerminalSessionInfo) {
	t.Helper()
	info, err := m.CreateSession(opts)
	if err != nil {
		t.Fatal(err)
	}
	s, err := m.lookupSession(info.SessionID, "svc", true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Terminate("test"); _ = s.WaitForExit(5 * time.Second) })
	return s, info
}

func readUntil(t *testing.T, st *Stream, want string) string {
	t.Helper()
	var got bytes.Buffer
	done := make(chan struct{})
	go func() {
		buf := make([]byte, 4096)
		for !strings.Contains(got.String(), want) {
			n, err := st.Read(buf)
			got.Write(buf[:n])
			if err != nil {
				break
			}
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %q, got %q", want, got.String())
	}
	if !strings.Contains(got.String(), want) {
		t.Fatalf("stream ended before %q, got %q", want, got.String())
	}
	return got.String()
}

func TestResumeReattachReplaysAndKeepsOneSession(t *testing.T) {
	fakeSSH(t)
	m := newTestManager(t, 50)
	s, info := mustCreate(t, m, resumeOpts("u1", "pc1"))
	if info.Reattached {
		t.Fatal("first create must not be a reattach")
	}
	_, st, err := m.BeginStream(info.SessionID, "svc", true)
	if err != nil {
		t.Fatal(err)
	}
	if st.Reattached {
		t.Fatal("first stream is not a reattach")
	}
	if err := s.WriteInput(st, "hello\n"); err != nil {
		t.Fatal(err)
	}
	readUntil(t, st, "hello")

	for i := 0; i < 5; i++ {
		s.Drop(st, "test")
		again, err := m.CreateSession(resumeOpts("u1", "pc1"))
		if err != nil {
			t.Fatal(err)
		}
		if again.SessionID != info.SessionID || !again.Reattached {
			t.Fatalf("round %d: want reattach of %s, got %+v", i, info.SessionID, again)
		}
		_, st, err = m.BeginStream(info.SessionID, "svc", true)
		if err != nil {
			t.Fatal(err)
		}
		if !st.Reattached || !bytes.Contains(st.Replay, []byte("hello")) {
			t.Fatalf("round %d: reattached=%v replay=%q", i, st.Reattached, st.Replay)
		}
	}
	if len(m.sessions) != 1 || len(m.byKey) != 1 {
		t.Fatalf("sessions=%d byKey=%d, want 1/1", len(m.sessions), len(m.byKey))
	}
}

func TestResumeParkExpiry(t *testing.T) {
	fakeSSH(t)
	m := newTestManager(t, 50)
	m.parkTimeout = 50 * time.Millisecond
	s, info := mustCreate(t, m, resumeOpts("u1", "pc1"))
	_, st, _ := m.BeginStream(info.SessionID, "svc", true)

	m.cleanupExpiredSessions()
	if s.isClosed() || s.state != "ready" {
		t.Fatal("an attached session must not expire")
	}
	s.Drop(st, "test")
	time.Sleep(80 * time.Millisecond)
	m.cleanupExpiredSessions()
	if err := s.WaitForExit(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	if s.Snapshot().Error != "parked session expired" {
		t.Fatalf("error = %q", s.Snapshot().Error)
	}
}

func TestResumeExpiryLosesToReattach(t *testing.T) {
	fakeSSH(t)
	m := newTestManager(t, 50)
	m.parkTimeout = 10 * time.Millisecond
	s, info := mustCreate(t, m, resumeOpts("u1", "pc1"))
	_, st, _ := m.BeginStream(info.SessionID, "svc", true)
	s.Drop(st, "test")
	time.Sleep(30 * time.Millisecond)
	s.mu.RLock()
	parkedAt := s.parkedAt
	s.mu.RUnlock()
	if _, _, err := m.BeginStream(info.SessionID, "svc", true); err != nil {
		t.Fatal(err)
	}
	// the cleanup pass read parkedAt before the reattach; its guard must veto
	s.terminate("parked session expired", func() bool { return s.cur == nil && s.parkedAt.Equal(parkedAt) })
	if s.state != "ready" {
		t.Fatalf("state = %s, reattached session was terminated", s.state)
	}
}

func TestResumeTakeover(t *testing.T) {
	fakeSSH(t)
	m := newTestManager(t, 50)
	s, info := mustCreate(t, m, resumeOpts("u1", "pc1"))
	_, st1, _ := m.BeginStream(info.SessionID, "svc", true)
	_, st2, err := m.BeginStream(info.SessionID, "svc", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st1.Read(make([]byte, 8)); !errors.Is(err, ErrTakenOver) {
		t.Fatalf("old stream read err = %v, want ErrTakenOver", err)
	}
	if err := s.WriteInput(st1, "x"); !errors.Is(err, ErrDetached) {
		t.Fatalf("old stream input err = %v", err)
	}
	if err := s.Resize(st1, 100, 30); !errors.Is(err, ErrDetached) {
		t.Fatalf("old stream resize err = %v", err)
	}
	// a stale tab leaving or closing must not touch the shell the new tab holds
	s.Drop(st1, "stale")
	s.Close(st1, "stale close")
	if s.state != "ready" || s.cur != st2 {
		t.Fatalf("stale stream changed the session: state=%s", s.state)
	}
	s.Close(st2, "deliberate")
	if err := s.WaitForExit(5 * time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestResumeOnlyForSSHLinuxWithKey(t *testing.T) {
	fakeSSH(t)
	m := newTestManager(t, 50)
	noKey := resumeOpts("", "pc1")
	s1, i1 := mustCreate(t, m, noKey)
	_, i2 := mustCreate(t, m, noKey)
	if i1.SessionID == i2.SessionID || s1.resumable || len(m.byKey) != 0 {
		t.Fatal("a request without a key must not be resumable")
	}
	iol := resumeOpts("u1", "r1")
	iol.ContainerKind = "cisco_iol"
	s3, _ := mustCreate(t, m, iol)
	if s3.resumable {
		t.Fatal("non-linux must not be resumable")
	}
	_, st, _ := m.BeginStream(i1.SessionID, "svc", true)
	if _, _, err := m.BeginStream(i1.SessionID, "svc", true); !errors.Is(err, ErrSessionAttached) {
		t.Fatalf("second attach to a non-resumable session = %v, want ErrSessionAttached", err)
	}
	s1.Drop(st, "client gone") // non-resumable: ends
	if err := s1.WaitForExit(5 * time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestResumeOtherContainerStartsFresh(t *testing.T) {
	fakeSSH(t)
	m := newTestManager(t, 50)
	_, first := mustCreate(t, m, resumeOpts("u1", "pc1"))
	changed := resumeOpts("u1", "pc1")
	changed.ContainerID = "recreated"
	s2, second := mustCreate(t, m, changed)
	if second.SessionID == first.SessionID || second.Reattached {
		t.Fatal("a recreated node must get a new session")
	}
	old, _ := m.lookupSession(first.SessionID, "svc", true)
	if err := old.WaitForExit(5 * time.Second); err != nil {
		t.Fatal("stale session was not terminated")
	}
	if m.byKey[s2.key] != s2 {
		t.Fatal("new session not registered under the key")
	}
}

func TestResumeViewersAreSeparate(t *testing.T) {
	fakeSSH(t)
	m := newTestManager(t, 50)
	_, a := mustCreate(t, m, resumeOpts("student", "pc1"))
	_, b := mustCreate(t, m, resumeOpts("staff", "pc1"))
	if a.SessionID == b.SessionID {
		t.Fatal("two viewers must not share a shell")
	}
}

func TestCloseResumableByNodeAndLab(t *testing.T) {
	fakeSSH(t)
	m := newTestManager(t, 50)
	s1, _ := mustCreate(t, m, resumeOpts("u1", "pc1"))
	s2, _ := mustCreate(t, m, resumeOpts("u1", "pc2"))
	other := resumeOpts("u1", "pc1")
	other.LabName = "other"
	s3, _ := mustCreate(t, m, other)

	m.CloseResumable("lab", "pc1")
	if err := s1.WaitForExit(5 * time.Second); err != nil {
		t.Fatal("pc1 not closed")
	}
	if s2.isClosed() || s3.isClosed() {
		t.Fatal("pc2 and the other lab must survive a node close")
	}
	m.CloseResumable("lab")
	if err := s2.WaitForExit(5 * time.Second); err != nil {
		t.Fatal("pc2 not closed by lab close")
	}
	if s3.isClosed() {
		t.Fatal("other lab must survive")
	}
}

func TestResumeBackendExitClearsKey(t *testing.T) {
	fakeSSH(t)
	m := newTestManager(t, 50)
	s, info := mustCreate(t, m, resumeOpts("u1", "pc1"))
	_, st, _ := m.BeginStream(info.SessionID, "svc", true)
	s.Drop(st, "test") // parked when the shell exits
	if err := s.WriteInput(st, "\x04"); !errors.Is(err, ErrDetached) {
		t.Fatalf("detached write = %v", err)
	}
	if _, err := s.term.Write([]byte("\x04")); err != nil { // Ctrl-D on the pty
		t.Fatal(err)
	}
	if err := s.WaitForExit(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.byKey) != 0 {
		t.Fatal("key must be released when the shell exits")
	}
}

func TestResumeRingIsBoundedAndReplayStartsOnALine(t *testing.T) {
	fakeSSH(t)
	m := newTestManager(t, 50)
	s, info := mustCreate(t, m, resumeOpts("u1", "pc1"))
	_, st, _ := m.BeginStream(info.SessionID, "svc", true)
	s.Drop(st, "test") // parked: pump keeps reading, nothing blocks
	line := strings.Repeat("x", 59) + "\n"
	for i := 0; i < 1500; i++ { // echo + cat output = ~180 KB
		if _, err := s.term.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.RLock()
		trimmed := s.trimmed
		s.mu.RUnlock()
		if trimmed || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, st2, _ := m.BeginStream(info.SessionID, "svc", true)
	if len(st2.Replay) == 0 || len(st2.Replay) > RingBytes {
		t.Fatalf("replay = %d bytes, want 1..%d", len(st2.Replay), RingBytes)
	}
	if !strings.HasPrefix(string(st2.Replay), "x") || strings.HasPrefix(string(st2.Replay), "\n") {
		t.Fatalf("replay must start at a line start, got %q", st2.Replay[:20])
	}
}

func TestResumeTerminateParkedSession(t *testing.T) {
	fakeSSH(t)
	m := newTestManager(t, 50)
	s, info := mustCreate(t, m, resumeOpts("u1", "pc1"))
	_, st, _ := m.BeginStream(info.SessionID, "svc", true)
	s.Drop(st, "test")
	if err := m.TerminateSession(info.SessionID, "svc", false); err != nil {
		t.Fatal(err)
	}
	if err := s.WaitForExit(5 * time.Second); err != nil {
		t.Fatal(err)
	}
}

// B2: a takeover racing the end of the session must either fail or hand out a stream that ends.
func TestResumeTakeoverRacesEnd(t *testing.T) {
	fakeSSH(t)
	for name, end := range map[string]func(s *Session){
		"terminate":  func(s *Session) { s.Terminate("test") },
		"backendEOF": func(s *Session) { _, _ = s.term.Write([]byte("\x04")) },
	} {
		for i := 0; i < 20; i++ {
			m := newTestManager(t, 50)
			s, info := mustCreate(t, m, resumeOpts("u1", "pc1"))
			_, st, _ := m.BeginStream(info.SessionID, "svc", true)
			s.Drop(st, "test")
			started := make(chan struct{})
			go func() { close(started); end(s) }()
			<-started
			_, st2, err := m.BeginStream(info.SessionID, "svc", true)
			if err != nil {
				if !errors.Is(err, ErrSessionExited) {
					t.Fatalf("%s: err = %v", name, err)
				}
				continue
			}
			done := make(chan struct{})
			go func() {
				buf := make([]byte, 256)
				for {
					if _, err := st2.Read(buf); err != nil {
						close(done)
						return
					}
				}
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatalf("%s round %d: a stream taken over a dying session never ended", name, i)
			}
		}
	}
}
