package uspclient

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/elastic/go-grok"
	"github.com/gorilla/websocket"
	lc "github.com/refractionPOINT/go-limacharlie/limacharlie"
	"github.com/refractionPOINT/go-uspclient/protocol"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/google/uuid"
)

//go:embed certs/google_wr3.pem
var certsGoogleWr3 []byte

type Client struct {
	options ClientOptions
	org     *lc.Organization
	wssURL  string

	conn           *websocket.Conn
	ab             *AckBuffer
	wg             sync.WaitGroup
	connMutex      sync.Mutex
	isStart        *Event
	isStop         *Event
	isReconnecting uint32
	backoffTime    uint64

	lastError  error
	errorMutex sync.Mutex

	instanceID string

	// Set by Close(). A reconnect loop must never outlive Close(), otherwise
	// it keeps dialing forever and resurrects a connection nobody owns.
	isClosing   atomic.Bool
	closeCtx    context.Context
	closeCancel context.CancelFunc
}

// reconnectRetryDelay is how long a background reconnect waits between failed
// attempts, in nanoseconds. Atomic so tests can shorten it while a reconnect
// goroutine from an earlier test is still winding down.
var reconnectRetryDelay atomic.Int64

func init() { reconnectRetryDelay.Store(int64(5 * time.Second)) }

var errClientClosing = errors.New("client is closing")

type Identity struct {
	Oid             string `json:"oid" yaml:"oid" msgpack:"oid"`
	InstallationKey string `json:"installation_key" yaml:"installation_key" msgpack:"installation_key"`
}

type ProxyOptions struct {
	URL              string        `json:"url,omitempty" yaml:"url,omitempty" msgpack:"url,omitempty"`
	Username         string        `json:"username,omitempty" yaml:"username,omitempty" msgpack:"username,omitempty"`
	Password         string        `json:"password,omitempty" yaml:"password,omitempty" msgpack:"password,omitempty"`
	HandshakeTimeout time.Duration `json:"handshake_timeout,omitempty" yaml:"handshake_timeout,omitempty" msgpack:"handshake_timeout,omitempty"`
	ConnectTimeout   time.Duration `json:"connect_timeout,omitempty" yaml:"connect_timeout,omitempty" msgpack:"connect_timeout,omitempty"`
	ReadWriteTimeout time.Duration `json:"read_write_timeout,omitempty" yaml:"read_write_timeout,omitempty" msgpack:"read_write_timeout,omitempty"`
}

type ClientOptions struct {
	Identity      Identity                     `json:"identity" yaml:"identity" msgpack:"identity"`
	Hostname      string                       `json:"hostname,omitempty" yaml:"hostname,omitempty" msgpack:"hostname,omitempty"`
	Platform      string                       `json:"platform,omitempty" yaml:"platform,omitempty" msgpack:"platform,omitempty"`
	Architecture  string                       `json:"architecture,omitempty" yaml:"architecture,omitempty" msgpack:"architecture,omitempty"`
	Mapping       protocol.MappingDescriptor   `json:"mapping,omitempty" yaml:"mapping,omitempty" msgpack:"mapping,omitempty"`
	Mappings      []protocol.MappingDescriptor `json:"mappings,omitempty" yaml:"mappings,omitempty" msgpack:"mappings,omitempty"`
	Indexing      []protocol.IndexDescriptor   `json:"indexing,omitempty" yaml:"indexing,omitempty" msgpack:"indexing,omitempty"`
	BufferOptions AckBufferOptions             `json:"buffer_options,omitempty" yaml:"buffer_options,omitempty" msgpack:"buffer_options,omitempty"`
	IsCompressed  bool                         `json:"is_compressed,omitempty" yaml:"is_compressed,omitempty" msgpack:"is_compressed,omitempty"`

	SensorSeedKey string `json:"sensor_seed_key" yaml:"sensor_seed_key" msgpack:"sensor_seed_key"`

	DebugLog  func(string) `json:"-" yaml:"-" msgpack:"-"`
	OnError   func(error)  `json:"-" yaml:"-" msgpack:"-"`
	OnWarning func(string) `json:"-" yaml:"-" msgpack:"-"`

	// Auto-detect if not specified (preferred).
	DestURL string        `json:"dest_url,omitempty" yaml:"dest_url,omitempty" msgpack:"dest_url,omitempty"`
	GenURL  func() string `json:"-" yaml:"-" msgpack:"-"`

	// Simple flag to operate the client as a sink for testing.
	TestSinkMode bool `json:"-" yaml:"-" msgpack:"-"`

	// Proxy configuration for connecting to LimaCharlie cloud
	Proxy ProxyOptions `json:"proxy,omitempty" yaml:"proxy,omitempty" msgpack:"proxy,omitempty"`
}

