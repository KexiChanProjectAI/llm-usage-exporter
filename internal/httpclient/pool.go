// Package httpclient keeps one HTTP client per outbound proxy.
package httpclient

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Pool hands out one *http.Client per proxy URL ("" = direct) so that
// connections to the same upstream through the same proxy are reused.
type Pool struct {
	timeout time.Duration
	mu      sync.Mutex
	clients map[string]*http.Client
}

func NewPool(timeout time.Duration) *Pool {
	return &Pool{timeout: timeout, clients: map[string]*http.Client{}}
}

// Get returns the client for proxyURL; "" means a direct connection.
// Environment proxy variables are deliberately ignored: the config decides.
func (p *Pool) Get(proxyURL string) (*http.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.clients[proxyURL]; ok {
		return c, nil
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: p.timeout,
	}
	if proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy URL")
		}
		switch strings.ToLower(u.Scheme) {
		case "http", "https", "socks5", "socks5h":
		default:
			return nil, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
		}
		transport.Proxy = http.ProxyURL(u)
	}
	c := &http.Client{
		Transport: transport,
		Timeout:   p.timeout,
		// Never follow redirects: an API key must not be replayed to another
		// URL, and a redirect from a quota endpoint is an error anyway.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	p.clients[proxyURL] = c
	return c, nil
}

// Retain closes idle connections of clients whose proxy is no longer used.
func (p *Pool) Retain(inUse map[string]bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, c := range p.clients {
		if !inUse[key] {
			c.CloseIdleConnections()
			delete(p.clients, key)
		}
	}
}

// Redact returns proxyURL without its password, for logs.
func Redact(proxyURL string) string {
	if proxyURL == "" {
		return "direct"
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		return "invalid"
	}
	return u.Redacted()
}
