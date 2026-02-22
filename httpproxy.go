package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

type httpConnectProxy struct {
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (p *httpConnectProxy) Serve(ln net.Listener) error {
	srv := &http.Server{
		Handler: p,
	}
	return srv.Serve(ln)
}

func (p *httpConnectProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	p.handleHTTP(w, r)
}

func (p *httpConnectProxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if !strings.Contains(host, ":") {
		host += ":443"
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	upstream, err := p.dial(ctx, "tcp", host)
	if err != nil {
		http.Error(w, fmt.Sprintf("connect: %v", err), http.StatusBadGateway)
		return
	}
	defer upstream.Close()

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	client, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	defer client.Close()

	// Flush any buffered data
	if buf.Reader.Buffered() > 0 {
		buffered := make([]byte, buf.Reader.Buffered())
		buf.Read(buffered)
		upstream.Write(buffered)
	}

	go io.Copy(upstream, client)
	io.Copy(client, upstream)
}

func (p *httpConnectProxy) handleHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Host == "" {
		http.Error(w, "missing host", http.StatusBadRequest)
		return
	}

	host := r.URL.Host
	if !strings.Contains(host, ":") {
		host += ":80"
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	upstream, err := p.dial(ctx, "tcp", host)
	if err != nil {
		http.Error(w, fmt.Sprintf("connect: %v", err), http.StatusBadGateway)
		return
	}
	defer upstream.Close()

	// Forward the request
	r.RequestURI = ""
	r.Header.Del("Proxy-Connection")

	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return upstream, nil
		},
	}
	resp, err := transport.RoundTrip(r)
	if err != nil {
		http.Error(w, fmt.Sprintf("roundtrip: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}
