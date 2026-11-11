// Package signalr is a minimal client for an ASP.NET Core SignalR hub, enough
// to listen to the invocations a server pushes. It speaks the JSON hub
// protocol over WebSockets only, and never invokes hub methods.
package signalr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

// recordSeparator ends every message of the JSON hub protocol.
const recordSeparator = 0x1e

const (
	messageInvocation = 1
	messagePing       = 6
	messageClose      = 7
)

const (
	// pingInterval is how often the client tells the server it is alive. The
	// server drops a client it has not heard from in 30 seconds by default.
	pingInterval = 15 * time.Second
	// serverTimeout is how long the server may stay silent. It pings every
	// 15 seconds by default.
	serverTimeout = time.Minute
	// maxMessageBytes bounds a single message, which for an event is tiny.
	maxMessageBytes = 4 << 20
)

// Config names a hub and how to authenticate to it.
type Config struct {
	// BaseURL is the server's http or https URL, without the hub path.
	BaseURL string
	// HubPath is the hub's path under BaseURL, such as /signalr/aggregate.
	HubPath string
	// Query is added to the hub URL, such as feeds=shoko,file.
	Query url.Values
	// Header is sent with the negotiate request and the WebSocket upgrade.
	Header http.Header
	// HTTPClient sends the negotiate request.
	HTTPClient *http.Client
}

// Invocation is a hub method the server called on the client.
type Invocation struct {
	Target    string
	Arguments []json.RawMessage
}

// Conn is an open hub connection.
type Conn struct {
	ws        *websocket.Conn
	buffer    []byte
	writeMu   sync.Mutex
	closeOnce sync.Once
	done      chan struct{}
}

// Dial negotiates with the hub, opens a WebSocket and completes the protocol
// handshake.
func Dial(ctx context.Context, cfg Config) (*Conn, error) {
	base, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") {
		return nil, errors.New("signalr: base URL must be an http or https URL")
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	hub := *base
	hub.Path = strings.TrimRight(base.Path, "/") + cfg.HubPath
	query := url.Values{}
	for key, values := range cfg.Query {
		query[key] = append([]string(nil), values...)
	}

	token, err := negotiate(ctx, httpClient, hub, query, cfg.Header)
	if err != nil {
		return nil, err
	}

	socketURL := hub
	if hub.Scheme == "https" {
		socketURL.Scheme = "wss"
	} else {
		socketURL.Scheme = "ws"
	}
	if token != "" {
		query.Set("id", token)
	}
	socketURL.RawQuery = query.Encode()
	origin := *base
	origin.Path, origin.RawQuery = "", ""
	wsConfig, err := websocket.NewConfig(socketURL.String(), origin.String())
	if err != nil {
		return nil, fmt.Errorf("signalr: websocket config: %w", err)
	}
	wsConfig.Header = cfg.Header.Clone()
	if wsConfig.Header == nil {
		wsConfig.Header = http.Header{}
	}
	ws, err := wsConfig.DialContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("signalr: open websocket: %w", err)
	}
	ws.MaxPayloadBytes = maxMessageBytes
	conn := &Conn{ws: ws, done: make(chan struct{})}
	if err := conn.handshake(ctx); err != nil {
		_ = ws.Close()
		return nil, err
	}
	go conn.keepAlive()
	return conn, nil
}

