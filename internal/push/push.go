// Package push sends web push notifications to the operator's devices:
// VAPID (RFC 8292) to say who sends, and aes128gcm (RFC 8291) so the push
// service carries a message it cannot read. Both are written against the
// standard library, because the core takes no dependencies.
//
// What gets pushed is decided in one place, Notifier: a closed list of
// event types, so a push is never something a caller can make up.
package push

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dcimorra/the-middleware/internal/fsx"
)

var b64 = base64.RawURLEncoding

// Subscription is one device, as the browser's PushManager gives it.
type Subscription struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
	Name    string    `json:"name,omitempty"`
	Created time.Time `json:"created"`
	// Kinds are what this device wants (Kinds()); empty is all of them.
	Kinds []string `json:"kinds,omitempty"`
	// LastError is the last failure to deliver, shown in the panel: a
	// device that silently stopped receiving looks like a quiet house.
	LastError string    `json:"last_error,omitempty"`
	LastSent  time.Time `json:"last_sent,omitempty"`
}

// Message is one notification. The service worker shows Title and Body
// and opens URL on a click; Tag replaces an earlier one with the same tag.
type Message struct {
	Kind  string `json:"kind"`
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
	URL   string `json:"url,omitempty"`
	Tag   string `json:"tag,omitempty"`
}

// Options configure a Sender.
type Options struct {
	DataDir string
	// Subject is the VAPID contact, a mailto: or https: URL push services
	// may use to reach the sender.
	Subject string
	Client  *http.Client
	Now     func() time.Time
}

// Sender keeps the VAPID key and the subscriptions, under data/push.
type Sender struct {
	opts Options
	key  *ecdsa.PrivateKey
	mu   sync.Mutex
}

type keyFile struct {
	D string `json:"d"`
}

// New loads or creates the VAPID key. It is kept: a new key would
// invalidate every subscription, which the browsers tie to it.
func New(opts Options) (*Sender, error) {
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: 30 * time.Second}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Subject == "" {
		opts.Subject = "mailto:operator@localhost"
	}
	s := &Sender{opts: opts}
	var kf keyFile
	err := fsx.ReadJSON(s.keyPath(), &kf)
	switch {
	case err == nil:
		d, derr := b64.DecodeString(kf.D)
		if derr != nil || len(d) != 32 {
			return nil, errors.New("push: the VAPID key is unreadable")
		}
		k := new(ecdsa.PrivateKey)
		k.Curve = elliptic.P256()
		k.D = new(big.Int).SetBytes(d)
		k.X, k.Y = k.Curve.ScalarBaseMult(d)
		s.key = k
	case fsx.IsNotExist(err):
		k, gerr := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if gerr != nil {
			return nil, gerr
		}
		d := make([]byte, 32)
		k.D.FillBytes(d)
		if err := fsx.WriteJSON(s.keyPath(), keyFile{D: b64.EncodeToString(d)}, 0o600); err != nil {
			return nil, err
		}
		s.key = k
	default:
		return nil, err
	}
	return s, nil
}

func (s *Sender) keyPath() string { return filepath.Join(s.opts.DataDir, "push", "vapid.json") }
func (s *Sender) subsPath() string {
	return filepath.Join(s.opts.DataDir, "push", "subscriptions.json")
}

// PublicKey is the VAPID public key as the browser's applicationServerKey
// wants it: the uncompressed point, base64url.
func (s *Sender) PublicKey() string {
	k, err := s.key.ECDH()
	if err != nil {
		return ""
	}
	return b64.EncodeToString(k.PublicKey().Bytes())
}

func (s *Sender) load() []Subscription {
	var list []Subscription
	_ = fsx.ReadJSON(s.subsPath(), &list)
	return list
}

func (s *Sender) save(list []Subscription) error {
	if list == nil {
		list = []Subscription{}
	}
	return fsx.WriteJSON(s.subsPath(), list, 0o600)
}

