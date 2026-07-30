package httpstats

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"sync"
	"time"
)

type ClientOption func(*clientConfig)

type clientConfig struct {
	rt       http.RoundTripper
	handler  Handler
	nameFunc func(*RequestTagInfo)
}

// WithClientTransport sets the base RoundTripper. Defaults to http.DefaultTransport.
func WithClientTransport(rt http.RoundTripper) ClientOption {
	return func(c *clientConfig) { c.rt = rt }
}

// WithClientHandler sets the stats handler. Multiple handlers are composed
// via MultiHandler so each receives every event.
func WithClientHandler(h ...Handler) ClientOption {
	return func(c *clientConfig) {
		switch len(h) {
		case 0:
		case 1:
			c.handler = h[0]
		default:
			c.handler = MultiHandler(h)
		}
	}
}

// WithClientNameFunc sets a function that populates RequestTagInfo.OperationName.
func WithClientNameFunc(f func(*RequestTagInfo)) ClientOption {
	return func(c *clientConfig) { c.nameFunc = f }
}

// ClientTransport wraps http.RoundTripper with the full request lifecycle:
//
//	TagRequest → Begin → OutHeader → OutData* → InHeader → InData* → End
//
// Uses httptrace for low-level connection events. On RoundTrip error,
// the response body is drained for connection reuse and End fires with
// the error.
type ClientTransport struct {
	rt       http.RoundTripper
	handler  Handler
	nameFunc func(*RequestTagInfo)
}

// NewClientTransport creates a ClientTransport. Defaults to NoopHandler
// and http.DefaultTransport.
func NewClientTransport(opts ...ClientOption) *ClientTransport {
	cfg := &clientConfig{rt: http.DefaultTransport, handler: NoopHandler{}}
	for _, o := range opts {
		o(cfg)
	}
	return &ClientTransport{rt: cfg.rt, handler: cfg.handler, nameFunc: cfg.nameFunc}
}

func (ct *ClientTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	ctx = InjectTags(ctx, NewTags())

	info := &RequestTagInfo{
		Client:         true,
		Method:         req.Method,
		Host:           req.URL.Host,
		URL:            req.URL,
		RequestPattern: req.Pattern,
	}
	if ct.nameFunc != nil {
		ct.nameFunc(info)
	}
	ctx = ct.handler.TagRequest(ctx, info)

	startTime := time.Now()
	ct.handler.HandleRequest(ctx, &Begin{Client: true, StartTime: startTime})

	timings := &traceTimings{}
	ctx = ct.attachTrace(ctx, req, timings)
	req = req.WithContext(ctx)

	if req.Body != nil {
		req.Body = &bodyEventReader{
			inbound: false, client: true,
			body:    req.Body,
			handler: ct.handler,
			ctx:     ctx,
		}
	}

	resp, err := ct.rt.RoundTrip(req)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		ct.handler.HandleRequest(ctx, &End{
			Client:  true,
			EndTime: time.Now(),
			Error:   err,
		})
		return nil, err //nolint:wrapcheck // returning RoundTripper error directly
	}

	ttfb := time.Duration(0)
	if !timings.firstByteAt.IsZero() && !timings.wroteRequestAt.IsZero() {
		ttfb = timings.firstByteAt.Sub(timings.wroteRequestAt)
	}
	ct.handler.HandleRequest(ctx, &InHeader{
		Client:        true,
		StatusCode:    resp.StatusCode,
		Header:        resp.Header.Clone(),
		ContentLength: resp.ContentLength,
		Method:        req.Method,
		TTFB:          ttfb,
	})

	resp.Body = &responseBodyReader{
		body:    resp.Body,
		handler: ct.handler,
		ctx:     ctx,
		start:   startTime,
	}

	return resp, nil
}

type traceTimings struct {
	dnsStart, dnsDone           time.Time
	tcpStart, tcpDone           time.Time
	tlsStart, tlsDone           time.Time
	wroteRequestAt, firstByteAt time.Time
}

