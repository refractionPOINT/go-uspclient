package uspclient

import (
	"bufio"
	"context"
	"encoding/base64"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
)

// mockProxyServer creates a simple HTTP CONNECT proxy for testing
type mockProxyServer struct {
	listener net.Listener
	addr     string
	username string
	password string
	t        *testing.T
}

func newMockProxyServer(t *testing.T, username, password string) (*mockProxyServer, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}

	proxy := &mockProxyServer{
		listener: listener,
		addr:     listener.Addr().String(),
		username: username,
		password: password,
		t:        t,
	}

	go proxy.serve()
	return proxy, nil
}

func (p *mockProxyServer) serve() {
	for {
		conn, err := p.listener.Accept()
		if err != nil {
			return
		}
		go p.handleConnection(conn)
	}
}

func (p *mockProxyServer) handleConnection(conn net.Conn) {
	defer conn.Close()

	reader := bufio.NewReader(conn)
	request, err := http.ReadRequest(reader)
	if err != nil {
		p.t.Logf("Failed to read request: %v", err)
		return
	}

	if request.Method != "CONNECT" {
		conn.Write([]byte("HTTP/1.1 405 Method Not Allowed\r\n\r\n"))
		return
	}

	// Check authentication if required
	if p.username != "" && p.password != "" {
		authHeader := request.Header.Get("Proxy-Authorization")
		expectedAuth := "Basic " + basicAuth(p.username, p.password)
		if authHeader != expectedAuth {
			conn.Write([]byte("HTTP/1.1 407 Proxy Authentication Required\r\n\r\n"))
			return
		}
	}

	// Simulate successful CONNECT
	conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	// For testing purposes, just echo back any data received
	buffer := make([]byte, 1024)
	for {
		n, err := conn.Read(buffer)
		if err != nil {
			return
		}
		conn.Write(buffer[:n])
	}
}

func (p *mockProxyServer) close() {
	p.listener.Close()
}

func (p *mockProxyServer) URL() string {
	return "http://" + p.addr
}

func basicAuth(username, password string) string {
	auth := username + ":" + password
	return base64.StdEncoding.EncodeToString([]byte(auth))
}

