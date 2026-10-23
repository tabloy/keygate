package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/http/httpproxy"
	"golang.org/x/net/idna"

	"github.com/tabloy/keygate/internal/middleware"
	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

type WebhookService struct {
	store        *store.Store
	logger       *slog.Logger
	client       *http.Client
	proxyFor     func(*url.URL) (*url.URL, error)
	proxyAddrs   map[string]bool // host:port the dialer exempts from the guard
	lookup       func(ctx context.Context, host string) ([]net.IPAddr, error)
	proxyTLS     *tls.Config // base config for TLS to an https proxy; tests inject roots
	httpTimeout  time.Duration
	maxRetries   int
	allowPrivate bool
	sem          chan struct{} // concurrency limiter
}

// ErrPrivateTarget is returned when a webhook URL points at a loopback,
// private or link-local address and WEBHOOK_ALLOW_PRIVATE is not set.
var ErrPrivateTarget = errors.New("webhook target is a private address; set WEBHOOK_ALLOW_PRIVATE=true to allow it")

// ErrUnresolvableTarget is returned for a proxied delivery whose host
// this process cannot resolve, so it cannot be classified.
var ErrUnresolvableTarget = errors.New("webhook target could not be resolved locally")

func NewWebhookService(s *store.Store, logger *slog.Logger, httpTimeout time.Duration, maxRetries int, allowPrivate bool) *WebhookService {
	client, proxyFor, proxyAddrs := newWebhookClient(httpTimeout, allowPrivate)
	svc := &WebhookService{
		store:        s,
		logger:       logger,
		client:       client,
		proxyFor:     proxyFor,
		proxyAddrs:   proxyAddrs,
		lookup:       net.DefaultResolver.LookupIPAddr,
		httpTimeout:  httpTimeout,
		maxRetries:   maxRetries,
		allowPrivate: allowPrivate,
		sem:          make(chan struct{}, 20), // max 20 concurrent deliveries
	}
	// A public receiver may answer with a redirect to a private
	// address. Direct hops are caught by the dialer, but through a
	// proxy only the proxy is ever dialed — so classify every hop
	// here, the same way the first request was.
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return svc.guardTarget(req.Context(), req.URL)
	}
	return svc
}

// newWebhookClient builds the delivery client. The non-public-address
// check runs in the dialer's Control hook, i.e. on the resolved IP
// right before connect, so it also covers hostnames that resolve to
// an internal address and redirects the receiver may send us on.
//
// HTTP_PROXY / HTTPS_PROXY / NO_PROXY are honoured like the default
// client. A dial to the proxy itself is exempt — the proxy is the
// operator's egress point and routinely sits on a private address —
// which means a proxied target is never dialed by this process.
// guardTarget covers that case by resolving and classifying the
// target host before the request is sent.
func newWebhookClient(timeout time.Duration, allowPrivate bool) (*http.Client, func(*url.URL) (*url.URL, error), map[string]bool) {
	proxyCfg := httpproxy.FromEnvironment()
	proxyFor := proxyCfg.ProxyFunc()
	proxyAddrs := proxyDialAddrs(proxyCfg.HTTPProxy, proxyCfg.HTTPSProxy)

	plain := &net.Dialer{Timeout: timeout}
	guarded := &net.Dialer{Timeout: timeout}
	guarded.Control = func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		if isNonPublicIP(net.ParseIP(host)) {
			return ErrPrivateTarget
		}
		return nil
	}
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		if allowPrivate || proxyAddrs[addr] {
			return plain.DialContext(ctx, network, addr)
		}
		return guarded.DialContext(ctx, network, addr)
	}

	// Start from DefaultTransport so idle-connection reaping, HTTP/2
	// and the other defaults stay; only the routing bits change.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = func(r *http.Request) (*url.URL, error) { return proxyFor(r.URL) }
	transport.DialContext = dial
	transport.TLSHandshakeTimeout = timeout
	transport.MaxIdleConnsPerHost = 20 // matches the delivery semaphore
	client := &http.Client{Timeout: timeout, Transport: transport}
	return client, proxyFor, proxyAddrs
}

// guardTarget is the pre-send half of the target check, for requests
// that will leave through a proxy: the proxy does the dialing, so the
// Control hook never sees the target. Resolve it here and apply the
// same rule. Direct requests return early — the dialer handles them
// on the actual connect address, which is the stronger check.
func (s *WebhookService) guardTarget(ctx context.Context, u *url.URL) error {
	if s.allowPrivate || s.proxyFor == nil {
		return nil
	}
	proxy, err := s.proxyFor(u)
	if err != nil {
		return err
	}
	if proxy == nil && !s.proxyAddrs[canonicalAddr(u)] {
		return nil
	}
	// Either the request goes through the proxy (the proxy dials the
	// target, not us), or it goes directly to the proxy's own
	// host:port — which the dialer exempts so proxied traffic can
	// reach it. Both cases need the classification done here.
	_, err = s.publicAddrs(ctx, u.Hostname())
	return err
}

