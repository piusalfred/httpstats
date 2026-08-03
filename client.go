// MIT License
//
// Copyright (c) 2026 Pius Alfred
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package httpstats

import (
	"context"
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

func WithClientTransport(rt http.RoundTripper) ClientOption {
	return func(c *clientConfig) { c.rt = rt }
}

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

func WithClientNameFunc(f func(*RequestTagInfo)) ClientOption {
	return func(c *clientConfig) { c.nameFunc = f }
}

type ClientTransport struct {
	rt       http.RoundTripper
	handler  Handler
	nameFunc func(*RequestTagInfo)
}

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

	ev := NewEvent()
	ev.Type = OpBegin
	ev.Client = true
	ev.StartTime = startTime
	ct.handler.HandleRequest(ctx, ev)
	PutEvent(ev)

	ctx = ct.attachTrace(ctx, req)
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
		ev2 := NewEvent()
		ev2.Type = OpEnd
		ev2.Client = true
		ev2.EndTime = time.Now()
		ev2.Error = err
		ct.handler.HandleRequest(ctx, ev2)
		PutEvent(ev2)
		return nil, err //nolint:wrapcheck // returning RoundTripper error directly
	}

	ev = NewEvent()
	ev.Type = OpInHeader
	ev.Client = true
	ev.StatusCode = resp.StatusCode
	ev.Header = resp.Header.Clone()
	ev.ContentLength = resp.ContentLength
	ev.Method = req.Method
	ct.handler.HandleRequest(ctx, ev)
	PutEvent(ev)

	resp.Body = &responseBodyReader{
		body:    resp.Body,
		handler: ct.handler,
		ctx:     ctx,
		start:   startTime,
	}

	return resp, nil
}

func (ct *ClientTransport) attachTrace(ctx context.Context, req *http.Request) context.Context {
	var gotConnReused bool
	var connCtx context.Context

	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			gotConnReused = info.Reused
			if !info.Reused {
				connCtx = ct.handler.TagConn( //nolint:fatcontext // connection context captured from parent
					ctx,
					&ConnTagInfo{
						Client:     true,
						RemoteAddr: info.Conn.RemoteAddr(),
					},
				)
				ct.handler.HandleConn(connCtx, &ConnEvent{
					Type:       ConnOpBegin,
					Client:     true,
					StartTime:  time.Now(),
					RemoteAddr: info.Conn.RemoteAddr().String(),
				})
			}
		},
		WroteHeaders: func() {
			ev := NewEvent()
			ev.Type = OpOutHeader
			ev.Client = true
			ev.Header = req.Header.Clone()
			ev.ContentLength = req.ContentLength
			ct.handler.HandleRequest(ctx, ev)
			PutEvent(ev)
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err != nil {
				ev := NewEvent()
				ev.Type = OpOutData
				ev.Client = true
				ev.Error = info.Err
				ct.handler.HandleRequest(ctx, ev)
				PutEvent(ev)
			}
		},
		PutIdleConn: func(err error) {
			if !gotConnReused || err != nil {
				if connCtx == nil {
					connCtx = ctx //nolint:fatcontext // fallback to request context
				}
				ct.handler.HandleConn(connCtx, &ConnEvent{Type: ConnOpEnd, Client: true, Error: err})
			}
		},
		Got100Continue: func() {},
		Got1xxResponse: func(_ int, _ textproto.MIMEHeader) error { return nil },
	}

	return httptrace.WithClientTrace(ctx, trace)
}

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
		ev := NewEvent()
		ev.Type = OpInData
		ev.Client = true
		ev.Length = n
		r.handler.HandleRequest(r.ctx, ev)
		PutEvent(ev)
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
		ev := NewEvent()
		ev.Type = OpEnd
		ev.Client = true
		ev.EndTime = time.Now()
		ev.Error = endErr
		r.handler.HandleRequest(r.ctx, ev)
		PutEvent(ev)
	})
}
