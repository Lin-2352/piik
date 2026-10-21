// Package window shows the Piik UI in its own native window instead of a
// browser tab. The window only hosts the existing local page; no UI lives here.
package window

import "errors"

// ErrUnavailable means no native window can be shown on this machine, so the
// caller falls back to the system browser.
var ErrUnavailable = errors.New("native window is unavailable")
