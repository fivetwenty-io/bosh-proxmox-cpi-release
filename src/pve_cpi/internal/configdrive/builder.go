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
// removes it. The caller must invoke cleanup once the ISO has been uploaded.
// On any error the partially-written file is removed before returning.
func Build(payload []byte) (path string, cleanup func(), err error) {
	tmp, err := os.CreateTemp("", "bosh-cpi-configdrive-*.iso")
	if err != nil {
		return "", nil, fmt.Errorf("configdrive: create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	// diskfs.Create opens the file with O_EXCL via the underlying backend, so
	// remove the empty placeholder created by os.CreateTemp first. The unique
	// name from CreateTemp guarantees no collision with other goroutines.
	_ = os.Remove(tmpPath)

	removeOnError := func() {
		_ = os.Remove(tmpPath)
	}

	d, err := diskfs.Create(tmpPath, isoSize, diskfs.SectorSizeDefault)
	if err != nil {
		removeOnError()
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
		removeOnError()
		return "", nil, fmt.Errorf("configdrive: CreateFilesystem: %w", err)
	}

	if err := writeFiles(fs, payload); err != nil {
		removeOnError()
		return "", nil, err
	}

	iso, ok := fs.(*iso9660.FileSystem)
	if !ok {
		removeOnError()
		return "", nil, fmt.Errorf("configdrive: unexpected filesystem type %T", fs)
	}
	finalizeOpts := iso9660.FinalizeOptions{
		RockRidge:        true,
		VolumeIdentifier: volumeLabel,
	}
	if err := iso.Finalize(finalizeOpts); err != nil {
		removeOnError()
		return "", nil, fmt.Errorf("configdrive: Finalize: %w", err)
	}

	cleanup = func() { _ = os.Remove(tmpPath) }
	return tmpPath, cleanup, nil
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
	if err := writeISOFile(fs, "/ec2/latest/meta-data.json", payload); err != nil {
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
