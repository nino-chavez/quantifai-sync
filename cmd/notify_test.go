package cmd

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// notifyStatus sends exactly "STATUS=<text>" on one line to $NOTIFY_SOCKET,
// as systemd's sd_notify protocol expects.
func TestNotifyStatusSendsStatusDatagram(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unixgram sockets; systemd is Linux-only")
	}
	dir, err := os.MkdirTemp("/tmp", "qsn") // short: socket paths are length-limited
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "notify")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: sock, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	t.Setenv("NOTIFY_SOCKET", sock)

	notifyStatus("Waiting: no API key\nsecond line")

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("no status datagram: %v", err)
	}
	if got, want := string(buf[:n]), "STATUS=Waiting: no API key second line"; got != want {
		t.Fatalf("sent %q, want %q", got, want)
	}
}