func (o *ClientOptions) normalizeProxyConfig() {
	// Set default timeouts if not specified
	if o.Proxy.HandshakeTimeout == 0 {
		o.Proxy.HandshakeTimeout = 45 * time.Second
	}
	if o.Proxy.ConnectTimeout == 0 {
		o.Proxy.ConnectTimeout = 10 * time.Second
	}
	if o.Proxy.ReadWriteTimeout == 0 {
		o.Proxy.ReadWriteTimeout = 30 * time.Second
	}
}

func (o ClientOptions) Validate() error {
	if o.Identity.Oid == "" {
		return errors.New("missing oid")
	}
	if o.Identity.InstallationKey == "" {
		return errors.New("missing installation_key")
	}
	if o.Platform == "" {
		return errors.New("missing platform")
	}

	// Validate proxy configuration if provided
	if o.Proxy.URL != "" {
		parsedURL, err := url.Parse(o.Proxy.URL)
		if err != nil {
			return fmt.Errorf("invalid proxy URL: %v", err)
		}

		// Check if URL contains credentials
		if parsedURL.User != nil {
			return errors.New("proxy URL must not contain credentials - use proxy.username and proxy.password fields instead")
		}

		// Check authentication completeness
		if (o.Proxy.Username != "" && o.Proxy.Password == "") || (o.Proxy.Username == "" && o.Proxy.Password != "") {
			return errors.New("proxy authentication requires both username and password")
		}
	}

	for i, desc := range o.Indexing {
		if err := desc.Validate(); err != nil {
			return fmt.Errorf("index descriptor %d invalid: %v", i, err)
		}
	}
	if len(o.Mappings) != 0 {
		for i, m := range o.Mappings {
			if err := m.Validate(); err != nil {
				return fmt.Errorf("mapping descriptor %d invalid: %v", i, err)
			}
		}
	} else {
		if err := o.Mapping.Validate(); err != nil {
			return fmt.Errorf("mapping descriptor invalid: %v", err)
		}
	}

	return nil
}

// ErrorBufferFull is returned by Ship when there was no room in the buffer
// within the timeout.
var ErrorBufferFull = errors.New("buffer full")

// ErrorClosed is returned by Ship when the client is closed, or is closed while
// Ship is waiting for room. It is a clean shutdown, not a failure: retrying is
// pointless and it should not be reported as an error.
//
// Before this error existed, Ship returned ErrorBufferFull in that case. Code
// that retries on ErrorBufferFull no longer retries a closed client (good), but
// code that treats every other error as fatal now sees ErrorClosed there and
// should check for it first.
var ErrorClosed = errors.New("client closed")

