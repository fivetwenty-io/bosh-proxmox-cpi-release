package cloudinit

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/client"
)

const dhcpIPConfig = "ip=dhcp"

// Service defines cloud-init helpers.
type Service interface {
	BuildIPConfig(networks map[string]any) (map[string]string, error)
	// BuildIPConfigs builds ipconfigN for multiple NICs and optional global nameserver.
	BuildIPConfigs(specs []NICSpec, globalNameservers []string) (map[string]string, error)
	// BuildIPConfigsFromCPISpec builds ipconfig map from a tolerant CPI-like network spec.
	// Expected shape (tolerant): {
	//   "interfaces": [ {"dhcp":true}, {"address":"10.0.0.10/24","gateway":"10.0.0.1"} ],
	//   "nameservers": ["8.8.8.8","1.1.1.1"]
	// }
	BuildIPConfigsFromCPISpec(spec map[string]any) (map[string]string, error)
	Attach(ctx context.Context, node string, vmid int, storage string, userData []byte) error
	// AttachWithNetwork uploads user-data and network-data snippets (if provided) and sets cicustom accordingly.
	AttachWithNetwork(ctx context.Context, node string, vmid int, storage string, userData, networkData []byte) error
}

type service struct{ c client.Client }

// New returns a new cloud-init service.
//
//nolint:ireturn // Factory pattern - returns interface to encapsulate implementation and enable mocking
func New(c client.Client) Service { return &service{c: c} }

func (s *service) BuildIPConfig(networks map[string]any) (map[string]string, error) {
	// Minimal builder: expects keys "ip", "gw", "nameserver" directly
	if networks == nil {
		return map[string]string{}, nil
	}

	ipAddress, _ := networks["ip"].(string)
	gateway, _ := networks["gw"].(string)
	nameserver, _ := networks["nameserver"].(string)

	res := map[string]string{}
	if ipAddress != "" {
		res["ipconfig0"] = "ip=" + ipAddress
		if gateway != "" {
			res["ipconfig0"] += ",gw=" + gateway
		}
	}

	if nameserver != "" {
		res["nameserver"] = nameserver
	}

	return res, nil
}

// NICSpec represents one network interface cloud-init config.
type NICSpec struct {
	// If DHCP is true, ipconfig will be set to dhcp; AddressCIDR and Gateway are ignored.
	DHCP bool
	// AddressCIDR like "192.168.1.10/24" when DHCP is false.
	AddressCIDR string
	// Gateway for this NIC when DHCP is false.
	Gateway string
}

// BuildIPConfigs builds ipconfigN for multiple NICs and an optional global nameserver list.
func (s *service) BuildIPConfigs(specs []NICSpec, globalNameservers []string) (map[string]string, error) {
	result := make(map[string]string)

	for i, nic := range specs {
		key := fmt.Sprintf("ipconfig%d", i)
		if nic.DHCP {
			result[key] = dhcpIPConfig
		} else if nic.AddressCIDR != "" {
			result[key] = "ip=" + nic.AddressCIDR
			if nic.Gateway != "" {
				result[key] += ",gw=" + nic.Gateway
			}
		}
	}

	if len(globalNameservers) > 0 {
		// Proxmox expects nameserver as a space-separated string
		nameservers := ""

		var nameserversSb89 strings.Builder

		for i, server := range globalNameservers {
			if i > 0 {
				nameserversSb89.WriteString(" ")
			}

			nameserversSb89.WriteString(server)
		}

		nameservers += nameserversSb89.String()

		result["nameserver"] = nameservers
	}

	return result, nil
}

// BuildIPConfigsFromCPISpec parses a tolerant CPI-like spec into ipconfig map.
func (s *service) BuildIPConfigsFromCPISpec(spec map[string]any) (map[string]string, error) {
	if spec == nil {
		return map[string]string{}, nil
	}
	// interfaces
	nicSpecs := parseInterfaces(spec)
	// nameservers
	var names []string

	if nsa, ok := spec["nameservers"].([]any); ok {
		for _, v := range nsa {
			if s, ok := v.(string); ok {
				names = append(names, s)
			}
		}
	}

	return s.BuildIPConfigs(nicSpecs, names)
}

