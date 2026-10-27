package main

import (
	"fmt"
	"sync"
	"time"

	"github.com/gosnmp/gosnmp"
)

// IF-MIB columns. The HC (64-bit) octet counters don't wrap at gigabit speeds
// between polls the way the 32-bit ifInOctets ones do.
const (
	oidIfType       = "1.3.6.1.2.1.2.2.1.3"
	oidIfOperStatus = "1.3.6.1.2.1.2.2.1.8"
	oidIfName       = "1.3.6.1.2.1.31.1.1.1.1"
	oidIfHCInOctets = "1.3.6.1.2.1.31.1.1.1.6"
	oidIfHCOutOcts  = "1.3.6.1.2.1.31.1.1.1.10"

	ifOperUp         = 1
	ifTypeLoopback   = 24
	trafficUnit      = "Mbps"
	maxTrafficSample = 15 * time.Minute // older counts are too stale to rate against
)

type octetSample struct {
	in, out uint64
	at      time.Time
}

// The last counter reading per "ip/ifIndex". A rate needs two polls, so traffic
// starts on the second poll after the app starts.
var (
	trafficMu   sync.Mutex
	trafficLast = map[string]octetSample{}
)

// collectTraffic returns in and out Mbps for each interface that is up, named
// "<ifName> in" and "<ifName> out".
func collectTraffic(client *gosnmp.GoSNMP, ip string) ([]SensorMetrics, error) {
	names, err := walkIndexed(client, oidIfName)
	if err != nil {
		return nil, fmt.Errorf("interfaces: %w", err)
	}
	status, _ := walkIndexed(client, oidIfOperStatus)
	types, _ := walkIndexed(client, oidIfType)
	ins, _ := walkIndexed(client, oidIfHCInOctets)
	outs, _ := walkIndexed(client, oidIfHCOutOcts)
	now := time.Now()

	trafficMu.Lock()
	defer trafficMu.Unlock()

	var sensors []SensorMetrics
	for idx, nameVal := range names {
		up, _ := snmpInt(status[idx])
		ifType, _ := snmpInt(types[idx])
		name := sensorName(snmpString(nameVal))
		// RouterOS doesn't give its "lo" the loopback ifType, so match the name too
		if up != ifOperUp || ifType == ifTypeLoopback || name == "lo" || ins[idx] == nil || outs[idx] == nil {
			continue
		}
		cur := octetSample{
			in:  gosnmp.ToBigInt(ins[idx]).Uint64(),
			out: gosnmp.ToBigInt(outs[idx]).Uint64(),
			at:  now,
		}
		key := ip + "/" + idx
		prev, seen := trafficLast[key]
		trafficLast[key] = cur

		elapsed := cur.at.Sub(prev.at)
		// A counter that went backwards means the interface or device reset
		if !seen || elapsed <= 0 || elapsed > maxTrafficSample || cur.in < prev.in || cur.out < prev.out {
			continue
		}
		if len(name) > sensorNameMax-4 {
			name = name[:sensorNameMax-4]
		}
		mbps := func(delta uint64) float64 { return float64(delta) * 8 / elapsed.Seconds() / 1e6 }
		sensors = append(sensors,
			SensorMetrics{Name: name + " in", Unit: trafficUnit, Value: mbps(cur.in - prev.in)},
			SensorMetrics{Name: name + " out", Unit: trafficUnit, Value: mbps(cur.out - prev.out)},
		)
	}
	return sensors, nil
}
