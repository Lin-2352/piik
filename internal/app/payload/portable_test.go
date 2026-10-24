package payload

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPortableConfigCopiesTheUserConfigurationOnce(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, "appdata", "Piik", "client.json")
	write(t, legacy, "config")
	write(t, legacy+".mode", "local")
	write(t, legacy+".host-settings.json", "presets")
	dataDir := filepath.Join(root, "app", "piik-data")

	path, err := PortableConfig(dataDir, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dataDir, "client.json") {
		t.Fatalf("config path %q", path)
	}
	for suffix, want := range map[string]string{"": "config", ".mode": "local", ".host-settings.json": "presets"} {
		if got := read(t, path+suffix); got != want {
			t.Fatalf("client.json%s = %q, want %q", suffix, got, want)
		}
	}
}

func TestPortableConfigNeverOverwritesPortableSettings(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, "appdata", "Piik", "client.json")
	write(t, legacy+".host-settings.json", "old presets")
	dataDir := filepath.Join(root, "app", "piik-data")
	write(t, filepath.Join(dataDir, "client.json.host-settings.json"), "my portable presets")

	path, err := PortableConfig(dataDir, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, path+".host-settings.json"); got != "my portable presets" {
		t.Fatalf("portable presets were overwritten: %q", got)
	}
}

func TestPortableConfigCopiesOnlyWhatTheUserConfigurationHas(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, "appdata", "Piik", "client.json")
	write(t, legacy+".host-settings.json", "presets")
	dataDir := filepath.Join(root, "app", "piik-data")

	path, err := PortableConfig(dataDir, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, path+".host-settings.json"); got != "presets" {
		t.Fatalf("presets not copied: %q", got)
	}
	for _, suffix := range []string{"", ".mode"} {
		if _, err := os.Stat(path + suffix); !os.IsNotExist(err) {
			t.Fatalf("client.json%s should not exist: %v", suffix, err)
		}
	}
}

func TestPortableConfigWorksWithNothingToCopy(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "app", "piik-data")
	path, err := PortableConfig(dataDir, filepath.Join(root, "missing", "client.json"))
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dataDir, "client.json") {
		t.Fatalf("config path %q", path)
	}
	if info, err := os.Stat(dataDir); err != nil || !info.IsDir() {
		t.Fatalf("data folder was not created: %v", err)
	}
}