func negotiate(ctx context.Context, client *http.Client, hub url.URL, query url.Values, header http.Header) (string, error) {
	target := hub
	target.Path += "/negotiate"
	negotiateQuery := url.Values{}
	for key, values := range query {
		negotiateQuery[key] = values
	}
	negotiateQuery.Set("negotiateVersion", "1")
	target.RawQuery = negotiateQuery.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), nil)
	if err != nil {
		return "", fmt.Errorf("signalr: negotiate request: %w", err)
	}
	for key, values := range header {
		request.Header[key] = append([]string(nil), values...)
	}
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("signalr: negotiate: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("signalr: read negotiate response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", &HTTPError{StatusCode: response.StatusCode}
	}
	var result struct {
		ConnectionID        string `json:"connectionId"`
		ConnectionToken     string `json:"connectionToken"`
		Error               string `json:"error"`
		URL                 string `json:"url"`
		AvailableTransports []struct {
			Transport string `json:"transport"`
		} `json:"availableTransports"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("signalr: decode negotiate response: %w", err)
	}
	if result.Error != "" {
		return "", fmt.Errorf("signalr: negotiate refused: %s", result.Error)
	}
	if result.URL != "" {
		return "", errors.New("signalr: the server redirects to another service, which is not supported")
	}
	webSockets := false
	for _, transport := range result.AvailableTransports {
		if transport.Transport == "WebSockets" {
			webSockets = true
		}
	}
	if !webSockets {
		return "", errors.New("signalr: the server does not offer WebSockets")
	}
	if result.ConnectionToken != "" {
		return result.ConnectionToken, nil
	}
	return result.ConnectionID, nil
}

// HTTPError is a negotiate request the server refused.
type HTTPError struct {
	StatusCode int
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("signalr: negotiate returned HTTP %d", e.StatusCode)
}

func (c *Conn) handshake(ctx context.Context) error {
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.ws.SetDeadline(deadline)
	} else {
		_ = c.ws.SetDeadline(time.Now().Add(30 * time.Second))
	}
	defer c.ws.SetDeadline(time.Time{})
	if err := c.write([]byte(`{"protocol":"json","version":1}`)); err != nil {
		return fmt.Errorf("signalr: send handshake: %w", err)
	}
	message, err := c.readMessage()
	if err != nil {
		return fmt.Errorf("signalr: read handshake: %w", err)
	}
	var response struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(message, &response); err != nil {
		return fmt.Errorf("signalr: decode handshake: %w", err)
	}
	if response.Error != "" {
		return fmt.Errorf("signalr: handshake refused: %s", response.Error)
	}
	return nil
}

// Next returns the next invocation from the server. It fails when the
// connection closes, the server goes silent, or ctx ends; the connection is
// then unusable.
func (c *Conn) Next(ctx context.Context) (Invocation, error) {
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	for {
		_ = c.ws.SetReadDeadline(time.Now().Add(serverTimeout))
		message, err := c.readMessage()
		if err != nil {
			if ctx.Err() != nil {
				return Invocation{}, ctx.Err()
			}
			return Invocation{}, err
		}
		var envelope struct {
			Type      int               `json:"type"`
			Target    string            `json:"target"`
			Arguments []json.RawMessage `json:"arguments"`
			Error     string            `json:"error"`
		}
		if err := json.Unmarshal(message, &envelope); err != nil {
			return Invocation{}, fmt.Errorf("signalr: decode message: %w", err)
		}
		switch envelope.Type {
		case messageInvocation:
			return Invocation{Target: envelope.Target, Arguments: envelope.Arguments}, nil
		case messageClose:
			if envelope.Error != "" {
				return Invocation{}, fmt.Errorf("signalr: server closed the connection: %s", envelope.Error)
			}
			return Invocation{}, errors.New("signalr: server closed the connection")
		}
		// Pings and message types a listener has no use for.
	}
}

// Close closes the connection. It is safe to call more than once.
func (c *Conn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		close(c.done)
		err = c.ws.Close()
	})
	return err
}

func (c *Conn) keepAlive() {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			if err := c.write([]byte(`{"type":6}`)); err != nil {
				_ = c.Close()
				return
			}
		}
	}
}

func (c *Conn) write(message []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return websocket.Message.Send(c.ws, string(append(message, recordSeparator)))
}

// readMessage returns the next complete message. A frame may hold several
// messages, or part of one.
func (c *Conn) readMessage() ([]byte, error) {
	for {
		if index := bytes.IndexByte(c.buffer, recordSeparator); index >= 0 {
			message := bytes.Clone(c.buffer[:index])
			c.buffer = c.buffer[index+1:]
			return message, nil
		}
		if len(c.buffer) > maxMessageBytes {
			return nil, errors.New("signalr: message exceeds 4 MiB")
		}
		var frame []byte
		if err := websocket.Message.Receive(c.ws, &frame); err != nil {
			return nil, err
		}
		c.buffer = append(c.buffer, frame...)
	}
}
