// Package handlers_test — generated panic stub for nodes.Service.
// This file is generated; do not edit manually.
package handlers_test

import (
	"context"

	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
)

// panicNodesStub satisfies nodes.Service; every method panics on call.
// Compose with mockNodesService to override specific methods in tests.
type panicNodesStub struct{}

// Compile-time interface satisfaction check.
var _ nodes.Service = (*panicNodesStub)(nil)

func (s *panicNodesStub) ListNodes(ctx context.Context) (*nodes.ListNodesResponse, error) {
	panic("panicNodesStub.ListNodes: not expected")
}

func (s *panicNodesStub) GetNodes(ctx context.Context, node string) (*nodes.GetNodesResponse, error) {
	panic("panicNodesStub.GetNodes: not expected")
}

func (s *panicNodesStub) ListAplinfo(ctx context.Context, node string) (*nodes.ListAplinfoResponse, error) {
	panic("panicNodesStub.ListAplinfo: not expected")
}

func (s *panicNodesStub) CreateAplinfo(ctx context.Context, node string, params *nodes.CreateAplinfoParams) (*nodes.CreateAplinfoResponse, error) {
	panic("panicNodesStub.CreateAplinfo: not expected")
}

func (s *panicNodesStub) ListApt(ctx context.Context, node string) (*nodes.ListAptResponse, error) {
	panic("panicNodesStub.ListApt: not expected")
}

func (s *panicNodesStub) ListAptChangelog(ctx context.Context, node string, params *nodes.ListAptChangelogParams) (*nodes.ListAptChangelogResponse, error) {
	panic("panicNodesStub.ListAptChangelog: not expected")
}

func (s *panicNodesStub) ListAptRepositories(ctx context.Context, node string) (*nodes.ListAptRepositoriesResponse, error) {
	panic("panicNodesStub.ListAptRepositories: not expected")
}

func (s *panicNodesStub) CreateAptRepositories(ctx context.Context, node string, params *nodes.CreateAptRepositoriesParams) error {
	panic("panicNodesStub.CreateAptRepositories: not expected")
}

func (s *panicNodesStub) UpdateAptRepositories(ctx context.Context, node string, params *nodes.UpdateAptRepositoriesParams) error {
	panic("panicNodesStub.UpdateAptRepositories: not expected")
}

func (s *panicNodesStub) ListAptUpdate(ctx context.Context, node string) (*nodes.ListAptUpdateResponse, error) {
	panic("panicNodesStub.ListAptUpdate: not expected")
}

func (s *panicNodesStub) CreateAptUpdate(ctx context.Context, node string, params *nodes.CreateAptUpdateParams) (*nodes.CreateAptUpdateResponse, error) {
	panic("panicNodesStub.CreateAptUpdate: not expected")
}

func (s *panicNodesStub) ListAptVersions(ctx context.Context, node string) (*nodes.ListAptVersionsResponse, error) {
	panic("panicNodesStub.ListAptVersions: not expected")
}

func (s *panicNodesStub) ListCapabilities(ctx context.Context, node string) (*nodes.ListCapabilitiesResponse, error) {
	panic("panicNodesStub.ListCapabilities: not expected")
}

func (s *panicNodesStub) ListCapabilitiesQemu(ctx context.Context, node string) (*nodes.ListCapabilitiesQemuResponse, error) {
	panic("panicNodesStub.ListCapabilitiesQemu: not expected")
}

func (s *panicNodesStub) ListCapabilitiesQemuCpu(ctx context.Context, node string, params *nodes.ListCapabilitiesQemuCpuParams) (*nodes.ListCapabilitiesQemuCpuResponse, error) {
	panic("panicNodesStub.ListCapabilitiesQemuCpu: not expected")
}

func (s *panicNodesStub) ListCapabilitiesQemuCpuFlags(ctx context.Context, node string, params *nodes.ListCapabilitiesQemuCpuFlagsParams) (*nodes.ListCapabilitiesQemuCpuFlagsResponse, error) {
	panic("panicNodesStub.ListCapabilitiesQemuCpuFlags: not expected")
}

func (s *panicNodesStub) ListCapabilitiesQemuMachines(ctx context.Context, node string, params *nodes.ListCapabilitiesQemuMachinesParams) (*nodes.ListCapabilitiesQemuMachinesResponse, error) {
	panic("panicNodesStub.ListCapabilitiesQemuMachines: not expected")
}

func (s *panicNodesStub) ListCapabilitiesQemuMigration(ctx context.Context, node string) (*nodes.ListCapabilitiesQemuMigrationResponse, error) {
	panic("panicNodesStub.ListCapabilitiesQemuMigration: not expected")
}

func (s *panicNodesStub) ListCeph(ctx context.Context, node string) (*nodes.ListCephResponse, error) {
	panic("panicNodesStub.ListCeph: not expected")
}

func (s *panicNodesStub) ListCephCfg(ctx context.Context, node string) (*nodes.ListCephCfgResponse, error) {
	panic("panicNodesStub.ListCephCfg: not expected")
}

func (s *panicNodesStub) ListCephCfgDb(ctx context.Context, node string) (*nodes.ListCephCfgDbResponse, error) {
	panic("panicNodesStub.ListCephCfgDb: not expected")
}

func (s *panicNodesStub) ListCephCfgRaw(ctx context.Context, node string) (*nodes.ListCephCfgRawResponse, error) {
	panic("panicNodesStub.ListCephCfgRaw: not expected")
}

func (s *panicNodesStub) ListCephCfgValue(ctx context.Context, node string, params *nodes.ListCephCfgValueParams) (*nodes.ListCephCfgValueResponse, error) {
	panic("panicNodesStub.ListCephCfgValue: not expected")
}

func (s *panicNodesStub) ListCephCmdSafety(ctx context.Context, node string, params *nodes.ListCephCmdSafetyParams) (*nodes.ListCephCmdSafetyResponse, error) {
	panic("panicNodesStub.ListCephCmdSafety: not expected")
}

func (s *panicNodesStub) ListCephCrush(ctx context.Context, node string) (*nodes.ListCephCrushResponse, error) {
	panic("panicNodesStub.ListCephCrush: not expected")
}

func (s *panicNodesStub) ListCephFs(ctx context.Context, node string) (*nodes.ListCephFsResponse, error) {
	panic("panicNodesStub.ListCephFs: not expected")
}

func (s *panicNodesStub) CreateCephFs(ctx context.Context, node string, name string, params *nodes.CreateCephFsParams) (*nodes.CreateCephFsResponse, error) {
	panic("panicNodesStub.CreateCephFs: not expected")
}

func (s *panicNodesStub) CreateCephInit(ctx context.Context, node string, params *nodes.CreateCephInitParams) error {
	panic("panicNodesStub.CreateCephInit: not expected")
}

func (s *panicNodesStub) ListCephLog(ctx context.Context, node string, params *nodes.ListCephLogParams) (*nodes.ListCephLogResponse, error) {
	panic("panicNodesStub.ListCephLog: not expected")
}

func (s *panicNodesStub) ListCephMds(ctx context.Context, node string) (*nodes.ListCephMdsResponse, error) {
	panic("panicNodesStub.ListCephMds: not expected")
}

func (s *panicNodesStub) DeleteCephMds(ctx context.Context, node string, name string) (*nodes.DeleteCephMdsResponse, error) {
	panic("panicNodesStub.DeleteCephMds: not expected")
}

func (s *panicNodesStub) CreateCephMds(ctx context.Context, node string, name string, params *nodes.CreateCephMdsParams) (*nodes.CreateCephMdsResponse, error) {
	panic("panicNodesStub.CreateCephMds: not expected")
}

func (s *panicNodesStub) ListCephMgr(ctx context.Context, node string) (*nodes.ListCephMgrResponse, error) {
	panic("panicNodesStub.ListCephMgr: not expected")
}

func (s *panicNodesStub) DeleteCephMgr(ctx context.Context, node string, id string) (*nodes.DeleteCephMgrResponse, error) {
	panic("panicNodesStub.DeleteCephMgr: not expected")
}

func (s *panicNodesStub) CreateCephMgr(ctx context.Context, node string, id string) (*nodes.CreateCephMgrResponse, error) {
	panic("panicNodesStub.CreateCephMgr: not expected")
}

func (s *panicNodesStub) ListCephMon(ctx context.Context, node string) (*nodes.ListCephMonResponse, error) {
	panic("panicNodesStub.ListCephMon: not expected")
}

func (s *panicNodesStub) DeleteCephMon(ctx context.Context, node string, monid string) (*nodes.DeleteCephMonResponse, error) {
	panic("panicNodesStub.DeleteCephMon: not expected")
}

func (s *panicNodesStub) CreateCephMon(ctx context.Context, node string, monid string, params *nodes.CreateCephMonParams) (*nodes.CreateCephMonResponse, error) {
	panic("panicNodesStub.CreateCephMon: not expected")
}

func (s *panicNodesStub) ListCephOsd(ctx context.Context, node string) (*nodes.ListCephOsdResponse, error) {
	panic("panicNodesStub.ListCephOsd: not expected")
}

func (s *panicNodesStub) CreateCephOsd(ctx context.Context, node string, params *nodes.CreateCephOsdParams) (*nodes.CreateCephOsdResponse, error) {
	panic("panicNodesStub.CreateCephOsd: not expected")
}

func (s *panicNodesStub) DeleteCephOsd(ctx context.Context, node string, osdid string, params *nodes.DeleteCephOsdParams) (*nodes.DeleteCephOsdResponse, error) {
	panic("panicNodesStub.DeleteCephOsd: not expected")
}

func (s *panicNodesStub) GetCephOsd(ctx context.Context, node string, osdid string) (*nodes.GetCephOsdResponse, error) {
	panic("panicNodesStub.GetCephOsd: not expected")
}

func (s *panicNodesStub) CreateCephOsdIn(ctx context.Context, node string, osdid string) error {
	panic("panicNodesStub.CreateCephOsdIn: not expected")
}

func (s *panicNodesStub) ListCephOsdLvInfo(ctx context.Context, node string, osdid string, params *nodes.ListCephOsdLvInfoParams) (*nodes.ListCephOsdLvInfoResponse, error) {
	panic("panicNodesStub.ListCephOsdLvInfo: not expected")
}

func (s *panicNodesStub) ListCephOsdMetadata(ctx context.Context, node string, osdid string) (*nodes.ListCephOsdMetadataResponse, error) {
	panic("panicNodesStub.ListCephOsdMetadata: not expected")
}

func (s *panicNodesStub) CreateCephOsdOut(ctx context.Context, node string, osdid string) error {
	panic("panicNodesStub.CreateCephOsdOut: not expected")
}

func (s *panicNodesStub) CreateCephOsdScrub(ctx context.Context, node string, osdid string, params *nodes.CreateCephOsdScrubParams) error {
	panic("panicNodesStub.CreateCephOsdScrub: not expected")
}

