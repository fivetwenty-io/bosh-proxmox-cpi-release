package handlers

const (
	storageRoleRoot           = "root"
	storageRoleEphemeral      = "ephemeral"
	storageRolePersistent     = "persistent"
	storageRoleISO            = "iso"
	managedVMCallCreate       = "QEMU.Create"
	managedVMCallClone        = "Nodes.CreateQemuClone"
	managedVMCallUpdateConfig = "Nodes.UpdateQemuConfig"
	managedVMCallAttach       = "QEMU.AttachDisk"
	managedVMCallDetach       = "QEMU.DetachDisk"
	managedVMCallStart        = "QEMU.Start"
	managedVMCallResize       = "Nodes.UpdateQemuResize"
	managedVMCallCreateVolume = "Storage.CreateVolume"
	managedVMCallUpload       = "Storage.Upload"
	managedVMStepCreate       = "vm.QEMU.Create"
	managedVMStepClone        = "vm.Nodes.CreateQemuClone"
	managedVMStepCreateVolume = "vm.Storage.CreateVolume"
	managedVMStepUpload       = "vm.Storage.Upload"
	storageSelectorPool       = "pool"
	storageSelectorTier       = "tier"
)