// Subscriptions lists the devices.
func (s *Sender) Subscriptions() []Subscription {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

// Subscribe adds or updates a device, keyed by its endpoint.
func (s *Sender) Subscribe(sub Subscription) error {
	u, err := url.Parse(sub.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("push: the endpoint must be an https URL")
	}
	if p, err := b64.DecodeString(strings.TrimRight(sub.Keys.P256dh, "=")); err != nil || len(p) != 65 || p[0] != 4 {
		return errors.New("push: keys.p256dh is not a P-256 point")
	}
	if a, err := b64.DecodeString(strings.TrimRight(sub.Keys.Auth, "=")); err != nil || len(a) != 16 {
		return errors.New("push: keys.auth is not 16 bytes")
	}
	for _, k := range sub.Kinds {
		if !validKind(k) {
			return fmt.Errorf("push: %q is not a kind of notification", k)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.load()
	sub.Created = s.opts.Now().UTC()
	for i := range list {
		if list[i].Endpoint == sub.Endpoint {
			sub.Created = list[i].Created
			list[i] = sub
			return s.save(list)
		}
	}
	return s.save(append(list, sub))
}

// Unsubscribe removes a device.
func (s *Sender) Unsubscribe(endpoint string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.load()
	out := list[:0]
	for _, x := range list {
		if x.Endpoint != endpoint {
			out = append(out, x)
		}
	}
	return s.save(out)
}

// Send delivers m to every device that wants its kind, and returns how
// many took it. A device the push service says is gone (404, 410) is
// forgotten; any other failure is kept on the device for the panel.
func (s *Sender) Send(ctx context.Context, m Message) (int, error) {
	payload, err := json.Marshal(m)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	list := s.load()
	s.mu.Unlock()
	sent := 0
	var errs []string
	gone := map[string]bool{}
	results := map[string]string{}
	for _, sub := range list {
		if !wants(sub, m.Kind) {
			continue
		}
		status, err := s.deliver(ctx, sub, payload, m.Kind)
		switch {
		case err == nil:
			sent++
			results[sub.Endpoint] = ""
		case status == http.StatusNotFound || status == http.StatusGone:
			gone[sub.Endpoint] = true
		default:
			results[sub.Endpoint] = err.Error()
			errs = append(errs, err.Error())
		}
	}
	s.mu.Lock()
	list = s.load()
	out := list[:0]
	now := s.opts.Now().UTC()
	for _, x := range list {
		if gone[x.Endpoint] {
			continue
		}
		if e, ok := results[x.Endpoint]; ok {
			x.LastError = e
			if e == "" {
				x.LastSent = now
			}
		}
		out = append(out, x)
	}
	_ = s.save(out)
	s.mu.Unlock()
	if len(errs) > 0 {
		return sent, errors.New(strings.Join(errs, "; "))
	}
	return sent, nil
}

func wants(sub Subscription, kind string) bool {
	if len(sub.Kinds) == 0 || kind == KindTest {
		return true
	}
	for _, k := range sub.Kinds {
		if k == kind {
			return true
		}
	}
	return false
}

func (s *Sender) deliver(ctx context.Context, sub Subscription, payload []byte, kind string) (int, error) {
	uaPub, _ := b64.DecodeString(strings.TrimRight(sub.Keys.P256dh, "="))
	auth, _ := b64.DecodeString(strings.TrimRight(sub.Keys.Auth, "="))
	body, err := Encrypt(payload, uaPub, auth)
	if err != nil {
		return 0, err
	}
	u, err := url.Parse(sub.Endpoint)
	if err != nil {
		return 0, err
	}
	jwt, err := s.vapid(u.Scheme + "://" + u.Host)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("TTL", "86400")
	urgency := "normal"
	if kind == KindIntrusion {
		urgency = "high"
	}
	req.Header.Set("Urgency", urgency)
	req.Header.Set("Authorization", "vapid t="+jwt+", k="+s.PublicKey())
	resp, err := s.opts.Client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, fmt.Errorf("%s: %d %s", u.Host, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return resp.StatusCode, nil
}

// vapid is the JWT that says who sends (RFC 8292), signed ES256 with the
// raw r||s signature JWS wants, not ASN.1.
func (s *Sender) vapid(aud string) (string, error) {
	head := b64.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, _ := json.Marshal(map[string]any{"aud": aud, "exp": s.opts.Now().Add(12 * time.Hour).Unix(), "sub": s.opts.Subject})
	signing := head + "." + b64.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	r, sv, err := ecdsa.Sign(rand.Reader, s.key, sum[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	sv.FillBytes(sig[32:])
	return signing + "." + b64.EncodeToString(sig), nil
}

// Encrypt is RFC 8291's aes128gcm for one record: an ephemeral ECDH key
// against the browser's, the browser's auth secret mixed in, and the
// header (salt, record size, the ephemeral public key) in front.
func Encrypt(plaintext, uaPublic, authSecret []byte) ([]byte, error) {
	curve := ecdh.P256()
	ua, err := curve.NewPublicKey(uaPublic)
	if err != nil {
		return nil, fmt.Errorf("push: the device's key: %w", err)
	}
	as, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	return encrypt(plaintext, ua, as, authSecret, salt)
}

func encrypt(plaintext []byte, ua *ecdh.PublicKey, as *ecdh.PrivateKey, authSecret, salt []byte) ([]byte, error) {
	shared, err := as.ECDH(ua)
	if err != nil {
		return nil, err
	}
	asPub := as.PublicKey().Bytes()
	keyInfo := append(append([]byte("WebPush: info\x00"), ua.Bytes()...), asPub...)
	prkKey, err := hkdf.Extract(sha256.New, shared, authSecret)
	if err != nil {
		return nil, err
	}
	ikm, err := hkdf.Expand(sha256.New, prkKey, string(keyInfo), 32)
	if err != nil {
		return nil, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	// One record: the payload, then the delimiter 0x02 that says it is
	// the last one. 4096 bytes is what push services accept.
	const rs = 4096
	if len(plaintext)+1+16 > rs-86 {
		return nil, errors.New("push: the message is too long for one record")
	}
	ct := gcm.Seal(nil, nonce, append(append([]byte(nil), plaintext...), 0x02), nil)
	head := make([]byte, 0, 86+len(ct))
	head = append(head, salt...)
	head = binary.BigEndian.AppendUint32(head, rs)
	head = append(head, byte(len(asPub)))
	head = append(head, asPub...)
	return append(head, ct...), nil
}
