package uspclient

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

func TestConnection(t *testing.T) {
	testOID := uuid.NewString()
	testIK := "456"
	testHostname := "testhost"
	testHint := "hint"
	testPort := 7777
	nConnections := uint32(0)
	wg := sync.WaitGroup{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isConnReceived := false
		isHeaderReceived := false
		isHeaderValid := false

		defer func() {
			if !isConnReceived {
				t.Error("connection not received")
			}
			if !isHeaderReceived {
				t.Error("header not received")
			}
			if !isHeaderValid {
				t.Error("header not valid")
			}
		}()

		if r.URL.Path != "/usp" {
			t.Errorf("unexpected URL path: %s", r.URL.Path)
			return
		}
		isConnReceived = true
		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("Upgrade(): %v", err)
			return
		}

		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		f := frame{}
		if err := conn.ReadJSON(&f); err != nil {
			t.Errorf("ReadJSON(): %v", err)
			return
		}
		if len(f.Messages) == 0 {
			t.Error("not enough messages")
			return
		}
		msg := f.Messages[0].(map[string]interface{})
		isHeaderReceived = true

		oid, _ := msg["OID"].(string)
		ik, _ := msg["IK"].(string)
		hostname, _ := msg["HOST_NAME"].(string)
		hint, _ := msg["PARSE_HINT"].(string)

		if oid != testOID || ik != testIK || hostname != testHostname || hint != testHint {
			return
		}
		isHeaderValid = true
		nConnections++
		atomic.AddUint32(&nConnections, 1)

		m := sync.Mutex{}
		if atomic.LoadUint32(&nConnections) == 1 {
			// Only do a reconnect on the first connection.
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(5 * time.Second)
				m.Lock()
				defer m.Unlock()
				conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
				if err := conn.WriteJSON(&uspControlMessage{
					Verb: uspControlMessageRECONNECT,
				}); err != nil {
					fmt.Printf("WriteJSON(): %v\n", err)
					return
				}
			}()
		}

		for {
			conn.SetReadDeadline(time.Now().Add(20 * time.Second))
			f := frame{}
			if err := conn.ReadJSON(&f); err != nil {
				fmt.Printf("ReadJSON(): %v\n", err)
				return
			}
			if len(f.Messages) == 0 {
				t.Error("missing messages")
				return
			}
			for _, nm := range f.Messages {
				uMsg := UspDataMessage{}
				d, _ := json.Marshal(nm)
				json.Unmarshal(d, &uMsg)

				if uMsg.AckRequested {
					m.Lock()
					conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
					if err := conn.WriteJSON(&uspControlMessage{
						Verb:   uspControlMessageACK,
						SeqNum: uMsg.SeqNum,
					}); err != nil {
						fmt.Printf("WriteJSON(): %v\n", err)
						m.Unlock()
						return
					}
					m.Unlock()
				}
				if v, ok := uMsg.JsonPayload["some"]; !ok || v != "payload" {
					t.Error("missing payload")
					return
				}
			}
		}
	})
	srv := &http.Server{
		Handler: h,
		Addr:    fmt.Sprintf("127.0.0.1:%d", testPort),
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := srv.ListenAndServe(); err != nil {
			fmt.Printf("ListenAndServe(): %v\n", err)
		}
	}()
	time.Sleep(2 * time.Second)

	c, err := NewClient(ClientOptions{
		Identity: Identity{
			Oid:          testOID,
			IngestionKey: testIK,
		},
		DestURL:   fmt.Sprintf("ws://127.0.0.1:%d/usp", testPort),
		Hostname:  testHostname,
		ParseHint: testHint,
		DebugLog: func(s string) {
			fmt.Println(s)
		},
		BufferOptions: AckBufferOptions{
			BufferCapacity: 10,
		},
	})
	if err != nil {
		t.Errorf("NewClient(): %v", err)
		return
	}

	time.Sleep(2 * time.Second)

	for i := 0; i < 30; i++ {
		if err := c.Ship(&UspDataMessage{
			JsonPayload: map[string]interface{}{
				"some": "payload",
			},
		}, 1*time.Second); err != nil {
			t.Errorf("Ship(): %v", err)
			break
		}
	}

	time.Sleep(10 * time.Second)

	c.Close()

	if atomic.LoadUint32(&nConnections) != 2 {
		t.Errorf("unexpected number of total connections: %d", atomic.LoadUint32(&nConnections))
	}

	srv.Close()
	wg.Wait()
}
