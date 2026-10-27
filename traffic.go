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

type counterSample struct {
	value uint64
	at    time.Time
}

// The last reading of each byte counter, keyed by device and counter. A rate
// needs two polls, so rates start on the second poll after the app starts.
var (
	counterMu   sync.Mutex
	counterLast = map[string]counterSample{}
)

// counterRate turns a byte counter into Mbps against the previous poll's
// reading. A counter that went backwards is treated as a reset (device reboot,
// Wi-Fi client reconnect), not a wrap: that drops one sample when a 32-bit
// counter wraps, instead of drawing a false spike after every reconnect.
func counterRate(key string, value uint64, now time.Time) (float64, bool) {
	counterMu.Lock()
	defer counterMu.Unlock()
	prev, seen := counterLast[key]
	counterLast[key] = counterSample{value, now}
	elapsed := now.Sub(prev.at)
	if !seen || elapsed <= 0 || elapsed > maxTrafficSample || value < prev.value {
		return 0, false
	}
	return float64(value-prev.value) * 8 / elapsed.Seconds() / 1e6, true
}

// pruneCounters forgets counters that stopped reporting, such as Wi-Fi
// clients that left, so the map doesn't grow forever.
func pruneCounters(now time.Time) {
	counterMu.Lock()
	defer counterMu.Unlock()
	for k, s := range counterLast {
		if now.Sub(s.at) > maxTrafficSample {
			delete(counterLast, k)
		}
	}
}

// collectTraffic returns in and out Mbps for each interface that is up, named
// "<ifName> in" and "<ifName> out". include, when set, picks the interfaces.
func collectTraffic(client *gosnmp.GoSNMP, ip string, include func(name string) bool) ([]SensorMetrics, error) {
	names, err := walkIndexed(client, oidIfName)
	if err != nil {
		return nil, fmt.Errorf("interfaces: %w", err)
	}
	status, _ := walkIndexed(client, oidIfOperStatus)
	types, _ := walkIndexed(client, oidIfType)
	ins, _ := walkIndexed(client, oidIfHCInOctets)
	outs, _ := walkIndexed(client, oidIfHCOutOcts)
	now := time.Now()

	var sensors []SensorMetrics
	for idx, nameVal := range names {
		up, _ := snmpInt(status[idx])
		ifType, _ := snmpInt(types[idx])
		name := sensorName(snmpString(nameVal))
		// RouterOS doesn't give its "lo" the loopback ifType, so match the name too
		if up != ifOperUp || ifType == ifTypeLoopback || name == "lo" || ins[idx] == nil || outs[idx] == nil {
			continue
		}
		if include != nil && !include(name) {
			continue
		}
		key := ip + "/if/" + idx
		in, inOK := counterRate(key+"/in", gosnmp.ToBigInt(ins[idx]).Uint64(), now)
		out, outOK := counterRate(key+"/out", gosnmp.ToBigInt(outs[idx]).Uint64(), now)
		if !inOK || !outOK {
			continue
		}
		sensors = append(sensors, trafficSensors(name, in, out)...)
	}
	return sensors, nil
}

// trafficSensors names a pair of rates the way the UI groups traffic charts
func trafficSensors(name string, in, out float64) []SensorMetrics {
	if len(name) > sensorNameMax-4 {
		name = name[:sensorNameMax-4]
	}
	return []SensorMetrics{
		{Name: name + " in", Unit: trafficUnit, Value: in},
		{Name: name + " out", Unit: trafficUnit, Value: out},
	}
}
