package uspclient

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	lc "github.com/refractionPOINT/go-limacharlie/limacharlie"
)

type Client struct {
	ident   Identity
	options ClientOptions
	org     *lc.Organization
	wssURL  string

	conn           *websocket.Conn
	ab             *AckBuffer
	wg             sync.WaitGroup
	connMutex      sync.Mutex
	isStop         bool
	isReconnecting uint32
	backoffTime    uint64

	lastError error
}

type Identity struct {
	Oid          string
	IngestionKey string
}

type ClientOptions struct {
	Hostname      string
	ParseHint     string
	BufferOptions AckBufferOptions

	debugLog func(string)
}

type connectionHeader struct {
	Oid          string `json:"OID"`
	IngestionKey string `json:"IK"`
	Hostname     string `json:"HOST_NAME,omitempty"`
	ParseHint    string `json:"PARSE_HINT,omitempty"`
}

var ErrorStaleConnection = errors.New("connection stale")

func NewClient(i Identity, o ClientOptions) (*Client, error) {
	// Get an SDK instance so we can resolve the datacenter.
	org, err := lc.NewOrganizationFromClientOptions(lc.ClientOptions{}, nil)
	if err != nil {
		return nil, err
	}
	// Resolve the WSS URL for our datacenter.
	urls, err := org.GetURLs()
	if err != nil {
		return nil, err
	}
	wssEndpoint, ok := urls["lc_wss"]
	if !ok {
		return nil, errors.New("site url for org not found")
	}
	ab, err := NewAckBuffer(o.BufferOptions)
	if err != nil {
		return nil, err
	}
	c := &Client{
		ident:   i,
		options: o,
		org:     org,
		wssURL:  fmt.Sprintf("wss://%s/usp", wssEndpoint),
		ab:      ab,
	}
	if err := c.connect(); err != nil {
		return nil, err
	}

	return c, nil
}

func (c *Client) Close() ([]*UspDataMessage, error) {
	err1 := c.disconnect()
	messages, err2 := c.ab.GetUnAcked()
	err := err1
	if err == nil {
		err = err2
	}
	c.lastError = err
	return messages, err
}

func (c *Client) connect() error {
	c.connMutex.Lock()
	defer c.connMutex.Unlock()

	// Connect the websocket.
	conn, _, err := websocket.DefaultDialer.Dial(c.wssURL, nil)
	if err != nil {
		c.lastError = err
		return err
	}
	// Send the USP header.
	if err := conn.WriteJSON(connectionHeader{
		Oid:          c.ident.Oid,
		IngestionKey: c.ident.IngestionKey,
		Hostname:     c.options.Hostname,
		ParseHint:    c.options.ParseHint,
	}); err != nil {
		conn.Close()
		c.lastError = err
		return err
	}
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
	c.conn = conn
	return nil
}

func (c *Client) disconnect() error {
	c.isStop = true
	c.connMutex.Lock()
	defer c.connMutex.Unlock()
	err := c.conn.Close()
	c.wg.Wait()
	if err != nil {
		c.lastError = err
		return err
	}
	return nil
}

func (c *Client) reconnect() {
	// Make sure only one thread is reconnecting
	if !atomic.CompareAndSwapUint32(&c.isReconnecting, 0, 1) {
		return
	}
	if err := c.disconnect(); err != nil {
		c.lastError = err
		c.log(fmt.Sprintf("error disconnecting: %v", err))
	}

	// We assume that anything that was not ACKed should be resent
	c.ab.ResetDelivery()

	if err := c.connect(); err != nil {
		c.lastError = err
		c.log(fmt.Sprintf("error reconnecting: %v", err))
	}
}

func (c *Client) Ship(message *UspDataMessage, timeout time.Duration) error {
	if !c.ab.Add(message, timeout) {
		return ErrorStaleConnection
	}

	return nil
}

func (c *Client) GetUnsent() ([]*UspDataMessage, error) {
	messages, err := c.ab.GetUnAcked()
	if err != nil {
		c.lastError = err
		return nil, err
	}
	return messages, nil
}

func (c *Client) listener() {
	for !c.isStop {
		msg := uspControlMessage{}
		if err := c.conn.ReadJSON(&msg); err != nil {
			c.lastError = err
			go c.reconnect()
			return
		}

		switch msg.Verb {
		case uspControlMessageACK:
			if err := c.ab.Ack(msg.SeqNum); err != nil {
				c.lastError = err
				c.log(fmt.Sprintf("error acking %d: %v", msg.SeqNum, err))
			}
		case uspControlMessageBACKOFF:
			atomic.SwapUint64(&c.backoffTime, msg.Duration)
		case uspControlMessageRECONNECT:
			go c.reconnect()
			return
		default:
			// Ignoring unknown verbs.
			err := fmt.Errorf("received unknown control message: %s", msg.Verb)
			c.log(err.Error())
			c.lastError = err
		}
	}
}

func (c *Client) sender() {
	for !c.isStop {
		backoffSec := atomic.SwapUint64(&c.backoffTime, 0)
		if backoffSec != 0 {
			c.log(fmt.Sprintf("backing off %d seconds", backoffSec))
			time.Sleep(time.Duration(backoffSec) * time.Second)
		}
		message := c.ab.GetNextToDeliver(500 * time.Millisecond)
		if message == nil {
			continue
		}
		c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err := c.conn.WriteJSON(message); err != nil {
			c.lastError = err
			go c.reconnect()
			return
		}
	}
}

func (c *Client) GetLastError() error {
	return c.lastError
}

func (c *Client) log(m string) {
	if c.options.debugLog == nil {
		return
	}
	c.options.debugLog(m)
}
