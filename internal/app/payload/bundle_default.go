//go:build !standalone

package payload

import "io/fs"

// Bundle is nil in an ordinary build, which finds its helpers next to itself.
func Bundle() fs.FS { return nil }
