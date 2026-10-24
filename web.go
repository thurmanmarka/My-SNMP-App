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

	nextScanMutex sync.RWMutex
	nextScanTime  time.Time

	linuxPoller *LinuxPoller
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

	// Start automatic scan scheduler and Linux poller
	go startScheduler(config)
	linuxPoller = NewLinuxPoller(config)
	go linuxPoller.Start()

	// Routes
	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/api/devices", handleAPIDevices)
	http.HandleFunc("/api/scan", handleAPIScan)
	http.HandleFunc("/api/scan-status", handleAPIScanStatus)
	http.HandleFunc("/api/metrics", handleAPIMetrics)
	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	http.HandleFunc("/health", handleHealth)
	http.HandleFunc("/snmp/health", handleHealth)

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

	config, err := LoadConfig("config.yaml")
	if err != nil {
		http.Error(w, "Failed to load config", http.StatusInternalServerError)
		return
	}

	go runScan(config)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "Scan started",
	})
}

// runScan performs a full network scan and updates the device cache.
func runScan(config *Config) {
	scanStatusMutex.Lock()
	if scanStatus.Running {
		scanStatusMutex.Unlock()
		return
	}
	scanStatus = ScanStatus{
		Running:   true,
		Progress:  0,
		Total:     0,
		Message:   "Starting network scan...",
		StartedAt: time.Now(),
	}
	scanStatusMutex.Unlock()

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

	var allCommunities []string
	seen := map[string]bool{}
	for _, c := range config.SNMP.Communities {
		if !seen[c] {
			allCommunities = append(allCommunities, c)
			seen[c] = true
		}
	}
	for _, c := range []string{"public", "private"} {
		if !seen[c] {
			allCommunities = append(allCommunities, c)
			seen[c] = true
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

	if err := SaveDevices(config.Output.DevicesFile, devicesDB); err != nil {
		scanStatusMutex.Lock()
		scanStatus.Running = false
		scanStatus.Message = "Failed to save devices"
		scanStatus.FinishedAt = time.Now()
		scanStatusMutex.Unlock()
		log.Printf("Failed to save devices: %v", err)
		return
	}

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
}

// startScheduler runs a network scan once per day at midnight local time.
func startScheduler(config *Config) {
	if !config.Scheduler.Enabled {
		return
	}
	log.Println("Scheduler enabled: network scan runs daily at midnight")

	for {
		now := time.Now()
		next := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())

		nextScanMutex.Lock()
		nextScanTime = next
		nextScanMutex.Unlock()

		log.Printf("Scheduler: next scan at %s", next.Format("2006-01-02 15:04:05"))
		time.Sleep(time.Until(next))

		log.Println("Scheduler: starting midnight scan")
		cfg, err := LoadConfig("config.yaml")
		if err != nil {
			log.Printf("Scheduler: failed to reload config: %v", err)
			cfg = config
		}
		runScan(cfg)
	}
}

// handleAPIMetrics returns latest Linux metrics for all polled devices.
// Optional query param: ?ip=x.x.x.x to filter to a single device.
func handleAPIMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if linuxPoller == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"devices": []interface{}{}})
		return
	}
	if ip := r.URL.Query().Get("ip"); ip != "" {
		m, ok := linuxPoller.GetByIP(ip)
		if !ok {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(m)
		return
	}
	all := linuxPoller.GetAll()
	devices := make([]*LinuxMetrics, 0, len(all))
	for _, m := range all {
		devices = append(devices, m)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"devices": devices})
}

// handleAPIScanStatus returns scan progress and next scheduled scan time
func handleAPIScanStatus(w http.ResponseWriter, r *http.Request) {
	scanStatusMutex.RLock()
	status := scanStatus
	scanStatusMutex.RUnlock()

	nextScanMutex.RLock()
	next := nextScanTime
	nextScanMutex.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"running":     status.Running,
		"progress":    status.Progress,
		"total":       status.Total,
		"message":     status.Message,
		"started_at":  status.StartedAt,
		"finished_at": status.FinishedAt,
		"next_scan":   next,
	})
}
