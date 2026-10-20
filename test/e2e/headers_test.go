//go:build e2e

package e2e

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// TestStalledConnectionsAreCutOff covers the deadline on the start of a
// request. The body timeouts are deliberately unset so object streams are not
// cut off, and net/http derives both the header deadline and the TLS
// handshake deadline from ReadTimeout when ReadHeaderTimeout is unset, so
// without a header timeout a connection that trickles header bytes, or stalls
// mid-handshake, is held forever.
func TestStalledConnectionsAreCutOff(t *testing.T) {
	tests := []struct {
		name    string
		partial string
		useTLS  bool
	}{
		{
			name:    "unfinished headers",
			partial: "GET /_health HTTP/1.1\r\nHost: localhost\r\n",
		},
		{
			name:    "unfinished TLS handshake",
			useTLS:  true,
			partial: "\x16\x03\x01\x00\x80",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := startServer(t, tt.useTLS, map[string]string{
				"TULE_Server_HeaderTimeout": "500ms",
			})

			conn, err := net.Dial("tcp", srv.Addr)
			requireNoErr(t, err, "dial")
			defer func() { _ = conn.Close() }()

			_, err = io.WriteString(conn, tt.partial)
			requireNoErr(t, err, "write partial request")

			// Generous next to the 500ms deadline, but far short of waiting
			// forever: hitting it means the server never hung up.
			requireNoErr(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)), "set deadline")

			if _, err = io.ReadAll(conn); err != nil {
				var netErr net.Error
				if errors.As(err, &netErr) && netErr.Timeout() {
					t.Fatal("the server held the stalled connection open")
				}
				// Any other read error is the server closing on us, which is
				// the point.
			}
		})
	}
}