func NewClient(ctx context.Context, o ClientOptions) (*Client, error) {
	// Normalize proxy configuration for backward compatibility
	o.normalizeProxyConfig()

	// Validate declarations even when callers construct clients without Validate.
	if !protocol.ValidSensorIdentityType(o.Mapping.SensorIdentityType) {
		return nil, errors.New("invalid mapping.sensor_identity_type")
	}
	for _, mapping := range o.Mappings {
		if !protocol.ValidSensorIdentityType(mapping.SensorIdentityType) {
			return nil, errors.New("invalid mappings.sensor_identity_type")
		}
	}

	// Validate mappings before anything else
	for i, m := range o.Mappings {
		if m.ParsingRE == "" && len(m.ParsingGrok) == 0 {
			return nil, fmt.Errorf("mappings[%d].parsing_re or parsing_grok not set, one is required for multiple mappings", i)
		}
		if m.ParsingRE != "" {
			if _, err := regexp.Compile(m.ParsingRE); err != nil {
				return nil, fmt.Errorf("invalid mappings[%d].parsing_re: %v", i, err)
			}
		}
		if len(m.ParsingGrok) > 0 {
			g := grok.New()
			if err := g.AddPatterns(m.ParsingGrok); err != nil {
				return nil, fmt.Errorf("invalid mappings[%d].parsing_grok: %v", i, err)
			}
		}
	}
	if o.Mapping.ParsingRE != "" {
		if _, err := regexp.Compile(o.Mapping.ParsingRE); err != nil {
			return nil, fmt.Errorf("invalid mapping.parsing_re: %v", err)
		}
	}
	if len(o.Mapping.ParsingGrok) > 0 {
		g := grok.New()
		if err := g.AddPatterns(o.Mapping.ParsingGrok); err != nil {
			return nil, fmt.Errorf("invalid mapping.parsing_grok: %v", err)
		}
	}

	if o.TestSinkMode {
		return &Client{
			options: o,
		}, nil
	}
	// Get an SDK instance so we can resolve the datacenter.
	org, err := lc.NewOrganizationFromClientOptions(lc.ClientOptions{
		OID: o.Identity.Oid,
	}, nil)
	if err != nil {
		return nil, err
	}

	// Use the URL provided or auto-detect.
	wssEndpoint := o.DestURL
	if wssEndpoint == "" {
		// Auto-detect the WSS URL for our datacenter. URL discovery uses the
		// caller's context so initialization remains bounded during API retries.
		urls, err := org.GetURLsWithContext(ctx)
		if err != nil {
			// Check if context was cancelled/timed out
			if ctx.Err() != nil {
				return nil, fmt.Errorf("context cancelled during URL resolution: %w", ctx.Err())
			}
			return nil, err
		}
		u, ok := urls["lc_wss"]
		if !ok {
			return nil, errors.New("site url for org not found")
		}
		wssEndpoint = fmt.Sprintf("wss://%s/usp", u)
	}

	if o.Hostname == "" {
		o.Hostname, _ = os.Hostname()
	}

	ab, err := NewAckBuffer(o.BufferOptions)
	if err != nil {
		return nil, err
	}
	c := &Client{
		options: o,
		org:     org,
		wssURL:  wssEndpoint,
		ab:      ab,
		isStop:  NewEvent(),
		isStart: NewEvent(),
	}
	c.instanceID = uuid.NewString()
	c.closeCtx, c.closeCancel = context.WithCancel(context.Background())
	if err := c.connect(ctx); err != nil {
		c.closeCancel()
		return nil, err
	}

	return c, nil
}

// BeginDrain stops accepting new messages and wakes blocked Ship calls without
// waiting for acknowledgements. Shutdown code can call it before joining producer
// goroutines, then call Drain to confirm accepted data and Close to free resources.
func (c *Client) BeginDrain() {
	if c.ab == nil {
		return
	}
	c.ab.Lock()
	c.ab.isRunning = false
	c.ab.signalLocked()
	c.ab.Unlock()
}

// Drain stops accepting messages and waits for every buffered message to be
// acknowledged by the proxy. A successful socket write alone is not confirmation.
// On timeout the error includes the remaining count and sequence range; Close
// returns those messages for recovery. The client must still be closed afterwards.
func (c *Client) Drain(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return c.DrainContext(ctx)
}

// DrainContext is Drain with cancellation. Contexts without a deadline are capped
// at one minute so an unavailable or throttling proxy cannot hang shutdown.
// Reconnects continue while draining and resend unacknowledged messages.
func (c *Client) DrainContext(ctx context.Context) error {
	if c.options.TestSinkMode {
		return nil
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Minute)
		defer cancel()
	}
	c.log("usp-client draining")
	return c.ab.drain(ctx)
}

// DrainError reports the messages still awaiting acknowledgement when Drain
// stops. Close returns their payloads. Unwrap supports errors.Is with the context
// cancellation/deadline error or ErrorClosed.
type DrainError struct {
	Unacked           int
	FirstSeq, LastSeq uint64
	Err               error
}

func (e *DrainError) Error() string {
	return fmt.Sprintf("draining: %d unacknowledged messages (sequences %d..%d): %v", e.Unacked, e.FirstSeq, e.LastSeq, e.Err)
}