func (s *service) Attach(ctx context.Context, node string, vmid int, storage string, userData []byte) error {
	// Always set ide2 to cloudinit
	params := map[string]interface{}{
		"ide2": storage + ":cloudinit",
	}

	_, err := s.c.PutCtx(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/config", url.PathEscape(node), vmid), params)
	if err != nil {
		return fmt.Errorf("failed to set cloud-init IDE2 config: %w", err)
	}

	if len(userData) > 0 {
		filename := fmt.Sprintf("user-data-vm-%d.yaml", vmid)

		// PVE returns HTTP 400 when "filename" is sent both as a form field
		// and as the multipart part name; pass it only as the part name.
		fields := map[string]string{
			"content": "snippets",
		}

		_, err := s.c.UploadCtx(ctx, fmt.Sprintf("/nodes/%s/storage/%s/upload", url.PathEscape(node), url.PathEscape(storage)), fields, "filename", filename, bytes.NewReader(userData))
		if err != nil {
			return fmt.Errorf("failed to upload user-data file: %w", err)
		}

		_, err = s.c.PutCtx(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/config", url.PathEscape(node), vmid), map[string]interface{}{
			"cicustom": fmt.Sprintf("user=%s:snippets/%s", storage, filename),
		})
		if err != nil {
			return fmt.Errorf("failed to set cicustom user config: %w", err)
		}

		return nil
	}

	return nil
}

// AttachWithNetwork is like Attach but also uploads network-data if provided and sets cicustom for network.
func (s *service) AttachWithNetwork(ctx context.Context, node string, vmid int, storage string, userData, networkData []byte) error {
	err := s.Attach(ctx, node, vmid, storage, userData)
	if err != nil {
		return err
	}

	if len(networkData) == 0 {
		return nil
	}

	netFilename := fmt.Sprintf("network-data-vm-%d.yaml", vmid)

	// PVE returns HTTP 400 when "filename" is sent both as a form field
	// and as the multipart part name; pass it only as the part name.
	fields := map[string]string{
		"content": "snippets",
	}

	_, err = s.c.UploadCtx(ctx, fmt.Sprintf("/nodes/%s/storage/%s/upload", url.PathEscape(node), url.PathEscape(storage)), fields, "filename", netFilename, bytes.NewReader(networkData))
	if err != nil {
		return fmt.Errorf("failed to upload network-data file: %w", err)
	}
	// Update cicustom to include network= as well; preserve existing user= from previous call
	_, err = s.c.PutCtx(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/config", url.PathEscape(node), vmid), map[string]interface{}{
		"cicustom": fmt.Sprintf("user=%s:snippets/%s,network=%s:snippets/%s", storage, fmt.Sprintf("user-data-vm-%d.yaml", vmid), storage, netFilename),
	})
	if err != nil {
		return fmt.Errorf("failed to set cicustom network config: %w", err)
	}

	return nil
}

func parseInterfaces(spec map[string]any) []NICSpec {
	ifaces, ok := spec["interfaces"].([]any)
	if !ok {
		return nil
	}

	nicSpecs := make([]NICSpec, 0, len(ifaces))

	for _, iface := range ifaces {
		ifaceMap, ok := iface.(map[string]any)
		if !ok {
			continue
		}

		nic := NICSpec{}
		if dhcp, ok := ifaceMap["dhcp"].(bool); ok && dhcp {
			nic.DHCP = true
		}

		if addr, ok := ifaceMap["address"].(string); ok {
			nic.AddressCIDR = addr
		}

		if gw, ok := ifaceMap["gateway"].(string); ok {
			nic.Gateway = gw
		}

		nicSpecs = append(nicSpecs, nic)
	}

	return nicSpecs
}
