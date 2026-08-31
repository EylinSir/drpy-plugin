package main

import (
	// 标准库

	"bytes"
	"context"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/cookiejar"
	handleUrl "net/url"
	"os"
	"os/signal"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	// 本地包
	"MediaProxy/base"

	// 第三方库
	"github.com/go-resty/resty/v2"
	"github.com/patrickmn/go-cache"
	"github.com/sirupsen/logrus"
)

//go:embed static
var indexHTML embed.FS

var mediaCache = cache.New(4*time.Hour, 10*time.Minute)
var authKey string
var enableContentTypeGuess bool

const (
	AppVersion = "V1.1.0 20260831"

	// pieceSize 有序队列的数据块大小，也是内存池的基本单元
	pieceSize = int64(128 * 1024)
	// probeRange 探测请求的 Range（仅用于 HEAD 与冷缓存的后缀 Range 场景）
	probeRange = "bytes=0-1023"
	// idleReadTimeout 分段下载读空闲看门狗：超过该时长没有任何数据则强断重试
	idleReadTimeout = 30 * time.Second
	// segMaxAttempts 单个分段的最大尝试次数
	segMaxAttempts = 5
	// minSegSize / maxSegSize 分段大小的硬限制
	minSegSize = int64(128 * 1024)
	maxSegSize = int64(32 * 1024 * 1024)
)

// 引擎可调参数（由命令行 flag 初始化）
var (
	maxThreads     = 16                     // 自动线程档的上限
	windowMB       = 16                     // 预读窗口（MB）：已下载但未被播放器消费的最大数据量
	segSizeDefault = int64(2 * 1024 * 1024) // 默认分段大小
	retryBaseDelay = 500 * time.Millisecond // 重试退避基数（测试时可调小）
	cacheTTL       = 1800 * time.Second
)

// 预编译正则，避免每个请求重复编译
var (
	rangeRegex    = regexp.MustCompile(`bytes= *([0-9]+) *- *([0-9]*)`)
	suffixRegex   = regexp.MustCompile(`bytes= *-([0-9]+)`)
	totalRegex    = regexp.MustCompile(`.*/([0-9]+)`)
	rgRangeRegex  = regexp.MustCompile(`[0-9]+-([0-9]+)`)
	filenameRegex = regexp.MustCompile(`^.*filename="([^"]+)".*$`)
)

// pieceBufPool 数据块内存池，多路播放时显著降低 GC 压力
var pieceBufPool = sync.Pool{New: func() any {
	return make([]byte, pieceSize)
}}

// ---------------------------------------------------------------------------
// 播放器 Range 解析
// ---------------------------------------------------------------------------

type rangeReq struct {
	present   bool // 有合法的单区间 Range（含开放结尾）
	full      bool // 无 Range / 多区间 / 无法解析：按全量 200 处理
	start     int64
	end       int64 // -1 表示开放结尾
	exact     bool  // 显式指定了结尾
	suffix    bool  // bytes=-N 后缀形式
	suffixLen int64
}

func parsePlayerRange(requestRange string) rangeReq {
	if requestRange == "" {
		return rangeReq{full: true}
	}
	if m := suffixRegex.FindStringSubmatch(requestRange); m != nil {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		return rangeReq{present: true, suffix: true, suffixLen: n, end: -1}
	}
	if m := rangeRegex.FindStringSubmatch(requestRange); m != nil {
		start, _ := strconv.ParseInt(m[1], 10, 64)
		rr := rangeReq{present: true, start: start, end: -1}
		if len(m) > 2 && m[2] != "" {
			end, _ := strconv.ParseInt(m[2], 10, 64)
			rr.end = end
			rr.exact = true
		}
		return rr
	}
	// 多区间等无法解析的形式：按全量返回（与旧版行为一致）
	return rangeReq{full: true}
}

// resolveRange 结合文件总大小计算实际要服务的字节区间（闭区间）
func resolveRange(rr rangeReq, total int64) (start, end int64, ok bool) {
	if total <= 0 {
		return 0, 0, false
	}
	if rr.suffix {
		start = total - rr.suffixLen
		if start < 0 {
			start = 0
		}
		end = total - 1
	} else {
		start = rr.start
		end = total - 1
		if rr.exact && rr.end < total {
			end = rr.end
		}
	}
	if start < 0 {
		start = 0
	}
	if start > end || start >= total {
		return 0, 0, false
	}
	return start, end, true
}

// ---------------------------------------------------------------------------
// 源站信息探测与缓存
// ---------------------------------------------------------------------------

type originInfo struct {
	hdr    http.Header
	total  int64
	ranges bool
}

