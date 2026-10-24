package payload

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func gz(t *testing.T, content string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func bundle(t *testing.T, files map[string]string) fstest.MapFS {
	t.Helper()
	fsys := fstest.MapFS{}
	for name, content := range files {
		fsys[name+".gz"] = &fstest.MapFile{Data: gz(t, content)}
	}
	return fsys
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

var helpers = map[string]string{
	"native/piik-capture.exe":     "wrapper",
	"native/piik-capture-bin.exe": "helper binary",
	"tunnel/cloudflared.exe":      "tunnel binary",
}

func TestEnsureUnpacksEveryHelperIntoTheRuntimeFolder(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtime")
	if err := Ensure(bundle(t, helpers), root); err != nil {
		t.Fatal(err)
	}
	for name, want := range helpers {
		if got := read(t, filepath.Join(root, filepath.FromSlash(name))); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestEnsureRepairsADeletedOrDamagedHelperAndKeepsAGoodOne(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtime")
	fsys := bundle(t, helpers)
	if err := Ensure(fsys, root); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(root, "native", "piik-capture.exe")
	damaged := filepath.Join(root, "native", "piik-capture-bin.exe")
	if err := os.Remove(wrapper); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(damaged, []byte("truncated"), 0o755); err != nil { // different size
		t.Fatal(err)
	}
	good := filepath.Join(root, "tunnel", "cloudflared.exe")
	before, _ := os.Stat(good)
	if err := Ensure(fsys, root); err != nil {
		t.Fatal(err)
	}
	if read(t, wrapper) != "wrapper" || read(t, damaged) != "helper binary" {
		t.Fatal("helpers were not repaired")
	}
	if after, _ := os.Stat(good); !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("a healthy helper was rewritten")
	}
}

func TestEnsureReplacesHelpersWhenTheBundleChanges(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtime")
	if err := Ensure(bundle(t, helpers), root); err != nil {
		t.Fatal(err)
	}
	next := map[string]string{}
	for name, content := range helpers {
		next[name] = content
	}
	next["native/piik-capture-bin.exe"] = "helper binary v2 (new build)"
	if err := Ensure(bundle(t, next), root); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(root, "native", "piik-capture-bin.exe")); got != next["native/piik-capture-bin.exe"] {
		t.Fatalf("helper not updated: %q", got)
	}
}

func TestEnsureReplacesAHelperOfTheSameSizeWhenTheBundleChanges(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtime")
	first := map[string]string{"native/piik-capture-bin.exe": "AAAA"}
	if err := Ensure(bundle(t, first), root); err != nil {
		t.Fatal(err)
	}
	second := map[string]string{"native/piik-capture-bin.exe": "BBBB"}
	if err := Ensure(bundle(t, second), root); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(root, "native", "piik-capture-bin.exe")); got != "BBBB" {
		t.Fatalf("same-size change was not applied: %q", got)
	}
}

func TestEnsureDoesNothingWithoutABundle(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtime")
	if err := Ensure(nil, root); err != nil {
		t.Fatal(err)
	}
	if err := Ensure(fstest.MapFS{}, root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("a runtime folder was created without a bundle: %v", err)
	}
}

func TestEnsureNeverLeavesAPartialHelperBehind(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtime")
	fsys := fstest.MapFS{"native/piik-capture-bin.exe.gz": {Data: []byte("not gzip data")}}
	if err := Ensure(fsys, root); err == nil {
		t.Fatal("a corrupt bundle entry was accepted")
	}
	if _, err := os.Stat(filepath.Join(root, "native", "piik-capture-bin.exe")); !os.IsNotExist(err) {
		t.Fatalf("a partial helper was left behind: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(root, "native"))
	for _, entry := range entries {
		t.Fatalf("temporary file left behind: %s", entry.Name())
	}
}
