package cmd

import (
	"net"
	"os"
	"strings"
)

// notifyStatus sets the one-line status systemd shows in `systemctl
// status` ("Status: ..."), using the sd_notify protocol: one datagram
// "STATUS=<text>" to the socket in $NOTIFY_SOCKET. Outside systemd, or
// when the unit does not set NotifyAccess=, the variable is unset and this
// does nothing. A leading "@" names an abstract socket, which Go's
// unixgram dialer handles. Errors are ignored: the status line is a
// courtesy, never a reason to stop.
func notifyStatus(text string) {
	socket := os.Getenv("NOTIFY_SOCKET")
	if socket == "" {
		return
	}
	conn, err := net.Dial("unixgram", socket)
	if err != nil {
		return
	}
	defer conn.Close()
	// systemd reads one line per field; keep the status on one line.
	conn.Write([]byte("STATUS=" + strings.ReplaceAll(text, "\n", " ")))
}
