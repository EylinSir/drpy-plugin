package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"MediaProxy/base"
	"github.com/sirupsen/logrus"
)

// ---------------------------------------------------------------------------
// 模拟源站：支持 Range、可注入延迟/限速/故障
// ---------------------------------------------------------------------------

type testOrigin struct {
	data     []byte
	latency  time.Duration // 响应头之前的延迟
	throttle time.Duration // 每 64KB 的发送延迟（模拟限速）
	noRange  bool          // 无视 Range 头，永远返回 200 全量

	mu       sync.Mutex
	conns    int
	peak     int
	requests int

	// hook 返回 true 表示响应已由 hook 处理
	hook func(o *testOrigin, w http.ResponseWriter, r *http.Request, start, end int64) bool
}

func (o *testOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	o.conns++
	o.requests++
	if o.conns > o.peak {
		o.peak = o.conns
	}
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		o.conns--
		o.mu.Unlock()
	}()

	if o.latency > 0 {
		time.Sleep(o.latency)
	}

	start, end := int64(0), int64(len(o.data)-1)
	partial := false
	if rangeHdr := r.Header.Get("Range"); rangeHdr != "" && !o.noRange {
		if m := rangeRegex.FindStringSubmatch(rangeHdr); m != nil {
			start, _ = strconv.ParseInt(m[1], 10, 64)
			partial = true
			if len(m) > 2 && m[2] != "" {
				end, _ = strconv.ParseInt(m[2], 10, 64)
			}
		}
	}

	if o.hook != nil && o.hook(o, w, r, start, end) {
		return
	}
	if start >= int64(len(o.data)) {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(o.data)))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	if end >= int64(len(o.data)) {
		end = int64(len(o.data)) - 1
	}

	if partial {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(o.data)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.Header().Set("Accept-Ranges", "bytes")
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.Header().Set("Content-Length", strconv.Itoa(len(o.data)))
		w.WriteHeader(http.StatusOK)
	}
	o.writeBody(w, o.data[start:end+1])
}

func (o *testOrigin) writeBody(w http.ResponseWriter, b []byte) {
	const chunk = 64 * 1024
	for len(b) > 0 {
		n := chunk
		if n > len(b) {
			n = len(b)
		}
		if _, err := w.Write(b[:n]); err != nil {
			return
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		b = b[n:]
		if o.throttle > 0 && len(b) > 0 {
			time.Sleep(o.throttle)
		}
	}
}

func (o *testOrigin) stats() (peak, requests int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.peak, o.requests
}

// ---------------------------------------------------------------------------
// 测试脚手架
// ---------------------------------------------------------------------------

func fillData(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + 3)
	}
	return b
}

func setupProxy(t *testing.T) *httptest.Server {
	t.Helper()
	authKey = ""
	enableContentTypeGuess = false
	maxThreads = 16
	windowMB = 16
	segSizeDefault = 2 << 20
	retryBaseDelay = 10 * time.Millisecond
	mediaCache.Flush()
	base.InitClient()
	logrus.SetLevel(logrus.ErrorLevel)
	proxy := httptest.NewServer(http.HandlerFunc(handleMethod))
	t.Cleanup(proxy.Close)
	return proxy
}

func setupOrigin(t *testing.T, o *testOrigin) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(o)
	t.Cleanup(srv.Close)
	return srv
}

func proxyGet(proxyURL, originURL, query string) (*http.Response, error) {
	u := proxyURL + "/?url=" + url.QueryEscape(originURL)
	if query != "" {
		u += "&" + query
	}
	client := &http.Client{Timeout: 60 * time.Second}
	return client.Get(u)
}

// ---------------------------------------------------------------------------
// 基础正确性
// ---------------------------------------------------------------------------

