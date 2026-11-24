package netutil

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"
)

// virtualAdapterKeywords lists common virtual, container, and VPN adapter name fragments to deprioritize/exclude.
var virtualAdapterKeywords = []string{
	"docker",
	"br-",
	"veth",
	"wsl",
	"hyper-v",
	"vethernet",
	"default switch",
	"vmnet",
	"vmware",
	"vboxnet",
	"virtualbox",
	"virbr",
	"tailscale",
	"zerotier",
	"hamachi",
	"tun",
	"tap",
	"utun",
	"wg",
	"wireguard",
	"loopback",
	"teredo",
	"isatap",
}

// preferredAdapterKeywords lists common physical Wi-Fi and Ethernet interface name fragments.
var preferredAdapterKeywords = []string{
	"wi-fi",
	"wifi",
	"wlan",
	"wlp",
	"en0",
	"en1",
	"eth",
	"enp",
	"eno",
	"ethernet",
}

type scoredIP struct {
	ip    string
	score int
}

// DetectLANIP returns the machine's primary local Wi-Fi or Ethernet IPv4 address.
// It prioritizes the OS default outbound route and falls back to scanning physical interfaces
// while filtering out loopback, Docker (172.17.x.x), WSL/Hyper-V virtual adapters, and link-local addresses.
func DetectLANIP() (string, error) {
	// 1. Primary strategy: ask the OS routing table which local IPv4 address reaches the default gateway.
	// UDP Dial does not transmit packets on the wire; it only resolves the outbound route.
	if ip := detectOutboundIPv4(); ip != "" {
		return ip, nil
	}

	// 2. Fallback strategy: enumerate network interfaces and score private LAN IPv4 addresses.
	ifaces, err := net.Interfaces()
	if err != nil {
		return "127.0.0.1", fmt.Errorf("failed to list network interfaces: %w", err)
	}

	var candidates []scoredIP
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		lowerName := strings.ToLower(iface.Name)
		if isVirtualAdapter(lowerName) {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			ip := extractIPv4(addr)
			if ip == nil || !isUsableLANIPv4(ip) {
				continue
			}

			score := scoreCandidate(ip, lowerName)
			candidates = append(candidates, scoredIP{
				ip:    ip.String(),
				score: score,
			})
		}
	}

	if len(candidates) == 0 {
		return "127.0.0.1", errors.New("no active local Wi-Fi/Ethernet IPv4 address found; falling back to 127.0.0.1")
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})

	return candidates[0].ip, nil
}

// ListenAvailable binds to 0.0.0.0:preferredPort, or automatically selects an ephemeral free port if busy.
func ListenAvailable(preferredPort int) (net.Listener, int, error) {
	if preferredPort > 0 && preferredPort <= 65535 {
		ln, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", preferredPort))
		if err == nil {
			return ln, preferredPort, nil
		}
	}

	ln, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		return nil, 0, fmt.Errorf("failed to bind TCP listener: %w", err)
	}

	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close()
		return nil, 0, errors.New("unexpected listener address type")
	}

	return ln, tcpAddr.Port, nil
}

func detectOutboundIPv4() string {
	targets := []string{"8.8.8.8:80", "1.1.1.1:80"}
	for _, target := range targets {
		conn, err := net.DialTimeout("udp4", target, 500*time.Millisecond)
		if err != nil {
			continue
		}
		localAddr, ok := conn.LocalAddr().(*net.UDPAddr)
		_ = conn.Close()
		if !ok || localAddr == nil {
			continue
		}
		ip := localAddr.IP.To4()
		if ip == nil || !isUsableLANIPv4(ip) {
			continue
		}
		// Reject if the outbound IP actually belongs to a known virtual adapter (e.g., WSL/VPN).
		if belongsToVirtualInterface(ip) {
			continue
		}
		return ip.String()
	}
	return ""
}

func belongsToVirtualInterface(target net.IP) bool {
	ifaces, err := net.Interfaces()
	if err != nil {
		return false
	}
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ip := extractIPv4(addr)
			if ip != nil && ip.Equal(target) {
				return isVirtualAdapter(strings.ToLower(iface.Name))
			}
		}
	}
	return false
}

func extractIPv4(addr net.Addr) net.IP {
	switch v := addr.(type) {
	case *net.IPNet:
		return v.IP.To4()
	case *net.IPAddr:
		return v.IP.To4()
	default:
		return nil
	}
}

func isVirtualAdapter(lowerName string) bool {
	for _, kw := range virtualAdapterKeywords {
		if strings.Contains(lowerName, kw) {
			return true
		}
	}
	return false
}

func isUsableLANIPv4(ip net.IP) bool {
	if ip == nil {
		return false
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	if ip4.IsLoopback() || ip4.IsLinkLocalUnicast() || ip4.IsLinkLocalMulticast() || ip4.IsMulticast() || ip4.IsUnspecified() {
		return false
	}
	// Exclude Docker's default bridge subnet (172.17.0.0/16).
	if ip4[0] == 172 && ip4[1] == 17 {
		return false
	}
	// Accept standard RFC 1918 private ranges: 192.168.0.0/16, 10.0.0.0/8, 172.16.0.0/12.
	if ip4[0] == 192 && ip4[1] == 168 {
		return true
	}
	if ip4[0] == 10 {
		return true
	}
	if ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31 {
		return true
	}
	return false
}

func scoreCandidate(ip net.IP, lowerName string) int {
	score := 0
	// Prefer 192.168.x.x (typical home/office Wi-Fi), then 10.x.x.x, then 172.16-31.x.x.
	switch {
	case ip[0] == 192 && ip[1] == 168:
		score += 100
	case ip[0] == 10:
		score += 80
	case ip[0] == 172 && ip[1] >= 16 && ip[1] <= 31:
		score += 50
	}

	for _, kw := range preferredAdapterKeywords {
		if strings.Contains(lowerName, kw) {
			score += 30
			break
		}
	}

	return score
}
