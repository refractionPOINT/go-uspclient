package uspclient

import (
	"errors"
	"fmt"

	"github.com/gorilla/websocket"
	lc "github.com/refractionPOINT/go-limacharlie/limacharlie"
)

type Client struct {
	ident   Identity
	options ClientOptions
	org     *lc.Organization
	wssURL  string

	conn *websocket.Conn
}

type Identity struct {
	Oid          string
	IngestionKey string
}

type ClientOptions struct {
	Hostname    string
	ParseHint   string
	NumBuffered uint64
}

type connectionHeader struct {
	Oid          string `json:"OID"`
	IngestionKey string `json:"IK"`
	Hostname     string `json:"HOST_NAME,omitempty"`
	ParseHint    string `json:"PARSE_HINT,omitempty"`
}

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
	c := &Client{
		ident:   i,
		options: o,
		org:     org,
		wssURL:  fmt.Sprintf("wss://%s/usp", wssEndpoint),
	}

	return c, nil
}

func (c *Client) Connect() error {
	// Connect the websocket.
	conn, _, err := websocket.DefaultDialer.Dial(c.wssURL, nil)
	if err != nil {
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
		return err
	}
	c.conn = conn
	return nil
}

func (c *Client) Disconnect() error {
	return c.conn.Close()
}
