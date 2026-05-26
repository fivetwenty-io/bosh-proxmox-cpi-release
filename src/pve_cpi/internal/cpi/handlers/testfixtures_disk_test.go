// Package handlers_test — shared storage-type fixture for disk handler tests.
// diskStorageFixtures is the single source of truth for representative CIDs
// across each storage backend kind. Handler tests may inline their own CIDs
// for clarity; this table exists as a reference and for table-driven tests.
package handlers_test

// diskStorageFixture describes one storage backend type used in disk tests.
type diskStorageFixture struct {
	typeName    string // PVE storage type identifier
	cid         string // representative content-ID (storage:volid)
	blockDevice bool   // true = block device (lvm/zfs), false = file (dir/nfs/cephfs/cifs)
	shared      bool   // true = accessible from multiple nodes without co-location constraint
	active      bool   // true = covered by unit tests; false = pending integration harness
}

// diskStorageFixtures lists active local storage types with representative CIDs.
// The CID format follows PVE conventions:
//   - block types: <storage>:<volname>   (no subpath)
//   - file types:  <storage>:<vmid>/<volname>.<ext>
//
// Commented-out entries are network/shared types that require a live storage
// pool in the integration harness. Each carries a TODO(storage-network) tag
// so they can be found and re-enabled as infrastructure matures.
var diskStorageFixtures = []diskStorageFixture{
	{
		typeName:    "lvm",
		cid:         "local-lvm:vm-9001-disk-0",
		blockDevice: true,
		shared:      false,
		active:      true,
	},
	{
		typeName:    "lvmthin",
		cid:         "local-lvm-thin:vm-9001-disk-0",
		blockDevice: true,
		shared:      false,
		active:      true,
	},
	{
		typeName:    "zfspool",
		cid:         "local-zfs:vm-9001-disk-0",
		blockDevice: true,
		shared:      false,
		active:      true,
	},
	{
		typeName:    "dir",
		cid:         "local:9001/vm-9001-disk-0.raw",
		blockDevice: false,
		shared:      false,
		active:      true,
	},

	// TODO(storage-network): nfs — wired to PVE network-call boundary, stubbed pending
	// live shared-storage test infrastructure. Storage: nfs-store:9001/vm-9001-disk-0.qcow2.
	// Re-enable when integration-test harness provides a nfs pool via env.
	//
	// {
	// 	typeName:    "nfs",
	// 	cid:         "nfs-store:9001/vm-9001-disk-0.qcow2",
	// 	blockDevice: false,
	// 	shared:      true,
	// 	active:      false,
	// },

	// TODO(storage-network): rbd — wired to PVE network-call boundary, stubbed pending
	// live shared-storage test infrastructure. Storage: ceph-pool:vm-9001-disk-0.
	// Re-enable when integration-test harness provides a rbd pool via env.
	//
	// {
	// 	typeName:    "rbd",
	// 	cid:         "ceph-pool:vm-9001-disk-0",
	// 	blockDevice: true,
	// 	shared:      true,
	// 	active:      false,
	// },

	// TODO(storage-network): cephfs — wired to PVE network-call boundary, stubbed pending
	// live shared-storage test infrastructure. Storage: cephfs-pool:vm-9001-disk-0.
	// Re-enable when integration-test harness provides a cephfs pool via env.
	//
	// {
	// 	typeName:    "cephfs",
	// 	cid:         "cephfs-pool:vm-9001-disk-0",
	// 	blockDevice: false,
	// 	shared:      true,
	// 	active:      false,
	// },

	// TODO(storage-network): cifs — wired to PVE network-call boundary, stubbed pending
	// live shared-storage test infrastructure. Storage: cifs-store:9001/vm-9001-disk-0.qcow2.
	// Re-enable when integration-test harness provides a cifs pool via env.
	//
	// {
	// 	typeName:    "cifs",
	// 	cid:         "cifs-store:9001/vm-9001-disk-0.qcow2",
	// 	blockDevice: false,
	// 	shared:      true,
	// 	active:      false,
	// },
}
