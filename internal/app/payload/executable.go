package payload

import (
	"os"
	"path/filepath"
)

// EnsureNextToExecutable unpacks the embedded helpers into runtime/ beside the
// running executable (the layout the App already searches), if there are any.
func EnsureNextToExecutable() error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	return Ensure(Bundle(), filepath.Join(filepath.Dir(executable), "runtime"))
}
