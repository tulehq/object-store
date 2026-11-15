//go:build e2e

package e2e

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

// startLiveTLS starts the app as startLive does, behind a TLS-terminating
// listener that relays each connection's bytes unchanged to the app's
// plain-HTTP server, as a TLS-terminating load balancer in front of a
// deployment would. object-store sets no certificate on its server, so it
// serves plain HTTP, and this is how it is reached over TLS.
//
// The returned server's URL and Addr are the terminator's, and its Client
// trusts the terminator's self-signed certificate.
func startLiveTLS(t *testing.T, env map[string]string) *liveServer {
	t.Helper()
	backend := startLive(t, env)

	cert, roots := selfSignedCert(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	})
	requireNoErr(t, err, "listen for TLS")

	term := &tlsTerminator{t: t, ln: ln, backend: backend.Addr, conns: map[net.Conn]struct{}{}}
	term.wg.Add(1)
	go term.accept()

	tr := &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs:    roots,
		MinVersion: tls.VersionTLS12,
	}}
	// Cleanups run last first: this one runs before the app stops.
	t.Cleanup(func() {
		tr.CloseIdleConnections()
		term.close()
	})

	addr := ln.Addr().String()
	return &liveServer{URL: "https://" + addr, Addr: addr, httpClient: &http.Client{Transport: tr}}
}

// tlsTerminator accepts TLS connections on ln and relays each one to
// backend over plain TCP.
type tlsTerminator struct {
	t       *testing.T
	ln      net.Listener
	conns   map[net.Conn]struct{}
	backend string
	wg      sync.WaitGroup
	mu      sync.Mutex
	closed  bool
}

func (p *tlsTerminator) accept() {
	defer p.wg.Done()
	for {
		client, err := p.ln.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				p.t.Errorf("TLS terminator accept: %v", err)
			}
			return
		}
		p.wg.Add(1)
		go p.relay(client)
	}
}

// relay copies bytes both ways between client and a new connection to the
// backend until either side closes, then closes both.
func (p *tlsTerminator) relay(client net.Conn) {
	defer p.wg.Done()
	if !p.track(client) {
		return
	}
	defer p.untrack(client)

	backend, err := net.Dial("tcp", p.backend)
	if err != nil {
		p.t.Errorf("TLS terminator dial %s: %v", p.backend, err)
		return
	}
	if !p.track(backend) {
		return
	}
	defer p.untrack(backend)

	// A copy ends when its source closes or the other copy closes both
	// connections, so its error says nothing the client does not see.
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(backend, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, backend); done <- struct{}{} }()
	<-done
	_ = client.Close()
	_ = backend.Close()
	<-done
}

// track records c so close can close it. It closes c and reports false
// once the terminator is closed.
func (p *tlsTerminator) track(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		_ = c.Close()
		return false
	}
	p.conns[c] = struct{}{}
	return true
}

func (p *tlsTerminator) untrack(c net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_ = c.Close()
	delete(p.conns, c)
}

// close stops accepting, closes every relayed connection and waits for
// the relays to end.
func (p *tlsTerminator) close() {
	if err := p.ln.Close(); err != nil {
		p.t.Errorf("close TLS listener: %v", err)
	}
	p.mu.Lock()
	p.closed = true
	for c := range p.conns {
		_ = c.Close()
	}
	p.mu.Unlock()
	p.wg.Wait()
}

// selfSignedCert returns a certificate for 127.0.0.1 and localhost, valid
// for the test, and a pool trusting it.
func selfSignedCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	requireNoErr(t, err, "generate TLS key")
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "object-store e2e"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	requireNoErr(t, err, "create TLS certificate")
	leaf, err := x509.ParseCertificate(der)
	requireNoErr(t, err, "parse TLS certificate")

	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, roots
}
