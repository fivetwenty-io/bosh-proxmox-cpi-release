package agent

import (
	"context"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
)

type managedISOStorage struct {
	storage.Service
	exists bool
	err    error
	calls  int
}

func (s *managedISOStorage) Exists(context.Context, string, string, string) (bool, error) {
	s.calls++
	return s.exists, s.err
}

type managedISOClient struct {
	pve.Client
	s storage.Service
}

func (c managedISOClient) Storage() storage.Service { return c.s }
