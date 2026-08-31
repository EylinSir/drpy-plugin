package base

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-resty/resty/v2"
)

var (
	NoRedirectClient          *resty.Client
	NoRedirectClientWithProxy *resty.Client
	RestyClient               *resty.Client
	RestyClientWithProxy      *resty.Client
	HttpClient                *http.Client
	StreamClient              *http.Client // 多线程下载引擎专用：无整体超时（由请求 ctx 与读空闲看门狗控制），仅限制响应头等待
	DnsResolverIP             string
	IdleConnTimeout           = 90 * time.Second
	dnsResolverProto          = "udp"
	dnsResolverTimeoutMs      = 10000
	dnsCacheTTL               = 5 * time.Minute
)
var UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/87.0.4280.88 Safari/537.36"
var DefaultTimeout = time.Second * 30

// ---- 带缓存的 DNS 解析 ----
// 多线程并发建连时同一域名会被反复解析，这里缓存 5 分钟，
// 失效后自动回源，解析结果按 IPv4 优先排序。

type dnsEntry struct {
	ips    []string
	expire time.Time
}

type dnsCache struct {
	mu    sync.Mutex
	items map[string]dnsEntry
}

var gDnsCache = &dnsCache{items: make(map[string]dnsEntry)}

func (c *dnsCache) lookupAll(host string) ([]string, error) {
	if net.ParseIP(host) != nil {
		return []string{host}, nil
	}
	c.mu.Lock()
	e, ok := c.items[host]
	c.mu.Unlock()
	if ok && time.Now().Before(e.expire) && len(e.ips) > 0 {
		return e.ips, nil
	}

	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{
				Timeout: time.Duration(dnsResolverTimeoutMs) * time.Millisecond,
			}
			dnsAddr := DnsResolverIP
			if dnsAddr != "" && !strings.Contains(dnsAddr, ":") {
				dnsAddr = dnsAddr + ":53"
			}
			return d.DialContext(ctx, dnsResolverProto, dnsAddr)
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(dnsResolverTimeoutMs)*time.Millisecond)
	defer cancel()
	ips, err := resolver.LookupHost(ctx, host)
	if err != nil {
		return nil, err
	}
	sort.Slice(ips, func(i, j int) bool {
		return !strings.Contains(ips[i], ":") && strings.Contains(ips[j], ":")
	})
	c.mu.Lock()
	c.items[host] = dnsEntry{ips: ips, expire: time.Now().Add(dnsCacheTTL)}
	c.mu.Unlock()
	return ips, nil
}

func (c *dnsCache) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, addr)
	}
	ips, err := c.lookupAll(host)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, ip := range ips {
		conn, dialErr := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip, port))
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}
	if lastErr == nil {
		lastErr = &net.DNSError{Err: "no addresses", Name: host, IsNotFound: true}
	}
	return nil, lastErr
}

// newTransport 多连接下载的关键参数：
// MaxIdleConnsPerHost 默认只有 2，16 线程并发时连接会被反复关闭重建（TLS 握手风暴），必须调大；
// ForceAttemptHTTP2 让支持 h2 的 CDN 走多路复用，减少握手次数。
func newTransport() *http.Transport {
	return &http.Transport{
		DialContext:           gDnsCache.dialContext,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       IdleConnTimeout,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
	}
}

func InitClient() {
	NoRedirectClient = resty.New().SetRedirectPolicy(
		resty.RedirectPolicyFunc(func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}),
	).SetTLSClientConfig(&tls.Config{InsecureSkipVerify: true})
	NoRedirectClient.SetHeader("user-agent", UserAgent)

	NoRedirectClientWithProxy = resty.New().SetRedirectPolicy(
		resty.RedirectPolicyFunc(func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}),
	)
	NoRedirectClientWithProxy.SetHeader("user-agent", UserAgent)
	RestyClient = NewRestyClient()
	RestyClientWithProxy = NewRestyClient()
	HttpClient = NewHttpClient()
	StreamClient = NewStreamClient()
}

func NewRestyClient() *resty.Client {
	client := resty.New().
		SetHeader("user-agent", UserAgent).
		SetRetryCount(3).
		SetTimeout(DefaultTimeout).
		SetTransport(newTransport())
	return client
}

func NewHttpClient() *http.Client {
	return &http.Client{
		Timeout:   time.Hour * 48,
		Transport: newTransport(),
	}
}

func NewStreamClient() *http.Client {
	return &http.Client{
		Transport: newTransport(),
	}
}
