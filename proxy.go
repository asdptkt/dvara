package dvara

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/facebookgo/stats"
	corelog "github.com/intercom/gocore/log"
)

const headerLen = 16

var (
	errZeroMaxConnections          = errors.New("dvara: MaxConnections cannot be 0")
	errZeroMaxPerClientConnections = errors.New("dvara: MaxPerClientConnections cannot be 0")
	errNormalClose                 = errors.New("dvara: normal close")
	errClientReadTimeout           = errors.New("dvara: client read timeout")

	timeInPast = time.Now()
)

// Proxy sends stuff from clients to mongo servers.
type Proxy struct {
	ReplicaSet     *ReplicaSet
	ClientListener net.Listener // Listener for incoming client connections
	Username       string       // Mongo user, if mongo uses auth
	Password       string       // Mongo password, if mongo uses auth
	ProxyAddr      string       // Address for incoming client connections
	MongoAddr      string       // Address for destination Mongo server

	wg                      sync.WaitGroup
	closed                  chan struct{}
	serverPool              Pool
	stats                   stats.Client
	maxPerClientConnections *maxPerClientConnections
}

// String representation for debugging.
func (p *Proxy) String() string {
	return fmt.Sprintf("proxy %s => mongo %s", p.ProxyAddr, p.MongoAddr)
}

// Start the proxy.
func (p *Proxy) Start() error {
	if p.ReplicaSet.MaxConnections == 0 {
		return errZeroMaxConnections
	}
	if p.ReplicaSet.MaxPerClientConnections == 0 {
		return errZeroMaxPerClientConnections
	}

	p.closed = make(chan struct{})
	p.maxPerClientConnections = newMaxPerClientConnections(p.ReplicaSet.MaxPerClientConnections)
	p.serverPool = Pool{
		New:               p.newServerConn,
		CloseErrorHandler: p.serverCloseErrorHandler,
		Max:               p.ReplicaSet.MaxConnections,
		MinIdle:           p.ReplicaSet.MinIdleConnections,
		IdleTimeout:       p.ReplicaSet.ServerIdleTimeout,
		ClosePoolSize:     p.ReplicaSet.ServerClosePoolSize,
	}

	// plug stats if we can
	if p.ReplicaSet.Stats != nil {
		p.serverPool.Stats = stats.PrefixClient(
			[]string{"mongoproxy.server.pool."},
			p.ReplicaSet.Stats,
		)
		p.stats = stats.PrefixClient(
			[]string{"mongoproxy."},
			p.ReplicaSet.Stats,
		)
	}

	go p.clientAcceptLoop()

	return nil
}

// Stop the proxy.
func (p *Proxy) Stop() error {
	return p.stop(false)
}

func (p *Proxy) stop(hard bool) error {
	if err := p.ClientListener.Close(); err != nil {
		return err
	}
	close(p.closed)
	if !hard {
		p.wg.Wait()
	}
	p.serverPool.Close()
	return nil
}

func (p *Proxy) AuthConn(conn net.Conn) error {
	socket := &mongoSocket{
		conn: conn,
	}
	err := socket.Login(Credential{Username: p.Username, Password: p.Password, Source: "admin"})
	if err != nil {
		return err
	}
	return nil
}

// Open up a new connection to the server. Retry 7 times, doubling the sleep
// each time. This means we'll a total of 12.75 seconds with the last wait
// being 6.4 seconds.
func (p *Proxy) newServerConn() (io.Closer, error) {
	retrySleep := 50 * time.Millisecond
	for retryCount := 7; retryCount > 0; retryCount-- {
		c, err := net.DialTimeout("tcp", p.MongoAddr, time.Second)
		if err == nil {
			if len(p.Username) == 0 {
				return c, nil
			}
			err = p.AuthConn(c)
			if err == nil {
				return c, nil
			}
		}
		corelog.LogError("error", err)

		time.Sleep(retrySleep)
		retrySleep = retrySleep * 2
	}
	return nil, fmt.Errorf("could not connect to %s", p.MongoAddr)
}

// getServerConn gets a server connection from the pool.
func (p *Proxy) getServerConn() (net.Conn, error) {
	c, err := p.serverPool.Acquire()
	if err != nil {
		return nil, err
	}
	return c.(net.Conn), nil
}

func (p *Proxy) serverCloseErrorHandler(err error) {
	corelog.LogError("error", err)
}

