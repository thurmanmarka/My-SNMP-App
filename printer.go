package main

import (
	"fmt"
	"strings"

	"github.com/gosnmp/gosnmp"
)

// Printer-MIB (RFC 3805) and HOST-RESOURCES printer status
const (
	oidPrtSupplyDescr  = "1.3.6.1.2.1.43.11.1.1.6" // "Black Toner Cartridge", "Belt Unit"...
	oidPrtSupplyMax    = "1.3.6.1.2.1.43.11.1.1.8" // -2 unknown
	oidPrtSupplyLevel  = "1.3.6.1.2.1.43.11.1.1.9" // -3 "some remaining", -2 unknown, 0 empty
	oidPrtLifeCount    = "1.3.6.1.2.1.43.10.2.1.4" // pages printed
	oidPrtTrayName     = "1.3.6.1.2.1.43.8.2.1.13"
	oidPrtTrayMax      = "1.3.6.1.2.1.43.8.2.1.9"
	oidPrtTrayLevel    = "1.3.6.1.2.1.43.8.2.1.10"
	oidPrtDisplay      = "1.3.6.1.2.1.43.16.5.1.2" // front-panel text, e.g. "Sleep"
	oidHrPrinterStatus = "1.3.6.1.2.1.25.3.5.1.1"

	prtLevelSomeRemaining = -3

	// Brother's private MIB has the toner levels that the Printer-MIB only
	// reports as "some remaining", and colour/mono page counts. Both are
	// blobs of <code> 0x01 <len> <big-endian value> records ending in 0xff.
	// Codes are as decoded by Home Assistant's Brother integration and
	// checked against the Printer-MIB values on an HL-L8360-series printer.
	oidBrotherMaintenance = "1.3.6.1.4.1.2435.2.3.9.4.2.1.5.5.8.0"
	oidBrotherCounters    = "1.3.6.1.4.1.2435.2.3.9.4.2.1.5.5.10.0"
)

// Brother maintenance codes, values in hundredths of a percent remaining
var brotherToner = map[byte]string{0x6f: "Black", 0x70: "Cyan", 0x71: "Magenta", 0x72: "Yellow"}
var brotherParts = map[byte]string{0x6a: "Fuser", 0x6c: "Paper feed kit MP", 0x6d: "Paper feed kit 1"}

// Brother counter codes, in pages
var brotherPages = map[byte]string{0x01: "Black & white", 0x02: "Colour"}

var hrPrinterStatusText = map[int]string{1: "Other", 2: "Unknown", 3: "Idle", 4: "Printing", 5: "Warming up"}

// PrinterStatus is the printer's live state: shown in the device popup, not stored.
type PrinterStatus struct {
	Status   string          `json:"status"`
	Display  string          `json:"display"`
	Pages    int64           `json:"pages"`
	Supplies []PrinterSupply `json:"supplies"`
	Trays    []PrinterTray   `json:"trays"`
}

type PrinterSupply struct {
	Name string   `json:"name"`
	Pct  *float64 `json:"pct,omitempty"` // nil when the printer only says "some remaining"
	Note string   `json:"note,omitempty"`
}

type PrinterTray struct {
	Name     string `json:"name"`
	Capacity int    `json:"capacity"`
	State    string `json:"state"`
}

// isPrinter matches the printers the poller knows. The Printer-MIB part is
// standard; add other makes here as they turn up.
func isPrinter(d Device) bool {
	return strings.Contains(strings.ToLower(d.Description), "brother")
}

