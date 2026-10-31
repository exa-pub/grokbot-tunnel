package app

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	"github.com/exa-pub/grokbot-tunnel/tests/e2e/fakesand"
)

// listenNotify points NOTIFY_SOCKET at a socket like systemd's for
// Type=notify services.
func listenNotify(t *testing.T) *net.UnixConn {
	t.Helper()
	addr := filepath.Join(t.TempDir(), "notify")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: addr, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	t.Setenv("NOTIFY_SOCKET", addr)
	return conn
}

func readNotify(t *testing.T, conn *net.UnixConn) string {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	return string(buf[:n])
}

// served reports whether frames answer the requests of
// TestLoginConnectEndToEndLocal, failing the test on any throw.
func served(t *testing.T, frames []*aiserverv1.GrokBotUserComputerResponseFrame) bool {
	read, shell := false, false
	closed, files := 0, 0
	for _, f := range frames {
		if msg := fakesand.Client(f); msg != nil {
			read = read || msg.GetReadResult().GetSuccess().GetContent() == "world"
			shell = shell || msg.GetShellStream().GetStdout().GetData() == "shell-ok"
		}
		if msg := fakesand.Control(f); msg != nil {
			if msg.GetStreamClose() != nil {
				closed++
			}
			if msg.GetThrow() != nil {
				t.Error(msg.GetThrow())
			}
		}
		if f.GetFile() != nil {
			files++
		}
	}
	return read && shell && closed == 3 && files == 2
}

func TestLoginConnectEndToEndLocal(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	t.Setenv("HOME", home)
	os.WriteFile(filepath.Join(root, "hello"), []byte("world"), 0600)
	cloud := fakesand.New(t)
	cloud.Setenv(t)
	var out bytes.Buffer
	if err := Login(context.Background(), LoginOptions{NoBrowser: true}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Logged in as "+fakesand.Email) {
		t.Fatal(out.String())
	}
	cloud.Enqueue(
		fakesand.Exec("0", `{"id":900,"shellStreamArgs":{"command":"printf shell-ok"}}`),
		fakesand.Exec("1", `{"id":900,"readArgs":{"path":"hello"}}`),
		fakesand.Exec("2", `{"id":900,"lsArgs":{"path":"."}}`),
		fakesand.Upload("u", "uploaded", []byte("upload-ok")),
		fakesand.Download("d", "hello"),
	)
	notifications := listenNotify(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Connect(ctx, ConnectOptions{Root: root, Permission: "always"}) }()
	cloud.Wait(5*time.Second, func(frames []*aiserverv1.GrokBotUserComputerResponseFrame) bool { return served(t, frames) })
	if got := readNotify(t, notifications); got != "READY=1" {
		t.Fatal("systemd notification", got)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("graceful shutdown failed")
	}
	if got := readNotify(t, notifications); got != "STOPPING=1" {
		t.Fatal("systemd notification", got)
	}
	b, err := os.ReadFile(filepath.Join(root, "uploaded"))
	if err != nil || string(b) != "upload-ok" {
		t.Fatal(string(b), err)
	}
}