func (s *panicNodesStub) ListCephPool(ctx context.Context, node string) (*nodes.ListCephPoolResponse, error) {
	panic("panicNodesStub.ListCephPool: not expected")
}

func (s *panicNodesStub) CreateCephPool(ctx context.Context, node string, params *nodes.CreateCephPoolParams) (*nodes.CreateCephPoolResponse, error) {
	panic("panicNodesStub.CreateCephPool: not expected")
}

func (s *panicNodesStub) DeleteCephPool(ctx context.Context, node string, name string, params *nodes.DeleteCephPoolParams) (*nodes.DeleteCephPoolResponse, error) {
	panic("panicNodesStub.DeleteCephPool: not expected")
}

func (s *panicNodesStub) GetCephPool(ctx context.Context, node string, name string) (*nodes.GetCephPoolResponse, error) {
	panic("panicNodesStub.GetCephPool: not expected")
}

func (s *panicNodesStub) UpdateCephPool(ctx context.Context, node string, name string, params *nodes.UpdateCephPoolParams) (*nodes.UpdateCephPoolResponse, error) {
	panic("panicNodesStub.UpdateCephPool: not expected")
}

func (s *panicNodesStub) ListCephPoolStatus(ctx context.Context, node string, name string, params *nodes.ListCephPoolStatusParams) (*nodes.ListCephPoolStatusResponse, error) {
	panic("panicNodesStub.ListCephPoolStatus: not expected")
}

func (s *panicNodesStub) CreateCephRestart(ctx context.Context, node string, params *nodes.CreateCephRestartParams) (*nodes.CreateCephRestartResponse, error) {
	panic("panicNodesStub.CreateCephRestart: not expected")
}

func (s *panicNodesStub) ListCephRules(ctx context.Context, node string) (*nodes.ListCephRulesResponse, error) {
	panic("panicNodesStub.ListCephRules: not expected")
}

func (s *panicNodesStub) CreateCephStart(ctx context.Context, node string, params *nodes.CreateCephStartParams) (*nodes.CreateCephStartResponse, error) {
	panic("panicNodesStub.CreateCephStart: not expected")
}

func (s *panicNodesStub) ListCephStatus(ctx context.Context, node string) (*nodes.ListCephStatusResponse, error) {
	panic("panicNodesStub.ListCephStatus: not expected")
}

func (s *panicNodesStub) CreateCephStop(ctx context.Context, node string, params *nodes.CreateCephStopParams) (*nodes.CreateCephStopResponse, error) {
	panic("panicNodesStub.CreateCephStop: not expected")
}

func (s *panicNodesStub) ListCertificates(ctx context.Context, node string) (*nodes.ListCertificatesResponse, error) {
	panic("panicNodesStub.ListCertificates: not expected")
}

func (s *panicNodesStub) ListCertificatesAcme(ctx context.Context, node string) (*nodes.ListCertificatesAcmeResponse, error) {
	panic("panicNodesStub.ListCertificatesAcme: not expected")
}

func (s *panicNodesStub) DeleteCertificatesAcmeCertificate(ctx context.Context, node string) (*nodes.DeleteCertificatesAcmeCertificateResponse, error) {
	panic("panicNodesStub.DeleteCertificatesAcmeCertificate: not expected")
}

func (s *panicNodesStub) CreateCertificatesAcmeCertificate(ctx context.Context, node string, params *nodes.CreateCertificatesAcmeCertificateParams) (*nodes.CreateCertificatesAcmeCertificateResponse, error) {
	panic("panicNodesStub.CreateCertificatesAcmeCertificate: not expected")
}

func (s *panicNodesStub) UpdateCertificatesAcmeCertificate(ctx context.Context, node string, params *nodes.UpdateCertificatesAcmeCertificateParams) (*nodes.UpdateCertificatesAcmeCertificateResponse, error) {
	panic("panicNodesStub.UpdateCertificatesAcmeCertificate: not expected")
}

func (s *panicNodesStub) DeleteCertificatesCustom(ctx context.Context, node string, params *nodes.DeleteCertificatesCustomParams) error {
	panic("panicNodesStub.DeleteCertificatesCustom: not expected")
}

func (s *panicNodesStub) CreateCertificatesCustom(ctx context.Context, node string, params *nodes.CreateCertificatesCustomParams) (*nodes.CreateCertificatesCustomResponse, error) {
	panic("panicNodesStub.CreateCertificatesCustom: not expected")
}

func (s *panicNodesStub) ListCertificatesInfo(ctx context.Context, node string) (*nodes.ListCertificatesInfoResponse, error) {
	panic("panicNodesStub.ListCertificatesInfo: not expected")
}

func (s *panicNodesStub) ListConfig(ctx context.Context, node string, params *nodes.ListConfigParams) (*nodes.ListConfigResponse, error) {
	panic("panicNodesStub.ListConfig: not expected")
}

func (s *panicNodesStub) UpdateConfig(ctx context.Context, node string, params *nodes.UpdateConfigParams) error {
	panic("panicNodesStub.UpdateConfig: not expected")
}

func (s *panicNodesStub) ListDisks(ctx context.Context, node string) (*nodes.ListDisksResponse, error) {
	panic("panicNodesStub.ListDisks: not expected")
}

func (s *panicNodesStub) ListDisksDirectory(ctx context.Context, node string) (*nodes.ListDisksDirectoryResponse, error) {
	panic("panicNodesStub.ListDisksDirectory: not expected")
}

func (s *panicNodesStub) CreateDisksDirectory(ctx context.Context, node string, params *nodes.CreateDisksDirectoryParams) (*nodes.CreateDisksDirectoryResponse, error) {
	panic("panicNodesStub.CreateDisksDirectory: not expected")
}

func (s *panicNodesStub) DeleteDisksDirectory(ctx context.Context, node string, name string, params *nodes.DeleteDisksDirectoryParams) (*nodes.DeleteDisksDirectoryResponse, error) {
	panic("panicNodesStub.DeleteDisksDirectory: not expected")
}

func (s *panicNodesStub) CreateDisksInitgpt(ctx context.Context, node string, params *nodes.CreateDisksInitgptParams) (*nodes.CreateDisksInitgptResponse, error) {
	panic("panicNodesStub.CreateDisksInitgpt: not expected")
}

func (s *panicNodesStub) ListDisksList(ctx context.Context, node string, params *nodes.ListDisksListParams) (*nodes.ListDisksListResponse, error) {
	panic("panicNodesStub.ListDisksList: not expected")
}

func (s *panicNodesStub) ListDisksLvm(ctx context.Context, node string) (*nodes.ListDisksLvmResponse, error) {
	panic("panicNodesStub.ListDisksLvm: not expected")
}

func (s *panicNodesStub) CreateDisksLvm(ctx context.Context, node string, params *nodes.CreateDisksLvmParams) (*nodes.CreateDisksLvmResponse, error) {
	panic("panicNodesStub.CreateDisksLvm: not expected")
}

func (s *panicNodesStub) DeleteDisksLvm(ctx context.Context, node string, name string, params *nodes.DeleteDisksLvmParams) (*nodes.DeleteDisksLvmResponse, error) {
	panic("panicNodesStub.DeleteDisksLvm: not expected")
}

func (s *panicNodesStub) ListDisksLvmthin(ctx context.Context, node string) (*nodes.ListDisksLvmthinResponse, error) {
	panic("panicNodesStub.ListDisksLvmthin: not expected")
}

func (s *panicNodesStub) CreateDisksLvmthin(ctx context.Context, node string, params *nodes.CreateDisksLvmthinParams) (*nodes.CreateDisksLvmthinResponse, error) {
	panic("panicNodesStub.CreateDisksLvmthin: not expected")
}

func (s *panicNodesStub) DeleteDisksLvmthin(ctx context.Context, node string, name string, params *nodes.DeleteDisksLvmthinParams) (*nodes.DeleteDisksLvmthinResponse, error) {
	panic("panicNodesStub.DeleteDisksLvmthin: not expected")
}

func (s *panicNodesStub) ListDisksSmart(ctx context.Context, node string, params *nodes.ListDisksSmartParams) (*nodes.ListDisksSmartResponse, error) {
	panic("panicNodesStub.ListDisksSmart: not expected")
}

func (s *panicNodesStub) UpdateDisksWipedisk(ctx context.Context, node string, params *nodes.UpdateDisksWipediskParams) (*nodes.UpdateDisksWipediskResponse, error) {
	panic("panicNodesStub.UpdateDisksWipedisk: not expected")
}

func (s *panicNodesStub) ListDisksZfs(ctx context.Context, node string) (*nodes.ListDisksZfsResponse, error) {
	panic("panicNodesStub.ListDisksZfs: not expected")
}

func (s *panicNodesStub) CreateDisksZfs(ctx context.Context, node string, params *nodes.CreateDisksZfsParams) (*nodes.CreateDisksZfsResponse, error) {
	panic("panicNodesStub.CreateDisksZfs: not expected")
}

func (s *panicNodesStub) DeleteDisksZfs(ctx context.Context, node string, name string, params *nodes.DeleteDisksZfsParams) (*nodes.DeleteDisksZfsResponse, error) {
	panic("panicNodesStub.DeleteDisksZfs: not expected")
}

func (s *panicNodesStub) GetDisksZfs(ctx context.Context, node string, name string) (*nodes.GetDisksZfsResponse, error) {
	panic("panicNodesStub.GetDisksZfs: not expected")
}

func (s *panicNodesStub) ListDns(ctx context.Context, node string) (*nodes.ListDnsResponse, error) {
	panic("panicNodesStub.ListDns: not expected")
}

func (s *panicNodesStub) UpdateDns(ctx context.Context, node string, params *nodes.UpdateDnsParams) error {
	panic("panicNodesStub.UpdateDns: not expected")
}

func (s *panicNodesStub) CreateExecute(ctx context.Context, node string, params *nodes.CreateExecuteParams) (*nodes.CreateExecuteResponse, error) {
	panic("panicNodesStub.CreateExecute: not expected")
}

func (s *panicNodesStub) ListFirewall(ctx context.Context, node string) (*nodes.ListFirewallResponse, error) {
	panic("panicNodesStub.ListFirewall: not expected")
}

func (s *panicNodesStub) ListFirewallLog(ctx context.Context, node string, params *nodes.ListFirewallLogParams) (*nodes.ListFirewallLogResponse, error) {
	panic("panicNodesStub.ListFirewallLog: not expected")
}

func (s *panicNodesStub) ListFirewallOptions(ctx context.Context, node string) (*nodes.ListFirewallOptionsResponse, error) {
	panic("panicNodesStub.ListFirewallOptions: not expected")
}

func (s *panicNodesStub) UpdateFirewallOptions(ctx context.Context, node string, params *nodes.UpdateFirewallOptionsParams) error {
	panic("panicNodesStub.UpdateFirewallOptions: not expected")
}

