package app

import (
	"context"
	"log/slog"

	"github.com/TNTcraftHIM/Piik/internal/app/browser"
	"github.com/TNTcraftHIM/Piik/internal/app/window"
	"github.com/TNTcraftHIM/Piik/internal/diagnostics"
)

// openTarget returns how the App shows its page: the system browser, or Piik's
// own window when asked for and available (Windows with the WebView2 runtime).
// Closing the window quits the App, as closing a standalone app does.
func openTarget(useWindow bool, quit context.CancelFunc) func(string) error {
	if !useWindow {
		return browser.Open
	}
	return func(target string) error {
		err := window.Open(target, quit)
		if err == nil {
			return nil
		}
		slog.Debug("Could not open the Piik window; using the browser", diagnostics.Error(err))
		return browser.Open(target)
	}
}
