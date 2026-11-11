package signalr

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

// fakeHub is a SignalR hub that sends the given frames after the handshake.
func fakeHub(t *testing.T, frames []string, seen chan<- *http.Request) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/signalr/aggregate/negotiate", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Query().Get("negotiateVersion") != "1" || r.URL.Query().Get("feeds") != "shoko,file" {
			http.Error(w, "bad negotiate", http.StatusBadRequest)
			return
		}
		if r.Header.Get("Apikey") != "secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"negotiateVersion":1,"connectionId":"c","connectionToken":"tok","availableTransports":[{"transport":"WebSockets","transferFormats":["Text"]}]}`))
	})
	mux.Handle("/signalr/aggregate", websocket.Server{
		Handshake: func(*websocket.Config, *http.Request) error { return nil },
		Handler: func(ws *websocket.Conn) {
			request := ws.Request()
			if seen != nil {
				seen <- request
			}
			var handshake string
			if err := websocket.Message.Receive(ws, &handshake); err != nil {
				return
			}
			if !strings.HasPrefix(handshake, `{"protocol":"json","version":1}`) {
				return
			}
			_ = websocket.Message.Send(ws, "{}\x1e")
			for _, frame := range frames {
				if err := websocket.Message.Send(ws, frame); err != nil {
					return
				}
			}
			// Keep the connection open until the client goes.
			var ignored string
			for websocket.Message.Receive(ws, &ignored) == nil {
			}
		},
	})
	return httptest.NewServer(mux)
}

func dialHub(t *testing.T, server *httptest.Server) *Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := Dial(ctx, Config{
		BaseURL: server.URL,
		HubPath: "/signalr/aggregate",
		Query:   url.Values{"feeds": {"shoko,file"}},
		Header:  http.Header{"Apikey": {"secret"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

func TestDialAndReceiveInvocations(t *testing.T) {
	seen := make(chan *http.Request, 1)
	server := fakeHub(t, []string{
		// A ping, then two messages in one frame.
		`{"type":6}` + "\x1e" + `{"type":1,"target":"file:deleted","arguments":[{"FileID":5}]}` + "\x1e" + `{"type":1,"target":"release:saved","argu`,
		// The rest of a message split across frames.
		`ments":[{"FileID":6}]}` + "\x1e",
		`{"type":7,"error":"bye"}` + "\x1e",
	}, seen)
	defer server.Close()
	conn := dialHub(t, server)
	defer conn.Close()

	request := <-seen
	if request.URL.Query().Get("id") != "tok" || request.Header.Get("Apikey") != "secret" {
		t.Fatalf("websocket request = %s, headers %v", request.URL, request.Header)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, want := range []struct {
		target string
		fileID int
	}{{"file:deleted", 5}, {"release:saved", 6}} {
		invocation, err := conn.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var argument struct {
			FileID int `json:"FileID"`
		}
		if len(invocation.Arguments) != 1 || json.Unmarshal(invocation.Arguments[0], &argument) != nil {
			t.Fatalf("arguments = %s", invocation.Arguments)
		}
		if invocation.Target != want.target || argument.FileID != want.fileID {
			t.Fatalf("invocation = %s %d, want %s %d", invocation.Target, argument.FileID, want.target, want.fileID)
		}
	}
	if _, err := conn.Next(ctx); err == nil || !strings.Contains(err.Error(), "bye") {
		t.Fatalf("Next after close message = %v", err)
	}
}

func TestNextEndsWithContext(t *testing.T) {
	server := fakeHub(t, nil, nil)
	defer server.Close()
	conn := dialHub(t, server)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	if _, err := conn.Next(ctx); err != context.Canceled {
		t.Fatalf("Next after cancel = %v", err)
	}
}

func TestDialReportsRefusedNegotiate(t *testing.T) {
	server := fakeHub(t, nil, nil)
	defer server.Close()
	_, err := Dial(context.Background(), Config{
		BaseURL: server.URL,
		HubPath: "/signalr/aggregate",
		Query:   url.Values{"feeds": {"shoko,file"}},
		Header:  http.Header{"Apikey": {"wrong"}},
	})
	var httpErr *HTTPError
	if err == nil || !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Dial with a wrong key = %v", err)
	}
}
