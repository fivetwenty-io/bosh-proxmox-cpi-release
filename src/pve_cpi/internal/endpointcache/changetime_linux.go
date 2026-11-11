package endpointcache

import (
	"io/fs"
	"syscall"
)

// changeTime returns the inode change time of info in nanoseconds, and false
// when info does not carry a stat the platform understands.
func changeTime(info fs.FileInfo) (int64, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Ctim.Sec*1e9 + st.Ctim.Nsec, true
}
