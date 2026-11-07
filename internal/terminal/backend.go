package terminal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	dockerclient "github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/srl-labs/clab-api-server/internal/models"
)

// backend is the far end of a terminal session: a local process on a pty, or (NTG-445) a Docker exec
// attached through the Engine API.
type backend interface {
	io.ReadWriter
	Resize(cols, rows int) error
	// Hangup asks the far end to finish (SIGHUP / stdin EOF); Kill forces it.
	Hangup()
	Kill()
	// Wait blocks until the far end has finished and returns its exit code.
	Wait() (int, error)
}

func startBackend(protocol models.TerminalProtocol, command []string, opts CreateSessionOptions, cols, rows int) (backend, error) {
	if args, ok := dockerAPIExec(protocol, command, opts); ok {
		return startDockerExec(opts.ContainerID, args)
	}
	return startPTY(protocol, command, cols, rows)
}

// dockerAPIExec: console and console-control on Docker go through the Engine API; returns the command
// to run inside the container (what follows the container ID in the CLI argv).
func dockerAPIExec(protocol models.TerminalProtocol, command []string, opts CreateSessionOptions) ([]string, bool) {
	if protocol != models.TerminalProtocolConsole && protocol != models.TerminalProtocolConsoleControl {
		return nil, false
	}
	if resolveRuntime(opts.Runtime) != "docker" {
		return nil, false
	}
	for i, arg := range command {
		if arg == opts.ContainerID && i+1 < len(command) {
			return command[i+1:], true
		}
	}
	return nil, false
}

type ptyBackend struct {
	cmd *exec.Cmd
	f   *os.File
}

func startPTY(protocol models.TerminalProtocol, command []string, cols, rows int) (*ptyBackend, error) {
	cmd := exec.CommandContext(context.Background(), command[0], command[1:]...)
	cmd.Env = minimalCommandEnv()
	cmd.Dir = "/"
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, err
	}
	if protocol == models.TerminalProtocolConsole || protocol == models.TerminalProtocolConsoleControl {
		if err := rawPTY(f); err != nil {
			_ = cmd.Process.Kill()
			_ = f.Close()
			_ = cmd.Wait()
			return nil, err
		}
	}
	return &ptyBackend{cmd: cmd, f: f}, nil
}

func (b *ptyBackend) Read(p []byte) (int, error)  { return b.f.Read(p) }
func (b *ptyBackend) Write(p []byte) (int, error) { return b.f.Write(p) }
func (b *ptyBackend) Resize(cols, rows int) error {
	return pty.Setsize(b.f, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}
func (b *ptyBackend) Hangup() {
	_ = b.f.Close()
	if b.cmd.Process != nil {
		_ = b.cmd.Process.Signal(syscall.SIGHUP)
	}
}
func (b *ptyBackend) Kill() {
	if b.cmd.Process != nil {
		_ = b.cmd.Process.Kill()
	}
}
func (b *ptyBackend) Wait() (int, error) {
	err := b.cmd.Wait()
	_ = b.f.Close()
	var exitErr *exec.ExitError
	switch {
	case err == nil && b.cmd.ProcessState != nil:
		return b.cmd.ProcessState.ExitCode(), nil
	case errors.As(err, &exitErr):
		return exitErr.ExitCode(), err
	case err != nil:
		return -1, err
	}
	return 0, nil
}

var (
	dockerOnce sync.Once
	dockerCli  *dockerclient.Client
	dockerErr  error
)

func sharedDocker() (*dockerclient.Client, error) {
	dockerOnce.Do(func() {
		dockerCli, dockerErr = dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	})
	return dockerCli, dockerErr
}

// dockerExecBackend replaces one `docker exec -i` CLI child per console (9.5 MiB PSS each, 2.55 GiB at
// 275 consoles in NTG-436) with an exec attached over the Engine API. Same exec as before: no tty, so
// bytes reach the console unchanged and closing the connection gives the exec stdin EOF, which ends
// /console-attach and frees the console lock (NTG-254).
type dockerExecBackend struct {
	cli    *dockerclient.Client
	execID string
	conn   types.HijackedResponse
	out    *io.PipeReader
	done   chan struct{}
	once   sync.Once
}

func startDockerExec(containerID string, cmd []string) (*dockerExecBackend, error) {
	cli, err := sharedDocker()
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	created, err := cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		AttachStdin: true, AttachStdout: true, AttachStderr: true, Cmd: cmd,
	})
	if err != nil {
		return nil, fmt.Errorf("exec create: %w", err)
	}
	conn, err := cli.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{})
	if err != nil {
		return nil, fmt.Errorf("exec attach: %w", err)
	}
	pr, pw := io.Pipe()
	b := &dockerExecBackend{cli: cli, execID: created.ID, conn: conn, out: pr, done: make(chan struct{})}
	go func() {
		// No tty: stdout and stderr arrive multiplexed; the terminal shows both, like the CLI did.
		_, copyErr := stdcopy.StdCopy(pw, pw, conn.Reader)
		_ = pw.CloseWithError(copyErr)
		close(b.done)
	}()
	return b, nil
}

func (b *dockerExecBackend) Read(p []byte) (int, error)  { return b.out.Read(p) }
func (b *dockerExecBackend) Write(p []byte) (int, error) { return b.conn.Conn.Write(p) }

// Resize: the exec has no tty, as with `exec -i`, so there is nothing to size.
func (b *dockerExecBackend) Resize(int, int) error { return nil }

func (b *dockerExecBackend) Hangup() { b.once.Do(b.conn.Close) }
func (b *dockerExecBackend) Kill()   { b.once.Do(b.conn.Close) }

func (b *dockerExecBackend) Wait() (int, error) {
	<-b.done
	b.once.Do(b.conn.Close)
	// The exec ends right after its streams close; give the daemon a moment to record the exit code.
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		inspect, err := b.cli.ContainerExecInspect(ctx, b.execID)
		cancel()
		if err != nil {
			return -1, err
		}
		if !inspect.Running {
			return inspect.ExitCode, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return -1, fmt.Errorf("exec %s still running after its streams closed", b.execID)
}
