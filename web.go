package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

var (
	// In-memory device cache
	deviceCache      *DeviceDatabase
	deviceCacheMutex sync.RWMutex
	lastScanTime     time.Time

	scanStatusMutex sync.RWMutex
	scanStatus      ScanStatus
)

type ScanStatus struct {
	Running    bool      `json:"running"`
	Progress   int       `json:"progress"`
	Total      int       `json:"total"`
	Message    string    `json:"message"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

// StartWebServer starts the HTTP server for the web GUI
func StartWebServer(config *Config, port int) error {
	// Initialize device cache
	db, err := LoadDevices(config.Output.DevicesFile)
	if err != nil {
		return fmt.Errorf("failed to load devices: %w", err)
	}
	deviceCache = db

	// Routes
	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/api/devices", handleAPIDevices)
	http.HandleFunc("/api/scan", handleAPIScan)
	http.HandleFunc("/api/scan-status", handleAPIScanStatus)
	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	http.HandleFunc("/health", handleHealth)

	addr := fmt.Sprintf(":%d", port)
	log.Printf("Starting web server on http://localhost%s", addr)
	return http.ListenAndServe(addr, nil)
}

// handleHealth responds with 200 OK for service checks
func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status": "ok"}`))
}

// handleIndex serves the main page
func handleIndex(w http.ResponseWriter, r *http.Request) {
	content, err := os.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "Failed to load page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	w.Write(content)
}

// handleAPIDevices returns the device list as JSON
func handleAPIDevices(w http.ResponseWriter, r *http.Request) {
	deviceCacheMutex.RLock()
	defer deviceCacheMutex.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"devices":      deviceCache.Devices,
		"last_scan":    lastScanTime,
		"device_count": len(deviceCache.Devices),
	})
}

// handleAPIScan triggers a new network scan
func handleAPIScan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Run scan in background
	go func() {
		config, err := LoadConfig("config.yaml")
		if err != nil {
			log.Printf("Failed to load config: %v", err)
			return
		}

		scanStatusMutex.Lock()
		scanStatus = ScanStatus{
			Running:   true,
			Progress:  0,
			Total:     0,
			Message:   "Starting network scan...",
			StartedAt: time.Now(),
		}
		scanStatusMutex.Unlock()

		// Scan subnet
		results, err := ScanSubnet(
			config.Network.Subnet,
			config.GetPingTimeout(),
			config.Network.PingWorkers,
		)
		if err != nil {
			scanStatusMutex.Lock()
			scanStatus.Running = false
			scanStatus.Message = "Scan failed"
			scanStatus.FinishedAt = time.Now()
			scanStatusMutex.Unlock()
			log.Printf("Scan failed: %v", err)
			return
		}

		// Filter alive hosts
		var aliveHosts []ScanResult
		for _, result := range results {
			if result.IsUp {
				aliveHosts = append(aliveHosts, result)
			}
		}

		scanStatusMutex.Lock()
		scanStatus.Message = fmt.Sprintf("Found %d live hosts", len(aliveHosts))
		scanStatus.Total = len(aliveHosts)
		scanStatus.Progress = 0
		scanStatusMutex.Unlock()

		// Load existing devices
		devicesDB, err := LoadDevices(config.Output.DevicesFile)
		if err != nil {
			scanStatusMutex.Lock()
			scanStatus.Running = false
			scanStatus.Message = "Failed to load devices"
			scanStatus.FinishedAt = time.Now()
			scanStatusMutex.Unlock()
			log.Printf("Failed to load devices: %v", err)
			return
		}

		// SNMP discovery
		// Build list of communities: always try 'public', 'private', plus user-defined
		var allCommunities []string
		found := map[string]bool{}
		for _, c := range config.SNMP.Communities {
			if !found[c] {
				allCommunities = append(allCommunities, c)
				found[c] = true
			}
		}
		for _, c := range []string{"public", "private"} {
			if !found[c] {
				allCommunities = append(allCommunities, c)
				found[c] = true
			}
		}

		for i, host := range aliveHosts {
			device, err := DiscoverDevice(
				host.IP,
				allCommunities,
				config.OIDs,
				config.GetSNMPTimeout(),
				host.Latency,
			)
			if err == nil {
				devicesDB.MergeDevice(*device)
			}
			scanStatusMutex.Lock()
			scanStatus.Progress = i + 1
			scanStatus.Message = fmt.Sprintf("Scanning %d/%d devices...", scanStatus.Progress, scanStatus.Total)
			scanStatusMutex.Unlock()
		}

		// Save devices
		err = SaveDevices(config.Output.DevicesFile, devicesDB)
		if err != nil {
			scanStatusMutex.Lock()
			scanStatus.Running = false
			scanStatus.Message = "Failed to save devices"
			scanStatus.FinishedAt = time.Now()
			scanStatusMutex.Unlock()
			log.Printf("Failed to save devices: %v", err)
			return
		}

		// Update cache
		deviceCacheMutex.Lock()
		deviceCache = devicesDB
		lastScanTime = time.Now()
		deviceCacheMutex.Unlock()

		scanStatusMutex.Lock()
		scanStatus.Running = false
		scanStatus.Message = "Scan complete"
		scanStatus.FinishedAt = time.Now()
		scanStatusMutex.Unlock()
		log.Println("Scan complete")
	}()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "Scan started",
	})
}

// handleAPIScanStatus returns scan progress
func handleAPIScanStatus(w http.ResponseWriter, r *http.Request) {
	scanStatusMutex.RLock()
	defer scanStatusMutex.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(scanStatus)
}
