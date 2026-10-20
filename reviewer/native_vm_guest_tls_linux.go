//go:build linux

package reviewer

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"
)

const nativeGuestCAPath = "/sfl-provider-ca.pem"

func nativeGuestTLSEnabled(policy EgressPolicy) bool {
	for _, route := range policy.Routes {
		if route.Kind == "refresh" || route.TLSOnly {
			return true
		}
	}
	return false
}

// The CA exists only inside one disposable client namespace. Its public
// certificate is readable by the CLI; the key stays in its root supervisor.
// CONNECT admits only exact refresh/metadata authorities from captured policy.
type nativeGuestTLSProxy struct {
	ctx         context.Context
	cancel      context.CancelFunc
	certificate tls.Certificate
	authorities map[string]bool
	handler     http.Handler
	stop        func()
	mu          sync.Mutex
	closed      bool
	failure     int
	connections map[*nativeGuestTLSConn]bool
	wg          sync.WaitGroup
}

func newNativeGuestTLSProxy(parent context.Context, policy EgressPolicy, handler http.Handler, stop func()) (*nativeGuestTLSProxy, []byte, error) {
	if parent == nil || handler == nil || stop == nil {
		return nil, nil, ErrVMBoot
	}
	authorities := map[string]bool{}
	for _, route := range policy.Routes {
		if route.Kind == "refresh" || route.Kind == "metadata" {
			if !egressAuthority(route.Authority) {
				return nil, nil, ErrVMBoot
			}
			authorities[route.Authority] = true
		}
	}
	if len(authorities) == 0 || len(authorities) > 8 {
		return nil, nil, ErrVMBoot
	}
	names := make([]string, 0, len(authorities))
	for name := range authorities {
		names = append(names, name)
	}
	sort.Strings(names)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, ErrVMBoot
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, ErrVMBoot
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "SFL private session CA"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(10 * time.Minute), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		return nil, nil, ErrVMBoot
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, ErrVMBoot
	}
	leaf := &x509.Certificate{SerialNumber: new(big.Int).Add(serial, big.NewInt(1)), Subject: pkix.Name{CommonName: "SFL private session"}, DNSNames: names,
		NotBefore: template.NotBefore, NotAfter: template.NotAfter, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, template, leafKey.Public(), key)
	if err != nil {
		return nil, nil, ErrVMBoot
	}
	ctx, cancel := context.WithCancel(parent)
	p := &nativeGuestTLSProxy{ctx: ctx, cancel: cancel, certificate: tls.Certificate{Certificate: [][]byte{leafDER, der}, PrivateKey: leafKey}, authorities: authorities, handler: handler, stop: stop, connections: map[*nativeGuestTLSConn]bool{}}
	return p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

func (p *nativeGuestTLSProxy) ServeHTTP(w http.ResponseWriter, q *http.Request) {
	if q.Method != http.MethodConnect {
		p.handler.ServeHTTP(w, q)
		return
	}
	authority := ""
	for name := range p.authorities {
		if q.Host == name+":443" {
			authority = name
		}
	}
	if authority == "" || q.URL.Host != q.Host || q.URL.Scheme != "" || q.URL.User != nil || q.URL.Path != "" || q.URL.RawPath != "" || q.URL.RawQuery != "" || q.URL.Fragment != "" || q.ContentLength != 0 || len(q.TransferEncoding) != 0 || len(q.Trailer) != 0 || q.Header.Get("Upgrade") != "" {
		p.fail(221)
		http.Error(w, "refresh authority denied", http.StatusForbidden)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		p.fail(222)
		http.Error(w, "private TLS unavailable", 500)
		return
	}
	connection, buffered, err := hijacker.Hijack()
	if err != nil {
		p.fail(222)
		return
	}
	owned := &nativeGuestTLSConn{Conn: connection, done: make(chan struct{})}
	p.mu.Lock()
	if p.closed || len(p.connections) >= 4 || buffered.Reader.Buffered() != 0 {
		p.mu.Unlock()
		owned.Close()
		p.fail(222)
		return
	}
	p.connections[owned] = true
	p.wg.Add(1)
	p.mu.Unlock()
	defer func() { owned.Close(); p.mu.Lock(); delete(p.connections, owned); p.mu.Unlock(); p.wg.Done() }()
	if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil || buffered.Flush() != nil {
		return
	}
	configuration := &tls.Config{Certificates: []tls.Certificate{p.certificate}, MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}
	configuration.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		if hello.ServerName != authority {
			return nil, ErrVMBoot
		}
		return nil, nil
	}
	secured := tls.Server(owned, configuration)
	handshake, cancel := context.WithTimeout(p.ctx, 5*time.Second)
	err = secured.HandshakeContext(handshake)
	cancel()
	if err != nil {
		p.fail(223)
		return
	}
	if deadline, ok := p.ctx.Deadline(); ok {
		secured.SetDeadline(deadline)
	}
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 7 * time.Minute, WriteTimeout: 7 * time.Minute, IdleTimeout: time.Second,
		MaxHeaderBytes: 16 << 10, ErrorLog: log.New(io.Discard, "", 0), BaseContext: func(net.Listener) context.Context { return p.ctx },
		Handler: http.HandlerFunc(func(w http.ResponseWriter, in *http.Request) {
			p.mu.Lock()
			if p.closed || owned.closing {
				p.mu.Unlock()
				http.Error(w, "private TLS stopped", 503)
				return
			}
			p.wg.Add(1)
			p.mu.Unlock()
			defer p.wg.Done()
			if in.Host != authority && in.Host != authority+":443" {
				p.fail(224)
				http.Error(w, "refresh host denied", 403)
				return
			}
			p.handler.ServeHTTP(w, in)
		})}
	listener := &nativeGuestTLSListener{connection: secured, done: owned.done}
	server.Serve(listener)
	p.mu.Lock()
	owned.closing = true
	p.mu.Unlock()
	server.Close()
}

func (p *nativeGuestTLSProxy) Close() error {
	p.mu.Lock()
	p.closed = true
	p.cancel()
	for conn := range p.connections {
		conn.Close()
	}
	p.mu.Unlock()
	done := make(chan struct{})
	go func() { p.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-time.After(2 * time.Second):
		return ErrVMBoot
	}
}

type nativeGuestTLSConn struct {
	net.Conn
	once    sync.Once
	done    chan struct{}
	closing bool
}

func (c *nativeGuestTLSConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.done) })
	return err
}

type nativeGuestTLSListener struct {
	connection net.Conn
	done       <-chan struct{}
	accepted   bool
}

func (l *nativeGuestTLSListener) Accept() (net.Conn, error) {
	if !l.accepted {
		l.accepted = true
		return l.connection, nil
	}
	<-l.done
	return nil, net.ErrClosed
}
func (l *nativeGuestTLSListener) Close() error   { return l.connection.Close() }
func (l *nativeGuestTLSListener) Addr() net.Addr { return l.connection.LocalAddr() }

func (p *nativeGuestTLSProxy) fail(code int) {
	p.mu.Lock()
	if p.failure == 0 {
		p.failure = code
	}
	p.mu.Unlock()
	p.stop()
}
func (p *nativeGuestTLSProxy) failureCode() int { p.mu.Lock(); defer p.mu.Unlock(); return p.failure }
