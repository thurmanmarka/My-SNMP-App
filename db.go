package main

import (
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
)

// Store keeps devices, scans and Linux metrics in MariaDB. All times are UTC.
type Store struct {
	db *sql.DB

	// ip -> devices.id, so the poller doesn't look up the same IPs every minute
	idMu    sync.Mutex
	idsByIP map[string]int64
}

// dbStore is nil when the database is disabled; the app then falls back to
// devices.yaml and the metrics CSV files.
var dbStore *Store

var schema = []string{
	`CREATE TABLE IF NOT EXISTS devices (
		id           INT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
		ip           VARCHAR(45)  NOT NULL UNIQUE,
		hostname     VARCHAR(255) NOT NULL DEFAULT '',
		description  TEXT,
		contact      VARCHAR(255) NOT NULL DEFAULT '',
		location     VARCHAR(255) NOT NULL DEFAULT '',
		community    VARCHAR(255) NOT NULL DEFAULT '',
		uptime_ticks BIGINT UNSIGNED NULL,
		latency_ms   DOUBLE NULL,
		first_seen   DATETIME NOT NULL,
		last_seen    DATETIME NOT NULL
	) ENGINE=InnoDB`,
	`CREATE TABLE IF NOT EXISTS scans (
		id            INT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
		started_at    DATETIME NOT NULL,
		finished_at   DATETIME NULL,
		hosts_alive   INT NOT NULL DEFAULT 0,
		devices_found INT NOT NULL DEFAULT 0,
		status        VARCHAR(32)  NOT NULL,
		message       VARCHAR(255) NOT NULL DEFAULT ''
	) ENGINE=InnoDB`,
	`CREATE TABLE IF NOT EXISTS metrics_cpu (
		device_id    INT UNSIGNED NOT NULL,
		ts           DATETIME NOT NULL,
		avg_load_pct DECIMAL(6,2) NOT NULL,
		num_cpus     SMALLINT UNSIGNED NOT NULL,
		PRIMARY KEY (device_id, ts),
		KEY idx_ts (ts),
		FOREIGN KEY (device_id) REFERENCES devices(id) ON DELETE CASCADE
	) ENGINE=InnoDB`,
	`CREATE TABLE IF NOT EXISTS metrics_disk (
		device_id   INT UNSIGNED NOT NULL,
		ts          DATETIME NOT NULL,
		path        VARCHAR(255) NOT NULL,
		total_bytes BIGINT NOT NULL,
		used_bytes  BIGINT NOT NULL,
		use_pct     TINYINT UNSIGNED NOT NULL,
		PRIMARY KEY (device_id, path, ts),
		KEY idx_ts (ts),
		FOREIGN KEY (device_id) REFERENCES devices(id) ON DELETE CASCADE
	) ENGINE=InnoDB`,
	`CREATE TABLE IF NOT EXISTS metrics_temp (
		device_id INT UNSIGNED NOT NULL,
		ts        DATETIME NOT NULL,
		sensor    VARCHAR(64) NOT NULL,
		temp_c    DECIMAL(6,2) NOT NULL,
		PRIMARY KEY (device_id, sensor, ts),
		KEY idx_ts (ts),
		FOREIGN KEY (device_id) REFERENCES devices(id) ON DELETE CASCADE
	) ENGINE=InnoDB`,
	`CREATE TABLE IF NOT EXISTS poll_errors (
		id        BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
		device_id INT UNSIGNED NOT NULL,
		ts        DATETIME NOT NULL,
		error     VARCHAR(512) NOT NULL,
		KEY idx_device_ts (device_id, ts),
		KEY idx_ts (ts),
		FOREIGN KEY (device_id) REFERENCES devices(id) ON DELETE CASCADE
	) ENGINE=InnoDB`,

	// Hourly rollups are kept long after the raw 60-second rows are deleted.
	`CREATE TABLE IF NOT EXISTS metrics_cpu_hourly (
		device_id    INT UNSIGNED NOT NULL,
		hour         DATETIME NOT NULL,
		avg_load_pct DECIMAL(6,2) NOT NULL,
		max_load_pct DECIMAL(6,2) NOT NULL,
		samples      INT UNSIGNED NOT NULL,
		PRIMARY KEY (device_id, hour),
		FOREIGN KEY (device_id) REFERENCES devices(id) ON DELETE CASCADE
	) ENGINE=InnoDB`,
	`CREATE TABLE IF NOT EXISTS metrics_disk_hourly (
		device_id      INT UNSIGNED NOT NULL,
		path           VARCHAR(255) NOT NULL,
		hour           DATETIME NOT NULL,
		total_bytes    BIGINT NOT NULL,
		avg_used_bytes BIGINT NOT NULL,
		max_use_pct    TINYINT UNSIGNED NOT NULL,
		samples        INT UNSIGNED NOT NULL,
		PRIMARY KEY (device_id, path, hour),
		FOREIGN KEY (device_id) REFERENCES devices(id) ON DELETE CASCADE
	) ENGINE=InnoDB`,
	`CREATE TABLE IF NOT EXISTS metrics_temp_hourly (
		device_id  INT UNSIGNED NOT NULL,
		sensor     VARCHAR(64) NOT NULL,
		hour       DATETIME NOT NULL,
		avg_temp_c DECIMAL(6,2) NOT NULL,
		max_temp_c DECIMAL(6,2) NOT NULL,
		samples    INT UNSIGNED NOT NULL,
		PRIMARY KEY (device_id, sensor, hour),
		FOREIGN KEY (device_id) REFERENCES devices(id) ON DELETE CASCADE
	) ENGINE=InnoDB`,
}

