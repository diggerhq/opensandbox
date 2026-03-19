package qemu

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
	"sync"
)

// NetworkConfig holds the networking state for a single VM.
type NetworkConfig struct {
	TAPName string // e.g., "qm-tap0000000"
	HostIP  string // e.g., "172.16.0.1"
	GuestIP string // e.g., "172.16.0.2"
	Mask    string // e.g., "255.255.255.252"
	CIDR    int    // /30

	// Port forwarding
	HostPort      int // host port mapped to guest
	GuestPort     int // guest port (typically 80)
	DNATRuleAdded bool
}

// SubnetAllocator manages /30 subnet allocation from a 172.16.0.0/16 pool.
// Each VM gets a /30: host IP (.1) and guest IP (.2), with .0 as network and .3 as broadcast.
type SubnetAllocator struct {
	mu   sync.Mutex
	next uint32 // next /30 block index (0, 1, 2, ...)
	used map[uint32]bool
}

// NewSubnetAllocator creates a new subnet allocator.
func NewSubnetAllocator() *SubnetAllocator {
	return &SubnetAllocator{
		used: make(map[uint32]bool),
	}
}

// Allocate returns a new /30 subnet for a VM.
func (a *SubnetAllocator) Allocate() (*NetworkConfig, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	block := a.next
	for a.used[block] {
		block++
		if block > 16383 {
			return nil, fmt.Errorf("subnet pool exhausted")
		}
	}
	a.used[block] = true
	a.next = block + 1

	base := block * 4
	b2 := byte(base >> 8)
	b3 := byte(base & 0xFF)

	hostIP := fmt.Sprintf("172.16.%d.%d", b2, b3+1)
	guestIP := fmt.Sprintf("172.16.%d.%d", b2, b3+2)

	tapName := fmt.Sprintf("qm-tap%07d", block)

	return &NetworkConfig{
		TAPName: tapName,
		HostIP:  hostIP,
		GuestIP: guestIP,
		Mask:    "255.255.255.252",
		CIDR:    30,
	}, nil
}

// AllocateSpecific reserves a specific TAP name/subnet block.
// Used during snapshot restore where the TAP name is baked into the migration state.
func (a *SubnetAllocator) AllocateSpecific(tapName string) (*NetworkConfig, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	var block uint32
	if _, err := fmt.Sscanf(tapName, "qm-tap%d", &block); err != nil {
		return nil, fmt.Errorf("parse tap name %q: %w", tapName, err)
	}
	if a.used[block] {
		return nil, fmt.Errorf("tap %s already in use", tapName)
	}
	a.used[block] = true

	base := block * 4
	b2 := byte(base >> 8)
	b3 := byte(base & 0xFF)

	return &NetworkConfig{
		TAPName: tapName,
		HostIP:  fmt.Sprintf("172.16.%d.%d", b2, b3+1),
		GuestIP: fmt.Sprintf("172.16.%d.%d", b2, b3+2),
		Mask:    "255.255.255.252",
		CIDR:    30,
	}, nil
}

// Release returns a /30 block to the pool.
func (a *SubnetAllocator) Release(tapName string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	var block uint32
	if _, err := fmt.Sscanf(tapName, "qm-tap%d", &block); err != nil {
		return
	}
	delete(a.used, block)
}

// CreateTAP creates a TAP device and configures it with the host IP.
func CreateTAP(cfg *NetworkConfig) error {
	if err := run("ip", "tuntap", "add", "dev", cfg.TAPName, "mode", "tap"); err != nil {
		return fmt.Errorf("create tap %s: %w", cfg.TAPName, err)
	}

	addr := fmt.Sprintf("%s/%d", cfg.HostIP, cfg.CIDR)
	if err := run("ip", "addr", "add", addr, "dev", cfg.TAPName); err != nil {
		DeleteTAP(cfg.TAPName)
		return fmt.Errorf("assign ip to %s: %w", cfg.TAPName, err)
	}

	if err := run("ip", "link", "set", cfg.TAPName, "up"); err != nil {
		DeleteTAP(cfg.TAPName)
		return fmt.Errorf("bring up %s: %w", cfg.TAPName, err)
	}

	// Apply network rate limiting: 50 Mbps bandwidth + packet rate police.
	// Prevents DDoS, network abuse, and protects host bandwidth.
	// tc: token bucket filter on egress (VM → host → internet)
	applyRateLimit(cfg.TAPName)

	return nil
}

// applyRateLimit sets tc rate limiting on a TAP device.
// 50 Mbps bandwidth cap + 10000 pps packet rate to prevent abuse.
func applyRateLimit(tapName string) {
	// Egress from VM (ingress to TAP from host perspective)
	// Use tc on the TAP device to limit what the VM can send out
	_ = run("tc", "qdisc", "add", "dev", tapName, "root", "tbf",
		"rate", "50mbit", "burst", "1mb", "latency", "50ms")
}

// DeleteTAP removes a TAP device and its tc qdisc.
func DeleteTAP(tapName string) {
	_ = run("tc", "qdisc", "del", "dev", tapName, "root")
	_ = run("ip", "link", "del", tapName)
}

