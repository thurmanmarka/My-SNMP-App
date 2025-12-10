package main

import (
	"log"
)

func main() {
	config, err := LoadConfig("config.yaml")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	log.Println("Starting My SNMP App web server...")
	err = StartWebServer(config, 8082)
	if err != nil {
		log.Fatalf("Web server failed: %v", err)
	}
}
