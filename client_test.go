package uspclient

import (
	"fmt"
	"net/http"
	"sync"
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
		msg := map[string]interface{}{}
		if err := conn.ReadJSON(&msg); err != nil {
			t.Errorf("ReadJSON(): %v", err)
			return
		}
		isHeaderReceived = true

		oid, _ := msg["OID"].(string)
		ik, _ := msg["IK"].(string)
		hostname, _ := msg["HOST_NAME"].(string)
		hint, _ := msg["PARSE_HINT"].(string)

		if oid != testOID || ik != testIK || hostname != testHostname || hint != testHint {
			return
		}
		isHeaderValid = true
	})
	srv := &http.Server{
		Handler: h,
		Addr:    fmt.Sprintf("127.0.0.1:%d", testPort),
	}
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := srv.ListenAndServe(); err != nil {
			fmt.Printf("ListenAndServe(): %v\n", err)
		}
	}()
	time.Sleep(2 * time.Second)

	c, err := NewClient(Identity{
		Oid:          testOID,
		IngestionKey: testIK,
	}, ClientOptions{
		DestURL:   fmt.Sprintf("ws://127.0.0.1:%d/usp", testPort),
		Hostname:  testHostname,
		ParseHint: testHint,
		DebugLog: func(s string) {
			fmt.Println(s)
		},
	})
	if err != nil {
		t.Errorf("NewClient(): %v", err)
		return
	}

	time.Sleep(2 * time.Second)
	c.Close()

	srv.Close()
	wg.Wait()
}