func TestServeFullFile(t *testing.T) {
	data := fillData(3 << 20)
	o := &testOrigin{data: data}
	origin := setupOrigin(t, o)
	proxy := setupProxy(t)

	resp, err := proxyGet(proxy.URL, origin.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("状态码 = %d, 期望 200", resp.StatusCode)
	}
	if resp.ContentLength != int64(len(data)) {
		t.Fatalf("Content-Length = %d, 期望 %d", resp.ContentLength, len(data))
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatalf("数据不一致: got %d bytes", len(got))
	}
}

func TestServeRange(t *testing.T) {
	data := fillData(4 << 20)
	o := &testOrigin{data: data}
	origin := setupOrigin(t, o)
	proxy := setupProxy(t)

	// 先完整读一次全量请求（顺带预热缓存），避免泄漏未读的响应体
	resp, err := proxyGet(proxy.URL, origin.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	req, _ := http.NewRequest("GET", proxy.URL+"/?url="+url.QueryEscape(origin.URL), nil)
	req.Header.Set("Range", "bytes=1000-1999")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 206 {
		t.Fatalf("状态码 = %d, 期望 206", resp.StatusCode)
	}
	if cr := resp.Header.Get("Content-Range"); cr != fmt.Sprintf("bytes 1000-1999/%d", len(data)) {
		t.Fatalf("Content-Range = %q", cr)
	}
	got, _ := io.ReadAll(resp.Body)
	if len(got) != 1000 || string(got) != string(data[1000:2000]) {
		t.Fatalf("区间数据不一致: got %d bytes", len(got))
	}
}

func TestServeOpenEndRange(t *testing.T) {
	data := fillData(6 << 20)
	o := &testOrigin{data: data}
	origin := setupOrigin(t, o)
	proxy := setupProxy(t)

	req, _ := http.NewRequest("GET", proxy.URL+"/?url="+url.QueryEscape(origin.URL), nil)
	req.Header.Set("Range", "bytes=5242880-")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 206 {
		t.Fatalf("状态码 = %d, 期望 206", resp.StatusCode)
	}
	if cr := resp.Header.Get("Content-Range"); cr != fmt.Sprintf("bytes 5242880-%d/%d", len(data)-1, len(data)) {
		t.Fatalf("Content-Range = %q", cr)
	}
	got, _ := io.ReadAll(resp.Body)
	if len(got) != (6<<20)-5242880 || string(got) != string(data[5242880:]) {
		t.Fatalf("开放结尾区间数据不一致: got %d bytes", len(got))
	}
}

func TestServeSuffixRange(t *testing.T) {
	data := fillData(2 << 20)
	o := &testOrigin{data: data}
	origin := setupOrigin(t, o)
	proxy := setupProxy(t)

	req, _ := http.NewRequest("GET", proxy.URL+"/?url="+url.QueryEscape(origin.URL), nil)
	req.Header.Set("Range", "bytes=-500")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 206 {
		t.Fatalf("状态码 = %d, 期望 206", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if len(got) != 500 || string(got) != string(data[len(data)-500:]) {
		t.Fatalf("后缀区间数据不一致: got %d bytes", len(got))
	}
	// 冷缓存的后缀 Range 需要 1 次探测 + 1 次数据请求
	if _, reqs := o.stats(); reqs != 2 {
		t.Fatalf("源站请求数 = %d, 期望 2 (探测+数据)", reqs)
	}
}

func TestRangeBeyondEOF(t *testing.T) {
	data := fillData(1 << 20)
	o := &testOrigin{data: data}
	origin := setupOrigin(t, o)
	proxy := setupProxy(t)

	req, _ := http.NewRequest("GET", proxy.URL+"/?url="+url.QueryEscape(origin.URL), nil)
	req.Header.Set("Range", "bytes=2097152-")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 416 {
		t.Fatalf("状态码 = %d, 期望 416", resp.StatusCode)
	}
}

func TestHeadRequest(t *testing.T) {
	data := fillData(2 << 20)
	o := &testOrigin{data: data}
	origin := setupOrigin(t, o)
	proxy := setupProxy(t)

	resp, err := http.Head(proxy.URL + "/?url=" + url.QueryEscape(origin.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("状态码 = %d, 期望 200", resp.StatusCode)
	}
	if resp.ContentLength != int64(len(data)) {
		t.Fatalf("Content-Length = %d, 期望 %d", resp.ContentLength, len(data))
	}
}

func TestAuthRequired(t *testing.T) {
	data := fillData(64 << 10)
	o := &testOrigin{data: data}
	origin := setupOrigin(t, o)
	proxy := setupProxy(t)
	authKey = "secret"
	defer func() { authKey = "" }()

	resp, err := proxyGet(proxy.URL, origin.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("无auth状态码 = %d, 期望 401", resp.StatusCode)
	}

	resp, err = proxyGet(proxy.URL, origin.URL, "auth=secret")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("正确auth状态码 = %d, 期望 200", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// 多线程与起播速度
// ---------------------------------------------------------------------------

func TestMultiThreadEngaged(t *testing.T) {
	data := fillData(8 << 20)
	o := &testOrigin{data: data}
	origin := setupOrigin(t, o)
	proxy := setupProxy(t)

	resp, err := proxyGet(proxy.URL, origin.URL, "thread=8")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatalf("数据不一致: got %d bytes", len(got))
	}
	peak, _ := o.stats()
	if peak < 2 {
		t.Fatalf("源站峰值并发连接 = %d, 期望 >= 2（多线程未生效）", peak)
	}
}

// TestFastStartAndNoProbe 验证冷启动只有数据请求、没有独立探测请求，
// 且首字节只等待一次源站 RTT（旧架构需要 探测RTT + 首分片RTT）。
func TestFastStartAndNoProbe(t *testing.T) {
	data := fillData(4 << 20)
	o := &testOrigin{data: data, latency: 1200 * time.Millisecond}
	origin := setupOrigin(t, o)
	proxy := setupProxy(t)

	start := time.Now()
	resp, err := proxyGet(proxy.URL, origin.URL, "thread=2")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 1)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatal(err)
	}
	ttfb := time.Since(start)
	if ttfb > 2*time.Second {
		t.Fatalf("首字节耗时 %s, 期望 < 2s（一次源站RTT=1.2s + 余量）", ttfb)
	}

	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	full := append(buf, rest...)
	if string(full) != string(data) {
		t.Fatalf("数据不一致: got %d bytes", len(full))
	}
	_, reqs := o.stats()
	// 4MB / 2MB分段 = 2 个分段请求；旧架构此处会有 3 个（多 1 个探测）
	if reqs != 2 {
		t.Fatalf("源站请求数 = %d, 期望 2（不应存在独立探测请求）", reqs)
	}
}

// ---------------------------------------------------------------------------
// 故障注入：重试 / 降级 / 平滑结束
// ---------------------------------------------------------------------------

func TestRetryOn429(t *testing.T) {
	data := fillData(6 << 20)
	o := &testOrigin{data: data}
	var failedOnce sync.Map
	o.hook = func(o *testOrigin, w http.ResponseWriter, r *http.Request, start, end int64) bool {
		if start >= 2<<20 {
			if _, loaded := failedOnce.LoadOrStore("k", true); !loaded {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(429)
				return true
			}
		}
		return false
	}
	origin := setupOrigin(t, o)
	proxy := setupProxy(t)

	resp, err := proxyGet(proxy.URL, origin.URL, "thread=3")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatalf("429重试后数据不一致: got %d bytes", len(got))
	}
}

func TestBadContentRangeRetry(t *testing.T) {
	data := fillData(6 << 20)
	o := &testOrigin{data: data}
	var badOnce sync.Map
	o.hook = func(o *testOrigin, w http.ResponseWriter, r *http.Request, start, end int64) bool {
		if start > 0 {
			if _, loaded := badOnce.LoadOrStore("k", true); !loaded {
				// 返回错位的 Content-Range，模拟 CDN 串片
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start+1024, end+1024, len(o.data)))
				w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
				w.WriteHeader(206)
				w.Write(o.data[start : end+1])
				return true
			}
		}
		return false
	}
	origin := setupOrigin(t, o)
	proxy := setupProxy(t)

	resp, err := proxyGet(proxy.URL, origin.URL, "thread=3")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatalf("Content-Range校验重试后数据不一致: got %d bytes", len(got))
	}
}

