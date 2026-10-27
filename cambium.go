package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gosnmp/gosnmp"
)

// CAMBIUM-MIB (cnPilot / XV-series APs), under cambiumStateGroup. The AP has
// no HOST-RESOURCES-MIB, so CPU and memory come from here too.
const (
	oidCnAPCPU     = "1.3.6.1.4.1.17713.22.1.1.1.6" // %
	oidCnAPMemFree = "1.3.6.1.4.1.17713.22.1.1.1.7" // MIB says bytes; the XV2-2 reports % free

	oidCnRadioBand    = "1.3.6.1.4.1.17713.22.1.2.1.3" // "2.4GHz", "5GHz"
	oidCnRadioClients = "1.3.6.1.4.1.17713.22.1.2.1.5"
	oidCnRadioChannel = "1.3.6.1.4.1.17713.22.1.2.1.6"
	oidCnRadioWidth   = "1.3.6.1.4.1.17713.22.1.2.1.7" // MHz
	oidCnRadioTxPower = "1.3.6.1.4.1.17713.22.1.2.1.8" // dBm
	oidCnRadioTxBytes = "1.3.6.1.4.1.17713.22.1.2.1.11"
	oidCnRadioRxBytes = "1.3.6.1.4.1.17713.22.1.2.1.12"
	oidCnRadioNoise   = "1.3.6.1.4.1.17713.22.1.2.1.16" // dBm, as text
	oidCnRadioAirtime = "1.3.6.1.4.1.17713.22.1.2.1.18" // "total/tx/rx/busy" %

	oidCnClientMAC     = "1.3.6.1.4.1.17713.22.1.3.1.2"
	oidCnClientIP      = "1.3.6.1.4.1.17713.22.1.3.1.3"
	oidCnClientName    = "1.3.6.1.4.1.17713.22.1.3.1.4"
	oidCnClientSSID    = "1.3.6.1.4.1.17713.22.1.3.1.5"
	oidCnClientVendor  = "1.3.6.1.4.1.17713.22.1.3.1.6"
	oidCnClientMode    = "1.3.6.1.4.1.17713.22.1.3.1.7"  // "bgn", "ac", "axa"...
	oidCnClientRadio   = "1.3.6.1.4.1.17713.22.1.3.1.8"  // 1-based; radio table rows are 0-based
	oidCnClientSNR     = "1.3.6.1.4.1.17713.22.1.3.1.11" // dB
	oidCnClientRate    = "1.3.6.1.4.1.17713.22.1.3.1.12" // Mbps, as text
	oidCnClientTxBytes = "1.3.6.1.4.1.17713.22.1.3.1.14"
	oidCnClientRxBytes = "1.3.6.1.4.1.17713.22.1.3.1.16"

	oidCnWlanSSID    = "1.3.6.1.4.1.17713.22.1.4.1.2"
	oidCnWlanClients = "1.3.6.1.4.1.17713.22.1.4.1.7"
)

// WifiStatus is the AP's live state: shown in the device popup, not stored.
type WifiStatus struct {
	Radios  []WifiRadio  `json:"radios"`
	Clients []WifiClient `json:"clients"`
}

type WifiRadio struct {
	Index      string `json:"index"` // radio table row
	Band       string `json:"band"`
	Channel    string `json:"channel"`
	WidthMHz   string `json:"width_mhz"`
	TxPowerDBm int    `json:"tx_power_dbm"`
	NoiseDBm   int    `json:"noise_dbm"`
	AirtimePct int    `json:"airtime_pct"`
	Clients    int    `json:"clients"`
	// Byte counters (32-bit); whoever reads them works out the rate
	TxBytes uint64 `json:"tx_bytes"`
	RxBytes uint64 `json:"rx_bytes"`

	noiseOK, airtimeOK bool
}

type WifiClient struct {
	Name     string  `json:"name"`
	IP       string  `json:"ip"`
	MAC      string  `json:"mac"`
	SSID     string  `json:"ssid"`
	Band     string  `json:"band"`
	Mode     string  `json:"mode"`
	Vendor   string  `json:"vendor"`
	SNR      int     `json:"snr_db"`
	RateMbps float64 `json:"rate_mbps"`
	TxBytes  uint64  `json:"tx_bytes"`
	RxBytes  uint64  `json:"rx_bytes"`
}

func isCambium(d Device) bool {
	return strings.Contains(strings.ToLower(d.Description), "cambium")
}

