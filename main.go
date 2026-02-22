package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/charmbracelet/huh"
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

func main() {
	listenAddr := flag.String("listen", "localhost:1080", "SOCKS5 listen address")
	exitNode := flag.String("exit-node", "", "exit node IP, tailscale hostname, or \"last\"")
	hostname := flag.String("hostname", "tsocks", "tailscale hostname for this node")
	stateDirFlag := flag.String("state", "", "state directory (default: tsnet auto)")
	flag.Parse()

	stDir := stateDir(*hostname, *stateDirFlag)

	s := &tsnet.Server{
		Hostname: *hostname,
	}
	if *stateDirFlag != "" {
		s.Dir = *stateDirFlag
	}
	defer s.Close()

	log.Printf("Starting tsnet node %q...", *hostname)
	status, err := s.Up(context.Background())
	if err != nil {
		log.Fatalf("tsnet up: %v", err)
	}
	log.Printf("Tailscale node up: %s", status.TailscaleIPs[0])

	lc, err := s.LocalClient()
	if err != nil {
		log.Fatalf("local client: %v", err)
	}

	var exitNodeIP netip.Addr
	lastIP := loadLastExitNode(stDir)
	if *exitNode == "last" {
		if lastIP == "" {
			log.Fatal("no previously used exit node found")
		}
		ip, err := netip.ParseAddr(lastIP)
		if err != nil {
			log.Fatalf("invalid saved exit node %q: %v", lastIP, err)
		}
		exitNodeIP = ip
	} else if *exitNode != "" {
		// Resolve exit node: try as IP first, then look up by hostname
		if ip, err := netip.ParseAddr(*exitNode); err == nil {
			exitNodeIP = ip
		} else {
			st, err := lc.Status(context.Background())
			if err != nil {
				log.Fatalf("get status: %v", err)
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
				log.Fatalf("exit node %q not found among peers", *exitNode)
			}
		}
	} else {
		exitNodeIP, err = pickExitNode(lc, lastIP)
		if err != nil {
			log.Fatalf("pick exit node: %v", err)
		}
	}

	saveLastExitNode(stDir, exitNodeIP)

	log.Printf("Setting exit node to %s...", exitNodeIP)
	mp := &ipn.MaskedPrefs{
		Prefs: ipn.Prefs{
			ExitNodeIP: exitNodeIP,
		},
		ExitNodeIPSet: true,
	}
	if _, err := lc.EditPrefs(context.Background(), mp); err != nil {
		log.Fatalf("set exit node: %v", err)
	}

	ln, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}

	// Graceful shutdown on signal
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		cancel()
		ln.Close()
	}()

	log.Printf("SOCKS5 proxy listening on %s (exit node %s)", *listenAddr, exitNodeIP)

	m := newMetrics()
	go m.displayLoop(ctx)

	srv := &socks5.Server{
		Dialer: m.instrumentedDialer(s.Dial),
	}

	if err := srv.Serve(ln); err != nil && ctx.Err() == nil {
		log.Fatalf("socks5 serve: %v", err)
	}
}