// OpenStore connects to MariaDB and creates any missing tables.
func OpenStore(config *Config) (*Store, error) {
	c := config.Database
	mc := mysql.NewConfig()
	mc.User = c.User
	mc.Passwd = c.Password
	mc.Net = "tcp"
	mc.Addr = fmt.Sprintf("%s:%d", c.Host, c.Port)
	mc.DBName = c.Name
	mc.ParseTime = true
	mc.Loc = time.UTC
	mc.Params = map[string]string{"time_zone": "'+00:00'"}

	db, err := sql.Open("mysql", mc.FormatDSN())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect to %s as %s: %w", mc.Addr, c.User, err)
	}
	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("create schema: %w", err)
		}
	}
	return &Store{db: db, idsByIP: map[string]int64{}}, nil
}

// deviceID returns the devices.id for ip, adding a bare row if the IP is new.
func (s *Store) deviceID(ip string) (int64, error) {
	s.idMu.Lock()
	defer s.idMu.Unlock()
	if id, ok := s.idsByIP[ip]; ok {
		return id, nil
	}
	now := time.Now().UTC()
	// LAST_INSERT_ID(id) makes the existing row's id come back on a duplicate.
	res, err := s.db.Exec(`INSERT INTO devices (ip, first_seen, last_seen) VALUES (?, ?, ?)
		ON DUPLICATE KEY UPDATE id = LAST_INSERT_ID(id)`, ip, now, now)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	s.idsByIP[ip] = id
	return id, nil
}