func copyHeader(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, v := range h {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// normalizeOrigin 从源站响应头提取总大小、Range 支持情况，并做兼容性修正。
// 会就地修改 hdr（调用方传入深拷贝）。
func normalizeOrigin(hdr http.Header, url string) *originInfo {
	total := int64(0)
	cr := hdr.Get("Content-Range")
	if m := totalRegex.FindStringSubmatch(cr); m != nil {
		total, _ = strconv.ParseInt(m[1], 10, 64)
	}
	if total <= 0 {
		total, _ = strconv.ParseInt(hdr.Get("Content-Length"), 10, 64)
	}

	// 检查是否受限于网盘试看(如迅雷 rg=0-82432800)参数
	if total > 0 {
		if parsed, err := handleUrl.Parse(url); err == nil {
			if rg := parsed.Query().Get("rg"); rg != "" {
				if m := rgRangeRegex.FindStringSubmatch(rg); m != nil {
					rgEnd, _ := strconv.ParseInt(m[1], 10, 64)
					if rgEnd > 0 && rgEnd < total {
						total = rgEnd + 1
						if cr != "" {
							hdr.Set("Content-Range", totalRegex.ReplaceAllString(cr, fmt.Sprintf("/%d", total)))
						}
						hdr.Set("Content-Length", strconv.FormatInt(total, 10))
						logrus.Debugf("检测到 URL 包含试看范围限制 rg=%s，将文件总大小修正为: %d", rg, total)
					}
				}
			}
		}
	}

	// Content-Type 策略与旧版一致：默认删除让播放器强制嗅探，
	// 网盘的 fext=mp4 可能是假的（实际是 mkv 等），强行设置会导致拖拽卡死
	ct := hdr.Get("Content-Type")
	if ct == "" || ct == "application/octet-stream" {
		if enableContentTypeGuess {
			if g := guessContentType(url, hdr.Get("Content-Disposition")); g != "" {
				hdr.Set("Content-Type", g)
			} else {
				hdr.Del("Content-Type")
			}
		} else {
			hdr.Del("Content-Type")
		}
	}

	if total > 0 {
		hdr.Set("Content-Length", strconv.FormatInt(total, 10))
	}
	ranges := cr != "" || hdr.Get("Accept-Ranges") != ""
	return &originInfo{hdr: hdr, total: total, ranges: ranges}
}

func getCachedOrigin(url string) *originInfo {
	if v, found := mediaCache.Get(url + "#Origin"); found {
		if info, ok := v.(*originInfo); ok {
			return info
		}
	}
	return nil
}

// probeAndCache 仅用于 HEAD 请求与冷缓存的后缀 Range：发一个小 Range 请求拿响应头。
// 常规 GET 的冷启动由引擎 leader 直接承担，不再有独立的探测请求。
func probeAndCache(url string, headers http.Header, jar *cookiejar.Jar) (*originInfo, error) {
	resp, err := base.NewRestyClient().
		SetTimeout(30*time.Second).
		SetRetryCount(2).
		SetCookieJar(jar).
		R().
		SetDoNotParseResponse(true).
		SetHeaderMultiValues(headers).
		SetHeader("Range", probeRange).
		Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.RawBody().Close()
	if resp.StatusCode() < 200 || resp.StatusCode() >= 400 {
		return nil, fmt.Errorf("%s", resp.Status())
	}
	info := normalizeOrigin(copyHeader(resp.Header()), url)
	if info.total > 0 {
		mediaCache.Set(url+"#Origin", info, cacheTTL)
	}
	return info, nil
}

// ---------------------------------------------------------------------------
// leader-follower 流式分段下载引擎
// ---------------------------------------------------------------------------

var (
	errRangeNotSatisfiable = errors.New("range not satisfiable") // 源站 416：按平滑 EOF 处理
	errRangeIgnored        = errors.New("server ignored range")  // 源站对 start>0 返回 200：无法多线程
)

type segment struct {
	start, end int64 // 闭区间
	pieces     chan []byte
	err        error // 所有者协程在 close(pieces) 之前写入
}

type fatalError struct{ code int }

func (f *fatalError) Error() string { return fmt.Sprintf("源站返回不可恢复状态 %d", f.code) }

type retryableStatus struct {
	code  int
	after time.Duration // Retry-After
}

func (r *retryableStatus) Error() string { return fmt.Sprintf("源站限流状态 %d", r.code) }

func errAfter(err error) time.Duration {
	var rs *retryableStatus
	if errors.As(err, &rs) {
		return rs.after
	}
	return 0
}

type engine struct {
	ctx    context.Context
	cancel context.CancelFunc

	url     string
	headers http.Header // 已清洗的转发请求头（不含 Range）
	jar     *cookiejar.Jar
	client  *http.Client

	end       int64 // 服务区间的最后一个字节（闭区间）
	nextStart int64 // 下一个待认领偏移
	segSize   int64
	piece     int64

	tokens chan struct{} // 预读窗口令牌：限制已下载但未消费的数据量

	mu        sync.Mutex
	cond      *sync.Cond
	segs      []*segment
	running   bool
	firstResp *http.Response // 冷启动时 leader 已打开的响应
	wg        sync.WaitGroup
}

func newEngine(parent context.Context, url string, headers http.Header, jar *cookiejar.Jar, rangeStart, rangeEnd, segSize int64, windowBytes int64) *engine {
	ctx, cancel := context.WithCancel(parent)
	piece := segSize
	if piece > pieceSize {
		piece = pieceSize
	}
	windowTokens := int(windowBytes / piece)
	if windowTokens < 16 {
		windowTokens = 16
	}
	e := &engine{
		ctx:       ctx,
		cancel:    cancel,
		url:       url,
		headers:   headers,
		jar:       jar,
		client:    base.StreamClient,
		end:       rangeEnd,
		nextStart: rangeStart,
		segSize:   segSize,
		piece:     piece,
		tokens:    make(chan struct{}, windowTokens),
	}
	e.cond = sync.NewCond(&e.mu)
	return e
}

func (e *engine) newSegment(start int64) *segment {
	end := start + e.segSize - 1
	if end > e.end {
		end = e.end
	}
	return &segment{start: start, end: end, pieces: make(chan []byte, 2)}
}

// seed 注册首段（按序队列的头部），必须在 start 之前调用
func (e *engine) seed(seg *segment, firstResp *http.Response) {
	e.mu.Lock()
	e.segs = append(e.segs, seg)
	e.nextStart = seg.end + 1
	e.firstResp = firstResp
	e.running = true
	e.mu.Unlock()
	e.cond.Broadcast()
}

func (e *engine) start(threads int) {
	e.wg.Add(1)
	go e.runLeader(e.segs[0], e.firstResp)
	for i := 1; i < threads; i++ {
		e.wg.Add(1)
		go e.worker()
	}
}

func (e *engine) stop() {
	e.mu.Lock()
	if !e.running {
		e.mu.Unlock()
		return
	}
	e.running = false
	e.mu.Unlock()
	e.cond.Broadcast()
	e.cancel()
}

// claim 顺序认领下一个分段；队列取空或已停止时返回 nil
func (e *engine) claim() *segment {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.running || e.nextStart > e.end {
		return nil
	}
	seg := e.newSegment(e.nextStart)
	e.nextStart = seg.end + 1
	e.segs = append(e.segs, seg)
	e.cond.Broadcast()
	return seg
}

// pumpTo 按序消费所有分段并写入 emitter（播放器消费速度通过 io.Pipe 与窗口令牌双重背压）
func (e *engine) pumpTo(emitter *base.Emitter) {
	defer func() {
		e.stop()
		emitter.Close()
	}()
	startTime := time.Now()
	first := true
	for {
		e.mu.Lock()
		for len(e.segs) == 0 && e.running {
			e.cond.Wait()
		}
		if !e.running || len(e.segs) == 0 {
			e.mu.Unlock()
			return
		}
		seg := e.segs[0]
		e.mu.Unlock()

		for b := range seg.pieces {
			if first {
				first = false
				logrus.Debugf("首字节耗时: %s", time.Since(startTime))
			}
			if _, err := emitter.Write(b); err != nil {
				logrus.Debugf("播放器侧写入失败，终止下载: %v", err)
				e.stop()
				return
			}
			if cap(b) == int(pieceSize) {
				pieceBufPool.Put(b[:0])
			}
			<-e.tokens
		}
		if seg.err != nil {
			logrus.Errorf("分段 %d-%d 最终失败，终止本路播放: %v", seg.start, seg.end, seg.err)
			e.stop()
			return
		}
		e.mu.Lock()
		e.segs = e.segs[1:]
		done := len(e.segs) == 0 && e.nextStart > e.end
		e.mu.Unlock()
		if done {
			logrus.Debugf("播放流完成，总耗时 %s", time.Since(startTime))
			e.stop()
			return
		}
	}
}

func (e *engine) runLeader(seg *segment, firstResp *http.Response) {
	defer e.wg.Done()
	defer close(seg.pieces)

	var err error
	if firstResp != nil {
		err = e.consumeResp(seg, firstResp)
	} else {
		err = e.attemptSegment(seg)
	}
	for attempt := 1; err != nil && attempt < segMaxAttempts && e.ctx.Err() == nil; attempt++ {
		if isTerminalSegErr(err) {
			break
		}
		logrus.Warnf("首段 %d-%d 第%d次尝试失败，准备重试: %v", seg.start, seg.end, attempt, err)
		if !e.backoffSleep(attempt, errAfter(err)) {
			break
		}
		err = e.attemptSegment(seg)
	}
	if err != nil && !errors.Is(err, errRangeNotSatisfiable) {
		seg.err = err
		if e.ctx.Err() == nil {
			logrus.Errorf("首段 %d-%d 最终失败: %v", seg.start, seg.end, err)
			e.stop()
		}
	}
}

func (e *engine) worker() {
	defer e.wg.Done()
	for {
		seg := e.claim()
		if seg == nil {
			return
		}
		e.downloadSegment(seg)
		if seg.err != nil {
			if e.ctx.Err() != nil {
				return
			}
			logrus.Errorf("分段 %d-%d 重试 %d 次后仍失败，终止本路播放: %v", seg.start, seg.end, segMaxAttempts, seg.err)
			e.stop()
			return
		}
		if e.ctx.Err() != nil {
			return
		}
	}
}

func isTerminalSegErr(err error) bool {
	if errors.Is(err, errRangeNotSatisfiable) || errors.Is(err, errRangeIgnored) {
		return true
	}
	var fe *fatalError
	return errors.As(err, &fe)
}

func (e *engine) downloadSegment(seg *segment) {
	defer close(seg.pieces)
	for attempt := 0; ; attempt++ {
		if e.ctx.Err() != nil {
			seg.err = e.ctx.Err()
			return
		}
		err := e.attemptSegment(seg)
		if err == nil {
			return // 重试成功，不携带任何错误
		}
		if errors.Is(err, errRangeNotSatisfiable) {
			// 416：源站认为已到文件尾，按平滑 EOF 处理，不视为失败
			logrus.Debugf("分段 %d-%d 收到416，按EOF平滑结束", seg.start, seg.end)
			return
		}
		if errors.Is(err, errRangeIgnored) {
			seg.err = err
			return
		}
		if attempt+1 >= segMaxAttempts {
			seg.err = err
			return
		}
		var fe *fatalError
		if errors.As(err, &fe) {
			seg.err = err
			return
		}
		logrus.Warnf("分段 %d-%d 第%d次尝试失败: %v", seg.start, seg.end, attempt+1, err)
		if !e.backoffSleep(attempt+1, errAfter(err)) {
			seg.err = e.ctx.Err()
			return
		}
	}
}

func (e *engine) backoffSleep(attempt int, after time.Duration) bool {
	d := retryBaseDelay << (attempt - 1)
	if d > 8*time.Second {
		d = 8 * time.Second
	}
	if after > d {
		d = after
		if d > 30*time.Second {
			d = 30 * time.Second
		}
	}
	d += time.Duration(rand.Int63n(250)) * time.Millisecond
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-e.ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if sec, err := strconv.Atoi(v); err == nil && sec > 0 {
		return time.Duration(sec) * time.Second
	}
	return 0
}

func (e *engine) attemptSegment(seg *segment) error {
	req, err := http.NewRequestWithContext(e.ctx, http.MethodGet, e.url, nil)
	if err != nil {
		return err
	}
	h := e.headers.Clone()
	h.Set("Range", fmt.Sprintf("bytes=%d-%d", seg.start, seg.end))
	req.Header = h
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	return e.consumeResp(seg, resp)
}

// consumeResp 校验分段响应并流式消费 body
func (e *engine) consumeResp(seg *segment, resp *http.Response) error {
	body := resp.Body
	switch {
	case resp.StatusCode == 416:
		body.Close()
		return errRangeNotSatisfiable
	case resp.StatusCode == 429 || resp.StatusCode == 503:
		after := parseRetryAfter(resp.Header.Get("Retry-After"))
		body.Close()
		return &retryableStatus{code: resp.StatusCode, after: after}
	case resp.StatusCode == 200 && seg.start > 0:
		body.Close()
		return errRangeIgnored
	case resp.StatusCode >= 500:
		body.Close()
		return &retryableStatus{code: resp.StatusCode}
	case resp.StatusCode != 206 && resp.StatusCode != 200:
		body.Close()
		return &fatalError{code: resp.StatusCode}
	}
	// 严格校验 Content-Range 起始偏移，防止 CDN 返回错误分片导致播放器解码卡死
	if resp.StatusCode == 206 {
		if cr := resp.Header.Get("Content-Range"); cr != "" {
			if !strings.HasPrefix(cr, fmt.Sprintf("bytes %d-", seg.start)) {
				body.Close()
				return fmt.Errorf("CDN返回的Range偏移量错误: %s", cr)
			}
		}
	}
	return e.streamBody(seg, body)
}

// streamBody 边下边切：按 piece 大小切块入队（首字节无需等待整个分段下载完）
func (e *engine) streamBody(seg *segment, body io.ReadCloser) error {
	defer body.Close()
	// 看门狗：读空闲超时或 ctx 取消时强关 body 解除阻塞
	wd := time.AfterFunc(idleReadTimeout, func() { body.Close() })
	defer wd.Stop()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-e.ctx.Done():
			body.Close()
		case <-stop:
		}
		wd.Stop()
	}()

	expected := seg.end - seg.start + 1
	var sent int64
	buf := pieceBufPool.Get().([]byte)[:e.piece]
	filled := 0
	for {
		wd.Reset(idleReadTimeout)
		n, rerr := body.Read(buf[filled:])
		filled += n

		if filled == len(buf) || (rerr != nil && filled > 0) {
			pushN := int64(filled)
			if sent+pushN > expected {
				pushN = expected - sent // 截断源站多发的数据
			}
			if pushN > 0 {
				if err := e.pushPiece(seg, buf[:pushN]); err != nil {
					pieceBufPool.Put(buf[:0])
					return err
				}
				sent += pushN
			}
			buf = pieceBufPool.Get().([]byte)[:e.piece]
			filled = 0
		}
		if sent >= expected {
			pieceBufPool.Put(buf[:0])
			return nil
		}
		if rerr != nil {
			pieceBufPool.Put(buf[:0])
			if errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) {
				return fmt.Errorf("短读: 已收 %d/%d 字节", sent, expected)
			}
			return rerr
		}
	}
}

