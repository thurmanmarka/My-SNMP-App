package main

import (
	"fmt"
	"net"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-ping/ping"
)

// ScanResult represents the result of scanning a single IP
type ScanResult struct {
	IP      string
	IsUp    bool
	Latency time.Duration
}

// ScanSubnet performs ICMP ping sweep of the given subnet
func ScanSubnet(subnet string, timeout time.Duration, workers int) ([]ScanResult, error) {
	// Parse CIDR
	ip, ipnet, err := net.ParseCIDR(subnet)
	if err != nil {
		return nil, fmt.Errorf("invalid subnet: %w", err)
	}

	// Generate list of IPs in subnet
	var ips []string
	for ip := ip.Mask(ipnet.Mask); ipnet.Contains(ip); incIP(ip) {
		ips = append(ips, ip.String())
	}

	// Remove network and broadcast addresses
	if len(ips) > 2 {
		ips = ips[1 : len(ips)-1]
	}

	fmt.Printf("Scanning %d hosts in %s...\n", len(ips), subnet)

	// Channel for work distribution
	ipChan := make(chan string, len(ips))
	resultChan := make(chan ScanResult, len(ips))

	// Start workers
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ip := range ipChan {
				result := pingHost(ip, timeout)
				resultChan <- result
			}
		}()
	}

	// Send work to workers
	for _, ip := range ips {
		ipChan <- ip
	}
	close(ipChan)

	// Wait for all workers to finish
	go func() {
		wg.Wait()
		close(resultChan)
	}()

	// Collect results
	var results []ScanResult
	for result := range resultChan {
		results = append(results, result)
	}

	return results, nil
}

// pingHost pings a single host. On Windows it uses the built-in ping command
// (no admin required); on Linux/macOS it uses the ICMP library. Both fall back
// to TCP probes on common ports when ping fails.
func pingHost(ip string, timeout time.Duration) ScanResult {
	if runtime.GOOS == "windows" {
		return pingHostWindows(ip, timeout)
	}

	// ICMP via library (Linux/macOS — works without admin there)
	pinger, err := ping.NewPinger(ip)
	if err == nil {
		pinger.Count = 1
		pinger.Timeout = timeout
		pinger.SetPrivileged(true)
		if err = pinger.Run(); err == nil {
			if stats := pinger.Statistics(); stats.PacketsRecv > 0 {
				return ScanResult{IP: ip, IsUp: true, Latency: stats.AvgRtt}
			}
		}
	}

	return pingHostTCP(ip, timeout)
}

// pingHostWindows uses the OS ping command, which works without admin privileges.
func pingHostWindows(ip string, timeout time.Duration) ScanResult {
	ms := strconv.Itoa(int(timeout.Milliseconds()))
	start := time.Now()
	out, err := exec.Command("ping", "-n", "1", "-w", ms, ip).Output()
	if err == nil && strings.Contains(string(out), "TTL=") {
		return ScanResult{IP: ip, IsUp: true, Latency: time.Since(start)}
	}
	return pingHostTCP(ip, timeout)
}

// pingHostTCP probes common TCP ports as a last-resort host-up check.
// Port 161 is intentionally excluded — SNMP is UDP and will never respond to TCP.
func pingHostTCP(ip string, timeout time.Duration) ScanResult {
	start := time.Now()
	for _, port := range []int{80, 443, 22, 8080, 8443} {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", ip, port), timeout/5)
		if err == nil {
			conn.Close()
			return ScanResult{IP: ip, IsUp: true, Latency: time.Since(start)}
		}
	}
	return ScanResult{IP: ip, IsUp: false}
}

// incIP increments an IP address
func incIP(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] > 0 {
			break
		}
	}
}
