package api

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// The display side: echoes what it reads, prefixed so the test sees which way bytes went.
func startFakeDisplay(t *testing.T) (net.Listener, chan net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conns := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		conns <- c
		_, _ = c.Write([]byte("RFB 003.008\n"))
		buf := make([]byte, 1024)
		for {
			n, err := c.Read(buf)
			if err != nil {
				return
			}
			_, _ = c.Write(append([]byte("echo:"), buf[:n]...))
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln, conns
}

func startPipeServer(t *testing.T, addr string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream, err := net.Dial("tcp", addr)
		if err != nil {
			t.Error(err)
			return
		}
		ws, err := terminalUpgrader.Upgrade(w, r, nil)
		if err != nil {
			upstream.Close()
			return
		}
		pipeVnc(ws, upstream)
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func readBinary(t *testing.T, ws *websocket.Conn, want string) {
	t.Helper()
	var got []byte
	_ = ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	for len(got) < len(want) {
		kind, data, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v (got %q)", err, got)
		}
		if kind != websocket.BinaryMessage {
			t.Fatalf("message type %d, want binary", kind)
		}
		got = append(got, data...)
	}
	if !bytes.Equal(got, []byte(want)) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestPipeVncCarriesBytesBothWays(t *testing.T) {
	ln, _ := startFakeDisplay(t)
	ws, _, err := websocket.DefaultDialer.Dial(startPipeServer(t, ln.Addr().String()), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()

	readBinary(t, ws, "RFB 003.008\n")
	if err := ws.WriteMessage(websocket.BinaryMessage, []byte("RFB 003.008\n")); err != nil {
		t.Fatal(err)
	}
	readBinary(t, ws, "echo:RFB 003.008\n")
}

func TestPipeVncClosesWebsocketWhenDisplayGoes(t *testing.T) {
	ln, conns := startFakeDisplay(t)
	ws, _, err := websocket.DefaultDialer.Dial(startPipeServer(t, ln.Addr().String()), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	readBinary(t, ws, "RFB 003.008\n")

	(<-conns).Close()
	_ = ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, err := ws.ReadMessage(); !websocket.IsCloseError(err, websocket.CloseNormalClosure) {
		t.Fatalf("want a normal close, got %v", err)
	}
}

func TestPipeVncClosesDisplayWhenWebsocketGoes(t *testing.T) {
	ln, conns := startFakeDisplay(t)
	ws, _, err := websocket.DefaultDialer.Dial(startPipeServer(t, ln.Addr().String()), nil)
	if err != nil {
		t.Fatal(err)
	}
	readBinary(t, ws, "RFB 003.008\n")
	display := <-conns
	ws.Close()

	_ = display.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadAll(display); err != nil {
		t.Fatalf("display connection was not closed: %v", err)
	}
}
