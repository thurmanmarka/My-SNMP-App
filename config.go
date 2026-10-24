package main

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config represents the application configuration
type Config struct {
	Network struct {
		Subnet      string `yaml:"subnet"`
		PingTimeout int    `yaml:"ping_timeout"`
		PingWorkers int    `yaml:"ping_workers"`
	} `yaml:"network"`

	SNMP struct {
		Version     string   `yaml:"version"`
		Communities []string `yaml:"communities"`
		Port        int      `yaml:"port"`
		Timeout     int      `yaml:"timeout"`
		Retries     int      `yaml:"retries"`
	} `yaml:"snmp"`

	OIDs map[string]string `yaml:"oids"`

	Output struct {
		DevicesFile string `yaml:"devices_file"`
		Verbose     bool   `yaml:"verbose"`
	} `yaml:"output"`

	Scheduler struct {
		Enabled         bool `yaml:"enabled"`
		IntervalMinutes int  `yaml:"interval_minutes"`
	} `yaml:"scheduler"`

	Poller struct {
		Enabled         bool   `yaml:"enabled"`
		IntervalSeconds int    `yaml:"interval_seconds"`
		CSVDir          string `yaml:"csv_dir"`
	} `yaml:"poller"`
}

// LoadConfig reads and parses the YAML configuration file
func LoadConfig(filename string) (*Config, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// Set defaults if not specified
	if config.Network.PingTimeout == 0 {
		config.Network.PingTimeout = 1
	}
	if config.Network.PingWorkers == 0 {
		config.Network.PingWorkers = 50
	}
	if config.SNMP.Port == 0 {
		config.SNMP.Port = 161
	}
	if config.SNMP.Timeout == 0 {
		config.SNMP.Timeout = 2
	}
	if config.Output.DevicesFile == "" {
		config.Output.DevicesFile = "devices.yaml"
	}
	if config.Scheduler.IntervalMinutes == 0 {
		config.Scheduler.IntervalMinutes = 60
	}
	if config.Poller.IntervalSeconds == 0 {
		config.Poller.IntervalSeconds = 60
	}
	if config.Poller.CSVDir == "" {
		config.Poller.CSVDir = "metrics"
	}

	return &config, nil
}

// DeviceDatabase represents the devices YAML file
type DeviceDatabase struct {
	Devices []Device `yaml:"devices"`
}

// LoadDevices reads the devices database
func LoadDevices(filename string) (*DeviceDatabase, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		// File doesn't exist yet, return empty database
		if os.IsNotExist(err) {
			return &DeviceDatabase{Devices: []Device{}}, nil
		}
		return nil, fmt.Errorf("failed to read devices file: %w", err)
	}

	var db DeviceDatabase
	if err := yaml.Unmarshal(data, &db); err != nil {
		return nil, fmt.Errorf("failed to parse devices file: %w", err)
	}

	return &db, nil
}

// SaveDevices writes the devices database
func SaveDevices(filename string, db *DeviceDatabase) error {
	data, err := yaml.Marshal(db)
	if err != nil {
		return fmt.Errorf("failed to marshal devices: %w", err)
	}

	err = os.WriteFile(filename, data, 0644)
	if err != nil {
		return fmt.Errorf("failed to write devices file: %w", err)
	}

	return nil
}

// MergeDevice adds or updates a device in the database
func (db *DeviceDatabase) MergeDevice(device Device) {
	// Look for existing device by IP
	for i, existing := range db.Devices {
		if existing.IP == device.IP {
			// Update existing device but preserve some fields if new ones are empty
			if device.Hostname != "" {
				existing.Hostname = device.Hostname
			}
			if device.Description != "" {
				existing.Description = device.Description
			}
			if device.Contact != "" {
				existing.Contact = device.Contact
			}
			if device.Location != "" {
				existing.Location = device.Location
			}
			if device.Community != "" {
				existing.Community = device.Community
			}
			existing.Uptime = device.Uptime
			existing.LastSeen = device.LastSeen
			existing.Latency = device.Latency

			db.Devices[i] = existing
			return
		}
	}

	// New device, add to database
	db.Devices = append(db.Devices, device)
}

// GetPingTimeout returns ping timeout as duration
func (c *Config) GetPingTimeout() time.Duration {
	return time.Duration(c.Network.PingTimeout) * time.Second
}

// GetSNMPTimeout returns SNMP timeout as duration
func (c *Config) GetSNMPTimeout() time.Duration {
	return time.Duration(c.SNMP.Timeout) * time.Second
}