func (e *DrainError) Unwrap() error { return e.Err }

func (c *Client) Close() ([]*protocol.DataMessage, error) {
	if c.options.TestSinkMode {
		return nil, nil
	}
	c.log("usp-client closing")
	// Stop any background reconnect loop first: it holds no lock while it
	// sleeps between attempts, so disconnect() below cannot see it.
	c.isClosing.Store(true)
	if c.closeCancel != nil {
		c.closeCancel()
	}
	c.ab.Close()
	err1 := c.disconnect()
	messages, err2 := c.ab.GetUnAcked()
	err := err1
	if err == nil {
		err = err2
	}
	c.setLastError(err)
	return messages, err
}

// createProxyDialer creates a custom dialer that uses HTTP CONNECT for proxy tunneling
func (c *Client) createProxyDialer() (*websocket.Dialer, error) {
	if c.options.Proxy.URL == "" {
		// No proxy configured, use default dialer
		return websocket.DefaultDialer, nil
	}

	proxyURL, err := url.Parse(c.options.Proxy.URL)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy URL: %v", err)
	}

	dialer := &websocket.Dialer{
		Proxy:            nil, // We handle proxy ourselves via NetDialContext
		HandshakeTimeout: c.options.Proxy.HandshakeTimeout,
		NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			// Connect to proxy with context
			var d net.Dialer
			d.Timeout = c.options.Proxy.ConnectTimeout
			proxyConn, err := d.DialContext(ctx, "tcp", proxyURL.Host)
			if err != nil {
				return nil, fmt.Errorf("failed to connect to proxy: %v", err)
			}

			// Ensure cleanup on error
			defer func() {
				if err != nil {
					proxyConn.Close()
				}
			}()

			// Set deadline for proxy operations
			if err := proxyConn.SetDeadline(time.Now().Add(c.options.Proxy.ReadWriteTimeout)); err != nil {
				return nil, fmt.Errorf("failed to set deadline: %v", err)
			}

			// Create HTTP CONNECT request using http.Request for better standards compliance
			connectReq := &http.Request{
				Method:     "CONNECT",
				URL:        &url.URL{Opaque: addr},
				Host:       addr,
				Header:     make(http.Header),
				Proto:      "HTTP/1.1",
				ProtoMajor: 1,
				ProtoMinor: 1,
			}

			// Add proxy authentication if provided
			if c.options.Proxy.Username != "" && c.options.Proxy.Password != "" {
				connectReq.Header.Set("Proxy-Authorization", connectReq.Header.Get("Authorization"))
				connectReq.SetBasicAuth(c.options.Proxy.Username, c.options.Proxy.Password)
				// Move Authorization header to Proxy-Authorization
				if auth := connectReq.Header.Get("Authorization"); auth != "" {
					connectReq.Header.Set("Proxy-Authorization", auth)
					connectReq.Header.Del("Authorization")
				}
			}

			// Write the CONNECT request
			if err = connectReq.Write(proxyConn); err != nil {
				return nil, fmt.Errorf("failed to send CONNECT request: %v", err)
			}

			// Read response
			reader := bufio.NewReader(proxyConn)
			resp, err := http.ReadResponse(reader, connectReq)
			if err != nil {
				return nil, fmt.Errorf("failed to read CONNECT response: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != 200 {
				err = fmt.Errorf("proxy returned status %d", resp.StatusCode)
				return nil, err
			}

			// Clear deadline after successful CONNECT
			if err := proxyConn.SetDeadline(time.Time{}); err != nil {
				return nil, fmt.Errorf("failed to clear deadline: %v", err)
			}

			// Connection established, return the tunnel
			return proxyConn, nil
		},
	}

	// If the target is HTTPS/WSS, we need to handle TLS
	// Extract hostname from the WebSocket URL for TLS SNI
	targetURL, err := url.Parse(c.wssURL)
	if err == nil && targetURL.Host != "" {
		hostname := targetURL.Hostname()

		// Create a custom certificate pool with the trusted intermediate certificate
		certPool, err := x509.SystemCertPool()
		if err != nil {
			// If system cert pool is not available, create a new one
			certPool = x509.NewCertPool()
		}

		// Add the trusted intermediate certificate
		if !certPool.AppendCertsFromPEM(certsGoogleWr3) {
			return nil, fmt.Errorf("failed to add google WR3 intermediate certificate")
		}

		dialer.TLSClientConfig = &tls.Config{
			ServerName: hostname,
			RootCAs:    certPool,
		}
	}

	return dialer, nil
}

