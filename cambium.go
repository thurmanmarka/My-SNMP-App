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
	Band       string `json:"band"`
	Channel    string `json:"channel"`
	WidthMHz   string `json:"width_mhz"`
	TxPowerDBm int    `json:"tx_power_dbm"`
	NoiseDBm   int    `json:"noise_dbm"`
	AirtimePct int    `json:"airtime_pct"`
	Clients    int    `json:"clients"`
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

	wifi := &WifiStatus{Radios: []WifiRadio{}, Clients: []WifiClient{}}
	bands := collectCambiumRadios(client, ip, now, m, wifi)
	collectCambiumClients(client, ip, now, bands, m, wifi)

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

// collectCambiumRadios returns the band of each radio, by radio table index.
func collectCambiumRadios(client *gosnmp.GoSNMP, ip string, now time.Time, m *LinuxMetrics, wifi *WifiStatus) map[string]string {
	bandVals, _ := walkIndexed(client, oidCnRadioBand)
	clients, _ := walkIndexed(client, oidCnRadioClients)
	channels, _ := walkIndexed(client, oidCnRadioChannel)
	widths, _ := walkIndexed(client, oidCnRadioWidth)
	powers, _ := walkIndexed(client, oidCnRadioTxPower)
	txBytes, _ := walkIndexed(client, oidCnRadioTxBytes)
	rxBytes, _ := walkIndexed(client, oidCnRadioRxBytes)
	noises, _ := walkIndexed(client, oidCnRadioNoise)
	airtimes, _ := walkIndexed(client, oidCnRadioAirtime)

	bands := map[string]string{}
	idxs := sortedKeys(bandVals)
	for _, idx := range idxs {
		band := sensorName(snmpString(bandVals[idx]))
		if band == "" {
			band = "Radio " + idx
		}
		bands[idx] = band

		r := WifiRadio{
			Band:     band,
			Channel:  snmpString(channels[idx]),
			WidthMHz: snmpString(widths[idx]),
		}
		r.Clients, _ = snmpInt(clients[idx])
		r.TxPowerDBm, _ = snmpInt(powers[idx])
		noise, noiseOK := atoiText(noises[idx])
		r.NoiseDBm = noise
		// Airtime is "total/tx/rx/busy"; the total is the radio's utilisation
		airtime, airtimeOK := atoiText(strings.Split(snmpString(airtimes[idx]), "/")[0])
		r.AirtimePct = airtime
		wifi.Radios = append(wifi.Radios, r)

		m.Sensors = append(m.Sensors, SensorMetrics{Name: band, Unit: "clients", Value: float64(r.Clients)})
		if noiseOK {
			m.Sensors = append(m.Sensors, SensorMetrics{Name: band, Unit: "dBm", Value: float64(noise)})
		}
		if airtimeOK {
			m.Sensors = append(m.Sensors, SensorMetrics{Name: band, Unit: "airtime", Value: float64(airtime)})
		}
		// Rx is what the radio received from clients: traffic into the AP
		key := ip + "/radio/" + idx
		in, inOK := counterRate(key+"/rx", gosnmp.ToBigInt(rxBytes[idx]).Uint64(), now)
		out, outOK := counterRate(key+"/tx", gosnmp.ToBigInt(txBytes[idx]).Uint64(), now)
		if inOK && outOK {
			m.Sensors = append(m.Sensors, trafficSensors(band+" radio", in, out)...)
		}
	}
	return bands
}

func collectCambiumClients(client *gosnmp.GoSNMP, ip string, now time.Time, bands map[string]string, m *LinuxMetrics, wifi *WifiStatus) {
	macs, err := walkIndexed(client, oidCnClientMAC)
	if err != nil {
		return
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

	var rows []string // table row of each client, for its byte counters
	for _, idx := range sortedKeys(macs) {
		c := WifiClient{
			MAC:    strings.ToUpper(snmpString(macs[idx])),
			IP:     snmpString(ips[idx]),
			Name:   strings.TrimSpace(snmpString(names[idx])),
			SSID:   snmpString(ssids[idx]),
			Vendor: snmpString(vendors[idx]),
			Mode:   snmpString(modes[idx]),
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
		rows = append(rows, idx)
	}

	// Chart series are named by hostname, so the same device keeps one line
	// as its IP changes; a repeated hostname gets the end of its MAC added
	labels := clientLabels(wifi.Clients)
	for i, c := range wifi.Clients {
		label := labels[i]
		m.Sensors = append(m.Sensors, SensorMetrics{Name: label, Unit: "snr", Value: float64(c.SNR)})
		// Counters are keyed by MAC: the table's row numbers shift as clients come and go
		key := ip + "/client/" + c.MAC
		idx := rows[i]
		tx, txOK := counterRate(key+"/tx", gosnmp.ToBigInt(txBytes[idx]).Uint64(), now)
		rx, rxOK := counterRate(key+"/rx", gosnmp.ToBigInt(rxBytes[idx]).Uint64(), now)
		if txOK && rxOK {
			m.Sensors = append(m.Sensors, SensorMetrics{Name: label, Unit: "client_mbps", Value: tx + rx})
		}
	}
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

// sortedKeys orders table indexes numerically where they are numbers
func sortedKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, errA := strconv.Atoi(keys[i])
		b, errB := strconv.Atoi(keys[j])
		if errA == nil && errB == nil {
			return a < b
		}
		return keys[i] < keys[j]
	})
	return keys
}
