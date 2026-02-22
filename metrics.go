package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/term"
)

type metrics struct {
	startTime   time.Time
	elapsed     time.Duration // accumulated time before last pause
	bytesUp     atomic.Int64
	bytesDown   atomic.Int64
	activeConns atomic.Int32
	totalConns  atomic.Int64
	hideDisplay atomic.Bool
	paused      atomic.Bool
	mu          sync.Mutex
	connSet     map[*countConn]struct{}
}

func newMetrics() *metrics {
	return &metrics{
		startTime: time.Now(),
		connSet:   make(map[*countConn]struct{}),
	}
}

func (m *metrics) reset() {
	m.startTime = time.Now()
	m.elapsed = 0
	m.bytesUp.Store(0)
	m.bytesDown.Store(0)
	m.totalConns.Store(0)
}

func (m *metrics) pause() {
	m.elapsed += time.Since(m.startTime)
	m.paused.Store(true)
}

func (m *metrics) unpause() {
	m.startTime = time.Now()
	m.paused.Store(false)
}

func (m *metrics) sessionDuration() time.Duration {
	if m.paused.Load() {
		return m.elapsed
	}
	return m.elapsed + time.Since(m.startTime)
}

func (m *metrics) closeAllConns() {
	m.mu.Lock()
	conns := make([]*countConn, 0, len(m.connSet))
	for c := range m.connSet {
		conns = append(conns, c)
	}
	m.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
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
		cc := &countConn{Conn: conn, m: m}
		m.mu.Lock()
		m.connSet[cc] = struct{}{}
		m.mu.Unlock()
		return cc, nil
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
		c.m.mu.Lock()
		delete(c.m.connSet, c)
		c.m.mu.Unlock()
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

// readKeys reads single keypresses from stdin in raw mode.
func (m *metrics) readKeys(cancel context.CancelFunc, onPause func(), onSwitch func()) {
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	buf := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil || n == 0 {
			return
		}
		switch buf[0] {
		case 'q', 'Q', 0x03: // q or Ctrl-C
			cancel()
			return
		case 'r', 'R':
			m.reset()
		case 'p', 'P':
			onPause()
		case 's', 'S':
			m.hideDisplay.Store(true)
			// Clear display lines for picker
			fmt.Fprintf(os.Stderr, "\033[2K\r\n\033[2K\r\033[A")
			term.Restore(int(os.Stdin.Fd()), oldState)
			onSwitch()
			fmt.Fprintln(os.Stderr)
			oldState, _ = term.MakeRaw(int(os.Stdin.Fd()))
			m.hideDisplay.Store(false)
		}
	}
}

const helpLine = "q quit │ r reset │ p pause │ s switch exit node"

// displayLoop updates a two-line status display on stderr once per second.
func (m *metrics) displayLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	var lastUp, lastDown int64

	for {
		select {
		case <-ctx.Done():
			// Clear the two display lines, then print final summary
			fmt.Fprintf(os.Stderr, "\033[2K\r\033[A\033[2K\r")
			fmt.Fprintf(os.Stderr, "Session: %s │ ↑ %s │ ↓ %s │ %d connections\n",
				formatDuration(m.sessionDuration()),
				formatBytes(m.bytesUp.Load()),
				formatBytes(m.bytesDown.Load()),
				m.totalConns.Load(),
			)
			return
		case <-ticker.C:
			if m.hideDisplay.Load() {
				continue
			}

			up := m.bytesUp.Load()
			down := m.bytesDown.Load()
			upRate := up - lastUp
			downRate := down - lastDown
			lastUp = up
			lastDown = down

			elapsed := m.sessionDuration()
			active := m.activeConns.Load()

			connStr := "idle"
			if active == 1 {
				connStr = "1 conn"
			} else if active > 1 {
				connStr = fmt.Sprintf("%d conns", active)
			}

			status := ""
			if m.paused.Load() {
				status = "PAUSED │ "
			}

			fmt.Fprintf(os.Stderr, "\033[2K\r%s\n\033[2K\r%s⏱ %s │ ↑ %s (%s/s) │ ↓ %s (%s/s) │ %s\033[A\r",
				helpLine,
				status,
				formatDuration(elapsed),
				formatBytes(up), formatBytes(upRate),
				formatBytes(down), formatBytes(downRate),
				connStr,
			)
		}
	}
}