// proxyMessage proxies a message, possibly it's response, and possibly a
// follow up call.
func (p *Proxy) proxyMessage(
	h *messageHeader,
	client net.Conn,
	server net.Conn,
	lastError *LastError,
) error {
	deadline := time.Now().Add(p.ReplicaSet.MessageTimeout)
	server.SetDeadline(deadline)
	client.SetDeadline(deadline)

	// OpQuery may need to be transformed and need special handling in order to
	// make the proxy transparent.
	if h.OpCode == OpQuery {
		return p.ReplicaSet.ProxyQuery.Proxy(h, client, server, lastError)
	}

	// Anything besides a getlasterror call (which requires an OpQuery) resets
	// the lastError.
	if lastError.Exists() {
		corelog.LogInfoMessage("reset getLastError cache")
		lastError.Reset()
	}

	// For other Ops we proxy the header & raw body over.
	if err := h.WriteTo(server); err != nil {
		corelog.LogError("error", err)
		return err
	}

	if _, err := io.CopyN(server, client, int64(h.MessageLength-headerLen)); err != nil {
		corelog.LogError("error", err)
		return err
	}

	// For Ops with responses we proxy the raw response message over.
	if h.OpCode.HasResponse() {
		if err := copyMessage(client, server); err != nil {
			corelog.LogError("error", err)
			return err
		}
	}

	return nil
}

// clientAcceptLoop accepts new clients and creates a clientServeLoop for each
// new client that connects to the proxy.
func (p *Proxy) clientAcceptLoop() {
	for {
		p.wg.Add(1)
		c, err := p.ClientListener.Accept()
		if err != nil {
			p.wg.Done()
			if strings.Contains(err.Error(), "use of closed network connection") {
				break
			}
			corelog.LogError("error", err)
			continue
		}
		go p.clientServeLoop(c)
	}
}

// clientServeLoop loops on a single client connected to the proxy and
// dispatches its requests.
func (p *Proxy) clientServeLoop(c net.Conn) {
	remoteIP := c.RemoteAddr().(*net.TCPAddr).IP.String()

	// enforce per-client max connection limit
	if p.maxPerClientConnections.inc(remoteIP) {
		c.Close()
		stats.BumpSum(p.stats, "client.rejected.max.connections", 1)
		corelog.LogErrorMessage(fmt.Sprintf("rejecting client connection due to max connections limit: %s", remoteIP))
		return
	}

	// turn on TCP keep-alive and set it to the recommended period of 2 minutes
	// http://docs.mongodb.org/manual/faq/diagnostics/#faq-keepalive
	if conn, ok := c.(*net.TCPConn); ok {
		conn.SetKeepAlivePeriod(2 * time.Minute)
		conn.SetKeepAlive(true)
	}

	c = teeIf(fmt.Sprintf("client %s <=> %s", c.RemoteAddr(), p), c)
	stats.BumpSum(p.stats, "client.connected", 1)
	defer func() {
		p.wg.Done()
		if err := c.Close(); err != nil {
			corelog.LogError("error", err)
		}
		p.maxPerClientConnections.dec(remoteIP)
	}()

	var lastError LastError
	for {
		h, err := p.idleClientReadHeader(c)
		if err != nil {
			if err != errNormalClose {
				corelog.LogError("error", err)
			}
			return
		}

		mpt := stats.BumpTime(p.stats, "message.proxy.time")
		serverConn, err := p.getServerConn()
		if err != nil {
			if err != errNormalClose {
				corelog.LogError("error", err)
			}
			return
		}

		scht := stats.BumpTime(p.stats, "server.conn.held.time")
		for {
			err := p.proxyMessage(h, c, serverConn, &lastError)
			if err != nil {
				p.serverPool.Discard(serverConn)
				corelog.LogErrorMessage(fmt.Sprintf("Proxy message failed %s ", err))
				stats.BumpSum(p.stats, "message.proxy.error", 1)
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					stats.BumpSum(p.stats, "message.proxy.timeout", 1)
				}
				return
			}

			// One message was proxied, stop it's timer.
			mpt.End()

			if !h.OpCode.IsMutation() {
				break
			}

			// If the operation we just performed was a mutation, we always make the
			// follow up request on the same server because it's possibly a getLastErr
			// call which expects this behavior.

			stats.BumpSum(p.stats, "message.with.mutation", 1)
			h, err = p.gleClientReadHeader(c)
			if err != nil {
				// Client did not make _any_ query within the GetLastErrorTimeout.
				// Return the server to the pool and wait go back to outer loop.
				if err == errClientReadTimeout {
					break
				}
				// Prevent noise of normal client disconnects, but log if anything else.
				if err != errNormalClose {
					corelog.LogError("error", err)
				}
				// We need to return our server to the pool (it's still good as far
				// as we know).
				p.serverPool.Release(serverConn)
				return
			}

			// Successfully read message when waiting for the getLastError call.
			mpt = stats.BumpTime(p.stats, "message.proxy.time")
		}
		p.serverPool.Release(serverConn)
		scht.End()
		stats.BumpSum(p.stats, "message.proxy.success", 1)
	}
}

