package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"sync"
	"strings"
	"syscall"

	"time"

	"github.com/charmbracelet/huh"
	flag "github.com/spf13/pflag"
	"tailscale.com/client/local"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/net/socks5"
	"tailscale.com/tsnet"
)

const lastExitNodeFile = "last-exit-node"

func stateDir(hostname, explicitDir string) string {
	if explicitDir != "" {
		return explicitDir
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "tsnet-"+hostname)
}

func loadLastExitNode(dir string) string {
	if dir == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(dir, lastExitNodeFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func saveLastExitNode(dir string, ip netip.Addr) {
	if dir == "" {
		return
	}
	os.WriteFile(filepath.Join(dir, lastExitNodeFile), []byte(ip.String()+"\n"), 0644)
}

func hasStateFile(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "tailscaled.state"))
	return err == nil
}

type switchableWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *switchableWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func (s *switchableWriter) switchTo(w io.Writer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.w = w
}

func exitNodeLabel(peer *ipnstate.PeerStatus) string {
	label := peer.HostName
	if peer.Location != nil {
		parts := []string{}
		if peer.Location.City != "" {
			parts = append(parts, peer.Location.City)
		}
		if peer.Location.Country != "" {
			parts = append(parts, peer.Location.Country)
		}
		if len(parts) > 0 {
			label += " (" + strings.Join(parts, ", ") + ")"
		}
	}
	if !peer.Online {
		label += " [offline]"
	}
	return label
}

func exitNodeHostname(lc *local.Client, ip netip.Addr) string {
	st, err := lc.Status(context.Background())
	if err != nil {
		return ip.String()
	}
	for _, peer := range st.Peer {
		for _, pip := range peer.TailscaleIPs {
			if pip == ip {
				return peer.HostName
			}
		}
	}
	return ip.String()
}

func exitNodeName(lc *local.Client, ip netip.Addr) string {
	st, err := lc.Status(context.Background())
	if err != nil {
		return ip.String()
	}
	for _, peer := range st.Peer {
		for _, pip := range peer.TailscaleIPs {
			if pip == ip {
				return exitNodeLabel(peer) + " " + ip.String()
			}
		}
	}
	return ip.String()
}

func pickExitNode(lc *local.Client, lastIP string) (netip.Addr, error) {
	st, err := lc.Status(context.Background())
	if err != nil {
		return netip.Addr{}, fmt.Errorf("get status: %w", err)
	}

	type exitOption struct {
		peer  *ipnstate.PeerStatus
		label string
		ip    string
	}

	var opts []exitOption
	for _, peer := range st.Peer {
		if !peer.ExitNodeOption || len(peer.TailscaleIPs) == 0 {
			continue
		}
		opts = append(opts, exitOption{
			peer:  peer,
			label: exitNodeLabel(peer),
			ip:    peer.TailscaleIPs[0].String(),
		})
	}

	if len(opts) == 0 {
		return netip.Addr{}, fmt.Errorf("no exit nodes available in tailnet")
	}

	// Sort: online first, then by label
	sort.Slice(opts, func(i, j int) bool {
		if opts[i].peer.Online != opts[j].peer.Online {
			return opts[i].peer.Online
		}
		return opts[i].label < opts[j].label
	})

	huhOpts := make([]huh.Option[string], len(opts))
	for i, o := range opts {
		huhOpts[i] = huh.NewOption(o.label, o.ip)
	}

	var selected string
	if lastIP != "" {
		selected = lastIP
	}
	err = huh.NewSelect[string]().
		Title("Select exit node").
		Options(huhOpts...).
		Height(min(len(huhOpts)+2, 20)).
		Filtering(true).
		Value(&selected).
		Run()
	if err != nil {
		return netip.Addr{}, err
	}

	ip, err := netip.ParseAddr(selected)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("parse selected IP: %w", err)
	}
	return ip, nil
}

