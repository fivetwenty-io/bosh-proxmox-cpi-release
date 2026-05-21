package qemu

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/pve-apiclient-go/v3/internal/constants"
)

var diskKeyPattern = regexp.MustCompile(`^(scsi|virtio|ide|sata)(\d+)$`)

// ParseDisks returns a map of diskID -> volid from a VM config map.
func ParseDisks(cfg map[string]interface{}) map[string]string {
	out := make(map[string]string)

	for k, v := range cfg {
		m := diskKeyPattern.FindStringSubmatch(k)
		if len(m) == constants.ExpectedMatchCount {
			if s, ok := v.(string); ok && s != "" {
				out[k] = s
			}
		}
	}

	return out
}

// FindDiskIDByVolID returns the diskID for a given volid if present.
//
// PVE stores disk values in option-string format: the volid (e.g.
// "data:vm-9003-disk-0") is followed by comma-separated options
// (e.g. ",size=64G,iothread=1"). A plain string equality test misses
// these entries and leads callers to treat the disk as not attached,
// causing duplicate attachments. Comparison here matches both the bare
// volid and the option-string form by checking equality first, then
// the "<volid>," prefix.
func FindDiskIDByVolID(cfg map[string]interface{}, volid string) (string, bool) {
	for id, v := range ParseDisks(cfg) {
		if v == volid || strings.HasPrefix(v, volid+",") {
			return id, true
		}
	}

	return "", false
}

// NextIndexForBus returns the next free index for the given bus.
func NextIndexForBus(cfg map[string]interface{}, bus string) int {
	used := []int{}

	for k := range cfg {
		m := diskKeyPattern.FindStringSubmatch(k)
		if len(m) == constants.ExpectedMatchCount && m[1] == bus {
			idx, err := strconv.Atoi(m[2])
			if err == nil {
				used = append(used, idx)
			}
		}
	}

	sort.Ints(used)

	next := 0
	for _, v := range used {
		if v == next {
			next++
		} else if v > next {
			break
		}
	}

	return next
}

// GuessDevicePath returns a best-effort Linux device path for a diskID.
// virtioN -> vda, vdb..., scsiN/sataN/ideN -> sda, sdb... (approximation).
func GuessDevicePath(diskID string) string {
	matches := diskKeyPattern.FindStringSubmatch(diskID)
	if len(matches) != constants.ExpectedMatchCount {
		return ""
	}

	bus := matches[1]
	idx, _ := strconv.Atoi(matches[2])
	base := 'a' + idx

	switch bus {
	case "virtio":
		return fmt.Sprintf("/dev/vd%c", base)
	case "scsi", "sata", "ide":
		return fmt.Sprintf("/dev/sd%c", base)
	default:
		return ""
	}
}
