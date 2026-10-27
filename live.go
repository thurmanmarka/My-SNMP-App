package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gosnmp/gosnmp"
)

// The device popup's live Wi-Fi view reads the AP every few seconds while it's
// open, instead of waiting for the once-a-minute poll. It only reads; changing
// the AP (disconnecting clients) stays in Wi-Fi Control, which is admin-only.

// liveMaxAge lets everyone watching share one read, so several open popups
// don't each hit the AP.
const liveMaxAge = 1500 * time.Millisecond

type liveSnapshot struct {
	At   time.Time   `json:"at"`
	Wifi *WifiStatus `json:"wifi"`
}

var (
	liveMu    sync.Mutex
	liveCache = map[string]liveSnapshot{}
)

// handleAPILive returns a fresh read of a Wi-Fi AP's radios and clients:
// /api/live?ip=x.x.x.x
func handleAPILive(config *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		device, ok := findDevice(r.URL.Query().Get("ip"))
		if !ok || !isCambium(device) {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "no live view for this device"})
			return
		}
		snap, err := readLiveWifi(device, config.SNMP.Port)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"at":   snap.At,
			"wifi": snap.Wifi,
			// The page only offers the link to Wi-Fi Control to admins; the hub
			// enforces that on its side too
			"can_control": r.Header.Get("X-Hub-Role") == "admin",
		})
	}
}

// readLiveWifi holds the lock across the read, so requests that arrive
// together wait for one read instead of each starting their own.
func readLiveWifi(device Device, port int) (liveSnapshot, error) {
	liveMu.Lock()
	defer liveMu.Unlock()
	if snap, ok := liveCache[device.IP]; ok && time.Since(snap.At) < liveMaxAge {
		return snap, nil
	}

	client := &gosnmp.GoSNMP{
		Target:    device.IP,
		Port:      uint16(port),
		Community: device.Community,
		Version:   gosnmp.Version2c,
		Timeout:   3 * time.Second,
		Retries:   1,
	}
	if err := client.Connect(); err != nil {
		return liveSnapshot{}, fmt.Errorf("connect: %w", err)
	}
	defer client.Conn.Close()

	snap := liveSnapshot{At: time.Now(), Wifi: readCambiumWifi(client)}
	if len(snap.Wifi.Radios) == 0 {
		return liveSnapshot{}, fmt.Errorf("%s didn't answer the radio table", device.IP)
	}
	liveCache[device.IP] = snap
	return snap, nil
}

func findDevice(ip string) (Device, bool) {
	deviceCacheMutex.RLock()
	defer deviceCacheMutex.RUnlock()
	if deviceCache == nil || ip == "" {
		return Device{}, false
	}
	for _, d := range deviceCache.Devices {
		if d.IP == ip {
			return d, true
		}
	}
	return Device{}, false
}
