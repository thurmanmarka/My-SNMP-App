package main

import (
	"fmt"
	"strings"

	"github.com/gosnmp/gosnmp"
)

// MIKROTIK-MIB health gauges (mtxrGaugeTable): one row per sensor, with a unit
// code saying how to read the value.
const (
	oidMtxrGaugeName  = "1.3.6.1.4.1.14988.1.1.3.100.1.2"
	oidMtxrGaugeValue = "1.3.6.1.4.1.14988.1.1.3.100.1.3"
	oidMtxrGaugeUnit  = "1.3.6.1.4.1.14988.1.1.3.100.1.4"
)

// mtxrGaugeUnit codes: the unit to store under and the divisor to get there.
// status (6) rows such as psu1-state are flags, not gauges, and are skipped.
var mtxrUnits = map[int]struct {
	unit    string
	divisor float64
}{
	1: {"°C", 1},
	2: {"RPM", 1},
	3: {"V", 10}, // dV
	4: {"A", 10}, // dA
	5: {"W", 10}, // dW
}

func isMikroTik(d Device) bool {
	return strings.Contains(strings.ToLower(d.Description), "routeros")
}

// collectMikroTikHealth reads the health gauges. Temperatures go to the temp
// table like Linux sensors; fans, voltages, currents and power are sensors.
func collectMikroTikHealth(client *gosnmp.GoSNMP) ([]TempMetrics, []SensorMetrics, error) {
	names, err := walkIndexed(client, oidMtxrGaugeName)
	if err != nil {
		return nil, nil, fmt.Errorf("health: %w", err)
	}
	values, _ := walkIndexed(client, oidMtxrGaugeValue)
	units, _ := walkIndexed(client, oidMtxrGaugeUnit)

	var temps []TempMetrics
	var sensors []SensorMetrics
	for idx, nameVal := range names {
		name := sensorName(snmpString(nameVal))
		code, _ := snmpInt(units[idx])
		u, known := mtxrUnits[code]
		v, ok := snmpInt64(values[idx])
		if name == "" || !known || !ok {
			continue
		}
		value := float64(v) / u.divisor
		if u.unit == "°C" {
			temps = append(temps, TempMetrics{Name: name, TempC: value})
		} else {
			sensors = append(sensors, SensorMetrics{Name: name, Unit: u.unit, Value: value})
		}
	}
	return temps, sensors, nil
}
