package launcher

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostSettingsRoundTripAndRejectInvalid(t *testing.T) {
	path := HostSettingsPath(filepath.Join(t.TempDir(), "client.json"))
	if LoadHostSettings(path) != nil {
		t.Fatal("missing settings must load as nil")
	}
	want := `{"version":1,"last":{"codec":"h265"},"presets":[]}`
	if err := SaveHostSettings(path, []byte(want)); err != nil {
		t.Fatal(err)
	}
	if got := string(LoadHostSettings(path)); got != want {
		t.Fatalf("loaded %s", got)
	}
	for _, invalid := range []string{`[1]`, `"text"`, `null`, `{`, `{"a":"` + strings.Repeat("x", MaxHostSettingsBytes) + `"}`} {
		if SaveHostSettings(path, []byte(invalid)) == nil {
			t.Fatalf("saved invalid settings %.20s", invalid)
		}
	}
	if got := string(LoadHostSettings(path)); got != want {
		t.Fatalf("a rejected save replaced the settings: %s", got)
	}
	// A damaged file never blocks startup.
	if err := os.WriteFile(path, []byte(`{"version":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if LoadHostSettings(path) != nil {
		t.Fatal("damaged settings must load as nil")
	}
}
