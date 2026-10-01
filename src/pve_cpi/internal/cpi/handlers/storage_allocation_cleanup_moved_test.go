package handlers

import (
	"context"
	"strconv"
	"strings"

	nodesapi "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// volumeDestroyingPVE destroys a guest together with the volumes its
// configuration references, the way qm destroy does. The flow fake's
// DeleteQemu refuses a guest that still holds a volume, because it models
// only the retention parker, so an explicit cleanup cannot finish on it
// without this.
type volumeDestroyingPVE struct{ *lifecycleFlowPVE }

func (c volumeDestroyingPVE) Nodes() nodesapi.Service {
	return volumeDestroyingNodes{lifecycleFlowNodes: c.lifecycleFlowPVE.Nodes().(lifecycleFlowNodes)}
}

type volumeDestroyingNodes struct{ lifecycleFlowNodes }

func (n volumeDestroyingNodes) DeleteQemu(ctx context.Context, node, vmidText string, p *nodesapi.DeleteQemuParams) (*nodesapi.DeleteQemuResponse, error) {
	vmid, err := strconv.Atoi(vmidText)
	if err != nil {
		return nil, err
	}
	if n.c.vmNode(vmid) == node {
		config := n.c.state.configs[vmid]
		for key, value := range config {
			if !isDiskOptionKey(key) {
				continue
			}
			text, _ := value.(string)
			delete(n.c.state.volumes, strings.Split(text, ",")[0])
			delete(config, key)
		}
	}
	return n.lifecycleFlowNodes.DeleteQemu(ctx, node, vmidText, p)
}