// publicAddrs resolves host and returns its addresses, every one of
// them verified public. A literal IP is returned as-is.
func (s *WebhookService) publicAddrs(ctx context.Context, host string) ([]net.IP, error) {
	if strings.EqualFold(host, "localhost") {
		return nil, ErrPrivateTarget
	}
	if ip := net.ParseIP(host); ip != nil {
		if isNonPublicIP(ip) {
			return nil, ErrPrivateTarget
		}
		return []net.IP{ip}, nil
	}
	lookup := s.lookup
	if lookup == nil {
		lookup = net.DefaultResolver.LookupIPAddr
	}
	addrs, err := lookup(ctx, host)
	if err != nil {
		// Deliberately closed: letting an unresolvable name through
		// would hand an attacker a split-DNS bypass (NXDOMAIN for us,
		// metadata IP for the proxy). Deployments where only the
		// proxy can resolve names have to say so explicitly.
		return nil, fmt.Errorf("%w: cannot resolve %q from this host (%v); if the proxy is meant to resolve webhook hosts, set WEBHOOK_ALLOW_PRIVATE=true to delegate target policy to it",
			ErrUnresolvableTarget, host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("%w: %q has no addresses", ErrUnresolvableTarget, host)
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		if isNonPublicIP(a.IP) {
			return nil, ErrPrivateTarget
		}
		ips = append(ips, a.IP)
	}
	return ips, nil
}

// send performs one delivery request.
//
// Direct requests are guarded by the dialer on the address it actually
// connects to. A proxied request is different: the proxy resolves the
// hostname itself, so a split-horizon or attacker-controlled name can
// answer "public" to us and "metadata service" to the proxy. Checking
// our own resolution therefore proves nothing about where the proxy
// will connect. The fix is to not let the proxy resolve at all: the
// request goes out with the validated IP in the URL (that is what a
// proxy connects to), the original hostname in the Host header and as
// TLS ServerName, so routing and certificate checks are unchanged.
// Redirects are not followed on this path — each hop would need the
// same pinning against a different host — so a 3xx is returned as the
// receiver's answer and treated like any other non-2xx.
func (s *WebhookService) send(ctx context.Context, req *http.Request) (*http.Response, error) {
	if s.allowPrivate || s.proxyFor == nil {
		return s.client.Do(req)
	}
	proxy, err := s.proxyFor(req.URL)
	if err != nil {
		return nil, err
	}
	rctx, cancel := context.WithTimeout(ctx, s.httpTimeout)
	defer cancel()
	if proxy == nil {
		// Only the direct-to-proxy-address case needs help here; the
		// dialer covers every other direct target.
		if err := s.guardTarget(rctx, req.URL); err != nil {
			return nil, err
		}
		return s.client.Do(req)
	}

	host := req.URL.Hostname()
	ips, err := s.publicAddrs(rctx, host)
	if err != nil {
		return nil, err
	}
	port := req.URL.Port()
	if port == "" {
		port = "80"
		if req.URL.Scheme == "https" {
			port = "443"
		}
	}
	pinned := req.Clone(ctx)
	ipPort := net.JoinHostPort(ips[0].String(), port)
	pinned.URL.Host = ipPort
	pinned.Host = req.URL.Host
	if req.URL.Scheme == "http" {
		// For plain HTTP a proxy connects to whatever host is in the
		// absolute request-URI, and net/http builds that from the Host
		// header — the hostname again. Opaque lets us write the
		// request line ourselves: IP in the URI, name in Host.
		// (HTTPS needs nothing here: CONNECT targets URL.Host, and the
		// tunnelled request uses origin-form with the Host header.)
		path := req.URL.EscapedPath()
		if path == "" {
			path = "/"
		}
		pinned.URL.Opaque = "//" + ipPort + path // RequestURI appends the query itself
	}

	transport, ok := s.client.Transport.(*http.Transport)
	if !ok {
		return nil, errors.New("webhook client transport is not an *http.Transport")
	}
	pinnedTransport := transport.Clone()
	pinnedTransport.TLSClientConfig = &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	defer pinnedTransport.CloseIdleConnections()
	if proxy.Scheme == "https" {
		// http.Transport uses TLSClientConfig for the proxy handshake
		// too, and ServerName above is the target's — the proxy's
		// certificate would be checked against the wrong name. Hand
		// the transport a plain-http view of the proxy and do the
		// proxy TLS ourselves when the dialer reaches its address.
		proxyAddr := canonicalAddr(proxy)
		plainProxy := *proxy
		plainProxy.Scheme = "http"
		plainProxy.Host = proxyAddr
		pinnedTransport.Proxy = func(*http.Request) (*url.URL, error) { return &plainProxy, nil }
		baseDial := pinnedTransport.DialContext
		proxyTLS := s.proxyTLS
		if proxyTLS == nil {
			proxyTLS = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		proxyTLS = proxyTLS.Clone()
		proxyTLS.ServerName = proxy.Hostname()
		pinnedTransport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := baseDial(ctx, network, addr)
			if err != nil || addr != proxyAddr {
				return conn, err
			}
			tc := tls.Client(conn, proxyTLS)
			if err := tc.HandshakeContext(ctx); err != nil {
				conn.Close()
				return nil, fmt.Errorf("tls to proxy %s: %w", proxy.Host, err)
			}
			return tc, nil
		}
	}
	client := &http.Client{
		Timeout:   s.httpTimeout,
		Transport: pinnedTransport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return client.Do(pinned)
}

// canonicalAddr is host:port with the scheme's default port filled in,
// matching what the transport hands to DialContext.
func canonicalAddr(u *url.URL) string {
	host := u.Hostname()
	// net/http dials the IDNA (punycode) form; match it so a proxy with
	// a non-ASCII hostname still hits the exemption.
	if ascii, err := idna.Lookup.ToASCII(host); err == nil {
		host = ascii
	}
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https":
			port = "443"
		case "socks5":
			port = "1080"
		default:
			port = "80"
		}
	}
	return net.JoinHostPort(host, port)
}

