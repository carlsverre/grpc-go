/*
 *
 * Copyright 2014, Google Inc.
 * All rights reserved.
 *
 * Redistribution and use in source and binary forms, with or without
 * modification, are permitted provided that the following conditions are
 * met:
 *
 *     * Redistributions of source code must retain the above copyright
 * notice, this list of conditions and the following disclaimer.
 *     * Redistributions in binary form must reproduce the above
 * copyright notice, this list of conditions and the following disclaimer
 * in the documentation and/or other materials provided with the
 * distribution.
 *     * Neither the name of Google Inc. nor the names of its
 * contributors may be used to endorse or promote products derived from
 * this software without specific prior written permission.
 *
 * THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
 * "AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
 * LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
 * A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
 * OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
 * SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
 * LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
 * DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
 * THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
 * (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
 * OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
 *
 */

package grpc

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/context"
	"golang.org/x/net/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/naming"
	"google.golang.org/grpc/transport"
)

var (
	// ErrNoTransportSecurity indicates that there is no transport security
	// being set for ClientConn. Users should either set one or explicitly
	// call WithInsecure DialOption to disable security.
	ErrNoTransportSecurity = errors.New("grpc: no transport security set (use grpc.WithInsecure() explicitly or set credentials)")
	// ErrCredentialsMisuse indicates that users want to transmit security information
	// (e.g., oauth2 token) which requires secure connection on an insecure
	// connection.
	ErrCredentialsMisuse = errors.New("grpc: the credentials require transport level security (use grpc.WithTransportAuthenticator() to set)")
	// ErrClientConnClosing indicates that the operation is illegal because
	// the ClientConn is closing.
	ErrClientConnClosing = errors.New("grpc: the client connection is closing")
	// ErrClientConnTimeout indicates that the connection could not be
	// established or re-established within the specified timeout.
	ErrClientConnTimeout = errors.New("grpc: timed out trying to connect")
	// ErrNetworkIP indicates that the connection is down due to some network I/O error.
	ErrNetworkIO = errors.New("grpc: failed with network I/O error")
	// ErrConnDrain indicates that the connection starts to be drained and does not accept any new RPCs.
	ErrConnDrain   = errors.New("grpc: the connection is drained")
	errConnClosing = errors.New("grpc: the addrConn is closing")
	// minimum time to give a connection to complete
	minConnectTimeout = 20 * time.Second
)

// dialOptions configure a Dial call. dialOptions are set by the DialOption
// values passed to Dial.
type dialOptions struct {
	codec    Codec
	cp       Compressor
	dc       Decompressor
	bs       backoffStrategy
	resolver naming.Resolver
	balancer Balancer
	block    bool
	insecure bool
	copts    transport.ConnectOptions
}

// DialOption configures how we set up the connection.
type DialOption func(*dialOptions)

// WithCodec returns a DialOption which sets a codec for message marshaling and unmarshaling.
func WithCodec(c Codec) DialOption {
	return func(o *dialOptions) {
		o.codec = c
	}
}

// WithCompressor returns a DialOption which sets a CompressorGenerator for generating message
// compressor.
func WithCompressor(cp Compressor) DialOption {
	return func(o *dialOptions) {
		o.cp = cp
	}
}

// WithDecompressor returns a DialOption which sets a DecompressorGenerator for generating
// message decompressor.
func WithDecompressor(dc Decompressor) DialOption {
	return func(o *dialOptions) {
		o.dc = dc
	}
}

// WithNameResolver returns a DialOption which sets a name resolver for service discovery.
func WithNameResolver(r naming.Resolver) DialOption {
	return func(o *dialOptions) {
		o.resolver = r
	}
}

// WithBalancer returns a DialOption which sets a load balancer.
func WithBalancer(b Balancer) DialOption {
	return func(o *dialOptions) {
		o.balancer = b
	}
}

// WithBackoffMaxDelay configures the dialer to use the provided maximum delay
// when backing off after failed connection attempts.
func WithBackoffMaxDelay(md time.Duration) DialOption {
	return WithBackoffConfig(BackoffConfig{MaxDelay: md})
}

