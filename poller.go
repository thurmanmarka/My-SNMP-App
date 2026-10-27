package main

import (
	"encoding/csv"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gosnmp/gosnmp"
)

// HOST-RESOURCES-MIB and LM-SENSORS-MIB OIDs (standard Net-SNMP on Linux)
const (
	oidHRProcessorLoad     = "1.3.6.1.2.1.25.3.3.1.2"
	oidHRStorageType       = "1.3.6.1.2.1.25.2.3.1.2"
	oidHRStorageDescr      = "1.3.6.1.2.1.25.2.3.1.3"
	oidHRStorageAllocUnits = "1.3.6.1.2.1.25.2.3.1.4"
	oidHRStorageSize       = "1.3.6.1.2.1.25.2.3.1.5"
	oidHRStorageUsed       = "1.3.6.1.2.1.25.2.3.1.6"
	oidLmTempSensorsDevice = "1.3.6.1.4.1.2021.13.16.2.1.2"
	oidLmTempSensorsValue  = "1.3.6.1.4.1.2021.13.16.2.1.3"

	// OID suffix that identifies a fixed disk in hrStorageType
	hrStorageFixedDisk = "25.2.1.4"
)

// LinuxMetrics is one poll of a device: CPU and disks for Linux hosts,
// temperatures and other sensors for anything that reports them.
type LinuxMetrics struct {
	IP          string          `json:"ip"`
	CollectedAt time.Time       `json:"collected_at"`
	CPU         CPUMetrics      `json:"cpu"`
	Disks       []DiskMetrics   `json:"disks"`
	Temps       []TempMetrics   `json:"temps,omitempty"`
	Sensors     []SensorMetrics `json:"sensors,omitempty"`
	Error       string          `json:"error,omitempty"`
}

type CPUMetrics struct {
	AvgLoadPct float64 `json:"avg_load_pct"`
	NumCPUs    int     `json:"num_cpus"`
}

type DiskMetrics struct {
	Path       string `json:"path"`
	TotalBytes int64  `json:"total_bytes"`
	UsedBytes  int64  `json:"used_bytes"`
	AvailBytes int64  `json:"avail_bytes"`
	UsePct     int    `json:"use_pct"`
}

type TempMetrics struct {
	Name  string  `json:"name"`
	TempC float64 `json:"temp_c"`
}

// SensorMetrics is a named gauge reading such as a supply voltage ("V") or a
// port's current draw ("A").
type SensorMetrics struct {
	Name  string  `json:"name"`
	Unit  string  `json:"unit"`
	Value float64 `json:"value"`
}

type LinuxPoller struct {
	config  *Config
	metrics map[string]*LinuxMetrics
	mu      sync.RWMutex
}

func NewLinuxPoller(config *Config) *LinuxPoller {
	return &LinuxPoller{
		config:  config,
		metrics: make(map[string]*LinuxMetrics),
	}
}

func (p *LinuxPoller) Start() {
	if !p.config.Poller.Enabled {
		return
	}
	interval := time.Duration(p.config.Poller.IntervalSeconds) * time.Second
	log.Printf("Linux poller enabled: polling every %d seconds", p.config.Poller.IntervalSeconds)

	p.pollAll()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		p.pollAll()
	}
}

func (p *LinuxPoller) GetAll() map[string]*LinuxMetrics {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make(map[string]*LinuxMetrics, len(p.metrics))
	for k, v := range p.metrics {
		out[k] = v
	}
	return out
}

func (p *LinuxPoller) GetByIP(ip string) (*LinuxMetrics, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	m, ok := p.metrics[ip]
	return m, ok
}

func (p *LinuxPoller) pollAll() {
	deviceCacheMutex.RLock()
	if deviceCache == nil {
		deviceCacheMutex.RUnlock()
		return
	}
	devices := make([]Device, len(deviceCache.Devices))
	copy(devices, deviceCache.Devices)
	deviceCacheMutex.RUnlock()

	var (
		resultsMu sync.Mutex
		results   []*LinuxMetrics
		wg        sync.WaitGroup
	)
	for _, d := range devices {
		if !isLinuxDevice(d) && !isSiteMonitor(d) {
			continue
		}
		wg.Add(1)
		go func(dev Device) {
			defer wg.Done()
			m := pollDevice(dev, p.config)
			p.mu.Lock()
			p.metrics[dev.IP] = m
			p.mu.Unlock()
			resultsMu.Lock()
			results = append(results, m)
			resultsMu.Unlock()
		}(d)
	}
	wg.Wait()

	if len(results) == 0 {
		return
	}
	if dbStore != nil {
		if err := dbStore.SaveMetrics(results); err != nil {
			log.Printf("Poller: database write error: %v", err)
		}
		return
	}
	if err := writeMetricsCSV(results, p.config.Poller.CSVDir); err != nil {
		log.Printf("Poller: CSV write error: %v", err)
	}
}

