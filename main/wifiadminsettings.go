/*
wifiadminsettings.go: durable persistence for the wifiadmin package's
last-known-good configuration and any in-flight pending transaction -
implements wifiadmin.Persistence following this project's own
established temp-file+fsync+atomic-rename pattern (see
main/alertsettings.go's saveAlertSettings, docs/wifi-administration-
hardening.md's "Persistence and crash recovery" section).

Two separate files, not one, so a pending-transaction write can never
race with or partially corrupt the last-known-good record a rollback
needs to read:

	wifiAdminLastKnownGoodPath = PersistentDataPath + "/wifi-admin-last-known-good.json"
	wifiAdminPendingPath       = PersistentDataPath + "/wifi-admin-pending-transaction.json"
*/
package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/stratux/stratux/wifiadmin"
)

// wifiAdminLastKnownGoodPath/wifiAdminPendingPath are vars, not consts,
// solely so tests can redirect them to a temp directory - the same
// convention as trafficCPASettingsPath.
var (
	wifiAdminLastKnownGoodPath = PersistentDataPath + "/wifi-admin-last-known-good.json"
	wifiAdminPendingPath       = PersistentDataPath + "/wifi-admin-pending-transaction.json"
)

var wifiAdminPersistenceMu sync.Mutex

// filePersistence implements wifiadmin.Persistence over two plain JSON
// files, each written with the temp-file+fsync+atomic-rename pattern.
type filePersistence struct{}

func (filePersistence) SaveLastKnownGood(cfg wifiadmin.Config) error {
	wifiAdminPersistenceMu.Lock()
	defer wifiAdminPersistenceMu.Unlock()
	return atomicWriteJSON(wifiAdminLastKnownGoodPath, cfg)
}

func (filePersistence) LoadLastKnownGood() (wifiadmin.Config, bool, error) {
	wifiAdminPersistenceMu.Lock()
	defer wifiAdminPersistenceMu.Unlock()
	var cfg wifiadmin.Config
	ok, err := readJSONIfExists(wifiAdminLastKnownGoodPath, &cfg)
	return cfg, ok, err
}

func (filePersistence) SavePendingTransaction(rec wifiadmin.PendingTransactionRecord) error {
	wifiAdminPersistenceMu.Lock()
	defer wifiAdminPersistenceMu.Unlock()
	return atomicWriteJSON(wifiAdminPendingPath, rec)
}

func (filePersistence) LoadPendingTransaction() (wifiadmin.PendingTransactionRecord, bool, error) {
	wifiAdminPersistenceMu.Lock()
	defer wifiAdminPersistenceMu.Unlock()
	var rec wifiadmin.PendingTransactionRecord
	ok, err := readJSONIfExists(wifiAdminPendingPath, &rec)
	return rec, ok, err
}

func (filePersistence) ClearPendingTransaction() error {
	wifiAdminPersistenceMu.Lock()
	defer wifiAdminPersistenceMu.Unlock()
	if err := os.Remove(wifiAdminPendingPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// atomicWriteJSON marshals v and writes it to path using this project's
// own established temp-file+fsync+atomic-rename idiom - see
// main/alertsettings.go's saveAlertSettings for the canonical original.
func atomicWriteJSON(path string, v interface{}) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// readJSONIfExists reports ok=false (never an error) if path does not
// exist - a missing file is this feature's expected "nothing persisted
// yet" state, not a failure. A file that exists but fails to parse IS
// reported as an error (never silently treated as absent), matching
// this feature's own "reject corrupt state, never silently accept it"
// requirement.
func readJSONIfExists(path string, v interface{}) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if len(data) == 0 {
		log.Printf("wifiadmin: %s is empty, treating as absent", path)
		return false, nil
	}
	if err := json.Unmarshal(data, v); err != nil {
		return false, err
	}
	return true, nil
}
