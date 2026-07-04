// fakes_tasks_test.go — fakeTasksService, shared by tracing_tasks_test.go's
// Tasks success+error matrix.
package pve

import (
	"context"

	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/tasks"
)

// fakeTasksService implements tasks.Service for tests. Only Wait and
// GetStatus are wired (the two methods tracedTasksService overrides);
// WaitForUPID panics if called, since no test here should reach it.
type fakeTasksService struct {
	waitFn      func(ctx context.Context, node, upid string, opts *tasks.WaitOptions) (*tasks.Status, error)
	getStatusFn func(ctx context.Context, node, upid string) (*tasks.Status, error)
}

func (f *fakeTasksService) Wait(ctx context.Context, node, upid string, opts *tasks.WaitOptions) (*tasks.Status, error) {
	if f.waitFn != nil {
		return f.waitFn(ctx, node, upid, opts)
	}
	panic("fakeTasksService: Wait not wired")
}

func (f *fakeTasksService) WaitForUPID(context.Context, string, *tasks.WaitOptions) (*tasks.Status, error) {
	panic("fakeTasksService: WaitForUPID unexpected call")
}

func (f *fakeTasksService) GetStatus(ctx context.Context, node, upid string) (*tasks.Status, error) {
	if f.getStatusFn != nil {
		return f.getStatusFn(ctx, node, upid)
	}
	panic("fakeTasksService: GetStatus not wired")
}