// We wait for upto ClientIdleTimeout in MessageTimeout increments and keep
// checking if we're waiting to be closed. This ensures that at worse we
// wait for MessageTimeout when closing even when we're idling.
func (p *Proxy) idleClientReadHeader(c net.Conn) (*messageHeader, error) {
	h, err := p.clientReadHeader(c, p.ReplicaSet.ClientIdleTimeout)
	if err == errClientReadTimeout {
		stats.BumpSum(p.stats, "client.idle.timeout", 1)
	}
	return h, err
}

func (p *Proxy) gleClientReadHeader(c net.Conn) (*messageHeader, error) {
	h, err := p.clientReadHeader(c, p.ReplicaSet.GetLastErrorTimeout)
	if err == errClientReadTimeout {
		stats.BumpSum(p.stats, "client.gle.timeout", 1)
	}
	return h, err
}

func (p *Proxy) clientReadHeader(c net.Conn, timeout time.Duration) (*messageHeader, error) {
	type headerError struct {
		header *messageHeader
		error  error
	}
	resChan := make(chan headerError)

	c.SetReadDeadline(time.Now().Add(timeout))
	go func() {
		h, err := readHeader(c)
		resChan <- headerError{header: h, error: err}
	}()

	closed := false
	var response headerError

	select {
	case response = <-resChan:
		// all good
	case <-p.closed:
		closed = true
		c.SetReadDeadline(timeInPast)
		response = <-resChan
	}

	// Successfully read a header.
	if response.error == nil {
		return response.header, nil
	}

	// Client side disconnected.
	if response.error == io.EOF {
		return nil, errNormalClose
	}

	// We hit our ReadDeadline.
	if ne, ok := response.error.(net.Error); ok && ne.Timeout() {
		if closed {
			return nil, errNormalClose
		}
		return nil, errClientReadTimeout
	}

	// Some other unknown error.
	stats.BumpSum(p.stats, "client.error.disconnect", 1)
	corelog.LogError("error", response.error)
	return nil, response.error
}

var teeIfEnable = os.Getenv("MONGOPROXY_TEE") == "1"

type teeConn struct {
	context string
	net.Conn
}

func (t teeConn) Read(b []byte) (int, error) {
	n, err := t.Conn.Read(b)
	if n > 0 {
		fmt.Fprintf(os.Stdout, "READ %s: %s %v\n", t.context, b[0:n], b[0:n])
	}
	return n, err
}

func (t teeConn) Write(b []byte) (int, error) {
	n, err := t.Conn.Write(b)
	if n > 0 {
		fmt.Fprintf(os.Stdout, "WRIT %s: %s %v\n", t.context, b[0:n], b[0:n])
	}
	return n, err
}

func teeIf(context string, c net.Conn) net.Conn {
	if teeIfEnable {
		return teeConn{
			context: context,
			Conn:    c,
		}
	}
	return c
}

type maxPerClientConnections struct {
	max    uint
	counts map[string]uint
	mutex  sync.Mutex
}

func newMaxPerClientConnections(max uint) *maxPerClientConnections {
	return &maxPerClientConnections{
		max:    max,
		counts: make(map[string]uint),
	}
}

func (m *maxPerClientConnections) inc(remoteIP string) bool {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	current := m.counts[remoteIP]
	if current >= m.max {
		return true
	}
	m.counts[remoteIP] = current + 1
	return false
}

func (m *maxPerClientConnections) dec(remoteIP string) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	current := m.counts[remoteIP]

	// delete rather than having entries with 0 connections
	if current == 1 {
		delete(m.counts, remoteIP)
	} else {
		m.counts[remoteIP] = current - 1
	}
}