func collectPrinter(client *gosnmp.GoSNMP, m *LinuxMetrics) error {
	descrs, err := walkIndexed(client, oidPrtSupplyDescr)
	if err != nil {
		return fmt.Errorf("printer: %w", err)
	}
	maxes, _ := walkIndexed(client, oidPrtSupplyMax)
	levels, _ := walkIndexed(client, oidPrtSupplyLevel)
	p := &PrinterStatus{Supplies: []PrinterSupply{}, Trays: []PrinterTray{}}

	toner := map[string]float64{}
	if blob, ok := getBytes(client, oidBrotherMaintenance); ok {
		for code, v := range brotherRecords(blob) {
			if color, ok := brotherToner[code]; ok {
				toner[color] = float64(v) / 100
			}
			if part, ok := brotherParts[code]; ok {
				m.Sensors = append(m.Sensors, SensorMetrics{Name: part, Unit: "life", Value: float64(v) / 100})
			}
		}
	}

	for _, idx := range sortedKeys(descrs) {
		name := sensorName(snmpString(descrs[idx]))
		max, _ := snmpInt(maxes[idx])
		level, _ := snmpInt(levels[idx])
		s := PrinterSupply{Name: name}
		color := strings.Fields(name + " ")[0]
		switch {
		case strings.Contains(name, "Toner Cartridge") && hasKey(toner, color):
			pct := toner[color]
			s.Pct = &pct
			m.Sensors = append(m.Sensors, SensorMetrics{Name: color, Unit: "toner", Value: pct})
		case max > 0 && level >= 0:
			pct := 100 * float64(level) / float64(max)
			s.Pct = &pct
			s.Note = fmt.Sprintf("%d of %d pages left", level, max)
			m.Sensors = append(m.Sensors, SensorMetrics{Name: name, Unit: "life", Value: pct})
		case level == 0:
			s.Note = "Empty"
		case level == prtLevelSomeRemaining:
			s.Note = "OK"
		default:
			s.Note = "Unknown"
		}
		p.Supplies = append(p.Supplies, s)
	}

	if counts, err := walkIndexed(client, oidPrtLifeCount); err == nil {
		if n, ok := snmpInt64(firstValue(counts)); ok {
			p.Pages = n
			m.Sensors = append(m.Sensors, SensorMetrics{Name: "Total", Unit: "pages", Value: float64(n)})
		}
	}
	if blob, ok := getBytes(client, oidBrotherCounters); ok {
		for code, v := range brotherRecords(blob) {
			if name, ok := brotherPages[code]; ok {
				m.Sensors = append(m.Sensors, SensorMetrics{Name: name, Unit: "pages", Value: float64(v)})
			}
		}
	}

	trayNames, _ := walkIndexed(client, oidPrtTrayName)
	trayMaxes, _ := walkIndexed(client, oidPrtTrayMax)
	trayLevels, _ := walkIndexed(client, oidPrtTrayLevel)
	for _, idx := range sortedKeys(trayNames) {
		name := snmpString(trayNames[idx])
		if name == "" || name == "AUTO" { // "AUTO" is tray selection, not a tray
			continue
		}
		t := PrinterTray{Name: name}
		t.Capacity, _ = snmpInt(trayMaxes[idx])
		level, _ := snmpInt(trayLevels[idx])
		switch {
		case level == 0:
			t.State = "Empty"
		case level == prtLevelSomeRemaining:
			t.State = "Paper loaded"
		case level > 0:
			t.State = fmt.Sprintf("%d sheets", level)
		default:
			t.State = "Unknown"
		}
		p.Trays = append(p.Trays, t)
	}

	if st, err := walkIndexed(client, oidHrPrinterStatus); err == nil {
		if n, ok := snmpInt(firstValue(st)); ok {
			p.Status = hrPrinterStatusText[n]
		}
	}
	if disp, err := walkIndexed(client, oidPrtDisplay); err == nil {
		var lines []string
		for _, idx := range sortedKeys(disp) {
			if line := strings.TrimSpace(snmpString(disp[idx])); line != "" {
				lines = append(lines, line)
			}
		}
		p.Display = strings.Join(lines, " · ")
	}

	m.Printer = p
	return nil
}

// brotherRecords decodes Brother's <code> 0x01 <len> <value> blobs
func brotherRecords(b []byte) map[byte]uint64 {
	out := map[byte]uint64{}
	for i := 0; i+3 <= len(b) && b[i] != 0xff; {
		code, n := b[i], int(b[i+2])
		if i+3+n > len(b) {
			break
		}
		var v uint64
		for _, x := range b[i+3 : i+3+n] {
			v = v<<8 | uint64(x)
		}
		out[code] = v
		i += 3 + n
	}
	return out
}

func getBytes(client *gosnmp.GoSNMP, oid string) ([]byte, bool) {
	res, err := client.Get([]string{oid})
	if err != nil || len(res.Variables) == 0 {
		return nil, false
	}
	b, ok := res.Variables[0].Value.([]byte)
	return b, ok
}

func hasKey(m map[string]float64, k string) bool {
	_, ok := m[k]
	return ok
}
