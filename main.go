package main

import (
	"flag"
	"log"
)

func main() {
	importDir := flag.String("import-csv", "", "import metrics_*.csv files from this directory into the database, then exit")
	flag.Parse()

	config, err := LoadConfig("config.yaml")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	if config.Database.Enabled {
		dbStore, err = OpenStore(config)
		if err != nil {
			log.Fatalf("Failed to open database: %v", err)
		}
		log.Printf("Using MariaDB database %s on %s:%d", config.Database.Name, config.Database.Host, config.Database.Port)
	}

	if *importDir != "" {
		if dbStore == nil {
			log.Fatal("-import-csv needs database.enabled: true in config.yaml")
		}
		if err := dbStore.ImportCSV(*importDir); err != nil {
			log.Fatalf("Import failed: %v", err)
		}
		log.Println("Import complete")
		return
	}

	if dbStore != nil {
		if err := dbStore.ImportDevicesYAML(config.Output.DevicesFile); err != nil {
			log.Fatalf("Failed to import %s: %v", config.Output.DevicesFile, err)
		}
		go dbStore.StartMaintenance(config.Database.RetentionDays)
	}

	log.Println("Starting My SNMP App web server...")
	err = StartWebServer(config)
	if err != nil {
		log.Fatalf("Web server failed: %v", err)
	}
}
