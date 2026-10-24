//go:build !windows

package window

// Open is unavailable off Windows: macOS and Linux keep using the browser.
func Open(string, func()) error { return ErrUnavailable }