func isLinuxDevice(d Device) bool {
	return strings.Contains(strings.ToLower(d.Description), "linux")
}

func pollDevice(device Device, config *Config) *LinuxMetrics {
	m := &LinuxMetrics{
		IP:          device.IP,
		CollectedAt: time.Now(),
	}

	client := &gosnmp.GoSNMP{
		Target:    device.IP,
		Port:      uint16(config.SNMP.Port),
		Community: device.Community,
		Version:   gosnmp.Version2c,
		Timeout:   config.GetSNMPTimeout(),
		Retries:   config.SNMP.Retries,
	}
	if err := client.Connect(); err != nil {
		m.Error = fmt.Sprintf("connect: %v", err)
		return m
	}
	defer client.Conn.Close()

	if isSiteMonitor(device) {
		temps, sensors, err := collectSiteMonitor(client)
		if err != nil {
			m.Error = err.Error()
		}
		m.Temps, m.Sensors = temps, sensors
		return m
	}

	if cpu, err := collectCPU(client); err == nil {
		m.CPU = cpu
	} else {
		log.Printf("Poller %s: CPU error: %v", device.IP, err)
	}

	if disks, err := collectDisks(client); err == nil {
		m.Disks = disks
	} else {
		log.Printf("Poller %s: disk error: %v", device.IP, err)
	}

	// Temperatures are optional — not all Linux devices expose lm-sensors via SNMP
	if temps, err := collectTemps(client); err == nil {
		m.Temps = temps
	}

	return m
}

func collectCPU(client *gosnmp.GoSNMP) (CPUMetrics, error) {
	results, err := client.WalkAll(oidHRProcessorLoad)
	if err != nil {
		return CPUMetrics{}, err
	}
	if len(results) == 0 {
		return CPUMetrics{}, fmt.Errorf("no hrProcessorLoad entries")
	}
	var total int
	for _, v := range results {
		if n, ok := snmpInt(v.Value); ok {
			total += n
		}
	}
	return CPUMetrics{
		AvgLoadPct: float64(total) / float64(len(results)),
		NumCPUs:    len(results),
	}, nil
}

func collectDisks(client *gosnmp.GoSNMP) ([]DiskMetrics, error) {
	types, err := walkIndexed(client, oidHRStorageType)
	if err != nil {
		return nil, err
	}
	descrs, _ := walkIndexed(client, oidHRStorageDescr)
	allUnits, _ := walkIndexed(client, oidHRStorageAllocUnits)
	sizes, _ := walkIndexed(client, oidHRStorageSize)
	usedMap, _ := walkIndexed(client, oidHRStorageUsed)

	var disks []DiskMetrics
	for idx, typeVal := range types {
		// hrStorageType is an OID; check for hrStorageFixedDisk suffix
		typeStr := strings.TrimPrefix(fmt.Sprintf("%v", typeVal), ".")
		if !strings.HasSuffix(typeStr, hrStorageFixedDisk) {
			continue
		}

		unit, _ := snmpInt64(allUnits[idx])
		size, _ := snmpInt64(sizes[idx])
		used, _ := snmpInt64(usedMap[idx])

		totalBytes := unit * size
		usedBytes := unit * used
		availBytes := totalBytes - usedBytes

		var usePct int
		if totalBytes > 0 {
			usePct = int(100 * usedBytes / totalBytes)
		}

		path := snmpString(descrs[idx])
		disks = append(disks, DiskMetrics{
			Path:       path,
			TotalBytes: totalBytes,
			UsedBytes:  usedBytes,
			AvailBytes: availBytes,
			UsePct:     usePct,
		})
	}
	return disks, nil
}

func collectTemps(client *gosnmp.GoSNMP) ([]TempMetrics, error) {
	names, err := walkIndexed(client, oidLmTempSensorsDevice)
	if err != nil {
		return nil, err
	}
	values, _ := walkIndexed(client, oidLmTempSensorsValue)

	var temps []TempMetrics
	for idx, nameVal := range names {
		milliC, _ := snmpInt64(values[idx])
		temps = append(temps, TempMetrics{
			Name:  snmpString(nameVal),
			TempC: float64(milliC) / 1000.0,
		})
	}
	return temps, nil
}

