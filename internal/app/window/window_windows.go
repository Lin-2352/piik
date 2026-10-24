//go:build windows

package window

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	webview2 "github.com/jchv/go-webview2"
	"github.com/jchv/go-webview2/webviewloader"
	"golang.org/x/sys/windows"
)

var (
	mu                  sync.Mutex
	current             webview2.WebView
	setForegroundWindow = windows.NewLazySystemDLL("user32.dll").NewProc("SetForegroundWindow")
)

// Open shows target in the Piik window and returns once the window exists, or
// with an error when it cannot be created (for example the WebView2 runtime is
// missing). A second call reuses the open window. onClosed runs once when the
// user closes the window.
func Open(target string, onClosed func()) error {
	parsed, err := url.Parse(target)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("window URL is invalid")
	}
	mu.Lock()
	if existing := current; existing != nil {
		mu.Unlock()
		existing.Dispatch(func() {
			existing.Navigate(parsed.String())
			setForegroundWindow.Call(uintptr(existing.Window()))
		})
		return nil
	}
	mu.Unlock()
	if _, err := webviewloader.GetInstalledVersion(); err != nil {
		return fmt.Errorf("%w: WebView2 runtime not found: %v", ErrUnavailable, err)
	}
	ready := make(chan error, 1)
	go run(parsed.String(), onClosed, ready)
	return <-ready
}

// run owns the window's thread: Windows delivers its messages there.
func run(target string, onClosed func(), ready chan<- error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	view := webview2.NewWithOptions(webview2.WebViewOptions{
		DataPath:  dataPath(),
		AutoFocus: true,
		WindowOptions: webview2.WindowOptions{
			Title: "Piik", Width: 1360, Height: 860, Center: true, IconId: 1,
		},
	})
	if view == nil {
		ready <- fmt.Errorf("%w: the window could not be created", ErrUnavailable)
		return
	}
	mu.Lock()
	current = view
	mu.Unlock()
	view.Navigate(target)
	ready <- nil
	view.Run()
	mu.Lock()
	current = nil
	mu.Unlock()
	view.Destroy()
	if onClosed != nil {
		onClosed()
	}
}

// dataPath keeps the window's storage (settings, sign-in) between launches.
func dataPath() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		return ""
	}
	path := filepath.Join(base, "Piik", "webview")
	if os.MkdirAll(path, 0o700) != nil {
		return ""
	}
	return path
}