func (e *engine) pushPiece(seg *segment, b []byte) error {
	select {
	case e.tokens <- struct{}{}:
	case <-e.ctx.Done():
		return e.ctx.Err()
	}
	select {
	case seg.pieces <- b:
		return nil
	case <-e.ctx.Done():
		<-e.tokens
		return e.ctx.Err()
	}
}

// pickThreads 计算线程数：URL thread 参数优先（1-32），否则按区间大小自动分档
func pickThreads(span int64, segSize int64, userThread string) int {
	n := int64(0)
	if userThread != "" {
		n, _ = strconv.ParseInt(userThread, 10, 64)
		if n > 32 {
			logrus.Debugf("请求线程数(%d)过大，限制为32以防止被封禁", n)
			n = 32
		}
	}
	if n <= 0 {
		switch {
		case span <= 8<<20:
			n = 2
		case span <= 64<<20:
			n = 6
		case span <= 256<<20:
			n = 12
		default:
			n = int64(maxThreads)
		}
		if n > int64(maxThreads) {
			n = int64(maxThreads)
		}
	}
	if feas := (span + segSize - 1) / segSize; n > feas {
		n = feas
	}
	if n < 1 {
		n = 1
	}
	return int(n)
}

// ---------------------------------------------------------------------------
// 响应头组装
// ---------------------------------------------------------------------------