func (ct *ClientTransport) attachTrace(ctx context.Context, req *http.Request, t *traceTimings) context.Context {
	var gotConnReused bool
	var connCtx context.Context

	trace := &httptrace.ClientTrace{
		DNSStart:          func(_ httptrace.DNSStartInfo) { t.dnsStart = time.Now() },
		DNSDone:           func(_ httptrace.DNSDoneInfo) { t.dnsDone = time.Now() },
		ConnectStart:      func(_, _ string) { t.tcpStart = time.Now() },
		ConnectDone:       func(_, _ string, _ error) { t.tcpDone = time.Now() },
		TLSHandshakeStart: func() { t.tlsStart = time.Now() },
		TLSHandshakeDone:  func(_ tls.ConnectionState, _ error) { t.tlsDone = time.Now() },
		GotConn: func(info httptrace.GotConnInfo) {
			gotConnReused = info.Reused
			if !info.Reused {
				connCtx = ct.handler.TagConn( //nolint:fatcontext // context threading through handler chain
					ctx,
					&ConnTagInfo{
						Client:     true,
						RemoteAddr: info.Conn.RemoteAddr(),
					},
				)
				ct.handler.HandleConn(connCtx, &ConnBegin{
					Client:      true,
					StartTime:   time.Now(),
					RemoteAddr:  info.Conn.RemoteAddr().String(),
					DNSDuration: dnsDuration(t.dnsStart, t.dnsDone),
					TCPDuration: tcpDuration(t.tcpStart, t.tcpDone),
					TLSDuration: tlsDuration(t.tlsStart, t.tlsDone),
				})
			}
		},
		WroteHeaders: func() {
			ct.handler.HandleRequest(ctx, &OutHeader{
				Client:        true,
				Header:        req.Header.Clone(),
				ContentLength: req.ContentLength,
			})
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			t.wroteRequestAt = time.Now()
			if info.Err != nil {
				ct.handler.HandleRequest(ctx, &OutData{
					Client: true,
					Length: 0,
					Error:  info.Err,
				})
			}
		},
		GotFirstResponseByte: func() { t.firstByteAt = time.Now() },
		PutIdleConn: func(err error) {
			if !gotConnReused || err != nil {
				if connCtx == nil {
					connCtx = ctx //nolint:fatcontext // fallback to request context
				}
				ct.handler.HandleConn(connCtx, &ConnEnd{Client: true, Error: err})
			}
		},
		Got100Continue: func() {},
		Got1xxResponse: func(code int, header textproto.MIMEHeader) error {
			ct.handler.HandleRequest(ctx, &InHeader{
				Client:     true,
				StatusCode: code,
				Header:     http.Header(header),
			})
			return nil
		},
	}

	return httptrace.WithClientTrace(ctx, trace)
}

func dnsDuration(start, end time.Time) time.Duration {
	if start.IsZero() || end.IsZero() {
		return 0
	}
	return end.Sub(start)
}

func tcpDuration(start, end time.Time) time.Duration {
	if start.IsZero() || end.IsZero() {
		return 0
	}
	return end.Sub(start)
}

func tlsDuration(start, end time.Time) time.Duration {
	if start.IsZero() || end.IsZero() {
		return 0
	}
	return end.Sub(start)
}

// responseBodyReader wraps the response body to fire InData and the final
// End event. sync.Once guarantees exactly one End even if Read and Close
// are called concurrently.
type responseBodyReader struct {
	body    io.ReadCloser
	handler Handler
	ctx     context.Context
	start   time.Time
	endOnce sync.Once
}

func (r *responseBodyReader) Read(p []byte) (int, error) {
	n, err := r.body.Read(p)
	if n > 0 {
		r.handler.HandleRequest(r.ctx, &InData{Client: true, Length: n})
	}
	if err != nil {
		r.fireEndOnce(err)
	}
	return n, err //nolint:wrapcheck // returning Read error directly
}

func (r *responseBodyReader) Close() error {
	defer r.body.Close()
	r.fireEndOnce(nil)
	return nil
}

func (r *responseBodyReader) fireEndOnce(readErr error) {
	r.endOnce.Do(func() {
		var endErr error
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			endErr = readErr
		}
		r.handler.HandleRequest(r.ctx, &End{
			Client:  true,
			EndTime: time.Now(),
			Error:   endErr,
		})
	})
}