// proxyDialAddrs returns the host:port strings the transport will dial
// for the configured proxies, in the same shape DialContext receives
// them (default port filled in).
func proxyDialAddrs(raws ...string) map[string]bool {
	set := map[string]bool{}
	for _, raw := range raws {
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			// httpproxy accepts a bare host:port and treats it as http.
			if u, err = url.Parse("http://" + raw); err != nil {
				continue
			}
		}
		set[canonicalAddr(u)] = true
	}
	return set
}

// nonPublicNets is everything a webhook must not be delivered to:
// loopback, RFC 1918, CGNAT (100.64/10 — cloud metadata services live
// there too), link-local, the IETF/benchmark/documentation blocks,
// class E, broadcast, and the IPv6 equivalents including the ranges
// that embed an IPv4 address (NAT64, 6to4). net.IP.IsPrivate alone
// only knows RFC 1918 and fc00::/7.
var nonPublicNets = func() []*net.IPNet {
	cidrs := []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
		"169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24",
		"192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24",
		"224.0.0.0/4", "240.0.0.0/4", "255.255.255.255/32",
		"::/128", "::1/128", "64:ff9b::/96", "100::/64", "2001::/32",
		"2001:db8::/32", "2002::/16", "fc00::/7", "fe80::/10", "ff00::/8",
	}
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic("webhook: bad cidr " + c)
		}
		nets = append(nets, n)
	}
	return nets
}()