func probeIPs(dial func(ctx context.Context, network, addr string) (net.Conn, error)) (v4, v6 string) {
	transport := &http.Transport{}
	if dial != nil {
		transport.DialContext = dial
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   5 * time.Second,
	}
	probe := func(url string) string {
		resp, err := client.Get(url)
		if err != nil {
			return ""
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
		if err != nil {
			return ""
		}
		ip := strings.TrimSpace(string(body))
		if _, err := netip.ParseAddr(ip); err != nil {
			return ""
		}
		return ip
	}

	ch4 := make(chan string, 1)
	ch6 := make(chan string, 1)
	go func() { ch4 <- probe("https://api4.ipify.org") }()
	go func() { ch6 <- probe("https://api6.ipify.org") }()
	return <-ch4, <-ch6
}

func main() {
	listenAddr := flag.String("listen", "localhost:1080", "SOCKS5 listen address")
	i2pAddr := flag.String("i2p-listen", "localhost:4444", "I2P-compatible HTTP proxy listen address")
	exitNode := flag.String("exit-node", "", "exit node IP or tailscale hostname (default: last used)")
	hostname := flag.String("hostname", "tsocks", "tailscale hostname for this node")
	stateDirFlag := flag.String("state", "", "state directory (default: tsnet auto)")
	flag.Parse()

	stDir := stateDir(*hostname, *stateDirFlag)
	os.MkdirAll(stDir, 0700)

	// Send tsnet verbose logs to a file, not the console
	logFile, err := os.Create(filepath.Join(stDir, "tsnet.log"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "create log file: %v\n", err)
		os.Exit(1)
	}
	defer logFile.Close()

	// If already authenticated, logs go straight to file.
	// Otherwise, tee to stderr so the user sees the auth URL,
	// then switch to file-only once Up() returns.
	needsAuth := !hasStateFile(stDir)
	sw := &switchableWriter{w: logFile}
	if needsAuth {
		sw.w = io.MultiWriter(os.Stderr, logFile)
	}
	tsnetLog := log.New(sw, "", log.LstdFlags)
	log.SetOutput(sw)

	s := &tsnet.Server{
		Hostname: *hostname,
		Logf:     tsnetLog.Printf,
		UserLogf: tsnetLog.Printf,
	}
	if *stateDirFlag != "" {
		s.Dir = *stateDirFlag
	}
	defer s.Close()

	fmt.Fprintf(os.Stderr, "Starting tsnet node %q...\n", *hostname)
	status, err := s.Up(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "tsnet up: %v\n", err)
		os.Exit(1)
	}

	// Auth complete — switch logs to file only
	if needsAuth {
		fmt.Fprintln(os.Stderr)
	}
	sw.switchTo(logFile)

	fmt.Fprintf(os.Stderr, "Tailscale node up: %s\n", status.TailscaleIPs[0])

	lc, err := s.LocalClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "local client: %v\n", err)
		os.Exit(1)
	}

	var exitNodeIP netip.Addr
	lastIP := loadLastExitNode(stDir)
	if *exitNode != "" {
		// Resolve exit node: try as IP first, then look up by hostname
		if ip, err := netip.ParseAddr(*exitNode); err == nil {
			exitNodeIP = ip
		} else {
			st, err := lc.Status(context.Background())
			if err != nil {
				fmt.Fprintf(os.Stderr, "get status: %v\n", err)
				os.Exit(1)
			}
			target := strings.TrimSuffix(strings.ToLower(*exitNode), ".")
			for _, peer := range st.Peer {
				name := strings.TrimSuffix(strings.ToLower(peer.HostName), ".")
				dns := strings.TrimSuffix(strings.ToLower(peer.DNSName), ".")
				if name == target || dns == target {
					if len(peer.TailscaleIPs) > 0 {
						exitNodeIP = peer.TailscaleIPs[0]
						break
					}
				}
			}
			if !exitNodeIP.IsValid() {
				fmt.Fprintf(os.Stderr, "exit node %q not found among peers\n", *exitNode)
				os.Exit(1)
			}
		}
	} else if lastIP != "" {
		// Default to last-used exit node
		ip, err := netip.ParseAddr(lastIP)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid saved exit node %q, showing picker\n", lastIP)
			exitNodeIP, err = pickExitNode(lc, "")
			if err != nil {
				fmt.Fprintf(os.Stderr, "pick exit node: %v\n", err)
				os.Exit(1)
			}
		} else {
			exitNodeIP = ip
		}
	} else {
		exitNodeIP, err = pickExitNode(lc, "")
		if err != nil {
			fmt.Fprintf(os.Stderr, "pick exit node: %v\n", err)
			os.Exit(1)
		}
	}

	saveLastExitNode(stDir, exitNodeIP)
	exitNodeDesc := exitNodeName(lc, exitNodeIP)

	fmt.Fprintf(os.Stderr, "Setting exit node to %s...\n", exitNodeDesc)
	mp := &ipn.MaskedPrefs{
		Prefs: ipn.Prefs{
			ExitNodeIP: exitNodeIP,
		},
		ExitNodeIPSet: true,
	}
	if _, err := lc.EditPrefs(context.Background(), mp); err != nil {
		fmt.Fprintf(os.Stderr, "set exit node: %v\n", err)
		os.Exit(1)
	}

	// Remove inline IP probe — handled by displayLoop now
	m := newMetrics()
	m.probeIPs = func() (string, string) { return probeIPs(s.Dial) }
	m.probeDirectIPs = func() (string, string) { return probeIPs(nil) }
	m.setExitNode(exitNodeHostname(lc, exitNodeIP))

	dial := m.instrumentedDialer(s.Dial)
	var lnMu sync.Mutex
	var socksLn, httpLn net.Listener

	socksSrv := &socks5.Server{
		Dialer: dial,
	}

	httpProxy := &httpConnectProxy{dial: dial}

	startServing := func() error {
		l, err := net.Listen("tcp", *listenAddr)
		if err != nil {
			return err
		}
		h, err := net.Listen("tcp", *i2pAddr)
		if err != nil {
			l.Close()
			return err
		}
		lnMu.Lock()
		socksLn = l
		httpLn = h
		lnMu.Unlock()
		go socksSrv.Serve(l)
		go httpProxy.Serve(h)
		return nil
	}

	stopServing := func() {
		lnMu.Lock()
		if socksLn != nil {
			socksLn.Close()
			socksLn = nil
		}
		if httpLn != nil {
			httpLn.Close()
			httpLn = nil
		}
		lnMu.Unlock()
		m.closeAllConns()
	}

	if err := startServing(); err != nil {
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		os.Exit(1)
	}

	// Graceful shutdown on signal or keypress
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		select {
		case <-sig:
		case <-ctx.Done():
		}
		cancel()
		stopServing()
	}()

	fmt.Fprintf(os.Stderr, "SOCKS5 proxy on %s │ HTTP proxy on %s (exit node %s)\n\n\n", *listenAddr, *i2pAddr, exitNodeDesc)

	togglePause := func() {
		if m.paused.Load() {
			if err := startServing(); err != nil {
				return
			}
			m.unpause()
		} else {
			m.pause()
			stopServing()
		}
		go m.refreshExternalIPs()
	}

	switchExitNode := func() {
		newIP, err := pickExitNode(lc, exitNodeIP.String())
		if err != nil {
			fmt.Fprintf(os.Stderr, "switch exit node: %v\n", err)
			return
		}
		fmt.Fprintf(os.Stderr, "Switching exit node to %s...", exitNodeName(lc, newIP))
		mp := &ipn.MaskedPrefs{
			Prefs: ipn.Prefs{
				ExitNodeIP: newIP,
			},
			ExitNodeIPSet: true,
		}
		if _, err := lc.EditPrefs(context.Background(), mp); err != nil {
			fmt.Fprintf(os.Stderr, " failed: %v\n", err)
			return
		}
		exitNodeIP = newIP
		saveLastExitNode(stDir, exitNodeIP)
		m.setExitNode(exitNodeHostname(lc, exitNodeIP))
		fmt.Fprintf(os.Stderr, " done")
	}

	go m.readKeys(cancel, togglePause, switchExitNode)
	go m.displayLoop(ctx)

	<-ctx.Done()
}
