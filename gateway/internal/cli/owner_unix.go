//go:build unix

package cli

import (
	"os"
	"syscall"
)

// fileOwner reports the uid and gid of fi on Unix, so writeInPlace can keep a
// root-owned /etc/kelvran-gateway/config.yaml root-owned.
func fileOwner(fi os.FileInfo) (uid, gid int, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}