func (s *panicNodesStub) ListFirewallRules(ctx context.Context, node string) (*nodes.ListFirewallRulesResponse, error) {
	panic("panicNodesStub.ListFirewallRules: not expected")
}

func (s *panicNodesStub) CreateFirewallRules(ctx context.Context, node string, params *nodes.CreateFirewallRulesParams) error {
	panic("panicNodesStub.CreateFirewallRules: not expected")
}

func (s *panicNodesStub) DeleteFirewallRules(ctx context.Context, node string, pos string, params *nodes.DeleteFirewallRulesParams) error {
	panic("panicNodesStub.DeleteFirewallRules: not expected")
}

func (s *panicNodesStub) GetFirewallRules(ctx context.Context, node string, pos string) (*nodes.GetFirewallRulesResponse, error) {
	panic("panicNodesStub.GetFirewallRules: not expected")
}

func (s *panicNodesStub) UpdateFirewallRules(ctx context.Context, node string, pos string, params *nodes.UpdateFirewallRulesParams) error {
	panic("panicNodesStub.UpdateFirewallRules: not expected")
}

func (s *panicNodesStub) ListHardware(ctx context.Context, node string) (*nodes.ListHardwareResponse, error) {
	panic("panicNodesStub.ListHardware: not expected")
}

func (s *panicNodesStub) ListHardwarePci(ctx context.Context, node string, params *nodes.ListHardwarePciParams) (*nodes.ListHardwarePciResponse, error) {
	panic("panicNodesStub.ListHardwarePci: not expected")
}

func (s *panicNodesStub) GetHardwarePci(ctx context.Context, node string, pciIdOrMapping string) (*nodes.GetHardwarePciResponse, error) {
	panic("panicNodesStub.GetHardwarePci: not expected")
}

func (s *panicNodesStub) ListHardwarePciMdev(ctx context.Context, node string, pciIdOrMapping string) (*nodes.ListHardwarePciMdevResponse, error) {
	panic("panicNodesStub.ListHardwarePciMdev: not expected")
}

func (s *panicNodesStub) ListHardwareUsb(ctx context.Context, node string) (*nodes.ListHardwareUsbResponse, error) {
	panic("panicNodesStub.ListHardwareUsb: not expected")
}

func (s *panicNodesStub) ListHosts(ctx context.Context, node string) (*nodes.ListHostsResponse, error) {
	panic("panicNodesStub.ListHosts: not expected")
}

func (s *panicNodesStub) CreateHosts(ctx context.Context, node string, params *nodes.CreateHostsParams) error {
	panic("panicNodesStub.CreateHosts: not expected")
}

func (s *panicNodesStub) ListJournal(ctx context.Context, node string, params *nodes.ListJournalParams) (*nodes.ListJournalResponse, error) {
	panic("panicNodesStub.ListJournal: not expected")
}

func (s *panicNodesStub) ListLxc(ctx context.Context, node string) (*nodes.ListLxcResponse, error) {
	panic("panicNodesStub.ListLxc: not expected")
}

func (s *panicNodesStub) CreateLxc(ctx context.Context, node string, params *nodes.CreateLxcParams) (*nodes.CreateLxcResponse, error) {
	panic("panicNodesStub.CreateLxc: not expected")
}

func (s *panicNodesStub) DeleteLxc(ctx context.Context, node string, vmid string, params *nodes.DeleteLxcParams) (*nodes.DeleteLxcResponse, error) {
	panic("panicNodesStub.DeleteLxc: not expected")
}

func (s *panicNodesStub) GetLxc(ctx context.Context, node string, vmid string) (*nodes.GetLxcResponse, error) {
	panic("panicNodesStub.GetLxc: not expected")
}

func (s *panicNodesStub) CreateLxcClone(ctx context.Context, node string, vmid string, params *nodes.CreateLxcCloneParams) (*nodes.CreateLxcCloneResponse, error) {
	panic("panicNodesStub.CreateLxcClone: not expected")
}

func (s *panicNodesStub) ListLxcConfig(ctx context.Context, node string, vmid string, params *nodes.ListLxcConfigParams) (*nodes.ListLxcConfigResponse, error) {
	panic("panicNodesStub.ListLxcConfig: not expected")
}

func (s *panicNodesStub) UpdateLxcConfig(ctx context.Context, node string, vmid string, params *nodes.UpdateLxcConfigParams) error {
	panic("panicNodesStub.UpdateLxcConfig: not expected")
}

func (s *panicNodesStub) ListLxcFeature(ctx context.Context, node string, vmid string, params *nodes.ListLxcFeatureParams) (*nodes.ListLxcFeatureResponse, error) {
	panic("panicNodesStub.ListLxcFeature: not expected")
}

func (s *panicNodesStub) ListLxcFirewall(ctx context.Context, node string, vmid string) (*nodes.ListLxcFirewallResponse, error) {
	panic("panicNodesStub.ListLxcFirewall: not expected")
}

func (s *panicNodesStub) ListLxcFirewallAliases(ctx context.Context, node string, vmid string) (*nodes.ListLxcFirewallAliasesResponse, error) {
	panic("panicNodesStub.ListLxcFirewallAliases: not expected")
}

func (s *panicNodesStub) CreateLxcFirewallAliases(ctx context.Context, node string, vmid string, params *nodes.CreateLxcFirewallAliasesParams) error {
	panic("panicNodesStub.CreateLxcFirewallAliases: not expected")
}

func (s *panicNodesStub) DeleteLxcFirewallAliases(ctx context.Context, node string, vmid string, name string, params *nodes.DeleteLxcFirewallAliasesParams) error {
	panic("panicNodesStub.DeleteLxcFirewallAliases: not expected")
}

func (s *panicNodesStub) GetLxcFirewallAliases(ctx context.Context, node string, vmid string, name string) (*nodes.GetLxcFirewallAliasesResponse, error) {
	panic("panicNodesStub.GetLxcFirewallAliases: not expected")
}

func (s *panicNodesStub) UpdateLxcFirewallAliases(ctx context.Context, node string, vmid string, name string, params *nodes.UpdateLxcFirewallAliasesParams) error {
	panic("panicNodesStub.UpdateLxcFirewallAliases: not expected")
}

func (s *panicNodesStub) ListLxcFirewallIpset(ctx context.Context, node string, vmid string) (*nodes.ListLxcFirewallIpsetResponse, error) {
	panic("panicNodesStub.ListLxcFirewallIpset: not expected")
}

func (s *panicNodesStub) CreateLxcFirewallIpset(ctx context.Context, node string, vmid string, params *nodes.CreateLxcFirewallIpsetParams) error {
	panic("panicNodesStub.CreateLxcFirewallIpset: not expected")
}

func (s *panicNodesStub) DeleteLxcFirewallIpset(ctx context.Context, node string, vmid string, name string, params *nodes.DeleteLxcFirewallIpsetParams) error {
	panic("panicNodesStub.DeleteLxcFirewallIpset: not expected")
}

func (s *panicNodesStub) GetLxcFirewallIpset(ctx context.Context, node string, vmid string, name string) (*nodes.GetLxcFirewallIpsetResponse, error) {
	panic("panicNodesStub.GetLxcFirewallIpset: not expected")
}

func (s *panicNodesStub) CreateLxcFirewallIpset2(ctx context.Context, node string, vmid string, name string, params *nodes.CreateLxcFirewallIpset2Params) error {
	panic("panicNodesStub.CreateLxcFirewallIpset2: not expected")
}

func (s *panicNodesStub) DeleteLxcFirewallIpset2(ctx context.Context, node string, vmid string, name string, cidr string, params *nodes.DeleteLxcFirewallIpset2Params) error {
	panic("panicNodesStub.DeleteLxcFirewallIpset2: not expected")
}

func (s *panicNodesStub) GetLxcFirewallIpset2(ctx context.Context, node string, vmid string, name string, cidr string) (*nodes.GetLxcFirewallIpset2Response, error) {
	panic("panicNodesStub.GetLxcFirewallIpset2: not expected")
}

func (s *panicNodesStub) UpdateLxcFirewallIpset(ctx context.Context, node string, vmid string, name string, cidr string, params *nodes.UpdateLxcFirewallIpsetParams) error {
	panic("panicNodesStub.UpdateLxcFirewallIpset: not expected")
}

func (s *panicNodesStub) ListLxcFirewallLog(ctx context.Context, node string, vmid string, params *nodes.ListLxcFirewallLogParams) (*nodes.ListLxcFirewallLogResponse, error) {
	panic("panicNodesStub.ListLxcFirewallLog: not expected")
}

func (s *panicNodesStub) ListLxcFirewallOptions(ctx context.Context, node string, vmid string) (*nodes.ListLxcFirewallOptionsResponse, error) {
	panic("panicNodesStub.ListLxcFirewallOptions: not expected")
}

func (s *panicNodesStub) UpdateLxcFirewallOptions(ctx context.Context, node string, vmid string, params *nodes.UpdateLxcFirewallOptionsParams) error {
	panic("panicNodesStub.UpdateLxcFirewallOptions: not expected")
}

func (s *panicNodesStub) ListLxcFirewallRefs(ctx context.Context, node string, vmid string, params *nodes.ListLxcFirewallRefsParams) (*nodes.ListLxcFirewallRefsResponse, error) {
	panic("panicNodesStub.ListLxcFirewallRefs: not expected")
}

func (s *panicNodesStub) ListLxcFirewallRules(ctx context.Context, node string, vmid string) (*nodes.ListLxcFirewallRulesResponse, error) {
	panic("panicNodesStub.ListLxcFirewallRules: not expected")
}

func (s *panicNodesStub) CreateLxcFirewallRules(ctx context.Context, node string, vmid string, params *nodes.CreateLxcFirewallRulesParams) error {
	panic("panicNodesStub.CreateLxcFirewallRules: not expected")
}

func (s *panicNodesStub) DeleteLxcFirewallRules(ctx context.Context, node string, vmid string, pos string, params *nodes.DeleteLxcFirewallRulesParams) error {
	panic("panicNodesStub.DeleteLxcFirewallRules: not expected")
}

func (s *panicNodesStub) GetLxcFirewallRules(ctx context.Context, node string, vmid string, pos string) (*nodes.GetLxcFirewallRulesResponse, error) {
	panic("panicNodesStub.GetLxcFirewallRules: not expected")
}

func (s *panicNodesStub) UpdateLxcFirewallRules(ctx context.Context, node string, vmid string, pos string, params *nodes.UpdateLxcFirewallRulesParams) error {
	panic("panicNodesStub.UpdateLxcFirewallRules: not expected")
}

func (s *panicNodesStub) ListLxcInterfaces(ctx context.Context, node string, vmid string) (*nodes.ListLxcInterfacesResponse, error) {
	panic("panicNodesStub.ListLxcInterfaces: not expected")
}

func (s *panicNodesStub) ListLxcMigrate(ctx context.Context, node string, vmid string, params *nodes.ListLxcMigrateParams) (*nodes.ListLxcMigrateResponse, error) {
	panic("panicNodesStub.ListLxcMigrate: not expected")
}

