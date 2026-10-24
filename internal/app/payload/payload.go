// Package payload carries the App's helper programs (the native capture helper
// and the public-link tunnel) inside the one executable and unpacks them next
// to it, where the App already looks for them (runtime/native, runtime/tunnel).
package payload

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const manifestName = ".payload.json"

// Ensure makes every helper in bundle exist, unchanged, under runtimeDir. The
// bundle holds gzip-compressed files named like their destination plus ".gz".
// It only writes what is missing, damaged or changed, and every file appears
// by an atomic rename, so a half-written helper is never started. A nil or
// empty bundle (an ordinary, non-standalone build) does nothing.
func Ensure(bundle fs.FS, runtimeDir string) error {
	if bundle == nil {
		return nil
	}
	entries := map[string][]byte{}
	err := fs.WalkDir(bundle, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(name, ".gz") {
			return nil
		}
		data, readErr := fs.ReadFile(bundle, name)
		if readErr != nil {
			return readErr
		}
		entries[strings.TrimSuffix(name, ".gz")] = data
		return nil
	})
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	known := readManifest(runtimeDir)
	done := map[string]string{}
	var failures []error
	for name, compressed := range entries {
		sum := sha256.Sum256(compressed)
		digest := hex.EncodeToString(sum[:])
		destination := filepath.Join(runtimeDir, filepath.FromSlash(name))
		if known[name] == digest && healthy(destination, compressed) {
			done[name] = digest
			continue
		}
		if err := unpack(compressed, destination); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", path.Base(name), err))
			continue
		}
		done[name] = digest
	}
	if err := writeManifest(runtimeDir, done); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

// healthy reports whether destination exists at the size the bundle says.
func healthy(destination string, compressed []byte) bool {
	info, err := os.Stat(destination)
	return err == nil && info.Mode().IsRegular() && info.Size() == int64(uncompressedSize(compressed))
}

// uncompressedSize reads the gzip trailer (ISIZE, size modulo 2^32).
func uncompressedSize(compressed []byte) uint32 {
	if len(compressed) < 4 {
		return 0
	}
	return binary.LittleEndian.Uint32(compressed[len(compressed)-4:])
}

func unpack(compressed []byte, destination string) (err error) {
	directory := filepath.Dir(destination)
	if err = os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	reader, err := gzip.NewReader(strings.NewReader(string(compressed)))
	if err != nil {
		return err
	}
	defer reader.Close()
	temporary, err := os.CreateTemp(directory, filepath.Base(destination)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = temporary.Close()
			_ = os.Remove(temporary.Name())
		}
	}()
	if _, err = io.Copy(temporary, reader); err != nil {
		return err
	}
	if err = temporary.Close(); err != nil {
		return err
	}
	if err = os.Chmod(temporary.Name(), 0o755); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), destination)
}

func readManifest(runtimeDir string) map[string]string {
	data, err := os.ReadFile(filepath.Join(runtimeDir, manifestName))
	if err != nil {
		return nil
	}
	var known map[string]string
	if json.Unmarshal(data, &known) != nil {
		return nil
	}
	return known
}

func writeManifest(runtimeDir string, done map[string]string) error {
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(done)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(runtimeDir, manifestName), data, 0o644)
}
