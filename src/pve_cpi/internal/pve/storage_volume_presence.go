package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ObserveStorageVolumePresence checks exact membership in a successful content listing.
// NFS returns a server error when a missing image has no format, so a direct
// image GET cannot distinguish absence from an unavailable backend.
func ObserveStorageVolumePresence(ctx context.Context, client Client, node, volume string) (bool, error) {
	found, _, err := observeStorageVolumeContent(ctx, client, node, volume, 0, nil)
	return found, err
}

// ObserveStorageVolumeContent reports exact membership and how many volumes the
// listing carried. The count is what separates a genuinely empty storage from a
// dir storage whose mount went away, which PVE lists as an empty array rather
// than an error unless is_mountpoint is set on it. On an error the count is
// zero and carries no meaning; only the error does.
func ObserveStorageVolumeContent(ctx context.Context, client Client, node, volume string) (bool, int, error) {
	return observeStorageVolumeContent(ctx, client, node, volume, 0, nil)
}

// StorageISOContentEvidence identifies the exact listed ISO file.
type StorageISOContentEvidence struct {
	Size  uint64
	CTime int64
}

// ObserveStorageISOContent proves exact ISO content membership and file size.
// PVE image detail reports ISO files as raw; the content listing identifies ISOs.
func ObserveStorageISOContent(ctx context.Context, client Client, node, volume string, size uint64) (*StorageISOContentEvidence, error) {
	_, bare, err := ParseDiskCID(volume)
	if err != nil || size == 0 || !strings.HasPrefix(bare, "iso/") || !strings.HasSuffix(bare, ".iso") || strings.Contains(strings.TrimPrefix(bare, "iso/"), "/") {
		return nil, fmt.Errorf("ISO content observation requires exact ISO identity and size")
	}
	evidence := &StorageISOContentEvidence{}
	found, _, err := observeStorageVolumeContent(ctx, client, node, volume, size, evidence)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("ISO artifact absent from content inventory")
	}
	return evidence, nil
}

func observeStorageVolumeContent(
	ctx context.Context, client Client, node, volume string, isoBytes uint64, isoEvidence *StorageISOContentEvidence,
) (bool, int, error) {
	if ctx == nil || client == nil || node == "" {
		return false, 0, fmt.Errorf("storage volume observation requires context, client, and node")
	}
	storage, _, err := ParseDiskCID(volume)
	if err != nil {
		return false, 0, err
	}
	listing, err := client.Nodes().ListStorageContent(ctx, node, storage, nil)
	if err != nil {
		return false, 0, storageContentFailure(err)
	}
	if listing == nil || *listing == nil {
		return false, 0, &storageContentObservationError{reason: "listing_data_missing"}
	}
	listed := len(*listing)
	found := false
	seen := make(map[string]bool, len(*listing))
	for _, raw := range *listing {
		var item struct {
			Volid string `json:"volid"`
		}
		if err := json.Unmarshal(raw, &item); err != nil || item.Volid == "" || seen[item.Volid] {
			return false, 0, fmt.Errorf("managed volume content listing malformed")
		}
		itemStorage, _, err := ParseDiskCID(item.Volid)
		if err != nil || itemStorage != storage {
			return false, 0, fmt.Errorf("managed volume content listing target mismatch")
		}
		seen[item.Volid] = true
		if item.Volid == volume {
			if isoBytes > 0 {
				var iso struct {
					Content string `json:"content"`
					Format  string `json:"format"`
					Size    uint64 `json:"size"`
					CTime   int64  `json:"ctime"`
				}
				if json.Unmarshal(raw, &iso) != nil || iso.Content != "iso" || iso.Format != "iso" || iso.Size != isoBytes || iso.CTime <= 0 {
					return false, 0, fmt.Errorf("ISO content identity, format, or exact size differs")
				}
				*isoEvidence = StorageISOContentEvidence{Size: iso.Size, CTime: iso.CTime}
			}
			found = true
		}
	}
	if !found {
		visibility, ok := client.(StorageAuditVisibilityReader)
		if !ok {
			return false, 0, fmt.Errorf("managed volume content visibility proof unavailable")
		}
		if err := visibility.StorageAuditVisibility(ctx); err != nil {
			return false, 0, fmt.Errorf("managed volume content visibility unproven")
		}
	}
	return found, listed, nil
}