// WithBackoffConfig configures the dialer to use the provided backoff
// parameters after connection failures.
//
// Use WithBackoffMaxDelay until more parameters on BackoffConfig are opened up
// for use.
func WithBackoffConfig(b BackoffConfig) DialOption {
	// Set defaults to ensure that provided BackoffConfig is valid and
	// unexported fields get default values.
	setDefaults(&b)
	return withBackoff(b)
}

// withBackoff sets the backoff strategy used for retries after a
// failed connection attempt.
//
// This can be exported if arbitrary backoff strategies are allowed by GRPC.
func withBackoff(bs backoffStrategy) DialOption {
	return func(o *dialOptions) {
		o.bs = bs
	}
}

// WithBlock returns a DialOption which makes caller of Dial blocks until the underlying
// connection is up. Without this, Dial returns immediately and connecting the server
// happens in background.
func WithBlock() DialOption {
	return func(o *dialOptions) {
		o.block = true
	}
}

// WithInsecure returns a DialOption which disables transport security for this ClientConn.
// Note that transport security is required unless WithInsecure is set.
func WithInsecure() DialOption {
	return func(o *dialOptions) {
		o.insecure = true
	}
}

// WithTransportCredentials returns a DialOption which configures a
// connection level security credentials (e.g., TLS/SSL).
func WithTransportCredentials(creds credentials.TransportAuthenticator) DialOption {
	return func(o *dialOptions) {
		o.copts.AuthOptions = append(o.copts.AuthOptions, creds)
	}
}

// WithPerRPCCredentials returns a DialOption which sets
// credentials which will place auth state on each outbound RPC.
func WithPerRPCCredentials(creds credentials.Credentials) DialOption {
	return func(o *dialOptions) {
		o.copts.AuthOptions = append(o.copts.AuthOptions, creds)
	}
}

// WithTimeout returns a DialOption that configures a timeout for dialing a client connection.
func WithTimeout(d time.Duration) DialOption {
	return func(o *dialOptions) {
		o.copts.Timeout = d
	}
}

// WithDialer returns a DialOption that specifies a function to use for dialing network addresses.
func WithDialer(f func(addr string, timeout time.Duration) (net.Conn, error)) DialOption {
	return func(o *dialOptions) {
		o.copts.Dialer = f
	}
}

// WithUserAgent returns a DialOption that specifies a user agent string for all the RPCs.
func WithUserAgent(s string) DialOption {
	return func(o *dialOptions) {
		o.copts.UserAgent = s
	}
}

// Dial creates a client connection the given target.
func Dial(target string, opts ...DialOption) (*ClientConn, error) {
	cc := &ClientConn{
		target: target,
		conns:  make(map[Address]*addrConn),
	}
	for _, opt := range opts {
		opt(&cc.dopts)
	}
	if cc.dopts.codec == nil {
		// Set the default codec.
		cc.dopts.codec = protoCodec{}
	}

	if cc.dopts.bs == nil {
		cc.dopts.bs = DefaultBackoffConfig
	}

	cc.balancer = cc.dopts.balancer
	if cc.balancer == nil {
		cc.balancer = RoundRobin()
	}

	if cc.dopts.resolver == nil {
		addr := Address{
			Addr: cc.target,
		}
		if err := cc.newAddrConn(addr); err != nil {
			return nil, err
		}
	} else {
		w, err := cc.dopts.resolver.Resolve(cc.target)
		if err != nil {
			return nil, err
		}
		cc.watcher = w
		// Get the initial name resolution and dial the first connection.
		if err := cc.watchAddrUpdates(); err != nil {
			return nil, err
		}
		// Start a goroutine to watch for the future name resolution changes.
		go func() {
			for {
				if err := cc.watchAddrUpdates(); err != nil {
					return
				}
			}
		}()
	}

	colonPos := strings.LastIndex(target, ":")
	if colonPos == -1 {
		colonPos = len(target)
	}
	cc.authority = target[:colonPos]
	return cc, nil
}

// ConnectivityState indicates the state of a client connection.
type ConnectivityState int

const (
	// Idle indicates the ClientConn is idle.
	Idle ConnectivityState = iota
	// Connecting indicates the ClienConn is connecting.
	Connecting
	// Ready indicates the ClientConn is ready for work.
	Ready
	// TransientFailure indicates the ClientConn has seen a failure but expects to recover.
	TransientFailure
	// Shutdown indicates the ClientConn has started shutting down.
	Shutdown
)

