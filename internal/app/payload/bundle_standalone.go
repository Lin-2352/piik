//go:build standalone

package payload

import (
	"embed"
	"io/fs"
)

// files is filled by scripts/build-standalone.ps1 (gzip copies of runtime/).
//
//go:embed files
var embedded embed.FS

// Bundle returns the helpers embedded in this executable.
func Bundle() fs.FS {
	bundle, err := fs.Sub(embedded, "files")
	if err != nil {
		return nil
	}
	return bundle
}
