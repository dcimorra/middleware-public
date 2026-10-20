package api

import (
	"net/http"
	"net/url"

	"github.com/dcimorra/middleware-public/internal/push"
)

// RegisterPush adds the /push routes. s may be nil: 503.
func (s *Server) RegisterPush(p *push.Sender) {
	base := Prefix + "/push"
	need := func() (*push.Sender, error) {
		if p == nil {
			return nil, Errorf(http.StatusServiceUnavailable, "push notifications did not start; see the push.error event")
		}
		return p, nil
	}
	s.Handle("GET", base, "The VAPID key, the kinds and the subscribed devices", func(w http.ResponseWriter, r *http.Request) (any, error) {
		ps, err := need()
		if err != nil {
			return nil, err
		}
		type dev struct {
			Endpoint string `json:"endpoint"`
			Service  string `json:"service"`
			push.Subscription
		}
		var devs []dev
		for _, sub := range ps.Subscriptions() {
			host := ""
			if u, err := url.Parse(sub.Endpoint); err == nil {
				host = u.Host
			}
			sub.Keys.P256dh, sub.Keys.Auth = "", ""
			devs = append(devs, dev{Endpoint: sub.Endpoint, Service: host, Subscription: sub})
		}
		if devs == nil {
			devs = []dev{}
		}
		return map[string]any{"public_key": ps.PublicKey(), "kinds": push.Kinds(), "devices": devs}, nil
	})
	s.Handle("POST", base+"/subscriptions", "Subscribe this device (the browser's PushSubscription, a name, the kinds it wants)", func(w http.ResponseWriter, r *http.Request) (any, error) {
		ps, err := need()
		if err != nil {
			return nil, err
		}
		var sub push.Subscription
		if err := ReadJSON(r, &sub); err != nil {
			return nil, err
		}
		if err := ps.Subscribe(sub); err != nil {
			return nil, Errorf(http.StatusBadRequest, "%v", err)
		}
		return OK, nil
	})
	s.Handle("DELETE", base+"/subscriptions", "Unsubscribe a device: ?endpoint=<its endpoint>", func(w http.ResponseWriter, r *http.Request) (any, error) {
		ps, err := need()
		if err != nil {
			return nil, err
		}
		ep := r.URL.Query().Get("endpoint")
		if ep == "" {
			return nil, Errorf(http.StatusBadRequest, "endpoint: required")
		}
		return OK, ps.Unsubscribe(ep)
	})
	s.Handle("POST", base+"/test", "Send a test notification to every device", func(w http.ResponseWriter, r *http.Request) (any, error) {
		ps, err := need()
		if err != nil {
			return nil, err
		}
		n, err := ps.Send(r.Context(), push.Message{Kind: push.KindTest, Title: "middleware", Body: "Notifications reach this device.", URL: "/#/settings", Tag: "test"})
		out := map[string]any{"sent": n}
		if err != nil {
			out["error"] = err.Error()
		}
		return out, nil
	})
}