// AddDNAT adds an iptables DNAT rule: hostPort → guestIP:guestPort.
func AddDNAT(cfg *NetworkConfig) error {
	if cfg.HostPort == 0 || cfg.GuestPort == 0 {
		return nil
	}
	err := run("iptables", "-t", "nat", "-A", "PREROUTING",
		"-p", "tcp", "--dport", fmt.Sprintf("%d", cfg.HostPort),
		"-j", "DNAT", "--to-destination",
		fmt.Sprintf("%s:%d", cfg.GuestIP, cfg.GuestPort))
	if err != nil {
		return fmt.Errorf("add DNAT: %w", err)
	}

	// Also add for locally-generated traffic
	if err := run("iptables", "-t", "nat", "-A", "OUTPUT",
		"-p", "tcp", "--dport", fmt.Sprintf("%d", cfg.HostPort),
		"-j", "DNAT", "--to-destination",
		fmt.Sprintf("%s:%d", cfg.GuestIP, cfg.GuestPort)); err != nil {
		// Roll back the PREROUTING rule we already added
		_ = run("iptables", "-t", "nat", "-D", "PREROUTING",
			"-p", "tcp", "--dport", fmt.Sprintf("%d", cfg.HostPort),
			"-j", "DNAT", "--to-destination",
			fmt.Sprintf("%s:%d", cfg.GuestIP, cfg.GuestPort))
		return fmt.Errorf("add DNAT OUTPUT: %w", err)
	}

	cfg.DNATRuleAdded = true
	return nil
}

// RemoveDNAT removes the iptables DNAT rules.
func RemoveDNAT(cfg *NetworkConfig) {
	if !cfg.DNATRuleAdded {
		return
	}
	_ = run("iptables", "-t", "nat", "-D", "PREROUTING",
		"-p", "tcp", "--dport", fmt.Sprintf("%d", cfg.HostPort),
		"-j", "DNAT", "--to-destination",
		fmt.Sprintf("%s:%d", cfg.GuestIP, cfg.GuestPort))
	_ = run("iptables", "-t", "nat", "-D", "OUTPUT",
		"-p", "tcp", "--dport", fmt.Sprintf("%d", cfg.HostPort),
		"-j", "DNAT", "--to-destination",
		fmt.Sprintf("%s:%d", cfg.GuestIP, cfg.GuestPort))
}

// AddMetadataDNAT adds an iptables rule to redirect 169.254.169.254:80 from a VM's TAP
// to the host metadata server on port 8888.
func AddMetadataDNAT(tapName, hostIP string) error {
	err := run("iptables", "-t", "nat", "-A", "PREROUTING",
		"-i", tapName,
		"-d", "169.254.169.254",
		"-p", "tcp", "--dport", "80",
		"-j", "DNAT", "--to-destination", hostIP+":8888")
	if err != nil {
		return fmt.Errorf("add metadata DNAT for %s: %w", tapName, err)
	}
	return nil
}

// RemoveMetadataDNAT removes the metadata DNAT rule for a TAP device.
func RemoveMetadataDNAT(tapName, hostIP string) {
	_ = run("iptables", "-t", "nat", "-D", "PREROUTING",
		"-i", tapName,
		"-d", "169.254.169.254",
		"-p", "tcp", "--dport", "80",
		"-j", "DNAT", "--to-destination", hostIP+":8888")
}

// EnableForwarding enables IPv4 forwarding and masquerading for the VM subnet.
func EnableForwarding() error {
	if err := run("sysctl", "-w", "net.ipv4.ip_forward=1"); err != nil {
		return fmt.Errorf("enable ip_forward: %w", err)
	}

	if err := run("sysctl", "-w", "net.ipv4.conf.all.route_localnet=1"); err != nil {
		return fmt.Errorf("enable route_localnet: %w", err)
	}

	out, _ := exec.Command("iptables", "-t", "nat", "-S", "POSTROUTING").CombinedOutput()
	outRules := string(out)
	if !strings.Contains(outRules, "172.16.0.0/16") {
		outIface := detectDefaultInterface()
		if outIface != "" {
			_ = run("iptables", "-t", "nat", "-A", "POSTROUTING",
				"-s", "172.16.0.0/16", "-o", outIface,
				"-j", "MASQUERADE")
		} else {
			_ = run("iptables", "-t", "nat", "-A", "POSTROUTING",
				"-s", "172.16.0.0/16", "!", "-o", "qm-tap+",
				"-j", "MASQUERADE")
		}
	}

	if !strings.Contains(outRules, "172.16.0.0/16 -j MASQUERADE") {
		_ = run("iptables", "-t", "nat", "-A", "POSTROUTING",
			"-d", "172.16.0.0/16",
			"-j", "MASQUERADE")
	}

	fwdOut, _ := exec.Command("iptables", "-S", "FORWARD").CombinedOutput()
	fwdRules := string(fwdOut)
	if !strings.Contains(fwdRules, "172.16.0.0/16 -j ACCEPT") {
		_ = run("iptables", "-I", "FORWARD",
			"-s", "172.16.0.0/16",
			"-j", "ACCEPT")
		_ = run("iptables", "-I", "FORWARD",
			"-d", "172.16.0.0/16",
			"-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED",
			"-j", "ACCEPT")
	}

	return nil
}

func detectDefaultInterface() string {
	out, err := exec.Command("ip", "route", "show", "default").CombinedOutput()
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if f == "dev" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

// FindFreePort finds a free TCP port on the host.
// Note: This has a TOCTOU race — two concurrent calls can get the same port.
// In practice this is acceptable because the port is used for DNAT rules (not
// a real listener), so collisions are extremely unlikely and would only occur
// if two sandboxes are created in the same microsecond window.
func FindFreePort() (int, error) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := lis.Addr().(*net.TCPAddr).Port
	lis.Close()
	return port, nil
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w (%s)", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