func (s *panicNodesStub) CreateLxcMigrate(ctx context.Context, node string, vmid string, params *nodes.CreateLxcMigrateParams) (*nodes.CreateLxcMigrateResponse, error) {
	panic("panicNodesStub.CreateLxcMigrate: not expected")
}

func (s *panicNodesStub) CreateLxcMoveVolume(ctx context.Context, node string, vmid string, params *nodes.CreateLxcMoveVolumeParams) (*nodes.CreateLxcMoveVolumeResponse, error) {
	panic("panicNodesStub.CreateLxcMoveVolume: not expected")
}

func (s *panicNodesStub) CreateLxcMtunnel(ctx context.Context, node string, vmid string, params *nodes.CreateLxcMtunnelParams) (*nodes.CreateLxcMtunnelResponse, error) {
	panic("panicNodesStub.CreateLxcMtunnel: not expected")
}

func (s *panicNodesStub) ListLxcMtunnelwebsocket(ctx context.Context, node string, vmid string, params *nodes.ListLxcMtunnelwebsocketParams) (*nodes.ListLxcMtunnelwebsocketResponse, error) {
	panic("panicNodesStub.ListLxcMtunnelwebsocket: not expected")
}

func (s *panicNodesStub) ListLxcPending(ctx context.Context, node string, vmid string) (*nodes.ListLxcPendingResponse, error) {
	panic("panicNodesStub.ListLxcPending: not expected")
}

func (s *panicNodesStub) CreateLxcRemoteMigrate(ctx context.Context, node string, vmid string, params *nodes.CreateLxcRemoteMigrateParams) (*nodes.CreateLxcRemoteMigrateResponse, error) {
	panic("panicNodesStub.CreateLxcRemoteMigrate: not expected")
}

func (s *panicNodesStub) UpdateLxcResize(ctx context.Context, node string, vmid string, params *nodes.UpdateLxcResizeParams) (*nodes.UpdateLxcResizeResponse, error) {
	panic("panicNodesStub.UpdateLxcResize: not expected")
}

func (s *panicNodesStub) ListLxcRrd(ctx context.Context, node string, vmid string, params *nodes.ListLxcRrdParams) (*nodes.ListLxcRrdResponse, error) {
	panic("panicNodesStub.ListLxcRrd: not expected")
}

func (s *panicNodesStub) ListLxcRrddata(ctx context.Context, node string, vmid string, params *nodes.ListLxcRrddataParams) (*nodes.ListLxcRrddataResponse, error) {
	panic("panicNodesStub.ListLxcRrddata: not expected")
}

func (s *panicNodesStub) ListLxcSnapshot(ctx context.Context, node string, vmid string) (*nodes.ListLxcSnapshotResponse, error) {
	panic("panicNodesStub.ListLxcSnapshot: not expected")
}

func (s *panicNodesStub) CreateLxcSnapshot(ctx context.Context, node string, vmid string, params *nodes.CreateLxcSnapshotParams) (*nodes.CreateLxcSnapshotResponse, error) {
	panic("panicNodesStub.CreateLxcSnapshot: not expected")
}

func (s *panicNodesStub) DeleteLxcSnapshot(ctx context.Context, node string, vmid string, snapname string, params *nodes.DeleteLxcSnapshotParams) (*nodes.DeleteLxcSnapshotResponse, error) {
	panic("panicNodesStub.DeleteLxcSnapshot: not expected")
}

func (s *panicNodesStub) GetLxcSnapshot(ctx context.Context, node string, vmid string, snapname string) (*nodes.GetLxcSnapshotResponse, error) {
	panic("panicNodesStub.GetLxcSnapshot: not expected")
}

func (s *panicNodesStub) ListLxcSnapshotConfig(ctx context.Context, node string, vmid string, snapname string) (*nodes.ListLxcSnapshotConfigResponse, error) {
	panic("panicNodesStub.ListLxcSnapshotConfig: not expected")
}

func (s *panicNodesStub) UpdateLxcSnapshotConfig(ctx context.Context, node string, vmid string, snapname string, params *nodes.UpdateLxcSnapshotConfigParams) error {
	panic("panicNodesStub.UpdateLxcSnapshotConfig: not expected")
}

func (s *panicNodesStub) CreateLxcSnapshotRollback(ctx context.Context, node string, vmid string, snapname string, params *nodes.CreateLxcSnapshotRollbackParams) (*nodes.CreateLxcSnapshotRollbackResponse, error) {
	panic("panicNodesStub.CreateLxcSnapshotRollback: not expected")
}

func (s *panicNodesStub) CreateLxcSpiceproxy(ctx context.Context, node string, vmid string, params *nodes.CreateLxcSpiceproxyParams) (*nodes.CreateLxcSpiceproxyResponse, error) {
	panic("panicNodesStub.CreateLxcSpiceproxy: not expected")
}

func (s *panicNodesStub) ListLxcStatus(ctx context.Context, node string, vmid string) (*nodes.ListLxcStatusResponse, error) {
	panic("panicNodesStub.ListLxcStatus: not expected")
}

func (s *panicNodesStub) ListLxcStatusCurrent(ctx context.Context, node string, vmid string) (*nodes.ListLxcStatusCurrentResponse, error) {
	panic("panicNodesStub.ListLxcStatusCurrent: not expected")
}

func (s *panicNodesStub) CreateLxcStatusReboot(ctx context.Context, node string, vmid string, params *nodes.CreateLxcStatusRebootParams) (*nodes.CreateLxcStatusRebootResponse, error) {
	panic("panicNodesStub.CreateLxcStatusReboot: not expected")
}

func (s *panicNodesStub) CreateLxcStatusResume(ctx context.Context, node string, vmid string) (*nodes.CreateLxcStatusResumeResponse, error) {
	panic("panicNodesStub.CreateLxcStatusResume: not expected")
}

func (s *panicNodesStub) CreateLxcStatusShutdown(ctx context.Context, node string, vmid string, params *nodes.CreateLxcStatusShutdownParams) (*nodes.CreateLxcStatusShutdownResponse, error) {
	panic("panicNodesStub.CreateLxcStatusShutdown: not expected")
}

func (s *panicNodesStub) CreateLxcStatusStart(ctx context.Context, node string, vmid string, params *nodes.CreateLxcStatusStartParams) (*nodes.CreateLxcStatusStartResponse, error) {
	panic("panicNodesStub.CreateLxcStatusStart: not expected")
}

func (s *panicNodesStub) CreateLxcStatusStop(ctx context.Context, node string, vmid string, params *nodes.CreateLxcStatusStopParams) (*nodes.CreateLxcStatusStopResponse, error) {
	panic("panicNodesStub.CreateLxcStatusStop: not expected")
}

func (s *panicNodesStub) CreateLxcStatusSuspend(ctx context.Context, node string, vmid string) (*nodes.CreateLxcStatusSuspendResponse, error) {
	panic("panicNodesStub.CreateLxcStatusSuspend: not expected")
}

func (s *panicNodesStub) CreateLxcTemplate(ctx context.Context, node string, vmid string) error {
	panic("panicNodesStub.CreateLxcTemplate: not expected")
}

func (s *panicNodesStub) CreateLxcTermproxy(ctx context.Context, node string, vmid string) (*nodes.CreateLxcTermproxyResponse, error) {
	panic("panicNodesStub.CreateLxcTermproxy: not expected")
}

func (s *panicNodesStub) CreateLxcVncproxy(ctx context.Context, node string, vmid string, params *nodes.CreateLxcVncproxyParams) (*nodes.CreateLxcVncproxyResponse, error) {
	panic("panicNodesStub.CreateLxcVncproxy: not expected")
}

func (s *panicNodesStub) ListLxcVncwebsocket(ctx context.Context, node string, vmid string, params *nodes.ListLxcVncwebsocketParams) (*nodes.ListLxcVncwebsocketResponse, error) {
	panic("panicNodesStub.ListLxcVncwebsocket: not expected")
}

func (s *panicNodesStub) CreateMigrateall(ctx context.Context, node string, params *nodes.CreateMigrateallParams) (*nodes.CreateMigrateallResponse, error) {
	panic("panicNodesStub.CreateMigrateall: not expected")
}

func (s *panicNodesStub) ListNetstat(ctx context.Context, node string) (*nodes.ListNetstatResponse, error) {
	panic("panicNodesStub.ListNetstat: not expected")
}

func (s *panicNodesStub) DeleteNetwork(ctx context.Context, node string) error {
	panic("panicNodesStub.DeleteNetwork: not expected")
}

func (s *panicNodesStub) ListNetwork(ctx context.Context, node string, params *nodes.ListNetworkParams) (*nodes.ListNetworkResponse, error) {
	panic("panicNodesStub.ListNetwork: not expected")
}

func (s *panicNodesStub) CreateNetwork(ctx context.Context, node string, params *nodes.CreateNetworkParams) error {
	panic("panicNodesStub.CreateNetwork: not expected")
}

func (s *panicNodesStub) UpdateNetwork(ctx context.Context, node string, params *nodes.UpdateNetworkParams) (*nodes.UpdateNetworkResponse, error) {
	panic("panicNodesStub.UpdateNetwork: not expected")
}

func (s *panicNodesStub) DeleteNetwork2(ctx context.Context, node string, iface string) error {
	panic("panicNodesStub.DeleteNetwork2: not expected")
}

func (s *panicNodesStub) GetNetwork(ctx context.Context, node string, iface string) (*nodes.GetNetworkResponse, error) {
	panic("panicNodesStub.GetNetwork: not expected")
}

func (s *panicNodesStub) UpdateNetwork2(ctx context.Context, node string, iface string, params *nodes.UpdateNetwork2Params) error {
	panic("panicNodesStub.UpdateNetwork2: not expected")
}

func (s *panicNodesStub) ListQemu(ctx context.Context, node string, params *nodes.ListQemuParams) (*nodes.ListQemuResponse, error) {
	panic("panicNodesStub.ListQemu: not expected")
}

func (s *panicNodesStub) CreateQemu(ctx context.Context, node string, params *nodes.CreateQemuParams) (*nodes.CreateQemuResponse, error) {
	panic("panicNodesStub.CreateQemu: not expected")
}

func (s *panicNodesStub) DeleteQemu(ctx context.Context, node string, vmid string, params *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
	panic("panicNodesStub.DeleteQemu: not expected")
}

func (s *panicNodesStub) GetQemu(ctx context.Context, node string, vmid string) (*nodes.GetQemuResponse, error) {
	panic("panicNodesStub.GetQemu: not expected")
}

func (s *panicNodesStub) ListQemuAgent(ctx context.Context, node string, vmid string) (*nodes.ListQemuAgentResponse, error) {
	panic("panicNodesStub.ListQemuAgent: not expected")
}