func (c *Client) connect(ctx context.Context) error {
	c.log("usp-client connecting")
	c.connMutex.Lock()
	defer c.connMutex.Unlock()
	if c.isClosing.Load() {
		return errClientClosing
	}
	c.isStart.Clear()

	// Connect the websocket.
	url := c.wssURL
	if c.options.GenURL != nil {
		url = c.options.GenURL()
	}

	// Create dialer with proxy support if configured
	dialer, err := c.createProxyDialer()
	if err != nil {
		c.onError(fmt.Errorf("createProxyDialer(): %v", err))
		c.setLastError(err)
		return err
	}

	// Try to connect with retry logic for proxy connections
	var conn *websocket.Conn
	maxRetries := 1
	if c.options.Proxy.URL != "" {
		maxRetries = 3 // 3 retries for proxy connections
	}

	for i := 0; i < maxRetries; i++ {
		// Check if context is cancelled before attempting dial
		if ctx.Err() != nil {
			return ctx.Err()
		}
		conn, _, err = dialer.DialContext(ctx, url, nil)
		if err == nil {
			break
		}

		if i < maxRetries-1 {
			c.onWarning(fmt.Sprintf("Dial attempt %d failed: %v, retrying...", i+1, err))
			// Use select to allow context cancellation during backoff sleep
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(i+1) * time.Second):
			}
		}
	}

	if err != nil {
		c.onError(fmt.Errorf("Dial(): %v", err))
		c.setLastError(err)
		return err
	}

	// Send the USP header.
	if err := conn.WriteJSON(protocol.ConnectionHeader{
		Version:         protocol.CurrentVersion,
		Oid:             c.options.Identity.Oid,
		InstallationKey: c.options.Identity.InstallationKey,
		Hostname:        c.options.Hostname,
		Platform:        c.options.Platform,
		Architecture:    c.options.Architecture,
		Mapping:         c.options.Mapping,
		Mappings:        c.options.Mappings,
		Indexing:        c.options.Indexing,
		SensorSeedKey:   c.options.SensorSeedKey,
		IsCompressed:    c.options.IsCompressed,
		DataFormat:      "msgpack",
		InstanceID:      c.instanceID,
	}); err != nil {
		c.onWarning(fmt.Sprintf("WriteJSON(): %v", err))
		conn.WriteControl(websocket.CloseMessage, []byte{}, time.Now().Add(5*time.Second))
		conn.Close()
		c.setLastError(err)
		return err
	}
	// Wait for sink ready signal.
	// Closing the socket on cancellation unblocks the READY read below, so
	// Close() (which waits on connMutex) does not sit out the whole timeout.
	stopCloseOnCancel := context.AfterFunc(ctx, func() { conn.Close() })
	defer stopCloseOnCancel()
	// Use context deadline if available, otherwise default to 25s timeout.
	msg := protocol.ControlMessage{}
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetReadDeadline(deadline)
	} else {
		conn.SetReadDeadline(time.Now().Add(25 * time.Second))
	}
	if err := conn.ReadJSON(&msg); err != nil {
		// Nobody else owns this socket: close it or it stays open (and
		// counted by the server) until the garbage collector finds it.
		conn.Close()
		c.setLastError(err)
		return err
	}

	// The context fired after the read returned: the socket was just closed.
	if !stopCloseOnCancel() {
		c.setLastError(ctx.Err())
		return ctx.Err()
	}

	// We expect a READY control message to tell us
	// we're good to go. If not, abort.
	if err := c.processControlMessage(msg); err != nil {
		conn.Close()
		c.setLastError(err)
		return err
	}

	// Close() may have run while we were connecting (it only waits for us at
	// disconnect()). Starting the workers now would resurrect a client that
	// was just closed, and clearing isStop would keep disconnect() waiting on
	// them forever.
	if c.isClosing.Load() {
		conn.WriteControl(websocket.CloseMessage, []byte{}, time.Now().Add(5*time.Second))
		conn.Close()
		return errClientClosing
	}

	c.isStop.Clear()
	defer c.isStart.Set()
	c.conn = conn
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.listener()
	}()
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.sender()
	}()
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.keepAliveSender()
	}()
	c.log("usp-client connected")
	return nil
}

