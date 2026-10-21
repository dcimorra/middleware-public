package nodes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// APIPrefix is where every node's API lives.
const APIPrefix = "/api/v1"

// transport is shared by the calls and the proxy. No overall timeout: the
// proxy carries the event stream and a chat's, which last as long as the
// tab is open; a call that must be short brings its own context.
var transport = &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	TLSHandshakeTimeout:   10 * time.Second,
	MaxIdleConnsPerHost:   8,
	IdleConnTimeout:       90 * time.Second,
	ExpectContinueTimeout: time.Second,
}

// Call does one JSON request to a node's API. path is under /api/v1. An
// answer that is not 2xx is an error carrying the node's own message.
func Call(ctx context.Context, base, token, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+APIPrefix+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", base, unwrapURL(err))
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if res.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return &RemoteError{Status: res.StatusCode, Message: e.Error}
		}
		return &RemoteError{Status: res.StatusCode, Message: strings.TrimSpace(firstLine(string(raw)))}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		// A 200 that is not JSON is the panel's page: the address is
		// right, but something in front of it is answering, or it is not
		// a middleware at all.
		return fmt.Errorf("%s did not answer as a middleware API", base)
	}
	return nil
}

// RemoteError is a node's own refusal, with its status.
type RemoteError struct {
	Status  int
	Message string
}

func (e *RemoteError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("the node answered %d", e.Status)
	}
	return e.Message
}

func unwrapURL(err error) error {
	var u *url.Error
	if errors.As(err, &u) {
		return u.Err
	}
	return err
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// Proxy forwards a request to a peer's API: rest is the path after
// /api/v1. What the browser authenticated with here (the cookie, the CSRF
// header) is not sent: on the other side this node is a token, and the
// cookie of one panel means nothing to another. The answer streams back
// as it comes, which is what keeps the event stream and a chat's turn
// live through the hop.
func Proxy(p Peer, rest string) (http.Handler, error) {
	target, err := url.Parse(p.URL)
	if err != nil {
		return nil, err
	}
	rp := &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			out := pr.Out
			out.URL.Scheme, out.URL.Host = target.Scheme, target.Host
			out.URL.Path, out.URL.RawPath = APIPrefix+"/"+strings.TrimPrefix(rest, "/"), ""
			out.URL.RawQuery = pr.In.URL.RawQuery
			out.Host = target.Host
			out.Header.Del("Cookie")
			out.Header.Del("Authorization")
			out.Header.Del("X-Requested-With")
			out.Header.Del("Origin")
			out.Header.Del("Referer")
			for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip", "Forwarded", "Cf-Connecting-Ip", "Cf-Ipcountry", "True-Client-Ip"} {
				out.Header.Del(h)
			}
			out.Header.Set("Authorization", "Bearer "+p.Token)
		},
		ModifyResponse: func(res *http.Response) error {
			// A session of the other panel must never land in this one's
			// cookie jar.
			res.Header.Del("Set-Cookie")
			// The other node refusing this one's token is not the
			// operator's session ending here: passed on as a 401 it would
			// log them out of this panel. It is a broken link, and says so.
			if res.StatusCode == http.StatusUnauthorized {
				res.Body.Close()
				msg, _ := json.Marshal(map[string]string{"error": fmt.Sprintf("node %s refused this node's token: the link was undone on its side; unlink and link again", p.Name)})
				msg = append(msg, '\n')
				res.StatusCode, res.Status = http.StatusBadGateway, "502 Bad Gateway"
				res.Header = http.Header{"Content-Type": {"application/json; charset=utf-8"}}
				res.Body = io.NopCloser(bytes.NewReader(msg))
				res.ContentLength = int64(len(msg))
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				return
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			msg, _ := json.Marshal(map[string]string{"error": fmt.Sprintf("node %s (%s) cannot be reached: %v", p.Name, p.URL, unwrapURL(err))})
			_, _ = w.Write(append(msg, '\n'))
		},
	}
	return rp, nil
}
