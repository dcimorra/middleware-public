// Package cli is the operator's command line: `middleware site …`,
// `middleware events`, `middleware token …`. It talks to the running
// supervisor through the same API the panel uses, over the unix socket
// when it can and over loopback with a token when it cannot, so there is
// exactly one way to change anything.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/api"
	"github.com/dcimorra/the-middleware/internal/settings"
)

// TokenEnv names the token the client sends when the socket is out of
// reach (another user, another machine through a tunnel).
const TokenEnv = "MIDDLEWARE_TOKEN"

// Client calls the API.
type Client struct {
	// Base is the URL prefix; for the socket it is a placeholder host.
	Base  string
	Token string
	HTTP  *http.Client
	// Via says how the client connects, for messages: "socket" or the URL.
	Via string
}

// ErrNotRunning says nothing answers on the socket or the loopback port.
var ErrNotRunning = errors.New("the supervisor is not running (or this user cannot open data/api.sock)")

// APIError is a non-2xx answer.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return e.Message }

// Connect picks the socket under root when it answers, and otherwise the
// loopback listener with the token from MIDDLEWARE_TOKEN.
func Connect(root string) (*Client, error) {
	sock := api.SocketPath(root)
	if c, err := net.DialTimeout("unix", sock, time.Second); err == nil {
		c.Close()
		return SocketClient(sock), nil
	}
	st, err := settings.Load(root)
	if err != nil {
		return nil, err
	}
	token := os.Getenv(TokenEnv)
	if token == "" {
		return nil, fmt.Errorf("%w; set %s to use http://%s", ErrNotRunning, TokenEnv, st.Listen)
	}
	return &Client{Base: "http://" + st.Listen, Token: token, HTTP: &http.Client{}, Via: "http://" + st.Listen}, nil
}

// SocketClient talks to a unix socket.
func SocketClient(path string) *Client {
	return &Client{
		Base: "http://middleware",
		Via:  "socket",
		HTTP: &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", path)
			},
		}},
	}
}

// Do sends a JSON request and decodes the answer into out (nil to
// discard). A `{"error"}` answer becomes an *APIError.
func (c *Client) Do(method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		switch b := body.(type) {
		case []byte:
			rd = bytes.NewReader(b)
		case string:
			rd = strings.NewReader(b)
		default:
			data, err := json.Marshal(b)
			if err != nil {
				return err
			}
			rd = bytes.NewReader(data)
		}
	}
	resp, err := c.request(context.Background(), method, path, rd)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return apiError(resp.StatusCode, data)
	}
	if out == nil {
		return nil
	}
	if raw, ok := out.(*[]byte); ok {
		*raw = data
		return nil
	}
	return json.Unmarshal(data, out)
}

// Stream opens a request and hands back the body for the caller to read
// as it comes: logs with ?follow=1, the event stream.
func (c *Client) Stream(ctx context.Context, path string) (io.ReadCloser, error) {
	resp, err := c.request(ctx, "GET", path, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, apiError(resp.StatusCode, data)
	}
	return resp.Body, nil
}

func (c *Client) request(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) || strings.Contains(err.Error(), "connection refused") || strings.Contains(err.Error(), "no such file") {
			return nil, fmt.Errorf("%w: %v", ErrNotRunning, err)
		}
		return nil, err
	}
	return resp, nil
}

func apiError(status int, data []byte) error {
	var body struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &body) == nil && body.Error != "" {
		return &APIError{Status: status, Message: body.Error}
	}
	return &APIError{Status: status, Message: fmt.Sprintf("http %d: %s", status, strings.TrimSpace(string(data)))}
}