func (s *panicNodesStub) CreateQemuAgent(ctx context.Context, node string, vmid string, params *nodes.CreateQemuAgentParams) (*nodes.CreateQemuAgentResponse, error) {
	panic("panicNodesStub.CreateQemuAgent: not expected")
}

func (s *panicNodesStub) CreateQemuAgentExec(ctx context.Context, node string, vmid string, params *nodes.CreateQemuAgentExecParams) (*nodes.CreateQemuAgentExecResponse, error) {
	panic("panicNodesStub.CreateQemuAgentExec: not expected")
}

func (s *panicNodesStub) ListQemuAgentExecStatus(ctx context.Context, node string, vmid string, params *nodes.ListQemuAgentExecStatusParams) (*nodes.ListQemuAgentExecStatusResponse, error) {
	panic("panicNodesStub.ListQemuAgentExecStatus: not expected")
}

func (s *panicNodesStub) ListQemuAgentFileRead(ctx context.Context, node string, vmid string, params *nodes.ListQemuAgentFileReadParams) (*nodes.ListQemuAgentFileReadResponse, error) {
	panic("panicNodesStub.ListQemuAgentFileRead: not expected")
}

func (s *panicNodesStub) CreateQemuAgentFileWrite(ctx context.Context, node string, vmid string, params *nodes.CreateQemuAgentFileWriteParams) error {
	panic("panicNodesStub.CreateQemuAgentFileWrite: not expected")
}

func (s *panicNodesStub) CreateQemuAgentFsfreezeFreeze(ctx context.Context, node string, vmid string) (*nodes.CreateQemuAgentFsfreezeFreezeResponse, error) {
	panic("panicNodesStub.CreateQemuAgentFsfreezeFreeze: not expected")
}

func (s *panicNodesStub) CreateQemuAgentFsfreezeStatus(ctx context.Context, node string, vmid string) (*nodes.CreateQemuAgentFsfreezeStatusResponse, error) {
	panic("panicNodesStub.CreateQemuAgentFsfreezeStatus: not expected")
}

func (s *panicNodesStub) CreateQemuAgentFsfreezeThaw(ctx context.Context, node string, vmid string) (*nodes.CreateQemuAgentFsfreezeThawResponse, error) {
	panic("panicNodesStub.CreateQemuAgentFsfreezeThaw: not expected")
}

func (s *panicNodesStub) CreateQemuAgentFstrim(ctx context.Context, node string, vmid string) (*nodes.CreateQemuAgentFstrimResponse, error) {
	panic("panicNodesStub.CreateQemuAgentFstrim: not expected")
}

func (s *panicNodesStub) ListQemuAgentGetFsinfo(ctx context.Context, node string, vmid string) (*nodes.ListQemuAgentGetFsinfoResponse, error) {
	panic("panicNodesStub.ListQemuAgentGetFsinfo: not expected")
}

func (s *panicNodesStub) ListQemuAgentGetHostName(ctx context.Context, node string, vmid string) (*nodes.ListQemuAgentGetHostNameResponse, error) {
	panic("panicNodesStub.ListQemuAgentGetHostName: not expected")
}

func (s *panicNodesStub) ListQemuAgentGetMemoryBlockInfo(ctx context.Context, node string, vmid string) (*nodes.ListQemuAgentGetMemoryBlockInfoResponse, error) {
	panic("panicNodesStub.ListQemuAgentGetMemoryBlockInfo: not expected")
}

func (s *panicNodesStub) ListQemuAgentGetMemoryBlocks(ctx context.Context, node string, vmid string) (*nodes.ListQemuAgentGetMemoryBlocksResponse, error) {
	panic("panicNodesStub.ListQemuAgentGetMemoryBlocks: not expected")
}

func (s *panicNodesStub) ListQemuAgentGetOsinfo(ctx context.Context, node string, vmid string) (*nodes.ListQemuAgentGetOsinfoResponse, error) {
	panic("panicNodesStub.ListQemuAgentGetOsinfo: not expected")
}

func (s *panicNodesStub) ListQemuAgentGetTime(ctx context.Context, node string, vmid string) (*nodes.ListQemuAgentGetTimeResponse, error) {
	panic("panicNodesStub.ListQemuAgentGetTime: not expected")
}

func (s *panicNodesStub) ListQemuAgentGetTimezone(ctx context.Context, node string, vmid string) (*nodes.ListQemuAgentGetTimezoneResponse, error) {
	panic("panicNodesStub.ListQemuAgentGetTimezone: not expected")
}

func (s *panicNodesStub) ListQemuAgentGetUsers(ctx context.Context, node string, vmid string) (*nodes.ListQemuAgentGetUsersResponse, error) {
	panic("panicNodesStub.ListQemuAgentGetUsers: not expected")
}

func (s *panicNodesStub) ListQemuAgentGetVcpus(ctx context.Context, node string, vmid string) (*nodes.ListQemuAgentGetVcpusResponse, error) {
	panic("panicNodesStub.ListQemuAgentGetVcpus: not expected")
}

func (s *panicNodesStub) ListQemuAgentInfo(ctx context.Context, node string, vmid string) (*nodes.ListQemuAgentInfoResponse, error) {
	panic("panicNodesStub.ListQemuAgentInfo: not expected")
}

func (s *panicNodesStub) ListQemuAgentNetworkGetInterfaces(ctx context.Context, node string, vmid string) (*nodes.ListQemuAgentNetworkGetInterfacesResponse, error) {
	panic("panicNodesStub.ListQemuAgentNetworkGetInterfaces: not expected")
}

func (s *panicNodesStub) CreateQemuAgentPing(ctx context.Context, node string, vmid string) (*nodes.CreateQemuAgentPingResponse, error) {
	panic("panicNodesStub.CreateQemuAgentPing: not expected")
}

func (s *panicNodesStub) CreateQemuAgentSetUserPassword(ctx context.Context, node string, vmid string, params *nodes.CreateQemuAgentSetUserPasswordParams) (*nodes.CreateQemuAgentSetUserPasswordResponse, error) {
	panic("panicNodesStub.CreateQemuAgentSetUserPassword: not expected")
}

func (s *panicNodesStub) CreateQemuAgentShutdown(ctx context.Context, node string, vmid string) (*nodes.CreateQemuAgentShutdownResponse, error) {
	panic("panicNodesStub.CreateQemuAgentShutdown: not expected")
}

func (s *panicNodesStub) CreateQemuAgentSuspendDisk(ctx context.Context, node string, vmid string) (*nodes.CreateQemuAgentSuspendDiskResponse, error) {
	panic("panicNodesStub.CreateQemuAgentSuspendDisk: not expected")
}

func (s *panicNodesStub) CreateQemuAgentSuspendHybrid(ctx context.Context, node string, vmid string) (*nodes.CreateQemuAgentSuspendHybridResponse, error) {
	panic("panicNodesStub.CreateQemuAgentSuspendHybrid: not expected")
}

func (s *panicNodesStub) CreateQemuAgentSuspendRam(ctx context.Context, node string, vmid string) (*nodes.CreateQemuAgentSuspendRamResponse, error) {
	panic("panicNodesStub.CreateQemuAgentSuspendRam: not expected")
}

func (s *panicNodesStub) CreateQemuClone(ctx context.Context, node string, vmid string, params *nodes.CreateQemuCloneParams) (*nodes.CreateQemuCloneResponse, error) {
	panic("panicNodesStub.CreateQemuClone: not expected")
}

func (s *panicNodesStub) ListQemuCloudinit(ctx context.Context, node string, vmid string) (*nodes.ListQemuCloudinitResponse, error) {
	panic("panicNodesStub.ListQemuCloudinit: not expected")
}

func (s *panicNodesStub) UpdateQemuCloudinit(ctx context.Context, node string, vmid string) error {
	panic("panicNodesStub.UpdateQemuCloudinit: not expected")
}

func (s *panicNodesStub) ListQemuCloudinitDump(ctx context.Context, node string, vmid string, params *nodes.ListQemuCloudinitDumpParams) (*nodes.ListQemuCloudinitDumpResponse, error) {
	panic("panicNodesStub.ListQemuCloudinitDump: not expected")
}

func (s *panicNodesStub) ListQemuConfig(ctx context.Context, node string, vmid string, params *nodes.ListQemuConfigParams) (*nodes.ListQemuConfigResponse, error) {
	panic("panicNodesStub.ListQemuConfig: not expected")
}

func (s *panicNodesStub) CreateQemuConfig(ctx context.Context, node string, vmid string, params *nodes.CreateQemuConfigParams) (*nodes.CreateQemuConfigResponse, error) {
	panic("panicNodesStub.CreateQemuConfig: not expected")
}

func (s *panicNodesStub) UpdateQemuConfig(ctx context.Context, node string, vmid string, params *nodes.UpdateQemuConfigParams) error {
	panic("panicNodesStub.UpdateQemuConfig: not expected")
}

func (s *panicNodesStub) CreateQemuDbusVmstate(ctx context.Context, node string, vmid string, params *nodes.CreateQemuDbusVmstateParams) error {
	panic("panicNodesStub.CreateQemuDbusVmstate: not expected")
}

func (s *panicNodesStub) ListQemuFeature(ctx context.Context, node string, vmid string, params *nodes.ListQemuFeatureParams) (*nodes.ListQemuFeatureResponse, error) {
	panic("panicNodesStub.ListQemuFeature: not expected")
}

func (s *panicNodesStub) ListQemuFirewall(ctx context.Context, node string, vmid string) (*nodes.ListQemuFirewallResponse, error) {
	panic("panicNodesStub.ListQemuFirewall: not expected")
}

func (s *panicNodesStub) ListQemuFirewallAliases(ctx context.Context, node string, vmid string) (*nodes.ListQemuFirewallAliasesResponse, error) {
	panic("panicNodesStub.ListQemuFirewallAliases: not expected")
}

func (s *panicNodesStub) CreateQemuFirewallAliases(ctx context.Context, node string, vmid string, params *nodes.CreateQemuFirewallAliasesParams) error {
	panic("panicNodesStub.CreateQemuFirewallAliases: not expected")
}

func (s *panicNodesStub) DeleteQemuFirewallAliases(ctx context.Context, node string, vmid string, name string, params *nodes.DeleteQemuFirewallAliasesParams) error {
	panic("panicNodesStub.DeleteQemuFirewallAliases: not expected")
}

func (s *panicNodesStub) GetQemuFirewallAliases(ctx context.Context, node string, vmid string, name string) (*nodes.GetQemuFirewallAliasesResponse, error) {
	panic("panicNodesStub.GetQemuFirewallAliases: not expected")
}

func (s *panicNodesStub) UpdateQemuFirewallAliases(ctx context.Context, node string, vmid string, name string, params *nodes.UpdateQemuFirewallAliasesParams) error {
	panic("panicNodesStub.UpdateQemuFirewallAliases: not expected")
}

func (s *panicNodesStub) ListQemuFirewallIpset(ctx context.Context, node string, vmid string) (*nodes.ListQemuFirewallIpsetResponse, error) {
	panic("panicNodesStub.ListQemuFirewallIpset: not expected")
}

