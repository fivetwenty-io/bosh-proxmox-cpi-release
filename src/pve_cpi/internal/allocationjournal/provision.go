package allocationjournal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// ProvisionDirectory creates private journal directories for the CPI runtime
// owner. It does not initialize an authority or modify any existing permissions.
// Durability of the chosen filesystem remains an operator responsibility.
func ProvisionDirectory(path string, uid, gid int) (retErr error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == string(filepath.Separator) || uid < 0 || gid < 0 {
		return fmt.Errorf("journal provisioning: require a clean absolute non-root path and valid owner")
	}
	if os.Geteuid() != 0 && (uid != os.Geteuid() || gid != os.Getegid()) {
		return fmt.Errorf("journal provisioning: only root may select another owner")
	}
	root, err := os.OpenRoot(string(filepath.Separator))
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, root.Close()) }()
	parts := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
	for i, part := range parts {
		created := false
		info, err := root.Lstat(part)
		if errors.Is(err, os.ErrNotExist) {
			if err = root.Mkdir(part, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			created = err == nil
			info, err = root.Lstat(part)
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("journal provisioning: symlink or non-directory component rejected")
		}
		child, err := root.OpenRoot(part)
		if err != nil {
			return err
		}
		f, err := child.Open(".")
		if err != nil {
			return errors.Join(err, child.Close())
		}
		opened, statErr := f.Stat()
		if statErr != nil || !os.SameFile(info, opened) {
			return errors.Join(fmt.Errorf("journal provisioning: directory changed during traversal"), statErr, f.Close(), child.Close())
		}
		if created {
			opened, err = initializeProvisionComponent(f, root, uid, gid)
			if err != nil {
				return errors.Join(err, f.Close(), child.Close())
			}
		}
		if err = validateProvisionComponent(opened, uid, gid, i == len(parts)-1); err != nil {
			return errors.Join(err, f.Close(), child.Close())
		}
		if err = errors.Join(f.Close(), root.Close()); err != nil {
			return errors.Join(err, child.Close())
		}
		root = child
	}
	return syncDirectory(root)
}

func validateProvisionComponent(info os.FileInfo, uid, gid int, final bool) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("journal provisioning: invalid directory metadata")
	}
	if final {
		if int64(st.Uid) != int64(uid) || int64(st.Gid) != int64(gid) || info.Mode().Perm() != 0o700 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			return fmt.Errorf("journal provisioning: existing directory must have runtime owner/group and mode 0700; permissions were not changed")
		}
	} else if (st.Uid != 0 && int64(st.Uid) != int64(uid)) || (info.Mode().Perm()&0o022 != 0 && info.Mode()&os.ModeSticky == 0) {
		return fmt.Errorf("journal provisioning: parent directory permits untrusted replacement")
	}
	return nil
}

func initializeProvisionComponent(file *os.File, parent *os.Root, uid, gid int) (os.FileInfo, error) {
	if os.Geteuid() == 0 {
		if err := file.Chown(uid, gid); err != nil {
			return nil, err
		}
	}
	if err := file.Chmod(0o700); err != nil {
		return nil, err
	}
	if err := file.Sync(); err != nil {
		return nil, err
	}
	if err := syncDirectory(parent); err != nil {
		return nil, err
	}
	return file.Stat()
}