func TestClientOptions_Validate_ProxyConfig(t *testing.T) {
	tests := []struct {
		name        string
		proxyURL    string
		proxyUser   string
		proxyPass   string
		expectError bool
		errorMsg    string
	}{
		{
			name:        "valid proxy URL",
			proxyURL:    "http://proxy.example.com:8080",
			expectError: false,
		},
		{
			name:        "invalid proxy URL",
			proxyURL:    "://invalid-url",
			expectError: true,
			errorMsg:    "invalid proxy URL",
		},
		{
			name:        "proxy with valid auth",
			proxyURL:    "http://proxy.example.com:8080",
			proxyUser:   "user",
			proxyPass:   "pass",
			expectError: false,
		},
		{
			name:        "proxy with incomplete auth - missing password",
			proxyURL:    "http://proxy.example.com:8080",
			proxyUser:   "user",
			proxyPass:   "",
			expectError: true,
			errorMsg:    "proxy authentication requires both username and password",
		},
		{
			name:        "proxy with incomplete auth - missing username",
			proxyURL:    "http://proxy.example.com:8080",
			proxyUser:   "",
			proxyPass:   "pass",
			expectError: true,
			errorMsg:    "proxy authentication requires both username and password",
		},
		{
			name:        "proxy URL with credentials",
			proxyURL:    "http://user:pass@proxy.example.com:8080",
			expectError: true,
			errorMsg:    "proxy URL must not contain credentials",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := ClientOptions{
				Identity: Identity{
					Oid:             "test-org",
					InstallationKey: "test-key",
				},
				Platform: "test",
				Proxy: ProxyOptions{
					URL:      tt.proxyURL,
					Username: tt.proxyUser,
					Password: tt.proxyPass,
				},
			}

			err := opts.Validate()
			if tt.expectError {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.errorMsg)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestCreateProxyDialer(t *testing.T) {
	tests := []struct {
		name          string
		setupProxy    bool
		proxyAuth     bool
		proxyURL      string
		expectDefault bool
	}{
		{
			name:          "no proxy configured",
			setupProxy:    false,
			expectDefault: true,
		},
		{
			name:          "proxy without auth",
			setupProxy:    true,
			proxyAuth:     false,
			expectDefault: false,
		},
		{
			name:          "proxy with auth",
			setupProxy:    true,
			proxyAuth:     true,
			expectDefault: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var proxyURL string
			var client *Client

			if tt.setupProxy {
				// Create mock proxy server
				username := ""
				password := ""
				if tt.proxyAuth {
					username = "testuser"
					password = "testpass"
				}

				proxy, err := newMockProxyServer(t, username, password)
				assert.NoError(t, err)
				defer proxy.close()

				proxyURL = proxy.URL()
				opts := ClientOptions{
					Proxy: ProxyOptions{
						URL:      proxyURL,
						Username: username,
						Password: password,
					},
				}
				opts.normalizeProxyConfig()
				client = &Client{
					options: opts,
					wssURL: "wss://test.limacharlie.io/test",
				}
			} else {
				opts := ClientOptions{}
				opts.normalizeProxyConfig()
				client = &Client{
					options: opts,
					wssURL:  "wss://test.limacharlie.io/test",
				}
			}

			dialer, err := client.createProxyDialer()
			assert.NoError(t, err)
			assert.NotNil(t, dialer)

			if tt.expectDefault {
				// When no proxy is configured, should return default dialer
				assert.Equal(t, websocket.DefaultDialer, dialer)
			} else {
				// When proxy is configured, should return custom dialer
				assert.NotEqual(t, websocket.DefaultDialer, dialer)
				assert.NotNil(t, dialer.NetDialContext)
				assert.Nil(t, dialer.Proxy)
				assert.NotNil(t, dialer.TLSClientConfig)
			}
		})
	}
}

func TestProxyDialerConnection(t *testing.T) {
	// Create mock proxy server
	proxy, err := newMockProxyServer(t, "testuser", "testpass")
	assert.NoError(t, err)
	defer proxy.close()

	opts := ClientOptions{
		Proxy: ProxyOptions{
			URL:      proxy.URL(),
			Username: "testuser",
			Password: "testpass",
		},
	}
	opts.normalizeProxyConfig()
	client := &Client{
		options: opts,
		wssURL: "wss://test.limacharlie.io/test",
	}

	dialer, err := client.createProxyDialer()
	assert.NoError(t, err)

	// Test the NetDialContext function
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := dialer.NetDialContext(ctx, "tcp", "test.limacharlie.io:443")
	assert.NoError(t, err)
	assert.NotNil(t, conn)
	if conn != nil {
		conn.Close()
	}
}

func TestProxyDialerAuthFailure(t *testing.T) {
	// Create mock proxy server with auth
	proxy, err := newMockProxyServer(t, "testuser", "testpass")
	assert.NoError(t, err)
	defer proxy.close()

	opts := ClientOptions{
		Proxy: ProxyOptions{
			URL:      proxy.URL(),
			Username: "wronguser",
			Password: "wrongpass",
		},
	}
	opts.normalizeProxyConfig()
	client := &Client{
		options: opts,
		wssURL: "wss://test.limacharlie.io/test",
	}

	dialer, err := client.createProxyDialer()
	assert.NoError(t, err)

	// Test the NetDialContext function with wrong credentials
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := dialer.NetDialContext(ctx, "tcp", "test.limacharlie.io:443")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "proxy returned status 407")
	assert.Nil(t, conn)
}

func TestInvalidProxyURL(t *testing.T) {
	opts := ClientOptions{
		Proxy: ProxyOptions{
			URL: "://invalid-url",
		},
	}
	opts.normalizeProxyConfig()
	client := &Client{
		options: opts,
		wssURL: "wss://test.limacharlie.io/test",
	}

	dialer, err := client.createProxyDialer()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid proxy URL")
	assert.Nil(t, dialer)
}

func TestProxyTimeoutConfiguration(t *testing.T) {
	tests := []struct {
		name                     string
		handshakeTimeout         time.Duration
		connectTimeout           time.Duration
		readWriteTimeout         time.Duration
		expectedHandshakeTimeout time.Duration
		expectedConnectTimeout   time.Duration
		expectedReadWriteTimeout time.Duration
	}{
		{
			name:                     "default timeouts",
			handshakeTimeout:         0,
			connectTimeout:           0,
			readWriteTimeout:         0,
			expectedHandshakeTimeout: 45 * time.Second,
			expectedConnectTimeout:   10 * time.Second,
			expectedReadWriteTimeout: 30 * time.Second,
		},
		{
			name:                     "custom timeouts",
			handshakeTimeout:         60 * time.Second,
			connectTimeout:           20 * time.Second,
			readWriteTimeout:         40 * time.Second,
			expectedHandshakeTimeout: 60 * time.Second,
			expectedConnectTimeout:   20 * time.Second,
			expectedReadWriteTimeout: 40 * time.Second,
		},
		{
			name:                     "partial custom timeouts",
			handshakeTimeout:         0,
			connectTimeout:           15 * time.Second,
			readWriteTimeout:         0,
			expectedHandshakeTimeout: 45 * time.Second,
			expectedConnectTimeout:   15 * time.Second,
			expectedReadWriteTimeout: 30 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := ClientOptions{
				Proxy: ProxyOptions{
					URL:              "http://proxy.example.com:8080",
					HandshakeTimeout: tt.handshakeTimeout,
					ConnectTimeout:   tt.connectTimeout,
					ReadWriteTimeout: tt.readWriteTimeout,
				},
			}
			opts.normalizeProxyConfig()

			assert.Equal(t, tt.expectedHandshakeTimeout, opts.Proxy.HandshakeTimeout)
			assert.Equal(t, tt.expectedConnectTimeout, opts.Proxy.ConnectTimeout)
			assert.Equal(t, tt.expectedReadWriteTimeout, opts.Proxy.ReadWriteTimeout)
		})
	}
}

