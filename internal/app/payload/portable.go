package payload

import (
	"errors"
	"io"
	"os"
	"path/filepath"
)

// configSuffixes are the files that make up one App configuration: the
// configuration itself, the remembered mode and the Host settings and presets.
var configSuffixes = []string{"", ".mode", ".host-settings.json"}

// PortableConfig returns the configuration file inside dataDir (created if
// needed), so settings and presets travel with the folder. The first time,
// whatever the per-user configuration (legacyConfig) already holds is copied
// in; an existing portable file is never overwritten.
func PortableConfig(dataDir, legacyConfig string) (string, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return "", err
	}
	target := filepath.Join(dataDir, "client.json")
	for _, suffix := range configSuffixes {
		// A file that cannot be copied only costs that migrated setting.
		_ = copyIfMissing(legacyConfig+suffix, target+suffix)
	}
	return target, nil
}

func copyIfMissing(source, destination string) error {
	if _, err := os.Stat(destination); err == nil {
		return nil
	}
	in, err := os.Open(source)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(destination)
		return err
	}
	return out.Close()
}

// Standalone reports whether this executable carries its helper programs.
func Standalone() bool { return Bundle() != nil }