func shouldSkipCopyHeader(key string) bool {
	switch strings.ToLower(key) {
	case "connection", "proxy-connection", "keep-alive", "transfer-encoding",
		"content-encoding", "content-length", "content-range":
		return true
	}
	return false
}

func sendRangeHeaders(w http.ResponseWriter, origin http.Header, total, start, end int64, partial bool) {
	h := w.Header()
	for k, v := range origin {
		if shouldSkipCopyHeader(k) {
			continue
		}
		h.Set(k, strings.Join(v, ","))
	}
	h.Del("Pragma")
	h.Del("Expires")
	h.Set("Cache-Control", "public, max-age=31536000")
	h.Set("Connection", "keep-alive")
	h.Set("Accept-Ranges", "bytes")
	if partial {
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		h.Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		h.Del("Content-Range")
		h.Set("Content-Length", strconv.FormatInt(total, 10))
		w.WriteHeader(http.StatusOK)
	}
}

func send416(w http.ResponseWriter, total int64) {
	w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", total))
	w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
}

func isClientAbort(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "write on closed pipe") ||
		strings.Contains(err.Error(), "client disconnected") ||
		strings.Contains(err.Error(), "forcibly closed") ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET)
}

// streamSingle 源站不支持 Range 时的单流降级：直接转发这份从 0 开始的全量响应
func streamSingle(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()
	h := w.Header()
	for k, v := range resp.Header {
		if shouldSkipCopyHeader(k) {
			continue
		}
		h.Set(k, strings.Join(v, ","))
	}
	h.Set("Cache-Control", "public, max-age=31536000")
	h.Set("Connection", "keep-alive")
	if resp.ContentLength > 0 {
		h.Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	w.WriteHeader(http.StatusOK)
	buf := make([]byte, 32*1024)
	if _, err := io.CopyBuffer(w, resp.Body, buf); err != nil && !isClientAbort(err) {
		logrus.Debugf("单流转发出错: %v", err)
	}
}

// serveEngine 组装 emitter 管道并把引擎数据泵给播放器
func serveEngine(w http.ResponseWriter, eng *engine) {
	rp, wp := io.Pipe()
	emitter := base.NewEmitter(rp, wp)
	defer func() {
		if !emitter.IsClosed() {
			emitter.Close()
		}
	}()
	go eng.pumpTo(emitter)

	// 32KB 复制缓冲：配合管道背压，防止播放器贪婪拉取导致带宽暴走
	buf := make([]byte, 32*1024)
	if _, err := io.CopyBuffer(w, emitter, buf); err != nil && !isClientAbort(err) {
		logrus.Debugf("io.Copy error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 请求处理
// ---------------------------------------------------------------------------

func guessContentType(url string, contentDisposition string) string {
	var fileName string
	contentDisposition = strings.ToLower(contentDisposition)
	if contentDisposition != "" {
		if filenameRegex.MatchString(contentDisposition) {
			fileName = filenameRegex.ReplaceAllString(contentDisposition, "$1")
		}
	} else {
		lastSlashIndex := strings.LastIndex(url, "/")
		queryIndex := strings.Index(url, "?")
		if queryIndex == -1 {
			fileName = url[lastSlashIndex+1:]
		} else {
			fileName = url[lastSlashIndex+1 : queryIndex]
		}
	}

	contentType := ""
	urlLower := strings.ToLower(url)
	if strings.HasSuffix(fileName, ".webm") || strings.Contains(urlLower, "fext=webm") || strings.Contains(urlLower, ".webm") {
		contentType = "video/webm"
	} else if strings.HasSuffix(fileName, ".avi") || strings.Contains(urlLower, "fext=avi") || strings.Contains(urlLower, ".avi") {
		contentType = "video/x-msvideo"
	} else if strings.HasSuffix(fileName, ".wmv") || strings.Contains(urlLower, "fext=wmv") || strings.Contains(urlLower, ".wmv") {
		contentType = "video/x-ms-wmv"
	} else if strings.HasSuffix(fileName, ".flv") || strings.Contains(urlLower, "fext=flv") || strings.Contains(urlLower, ".flv") {
		contentType = "video/x-flv"
	} else if strings.HasSuffix(fileName, ".mov") || strings.Contains(urlLower, "fext=mov") || strings.Contains(urlLower, ".mov") {
		contentType = "video/quicktime"
	} else if strings.HasSuffix(fileName, ".mkv") || strings.Contains(urlLower, "fext=mkv") || strings.Contains(urlLower, ".mkv") {
		contentType = "video/x-matroska"
	} else if strings.HasSuffix(fileName, ".ts") || strings.Contains(urlLower, "fext=ts") || strings.Contains(urlLower, ".ts") {
		contentType = "video/mp2t"
	} else if strings.HasSuffix(fileName, ".mpeg") || strings.HasSuffix(fileName, ".mpg") {
		contentType = "video/mpeg"
	} else if strings.HasSuffix(fileName, ".3gpp") || strings.HasSuffix(fileName, ".3gp") {
		contentType = "video/3gpp"
	} else if strings.HasSuffix(fileName, ".mp4") || strings.HasSuffix(fileName, ".m4s") || strings.Contains(urlLower, "fext=mp4") || strings.Contains(urlLower, ".mp4") {
		contentType = "video/mp4"
	}
	return contentType
}

func authOK(got string) bool {
	if authKey == "" {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(authKey)) == 1
}

func handleMethod(w http.ResponseWriter, req *http.Request) {
	switch req.Method {
	case http.MethodGet, http.MethodHead:
		logrus.Info("正在 GET/HEAD 请求")
		if req.URL.RawQuery == "" {
			if req.Method == http.MethodGet {
				indexContent, err := indexHTML.ReadFile("static/index.html")
				if err == nil {
					w.Header().Set("Content-Type", "text/html; charset=utf-8")
					w.Write(indexContent)
				} else {
					w.Header().Set("Content-Type", "text/plain; charset=utf-8")
					w.Write([]byte(fmt.Sprintf("欢迎使用drpyS专用多线程媒体代理服务，由道长于2026年开发\n版本: %s", AppVersion)))
				}
			}
		} else {
			handleGetMethod(w, req)
		}
	default:
		logrus.Infof("正在处理 %v 请求", req.Method)
		handleOtherMethod(w, req)
	}
}

func shouldFilterHeaderName(key string) bool {
	if len(strings.TrimSpace(key)) == 0 {
		return false
	}
	key = strings.ToLower(key)
	// 保留 Range 的转发；过滤 if-range 防止源站因时间戳不匹配返回全量
	return key == "host" || key == "http-client-ip" || key == "remote-addr" || key == "accept-encoding" || key == "if-range"
}

func handleGetMethod(w http.ResponseWriter, req *http.Request) {
	logrus.Debugf("当前活跃的协程数量: %d", runtime.NumGoroutine())

	query := req.URL.Query()
	url := query.Get("url")
	strForm := query.Get("form")
	strHeader := query.Get("headers")
	if strHeader == "" {
		strHeader = query.Get("header")
	}
	strAuth := query.Get("auth")
	strThread := query.Get("thread")
	strSegSize := query.Get("size")
	if strSegSize == "" {
		strSegSize = query.Get("chunkSize")
	}

	if !authOK(strAuth) {
		http.Error(w, "无效的认证参数", http.StatusUnauthorized)
		return
	}

	if url == "" {
		http.Error(w, "缺少url参数", http.StatusBadRequest)
		return
	}
	if strForm == "base64" {
		bytesUrl, err := base64.StdEncoding.DecodeString(url)
		if err != nil {
			http.Error(w, fmt.Sprintf("无效的 Base64 Url: %v", err), http.StatusBadRequest)
			return
		}
		url = string(bytesUrl)
	}

	if strHeader != "" {
		if strForm == "base64" {
			bytesStrHeader, err := base64.StdEncoding.DecodeString(strHeader)
			if err != nil {
				http.Error(w, fmt.Sprintf("无效的Base64 Headers: %v", err), http.StatusBadRequest)
				return
			}
			strHeader = string(bytesStrHeader)
		}
		var headers map[string]string
		if err := json.Unmarshal([]byte(strHeader), &headers); err != nil {
			http.Error(w, fmt.Sprintf("Header Json格式化错误: %v", err), http.StatusInternalServerError)
			return
		}
		for key, value := range headers {
			req.Header.Set(key, value)
		}
	}

	// 清洗转发头：强制 identity 禁止源站 gzip（否则分片数据大小不匹配）
	originHeaders := make(http.Header)
	for name, value := range req.Header {
		if !shouldFilterHeaderName(name) {
			originHeaders[name] = value
		}
	}
	originHeaders.Set("Accept-Encoding", "identity")

	jar, _ := cookiejar.New(nil)
	if cookies := req.Cookies(); len(cookies) > 0 {
		if u, err := handleUrl.Parse(url); err == nil {
			jar.SetCookies(u, cookies)
		}
	}

	isHead := req.Method == http.MethodHead
	rr := parsePlayerRange(req.Header.Get("Range"))

	info := getCachedOrigin(url)
	if isHead || (rr.suffix && info == nil) {
		var err error
		info, err = probeAndCache(url, originHeaders, jar)
		if err != nil {
			http.Error(w, fmt.Sprintf("下载 %v 链接失败: %v", url, err), http.StatusBadGateway)
			return
		}
	}

	// HEAD：只回响应头
	if isHead {
		start, end, ok := resolveRange(rr, info.total)
		if !ok {
			send416(w, info.total)
			return
		}
		sendRangeHeaders(w, info.hdr, info.total, start, end, rr.present)
		return
	}

	// 源站不支持 Range（探测/缓存获知）：单流降级
	if info != nil && !info.ranges {
		logrus.Warnf("源站不支持断点续传，降级为单流转发: %s", url)
		streamWithoutRange(w, req, url, originHeaders, jar)
		return
	}

	segSize := resolveSegSize(strSegSize)

	// 命中缓存：立即写播放器响应头（0 额外往返），引擎随后开播
	if info != nil && info.total > 0 {
		start, end, ok := resolveRange(rr, info.total)
		if !ok {
			send416(w, info.total)
			return
		}
		sendRangeHeaders(w, info.hdr, info.total, start, end, rr.present)
		eng := newEngine(req.Context(), url, originHeaders, jar, start, end, segSize, int64(windowMB)*1024*1024)
		eng.seed(eng.newSegment(start), nil)
		threads := pickThreads(end-start+1, segSize, strThread)
		logrus.Debugf("Proxy data transfer(cached): thread=%d, segSize=%d, range=%d-%d", threads, segSize, start, end)
		eng.start(threads)
		serveEngine(w, eng)
		return
	}

	// 冷启动：leader 直接以播放器请求的偏移发起首个请求，
	// 响应头到达后立即写播放器头并开始流式转发（首字节 ≈ 1 个源站 RTT）
	leaderStart := int64(0)
	leaderEndCap := int64(-1)
	if rr.present && !rr.suffix {
		leaderStart = rr.start
		if rr.exact {
			leaderEndCap = rr.end
		}
	}
	seg0End := leaderStart + segSize - 1
	if leaderEndCap >= 0 && seg0End > leaderEndCap {
		seg0End = leaderEndCap
	}
	seg0 := &segment{start: leaderStart, end: seg0End, pieces: make(chan []byte, 2)}

	leaderReq, err := http.NewRequestWithContext(req.Context(), http.MethodGet, url, nil)
	if err != nil {
		http.Error(w, fmt.Sprintf("构建请求失败: %v", err), http.StatusInternalServerError)
		return
	}
	lh := originHeaders.Clone()
	lh.Set("Range", fmt.Sprintf("bytes=%d-%d", seg0.start, seg0.end))
	leaderReq.Header = lh
	leaderReq.Header.Set("Accept-Encoding", "identity")
	// leader 单独挂 cookie jar
	lc := *base.StreamClient
	lc.Jar = jar
	leaderResp, err := lc.Do(leaderReq)
	if err != nil {
		http.Error(w, fmt.Sprintf("下载 %v 链接失败: %v", url, err), http.StatusBadGateway)
		return
	}

	// 源站忽略 Range（返回 200 全量）：直接单流转发这份已打开的响应
	if leaderResp.StatusCode == http.StatusOK {
		logrus.Warnf("源站忽略Range请求，降级为单流转发: %s", url)
		streamSingle(w, leaderResp)
		return
	}
	if leaderResp.StatusCode != http.StatusPartialContent {
		defer leaderResp.Body.Close()
		http.Error(w, leaderResp.Status, leaderResp.StatusCode)
		return
	}

	info = normalizeOrigin(copyHeader(leaderResp.Header), url)

	// 总大小未知（极罕见）：把 leader 这一路作为精确区间直接回给播放器
	if info.total <= 0 {
		h := w.Header()
		for k, v := range leaderResp.Header {
			if shouldSkipCopyHeader(k) {
				continue
			}
			h.Set(k, strings.Join(v, ","))
		}
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/*", seg0.start, seg0.end))
		h.Set("Content-Length", strconv.FormatInt(seg0.end-seg0.start+1, 10))
		h.Set("Cache-Control", "public, max-age=31536000")
		h.Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusPartialContent)
		buf := make([]byte, 32*1024)
		if _, err := io.CopyBuffer(w, leaderResp.Body, buf); err != nil && !isClientAbort(err) {
			logrus.Debugf("未知大小单流转发出错: %v", err)
		}
		leaderResp.Body.Close()
		return
	}

	if info.total > 0 {
		mediaCache.Set(url+"#Origin", info, cacheTTL)
	}

	start, end, ok := resolveRange(rr, info.total)
	if !ok {
		leaderResp.Body.Close()
		send416(w, info.total)
		return
	}
	if end < seg0.end {
		seg0.end = end
	}

	sendRangeHeaders(w, info.hdr, info.total, start, end, rr.present)

	eng := newEngine(req.Context(), url, originHeaders, jar, start, end, segSize, int64(windowMB)*1024*1024)
	eng.seed(seg0, leaderResp)
	threads := pickThreads(end-start+1, segSize, strThread)
	logrus.Debugf("Proxy data transfer: thread=%d, segSize=%d, range=%d-%d, total=%d", threads, segSize, start, end, info.total)
	eng.start(threads)
	serveEngine(w, eng)
}

// streamWithoutRange 主动发一个不带 Range 的请求，把全量数据单流转发
func streamWithoutRange(w http.ResponseWriter, req *http.Request, url string, headers http.Header, jar *cookiejar.Jar) {
	httpReq, err := http.NewRequestWithContext(req.Context(), http.MethodGet, url, nil)
	if err != nil {
		http.Error(w, fmt.Sprintf("构建请求失败: %v", err), http.StatusInternalServerError)
		return
	}
	httpReq.Header = headers.Clone()
	lc := *base.StreamClient
	lc.Jar = jar
	resp, err := lc.Do(httpReq)
	if err != nil {
		http.Error(w, fmt.Sprintf("下载 %v 链接失败: %v", url, err), http.StatusBadGateway)
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		defer resp.Body.Close()
		http.Error(w, resp.Status, resp.StatusCode)
		return
	}
	streamSingle(w, resp)
}

// resolveSegSize 解析 URL 的 size/chunkSize 参数（兼容旧版的 K/M/B 单位格式）
func resolveSegSize(strSegSize string) int64 {
	if strSegSize == "" {
		return segSizeDefault
	}
	s := strings.ToUpper(strSegSize)
	var val int64
	switch {
	case strings.HasSuffix(s, "KB"):
		val, _ = strconv.ParseInt(strings.TrimSuffix(s, "KB"), 10, 64)
		val *= 1024
	case strings.HasSuffix(s, "K"):
		val, _ = strconv.ParseInt(strings.TrimSuffix(s, "K"), 10, 64)
		val *= 1024
	case strings.HasSuffix(s, "MB"):
		val, _ = strconv.ParseInt(strings.TrimSuffix(s, "MB"), 10, 64)
		val *= 1024 * 1024
	case strings.HasSuffix(s, "M"):
		val, _ = strconv.ParseInt(strings.TrimSuffix(s, "M"), 10, 64)
		val *= 1024 * 1024
	case strings.HasSuffix(s, "B"):
		val, _ = strconv.ParseInt(strings.TrimSuffix(s, "B"), 10, 64)
	default:
		// 纯数字，默认单位为 KB（与旧版一致）
		val, _ = strconv.ParseInt(s, 10, 64)
		val *= 1024
	}
	if val > maxSegSize {
		val = maxSegSize
	}
	if val < minSegSize {
		val = minSegSize
	}
	return val
}

func handleOtherMethod(w http.ResponseWriter, req *http.Request) {
	defer req.Body.Close()

	query := req.URL.Query()
	url := query.Get("url")
	strForm := query.Get("form")
	strHeader := query.Get("headers")
	strAuth := query.Get("auth")

	if !authOK(strAuth) {
		http.Error(w, "无效的认证参数", http.StatusUnauthorized)
		return
	}

	if url == "" {
		http.Error(w, "缺少 url 参数", http.StatusBadRequest)
		return
	}
	if strForm == "base64" {
		bytesUrl, err := base64.StdEncoding.DecodeString(url)
		if err != nil {
			http.Error(w, fmt.Sprintf("无效的 Base64 Url: %v", err), http.StatusBadRequest)
			return
		}
		url = string(bytesUrl)
	}

	var headers map[string]string
	if strHeader != "" {
		if strForm == "base64" {
			bytesStrHeader, err := base64.StdEncoding.DecodeString(strHeader)
			if err != nil {
				http.Error(w, fmt.Sprintf("无效的Base64 Headers: %v", err), http.StatusBadRequest)
				return
			}
			strHeader = string(bytesStrHeader)
		}
		if err := json.Unmarshal([]byte(strHeader), &headers); err != nil {
			http.Error(w, fmt.Sprintf("Header Json格式化错误: %v", err), http.StatusInternalServerError)
			return
		}
		for key, value := range headers {
			req.Header.Set(key, value)
		}
	}
	newHeader := make(map[string][]string)
	for name, value := range req.Header {
		if !shouldFilterHeaderName(name) {
			newHeader[name] = value
		}
	}
	newHeader["Accept-Encoding"] = []string{"identity"}

	jar, _ := cookiejar.New(nil)
	if cookies := req.Cookies(); len(cookies) > 0 {
		if u, err := handleUrl.Parse(req.URL.String()); err == nil {
			jar.SetCookies(u, cookies)
		}
	}

	var reqBody []byte
	if req.Body != nil {
		reqBody, _ = io.ReadAll(req.Body)
	}

	// Clone 出独立实例再改超时，避免并发请求互相污染全局配置
	cl := base.RestyClient.Clone().SetTimeout(10 * time.Second).SetRetryCount(2).SetCookieJar(jar)
	r := cl.R().
		SetBody(reqBody).
		SetHeaderMultiValues(newHeader)

	var resp *resty.Response
	var err error
	switch req.Method {
	case http.MethodPost:
		resp, err = r.Post(url)
	case http.MethodPut:
		resp, err = r.Put(url)
	case http.MethodOptions:
		resp, err = r.Options(url)
	case http.MethodDelete:
		resp, err = r.Delete(url)
	case http.MethodPatch:
		resp, err = r.Patch(url)
	default:
		http.Error(w, fmt.Sprintf("无效的Method: %v", req.Method), http.StatusBadRequest)
		return
	}

	if err != nil {
		http.Error(w, fmt.Sprintf("%v 链接 %v 失败: %v", req.Method, url, err), http.StatusInternalServerError)
		return
	}
	if resp.StatusCode() < 200 || resp.StatusCode() >= 400 {
		http.Error(w, resp.Status(), resp.StatusCode())
		return
	}

	w.Header().Set("Connection", "close")
	for name, values := range resp.Header() {
		w.Header().Set(name, strings.Join(values, ","))
	}
	w.WriteHeader(resp.StatusCode())
	io.Copy(w, bytes.NewReader(resp.Body()))
}

func main() {
	// 定义命令行参数
	dns := flag.String("dns", "8.8.8.8", "DNS解析 IP:port")
	port := flag.String("port", "5575", "服务器端口")
	debug := flag.Bool("debug", false, "Debug模式")
	auth := flag.String("auth", "", "认证密钥")
	guessType := flag.Bool("guess-type", false, "是否根据URL强制猜测并设置 Content-Type (可能导致 MPV 等播放器拖拽失败，默认不启用)")
	threadsFlag := flag.Int("threads", 16, "自动线程档的上限（URL 传 thread 可覆盖，硬上限 32）")
	windowFlag := flag.Int("window", 16, "预读窗口大小(MB)：已下载但未被播放器消费的最大数据量")
	segFlag := flag.String("seg", "2M", "多线程分段大小(支持 K/M/B 单位，纯数字默认 KB)，越大请求越少")

	// 帮助和版本信息
	showHelp := flag.Bool("h", false, "显示帮助信息")
	showHelpLong := flag.Bool("help", false, "显示帮助信息")
	showVersion := flag.Bool("v", false, "显示版本信息")
	showVersionLong := flag.Bool("version", false, "显示版本信息")

	// 自定义 Usage
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "drpyS专用多线程媒体代理服务 %s\n\n", AppVersion)
		fmt.Fprintf(os.Stderr, "用法:\n")
		fmt.Fprintf(os.Stderr, "  %s [参数]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "参数列表:\n")
		flag.PrintDefaults()
	}

	flag.Parse()

	if *showHelp || *showHelpLong {
		flag.Usage()
		return
	}

	if *showVersion || *showVersionLong {
		fmt.Printf("drpyS专用多线程媒体代理服务 %s\n", AppVersion)
		return
	}

	// 忽略 SIGPIPE 信号
	signal.Ignore(syscall.SIGPIPE)

	// 设置日志输出和级别
	logrus.SetOutput(os.Stdout)
	if *debug {
		logrus.SetLevel(logrus.DebugLevel)
		logrus.Info("已开启Debug模式")
	} else {
		logrus.SetLevel(logrus.InfoLevel)
	}

	if *threadsFlag < 1 {
		*threadsFlag = 1
	}
	if *threadsFlag > 32 {
		*threadsFlag = 32
	}
	if *windowFlag < 1 {
		*windowFlag = 1
	}
	maxThreads = *threadsFlag
	windowMB = *windowFlag
	segSizeDefault = resolveSegSize(*segFlag)

	logrus.Infof("服务器运行在 %s 端口. (threads<=%d, window=%dMB, seg=%dKB)", *port, maxThreads, windowMB, segSizeDefault/1024)

	// 设置全局变量
	authKey = *auth
	enableContentTypeGuess = *guessType
	base.DnsResolverIP = *dns
	base.InitClient()
	var server = http.Server{
		Addr:              ":" + *port,
		Handler:           http.HandlerFunc(handleMethod),
		ReadHeaderTimeout: 30 * time.Second,
	}
	// 注意：不要设置 WriteTimeout，长时间拖住的播放流会被掐断
	server.ListenAndServe()
}
