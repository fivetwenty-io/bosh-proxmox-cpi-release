package handlers

import "testing"

func TestSourceForPrefersSameStorageTemplate(t *testing.T) {
	t.Parallel()
	req, _, _ := planFixture(t, nil)
	req.Sources = []StorageRootSource{
		{Node: "n1", StorageID: "source", VolumeID: "source:import/stemcell.qcow2", VirtualBytes: 5 << 30},
		{Node: "n1", StorageID: "a", VolumeID: "a:30010/base-30010-disk-0.qcow2", TemplateVMID: 30010, VirtualBytes: 5 << 30},
		{Node: "n1", StorageID: "b", VolumeID: "b:30005/base-30005-disk-0.qcow2", TemplateVMID: 30005, VirtualBytes: 5 << 30},
	}
	it, err := NewStoragePlanIterator(req)
	if err != nil {
		t.Fatal(err)
	}
	src, mech, err := it.sourceFor("n1", "b")
	if err != nil {
		t.Fatal(err)
	}
	if src.TemplateVMID != 30005 || mech != "linked_clone" {
		t.Fatalf("target b: want template 30005 linked_clone, got %d %s", src.TemplateVMID, mech)
	}
	src, mech, err = it.sourceFor("n1", "a")
	if err != nil {
		t.Fatal(err)
	}
	if src.TemplateVMID != 30010 || mech != "linked_clone" {
		t.Fatalf("target a: want template 30010 linked_clone, got %d %s", src.TemplateVMID, mech)
	}
}

func TestSourceForFallsBackToHighestVMIDFullClone(t *testing.T) {
	t.Parallel()
	req, _, _ := planFixture(t, nil)
	req.Sources = []StorageRootSource{
		{Node: "n1", StorageID: "a", VolumeID: "a:30010/base-30010-disk-0.qcow2", TemplateVMID: 30010, VirtualBytes: 5 << 30},
		{Node: "n1", StorageID: "source", VolumeID: "source:30020/base-30020-disk-0.qcow2", TemplateVMID: 30020, VirtualBytes: 5 << 30},
	}
	it, err := NewStoragePlanIterator(req)
	if err != nil {
		t.Fatal(err)
	}
	src, mech, err := it.sourceFor("n1", "b")
	if err != nil {
		t.Fatal(err)
	}
	if src.TemplateVMID != 30020 || mech != "full_clone" {
		t.Fatalf("want highest-VMID template 30020 full_clone, got %d %s", src.TemplateVMID, mech)
	}
}

func TestSourceForTemplateBeatsImportOnStemcellPool(t *testing.T) {
	t.Parallel()
	req, _, _ := planFixture(t, nil)
	req.Sources = []StorageRootSource{
		{Node: "n1", StorageID: "b", VolumeID: "b:import/stemcell.qcow2", VirtualBytes: 5 << 30},
		{Node: "n1", StorageID: "a", VolumeID: "a:30010/base-30010-disk-0.qcow2", TemplateVMID: 30010, VirtualBytes: 5 << 30},
	}
	it, err := NewStoragePlanIterator(req)
	if err != nil {
		t.Fatal(err)
	}
	src, mech, err := it.sourceFor("n1", "b")
	if err != nil {
		t.Fatal(err)
	}
	if src.TemplateVMID != 30010 || mech != "full_clone" {
		t.Fatalf("a template elsewhere must beat the import candidate on the target itself, got %d %s", src.TemplateVMID, mech)
	}
}

// TestSourceForSameStorageTieBreaksByVMIDDescending passes before the sort
// change too; it pins the tiebreak so a later edit cannot drop it.
func TestSourceForSameStorageTieBreaksByVMIDDescending(t *testing.T) {
	t.Parallel()
	req, _, _ := planFixture(t, nil)
	req.Sources = []StorageRootSource{
		{Node: "n1", StorageID: "b", VolumeID: "b:30005/base-30005-disk-0.qcow2", TemplateVMID: 30005, VirtualBytes: 5 << 30},
		{Node: "n1", StorageID: "b", VolumeID: "b:30007/base-30007-disk-0.qcow2", TemplateVMID: 30007, VirtualBytes: 5 << 30},
	}
	it, err := NewStoragePlanIterator(req)
	if err != nil {
		t.Fatal(err)
	}
	src, _, err := it.sourceFor("n1", "b")
	if err != nil || src.TemplateVMID != 30007 {
		t.Fatalf("want 30007, got %+v err=%v", src, err)
	}
}