func isNonPublicIP(ip net.IP) bool {
	if ip == nil {
		return true // unparseable: fail closed
	}
	// A v4-mapped v6 address is checked as its v4 self.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	for _, n := range nonPublicNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ValidateTarget rejects a webhook URL whose host is a literal private
// IP or localhost, so the admin gets a 400 when saving instead of a
// failed delivery later. Hostnames are not resolved here — DNS can
// change between save and delivery — the dialer check is the real guard.
func (s *WebhookService) ValidateTarget(rawURL string) error {
	if s.allowPrivate {
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return ErrPrivateTarget
	}
	if ip := net.ParseIP(host); ip != nil && isNonPublicIP(ip) {
		return ErrPrivateTarget
	}
	// 2130706433, 0x7f000001 and 127.1 are 127.0.0.1 to most resolvers,
	// but not to net.ParseIP. No public host name is all digits (a TLD
	// never is), so a numeric host ParseIP cannot read is refused here
	// rather than left to the check at delivery.
	if net.ParseIP(host) == nil && numericHost(host) {
		return ErrPrivateTarget
	}
	return nil
}

// numericHost reports whether every dot-separated part of host is a
// number, decimal or 0x hex: an IPv4 address in one of the shorthand
// forms inet_aton accepts. 0xbeef.cafe is a host name, since cafe is
// not a number.
func numericHost(host string) bool {
	parts := strings.Split(strings.ToLower(strings.TrimSuffix(host, ".")), ".")
	if len(parts) > 4 {
		return false
	}
	for _, p := range parts {
		digits := "0123456789"
		if strings.HasPrefix(p, "0x") {
			p, digits = p[2:], "0123456789abcdef"
		}
		if p == "" || strings.Trim(p, digits) != "" {
			return false
		}
	}
	return true
}

func (s *WebhookService) Dispatch(ctx context.Context, productID, event string, data map[string]any) {
	if err := s.DispatchWithLog(ctx, productID, event, data); err != nil {
		s.logger.Error("webhook dispatch failed", "event", event, "product_id", productID, "error", err)
	}
}

// DispatchWithLog dispatches webhook events and returns any error that occurs during setup.
// Use this when the caller needs to handle or log dispatch failures explicitly.
// inflightHoldOff is the next_retry stamped on a delivery row while its
// first attempt is in flight. ListPendingDeliveries treats a NULL
// next_retry as "due now", so without it the retry loop could pick the
// row up and POST the same delivery id a second time while the first
// attempt is still waiting on the receiver.
func (s *WebhookService) inflightHoldOff() *time.Time {
	t := time.Now().Add(2*s.httpTimeout + 30*time.Second)
	return &t
}

func (s *WebhookService) DispatchWithLog(ctx context.Context, productID, event string, data map[string]any) error {
	webhooks, err := s.store.FindWebhooksForEvent(ctx, productID, event)
	if err != nil {
		return fmt.Errorf("find webhooks: %w", err)
	}
	if len(webhooks) == 0 {
		return nil
	}

	payload := map[string]any{
		"event":     event,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"data":      data,
	}

	for _, wh := range webhooks {
		delivery := &model.WebhookDelivery{
			WebhookID: wh.ID,
			Event:     event,
			Payload:   payload,
			Status:    "pending",
			NextRetry: s.inflightHoldOff(),
		}
		if err := s.store.CreateWebhookDelivery(ctx, delivery); err != nil {
			s.logger.Error("webhook delivery create failed", "webhook_id", wh.ID, "error", err)
			continue
		}
		go func() {
			s.sem <- struct{}{}        // acquire
			defer func() { <-s.sem }() // release
			s.deliver(context.Background(), wh, delivery)
		}()
	}
	return nil
}

// DeliverTest fires a webhook.test event at exactly this webhook and
// waits for the result, so the admin sees the response code instead
// of "dispatched". The async Dispatch path keys on (product, event)
// and would fan the test out to every subscription on the product.
func (s *WebhookService) DeliverTest(ctx context.Context, wh *model.Webhook) (*model.WebhookDelivery, error) {
	if !wh.Active {
		return nil, ErrWebhookInactive
	}
	delivery := &model.WebhookDelivery{
		WebhookID: wh.ID,
		Event:     "webhook.test",
		Status:    "pending",
		NextRetry: s.inflightHoldOff(),
		Payload: map[string]any{
			"event":     "webhook.test",
			"timestamp": time.Now().UTC().Format(time.RFC3339),
			"data": map[string]any{
				"webhook_id": wh.ID,
				"message":    "This is a test delivery from Keygate.",
			},
		},
	}
	if err := s.store.CreateWebhookDelivery(ctx, delivery); err != nil {
		return nil, fmt.Errorf("create delivery: %w", err)
	}
	// Same concurrency cap as async deliveries; a burst of test
	// clicks against a slow receiver must not open unbounded
	// connections.
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	s.deliver(ctx, wh, delivery)
	return delivery, nil
}

func (s *WebhookService) deliver(ctx context.Context, wh *model.Webhook, delivery *model.WebhookDelivery) {
	body, _ := json.Marshal(delivery.Payload)
	sig := signPayload(body, wh.Secret)

	req, err := http.NewRequestWithContext(ctx, "POST", wh.URL, bytes.NewReader(body))
	if err != nil {
		s.failDelivery(ctx, delivery, 0, err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Keygate-Event", delivery.Event)
	req.Header.Set("X-Keygate-Signature", "sha256="+sig)
	req.Header.Set("X-Keygate-Delivery", delivery.ID)

	resp, err := s.send(ctx, req)
	if err != nil {
		s.failDelivery(ctx, delivery, 0, err.Error())
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	delivery.ResponseCode = resp.StatusCode
	delivery.ResponseBody = string(respBody)
	delivery.Attempts++

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		now := time.Now()
		delivery.Status = "delivered"
		delivery.DeliveredAt = &now
		middleware.WebhookDeliveries.WithLabelValues("delivered").Inc()
	} else {
		s.scheduleRetry(delivery)
	}
	_ = s.store.UpdateWebhookDelivery(ctx, delivery)
}

func (s *WebhookService) scheduleRetry(d *model.WebhookDelivery) {
	if d.Attempts >= s.maxRetries {
		d.Status = "failed"
		middleware.WebhookDeliveries.WithLabelValues("failed").Inc()
		return
	}
	backoff := time.Duration(1<<uint(d.Attempts)) * 30 * time.Second
	next := time.Now().Add(backoff)
	d.NextRetry = &next
	d.Status = "pending"
	middleware.WebhookDeliveries.WithLabelValues("retrying").Inc()
}

func (s *WebhookService) failDelivery(ctx context.Context, d *model.WebhookDelivery, code int, body string) {
	d.Attempts++
	d.ResponseCode = code
	d.ResponseBody = body
	s.scheduleRetry(d)
	_ = s.store.UpdateWebhookDelivery(ctx, d)
}

// ErrWebhookDeliveryNotResendable is returned by Redispatch when the
// target delivery exists but the parent webhook has been deleted or
// disabled. Surfaces as 409 so the admin UI can disable the button
// instead of silently failing.
var ErrWebhookDeliveryNotResendable = fmt.Errorf("webhook delivery is not resendable")

// ErrWebhookInactive is returned when a test delivery is requested for
// a disabled webhook; the async paths simply never select those.
var ErrWebhookInactive = errors.New("webhook is disabled")

// Redispatch fires a fresh delivery using the payload of an existing
// one. Industry-standard "resend" behaviour: the receiver sees the
// SAME `data` (so its idempotency dedup still works) but a new
// `X-Keygate-Delivery` header and a new row in the deliveries table
// — so retries, response codes, and timestamps are tracked
// independently of the original attempt.
//
// Returns the new delivery on success. Caller should audit-log the
// admin action with both delivery IDs.
func (s *WebhookService) Redispatch(ctx context.Context, deliveryID string) (*model.WebhookDelivery, error) {
	orig, err := s.store.FindWebhookDeliveryByID(ctx, deliveryID)
	if err != nil {
		return nil, err
	}
	wh, err := s.store.FindWebhookByID(ctx, orig.WebhookID)
	if err != nil {
		return nil, ErrWebhookDeliveryNotResendable
	}
	if !wh.Active {
		return nil, ErrWebhookDeliveryNotResendable
	}
	fresh := &model.WebhookDelivery{
		WebhookID: wh.ID,
		Event:     orig.Event,
		Payload:   orig.Payload, // byte-identical replay
		Status:    "pending",
		NextRetry: s.inflightHoldOff(),
	}
	if err := s.store.CreateWebhookDelivery(ctx, fresh); err != nil {
		return nil, err
	}
	go func() {
		s.sem <- struct{}{}
		defer func() { <-s.sem }()
		s.deliver(context.Background(), wh, fresh)
	}()
	return fresh, nil
}

func (s *WebhookService) ProcessRetries(ctx context.Context) {
	deliveries, err := s.store.ListPendingDeliveries(ctx, 50)
	if err != nil || len(deliveries) == 0 {
		return
	}
	for _, d := range deliveries {
		wh, err := s.store.FindWebhookByID(ctx, d.WebhookID)
		if err != nil || !wh.Active {
			// Webhook gone or disabled: nothing will ever accept this
			// delivery, so stop re-listing it every tick.
			d.Status = "failed"
			d.NextRetry = nil
			_ = s.store.UpdateWebhookDelivery(ctx, d)
			continue
		}
		// Claim the row before the goroutine (which may sit on the
		// semaphore past the next tick) so the next ProcessRetries does
		// not list it again and POST the same delivery id twice.
		d.NextRetry = s.inflightHoldOff()
		if err := s.store.UpdateWebhookDelivery(ctx, d); err != nil {
			continue
		}
		go func(wh *model.Webhook, d *model.WebhookDelivery) {
			s.sem <- struct{}{}
			defer func() { <-s.sem }()
			s.deliver(context.Background(), wh, d)
		}(wh, d)
	}
}

func (s *WebhookService) StartRetryLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.ProcessRetries(ctx)
		}
	}
}

func GenerateWebhookSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func signPayload(payload []byte, secret string) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write(payload)
	return hex.EncodeToString(h.Sum(nil))
}