func (s *panicNodesStub) CreateQemuFirewallIpset(ctx context.Context, node string, vmid string, params *nodes.CreateQemuFirewallIpsetParams) error {
	panic("panicNodesStub.CreateQemuFirewallIpset: not expected")
}

func (s *panicNodesStub) DeleteQemuFirewallIpset(ctx context.Context, node string, vmid string, name string, params *nodes.DeleteQemuFirewallIpsetParams) error {
	panic("panicNodesStub.DeleteQemuFirewallIpset: not expected")
}

func (s *panicNodesStub) GetQemuFirewallIpset(ctx context.Context, node string, vmid string, name string) (*nodes.GetQemuFirewallIpsetResponse, error) {
	panic("panicNodesStub.GetQemuFirewallIpset: not expected")
}

func (s *panicNodesStub) CreateQemuFirewallIpset2(ctx context.Context, node string, vmid string, name string, params *nodes.CreateQemuFirewallIpset2Params) error {
	panic("panicNodesStub.CreateQemuFirewallIpset2: not expected")
}

func (s *panicNodesStub) DeleteQemuFirewallIpset2(ctx context.Context, node string, vmid string, name string, cidr string, params *nodes.DeleteQemuFirewallIpset2Params) error {
	panic("panicNodesStub.DeleteQemuFirewallIpset2: not expected")
}

func (s *panicNodesStub) GetQemuFirewallIpset2(ctx context.Context, node string, vmid string, name string, cidr string) (*nodes.GetQemuFirewallIpset2Response, error) {
	panic("panicNodesStub.GetQemuFirewallIpset2: not expected")
}

func (s *panicNodesStub) UpdateQemuFirewallIpset(ctx context.Context, node string, vmid string, name string, cidr string, params *nodes.UpdateQemuFirewallIpsetParams) error {
	panic("panicNodesStub.UpdateQemuFirewallIpset: not expected")
}

func (s *panicNodesStub) ListQemuFirewallLog(ctx context.Context, node string, vmid string, params *nodes.ListQemuFirewallLogParams) (*nodes.ListQemuFirewallLogResponse, error) {
	panic("panicNodesStub.ListQemuFirewallLog: not expected")
}

func (s *panicNodesStub) ListQemuFirewallOptions(ctx context.Context, node string, vmid string) (*nodes.ListQemuFirewallOptionsResponse, error) {
	panic("panicNodesStub.ListQemuFirewallOptions: not expected")
}

func (s *panicNodesStub) UpdateQemuFirewallOptions(ctx context.Context, node string, vmid string, params *nodes.UpdateQemuFirewallOptionsParams) error {
	panic("panicNodesStub.UpdateQemuFirewallOptions: not expected")
}

func (s *panicNodesStub) ListQemuFirewallRefs(ctx context.Context, node string, vmid string, params *nodes.ListQemuFirewallRefsParams) (*nodes.ListQemuFirewallRefsResponse, error) {
	panic("panicNodesStub.ListQemuFirewallRefs: not expected")
}

func (s *panicNodesStub) ListQemuFirewallRules(ctx context.Context, node string, vmid string) (*nodes.ListQemuFirewallRulesResponse, error) {
	panic("panicNodesStub.ListQemuFirewallRules: not expected")
}

func (s *panicNodesStub) CreateQemuFirewallRules(ctx context.Context, node string, vmid string, params *nodes.CreateQemuFirewallRulesParams) error {
	panic("panicNodesStub.CreateQemuFirewallRules: not expected")
}

func (s *panicNodesStub) DeleteQemuFirewallRules(ctx context.Context, node string, vmid string, pos string, params *nodes.DeleteQemuFirewallRulesParams) error {
	panic("panicNodesStub.DeleteQemuFirewallRules: not expected")
}

func (s *panicNodesStub) GetQemuFirewallRules(ctx context.Context, node string, vmid string, pos string) (*nodes.GetQemuFirewallRulesResponse, error) {
	panic("panicNodesStub.GetQemuFirewallRules: not expected")
}

func (s *panicNodesStub) UpdateQemuFirewallRules(ctx context.Context, node string, vmid string, pos string, params *nodes.UpdateQemuFirewallRulesParams) error {
	panic("panicNodesStub.UpdateQemuFirewallRules: not expected")
}

func (s *panicNodesStub) ListQemuMigrate(ctx context.Context, node string, vmid string, params *nodes.ListQemuMigrateParams) (*nodes.ListQemuMigrateResponse, error) {
	panic("panicNodesStub.ListQemuMigrate: not expected")
}

func (s *panicNodesStub) CreateQemuMigrate(ctx context.Context, node string, vmid string, params *nodes.CreateQemuMigrateParams) (*nodes.CreateQemuMigrateResponse, error) {
	panic("panicNodesStub.CreateQemuMigrate: not expected")
}

func (s *panicNodesStub) CreateQemuMonitor(ctx context.Context, node string, vmid string, params *nodes.CreateQemuMonitorParams) (*nodes.CreateQemuMonitorResponse, error) {
	panic("panicNodesStub.CreateQemuMonitor: not expected")
}

func (s *panicNodesStub) CreateQemuMoveDisk(ctx context.Context, node string, vmid string, params *nodes.CreateQemuMoveDiskParams) (*nodes.CreateQemuMoveDiskResponse, error) {
	panic("panicNodesStub.CreateQemuMoveDisk: not expected")
}

func (s *panicNodesStub) CreateQemuMtunnel(ctx context.Context, node string, vmid string, params *nodes.CreateQemuMtunnelParams) (*nodes.CreateQemuMtunnelResponse, error) {
	panic("panicNodesStub.CreateQemuMtunnel: not expected")
}

func (s *panicNodesStub) ListQemuMtunnelwebsocket(ctx context.Context, node string, vmid string, params *nodes.ListQemuMtunnelwebsocketParams) (*nodes.ListQemuMtunnelwebsocketResponse, error) {
	panic("panicNodesStub.ListQemuMtunnelwebsocket: not expected")
}

func (s *panicNodesStub) ListQemuPending(ctx context.Context, node string, vmid string) (*nodes.ListQemuPendingResponse, error) {
	panic("panicNodesStub.ListQemuPending: not expected")
}

func (s *panicNodesStub) CreateQemuRemoteMigrate(ctx context.Context, node string, vmid string, params *nodes.CreateQemuRemoteMigrateParams) (*nodes.CreateQemuRemoteMigrateResponse, error) {
	panic("panicNodesStub.CreateQemuRemoteMigrate: not expected")
}

func (s *panicNodesStub) UpdateQemuResize(ctx context.Context, node string, vmid string, params *nodes.UpdateQemuResizeParams) (*nodes.UpdateQemuResizeResponse, error) {
	panic("panicNodesStub.UpdateQemuResize: not expected")
}

func (s *panicNodesStub) ListQemuRrd(ctx context.Context, node string, vmid string, params *nodes.ListQemuRrdParams) (*nodes.ListQemuRrdResponse, error) {
	panic("panicNodesStub.ListQemuRrd: not expected")
}

func (s *panicNodesStub) ListQemuRrddata(ctx context.Context, node string, vmid string, params *nodes.ListQemuRrddataParams) (*nodes.ListQemuRrddataResponse, error) {
	panic("panicNodesStub.ListQemuRrddata: not expected")
}

func (s *panicNodesStub) UpdateQemuSendkey(ctx context.Context, node string, vmid string, params *nodes.UpdateQemuSendkeyParams) error {
	panic("panicNodesStub.UpdateQemuSendkey: not expected")
}

func (s *panicNodesStub) ListQemuSnapshot(ctx context.Context, node string, vmid string) (*nodes.ListQemuSnapshotResponse, error) {
	panic("panicNodesStub.ListQemuSnapshot: not expected")
}

func (s *panicNodesStub) CreateQemuSnapshot(ctx context.Context, node string, vmid string, params *nodes.CreateQemuSnapshotParams) (*nodes.CreateQemuSnapshotResponse, error) {
	panic("panicNodesStub.CreateQemuSnapshot: not expected")
}

func (s *panicNodesStub) DeleteQemuSnapshot(ctx context.Context, node string, vmid string, snapname string, params *nodes.DeleteQemuSnapshotParams) (*nodes.DeleteQemuSnapshotResponse, error) {
	panic("panicNodesStub.DeleteQemuSnapshot: not expected")
}

func (s *panicNodesStub) GetQemuSnapshot(ctx context.Context, node string, vmid string, snapname string) (*nodes.GetQemuSnapshotResponse, error) {
	panic("panicNodesStub.GetQemuSnapshot: not expected")
}

func (s *panicNodesStub) ListQemuSnapshotConfig(ctx context.Context, node string, vmid string, snapname string) (*nodes.ListQemuSnapshotConfigResponse, error) {
	panic("panicNodesStub.ListQemuSnapshotConfig: not expected")
}

func (s *panicNodesStub) UpdateQemuSnapshotConfig(ctx context.Context, node string, vmid string, snapname string, params *nodes.UpdateQemuSnapshotConfigParams) error {
	panic("panicNodesStub.UpdateQemuSnapshotConfig: not expected")
}

func (s *panicNodesStub) CreateQemuSnapshotRollback(ctx context.Context, node string, vmid string, snapname string, params *nodes.CreateQemuSnapshotRollbackParams) (*nodes.CreateQemuSnapshotRollbackResponse, error) {
	panic("panicNodesStub.CreateQemuSnapshotRollback: not expected")
}

func (s *panicNodesStub) CreateQemuSpiceproxy(ctx context.Context, node string, vmid string, params *nodes.CreateQemuSpiceproxyParams) (*nodes.CreateQemuSpiceproxyResponse, error) {
	panic("panicNodesStub.CreateQemuSpiceproxy: not expected")
}

func (s *panicNodesStub) ListQemuStatus(ctx context.Context, node string, vmid string) (*nodes.ListQemuStatusResponse, error) {
	panic("panicNodesStub.ListQemuStatus: not expected")
}

func (s *panicNodesStub) ListQemuStatusCurrent(ctx context.Context, node string, vmid string) (*nodes.ListQemuStatusCurrentResponse, error) {
	panic("panicNodesStub.ListQemuStatusCurrent: not expected")
}

func (s *panicNodesStub) CreateQemuStatusReboot(ctx context.Context, node string, vmid string, params *nodes.CreateQemuStatusRebootParams) (*nodes.CreateQemuStatusRebootResponse, error) {
	panic("panicNodesStub.CreateQemuStatusReboot: not expected")
}

func (s *panicNodesStub) CreateQemuStatusReset(ctx context.Context, node string, vmid string, params *nodes.CreateQemuStatusResetParams) (*nodes.CreateQemuStatusResetResponse, error) {
	panic("panicNodesStub.CreateQemuStatusReset: not expected")
}