func Test416MidStreamCleanEnd(t *testing.T) {
	data := fillData(6 << 20)
	o := &testOrigin{data: data}
	o.hook = func(o *testOrigin, w http.ResponseWriter, r *http.Request, start, end int64) bool {
		if start == 4<<20 {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(o.data)))
			w.WriteHeader(416)
			return true
		}
		return false
	}
	origin := setupOrigin(t, o)
	proxy := setupProxy(t)

	resp, err := proxyGet(proxy.URL, origin.URL, "thread=3")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// 源站中途 416 与先前承诺的总大小矛盾，代理只能截断（播放器会以 Range 重连续传）。
	// 此处验证的关键契约：及时平滑结束（不挂死）+ 已交付前缀字节正确。
	start := time.Now()
	got, readErr := io.ReadAll(resp.Body)
	elapsed := time.Since(start)
	if readErr != nil && readErr != io.ErrUnexpectedEOF {
		t.Fatalf("读取异常: %v", readErr)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("416后未及时结束: 耗时 %s", elapsed)
	}
	if len(got) < 4<<20-128*1024 || len(got) > 4<<20 {
		t.Fatalf("416后应平滑结束在约4MB处, 实际 %d bytes", len(got))
	}
	if string(got) != string(data[:len(got)]) {
		t.Fatal("416前缀数据不一致")
	}
}