// LoadDevices returns every device, in the order they were first seen.
func (s *Store) LoadDevices() (*DeviceDatabase, error) {
	rows, err := s.db.Query(`SELECT ip, hostname, COALESCE(description, ''), contact, location,
		community, uptime_ticks, latency_ms, last_seen FROM devices ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	db := &DeviceDatabase{Devices: []Device{}}
	for rows.Next() {
		var d Device
		var uptime sql.NullInt64
		var latency sql.NullFloat64
		if err := rows.Scan(&d.IP, &d.Hostname, &d.Description, &d.Contact, &d.Location,
			&d.Community, &uptime, &latency, &d.LastSeen); err != nil {
			return nil, err
		}
		if uptime.Valid {
			d.Uptime = strconv.FormatInt(uptime.Int64, 10)
		}
		if latency.Valid {
			d.Latency = time.Duration(latency.Float64 * float64(time.Millisecond)).String()
		}
		d.LastSeen = d.LastSeen.Local()
		db.Devices = append(db.Devices, d)
	}
	return db, rows.Err()
}

// SaveDevices inserts or updates every device in one transaction. first_seen is
// only set when a device is new.
func (s *Store) SaveDevices(devices []Device) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT INTO devices
		(ip, hostname, description, contact, location, community, uptime_ticks, latency_ms, first_seen, last_seen)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			hostname = VALUES(hostname), description = VALUES(description),
			contact = VALUES(contact), location = VALUES(location),
			community = VALUES(community), uptime_ticks = VALUES(uptime_ticks),
			latency_ms = VALUES(latency_ms), last_seen = VALUES(last_seen)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, d := range devices {
		var uptime sql.NullInt64
		if n, err := strconv.ParseInt(d.Uptime, 10, 64); err == nil {
			uptime = sql.NullInt64{Int64: n, Valid: true}
		}
		var latency sql.NullFloat64
		if dur, err := time.ParseDuration(d.Latency); err == nil {
			latency = sql.NullFloat64{Float64: float64(dur) / float64(time.Millisecond), Valid: true}
		}
		lastSeen := d.LastSeen.UTC()
		if d.LastSeen.IsZero() {
			lastSeen = time.Now().UTC()
		}
		if _, err := stmt.Exec(d.IP, d.Hostname, d.Description, d.Contact, d.Location,
			d.Community, uptime, latency, lastSeen, lastSeen); err != nil {
			return fmt.Errorf("save device %s: %w", d.IP, err)
		}
	}
	return tx.Commit()
}

// ImportDevicesYAML copies devices.yaml into an empty devices table, so the
// first start with the database enabled keeps the existing inventory.
func (s *Store) ImportDevicesYAML(path string) error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM devices`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	yamlDB, err := LoadDevices(path)
	if err != nil || len(yamlDB.Devices) == 0 {
		return err
	}
	if err := s.SaveDevices(yamlDB.Devices); err != nil {
		return err
	}
	log.Printf("Imported %d devices from %s into the database", len(yamlDB.Devices), path)
	return nil
}

// StartScan records a scan as running and returns its id.
func (s *Store) StartScan(startedAt time.Time) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO scans (started_at, status) VALUES (?, 'running')`, startedAt.UTC())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FinishScan records how a scan ended.
func (s *Store) FinishScan(id int64, hostsAlive, devicesFound int, status, message string) error {
	_, err := s.db.Exec(`UPDATE scans SET finished_at = ?, hosts_alive = ?, devices_found = ?,
		status = ?, message = ? WHERE id = ?`,
		time.Now().UTC(), hostsAlive, devicesFound, status, message, id)
	return err
}

// SaveMetrics writes one poll's results. Failed polls go to poll_errors.
func (s *Store) SaveMetrics(results []*LinuxMetrics) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, m := range results {
		id, err := s.deviceID(m.IP)
		if err != nil {
			return fmt.Errorf("device id for %s: %w", m.IP, err)
		}
		ts := m.CollectedAt.UTC()

		if m.Error != "" {
			msg := m.Error
			if len(msg) > 512 {
				msg = msg[:512]
			}
			if _, err := tx.Exec(`INSERT INTO poll_errors (device_id, ts, error) VALUES (?, ?, ?)`,
				id, ts, msg); err != nil {
				return err
			}
			continue
		}

		// INSERT IGNORE: a repeated timestamp for the same device is skipped, not an error
		if m.CPU.NumCPUs > 0 {
			if _, err := tx.Exec(`INSERT IGNORE INTO metrics_cpu (device_id, ts, avg_load_pct, num_cpus)
				VALUES (?, ?, ?, ?)`, id, ts, m.CPU.AvgLoadPct, m.CPU.NumCPUs); err != nil {
				return err
			}
		}
		for _, d := range m.Disks {
			if _, err := tx.Exec(`INSERT IGNORE INTO metrics_disk (device_id, ts, path, total_bytes, used_bytes, use_pct)
				VALUES (?, ?, ?, ?, ?, ?)`, id, ts, d.Path, d.TotalBytes, d.UsedBytes, d.UsePct); err != nil {
				return err
			}
		}
		for _, t := range m.Temps {
			if _, err := tx.Exec(`INSERT IGNORE INTO metrics_temp (device_id, ts, sensor, temp_c)
				VALUES (?, ?, ?, ?)`, id, ts, t.Name, t.TempC); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// rollupQueries summarise raw rows into hourly rows for completed hours in
// [from, to). Re-running them over the same hours just recomputes the rows.
var rollupQueries = []string{
	`INSERT INTO metrics_cpu_hourly (device_id, hour, avg_load_pct, max_load_pct, samples)
		SELECT device_id, DATE_FORMAT(ts, '%Y-%m-%d %H:00:00') AS h,
			AVG(avg_load_pct), MAX(avg_load_pct), COUNT(*)
		FROM metrics_cpu WHERE ts >= ? AND ts < ? GROUP BY device_id, h
		ON DUPLICATE KEY UPDATE avg_load_pct = VALUES(avg_load_pct),
			max_load_pct = VALUES(max_load_pct), samples = VALUES(samples)`,
	`INSERT INTO metrics_disk_hourly (device_id, path, hour, total_bytes, avg_used_bytes, max_use_pct, samples)
		SELECT device_id, path, DATE_FORMAT(ts, '%Y-%m-%d %H:00:00') AS h,
			MAX(total_bytes), AVG(used_bytes), MAX(use_pct), COUNT(*)
		FROM metrics_disk WHERE ts >= ? AND ts < ? GROUP BY device_id, path, h
		ON DUPLICATE KEY UPDATE total_bytes = VALUES(total_bytes),
			avg_used_bytes = VALUES(avg_used_bytes), max_use_pct = VALUES(max_use_pct),
			samples = VALUES(samples)`,
	`INSERT INTO metrics_temp_hourly (device_id, sensor, hour, avg_temp_c, max_temp_c, samples)
		SELECT device_id, sensor, DATE_FORMAT(ts, '%Y-%m-%d %H:00:00') AS h,
			AVG(temp_c), MAX(temp_c), COUNT(*)
		FROM metrics_temp WHERE ts >= ? AND ts < ? GROUP BY device_id, sensor, h
		ON DUPLICATE KEY UPDATE avg_temp_c = VALUES(avg_temp_c),
			max_temp_c = VALUES(max_temp_c), samples = VALUES(samples)`,
}

// Rollup builds hourly rows for every completed hour since from.
func (s *Store) Rollup(from time.Time) error {
	from = from.UTC().Truncate(time.Hour)
	to := time.Now().UTC().Truncate(time.Hour)
	for _, q := range rollupQueries {
		if _, err := s.db.Exec(q, from, to); err != nil {
			return err
		}
	}
	return nil
}

// Prune deletes raw rows older than cutoff, a batch at a time so a large delete
// doesn't hold locks against the poller.
func (s *Store) Prune(cutoff time.Time) (int64, error) {
	var total int64
	for _, table := range []string{"metrics_cpu", "metrics_disk", "metrics_temp", "poll_errors"} {
		for {
			res, err := s.db.Exec(`DELETE FROM `+table+` WHERE ts < ? LIMIT 10000`, cutoff.UTC())
			if err != nil {
				return total, fmt.Errorf("prune %s: %w", table, err)
			}
			n, _ := res.RowsAffected()
			total += n
			if n < 10000 {
				break
			}
		}
	}
	return total, nil
}

// StartMaintenance rolls up and prunes now and then once an hour. Rollups look
// back a few hours so an hour missed while the app was down is still covered.
func (s *Store) StartMaintenance(retentionDays int) {
	retention := time.Duration(retentionDays) * 24 * time.Hour
	run := func(lookback time.Duration) {
		if err := s.Rollup(time.Now().Add(-lookback)); err != nil {
			log.Printf("DB maintenance: rollup error: %v", err)
			return
		}
		// Only prune after the rollup succeeded, so nothing is deleted un-summarised
		n, err := s.Prune(time.Now().Add(-retention))
		if err != nil {
			log.Printf("DB maintenance: %v", err)
		} else if n > 0 {
			log.Printf("DB maintenance: pruned %d rows older than %d days", n, retentionDays)
		}
	}

	run(retention)
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		run(3 * time.Hour)
	}
}

// ImportCSV loads the metrics_*.csv files the app wrote before it used the
// database. Rows already in the database are skipped, so it is safe to re-run.
func (s *Store) ImportCSV(dir string) error {
	files := []struct {
		name string
		cols int
		stmt string
		args func(id int64, ts time.Time, rec []string) ([]any, error)
	}{
		{"metrics_cpu.csv", 4,
			`INSERT IGNORE INTO metrics_cpu (device_id, ts, avg_load_pct, num_cpus) VALUES (?, ?, ?, ?)`,
			func(id int64, ts time.Time, rec []string) ([]any, error) {
				load, err := strconv.ParseFloat(rec[2], 64)
				if err != nil {
					return nil, err
				}
				cpus, err := strconv.Atoi(rec[3])
				return []any{id, ts, load, cpus}, err
			}},
		// avail_bytes (column 5) is total - used, so it isn't stored
		{"metrics_disk.csv", 7,
			`INSERT IGNORE INTO metrics_disk (device_id, ts, path, total_bytes, used_bytes, use_pct) VALUES (?, ?, ?, ?, ?, ?)`,
			func(id int64, ts time.Time, rec []string) ([]any, error) {
				total, err1 := strconv.ParseInt(rec[3], 10, 64)
				used, err2 := strconv.ParseInt(rec[4], 10, 64)
				pct, err3 := strconv.Atoi(rec[6])
				return []any{id, ts, rec[2], total, used, pct}, errors.Join(err1, err2, err3)
			}},
		{"metrics_temp.csv", 4,
			`INSERT IGNORE INTO metrics_temp (device_id, ts, sensor, temp_c) VALUES (?, ?, ?, ?)`,
			func(id int64, ts time.Time, rec []string) ([]any, error) {
				temp, err := strconv.ParseFloat(rec[3], 64)
				return []any{id, ts, rec[2], temp}, err
			}},
	}

	for _, f := range files {
		path := filepath.Join(dir, f.name)
		inserted, skipped, err := s.importCSVFile(path, f.cols, f.stmt, f.args)
		if errors.Is(err, os.ErrNotExist) {
			log.Printf("Import: %s not found, skipping", path)
			continue
		}
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		log.Printf("Import: %s: %d rows added, %d bad or duplicate rows skipped", path, inserted, skipped)
	}

	// Build the hourly rollups for the imported history
	return s.Rollup(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
}

func (s *Store) importCSVFile(path string, cols int, query string,
	args func(int64, time.Time, []string) ([]any, error)) (inserted, skipped int64, err error) {

	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	if _, err := r.Read(); err == io.EOF { // header
		return 0, 0, nil
	} else if err != nil {
		return 0, 0, err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(query)
	if err != nil {
		return 0, 0, err
	}
	defer stmt.Close()

	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil || len(rec) != cols {
			skipped++
			continue
		}
		ts, err := time.Parse(time.RFC3339, rec[0])
		if err != nil {
			skipped++
			continue
		}
		id, err := s.deviceID(rec[1])
		if err != nil {
			return inserted, skipped, err
		}
		a, err := args(id, ts.UTC(), rec)
		if err != nil {
			skipped++
			continue
		}
		res, err := stmt.Exec(a...)
		if err != nil {
			return inserted, skipped, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			inserted++
		} else {
			skipped++
		}
	}
	return inserted, skipped, tx.Commit()
}

// Point is one chart sample. T is Unix milliseconds. Max is only set for
// hourly rollups, where V is the hour's average.
type Point struct {
	T   int64    `json:"t"`
	V   float64  `json:"v"`
	Max *float64 `json:"max,omitempty"`
}

// History is a device's metrics over a time range, ready for charting.
type History struct {
	IP         string             `json:"ip"`
	Resolution string             `json:"resolution"` // "raw" or "hourly"
	From       int64              `json:"from"`
	To         int64              `json:"to"`
	CPU        []Point            `json:"cpu"`
	Disks      map[string][]Point `json:"disks"` // mount path -> use %
	DiskTotals map[string]int64   `json:"disk_totals"`
	Temps      map[string][]Point `json:"temps"` // sensor -> °C
}

// ErrUnknownDevice is returned by History for an IP that isn't in devices.
var ErrUnknownDevice = errors.New("unknown device")

// History returns metrics for ip since the given time. Short ranges use the
// raw 60-second rows; longer ones use the hourly rollups, which also outlive
// the raw retention period.
func (s *Store) History(ip string, since time.Time, hourly bool) (*History, error) {
	var id int64
	err := s.db.QueryRow(`SELECT id FROM devices WHERE ip = ?`, ip).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUnknownDevice
	}
	if err != nil {
		return nil, err
	}

	h := &History{
		IP:         ip,
		Resolution: "raw",
		From:       since.UnixMilli(),
		To:         time.Now().UnixMilli(),
		CPU:        []Point{},
		Disks:      map[string][]Point{},
		DiskTotals: map[string]int64{},
		Temps:      map[string][]Point{},
	}
	since = since.UTC()

	var cpuQ, diskQ, tempQ string
	if hourly {
		h.Resolution = "hourly"
		cpuQ = `SELECT hour, avg_load_pct, max_load_pct FROM metrics_cpu_hourly
			WHERE device_id = ? AND hour >= ? ORDER BY hour`
		diskQ = `SELECT path, hour, 100 * avg_used_bytes / NULLIF(total_bytes, 0), max_use_pct, total_bytes
			FROM metrics_disk_hourly WHERE device_id = ? AND hour >= ? ORDER BY path, hour`
		tempQ = `SELECT sensor, hour, avg_temp_c, max_temp_c FROM metrics_temp_hourly
			WHERE device_id = ? AND hour >= ? ORDER BY sensor, hour`
	} else {
		cpuQ = `SELECT ts, avg_load_pct, NULL FROM metrics_cpu
			WHERE device_id = ? AND ts >= ? ORDER BY ts`
		diskQ = `SELECT path, ts, 100 * used_bytes / NULLIF(total_bytes, 0), NULL, total_bytes
			FROM metrics_disk WHERE device_id = ? AND ts >= ? ORDER BY path, ts`
		tempQ = `SELECT sensor, ts, temp_c, NULL FROM metrics_temp
			WHERE device_id = ? AND ts >= ? ORDER BY sensor, ts`
	}

	point := func(t time.Time, v float64, max sql.NullFloat64) Point {
		p := Point{T: t.UnixMilli(), V: v}
		if max.Valid {
			m := max.Float64
			p.Max = &m
		}
		return p
	}

	rows, err := s.db.Query(cpuQ, id, since)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var t time.Time
		var v float64
		var max sql.NullFloat64
		if err := rows.Scan(&t, &v, &max); err != nil {
			rows.Close()
			return nil, err
		}
		h.CPU = append(h.CPU, point(t, v, max))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = s.db.Query(diskQ, id, since)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var path string
		var t time.Time
		var v sql.NullFloat64
		var max sql.NullFloat64
		var total int64
		if err := rows.Scan(&path, &t, &v, &max, &total); err != nil {
			rows.Close()
			return nil, err
		}
		if !v.Valid { // zero-size filesystem
			continue
		}
		h.Disks[path] = append(h.Disks[path], point(t, v.Float64, max))
		h.DiskTotals[path] = total
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = s.db.Query(tempQ, id, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sensor string
		var t time.Time
		var v float64
		var max sql.NullFloat64
		if err := rows.Scan(&sensor, &t, &v, &max); err != nil {
			return nil, err
		}
		h.Temps[sensor] = append(h.Temps[sensor], point(t, v, max))
	}
	return h, rows.Err()
}
