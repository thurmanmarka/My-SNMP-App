package main

import (
	"fmt"
	"time"

	"github.com/gosnmp/gosnmp"
)

// Device represents a discovered network device
type Device struct {
	IP          string    `yaml:"ip"`
	Hostname    string    `yaml:"hostname,omitempty"`
	Description string    `yaml:"description,omitempty"`
	Contact     string    `yaml:"contact,omitempty"`
	Location    string    `yaml:"location,omitempty"`
	Uptime      string    `yaml:"uptime,omitempty"`
	Community   string    `yaml:"community,omitempty"`
	LastSeen    time.Time `yaml:"last_seen"`
	Latency     string    `yaml:"latency,omitempty"`
}

// DiscoverDevice attempts SNMP discovery on a host
func DiscoverDevice(ip string, communities []string, oids map[string]string, timeout time.Duration, latency time.Duration) (*Device, error) {
	var workingCommunity string
	var snmpClient *gosnmp.GoSNMP

	// Try each community string
	for _, community := range communities {
		client := &gosnmp.GoSNMP{
			Target:    ip,
			Port:      161,
			Community: community,
			Version:   gosnmp.Version2c,
			Timeout:   timeout,
			Retries:   1,
		}

		err := client.Connect()
		if err != nil {
			continue
		}

		// Test with sysDescr OID
		_, err = client.Get([]string{oids["sysDescr"]})
		if err != nil {
			client.Conn.Close()
			continue
		}

		// Found working community
		workingCommunity = community
		snmpClient = client
		break
	}

	if snmpClient == nil {
		return nil, fmt.Errorf("no working SNMP community found")
	}
	defer snmpClient.Conn.Close()

	// Query all OIDs
	device := &Device{
		IP:        ip,
		Community: workingCommunity,
		LastSeen:  time.Now(),
		Latency:   latency.String(),
	}

	// sysDescr
	if val, err := getSNMPValue(snmpClient, oids["sysDescr"]); err == nil {
		device.Description = val
	}

	// sysName
	if val, err := getSNMPValue(snmpClient, oids["sysName"]); err == nil {
		device.Hostname = val
	}

	// sysContact
	if val, err := getSNMPValue(snmpClient, oids["sysContact"]); err == nil {
		device.Contact = val
	}

	// sysLocation
	if val, err := getSNMPValue(snmpClient, oids["sysLocation"]); err == nil {
		device.Location = val
	}

	// sysUpTime
	if val, err := getSNMPValue(snmpClient, oids["sysUpTime"]); err == nil {
		device.Uptime = val
	}

	return device, nil
}

// getSNMPValue performs an SNMP GET and returns the string value
func getSNMPValue(client *gosnmp.GoSNMP, oid string) (string, error) {
	result, err := client.Get([]string{oid})
	if err != nil {
		return "", err
	}

	if len(result.Variables) == 0 {
		return "", fmt.Errorf("no response")
	}

	return fmt.Sprintf("%s", result.Variables[0].Value), nil
}
