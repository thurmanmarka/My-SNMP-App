package main

import (
	"encoding/json"
	"errors"
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

// loadDevices reads the device inventory from the database, or from
// devices.yaml when the database is disabled.
func loadDevices(config *Config) (*DeviceDatabase, error) {
	if dbStore != nil {
		return dbStore.LoadDevices()
	}
	return LoadDevices(config.Output.DevicesFile)
}

// saveDevices writes the device inventory to the database or devices.yaml.
func saveDevices(config *Config, db *DeviceDatabase) error {
	if dbStore != nil {
		return dbStore.SaveDevices(db.Devices)
	}
	return SaveDevices(config.Output.DevicesFile, db)
}

// StartWebServer starts the HTTP server for the web GUI
func StartWebServer(config *Config) error {
	// Initialize device cache
	db, err := loadDevices(config)
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
	http.HandleFunc("/api/metrics/history", handleAPIMetricsHistory)
	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	http.HandleFunc("/health", handleHealth)
	http.HandleFunc("/snmp/health", handleHealth)

	addr := fmt.Sprintf("%s:%d", config.Server.Bind, config.Server.Port)
	log.Printf("Starting web server on http://%s", addr)
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

// handleAPIDevices returns the device list as JSON. Community strings are only
// included for admins (the hub sets X-Hub-Role).
func handleAPIDevices(w http.ResponseWriter, r *http.Request) {
	deviceCacheMutex.RLock()
	defer deviceCacheMutex.RUnlock()

	devices := deviceCache.Devices
	if r.Header.Get("X-Hub-Role") != "admin" {
		devices = make([]Device, len(deviceCache.Devices))
		copy(devices, deviceCache.Devices)
		for i := range devices {
			devices[i].Community = ""
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"devices":      devices,
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
	startedAt := scanStatus.StartedAt
	scanStatusMutex.Unlock()

	// Record the scan in the database; hostsAlive and devicesFound are filled
	// in as the scan goes so every exit path records them.
	var scanID int64
	var hostsAlive, devicesFound int
	if dbStore != nil {
		id, err := dbStore.StartScan(startedAt)
		if err != nil {
			log.Printf("Failed to record scan start: %v", err)
		}
		scanID = id
	}
	recordScan := func(status, message string) {
		if dbStore == nil || scanID == 0 {
			return
		}
		if err := dbStore.FinishScan(scanID, hostsAlive, devicesFound, status, message); err != nil {
			log.Printf("Failed to record scan result: %v", err)
		}
	}

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
		recordScan("failed", err.Error())
		return
	}

	var aliveHosts []ScanResult
	for _, result := range results {
		if result.IsUp {
			aliveHosts = append(aliveHosts, result)
		}
	}

	hostsAlive = len(aliveHosts)

	scanStatusMutex.Lock()
	scanStatus.Message = fmt.Sprintf("Found %d live hosts", len(aliveHosts))
	scanStatus.Total = len(aliveHosts)
	scanStatus.Progress = 0
	scanStatusMutex.Unlock()

	devicesDB, err := loadDevices(config)
	if err != nil {
		scanStatusMutex.Lock()
		scanStatus.Running = false
		scanStatus.Message = "Failed to load devices"
		scanStatus.FinishedAt = time.Now()
		scanStatusMutex.Unlock()
		log.Printf("Failed to load devices: %v", err)
		recordScan("failed", "load devices: "+err.Error())
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
			devicesFound++
		}
		scanStatusMutex.Lock()
		scanStatus.Progress = i + 1
		scanStatus.Message = fmt.Sprintf("Scanning %d/%d devices...", scanStatus.Progress, scanStatus.Total)
		scanStatusMutex.Unlock()
	}

	if err := saveDevices(config, devicesDB); err != nil {
		scanStatusMutex.Lock()
		scanStatus.Running = false
		scanStatus.Message = "Failed to save devices"
		scanStatus.FinishedAt = time.Now()
		scanStatusMutex.Unlock()
		log.Printf("Failed to save devices: %v", err)
		recordScan("failed", "save devices: "+err.Error())
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
	recordScan("complete", "")
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

// historyRanges are the chart ranges the UI offers. Ranges up to a day use raw
// 60-second samples; longer ones use hourly rollups.
var historyRanges = map[string]struct {
	d      time.Duration
	hourly bool
}{
	"6h":  {6 * time.Hour, false},
	"24h": {24 * time.Hour, false},
	"7d":  {7 * 24 * time.Hour, true},
	"30d": {30 * 24 * time.Hour, true},
	"90d": {90 * 24 * time.Hour, true},
	"1y":  {365 * 24 * time.Hour, true},
}

// handleAPIMetricsHistory returns CPU, disk and temperature history for one
// device: /api/metrics/history?ip=x.x.x.x&range=24h
func handleAPIMetricsHistory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if dbStore == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"error": "history needs database.enabled in config.yaml"})
		return
	}
	ip := r.URL.Query().Get("ip")
	rng, ok := historyRanges[r.URL.Query().Get("range")]
	if ip == "" || !ok {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "need ip and range (6h, 24h, 7d, 30d, 90d, 1y)"})
		return
	}

	h, err := dbStore.History(ip, time.Now().Add(-rng.d), rng.hourly)
	if errors.Is(err, ErrUnknownDevice) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "unknown device"})
		return
	}
	if err != nil {
		log.Printf("History query for %s failed: %v", ip, err)
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "history query failed"})
		return
	}
	json.NewEncoder(w).Encode(h)
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