func (c *Client) disconnect() error {
	c.log("usp-client disconnecting")
	c.isStop.Set()
	c.connMutex.Lock()
	defer c.connMutex.Unlock()
	var err error
	if c.conn != nil {
		c.conn.WriteControl(websocket.CloseMessage, []byte{}, time.Now().Add(5*time.Second))
		err = c.conn.Close()
	}
	c.wg.Wait()
	c.conn = nil
	if err != nil {
		c.setLastError(err)
		return err
	}
	return nil
}

func (c *Client) Reconnect() {
	if c.options.TestSinkMode {
		return
	}
	if c.isClosing.Load() {
		// If we're actually closing ignore
		// requests for reconnecting.
		return
	}
	// Make sure only one thread is reconnecting
	if !atomic.CompareAndSwapUint32(&c.isReconnecting, 0, 1) {
		return
	}

	go func() {
		if err := c.disconnect(); err != nil {
			c.setLastError(err)
			c.onWarning(fmt.Sprintf("error disconnecting: %v", err))
		}

		// Start the window size back to 1.
		c.ab.UpdateCapacity(1)

		// We assume that anything that was not ACKed should be resent
		c.ab.ResetDelivery()

		defer atomic.StoreUint32(&c.isReconnecting, 0)

		// Clients not built by NewClient have no close context.
		closeCtx := c.closeCtx
		if closeCtx == nil {
			closeCtx = context.Background()
		}

		for {
			// Keep trying until connected, but only until Close() is called.
			err := c.connect(closeCtx)
			if err == nil {
				return
			}
			if c.isClosing.Load() {
				return
			}
			c.setLastError(err)
			c.onWarning(fmt.Sprintf("error reconnecting: %v", err))
			select {
			case <-closeCtx.Done():
				return
			case <-time.After(time.Duration(reconnectRetryDelay.Load())):
			}
		}
	}()
}

// Ship queues a message for delivery, waiting up to timeout (0 = until there is
// room or the client is closed) for room in the buffer. It returns
// ErrorBufferFull if there was no room in time and ErrorClosed if the client is
// closed or gets closed while waiting.
func (c *Client) Ship(message *protocol.DataMessage, timeout time.Duration) error {
	if c.options.TestSinkMode {
		return nil
	}
	return c.ab.add(message, timeout)
}

// BufferedBytes returns the estimated size of the messages that were shipped
// but not yet acknowledged. It is only tracked when BufferOptions.MaxBytes or
// BufferOptions.SharedBudget is set, otherwise it is 0.
func (c *Client) BufferedBytes() uint64 {
	if c.ab == nil {
		return 0
	}
	return c.ab.BufferedBytes()
}

func (c *Client) GetUnsent() ([]*protocol.DataMessage, error) {
	if c.options.TestSinkMode {
		return nil, nil
	}
	messages, err := c.ab.GetUnAcked()
	if err != nil {
		c.setLastError(err)
		return nil, err
	}
	return messages, nil
}

func (c *Client) listener() {
	defer c.log("listener exited")

	c.isStart.Wait()

	for !c.isStop.IsSet() {
		msg := protocol.ControlMessage{}
		c.conn.SetReadDeadline(time.Now().Add(24 * time.Hour))
		if err := c.conn.ReadJSON(&msg); err != nil {
			c.onWarning(err.Error())
			c.setLastError(err)
			c.Reconnect()
			return
		}

		c.processControlMessage(msg)
	}
}