// walkIndexed walks an OID column and returns map[row-index -> value].
func walkIndexed(client *gosnmp.GoSNMP, baseOID string) (map[string]interface{}, error) {
	results, err := client.WalkAll(baseOID)
	if err != nil {
		return nil, err
	}
	prefix := baseOID + "."
	m := make(map[string]interface{})
	for _, v := range results {
		name := strings.TrimPrefix(v.Name, ".")
		if strings.HasPrefix(name, prefix) {
			m[name[len(prefix):]] = v.Value
		}
	}
	return m, nil
}

func snmpInt(v interface{}) (int, bool) {
	switch val := v.(type) {
	case int:
		return val, true
	case uint:
		return int(val), true
	case int64:
		return int(val), true
	case uint64:
		return int(val), true
	case uint32:
		return int(val), true
	}
	return 0, false
}

func snmpInt64(v interface{}) (int64, bool) {
	switch val := v.(type) {
	case int:
		return int64(val), true
	case uint:
		return int64(val), true
	case int64:
		return val, true
	case uint64:
		return int64(val), true
	case uint32:
		return int64(val), true
	}
	return 0, false
}

func snmpString(v interface{}) string {
	if v == nil {
		return ""
	}
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return fmt.Sprintf("%v", v)
}

// writeMetricsCSV appends this poll's results to the CSV files in dir.
func writeMetricsCSV(results []*LinuxMetrics, dir string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create csv dir: %w", err)
	}

	if err := appendCSV(
		filepath.Join(dir, "metrics_cpu.csv"),
		[]string{"timestamp", "ip", "avg_load_pct", "num_cpus"},
		func(w *csv.Writer) {
			for _, m := range results {
				if m.Error != "" {
					continue
				}
				w.Write([]string{
					m.CollectedAt.UTC().Format(time.RFC3339),
					m.IP,
					strconv.FormatFloat(m.CPU.AvgLoadPct, 'f', 2, 64),
					strconv.Itoa(m.CPU.NumCPUs),
				})
			}
		},
	); err != nil {
		return err
	}

	if err := appendCSV(
		filepath.Join(dir, "metrics_disk.csv"),
		[]string{"timestamp", "ip", "path", "total_bytes", "used_bytes", "avail_bytes", "use_pct"},
		func(w *csv.Writer) {
			for _, m := range results {
				if m.Error != "" {
					continue
				}
				for _, d := range m.Disks {
					w.Write([]string{
						m.CollectedAt.UTC().Format(time.RFC3339),
						m.IP,
						d.Path,
						strconv.FormatInt(d.TotalBytes, 10),
						strconv.FormatInt(d.UsedBytes, 10),
						strconv.FormatInt(d.AvailBytes, 10),
						strconv.Itoa(d.UsePct),
					})
				}
			}
		},
	); err != nil {
		return err
	}

	if err := appendCSV(
		filepath.Join(dir, "metrics_sensor.csv"),
		[]string{"timestamp", "ip", "sensor_name", "unit", "value"},
		func(w *csv.Writer) {
			for _, m := range results {
				if m.Error != "" {
					continue
				}
				for _, s := range m.Sensors {
					w.Write([]string{
						m.CollectedAt.UTC().Format(time.RFC3339),
						m.IP,
						s.Name,
						s.Unit,
						strconv.FormatFloat(s.Value, 'f', 4, 64),
					})
				}
			}
		},
	); err != nil {
		return err
	}

	return appendCSV(
		filepath.Join(dir, "metrics_temp.csv"),
		[]string{"timestamp", "ip", "sensor_name", "temp_c"},
		func(w *csv.Writer) {
			for _, m := range results {
				if m.Error != "" {
					continue
				}
				for _, t := range m.Temps {
					w.Write([]string{
						m.CollectedAt.UTC().Format(time.RFC3339),
						m.IP,
						t.Name,
						strconv.FormatFloat(t.TempC, 'f', 2, 64),
					})
				}
			}
		},
	)
}

// appendCSV opens path for appending, writes the header row if the file is new,
// then calls write to add data rows, and flushes.
func appendCSV(path string, header []string, write func(*csv.Writer)) error {
	isNew := false
	if _, err := os.Stat(path); os.IsNotExist(err) {
		isNew = true
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	if isNew {
		w.Write(header)
	}
	write(w)
	w.Flush()
	return w.Error()
}