func (s ConnectivityState) String() string {
	switch s {
	case Idle:
		return "IDLE"
	case Connecting:
		return "CONNECTING"
	case Ready:
		return "READY"
	case TransientFailure:
		return "TRANSIENT_FAILURE"
	case Shutdown:
		return "SHUTDOWN"
	default:
		panic(fmt.Sprintf("unknown connectivity state: %d", s))
	}
}

// ClientConn represents a client connection to an RPC service.
type ClientConn struct {
	target    string
	watcher   naming.Watcher
	balancer  Balancer
	authority string
	dopts     dialOptions

	mu    sync.RWMutex
	conns map[Address]*addrConn
}

func (cc *ClientConn) watchAddrUpdates() error {
	updates, err := cc.watcher.Next()
	if err != nil {
		return err
	}
	for _, update := range updates {
		switch update.Op {
		case naming.Add:
			cc.mu.RLock()
			addr := Address{
				Addr:     update.Addr,
				Metadata: update.Metadata,
			}
			if _, ok := cc.conns[addr]; ok {
				cc.mu.RUnlock()
				grpclog.Println("grpc: The name resolver wanted to add an existing address: ", addr)
				continue
			}
			cc.mu.RUnlock()
			if err := cc.newAddrConn(addr); err != nil {
				return err
			}
		case naming.Delete:
			cc.mu.RLock()
			addr := Address{
				Addr:     update.Addr,
				Metadata: update.Metadata,
			}
			ac, ok := cc.conns[addr]
			if !ok {
				cc.mu.RUnlock()
				grpclog.Println("grpc: The name resolver wanted to delete a non-exist address: ", addr)
				continue
			}
			cc.mu.RUnlock()
			ac.tearDown(ErrConnDrain)
		default:
			grpclog.Println("Unknown update.Op ", update.Op)
		}
	}
	return nil
}

func (cc *ClientConn) newAddrConn(addr Address) error {
	ac := &addrConn{
		cc:           cc,
		addr:         addr,
		dopts:        cc.dopts,
		shutdownChan: make(chan struct{}),
	}
	if EnableTracing {
		ac.events = trace.NewEventLog("grpc.ClientConn", ac.addr.Addr)
	}
	if !ac.dopts.insecure {
		var ok bool
		for _, cd := range ac.dopts.copts.AuthOptions {
			if _, ok = cd.(credentials.TransportAuthenticator); ok {
				break
			}
		}
		if !ok {
			return ErrNoTransportSecurity
		}
	} else {
		for _, cd := range ac.dopts.copts.AuthOptions {
			if cd.RequireTransportSecurity() {
				return ErrCredentialsMisuse
			}
		}
	}
	ac.cc.mu.Lock()
	if ac.cc.conns == nil {
		ac.cc.mu.Unlock()
		return ErrClientConnClosing
	}
	ac.cc.conns[ac.addr] = ac
	ac.cc.mu.Unlock()

	ac.stateCV = sync.NewCond(&ac.mu)
	if ac.dopts.block {
		if err := ac.resetTransport(false); err != nil {
			ac.tearDown(err)
			return err
		}
		// Start to monitor the error status of transport.
		go ac.transportMonitor()
	} else {
		// Start a goroutine connecting to the server asynchronously.
		go func() {
			if err := ac.resetTransport(false); err != nil {
				grpclog.Printf("Failed to dial %s: %v; please retry.", ac.addr.Addr, err)
				ac.tearDown(err)
				return
			}
			ac.transportMonitor()
		}()
	}
	return nil
}

func (cc *ClientConn) getTransport(ctx context.Context) (transport.ClientTransport, func(), error) {
	addr, put, err := cc.balancer.Get(ctx)
	if err != nil {
		return nil, nil, err
	}
	cc.mu.RLock()
	if cc.conns == nil {
		cc.mu.RUnlock()
		return nil, nil, ErrClientConnClosing
	}
	ac, ok := cc.conns[addr]
	cc.mu.RUnlock()
	if !ok {
		put()
		return nil, nil, transport.StreamErrorf(codes.Internal, "grpc: failed to find the transport to send the rpc")
	}
	t, err := ac.wait(ctx)
	if err != nil {
		put()
		return nil, nil, err
	}
	return t, put, nil
}

