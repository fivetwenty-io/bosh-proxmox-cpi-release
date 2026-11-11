package pve

import (
	"encoding/json"
	"strings"
	"testing"

	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// The SDK escapes the text of an API error and caps each text field at 1 KiB
// of escaped text. These tests send the replies the CPI's text classifiers are
// written for through the SDK's own parser, with the trailing newline Perl's
// die adds, with control characters around the phrase, and with a long tail
// of command output after it, and check that every classifier still matches.

// apiReply builds the error the SDK returns for a reply with status, message,
// and field errors.
func apiReply(t *testing.T, status int, message string, fields map[string]string) error {
	t.Helper()
	body, err := json.Marshal(map[string]any{"data": nil, "message": message, "errors": fields})
	if err != nil {
		t.Fatal(err)
	}
	return sdkerrors.ParseAPIError(status, body)
}

func TestTextClassifiersMatchTheSDKsEscapedAndCappedErrors(t *testing.T) {
	tail := "\n" + strings.Repeat("output line from the storage tool\n", 120)
	cases := []struct {
		name     string
		status   int
		message  string
		fields   map[string]string
		classify func(error) bool
	}{
		{"a VMID conflict", 500, "unable to create VM 4242 - VM 4242 already exists on node 'pve1'\n", nil,
			IsVMIDConflict},
		{"a missing guest config", 500, "Configuration file 'nodes/pve1/qemu-server/101.conf' does not exist\n",
			nil, IsPmxcfsConfigMissing},
		{"a storage lock timeout", 500, "can't lock file '/var/lock/pve-manager/pve-storage-data' - got timeout\n",
			nil, IsStorageLockTimeout},
		{"a guest config lock", 500, "VM 101 is locked (backup)\n", nil, IsVMConfigLocked},
		{"a lost quorum", 500, "cluster not ready - no quorum?\n", nil, IsClusterNotQuorate},
		{"a missing pool", 500, "pool 'p1' does not exist\n", nil, IsPoolNotFound},
		{"a pool member", 500, "VM 101 is already a pool member\n", nil, IsVMAlreadyPoolMember},
		{"a missing volume", 500, "lvs: no such logical volume data/vm-101-disk-1\n", nil, IsStorageVolumeMissing},
		{"a busy hot unplug", 400, "Parameter verification failed.", map[string]string{
			"scsi1": "hotplug problem - error on hot-unplugging device 'virtioscsi1' - still busy in guest?\n",
		}, IsHotUnplugBusy},
		{"a pushback", 503, "too many requests - worker busy\n", nil, IsPVEPushback},
	}
	shapes := []struct {
		name string
		wrap func(string) string
	}{
		{"as sent", func(s string) string { return s }},
		{"with control characters", func(s string) string { return "\x1b[31m\r" + s + "\x1b[0m\x00" }},
		{"with a long tail", func(s string) string { return strings.TrimSuffix(s, "\n") + tail }},
	}
	for _, tc := range cases {
		for _, shape := range shapes {
			t.Run(tc.name+", "+shape.name, func(t *testing.T) {
				fields := make(map[string]string, len(tc.fields))
				for key, value := range tc.fields {
					fields[key] = shape.wrap(value)
				}
				message := tc.message
				if len(fields) == 0 {
					message = shape.wrap(message)
				}
				err := apiReply(t, tc.status, message, fields)
				if !tc.classify(err) {
					t.Errorf("the classifier missed %q", err.Error())
				}
				if text := err.Error(); strings.ContainsAny(text, "\x1b\r\x00") {
					t.Errorf("the SDK left a control character in %q", text)
				}
			})
		}
	}
}

// The cap is a change the bump brings: a phrase that starts past the first
// 1 KiB of a message is cut away, and the message ends with the SDK's marker.
// PVE puts the phrases the classifiers read at the start of its messages, so
// this pins the limit rather than a case the CPI meets.
func TestAPhrasePastTheSDKsCapIsCut(t *testing.T) {
	err := apiReply(t, 500, strings.Repeat("x", sdkerrors.MaxServerTextBytes)+" VM 101 is locked (backup)", nil)
	if IsVMConfigLocked(err) {
		t.Errorf("the classifier matched a phrase past the cap in %q", err.Error())
	}
	if !strings.Contains(err.Error(), "...(truncated)") {
		t.Errorf("the message %q does not end with the SDK's marker", err.Error())
	}
}
