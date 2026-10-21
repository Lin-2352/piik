package launcher

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// MaxHostSettingsBytes bounds the remembered Host settings and presets.
const MaxHostSettingsBytes = 64 * 1024

// HostSettingsPath sits beside client.json for the same reason as the mode
// preference: published App versions reject unknown configuration fields.
func HostSettingsPath(configPath string) string {
	return configPath + ".host-settings.json"
}

// LoadHostSettings returns the stored settings object, or nil when there is
// none or it is unreadable. The Browser owns the schema and drops fields it
// no longer accepts, so a damaged or older file never blocks startup.
func LoadHostSettings(path string) json.RawMessage {
	payload, err := os.ReadFile(path)
	if err != nil || len(payload) > MaxHostSettingsBytes || !isJSONObject(payload) {
		return nil
	}
	return payload
}

// SaveHostSettings replaces the file atomically, so a crash mid-write keeps
// the previous settings instead of a truncated file.
func SaveHostSettings(path string, settings json.RawMessage) error {
	if len(settings) > MaxHostSettingsBytes || !isJSONObject(settings) {
		return errors.New("host settings must be a JSON object within 64 KiB")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".host-settings-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	_, writeErr := temporary.Write(settings)
	closeErr := temporary.Close()
	if err = errors.Join(writeErr, closeErr); err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		_ = os.Remove(name)
	}
	return err
}

func isJSONObject(payload []byte) bool {
	var object map[string]json.RawMessage
	return json.Unmarshal(payload, &object) == nil && object != nil
}