func TestNoRangeOriginFallback(t *testing.T) {
	data := fillData(1 << 20)
	o := &testOrigin{data: data, noRange: true}
	origin := setupOrigin(t, o)
	proxy := setupProxy(t)

	req, _ := http.NewRequest("GET", proxy.URL+"/?url="+url.QueryEscape(origin.URL), nil)
	req.Header.Set("Range", "bytes=100-")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("状态码 = %d, 期望 200（源站不支持Range应降级全量）", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if len(got) != len(data) || string(got) != string(data) {
		t.Fatalf("降级单流数据不一致: got %d bytes", len(got))
	}
}

// ---------------------------------------------------------------------------
// 中断与资源泄漏
// ---------------------------------------------------------------------------

func TestSeekAbortNoLeak(t *testing.T) {
	data := fillData(8 << 20)
	o := &testOrigin{data: data, throttle: 20 * time.Millisecond}
	origin := setupOrigin(t, o)
	proxy := setupProxy(t)

	// 预热连接与基线
	warm, err := proxyGet(proxy.URL, origin.URL, "thread=2&size=256K")
	if err != nil {
		t.Fatal(err)
	}
	warmBody, _ := io.ReadAll(warm.Body)
	warm.Body.Close()
	if len(warmBody) != len(data) {
		t.Fatalf("预热数据不一致: got %d bytes", len(warmBody))
	}
	time.Sleep(500 * time.Millisecond)
	runtime.GC()
	baseline := runtime.NumGoroutine()

	// 模拟拖拽：读一部分后客户端直接断开
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", proxy.URL+"/?url="+url.QueryEscape(origin.URL)+"&thread=4&size=256K", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 512<<10)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close() // 客户端粗暴断开（模拟播放器 seek）

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+5 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > baseline+5 {
		t.Fatalf("断开后协程未回收: 当前 %d, 基线 %d", n, baseline)
	}

	// 断开后重新请求（模拟播放器 seek 后重连，走缓存热路径）
	resp2, err := proxyGet(proxy.URL, origin.URL, "thread=2&size=256K")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	got, err := io.ReadAll(resp2.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(data) || string(got) != string(data) {
		t.Fatalf("seek后重连数据不一致: got %d bytes", len(got))
	}
}
