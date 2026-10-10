//go:build !unix

package cli

import "os"

// fileOwner has no owner to report off Unix; writeInPlace then skips chown.
func fileOwner(os.FileInfo) (uid, gid int, ok bool) { return 0, 0, false }