func (c *Client) processControlMessage(msg protocol.ControlMessage) error {
	switch msg.Verb {
	case protocol.ControlMessageACK:
		if err := c.ab.Ack(msg.SeqNum); err != nil {
			c.setLastError(err)
			c.onWarning(fmt.Sprintf("error acking %d: %v", msg.SeqNum, err))
		}
	case protocol.ControlMessageBACKOFF:
		atomic.SwapUint64(&c.backoffTime, msg.Duration)
	case protocol.ControlMessageRECONNECT:
		c.Reconnect()
		return errors.New("reconnect requested")
	case protocol.ControlMessageERROR:
		err := errors.New(msg.Error)
		c.onError(err)
		return err
	case protocol.ControlMessageREADY:
		return nil
	case protocol.ControlMessageFLOW:
		c.log(fmt.Sprintf("flow control adjusted: %d -> %d", c.ab.GetCurrentCapacity(), msg.WindowSize))
		c.ab.UpdateCapacity(msg.WindowSize)
	default:
		// Ignoring unknown verbs.
		err := fmt.Errorf("received unknown control message: %s", msg.Verb)
		c.onWarning(err.Error())
		c.setLastError(err)
		return err
	}
	return nil
}

func (c *Client) sender() {
	defer c.log("sender exited")

	c.isStart.Wait()

	if um, err := c.ab.GetUnAcked(); err == nil && len(um) != 0 {
		c.log(fmt.Sprintf("re-transmitting %d previously unacked messages", len(um)))
	}

	for !c.isStop.IsSet() {
		for {
			backoffSec := atomic.SwapUint64(&c.backoffTime, 0)
			if backoffSec == 0 {
				break
			}
			c.log(fmt.Sprintf("backing off %d seconds", backoffSec))
			// Stop must interrupt even a very long server throttle.
			// Cap before converting to Duration to avoid integer overflow.
			if backoffSec > uint64((24*time.Hour)/time.Second) {
				backoffSec = uint64((24 * time.Hour) / time.Second)
			}
			if c.waitBackoff(time.Duration(backoffSec) * time.Second) {
				return
			}
		}
		// Wait for a bit for messages to arrive.
		message := c.ab.GetNextToDeliver(500 * time.Millisecond)
		if message == nil {
			continue
		}

		// Apply compression if not done already.
		b := bytes.Buffer{}
		var (
			z *gzip.Writer
			w io.Writer
		)
		w = &b
		if c.options.IsCompressed {
			z = gzip.NewWriter(w)
			w = z
		}
		m := msgpack.NewEncoder(w)
		if err := m.Encode(message); err != nil {
			c.onError(fmt.Errorf("msgpack.Encode(): %v", err))
			c.setLastError(err)
			continue
		}
		if z != nil {
			if err := z.Close(); err != nil {
				c.onError(fmt.Errorf("gzip.Close(): %v", err))
				c.setLastError(err)

				continue
			}
		}

		c.conn.SetWriteDeadline(time.Now().Add(30 * time.Minute))
		if err := c.conn.WriteMessage(websocket.BinaryMessage, b.Bytes()); err != nil {
			c.onWarning(fmt.Sprintf("timeout sending data, reconnecting: %v", err))
			c.setLastError(err)
			c.Reconnect()
			return
		}
	}
}

// Reconnect preserves the proxy's throttle interval, while Close must be able
// to interrupt it even when disconnect is already waiting on the sender.
func (c *Client) waitBackoff(d time.Duration) bool {
	var closing <-chan struct{}
	if c.closeCtx != nil {
		closing = c.closeCtx.Done()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-closing:
		return true
	case <-timer.C:
		return false
	}
}

func (c *Client) keepAliveSender() {
	defer c.log("keepalive exited")

	c.isStart.Wait()

	for {
		if c.isStop.WaitFor(30 * time.Second) {
			return
		}

		if err := c.conn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(30*time.Minute)); err != nil {
			c.onWarning(fmt.Sprintf("keepalive failed, reconnecting: %v", err))
			c.setLastError(err)
			c.Reconnect()
			return
		}
	}
}

func (c *Client) GetLastError() error {
	return c.lastError
}

func (c *Client) setLastError(err error) {
	c.errorMutex.Lock()
	defer c.errorMutex.Unlock()
	c.lastError = err
}

func (c *Client) log(m string) {
	if c.options.DebugLog == nil {
		return
	}
	c.options.DebugLog(m)
}

func (c *Client) onWarning(m string) {
	if c.options.OnWarning != nil {
		c.options.OnWarning(m)
	}
}

func (c *Client) onError(err error) {
	if c.options.OnError != nil {
		c.options.OnError(err)
	}
}