// Close tears down the ClientConn and all underlying connections.
func (cc *ClientConn) Close() error {
	cc.mu.Lock()
	if cc.conns == nil {
		cc.mu.Unlock()
		return ErrClientConnClosing
	}
	conns := cc.conns
	cc.conns = nil
	cc.mu.Unlock()
	cc.balancer.Close()
	if cc.watcher != nil {
		cc.watcher.Close()
	}
	for _, ac := range conns {
		ac.tearDown(ErrClientConnClosing)
	}
	return nil
}

// addrConn is a network connection to a given address.
type addrConn struct {
	cc           *ClientConn
	addr         Address
	dopts        dialOptions
	shutdownChan chan struct{}
	events       trace.EventLog

	mu      sync.Mutex
	state   ConnectivityState
	stateCV *sync.Cond
	down    func(error) // the handler called when a connection is down.
	// ready is closed and becomes nil when a new transport is up or failed
	// due to timeout.
	ready     chan struct{}
	transport transport.ClientTransport
}

// printf records an event in ac's event log, unless ac has been closed.
// REQUIRES ac.mu is held.
func (ac *addrConn) printf(format string, a ...interface{}) {
	if ac.events != nil {
		ac.events.Printf(format, a...)
	}
}

// errorf records an error in ac's event log, unless ac has been closed.
// REQUIRES ac.mu is held.
func (ac *addrConn) errorf(format string, a ...interface{}) {
	if ac.events != nil {
		ac.events.Errorf(format, a...)
	}
}

// getState returns the connectivity state of the Conn
func (ac *addrConn) getState() ConnectivityState {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	return ac.state
}

// waitForStateChange blocks until the state changes to something other than the sourceState.
func (ac *addrConn) waitForStateChange(ctx context.Context, sourceState ConnectivityState) (ConnectivityState, error) {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	if sourceState != ac.state {
		return ac.state, nil
	}
	done := make(chan struct{})
	var err error
	go func() {
		select {
		case <-ctx.Done():
			ac.mu.Lock()
			err = ctx.Err()
			ac.stateCV.Broadcast()
			ac.mu.Unlock()
		case <-done:
		}
	}()
	defer close(done)
	for sourceState == ac.state {
		ac.stateCV.Wait()
		if err != nil {
			return ac.state, err
		}
	}
	return ac.state, nil
}

func (ac *addrConn) resetTransport(closeTransport bool) error {
	/*
		ac.cc.mu.Lock()
		if ac.cc.conns == nil {
			ac.cc.mu.Unlock()
			return ErrClientConnClosing
		}
		ac.cc.conns[ac.addr] = ac
		ac.cc.mu.Unlock()
	*/
	var retries int
	start := time.Now()
	for {
		ac.mu.Lock()
		ac.printf("connecting")
		if ac.state == Shutdown {
			// ac.tearDown(...) has been invoked.
			ac.mu.Unlock()
			return errConnClosing
		}
		if ac.down != nil {
			ac.down(ErrNetworkIO)
			ac.down = nil
		}
		ac.state = Connecting
		ac.stateCV.Broadcast()
		t := ac.transport
		ac.mu.Unlock()
		if closeTransport && t != nil {
			t.Close()
		}
		// Adjust timeout for the current try.
		copts := ac.dopts.copts
		if copts.Timeout < 0 {
			ac.tearDown(ErrClientConnTimeout)
			return ErrClientConnTimeout
		}
		if copts.Timeout > 0 {
			copts.Timeout -= time.Since(start)
			if copts.Timeout <= 0 {
				ac.tearDown(ErrClientConnTimeout)
				return ErrClientConnTimeout
			}
		}
		sleepTime := ac.dopts.bs.backoff(retries)
		timeout := sleepTime
		if timeout < minConnectTimeout {
			timeout = minConnectTimeout
		}
		if copts.Timeout == 0 || copts.Timeout > timeout {
			copts.Timeout = timeout
		}
		connectTime := time.Now()
		newTransport, err := transport.NewClientTransport(ac.addr.Addr, &copts)
		if err != nil {
			ac.mu.Lock()
			if ac.state == Shutdown {
				// ac.tearDown(...) has been invoked.
				ac.mu.Unlock()
				return errConnClosing
			}
			ac.errorf("transient failure: %v", err)
			ac.state = TransientFailure
			ac.stateCV.Broadcast()
			if ac.ready != nil {
				close(ac.ready)
				ac.ready = nil
			}
			ac.mu.Unlock()
			sleepTime -= time.Since(connectTime)
			if sleepTime < 0 {
				sleepTime = 0
			}
			// Fail early before falling into sleep.
			if ac.dopts.copts.Timeout > 0 && ac.dopts.copts.Timeout < sleepTime+time.Since(start) {
				ac.mu.Lock()
				ac.errorf("connection timeout")
				ac.mu.Unlock()
				ac.tearDown(ErrClientConnTimeout)
				return ErrClientConnTimeout
			}
			closeTransport = false
			select {
			case <-time.After(sleepTime):
			case <-ac.shutdownChan:
			}
			retries++
			grpclog.Printf("grpc: addrConn.resetTransport failed to create client transport: %v; Reconnecting to %q", err, ac.addr)
			continue
		}
		ac.mu.Lock()
		ac.printf("ready")
		if ac.state == Shutdown {
			// ac.tearDown(...) has been invoked.
			ac.mu.Unlock()
			newTransport.Close()
			return errConnClosing
		}
		ac.state = Ready
		ac.stateCV.Broadcast()
		ac.transport = newTransport
		ac.down = ac.cc.balancer.Up(ac.addr)
		if ac.ready != nil {
			close(ac.ready)
			ac.ready = nil
		}
		ac.mu.Unlock()
		return nil
	}
}

