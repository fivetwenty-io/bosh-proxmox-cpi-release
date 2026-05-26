// Package configdrive authors OpenStack-style ConfigDrive ISO images used to
// deliver BOSH agent settings to a newly created VM. The ISO 9660 + Rock Ridge
// volume is labeled "config-2" and contains:
//
//	/openstack/latest/user_data      — raw settings.json bytes
//	/openstack/latest/meta_data.json — minimal OpenStack metadata stub
//	/ec2/latest/user-data            — same payload (EC2 datasource fallback)
//	/ec2/latest/meta-data.json       — same payload (EC2 datasource fallback)
//
// BOSH openstack-kvm stemcells configure the agent's ConfigDrive datasource
// against the /openstack/latest/ paths (matching bosh-openstack-cpi). The
// /ec2/latest/ paths remain for stemcells that fall back to the EC2 datasource.
package configdrive

import (
	"fmt"
	"os"
	"path/filepath"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
)

// volumeLabel is the label the ConfigDrive datasource scans for when searching
// attached block devices. Case-insensitive, but stemcells typically test uppercase.
const volumeLabel = "config-2"

// isoSize is the on-disk size of the ISO image. ConfigDrive payloads are tiny
// (well under 1 MiB), but go-diskfs requires a size hint up front.
// 10 MiB matches the upstream example and leaves headroom for filesystem overhead.
const isoSize int64 = 10 * 1024 * 1024

// Build authors an ISO 9660 + Rock Ridge volume containing the OpenStack
// ConfigDrive layout that BOSH agents on openstack-kvm stemcells expect.
// payload is the raw settings.json bytes written to /openstack/latest/user_data
// (the path the BOSH openstack infrastructure datasource reads) and mirrored
// to /ec2/latest/user-data for stemcells whose datasource is configured for EC2.
// A minimal /openstack/latest/meta_data.json stub is generated alongside.
//
// Returns the path to the finalized ISO file on disk and a cleanup func that
// removes the temp directory containing it. The caller must invoke cleanup once
// the ISO has been uploaded. On any error the temp directory is removed before
// returning.
//
// The ISO path is placed inside a process-owned temp directory created with
// os.MkdirTemp (random suffix) to eliminate the TOCTOU race that existed when
// os.CreateTemp created a named placeholder that was then removed and re-opened.
func Build(payload []byte) (path string, cleanup func(), err error) {
	// Create a process-owned temp directory. The directory name carries a random
	// suffix chosen by the OS, so no other process can predict or race the ISO path.
	dir, err := os.MkdirTemp("", "bosh-cpi-configdrive-*")
	if err != nil {
		return "", nil, fmt.Errorf("configdrive: create temp dir: %w", err)
	}

	// cleanupDir removes the entire temp directory, including the ISO inside it.
	cleanupDir := func() {
		_ = os.RemoveAll(dir)
	}

	isoPath := filepath.Join(dir, "configdrive.iso")

	d, err := diskfs.Create(isoPath, isoSize, diskfs.SectorSizeDefault)
	if err != nil {
		cleanupDir()
		return "", nil, fmt.Errorf("configdrive: diskfs.Create: %w", err)
	}
	// ISO 9660 mandates 2 KiB logical blocks.
	d.LogicalBlocksize = 2048

	spec := disk.FilesystemSpec{
		Partition:   0,
		FSType:      filesystem.TypeISO9660,
		VolumeLabel: volumeLabel,
	}
	fs, err := d.CreateFilesystem(spec)
	if err != nil {
		cleanupDir()
		return "", nil, fmt.Errorf("configdrive: CreateFilesystem: %w", err)
	}

	if err := writeFiles(fs, payload); err != nil {
		cleanupDir()
		return "", nil, err
	}

	iso, ok := fs.(*iso9660.FileSystem)
	if !ok {
		cleanupDir()
		return "", nil, fmt.Errorf("configdrive: unexpected filesystem type %T", fs)
	}
	finalizeOpts := iso9660.FinalizeOptions{
		RockRidge:        true,
		VolumeIdentifier: volumeLabel,
	}
	if err := iso.Finalize(finalizeOpts); err != nil {
		cleanupDir()
		return "", nil, fmt.Errorf("configdrive: Finalize: %w", err)
	}

	return isoPath, cleanupDir, nil
}

// minimalOpenStackMetaData is the smallest /openstack/latest/meta_data.json
// payload that satisfies BOSH's ConfigDrive datasource parser. The agent reads
// settings from user_data; meta_data.json only needs to be a valid JSON object.
const minimalOpenStackMetaData = `{"uuid":"00000000-0000-0000-0000-000000000000","name":"bosh-vm"}`

func writeFiles(fs filesystem.FileSystem, payload []byte) error {
	// OpenStack ConfigDrive layout (primary — used by openstack-kvm stemcells).
	if err := fs.Mkdir("/openstack"); err != nil {
		return fmt.Errorf("configdrive: mkdir /openstack: %w", err)
	}
	if err := fs.Mkdir("/openstack/latest"); err != nil {
		return fmt.Errorf("configdrive: mkdir /openstack/latest: %w", err)
	}
	if err := writeISOFile(fs, "/openstack/latest/user_data", payload); err != nil {
		return err
	}
	if err := writeISOFile(fs, "/openstack/latest/meta_data.json", []byte(minimalOpenStackMetaData)); err != nil {
		return err
	}

	// EC2 datasource layout (fallback — for stemcells configured against EC2).
	if err := fs.Mkdir("/ec2"); err != nil {
		return fmt.Errorf("configdrive: mkdir /ec2: %w", err)
	}
	if err := fs.Mkdir("/ec2/latest"); err != nil {
		return fmt.Errorf("configdrive: mkdir /ec2/latest: %w", err)
	}
	if err := writeISOFile(fs, "/ec2/latest/user-data", payload); err != nil {
		return err
	}
	// EC2 meta-data.json must carry the same minimal stub as
	// /openstack/latest/meta_data.json. Writing the full settings payload
	// here was a bug: the EC2 datasource reads user-data for agent settings,
	// not meta-data. Stemcells that parse meta-data.json as JSON would fail
	// if they received a settings blob instead of a metadata object.
	if err := writeISOFile(fs, "/ec2/latest/meta-data.json", []byte(minimalOpenStackMetaData)); err != nil {
		return err
	}
	return nil
}

func writeISOFile(fs filesystem.FileSystem, name string, data []byte) error {
	f, err := fs.OpenFile(name, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return fmt.Errorf("configdrive: open %s: %w", name, err)
	}
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("configdrive: write %s: %w", name, err)
	}
	return nil
}