// collectCambium fills in CPU, the AP's sensors (clients, airtime, noise,
// per-client SNR and usage, radio and uplink traffic) and its live Wi-Fi state.
func collectCambium(client *gosnmp.GoSNMP, ip string, m *LinuxMetrics) error {
	cpu, err := walkIndexed(client, oidCnAPCPU)
	if err != nil {
		return fmt.Errorf("cambium: %w", err)
	}
	memFree, _ := walkIndexed(client, oidCnAPMemFree)
	now := time.Now()

	// The AP table has a single row; it reports one CPU figure, not per core
	if v, ok := snmpInt(firstValue(cpu)); ok {
		m.CPU = CPUMetrics{AvgLoadPct: float64(v), NumCPUs: 1}
	}
	if v, ok := snmpInt(firstValue(memFree)); ok {
		m.Sensors = append(m.Sensors, SensorMetrics{Name: "Memory", Unit: "mem_free", Value: float64(v)})
	}

	wifi := readCambiumWifi(client)
	for _, r := range wifi.Radios {
		m.Sensors = append(m.Sensors, SensorMetrics{Name: r.Band, Unit: "clients", Value: float64(r.Clients)})
		if r.noiseOK {
			m.Sensors = append(m.Sensors, SensorMetrics{Name: r.Band, Unit: "dBm", Value: float64(r.NoiseDBm)})
		}
		if r.airtimeOK {
			m.Sensors = append(m.Sensors, SensorMetrics{Name: r.Band, Unit: "airtime", Value: float64(r.AirtimePct)})
		}
		// Rx is what the radio received from clients: traffic into the AP
		key := ip + "/radio/" + r.Index
		in, inOK := counterRate(key+"/rx", r.RxBytes, now)
		out, outOK := counterRate(key+"/tx", r.TxBytes, now)
		if inOK && outOK {
			m.Sensors = append(m.Sensors, trafficSensors(r.Band+" radio", in, out)...)
		}
	}

	// Chart series are named by hostname, so the same device keeps one line
	// as its IP changes; a repeated hostname gets the end of its MAC added
	labels := clientLabels(wifi.Clients)
	for i, c := range wifi.Clients {
		label := labels[i]
		m.Sensors = append(m.Sensors, SensorMetrics{Name: label, Unit: "snr", Value: float64(c.SNR)})
		// Counters are keyed by MAC: the table's row numbers shift as clients come and go
		key := ip + "/client/" + c.MAC
		tx, txOK := counterRate(key+"/tx", c.TxBytes, now)
		rx, rxOK := counterRate(key+"/rx", c.RxBytes, now)
		if txOK && rxOK {
			m.Sensors = append(m.Sensors, SensorMetrics{Name: label, Unit: "client_mbps", Value: tx + rx})
		}
	}

	ssids, _ := walkIndexed(client, oidCnWlanSSID)
	ssidClients, _ := walkIndexed(client, oidCnWlanClients)
	for idx, ssidVal := range ssids {
		ssid := sensorName(snmpString(ssidVal))
		if n, ok := snmpInt(ssidClients[idx]); ok && ssid != "" {
			m.Sensors = append(m.Sensors, SensorMetrics{Name: ssid, Unit: "ssid_clients", Value: float64(n)})
		}
	}

	// Wired uplink; the radios are covered above with their band names
	uplink, err := collectTraffic(client, ip, func(name string) bool { return name == "eth0" })
	if err == nil {
		m.Sensors = append(m.Sensors, uplink...)
	}
	m.Wifi = wifi
	return nil
}

