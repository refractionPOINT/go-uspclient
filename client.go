package uspclient

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	lc "github.com/refractionPOINT/go-limacharlie/limacharlie"
)

type Client struct {
	options ClientOptions
	org     *lc.Organization
	wssURL  string

	conn           *websocket.Conn
	ab             *AckBuffer
	wg             sync.WaitGroup
	connMutex      sync.Mutex
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
	Identity      Identity         `json:"identity" yaml:"identity"`
	Hostname      string           `json:"hostname,omitempty" yaml:"hostname,omitempty"`
	Platform      string           `json:"platform,omitempty" yaml:"platform,omitempty"`
	Architecture  string           `json:"architecture,omitempty" yaml:"architecture,omitempty"`
	FormatRE      string           `json:"format_re,omitempty" yaml:"format_re,omitempty"`
	BufferOptions AckBufferOptions `json:"buffer_options,omitempty" yaml:"buffer_options,omitempty"`

	SensorKeyPath string `json:"sensor_key_path" yaml:"sensor_key_path"`
	SensorSeedKey string `json:"sensor_seed_key" yaml:"sensor_seed_key"`

	DebugLog func(string) `json:"-" yaml:"-"`

	// Auto-detect if not specified (preferred).
	DestURL string `json:"dest_url,omitempty" yaml:"dest_url,omitempty"`
}

type connectionHeader struct {
	Oid             string `json:"OID"`
	InstallationKey string `json:"IID"`
	Hostname        string `json:"HOST_NAME,omitempty"`
	Platform        string `json:"PLATFORM"`
	Architecture    string `json:"ARCHITECTURE"`
	FormatRE        string `json:"FORMAT_RE,omitempty"`
	SensorKeyPath   string `json:"SENSOR_KEY_PATH,omitempty"`
	SensorSeedKey   string `json:"SENSOR_SEED_KEY"`
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

	if o.FormatRE != "" {
		if _, err := regexp.Compile(o.FormatRE); err != nil {
			return nil, fmt.Errorf("invalid format_re: %v", err)
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
	}
	if err := c.connect(); err != nil {
		return nil, err
	}

	return c, nil
}

func (c *Client) Close() ([]*UspDataMessage, error) {
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

	// Connect the websocket.
	conn, _, err := websocket.DefaultDialer.Dial(c.wssURL, nil)
	if err != nil {
		c.log(fmt.Sprintf("usp-client Dial(): %v", err))
		c.setLastError(err)
		return err
	}
	// Send the USP header.
	if err := conn.WriteJSON(connectionHeader{
		Oid:             c.options.Identity.Oid,
		InstallationKey: c.options.Identity.InstallationKey,
		Hostname:        c.options.Hostname,
		Platform:        c.options.Platform,
		Architecture:    c.options.Architecture,
		FormatRE:        c.options.FormatRE,
		SensorKeyPath:   c.options.SensorKeyPath,
		SensorSeedKey:   c.options.SensorSeedKey,
	}); err != nil {
		c.log(fmt.Sprintf("usp-client WriteJSON(): %v", err))
		conn.Close()
		c.setLastError(err)
		return err
	}
	c.isStop.Clear()
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
	err := c.conn.Close()
	c.wg.Wait()
	if err != nil {
		c.setLastError(err)
		return err
	}
	return nil
}

func (c *Client) Reconnect() (bool, error) {
	// Make sure only one thread is reconnecting
	if !atomic.CompareAndSwapUint32(&c.isReconnecting, 0, 1) {
		return false, nil
	}
	if err := c.disconnect(); err != nil {
		c.setLastError(err)
		c.log(fmt.Sprintf("error disconnecting: %v", err))
	}

	// We assume that anything that was not ACKed should be resent
	c.ab.ResetDelivery()

	var err error
	for {
		err = c.connect()
		if err != nil {
			c.setLastError(err)
			c.log(fmt.Sprintf("error reconnecting: %v", err))
			time.Sleep(5 * time.Second)
			continue
		}
		break
	}

	atomic.StoreUint32(&c.isReconnecting, 0)

	return true, err
}

func (c *Client) Ship(message *UspDataMessage, timeout time.Duration) error {
	if !c.ab.Add(message, timeout) {
		return ErrorBufferFull
	}

	return nil
}

func (c *Client) GetUnsent() ([]*UspDataMessage, error) {
	messages, err := c.ab.GetUnAcked()
	if err != nil {
		c.setLastError(err)
		return nil, err
	}
	return messages, nil
}

func (c *Client) listener() {
	defer c.log("listener exited")

	for !c.isStop.IsSet() {
		msg := uspControlMessage{}
		c.conn.SetReadDeadline(time.Now().Add(60 * time.Minute))
		if err := c.conn.ReadJSON(&msg); err != nil {
			c.setLastError(err)
			go c.Reconnect()
			return
		}

		switch msg.Verb {
		case uspControlMessageACK:
			if err := c.ab.Ack(msg.SeqNum); err != nil {
				c.setLastError(err)
				c.log(fmt.Sprintf("error acking %d: %v", msg.SeqNum, err))
			}
		case uspControlMessageBACKOFF:
			atomic.SwapUint64(&c.backoffTime, msg.Duration)
		case uspControlMessageRECONNECT:
			go c.Reconnect()
			return
		case uspControlMessageERROR:
			c.log(fmt.Sprintf("receive error from LimaCharlie: %s", msg.Error))
		default:
			// Ignoring unknown verbs.
			err := fmt.Errorf("received unknown control message: %s", msg.Verb)
			c.log(err.Error())
			c.setLastError(err)
		}
	}
}

func (c *Client) sender() {
	defer c.log("sender exited")

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

		c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := c.conn.WriteJSON(message); err != nil {
			c.setLastError(err)
			go c.Reconnect()
			return
		}
	}
}

func (c *Client) keepAliveSender() {
	defer c.log("keepalive exited")

	for {
		if c.isStop.WaitFor(30 * time.Second) {
			return
		}

		c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := c.conn.WriteJSON(map[string]interface{}{}); err != nil {
			c.setLastError(err)
			go c.Reconnect()
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
