package uspclient

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	lc "github.com/refractionPOINT/go-limacharlie/limacharlie"
	"github.com/refractionPOINT/go-uspclient/protocol"

	"github.com/vmihailenco/msgpack/v5"
)

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
}

type Identity struct {
	Oid             string `json:"oid" yaml:"oid"`
	InstallationKey string `json:"installation_key" yaml:"installation_key"`
}

type ClientOptions struct {
	Identity      Identity                   `json:"identity" yaml:"identity"`
	Hostname      string                     `json:"hostname,omitempty" yaml:"hostname,omitempty"`
	Platform      string                     `json:"platform,omitempty" yaml:"platform,omitempty"`
	Architecture  string                     `json:"architecture,omitempty" yaml:"architecture,omitempty"`
	Mapping       protocol.MappingDescriptor `json:"mapping,omitempty" yaml:"mapping,omitempty"`
	BufferOptions AckBufferOptions           `json:"buffer_options,omitempty" yaml:"buffer_options,omitempty"`
	IsCompressed  bool                       `json:"is_compressed,omitempty" yaml:"is_compressed,omitempty"`

	SensorKeyPath string `json:"sensor_key_path" yaml:"sensor_key_path"`
	SensorSeedKey string `json:"sensor_seed_key" yaml:"sensor_seed_key"`

	DebugLog  func(string) `json:"-" yaml:"-"`
	OnError   func(error)  `json:"-" yaml:"-"`
	OnWarning func(string) `json:"-" yaml:"-"`

	// Auto-detect if not specified (preferred).
	DestURL string `json:"dest_url,omitempty" yaml:"dest_url,omitempty"`
}

var ErrorBufferFull = errors.New("buffer full")

func NewClient(o ClientOptions) (*Client, error) {
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
		// Audo-detect, resolve the WSS URL for our datacenter.
		urls, err := org.GetURLs()
		if err != nil {
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

	if o.Mapping.ParsingRE != "" {
		if _, err := regexp.Compile(o.Mapping.ParsingRE); err != nil {
			return nil, fmt.Errorf("invalid mapping.parsing_re: %v", err)
		}
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
	if err := c.connect(); err != nil {
		return nil, err
	}

	return c, nil
}

func (c *Client) Close() ([]*protocol.DataMessage, error) {
	c.log("usp-client closing")
	err1 := c.disconnect()
	messages, err2 := c.ab.GetUnAcked()
	err := err1
	if err == nil {
		err = err2
	}
	c.setLastError(err)
	return messages, err
}

func (c *Client) connect() error {
	c.log("usp-client connecting")
	c.connMutex.Lock()
	defer c.connMutex.Unlock()
	c.isStart.Clear()

	// Connect the websocket.
	conn, _, err := websocket.DefaultDialer.Dial(c.wssURL, nil)
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
		SensorSeedKey:   c.options.SensorSeedKey,
		IsCompressed:    c.options.IsCompressed,
		DataFormat:      "msgpack",
	}); err != nil {
		c.onWarning(fmt.Sprintf("WriteJSON(): %v", err))
		conn.WriteControl(websocket.CloseMessage, []byte{}, time.Now().Add(5*time.Second))
		conn.Close()
		c.setLastError(err)
		return err
	}
	// Wait for sink ready signal.
	msg := protocol.ControlMessage{}
	conn.SetReadDeadline(time.Now().Add(1 * time.Minute))
	if err := conn.ReadJSON(&msg); err != nil {
		c.setLastError(err)
		return err
	}

	// We expect a READY control message to tell us
	// we're good to go. If not, abort.
	if err := c.processControlMessage(msg); err != nil {
		c.setLastError(err)
		return err
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
	c.conn.WriteControl(websocket.CloseMessage, []byte{}, time.Now().Add(5*time.Second))
	err := c.conn.Close()
	c.wg.Wait()
	c.conn = nil
	if err != nil {
		c.setLastError(err)
		return err
	}
	return nil
}

func (c *Client) Reconnect() {
	// Make sure only one thread is reconnecting
	if !atomic.CompareAndSwapUint32(&c.isReconnecting, 0, 1) {
		return
	}
	go func() {
		if err := c.disconnect(); err != nil {
			c.setLastError(err)
			c.onWarning(fmt.Sprintf("error disconnecting: %v", err))
		}

		// We assume that anything that was not ACKed should be resent
		c.ab.ResetDelivery()

		var err error
		for {
			err = c.connect()
			if err != nil {
				c.setLastError(err)
				c.onWarning(fmt.Sprintf("error reconnecting: %v", err))
				time.Sleep(5 * time.Second)
				continue
			}
			break
		}

		atomic.StoreUint32(&c.isReconnecting, 0)
	}()
}

func (c *Client) Ship(message *protocol.DataMessage, timeout time.Duration) error {
	if !c.ab.Add(message, timeout) {
		return ErrorBufferFull
	}

	return nil
}

func (c *Client) GetUnsent() ([]*protocol.DataMessage, error) {
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
		c.conn.SetReadDeadline(time.Now().Add(60 * time.Minute))
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

	for !c.isStop.IsSet() {
		backoffSec := atomic.SwapUint64(&c.backoffTime, 0)
		if backoffSec != 0 {
			c.log(fmt.Sprintf("backing off %d seconds", backoffSec))
			time.Sleep(time.Duration(backoffSec) * time.Second)
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

		c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := c.conn.WriteMessage(websocket.BinaryMessage, b.Bytes()); err != nil {
			c.onWarning(fmt.Sprintf("timeout sending data, reconnecting: %v", err))
			c.setLastError(err)
			c.Reconnect()
			return
		}
	}
}

func (c *Client) keepAliveSender() {
	defer c.log("keepalive exited")

	c.isStart.Wait()

	for {
		if c.isStop.WaitFor(30 * time.Second) {
			return
		}

		if err := c.conn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(5*time.Second)); err != nil {
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
