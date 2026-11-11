package pve

import (
	"context"
	"strings"
	"testing"

	sdktasks "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// settledTaskClient answers every task status read with status and err, and
// records the node each read went to.
type settledTaskClient struct {
	*mmClient
	status *sdktasks.Status
	err    error
	reads  []string
}

func (c *settledTaskClient) Tasks() sdktasks.Service { return &settledTasks{c: c} }

type settledTasks struct {
	sdktasks.Service
	c *settledTaskClient
}

func (t *settledTasks) GetStatus(_ context.Context, node, _ string) (*sdktasks.Status, error) {
	t.c.reads = append(t.c.reads, node)
	return t.c.status, t.c.err
}

const settledUPID = "UPID:pve1:0000A1B2:0001C3D4:6734ABCD:qmdestroy:4130:root@pam:"

// TestConfirmGuestTaskOK_AcceptsOnlyACleanExitOfTheNamedTask accepts the
// named task stopped with exit status OK, and refuses a UPID for another node,
// type, or VM, a malformed UPID, a failed read, a read of another task, a task
// still running, and one that ended with warnings or an error.
func TestConfirmGuestTaskOK_AcceptsOnlyACleanExitOfTheNamedTask(t *testing.T) {
	t.Parallel()
	ok := &sdktasks.Status{Status: "stopped", ExitStatus: "OK", UpID: settledUPID}
	for _, tc := range []struct {
		name   string
		upid   string
		status *sdktasks.Status
		err    error
		want   string
		reads  int
	}{
		{"clean exit", settledUPID, ok, nil, "", 1},
		{"another node", strings.Replace(settledUPID, ":pve1:", ":pve2:", 1), ok, nil, "is not a qmdestroy task", 0},
		{"another type", strings.Replace(settledUPID, "qmdestroy", "qmigrate", 1), ok, nil, "is not a qmdestroy task", 0},
		{"another VM", strings.Replace(settledUPID, ":4130:", ":4131:", 1), ok, nil, "is not a qmdestroy task", 0},
		{"malformed", "UPID:pve1:destroy", ok, nil, "is not a UPID", 0},
		{"read fails", settledUPID, nil, &sdkerrors.ConnectionError{Host: "pve1", Port: 8006, Message: "reset"},
			"read task", 1},
		{"no status", settledUPID, nil, nil, "read back as another task", 1},
		{"another task", settledUPID, &sdktasks.Status{Status: "stopped", ExitStatus: "OK", UpID: "UPID:x"}, nil,
			"read back as another task", 1},
		{"running", settledUPID, &sdktasks.Status{Status: "running", UpID: settledUPID}, nil, "reports status", 1},
		{"warnings", settledUPID, &sdktasks.Status{Status: "stopped", ExitStatus: "WARNINGS: 1", UpID: settledUPID,
			Warned: true}, nil, "reports status", 1},
		{"warned flag", settledUPID, &sdktasks.Status{Status: "stopped", ExitStatus: "OK", UpID: settledUPID,
			Warned: true}, nil, "reports status", 1},
		{"error exit", settledUPID, &sdktasks.Status{Status: "stopped", ExitStatus: "command failed",
			UpID: settledUPID}, nil, "reports status", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := &settledTaskClient{mmClient: newMMClient(), status: tc.status, err: tc.err}
			err := ConfirmGuestTaskOK(context.Background(), c, "pve1", tc.upid, "qmdestroy", 4130)
			if tc.want == "" && err != nil {
				t.Fatalf("ConfirmGuestTaskOK = %v, want nil", err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("ConfirmGuestTaskOK = %v, want an error containing %q", err, tc.want)
			}
			if len(c.reads) != tc.reads || (tc.reads == 1 && c.reads[0] != "pve1") {
				t.Fatalf("status reads = %v, want %d on pve1", c.reads, tc.reads)
			}
		})
	}
}