// readCambiumWifi reads the radio and client tables, byte counters included.
// The poller and the live view share it; it has no side effects, so the live
// view reading every few seconds doesn't disturb the poller's traffic rates.
func readCambiumWifi(client *gosnmp.GoSNMP) *WifiStatus {
	wifi := &WifiStatus{Radios: []WifiRadio{}, Clients: []WifiClient{}}

	bandVals, _ := walkIndexed(client, oidCnRadioBand)
	radioClients, _ := walkIndexed(client, oidCnRadioClients)
	channels, _ := walkIndexed(client, oidCnRadioChannel)
	widths, _ := walkIndexed(client, oidCnRadioWidth)
	powers, _ := walkIndexed(client, oidCnRadioTxPower)
	radioTx, _ := walkIndexed(client, oidCnRadioTxBytes)
	radioRx, _ := walkIndexed(client, oidCnRadioRxBytes)
	noises, _ := walkIndexed(client, oidCnRadioNoise)
	airtimes, _ := walkIndexed(client, oidCnRadioAirtime)

	bands := map[string]string{}
	for _, idx := range sortedKeys(bandVals) {
		band := sensorName(snmpString(bandVals[idx]))
		if band == "" {
			band = "Radio " + idx
		}
		bands[idx] = band
		r := WifiRadio{
			Index:    idx,
			Band:     band,
			Channel:  snmpString(channels[idx]),
			WidthMHz: snmpString(widths[idx]),
			TxBytes:  counterValue(radioTx[idx]),
			RxBytes:  counterValue(radioRx[idx]),
		}
		r.Clients, _ = snmpInt(radioClients[idx])
		r.TxPowerDBm, _ = snmpInt(powers[idx])
		r.NoiseDBm, r.noiseOK = atoiText(noises[idx])
		// Airtime is "total/tx/rx/busy"; the total is the radio's utilisation
		r.AirtimePct, r.airtimeOK = atoiText(strings.Split(snmpString(airtimes[idx]), "/")[0])
		wifi.Radios = append(wifi.Radios, r)
	}

	macs, err := walkIndexed(client, oidCnClientMAC)
	if err != nil {
		return wifi
	}
	ips, _ := walkIndexed(client, oidCnClientIP)
	names, _ := walkIndexed(client, oidCnClientName)
	ssids, _ := walkIndexed(client, oidCnClientSSID)
	vendors, _ := walkIndexed(client, oidCnClientVendor)
	modes, _ := walkIndexed(client, oidCnClientMode)
	radios, _ := walkIndexed(client, oidCnClientRadio)
	snrs, _ := walkIndexed(client, oidCnClientSNR)
	rates, _ := walkIndexed(client, oidCnClientRate)
	txBytes, _ := walkIndexed(client, oidCnClientTxBytes)
	rxBytes, _ := walkIndexed(client, oidCnClientRxBytes)

	for _, idx := range sortedKeys(macs) {
		c := WifiClient{
			MAC:     strings.ToUpper(snmpString(macs[idx])),
			IP:      snmpString(ips[idx]),
			Name:    strings.TrimSpace(snmpString(names[idx])),
			SSID:    snmpString(ssids[idx]),
			Vendor:  snmpString(vendors[idx]),
			Mode:    snmpString(modes[idx]),
			TxBytes: counterValue(txBytes[idx]),
			RxBytes: counterValue(rxBytes[idx]),
		}
		if c.MAC == "" {
			continue
		}
		if r, ok := snmpInt(radios[idx]); ok {
			c.Band = bands[strconv.Itoa(r-1)]
		}
		c.SNR, _ = snmpInt(snrs[idx])
		c.RateMbps, _ = strconv.ParseFloat(strings.TrimSpace(snmpString(rates[idx])), 64)
		wifi.Clients = append(wifi.Clients, c)
	}
	return wifi
}

func counterValue(v interface{}) uint64 {
	if v == nil {
		return 0
	}
	return gosnmp.ToBigInt(v).Uint64()
}

func clientLabels(clients []WifiClient) []string {
	base := make([]string, len(clients))
	count := map[string]int{}
	for i, c := range clients {
		switch {
		case c.Name != "":
			base[i] = c.Name
		case c.IP != "":
			base[i] = c.IP
		default:
			base[i] = c.MAC
		}
		count[base[i]]++
	}
	labels := make([]string, len(clients))
	for i, c := range clients {
		label := base[i]
		if count[label] > 1 && len(c.MAC) >= 5 {
			label = sensorName(label[:min(len(label), sensorNameMax-6)]) + " " + c.MAC[len(c.MAC)-5:]
		}
		labels[i] = sensorName(label)
	}
	return labels
}

// atoiText parses an integer the AP sends as text, such as "-107"
func atoiText(v interface{}) (int, bool) {
	s, ok := v.(string)
	if !ok {
		s = snmpString(v)
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	return n, err == nil
}

func firstValue(m map[string]interface{}) interface{} {
	keys := sortedKeys(m)
	if len(keys) == 0 {
		return nil
	}
	return m[keys[0]]
}

// sortedKeys orders table indexes numerically, part by part, so "1.2" comes
// before "1.10"
func sortedKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := strings.Split(keys[i], "."), strings.Split(keys[j], ".")
		for n := 0; n < len(a) && n < len(b); n++ {
			x, errX := strconv.Atoi(a[n])
			y, errY := strconv.Atoi(b[n])
			if errX != nil || errY != nil {
				if a[n] != b[n] {
					return a[n] < b[n]
				}
				continue
			}
			if x != y {
				return x < y
			}
		}
		return len(a) < len(b)
	})
	return keys
}
