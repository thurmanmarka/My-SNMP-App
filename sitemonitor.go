package main

import (
	"fmt"
	"strings"

	"github.com/gosnmp/gosnmp"
)

// PacketFlux SiteMonitor (Base 3) OIDs. PacketFlux publishes no MIB for the
// Base 3, so these come from walking a "SiteMonitor Base 3 Classic". Rows are
// indexed <bus>.<channel>; the name column is the label set in the
// SiteMonitor's web UI.
const (
	oidSMVoltageName  = "1.3.6.1.4.1.32050.3.2.1.1.3" // e.g. "Battery"
	oidSMVoltageValue = "1.3.6.1.4.1.32050.3.2.1.1.4" // microvolts
	oidSMTempName     = "1.3.6.1.4.1.32050.3.2.3.1.3"
	oidSMTempValue    = "1.3.6.1.4.1.32050.3.2.3.1.4" // hundredths of °C
	oidSMTempLabel    = "1.3.6.1.4.1.32050.3.2.3.1.5" // "Board Temperature"
	oidSMPortName     = "1.3.6.1.4.1.32050.3.3.1.1.3"
	oidSMPortCurrent  = "1.3.6.1.4.1.32050.3.3.1.1.12" // microamps
)

// sensorNameMax matches the sensor columns in the database
const sensorNameMax = 64

func isSiteMonitor(d Device) bool {
	return strings.Contains(strings.ToLower(d.Description), "sitemonitor")
}

// collectSiteMonitor reads the board temperature, plus the voltage inputs and
// power ports that have been given a name on the SiteMonitor. Unnamed inputs
// are skipped: they are usually unconnected and read noise.
func collectSiteMonitor(client *gosnmp.GoSNMP) ([]TempMetrics, []SensorMetrics, error) {
	vNames, err := walkIndexed(client, oidSMVoltageName)
	if err != nil {
		return nil, nil, fmt.Errorf("voltages: %w", err)
	}
	vValues, _ := walkIndexed(client, oidSMVoltageValue)
	tNames, _ := walkIndexed(client, oidSMTempName)
	tValues, _ := walkIndexed(client, oidSMTempValue)
	tLabels, _ := walkIndexed(client, oidSMTempLabel)
	pNames, _ := walkIndexed(client, oidSMPortName)
	pCurrents, _ := walkIndexed(client, oidSMPortCurrent)

	var sensors []SensorMetrics
	for idx, nameVal := range vNames {
		name := sensorName(snmpString(nameVal))
		uv, ok := snmpInt64(vValues[idx])
		if name == "" || !ok {
			continue
		}
		sensors = append(sensors, SensorMetrics{Name: name, Unit: "V", Value: float64(uv) / 1e6})
	}
	for idx, nameVal := range pNames {
		name := sensorName(snmpString(nameVal))
		ua, ok := snmpInt64(pCurrents[idx])
		if name == "" || !ok {
			continue
		}
		sensors = append(sensors, SensorMetrics{Name: name, Unit: "A", Value: float64(ua) / 1e6})
	}

	var temps []TempMetrics
	for idx, val := range tValues {
		centiC, ok := snmpInt64(val)
		if !ok {
			continue
		}
		name := sensorName(snmpString(tNames[idx]))
		if name == "" {
			name = sensorName(snmpString(tLabels[idx]))
		}
		if name == "" {
			name = "Temperature " + idx
		}
		temps = append(temps, TempMetrics{Name: name, TempC: float64(centiC) / 100})
	}
	return temps, sensors, nil
}

func sensorName(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > sensorNameMax {
		s = s[:sensorNameMax]
	}
	return s
}