func (s *panicNodesStub) CreateQemuStatusResume(ctx context.Context, node string, vmid string, params *nodes.CreateQemuStatusResumeParams) (*nodes.CreateQemuStatusResumeResponse, error) {
	panic("panicNodesStub.CreateQemuStatusResume: not expected")
}

func (s *panicNodesStub) CreateQemuStatusShutdown(ctx context.Context, node string, vmid string, params *nodes.CreateQemuStatusShutdownParams) (*nodes.CreateQemuStatusShutdownResponse, error) {
	panic("panicNodesStub.CreateQemuStatusShutdown: not expected")
}

func (s *panicNodesStub) CreateQemuStatusStart(ctx context.Context, node string, vmid string, params *nodes.CreateQemuStatusStartParams) (*nodes.CreateQemuStatusStartResponse, error) {
	panic("panicNodesStub.CreateQemuStatusStart: not expected")
}

func (s *panicNodesStub) CreateQemuStatusStop(ctx context.Context, node string, vmid string, params *nodes.CreateQemuStatusStopParams) (*nodes.CreateQemuStatusStopResponse, error) {
	panic("panicNodesStub.CreateQemuStatusStop: not expected")
}

func (s *panicNodesStub) CreateQemuStatusSuspend(ctx context.Context, node string, vmid string, params *nodes.CreateQemuStatusSuspendParams) (*nodes.CreateQemuStatusSuspendResponse, error) {
	panic("panicNodesStub.CreateQemuStatusSuspend: not expected")
}

func (s *panicNodesStub) CreateQemuTemplate(ctx context.Context, node string, vmid string, params *nodes.CreateQemuTemplateParams) (*nodes.CreateQemuTemplateResponse, error) {
	panic("panicNodesStub.CreateQemuTemplate: not expected")
}

func (s *panicNodesStub) CreateQemuTermproxy(ctx context.Context, node string, vmid string, params *nodes.CreateQemuTermproxyParams) (*nodes.CreateQemuTermproxyResponse, error) {
	panic("panicNodesStub.CreateQemuTermproxy: not expected")
}

func (s *panicNodesStub) UpdateQemuUnlink(ctx context.Context, node string, vmid string, params *nodes.UpdateQemuUnlinkParams) error {
	panic("panicNodesStub.UpdateQemuUnlink: not expected")
}

func (s *panicNodesStub) CreateQemuVncproxy(ctx context.Context, node string, vmid string, params *nodes.CreateQemuVncproxyParams) (*nodes.CreateQemuVncproxyResponse, error) {
	panic("panicNodesStub.CreateQemuVncproxy: not expected")
}

func (s *panicNodesStub) ListQemuVncwebsocket(ctx context.Context, node string, vmid string, params *nodes.ListQemuVncwebsocketParams) (*nodes.ListQemuVncwebsocketResponse, error) {
	panic("panicNodesStub.ListQemuVncwebsocket: not expected")
}

func (s *panicNodesStub) ListQueryOciRepoTags(ctx context.Context, node string, params *nodes.ListQueryOciRepoTagsParams) (*nodes.ListQueryOciRepoTagsResponse, error) {
	panic("panicNodesStub.ListQueryOciRepoTags: not expected")
}

func (s *panicNodesStub) ListQueryUrlMetadata(ctx context.Context, node string, params *nodes.ListQueryUrlMetadataParams) (*nodes.ListQueryUrlMetadataResponse, error) {
	panic("panicNodesStub.ListQueryUrlMetadata: not expected")
}

func (s *panicNodesStub) ListReplication(ctx context.Context, node string, params *nodes.ListReplicationParams) (*nodes.ListReplicationResponse, error) {
	panic("panicNodesStub.ListReplication: not expected")
}

func (s *panicNodesStub) GetReplication(ctx context.Context, node string, id string) (*nodes.GetReplicationResponse, error) {
	panic("panicNodesStub.GetReplication: not expected")
}

func (s *panicNodesStub) ListReplicationLog(ctx context.Context, node string, id string, params *nodes.ListReplicationLogParams) (*nodes.ListReplicationLogResponse, error) {
	panic("panicNodesStub.ListReplicationLog: not expected")
}

func (s *panicNodesStub) CreateReplicationScheduleNow(ctx context.Context, node string, id string) (*nodes.CreateReplicationScheduleNowResponse, error) {
	panic("panicNodesStub.CreateReplicationScheduleNow: not expected")
}

func (s *panicNodesStub) ListReplicationStatus(ctx context.Context, node string, id string) (*nodes.ListReplicationStatusResponse, error) {
	panic("panicNodesStub.ListReplicationStatus: not expected")
}

func (s *panicNodesStub) ListReport(ctx context.Context, node string) (*nodes.ListReportResponse, error) {
	panic("panicNodesStub.ListReport: not expected")
}

func (s *panicNodesStub) ListRrd(ctx context.Context, node string, params *nodes.ListRrdParams) (*nodes.ListRrdResponse, error) {
	panic("panicNodesStub.ListRrd: not expected")
}

func (s *panicNodesStub) ListRrddata(ctx context.Context, node string, params *nodes.ListRrddataParams) (*nodes.ListRrddataResponse, error) {
	panic("panicNodesStub.ListRrddata: not expected")
}

func (s *panicNodesStub) ListScan(ctx context.Context, node string) (*nodes.ListScanResponse, error) {
	panic("panicNodesStub.ListScan: not expected")
}

func (s *panicNodesStub) ListScanCifs(ctx context.Context, node string, params *nodes.ListScanCifsParams) (*nodes.ListScanCifsResponse, error) {
	panic("panicNodesStub.ListScanCifs: not expected")
}

func (s *panicNodesStub) ListScanIscsi(ctx context.Context, node string, params *nodes.ListScanIscsiParams) (*nodes.ListScanIscsiResponse, error) {
	panic("panicNodesStub.ListScanIscsi: not expected")
}

func (s *panicNodesStub) ListScanLvm(ctx context.Context, node string) (*nodes.ListScanLvmResponse, error) {
	panic("panicNodesStub.ListScanLvm: not expected")
}

func (s *panicNodesStub) ListScanLvmthin(ctx context.Context, node string, params *nodes.ListScanLvmthinParams) (*nodes.ListScanLvmthinResponse, error) {
	panic("panicNodesStub.ListScanLvmthin: not expected")
}

func (s *panicNodesStub) ListScanNfs(ctx context.Context, node string, params *nodes.ListScanNfsParams) (*nodes.ListScanNfsResponse, error) {
	panic("panicNodesStub.ListScanNfs: not expected")
}

func (s *panicNodesStub) ListScanPbs(ctx context.Context, node string, params *nodes.ListScanPbsParams) (*nodes.ListScanPbsResponse, error) {
	panic("panicNodesStub.ListScanPbs: not expected")
}

func (s *panicNodesStub) ListScanZfs(ctx context.Context, node string) (*nodes.ListScanZfsResponse, error) {
	panic("panicNodesStub.ListScanZfs: not expected")
}

func (s *panicNodesStub) ListSdn(ctx context.Context, node string) (*nodes.ListSdnResponse, error) {
	panic("panicNodesStub.ListSdn: not expected")
}

func (s *panicNodesStub) GetSdnFabrics(ctx context.Context, node string, fabric string) (*nodes.GetSdnFabricsResponse, error) {
	panic("panicNodesStub.GetSdnFabrics: not expected")
}

func (s *panicNodesStub) ListSdnFabricsInterfaces(ctx context.Context, node string, fabric string) (*nodes.ListSdnFabricsInterfacesResponse, error) {
	panic("panicNodesStub.ListSdnFabricsInterfaces: not expected")
}

func (s *panicNodesStub) ListSdnFabricsNeighbors(ctx context.Context, node string, fabric string) (*nodes.ListSdnFabricsNeighborsResponse, error) {
	panic("panicNodesStub.ListSdnFabricsNeighbors: not expected")
}

func (s *panicNodesStub) ListSdnFabricsRoutes(ctx context.Context, node string, fabric string) (*nodes.ListSdnFabricsRoutesResponse, error) {
	panic("panicNodesStub.ListSdnFabricsRoutes: not expected")
}

func (s *panicNodesStub) GetSdnVnets(ctx context.Context, node string, vnet string) (*nodes.GetSdnVnetsResponse, error) {
	panic("panicNodesStub.GetSdnVnets: not expected")
}

func (s *panicNodesStub) ListSdnVnetsMacVrf(ctx context.Context, node string, vnet string) (*nodes.ListSdnVnetsMacVrfResponse, error) {
	panic("panicNodesStub.ListSdnVnetsMacVrf: not expected")
}

func (s *panicNodesStub) ListSdnZones(ctx context.Context, node string) (*nodes.ListSdnZonesResponse, error) {
	panic("panicNodesStub.ListSdnZones: not expected")
}

func (s *panicNodesStub) GetSdnZones(ctx context.Context, node string, zone string) (*nodes.GetSdnZonesResponse, error) {
	panic("panicNodesStub.GetSdnZones: not expected")
}

func (s *panicNodesStub) ListSdnZonesBridges(ctx context.Context, node string, zone string) (*nodes.ListSdnZonesBridgesResponse, error) {
	panic("panicNodesStub.ListSdnZonesBridges: not expected")
}

func (s *panicNodesStub) ListSdnZonesContent(ctx context.Context, node string, zone string) (*nodes.ListSdnZonesContentResponse, error) {
	panic("panicNodesStub.ListSdnZonesContent: not expected")
}

func (s *panicNodesStub) ListSdnZonesIpVrf(ctx context.Context, node string, zone string) (*nodes.ListSdnZonesIpVrfResponse, error) {
	panic("panicNodesStub.ListSdnZonesIpVrf: not expected")
}

func (s *panicNodesStub) ListServices(ctx context.Context, node string) (*nodes.ListServicesResponse, error) {
	panic("panicNodesStub.ListServices: not expected")
}

func (s *panicNodesStub) GetServices(ctx context.Context, node string, service string) (*nodes.GetServicesResponse, error) {
	panic("panicNodesStub.GetServices: not expected")
}

func (s *panicNodesStub) CreateServicesReload(ctx context.Context, node string, service string) (*nodes.CreateServicesReloadResponse, error) {
	panic("panicNodesStub.CreateServicesReload: not expected")
}

func (s *panicNodesStub) CreateServicesRestart(ctx context.Context, node string, service string) (*nodes.CreateServicesRestartResponse, error) {
	panic("panicNodesStub.CreateServicesRestart: not expected")
}

func (s *panicNodesStub) CreateServicesStart(ctx context.Context, node string, service string) (*nodes.CreateServicesStartResponse, error) {
	panic("panicNodesStub.CreateServicesStart: not expected")
}

func (s *panicNodesStub) ListServicesState(ctx context.Context, node string, service string) (*nodes.ListServicesStateResponse, error) {
	panic("panicNodesStub.ListServicesState: not expected")
}