// Run in a goroutine to track the error in transport and create the
// new transport if an error happens. It returns when the channel is closing.
func (ac *addrConn) transportMonitor() {
	for {
		ac.mu.Lock()
		t := ac.transport
		ac.mu.Unlock()
		select {
		// shutdownChan is needed to detect the teardown when
		// the addrConn is idle (i.e., no RPC in flight).
		case <-ac.shutdownChan:
			return
		case <-t.Error():
			ac.mu.Lock()
			if ac.state == Shutdown {
				// ac.tearDown(...) has been invoked.
				ac.mu.Unlock()
				return
			}
			ac.state = TransientFailure
			ac.stateCV.Broadcast()
			ac.mu.Unlock()
			if err := ac.resetTransport(true); err != nil {
				ac.mu.Lock()
				ac.printf("transport exiting: %v", err)
				ac.mu.Unlock()
				grpclog.Printf("grpc: addrConn.transportMonitor exits due to: %v", err)
				return
			}
		}
	}
}

// wait blocks until i) the new transport is up or ii) ctx is done or iii) ac is closed.
func (ac *addrConn) wait(ctx context.Context) (transport.ClientTransport, error) {
	for {
		ac.mu.Lock()
		switch {
		case ac.state == Shutdown:
			ac.mu.Unlock()
			return nil, errConnClosing
		case ac.state == Ready:
			ct := ac.transport
			ac.mu.Unlock()
			return ct, nil
		default:
			ready := ac.ready
			if ready == nil {
				ready = make(chan struct{})
				ac.ready = ready
			}
			ac.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, transport.ContextErr(ctx.Err())
			// Wait until the new transport is ready or failed.
			case <-ready:
			}
		}
	}
}

// tearDown starts to tear down the addrConn.
// TODO(zhaoq): Make this synchronous to avoid unbounded memory consumption in
// some edge cases (e.g., the caller opens and closes many addrConn's in a
// tight loop.
func (ac *addrConn) tearDown(err error) {
	ac.mu.Lock()
	defer func() {
		ac.mu.Unlock()
		ac.cc.mu.Lock()
		if ac.cc.conns != nil {
			delete(ac.cc.conns, ac.addr)
		}
		ac.cc.mu.Unlock()
	}()
	if ac.down != nil {
		ac.down(err)
		ac.down = nil
	}
	if ac.state == Shutdown {
		return
	}
	ac.state = Shutdown
	ac.stateCV.Broadcast()
	if ac.events != nil {
		ac.events.Finish()
		ac.events = nil
	}
	if ac.ready != nil {
		close(ac.ready)
		ac.ready = nil
	}
	if ac.transport != nil {
		if err == ErrConnDrain {
			ac.transport.GracefulClose()
		} else {
			ac.transport.Close()
		}
	}
	if ac.shutdownChan != nil {
		close(ac.shutdownChan)
	}
	return
}
