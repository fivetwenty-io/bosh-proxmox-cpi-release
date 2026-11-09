package privatefile

import (
	"errors"
	"os"
)

// Ops are the filesystem steps an atomic write performs. A test replaces
// them to inject a failure, and a store that does not need durability gets
// ops with no syncs from DefaultOps.
type Ops struct {
	Write    func(*os.File, []byte) (int, error)
	SyncFile func(*os.File) error
	Rename   func(*os.Root, string, string) error
	SyncDir  func(*os.Root) error
}

// DefaultOps returns the real steps. With durable false the file and
// directory syncs do nothing, and the write, rename, and error handling are
// unchanged, which suits a file that is rebuilt when a crash tears it. With
// durable true both syncs reach the disk.
func DefaultOps(durable bool) Ops {
	ops := Ops{
		Write:  func(f *os.File, b []byte) (int, error) { return f.Write(b) },
		Rename: func(r *os.Root, a, b string) error { return r.Rename(a, b) },
	}
	if durable {
		ops.SyncFile = func(f *os.File) error { return f.Sync() }
		ops.SyncDir = SyncDirectory
	} else {
		ops.SyncFile = func(*os.File) error { return nil }
		ops.SyncDir = func(*os.Root) error { return nil }
	}
	return ops
}

// withDefaults fills each step the caller left nil with the durable default,
// so an unset step never skips a sync.
func (o Ops) withDefaults() Ops {
	d := DefaultOps(true)
	if o.Write == nil {
		o.Write = d.Write
	}
	if o.SyncFile == nil {
		o.SyncFile = d.SyncFile
	}
	if o.Rename == nil {
		o.Rename = d.Rename
	}
	if o.SyncDir == nil {
		o.SyncDir = d.SyncDir
	}
	return o
}

// SyncDirectory flushes the directory r names.
func SyncDirectory(r *os.Root) error {
	f, err := r.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}