func (s *panicNodesStub) CreateServicesStop(ctx context.Context, node string, service string) (*nodes.CreateServicesStopResponse, error) {
	panic("panicNodesStub.CreateServicesStop: not expected")
}

func (s *panicNodesStub) CreateSpiceshell(ctx context.Context, node string, params *nodes.CreateSpiceshellParams) (*nodes.CreateSpiceshellResponse, error) {
	panic("panicNodesStub.CreateSpiceshell: not expected")
}

func (s *panicNodesStub) CreateStartall(ctx context.Context, node string, params *nodes.CreateStartallParams) (*nodes.CreateStartallResponse, error) {
	panic("panicNodesStub.CreateStartall: not expected")
}

func (s *panicNodesStub) ListStatus(ctx context.Context, node string) (*nodes.ListStatusResponse, error) {
	panic("panicNodesStub.ListStatus: not expected")
}

func (s *panicNodesStub) CreateStatus(ctx context.Context, node string, params *nodes.CreateStatusParams) error {
	panic("panicNodesStub.CreateStatus: not expected")
}

func (s *panicNodesStub) CreateStopall(ctx context.Context, node string, params *nodes.CreateStopallParams) (*nodes.CreateStopallResponse, error) {
	panic("panicNodesStub.CreateStopall: not expected")
}

func (s *panicNodesStub) ListStorage(ctx context.Context, node string, params *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
	panic("panicNodesStub.ListStorage: not expected")
}

func (s *panicNodesStub) GetStorage(ctx context.Context, node string, storage string) (*nodes.GetStorageResponse, error) {
	panic("panicNodesStub.GetStorage: not expected")
}

func (s *panicNodesStub) ListStorageContent(ctx context.Context, node string, storage string, params *nodes.ListStorageContentParams) (*nodes.ListStorageContentResponse, error) {
	panic("panicNodesStub.ListStorageContent: not expected")
}

func (s *panicNodesStub) CreateStorageContent(ctx context.Context, node string, storage string, params *nodes.CreateStorageContentParams) (*nodes.CreateStorageContentResponse, error) {
	panic("panicNodesStub.CreateStorageContent: not expected")
}

func (s *panicNodesStub) DeleteStorageContent(ctx context.Context, node string, storage string, volume string, params *nodes.DeleteStorageContentParams) (*nodes.DeleteStorageContentResponse, error) {
	panic("panicNodesStub.DeleteStorageContent: not expected")
}

func (s *panicNodesStub) GetStorageContent(ctx context.Context, node string, storage string, volume string) (*nodes.GetStorageContentResponse, error) {
	panic("panicNodesStub.GetStorageContent: not expected")
}

func (s *panicNodesStub) CreateStorageContent2(ctx context.Context, node string, storage string, volume string, params *nodes.CreateStorageContent2Params) (*nodes.CreateStorageContent2Response, error) {
	panic("panicNodesStub.CreateStorageContent2: not expected")
}

func (s *panicNodesStub) UpdateStorageContent(ctx context.Context, node string, storage string, volume string, params *nodes.UpdateStorageContentParams) error {
	panic("panicNodesStub.UpdateStorageContent: not expected")
}

func (s *panicNodesStub) CreateStorageDownloadUrl(ctx context.Context, node string, storage string, params *nodes.CreateStorageDownloadUrlParams) (*nodes.CreateStorageDownloadUrlResponse, error) {
	panic("panicNodesStub.CreateStorageDownloadUrl: not expected")
}

func (s *panicNodesStub) ListStorageFileRestoreDownload(ctx context.Context, node string, storage string, params *nodes.ListStorageFileRestoreDownloadParams) (*nodes.ListStorageFileRestoreDownloadResponse, error) {
	panic("panicNodesStub.ListStorageFileRestoreDownload: not expected")
}

func (s *panicNodesStub) ListStorageFileRestoreList(ctx context.Context, node string, storage string, params *nodes.ListStorageFileRestoreListParams) (*nodes.ListStorageFileRestoreListResponse, error) {
	panic("panicNodesStub.ListStorageFileRestoreList: not expected")
}

func (s *panicNodesStub) ListStorageIdentity(ctx context.Context, node string, storage string) (*nodes.ListStorageIdentityResponse, error) {
	panic("panicNodesStub.ListStorageIdentity: not expected")
}

func (s *panicNodesStub) ListStorageImportMetadata(ctx context.Context, node string, storage string, params *nodes.ListStorageImportMetadataParams) (*nodes.ListStorageImportMetadataResponse, error) {
	panic("panicNodesStub.ListStorageImportMetadata: not expected")
}

func (s *panicNodesStub) CreateStorageOciRegistryPull(ctx context.Context, node string, storage string, params *nodes.CreateStorageOciRegistryPullParams) (*nodes.CreateStorageOciRegistryPullResponse, error) {
	panic("panicNodesStub.CreateStorageOciRegistryPull: not expected")
}

func (s *panicNodesStub) DeleteStoragePrunebackups(ctx context.Context, node string, storage string, params *nodes.DeleteStoragePrunebackupsParams) (*nodes.DeleteStoragePrunebackupsResponse, error) {
	panic("panicNodesStub.DeleteStoragePrunebackups: not expected")
}

func (s *panicNodesStub) ListStoragePrunebackups(ctx context.Context, node string, storage string, params *nodes.ListStoragePrunebackupsParams) (*nodes.ListStoragePrunebackupsResponse, error) {
	panic("panicNodesStub.ListStoragePrunebackups: not expected")
}

func (s *panicNodesStub) ListStorageRrd(ctx context.Context, node string, storage string, params *nodes.ListStorageRrdParams) (*nodes.ListStorageRrdResponse, error) {
	panic("panicNodesStub.ListStorageRrd: not expected")
}

func (s *panicNodesStub) ListStorageRrddata(ctx context.Context, node string, storage string, params *nodes.ListStorageRrddataParams) (*nodes.ListStorageRrddataResponse, error) {
	panic("panicNodesStub.ListStorageRrddata: not expected")
}

func (s *panicNodesStub) ListStorageStatus(ctx context.Context, node string, storage string) (*nodes.ListStorageStatusResponse, error) {
	panic("panicNodesStub.ListStorageStatus: not expected")
}

func (s *panicNodesStub) CreateStorageUpload(ctx context.Context, node string, storage string, params *nodes.CreateStorageUploadParams) (*nodes.CreateStorageUploadResponse, error) {
	panic("panicNodesStub.CreateStorageUpload: not expected")
}

func (s *panicNodesStub) DeleteSubscription(ctx context.Context, node string) error {
	panic("panicNodesStub.DeleteSubscription: not expected")
}

func (s *panicNodesStub) ListSubscription(ctx context.Context, node string) (*nodes.ListSubscriptionResponse, error) {
	panic("panicNodesStub.ListSubscription: not expected")
}

func (s *panicNodesStub) CreateSubscription(ctx context.Context, node string, params *nodes.CreateSubscriptionParams) error {
	panic("panicNodesStub.CreateSubscription: not expected")
}

func (s *panicNodesStub) UpdateSubscription(ctx context.Context, node string, params *nodes.UpdateSubscriptionParams) error {
	panic("panicNodesStub.UpdateSubscription: not expected")
}

func (s *panicNodesStub) CreateSuspendall(ctx context.Context, node string, params *nodes.CreateSuspendallParams) (*nodes.CreateSuspendallResponse, error) {
	panic("panicNodesStub.CreateSuspendall: not expected")
}

func (s *panicNodesStub) ListSyslog(ctx context.Context, node string, params *nodes.ListSyslogParams) (*nodes.ListSyslogResponse, error) {
	panic("panicNodesStub.ListSyslog: not expected")
}

func (s *panicNodesStub) ListTasks(ctx context.Context, node string, params *nodes.ListTasksParams) (*nodes.ListTasksResponse, error) {
	panic("panicNodesStub.ListTasks: not expected")
}

func (s *panicNodesStub) DeleteTasks(ctx context.Context, node string, upid string) error {
	panic("panicNodesStub.DeleteTasks: not expected")
}

func (s *panicNodesStub) GetTasks(ctx context.Context, node string, upid string) (*nodes.GetTasksResponse, error) {
	panic("panicNodesStub.GetTasks: not expected")
}

func (s *panicNodesStub) ListTasksLog(ctx context.Context, node string, upid string, params *nodes.ListTasksLogParams) (*nodes.ListTasksLogResponse, error) {
	panic("panicNodesStub.ListTasksLog: not expected")
}

func (s *panicNodesStub) ListTasksStatus(ctx context.Context, node string, upid string) (*nodes.ListTasksStatusResponse, error) {
	panic("panicNodesStub.ListTasksStatus: not expected")
}

func (s *panicNodesStub) CreateTermproxy(ctx context.Context, node string, params *nodes.CreateTermproxyParams) (*nodes.CreateTermproxyResponse, error) {
	panic("panicNodesStub.CreateTermproxy: not expected")
}

func (s *panicNodesStub) ListTime(ctx context.Context, node string) (*nodes.ListTimeResponse, error) {
	panic("panicNodesStub.ListTime: not expected")
}

func (s *panicNodesStub) UpdateTime(ctx context.Context, node string, params *nodes.UpdateTimeParams) error {
	panic("panicNodesStub.UpdateTime: not expected")
}

func (s *panicNodesStub) ListVersion(ctx context.Context, node string) (*nodes.ListVersionResponse, error) {
	panic("panicNodesStub.ListVersion: not expected")
}

func (s *panicNodesStub) CreateVncshell(ctx context.Context, node string, params *nodes.CreateVncshellParams) (*nodes.CreateVncshellResponse, error) {
	panic("panicNodesStub.CreateVncshell: not expected")
}

func (s *panicNodesStub) ListVncwebsocket(ctx context.Context, node string, params *nodes.ListVncwebsocketParams) (*nodes.ListVncwebsocketResponse, error) {
	panic("panicNodesStub.ListVncwebsocket: not expected")
}

func (s *panicNodesStub) CreateVzdump(ctx context.Context, node string, params *nodes.CreateVzdumpParams) (*nodes.CreateVzdumpResponse, error) {
	panic("panicNodesStub.CreateVzdump: not expected")
}

func (s *panicNodesStub) ListVzdumpDefaults(ctx context.Context, node string, params *nodes.ListVzdumpDefaultsParams) (*nodes.ListVzdumpDefaultsResponse, error) {
	panic("panicNodesStub.ListVzdumpDefaults: not expected")
}

func (s *panicNodesStub) ListVzdumpExtractconfig(ctx context.Context, node string, params *nodes.ListVzdumpExtractconfigParams) (*nodes.ListVzdumpExtractconfigResponse, error) {
	panic("panicNodesStub.ListVzdumpExtractconfig: not expected")
}

func (s *panicNodesStub) CreateWakeonlan(ctx context.Context, node string) (*nodes.CreateWakeonlanResponse, error) {
	panic("panicNodesStub.CreateWakeonlan: not expected")
}
