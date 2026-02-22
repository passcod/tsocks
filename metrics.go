package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"time"
)

type metrics struct {
	startTime   time.Time
	bytesUp     atomic.Int64
	bytesDown   atomic.Int64
	activeConns atomic.Int32
	totalConns  atomic.Int64
}

func newMetrics() *metrics {
	return &metrics{startTime: time.Now()}
}

// instrumentedDialer wraps a dialer to track bytes and connections through tsnet.
func (m *metrics) instrumentedDialer(dial func(ctx context.Context, network, addr string) (net.Conn, error)) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		m.activeConns.Add(1)
		m.totalConns.Add(1)
		return &countConn{Conn: conn, m: m}, nil
	}
}

// countConn wraps a net.Conn to count bytes and track lifetime.
type countConn struct {
	net.Conn
	m      *metrics
	closed atomic.Bool
}

func (c *countConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.m.bytesDown.Add(int64(n))
	}
	return n, err
}

func (c *countConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if n > 0 {
		c.m.bytesUp.Add(int64(n))
	}
	return n, err
}

func (c *countConn) Close() error {
	if c.closed.CompareAndSwap(false, true) {
		c.m.activeConns.Add(-1)
	}
	return c.Conn.Close()
}

func formatBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(b)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func formatDuration(d time.Duration) string {
	d = d.Truncate(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

// displayLoop updates a single status line on stderr once per second.
func (m *metrics) displayLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	var lastUp, lastDown int64

	for {
		select {
		case <-ctx.Done():
			fmt.Fprintf(os.Stderr, "\033[2K\rSession: %s │ ↑ %s │ ↓ %s │ %d connections\n",
				formatDuration(time.Since(m.startTime)),
				formatBytes(m.bytesUp.Load()),
				formatBytes(m.bytesDown.Load()),
				m.totalConns.Load(),
			)
			return
		case <-ticker.C:
			up := m.bytesUp.Load()
			down := m.bytesDown.Load()
			upRate := up - lastUp
			downRate := down - lastDown
			lastUp = up
			lastDown = down

			active := m.activeConns.Load()
			elapsed := time.Since(m.startTime)

			connStr := "idle"
			if active == 1 {
				connStr = "1 conn"
			} else if active > 1 {
				connStr = fmt.Sprintf("%d conns", active)
			}

			fmt.Fprintf(os.Stderr, "\033[2K\r⏱ %s │ ↑ %s (%s/s) │ ↓ %s (%s/s) │ %s",
				formatDuration(elapsed),
				formatBytes(up), formatBytes(upRate),
				formatBytes(down), formatBytes(downRate),
				connStr,
			)
		}
	}
}
