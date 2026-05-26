// Package handlers_test — panic stub for cluster.Service.
//
// panicClusterStub satisfies the full cluster.Service interface; every method
// panics with a message naming the method and how to opt in.
package handlers_test

import (
	"context"

	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
)

// panicClusterStub satisfies cluster.Service; every method panics on call.
type panicClusterStub struct{}

// Compile-time interface satisfaction check.
var _ cluster.Service = (*panicClusterStub)(nil)

func (s *panicClusterStub) ListCluster(_ context.Context) (*cluster.ListClusterResponse, error) {
	panic("panicClusterStub.ListCluster: not configured for SDN test; opt in by setting ListClusterFn")
}

func (s *panicClusterStub) ListAcme(_ context.Context) (*cluster.ListAcmeResponse, error) {
	panic("panicClusterStub.ListAcme: not configured for SDN test; opt in by setting ListAcmeFn")
}

func (s *panicClusterStub) ListAcmeAccount(_ context.Context) (*cluster.ListAcmeAccountResponse, error) {
	panic("panicClusterStub.ListAcmeAccount: not configured for SDN test; opt in by setting ListAcmeAccountFn")
}

func (s *panicClusterStub) CreateAcmeAccount(_ context.Context, _ *cluster.CreateAcmeAccountParams) (*cluster.CreateAcmeAccountResponse, error) {
	panic("panicClusterStub.CreateAcmeAccount: not configured for SDN test; opt in by setting CreateAcmeAccountFn")
}

func (s *panicClusterStub) DeleteAcmeAccount(_ context.Context, _ string) (*cluster.DeleteAcmeAccountResponse, error) {
	panic("panicClusterStub.DeleteAcmeAccount: not configured for SDN test; opt in by setting DeleteAcmeAccountFn")
}

func (s *panicClusterStub) GetAcmeAccount(_ context.Context, _ string) (*cluster.GetAcmeAccountResponse, error) {
	panic("panicClusterStub.GetAcmeAccount: not configured for SDN test; opt in by setting GetAcmeAccountFn")
}

func (s *panicClusterStub) UpdateAcmeAccount(_ context.Context, _ string, _ *cluster.UpdateAcmeAccountParams) (*cluster.UpdateAcmeAccountResponse, error) {
	panic("panicClusterStub.UpdateAcmeAccount: not configured for SDN test; opt in by setting UpdateAcmeAccountFn")
}

func (s *panicClusterStub) ListAcmeChallengeSchema(_ context.Context) (*cluster.ListAcmeChallengeSchemaResponse, error) {
	panic("panicClusterStub.ListAcmeChallengeSchema: not configured for SDN test; opt in by setting ListAcmeChallengeSchemaFn")
}

func (s *panicClusterStub) ListAcmeDirectories(_ context.Context) (*cluster.ListAcmeDirectoriesResponse, error) {
	panic("panicClusterStub.ListAcmeDirectories: not configured for SDN test; opt in by setting ListAcmeDirectoriesFn")
}

func (s *panicClusterStub) ListAcmeMeta(_ context.Context, _ *cluster.ListAcmeMetaParams) (*cluster.ListAcmeMetaResponse, error) {
	panic("panicClusterStub.ListAcmeMeta: not configured for SDN test; opt in by setting ListAcmeMetaFn")
}

func (s *panicClusterStub) ListAcmePlugins(_ context.Context, _ *cluster.ListAcmePluginsParams) (*cluster.ListAcmePluginsResponse, error) {
	panic("panicClusterStub.ListAcmePlugins: not configured for SDN test; opt in by setting ListAcmePluginsFn")
}

func (s *panicClusterStub) CreateAcmePlugins(_ context.Context, _ *cluster.CreateAcmePluginsParams) error {
	panic("panicClusterStub.CreateAcmePlugins: not configured for SDN test; opt in by setting CreateAcmePluginsFn")
}

func (s *panicClusterStub) DeleteAcmePlugins(_ context.Context, _ string) error {
	panic("panicClusterStub.DeleteAcmePlugins: not configured for SDN test; opt in by setting DeleteAcmePluginsFn")
}

func (s *panicClusterStub) GetAcmePlugins(_ context.Context, _ string) (*cluster.GetAcmePluginsResponse, error) {
	panic("panicClusterStub.GetAcmePlugins: not configured for SDN test; opt in by setting GetAcmePluginsFn")
}

func (s *panicClusterStub) UpdateAcmePlugins(_ context.Context, _ string, _ *cluster.UpdateAcmePluginsParams) error {
	panic("panicClusterStub.UpdateAcmePlugins: not configured for SDN test; opt in by setting UpdateAcmePluginsFn")
}

func (s *panicClusterStub) ListAcmeTos(_ context.Context, _ *cluster.ListAcmeTosParams) (*cluster.ListAcmeTosResponse, error) {
	panic("panicClusterStub.ListAcmeTos: not configured for SDN test; opt in by setting ListAcmeTosFn")
}

func (s *panicClusterStub) ListBackup(_ context.Context) (*cluster.ListBackupResponse, error) {
	panic("panicClusterStub.ListBackup: not configured for SDN test; opt in by setting ListBackupFn")
}

func (s *panicClusterStub) CreateBackup(_ context.Context, _ *cluster.CreateBackupParams) error {
	panic("panicClusterStub.CreateBackup: not configured for SDN test; opt in by setting CreateBackupFn")
}

func (s *panicClusterStub) ListBackupInfo(_ context.Context) (*cluster.ListBackupInfoResponse, error) {
	panic("panicClusterStub.ListBackupInfo: not configured for SDN test; opt in by setting ListBackupInfoFn")
}

func (s *panicClusterStub) ListBackupInfoNotBackedUp(_ context.Context) (*cluster.ListBackupInfoNotBackedUpResponse, error) {
	panic("panicClusterStub.ListBackupInfoNotBackedUp: not configured for SDN test; opt in by setting ListBackupInfoNotBackedUpFn")
}

func (s *panicClusterStub) DeleteBackup(_ context.Context, _ string) error {
	panic("panicClusterStub.DeleteBackup: not configured for SDN test; opt in by setting DeleteBackupFn")
}

func (s *panicClusterStub) GetBackup(_ context.Context, _ string) (*cluster.GetBackupResponse, error) {
	panic("panicClusterStub.GetBackup: not configured for SDN test; opt in by setting GetBackupFn")
}

func (s *panicClusterStub) UpdateBackup(_ context.Context, _ string, _ *cluster.UpdateBackupParams) error {
	panic("panicClusterStub.UpdateBackup: not configured for SDN test; opt in by setting UpdateBackupFn")
}

func (s *panicClusterStub) ListBackupIncludedVolumes(_ context.Context, _ string) (*cluster.ListBackupIncludedVolumesResponse, error) {
	panic("panicClusterStub.ListBackupIncludedVolumes: not configured for SDN test; opt in by setting ListBackupIncludedVolumesFn")
}

func (s *panicClusterStub) ListBulkAction(_ context.Context) (*cluster.ListBulkActionResponse, error) {
	panic("panicClusterStub.ListBulkAction: not configured for SDN test; opt in by setting ListBulkActionFn")
}

func (s *panicClusterStub) ListBulkActionGuest(_ context.Context) (*cluster.ListBulkActionGuestResponse, error) {
	panic("panicClusterStub.ListBulkActionGuest: not configured for SDN test; opt in by setting ListBulkActionGuestFn")
}

func (s *panicClusterStub) CreateBulkActionGuestMigrate(_ context.Context, _ *cluster.CreateBulkActionGuestMigrateParams) (*cluster.CreateBulkActionGuestMigrateResponse, error) {
	panic("panicClusterStub.CreateBulkActionGuestMigrate: not configured for SDN test; opt in by setting CreateBulkActionGuestMigrateFn")
}

func (s *panicClusterStub) CreateBulkActionGuestShutdown(_ context.Context, _ *cluster.CreateBulkActionGuestShutdownParams) (*cluster.CreateBulkActionGuestShutdownResponse, error) {
	panic("panicClusterStub.CreateBulkActionGuestShutdown: not configured for SDN test; opt in by setting CreateBulkActionGuestShutdownFn")
}

func (s *panicClusterStub) CreateBulkActionGuestStart(_ context.Context, _ *cluster.CreateBulkActionGuestStartParams) (*cluster.CreateBulkActionGuestStartResponse, error) {
	panic("panicClusterStub.CreateBulkActionGuestStart: not configured for SDN test; opt in by setting CreateBulkActionGuestStartFn")
}

func (s *panicClusterStub) CreateBulkActionGuestSuspend(_ context.Context, _ *cluster.CreateBulkActionGuestSuspendParams) (*cluster.CreateBulkActionGuestSuspendResponse, error) {
	panic("panicClusterStub.CreateBulkActionGuestSuspend: not configured for SDN test; opt in by setting CreateBulkActionGuestSuspendFn")
}

func (s *panicClusterStub) ListCeph(_ context.Context) (*cluster.ListCephResponse, error) {
	panic("panicClusterStub.ListCeph: not configured for SDN test; opt in by setting ListCephFn")
}

func (s *panicClusterStub) ListCephFlags(_ context.Context) (*cluster.ListCephFlagsResponse, error) {
	panic("panicClusterStub.ListCephFlags: not configured for SDN test; opt in by setting ListCephFlagsFn")
}

func (s *panicClusterStub) UpdateCephFlags(_ context.Context, _ *cluster.UpdateCephFlagsParams) (*cluster.UpdateCephFlagsResponse, error) {
	panic("panicClusterStub.UpdateCephFlags: not configured for SDN test; opt in by setting UpdateCephFlagsFn")
}

func (s *panicClusterStub) GetCephFlags(_ context.Context, _ string) (*cluster.GetCephFlagsResponse, error) {
	panic("panicClusterStub.GetCephFlags: not configured for SDN test; opt in by setting GetCephFlagsFn")
}

func (s *panicClusterStub) UpdateCephFlags2(_ context.Context, _ string, _ *cluster.UpdateCephFlags2Params) error {
	panic("panicClusterStub.UpdateCephFlags2: not configured for SDN test; opt in by setting UpdateCephFlags2Fn")
}

func (s *panicClusterStub) ListCephMetadata(_ context.Context, _ *cluster.ListCephMetadataParams) (*cluster.ListCephMetadataResponse, error) {
	panic("panicClusterStub.ListCephMetadata: not configured for SDN test; opt in by setting ListCephMetadataFn")
}

func (s *panicClusterStub) ListCephStatus(_ context.Context) (*cluster.ListCephStatusResponse, error) {
	panic("panicClusterStub.ListCephStatus: not configured for SDN test; opt in by setting ListCephStatusFn")
}

func (s *panicClusterStub) ListConfig(_ context.Context) (*cluster.ListConfigResponse, error) {
	panic("panicClusterStub.ListConfig: not configured for SDN test; opt in by setting ListConfigFn")
}

func (s *panicClusterStub) CreateConfig(_ context.Context, _ *cluster.CreateConfigParams) (*cluster.CreateConfigResponse, error) {
	panic("panicClusterStub.CreateConfig: not configured for SDN test; opt in by setting CreateConfigFn")
}

func (s *panicClusterStub) ListConfigApiversion(_ context.Context) (*cluster.ListConfigApiversionResponse, error) {
	panic("panicClusterStub.ListConfigApiversion: not configured for SDN test; opt in by setting ListConfigApiversionFn")
}

func (s *panicClusterStub) ListConfigJoin(_ context.Context, _ *cluster.ListConfigJoinParams) (*cluster.ListConfigJoinResponse, error) {
	panic("panicClusterStub.ListConfigJoin: not configured for SDN test; opt in by setting ListConfigJoinFn")
}

func (s *panicClusterStub) CreateConfigJoin(_ context.Context, _ *cluster.CreateConfigJoinParams) (*cluster.CreateConfigJoinResponse, error) {
	panic("panicClusterStub.CreateConfigJoin: not configured for SDN test; opt in by setting CreateConfigJoinFn")
}

func (s *panicClusterStub) ListConfigNodes(_ context.Context) (*cluster.ListConfigNodesResponse, error) {
	panic("panicClusterStub.ListConfigNodes: not configured for SDN test; opt in by setting ListConfigNodesFn")
}

func (s *panicClusterStub) DeleteConfigNodes(_ context.Context, _ string) error {
	panic("panicClusterStub.DeleteConfigNodes: not configured for SDN test; opt in by setting DeleteConfigNodesFn")
}

func (s *panicClusterStub) CreateConfigNodes(_ context.Context, _ string, _ *cluster.CreateConfigNodesParams) (*cluster.CreateConfigNodesResponse, error) {
	panic("panicClusterStub.CreateConfigNodes: not configured for SDN test; opt in by setting CreateConfigNodesFn")
}

func (s *panicClusterStub) ListConfigQdevice(_ context.Context) (*cluster.ListConfigQdeviceResponse, error) {
	panic("panicClusterStub.ListConfigQdevice: not configured for SDN test; opt in by setting ListConfigQdeviceFn")
}

func (s *panicClusterStub) ListConfigTotem(_ context.Context) (*cluster.ListConfigTotemResponse, error) {
	panic("panicClusterStub.ListConfigTotem: not configured for SDN test; opt in by setting ListConfigTotemFn")
}

func (s *panicClusterStub) ListFirewall(_ context.Context) (*cluster.ListFirewallResponse, error) {
	panic("panicClusterStub.ListFirewall: not configured for SDN test; opt in by setting ListFirewallFn")
}

func (s *panicClusterStub) ListFirewallAliases(_ context.Context) (*cluster.ListFirewallAliasesResponse, error) {
	panic("panicClusterStub.ListFirewallAliases: not configured for SDN test; opt in by setting ListFirewallAliasesFn")
}

func (s *panicClusterStub) CreateFirewallAliases(_ context.Context, _ *cluster.CreateFirewallAliasesParams) error {
	panic("panicClusterStub.CreateFirewallAliases: not configured for SDN test; opt in by setting CreateFirewallAliasesFn")
}

func (s *panicClusterStub) DeleteFirewallAliases(_ context.Context, _ string, _ *cluster.DeleteFirewallAliasesParams) error {
	panic("panicClusterStub.DeleteFirewallAliases: not configured for SDN test; opt in by setting DeleteFirewallAliasesFn")
}

func (s *panicClusterStub) GetFirewallAliases(_ context.Context, _ string) (*cluster.GetFirewallAliasesResponse, error) {
	panic("panicClusterStub.GetFirewallAliases: not configured for SDN test; opt in by setting GetFirewallAliasesFn")
}

func (s *panicClusterStub) UpdateFirewallAliases(_ context.Context, _ string, _ *cluster.UpdateFirewallAliasesParams) error {
	panic("panicClusterStub.UpdateFirewallAliases: not configured for SDN test; opt in by setting UpdateFirewallAliasesFn")
}

func (s *panicClusterStub) ListFirewallGroups(_ context.Context) (*cluster.ListFirewallGroupsResponse, error) {
	panic("panicClusterStub.ListFirewallGroups: not configured for SDN test; opt in by setting ListFirewallGroupsFn")
}

func (s *panicClusterStub) CreateFirewallGroups(_ context.Context, _ *cluster.CreateFirewallGroupsParams) error {
	panic("panicClusterStub.CreateFirewallGroups: not configured for SDN test; opt in by setting CreateFirewallGroupsFn")
}

func (s *panicClusterStub) DeleteFirewallGroups(_ context.Context, _ string) error {
	panic("panicClusterStub.DeleteFirewallGroups: not configured for SDN test; opt in by setting DeleteFirewallGroupsFn")
}

func (s *panicClusterStub) GetFirewallGroups(_ context.Context, _ string) (*cluster.GetFirewallGroupsResponse, error) {
	panic("panicClusterStub.GetFirewallGroups: not configured for SDN test; opt in by setting GetFirewallGroupsFn")
}

func (s *panicClusterStub) CreateFirewallGroups2(_ context.Context, _ string, _ *cluster.CreateFirewallGroups2Params) error {
	panic("panicClusterStub.CreateFirewallGroups2: not configured for SDN test; opt in by setting CreateFirewallGroups2Fn")
}

func (s *panicClusterStub) DeleteFirewallGroups2(_ context.Context, _ string, _ string, _ *cluster.DeleteFirewallGroups2Params) error {
	panic("panicClusterStub.DeleteFirewallGroups2: not configured for SDN test; opt in by setting DeleteFirewallGroups2Fn")
}

func (s *panicClusterStub) GetFirewallGroups2(_ context.Context, _ string, _ string) (*cluster.GetFirewallGroups2Response, error) {
	panic("panicClusterStub.GetFirewallGroups2: not configured for SDN test; opt in by setting GetFirewallGroups2Fn")
}

func (s *panicClusterStub) UpdateFirewallGroups(_ context.Context, _ string, _ string, _ *cluster.UpdateFirewallGroupsParams) error {
	panic("panicClusterStub.UpdateFirewallGroups: not configured for SDN test; opt in by setting UpdateFirewallGroupsFn")
}

func (s *panicClusterStub) ListFirewallIpset(_ context.Context) (*cluster.ListFirewallIpsetResponse, error) {
	panic("panicClusterStub.ListFirewallIpset: not configured for SDN test; opt in by setting ListFirewallIpsetFn")
}

func (s *panicClusterStub) CreateFirewallIpset(_ context.Context, _ *cluster.CreateFirewallIpsetParams) error {
	panic("panicClusterStub.CreateFirewallIpset: not configured for SDN test; opt in by setting CreateFirewallIpsetFn")
}

func (s *panicClusterStub) DeleteFirewallIpset(_ context.Context, _ string, _ *cluster.DeleteFirewallIpsetParams) error {
	panic("panicClusterStub.DeleteFirewallIpset: not configured for SDN test; opt in by setting DeleteFirewallIpsetFn")
}

func (s *panicClusterStub) GetFirewallIpset(_ context.Context, _ string) (*cluster.GetFirewallIpsetResponse, error) {
	panic("panicClusterStub.GetFirewallIpset: not configured for SDN test; opt in by setting GetFirewallIpsetFn")
}

func (s *panicClusterStub) CreateFirewallIpset2(_ context.Context, _ string, _ *cluster.CreateFirewallIpset2Params) error {
	panic("panicClusterStub.CreateFirewallIpset2: not configured for SDN test; opt in by setting CreateFirewallIpset2Fn")
}

func (s *panicClusterStub) DeleteFirewallIpset2(_ context.Context, _ string, _ string, _ *cluster.DeleteFirewallIpset2Params) error {
	panic("panicClusterStub.DeleteFirewallIpset2: not configured for SDN test; opt in by setting DeleteFirewallIpset2Fn")
}

func (s *panicClusterStub) GetFirewallIpset2(_ context.Context, _ string, _ string) (*cluster.GetFirewallIpset2Response, error) {
	panic("panicClusterStub.GetFirewallIpset2: not configured for SDN test; opt in by setting GetFirewallIpset2Fn")
}

func (s *panicClusterStub) UpdateFirewallIpset(_ context.Context, _ string, _ string, _ *cluster.UpdateFirewallIpsetParams) error {
	panic("panicClusterStub.UpdateFirewallIpset: not configured for SDN test; opt in by setting UpdateFirewallIpsetFn")
}

func (s *panicClusterStub) ListFirewallMacros(_ context.Context) (*cluster.ListFirewallMacrosResponse, error) {
	panic("panicClusterStub.ListFirewallMacros: not configured for SDN test; opt in by setting ListFirewallMacrosFn")
}

func (s *panicClusterStub) ListFirewallOptions(_ context.Context) (*cluster.ListFirewallOptionsResponse, error) {
	panic("panicClusterStub.ListFirewallOptions: not configured for SDN test; opt in by setting ListFirewallOptionsFn")
}

func (s *panicClusterStub) UpdateFirewallOptions(_ context.Context, _ *cluster.UpdateFirewallOptionsParams) error {
	panic("panicClusterStub.UpdateFirewallOptions: not configured for SDN test; opt in by setting UpdateFirewallOptionsFn")
}

func (s *panicClusterStub) ListFirewallRefs(_ context.Context, _ *cluster.ListFirewallRefsParams) (*cluster.ListFirewallRefsResponse, error) {
	panic("panicClusterStub.ListFirewallRefs: not configured for SDN test; opt in by setting ListFirewallRefsFn")
}

func (s *panicClusterStub) ListFirewallRules(_ context.Context) (*cluster.ListFirewallRulesResponse, error) {
	panic("panicClusterStub.ListFirewallRules: not configured for SDN test; opt in by setting ListFirewallRulesFn")
}

func (s *panicClusterStub) CreateFirewallRules(_ context.Context, _ *cluster.CreateFirewallRulesParams) error {
	panic("panicClusterStub.CreateFirewallRules: not configured for SDN test; opt in by setting CreateFirewallRulesFn")
}

func (s *panicClusterStub) DeleteFirewallRules(_ context.Context, _ string, _ *cluster.DeleteFirewallRulesParams) error {
	panic("panicClusterStub.DeleteFirewallRules: not configured for SDN test; opt in by setting DeleteFirewallRulesFn")
}

func (s *panicClusterStub) GetFirewallRules(_ context.Context, _ string) (*cluster.GetFirewallRulesResponse, error) {
	panic("panicClusterStub.GetFirewallRules: not configured for SDN test; opt in by setting GetFirewallRulesFn")
}

func (s *panicClusterStub) UpdateFirewallRules(_ context.Context, _ string, _ *cluster.UpdateFirewallRulesParams) error {
	panic("panicClusterStub.UpdateFirewallRules: not configured for SDN test; opt in by setting UpdateFirewallRulesFn")
}

func (s *panicClusterStub) ListHa(_ context.Context) (*cluster.ListHaResponse, error) {
	panic("panicClusterStub.ListHa: not configured for SDN test; opt in by setting ListHaFn")
}

func (s *panicClusterStub) ListHaGroups(_ context.Context) (*cluster.ListHaGroupsResponse, error) {
	panic("panicClusterStub.ListHaGroups: not configured for SDN test; opt in by setting ListHaGroupsFn")
}

func (s *panicClusterStub) CreateHaGroups(_ context.Context, _ *cluster.CreateHaGroupsParams) error {
	panic("panicClusterStub.CreateHaGroups: not configured for SDN test; opt in by setting CreateHaGroupsFn")
}

func (s *panicClusterStub) DeleteHaGroups(_ context.Context, _ string) error {
	panic("panicClusterStub.DeleteHaGroups: not configured for SDN test; opt in by setting DeleteHaGroupsFn")
}

func (s *panicClusterStub) GetHaGroups(_ context.Context, _ string) (*cluster.GetHaGroupsResponse, error) {
	panic("panicClusterStub.GetHaGroups: not configured for SDN test; opt in by setting GetHaGroupsFn")
}

func (s *panicClusterStub) UpdateHaGroups(_ context.Context, _ string, _ *cluster.UpdateHaGroupsParams) error {
	panic("panicClusterStub.UpdateHaGroups: not configured for SDN test; opt in by setting UpdateHaGroupsFn")
}

func (s *panicClusterStub) ListHaResources(_ context.Context, _ *cluster.ListHaResourcesParams) (*cluster.ListHaResourcesResponse, error) {
	panic("panicClusterStub.ListHaResources: not configured for SDN test; opt in by setting ListHaResourcesFn")
}

func (s *panicClusterStub) CreateHaResources(_ context.Context, _ *cluster.CreateHaResourcesParams) error {
	panic("panicClusterStub.CreateHaResources: not configured for SDN test; opt in by setting CreateHaResourcesFn")
}

func (s *panicClusterStub) DeleteHaResources(_ context.Context, _ string, _ *cluster.DeleteHaResourcesParams) error {
	panic("panicClusterStub.DeleteHaResources: not configured for SDN test; opt in by setting DeleteHaResourcesFn")
}

func (s *panicClusterStub) GetHaResources(_ context.Context, _ string) (*cluster.GetHaResourcesResponse, error) {
	panic("panicClusterStub.GetHaResources: not configured for SDN test; opt in by setting GetHaResourcesFn")
}

func (s *panicClusterStub) UpdateHaResources(_ context.Context, _ string, _ *cluster.UpdateHaResourcesParams) error {
	panic("panicClusterStub.UpdateHaResources: not configured for SDN test; opt in by setting UpdateHaResourcesFn")
}

func (s *panicClusterStub) CreateHaResourcesMigrate(_ context.Context, _ string, _ *cluster.CreateHaResourcesMigrateParams) (*cluster.CreateHaResourcesMigrateResponse, error) {
	panic("panicClusterStub.CreateHaResourcesMigrate: not configured for SDN test; opt in by setting CreateHaResourcesMigrateFn")
}

func (s *panicClusterStub) CreateHaResourcesRelocate(_ context.Context, _ string, _ *cluster.CreateHaResourcesRelocateParams) (*cluster.CreateHaResourcesRelocateResponse, error) {
	panic("panicClusterStub.CreateHaResourcesRelocate: not configured for SDN test; opt in by setting CreateHaResourcesRelocateFn")
}

func (s *panicClusterStub) ListHaRules(_ context.Context, _ *cluster.ListHaRulesParams) (*cluster.ListHaRulesResponse, error) {
	panic("panicClusterStub.ListHaRules: not configured for SDN test; opt in by setting ListHaRulesFn")
}

func (s *panicClusterStub) CreateHaRules(_ context.Context, _ *cluster.CreateHaRulesParams) error {
	panic("panicClusterStub.CreateHaRules: not configured for SDN test; opt in by setting CreateHaRulesFn")
}

func (s *panicClusterStub) DeleteHaRules(_ context.Context, _ string) error {
	panic("panicClusterStub.DeleteHaRules: not configured for SDN test; opt in by setting DeleteHaRulesFn")
}

func (s *panicClusterStub) GetHaRules(_ context.Context, _ string) (*cluster.GetHaRulesResponse, error) {
	panic("panicClusterStub.GetHaRules: not configured for SDN test; opt in by setting GetHaRulesFn")
}

func (s *panicClusterStub) UpdateHaRules(_ context.Context, _ string, _ *cluster.UpdateHaRulesParams) error {
	panic("panicClusterStub.UpdateHaRules: not configured for SDN test; opt in by setting UpdateHaRulesFn")
}

func (s *panicClusterStub) ListHaStatus(_ context.Context) (*cluster.ListHaStatusResponse, error) {
	panic("panicClusterStub.ListHaStatus: not configured for SDN test; opt in by setting ListHaStatusFn")
}

func (s *panicClusterStub) CreateHaStatusArmHa(_ context.Context) error {
	panic("panicClusterStub.CreateHaStatusArmHa: not configured for SDN test; opt in by setting CreateHaStatusArmHaFn")
}

func (s *panicClusterStub) ListHaStatusCurrent(_ context.Context) (*cluster.ListHaStatusCurrentResponse, error) {
	panic("panicClusterStub.ListHaStatusCurrent: not configured for SDN test; opt in by setting ListHaStatusCurrentFn")
}

func (s *panicClusterStub) CreateHaStatusDisarmHa(_ context.Context, _ *cluster.CreateHaStatusDisarmHaParams) error {
	panic("panicClusterStub.CreateHaStatusDisarmHa: not configured for SDN test; opt in by setting CreateHaStatusDisarmHaFn")
}

func (s *panicClusterStub) ListHaStatusManagerStatus(_ context.Context) (*cluster.ListHaStatusManagerStatusResponse, error) {
	panic("panicClusterStub.ListHaStatusManagerStatus: not configured for SDN test; opt in by setting ListHaStatusManagerStatusFn")
}

func (s *panicClusterStub) ListJobs(_ context.Context) (*cluster.ListJobsResponse, error) {
	panic("panicClusterStub.ListJobs: not configured for SDN test; opt in by setting ListJobsFn")
}

func (s *panicClusterStub) ListJobsRealmSync(_ context.Context) (*cluster.ListJobsRealmSyncResponse, error) {
	panic("panicClusterStub.ListJobsRealmSync: not configured for SDN test; opt in by setting ListJobsRealmSyncFn")
}

func (s *panicClusterStub) DeleteJobsRealmSync(_ context.Context, _ string) error {
	panic("panicClusterStub.DeleteJobsRealmSync: not configured for SDN test; opt in by setting DeleteJobsRealmSyncFn")
}

func (s *panicClusterStub) GetJobsRealmSync(_ context.Context, _ string) (*cluster.GetJobsRealmSyncResponse, error) {
	panic("panicClusterStub.GetJobsRealmSync: not configured for SDN test; opt in by setting GetJobsRealmSyncFn")
}

func (s *panicClusterStub) CreateJobsRealmSync(_ context.Context, _ string, _ *cluster.CreateJobsRealmSyncParams) error {
	panic("panicClusterStub.CreateJobsRealmSync: not configured for SDN test; opt in by setting CreateJobsRealmSyncFn")
}

func (s *panicClusterStub) UpdateJobsRealmSync(_ context.Context, _ string, _ *cluster.UpdateJobsRealmSyncParams) error {
	panic("panicClusterStub.UpdateJobsRealmSync: not configured for SDN test; opt in by setting UpdateJobsRealmSyncFn")
}

func (s *panicClusterStub) ListJobsScheduleAnalyze(_ context.Context, _ *cluster.ListJobsScheduleAnalyzeParams) (*cluster.ListJobsScheduleAnalyzeResponse, error) {
	panic("panicClusterStub.ListJobsScheduleAnalyze: not configured for SDN test; opt in by setting ListJobsScheduleAnalyzeFn")
}

func (s *panicClusterStub) ListLog(_ context.Context, _ *cluster.ListLogParams) (*cluster.ListLogResponse, error) {
	panic("panicClusterStub.ListLog: not configured for SDN test; opt in by setting ListLogFn")
}

func (s *panicClusterStub) ListMapping(_ context.Context) (*cluster.ListMappingResponse, error) {
	panic("panicClusterStub.ListMapping: not configured for SDN test; opt in by setting ListMappingFn")
}

func (s *panicClusterStub) ListMappingDir(_ context.Context, _ *cluster.ListMappingDirParams) (*cluster.ListMappingDirResponse, error) {
	panic("panicClusterStub.ListMappingDir: not configured for SDN test; opt in by setting ListMappingDirFn")
}

func (s *panicClusterStub) CreateMappingDir(_ context.Context, _ *cluster.CreateMappingDirParams) error {
	panic("panicClusterStub.CreateMappingDir: not configured for SDN test; opt in by setting CreateMappingDirFn")
}

func (s *panicClusterStub) DeleteMappingDir(_ context.Context, _ string) error {
	panic("panicClusterStub.DeleteMappingDir: not configured for SDN test; opt in by setting DeleteMappingDirFn")
}

func (s *panicClusterStub) GetMappingDir(_ context.Context, _ string) (*cluster.GetMappingDirResponse, error) {
	panic("panicClusterStub.GetMappingDir: not configured for SDN test; opt in by setting GetMappingDirFn")
}

func (s *panicClusterStub) UpdateMappingDir(_ context.Context, _ string, _ *cluster.UpdateMappingDirParams) error {
	panic("panicClusterStub.UpdateMappingDir: not configured for SDN test; opt in by setting UpdateMappingDirFn")
}

func (s *panicClusterStub) ListMappingPci(_ context.Context, _ *cluster.ListMappingPciParams) (*cluster.ListMappingPciResponse, error) {
	panic("panicClusterStub.ListMappingPci: not configured for SDN test; opt in by setting ListMappingPciFn")
}

func (s *panicClusterStub) CreateMappingPci(_ context.Context, _ *cluster.CreateMappingPciParams) error {
	panic("panicClusterStub.CreateMappingPci: not configured for SDN test; opt in by setting CreateMappingPciFn")
}

func (s *panicClusterStub) DeleteMappingPci(_ context.Context, _ string) error {
	panic("panicClusterStub.DeleteMappingPci: not configured for SDN test; opt in by setting DeleteMappingPciFn")
}

func (s *panicClusterStub) GetMappingPci(_ context.Context, _ string) (*cluster.GetMappingPciResponse, error) {
	panic("panicClusterStub.GetMappingPci: not configured for SDN test; opt in by setting GetMappingPciFn")
}

func (s *panicClusterStub) UpdateMappingPci(_ context.Context, _ string, _ *cluster.UpdateMappingPciParams) error {
	panic("panicClusterStub.UpdateMappingPci: not configured for SDN test; opt in by setting UpdateMappingPciFn")
}

func (s *panicClusterStub) ListMappingUsb(_ context.Context, _ *cluster.ListMappingUsbParams) (*cluster.ListMappingUsbResponse, error) {
	panic("panicClusterStub.ListMappingUsb: not configured for SDN test; opt in by setting ListMappingUsbFn")
}

func (s *panicClusterStub) CreateMappingUsb(_ context.Context, _ *cluster.CreateMappingUsbParams) error {
	panic("panicClusterStub.CreateMappingUsb: not configured for SDN test; opt in by setting CreateMappingUsbFn")
}

func (s *panicClusterStub) DeleteMappingUsb(_ context.Context, _ string) error {
	panic("panicClusterStub.DeleteMappingUsb: not configured for SDN test; opt in by setting DeleteMappingUsbFn")
}

func (s *panicClusterStub) GetMappingUsb(_ context.Context, _ string) (*cluster.GetMappingUsbResponse, error) {
	panic("panicClusterStub.GetMappingUsb: not configured for SDN test; opt in by setting GetMappingUsbFn")
}

func (s *panicClusterStub) UpdateMappingUsb(_ context.Context, _ string, _ *cluster.UpdateMappingUsbParams) error {
	panic("panicClusterStub.UpdateMappingUsb: not configured for SDN test; opt in by setting UpdateMappingUsbFn")
}

func (s *panicClusterStub) ListMetrics(_ context.Context) (*cluster.ListMetricsResponse, error) {
	panic("panicClusterStub.ListMetrics: not configured for SDN test; opt in by setting ListMetricsFn")
}

func (s *panicClusterStub) ListMetricsExport(_ context.Context, _ *cluster.ListMetricsExportParams) (*cluster.ListMetricsExportResponse, error) {
	panic("panicClusterStub.ListMetricsExport: not configured for SDN test; opt in by setting ListMetricsExportFn")
}

func (s *panicClusterStub) ListMetricsServer(_ context.Context) (*cluster.ListMetricsServerResponse, error) {
	panic("panicClusterStub.ListMetricsServer: not configured for SDN test; opt in by setting ListMetricsServerFn")
}

func (s *panicClusterStub) DeleteMetricsServer(_ context.Context, _ string) error {
	panic("panicClusterStub.DeleteMetricsServer: not configured for SDN test; opt in by setting DeleteMetricsServerFn")
}

func (s *panicClusterStub) GetMetricsServer(_ context.Context, _ string) (*cluster.GetMetricsServerResponse, error) {
	panic("panicClusterStub.GetMetricsServer: not configured for SDN test; opt in by setting GetMetricsServerFn")
}

func (s *panicClusterStub) CreateMetricsServer(_ context.Context, _ string, _ *cluster.CreateMetricsServerParams) error {
	panic("panicClusterStub.CreateMetricsServer: not configured for SDN test; opt in by setting CreateMetricsServerFn")
}

func (s *panicClusterStub) UpdateMetricsServer(_ context.Context, _ string, _ *cluster.UpdateMetricsServerParams) error {
	panic("panicClusterStub.UpdateMetricsServer: not configured for SDN test; opt in by setting UpdateMetricsServerFn")
}

func (s *panicClusterStub) ListNextid(_ context.Context, _ *cluster.ListNextidParams) (*cluster.ListNextidResponse, error) {
	panic("panicClusterStub.ListNextid: not configured for SDN test; opt in by setting ListNextidFn")
}

func (s *panicClusterStub) ListNotifications(_ context.Context) (*cluster.ListNotificationsResponse, error) {
	panic("panicClusterStub.ListNotifications: not configured for SDN test; opt in by setting ListNotificationsFn")
}

func (s *panicClusterStub) ListNotificationsEndpoints(_ context.Context) (*cluster.ListNotificationsEndpointsResponse, error) {
	panic("panicClusterStub.ListNotificationsEndpoints: not configured for SDN test; opt in by setting ListNotificationsEndpointsFn")
}

func (s *panicClusterStub) ListNotificationsEndpointsGotify(_ context.Context) (*cluster.ListNotificationsEndpointsGotifyResponse, error) {
	panic("panicClusterStub.ListNotificationsEndpointsGotify: not configured for SDN test; opt in by setting ListNotificationsEndpointsGotifyFn")
}

func (s *panicClusterStub) CreateNotificationsEndpointsGotify(_ context.Context, _ *cluster.CreateNotificationsEndpointsGotifyParams) error {
	panic("panicClusterStub.CreateNotificationsEndpointsGotify: not configured for SDN test; opt in by setting CreateNotificationsEndpointsGotifyFn")
}

func (s *panicClusterStub) DeleteNotificationsEndpointsGotify(_ context.Context, _ string) error {
	panic("panicClusterStub.DeleteNotificationsEndpointsGotify: not configured for SDN test; opt in by setting DeleteNotificationsEndpointsGotifyFn")
}

func (s *panicClusterStub) GetNotificationsEndpointsGotify(_ context.Context, _ string) (*cluster.GetNotificationsEndpointsGotifyResponse, error) {
	panic("panicClusterStub.GetNotificationsEndpointsGotify: not configured for SDN test; opt in by setting GetNotificationsEndpointsGotifyFn")
}

func (s *panicClusterStub) UpdateNotificationsEndpointsGotify(_ context.Context, _ string, _ *cluster.UpdateNotificationsEndpointsGotifyParams) error {
	panic("panicClusterStub.UpdateNotificationsEndpointsGotify: not configured for SDN test; opt in by setting UpdateNotificationsEndpointsGotifyFn")
}

func (s *panicClusterStub) ListNotificationsEndpointsSendmail(_ context.Context) (*cluster.ListNotificationsEndpointsSendmailResponse, error) {
	panic("panicClusterStub.ListNotificationsEndpointsSendmail: not configured for SDN test; opt in by setting ListNotificationsEndpointsSendmailFn")
}

func (s *panicClusterStub) CreateNotificationsEndpointsSendmail(_ context.Context, _ *cluster.CreateNotificationsEndpointsSendmailParams) error {
	panic("panicClusterStub.CreateNotificationsEndpointsSendmail: not configured for SDN test; opt in by setting CreateNotificationsEndpointsSendmailFn")
}

func (s *panicClusterStub) DeleteNotificationsEndpointsSendmail(_ context.Context, _ string) error {
	panic("panicClusterStub.DeleteNotificationsEndpointsSendmail: not configured for SDN test; opt in by setting DeleteNotificationsEndpointsSendmailFn")
}

func (s *panicClusterStub) GetNotificationsEndpointsSendmail(_ context.Context, _ string) (*cluster.GetNotificationsEndpointsSendmailResponse, error) {
	panic("panicClusterStub.GetNotificationsEndpointsSendmail: not configured for SDN test; opt in by setting GetNotificationsEndpointsSendmailFn")
}

func (s *panicClusterStub) UpdateNotificationsEndpointsSendmail(_ context.Context, _ string, _ *cluster.UpdateNotificationsEndpointsSendmailParams) error {
	panic("panicClusterStub.UpdateNotificationsEndpointsSendmail: not configured for SDN test; opt in by setting UpdateNotificationsEndpointsSendmailFn")
}

func (s *panicClusterStub) ListNotificationsEndpointsSmtp(_ context.Context) (*cluster.ListNotificationsEndpointsSmtpResponse, error) {
	panic("panicClusterStub.ListNotificationsEndpointsSmtp: not configured for SDN test; opt in by setting ListNotificationsEndpointsSmtpFn")
}

func (s *panicClusterStub) CreateNotificationsEndpointsSmtp(_ context.Context, _ *cluster.CreateNotificationsEndpointsSmtpParams) error {
	panic("panicClusterStub.CreateNotificationsEndpointsSmtp: not configured for SDN test; opt in by setting CreateNotificationsEndpointsSmtpFn")
}

func (s *panicClusterStub) DeleteNotificationsEndpointsSmtp(_ context.Context, _ string) error {
	panic("panicClusterStub.DeleteNotificationsEndpointsSmtp: not configured for SDN test; opt in by setting DeleteNotificationsEndpointsSmtpFn")
}

func (s *panicClusterStub) GetNotificationsEndpointsSmtp(_ context.Context, _ string) (*cluster.GetNotificationsEndpointsSmtpResponse, error) {
	panic("panicClusterStub.GetNotificationsEndpointsSmtp: not configured for SDN test; opt in by setting GetNotificationsEndpointsSmtpFn")
}

func (s *panicClusterStub) UpdateNotificationsEndpointsSmtp(_ context.Context, _ string, _ *cluster.UpdateNotificationsEndpointsSmtpParams) error {
	panic("panicClusterStub.UpdateNotificationsEndpointsSmtp: not configured for SDN test; opt in by setting UpdateNotificationsEndpointsSmtpFn")
}

func (s *panicClusterStub) ListNotificationsEndpointsWebhook(_ context.Context) (*cluster.ListNotificationsEndpointsWebhookResponse, error) {
	panic("panicClusterStub.ListNotificationsEndpointsWebhook: not configured for SDN test; opt in by setting ListNotificationsEndpointsWebhookFn")
}

func (s *panicClusterStub) CreateNotificationsEndpointsWebhook(_ context.Context, _ *cluster.CreateNotificationsEndpointsWebhookParams) error {
	panic("panicClusterStub.CreateNotificationsEndpointsWebhook: not configured for SDN test; opt in by setting CreateNotificationsEndpointsWebhookFn")
}

func (s *panicClusterStub) DeleteNotificationsEndpointsWebhook(_ context.Context, _ string) error {
	panic("panicClusterStub.DeleteNotificationsEndpointsWebhook: not configured for SDN test; opt in by setting DeleteNotificationsEndpointsWebhookFn")
}

func (s *panicClusterStub) GetNotificationsEndpointsWebhook(_ context.Context, _ string) (*cluster.GetNotificationsEndpointsWebhookResponse, error) {
	panic("panicClusterStub.GetNotificationsEndpointsWebhook: not configured for SDN test; opt in by setting GetNotificationsEndpointsWebhookFn")
}

func (s *panicClusterStub) UpdateNotificationsEndpointsWebhook(_ context.Context, _ string, _ *cluster.UpdateNotificationsEndpointsWebhookParams) error {
	panic("panicClusterStub.UpdateNotificationsEndpointsWebhook: not configured for SDN test; opt in by setting UpdateNotificationsEndpointsWebhookFn")
}

func (s *panicClusterStub) ListNotificationsMatcherFieldValues(_ context.Context) (*cluster.ListNotificationsMatcherFieldValuesResponse, error) {
	panic("panicClusterStub.ListNotificationsMatcherFieldValues: not configured for SDN test; opt in by setting ListNotificationsMatcherFieldValuesFn")
}

func (s *panicClusterStub) ListNotificationsMatcherFields(_ context.Context) (*cluster.ListNotificationsMatcherFieldsResponse, error) {
	panic("panicClusterStub.ListNotificationsMatcherFields: not configured for SDN test; opt in by setting ListNotificationsMatcherFieldsFn")
}

func (s *panicClusterStub) ListNotificationsMatchers(_ context.Context) (*cluster.ListNotificationsMatchersResponse, error) {
	panic("panicClusterStub.ListNotificationsMatchers: not configured for SDN test; opt in by setting ListNotificationsMatchersFn")
}

func (s *panicClusterStub) CreateNotificationsMatchers(_ context.Context, _ *cluster.CreateNotificationsMatchersParams) error {
	panic("panicClusterStub.CreateNotificationsMatchers: not configured for SDN test; opt in by setting CreateNotificationsMatchersFn")
}

func (s *panicClusterStub) DeleteNotificationsMatchers(_ context.Context, _ string) error {
	panic("panicClusterStub.DeleteNotificationsMatchers: not configured for SDN test; opt in by setting DeleteNotificationsMatchersFn")
}

func (s *panicClusterStub) GetNotificationsMatchers(_ context.Context, _ string) (*cluster.GetNotificationsMatchersResponse, error) {
	panic("panicClusterStub.GetNotificationsMatchers: not configured for SDN test; opt in by setting GetNotificationsMatchersFn")
}

func (s *panicClusterStub) UpdateNotificationsMatchers(_ context.Context, _ string, _ *cluster.UpdateNotificationsMatchersParams) error {
	panic("panicClusterStub.UpdateNotificationsMatchers: not configured for SDN test; opt in by setting UpdateNotificationsMatchersFn")
}

func (s *panicClusterStub) ListNotificationsTargets(_ context.Context) (*cluster.ListNotificationsTargetsResponse, error) {
	panic("panicClusterStub.ListNotificationsTargets: not configured for SDN test; opt in by setting ListNotificationsTargetsFn")
}

func (s *panicClusterStub) CreateNotificationsTargetsTest(_ context.Context, _ string) error {
	panic("panicClusterStub.CreateNotificationsTargetsTest: not configured for SDN test; opt in by setting CreateNotificationsTargetsTestFn")
}

func (s *panicClusterStub) ListOptions(_ context.Context) (*cluster.ListOptionsResponse, error) {
	panic("panicClusterStub.ListOptions: not configured for SDN test; opt in by setting ListOptionsFn")
}

func (s *panicClusterStub) UpdateOptions(_ context.Context, _ *cluster.UpdateOptionsParams) error {
	panic("panicClusterStub.UpdateOptions: not configured for SDN test; opt in by setting UpdateOptionsFn")
}

func (s *panicClusterStub) ListReplication(_ context.Context) (*cluster.ListReplicationResponse, error) {
	panic("panicClusterStub.ListReplication: not configured for SDN test; opt in by setting ListReplicationFn")
}

func (s *panicClusterStub) CreateReplication(_ context.Context, _ *cluster.CreateReplicationParams) error {
	panic("panicClusterStub.CreateReplication: not configured for SDN test; opt in by setting CreateReplicationFn")
}

func (s *panicClusterStub) DeleteReplication(_ context.Context, _ string, _ *cluster.DeleteReplicationParams) error {
	panic("panicClusterStub.DeleteReplication: not configured for SDN test; opt in by setting DeleteReplicationFn")
}

func (s *panicClusterStub) GetReplication(_ context.Context, _ string) (*cluster.GetReplicationResponse, error) {
	panic("panicClusterStub.GetReplication: not configured for SDN test; opt in by setting GetReplicationFn")
}

func (s *panicClusterStub) UpdateReplication(_ context.Context, _ string, _ *cluster.UpdateReplicationParams) error {
	panic("panicClusterStub.UpdateReplication: not configured for SDN test; opt in by setting UpdateReplicationFn")
}

func (s *panicClusterStub) ListResources(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
	panic("panicClusterStub.ListResources: not configured for SDN test; opt in by setting ListResourcesFn")
}

func (s *panicClusterStub) ListSdn(_ context.Context) (*cluster.ListSdnResponse, error) {
	panic("panicClusterStub.ListSdn: not configured for SDN test; opt in by setting ListSdnFn")
}

func (s *panicClusterStub) UpdateSdn(_ context.Context, _ *cluster.UpdateSdnParams) (*cluster.UpdateSdnResponse, error) {
	panic("panicClusterStub.UpdateSdn: not configured for SDN test; opt in by setting UpdateSdnFn")
}

func (s *panicClusterStub) ListSdnControllers(_ context.Context, _ *cluster.ListSdnControllersParams) (*cluster.ListSdnControllersResponse, error) {
	panic("panicClusterStub.ListSdnControllers: not configured for SDN test; opt in by setting ListSdnControllersFn")
}

func (s *panicClusterStub) CreateSdnControllers(_ context.Context, _ *cluster.CreateSdnControllersParams) error {
	panic("panicClusterStub.CreateSdnControllers: not configured for SDN test; opt in by setting CreateSdnControllersFn")
}

func (s *panicClusterStub) DeleteSdnControllers(_ context.Context, _ string, _ *cluster.DeleteSdnControllersParams) error {
	panic("panicClusterStub.DeleteSdnControllers: not configured for SDN test; opt in by setting DeleteSdnControllersFn")
}

func (s *panicClusterStub) GetSdnControllers(_ context.Context, _ string, _ *cluster.GetSdnControllersParams) (*cluster.GetSdnControllersResponse, error) {
	panic("panicClusterStub.GetSdnControllers: not configured for SDN test; opt in by setting GetSdnControllersFn")
}

func (s *panicClusterStub) UpdateSdnControllers(_ context.Context, _ string, _ *cluster.UpdateSdnControllersParams) error {
	panic("panicClusterStub.UpdateSdnControllers: not configured for SDN test; opt in by setting UpdateSdnControllersFn")
}

func (s *panicClusterStub) ListSdnDns(_ context.Context, _ *cluster.ListSdnDnsParams) (*cluster.ListSdnDnsResponse, error) {
	panic("panicClusterStub.ListSdnDns: not configured for SDN test; opt in by setting ListSdnDnsFn")
}

func (s *panicClusterStub) CreateSdnDns(_ context.Context, _ *cluster.CreateSdnDnsParams) error {
	panic("panicClusterStub.CreateSdnDns: not configured for SDN test; opt in by setting CreateSdnDnsFn")
}

func (s *panicClusterStub) DeleteSdnDns(_ context.Context, _ string, _ *cluster.DeleteSdnDnsParams) error {
	panic("panicClusterStub.DeleteSdnDns: not configured for SDN test; opt in by setting DeleteSdnDnsFn")
}

func (s *panicClusterStub) GetSdnDns(_ context.Context, _ string) (*cluster.GetSdnDnsResponse, error) {
	panic("panicClusterStub.GetSdnDns: not configured for SDN test; opt in by setting GetSdnDnsFn")
}

func (s *panicClusterStub) UpdateSdnDns(_ context.Context, _ string, _ *cluster.UpdateSdnDnsParams) error {
	panic("panicClusterStub.UpdateSdnDns: not configured for SDN test; opt in by setting UpdateSdnDnsFn")
}

func (s *panicClusterStub) ListSdnDryRun(_ context.Context, _ *cluster.ListSdnDryRunParams) (*cluster.ListSdnDryRunResponse, error) {
	panic("panicClusterStub.ListSdnDryRun: not configured for SDN test; opt in by setting ListSdnDryRunFn")
}

func (s *panicClusterStub) ListSdnFabrics(_ context.Context) (*cluster.ListSdnFabricsResponse, error) {
	panic("panicClusterStub.ListSdnFabrics: not configured for SDN test; opt in by setting ListSdnFabricsFn")
}

func (s *panicClusterStub) ListSdnFabricsAll(_ context.Context, _ *cluster.ListSdnFabricsAllParams) (*cluster.ListSdnFabricsAllResponse, error) {
	panic("panicClusterStub.ListSdnFabricsAll: not configured for SDN test; opt in by setting ListSdnFabricsAllFn")
}

func (s *panicClusterStub) ListSdnFabricsFabric(_ context.Context, _ *cluster.ListSdnFabricsFabricParams) (*cluster.ListSdnFabricsFabricResponse, error) {
	panic("panicClusterStub.ListSdnFabricsFabric: not configured for SDN test; opt in by setting ListSdnFabricsFabricFn")
}

func (s *panicClusterStub) CreateSdnFabricsFabric(_ context.Context, _ *cluster.CreateSdnFabricsFabricParams) error {
	panic("panicClusterStub.CreateSdnFabricsFabric: not configured for SDN test; opt in by setting CreateSdnFabricsFabricFn")
}

func (s *panicClusterStub) DeleteSdnFabricsFabric(_ context.Context, _ string) error {
	panic("panicClusterStub.DeleteSdnFabricsFabric: not configured for SDN test; opt in by setting DeleteSdnFabricsFabricFn")
}

func (s *panicClusterStub) GetSdnFabricsFabric(_ context.Context, _ string) (*cluster.GetSdnFabricsFabricResponse, error) {
	panic("panicClusterStub.GetSdnFabricsFabric: not configured for SDN test; opt in by setting GetSdnFabricsFabricFn")
}

func (s *panicClusterStub) UpdateSdnFabricsFabric(_ context.Context, _ string, _ *cluster.UpdateSdnFabricsFabricParams) error {
	panic("panicClusterStub.UpdateSdnFabricsFabric: not configured for SDN test; opt in by setting UpdateSdnFabricsFabricFn")
}

func (s *panicClusterStub) ListSdnFabricsNode(_ context.Context, _ *cluster.ListSdnFabricsNodeParams) (*cluster.ListSdnFabricsNodeResponse, error) {
	panic("panicClusterStub.ListSdnFabricsNode: not configured for SDN test; opt in by setting ListSdnFabricsNodeFn")
}

func (s *panicClusterStub) GetSdnFabricsNode(_ context.Context, _ string, _ *cluster.GetSdnFabricsNodeParams) (*cluster.GetSdnFabricsNodeResponse, error) {
	panic("panicClusterStub.GetSdnFabricsNode: not configured for SDN test; opt in by setting GetSdnFabricsNodeFn")
}

func (s *panicClusterStub) CreateSdnFabricsNode(_ context.Context, _ string, _ *cluster.CreateSdnFabricsNodeParams) error {
	panic("panicClusterStub.CreateSdnFabricsNode: not configured for SDN test; opt in by setting CreateSdnFabricsNodeFn")
}

func (s *panicClusterStub) DeleteSdnFabricsNode(_ context.Context, _ string, _ string) error {
	panic("panicClusterStub.DeleteSdnFabricsNode: not configured for SDN test; opt in by setting DeleteSdnFabricsNodeFn")
}

func (s *panicClusterStub) GetSdnFabricsNode2(_ context.Context, _ string, _ string) (*cluster.GetSdnFabricsNode2Response, error) {
	panic("panicClusterStub.GetSdnFabricsNode2: not configured for SDN test; opt in by setting GetSdnFabricsNode2Fn")
}

func (s *panicClusterStub) UpdateSdnFabricsNode(_ context.Context, _ string, _ string, _ *cluster.UpdateSdnFabricsNodeParams) error {
	panic("panicClusterStub.UpdateSdnFabricsNode: not configured for SDN test; opt in by setting UpdateSdnFabricsNodeFn")
}

func (s *panicClusterStub) ListSdnIpams(_ context.Context, _ *cluster.ListSdnIpamsParams) (*cluster.ListSdnIpamsResponse, error) {
	panic("panicClusterStub.ListSdnIpams: not configured for SDN test; opt in by setting ListSdnIpamsFn")
}

func (s *panicClusterStub) CreateSdnIpams(_ context.Context, _ *cluster.CreateSdnIpamsParams) error {
	panic("panicClusterStub.CreateSdnIpams: not configured for SDN test; opt in by setting CreateSdnIpamsFn")
}

func (s *panicClusterStub) DeleteSdnIpams(_ context.Context, _ string, _ *cluster.DeleteSdnIpamsParams) error {
	panic("panicClusterStub.DeleteSdnIpams: not configured for SDN test; opt in by setting DeleteSdnIpamsFn")
}

func (s *panicClusterStub) GetSdnIpams(_ context.Context, _ string) (*cluster.GetSdnIpamsResponse, error) {
	panic("panicClusterStub.GetSdnIpams: not configured for SDN test; opt in by setting GetSdnIpamsFn")
}

func (s *panicClusterStub) UpdateSdnIpams(_ context.Context, _ string, _ *cluster.UpdateSdnIpamsParams) error {
	panic("panicClusterStub.UpdateSdnIpams: not configured for SDN test; opt in by setting UpdateSdnIpamsFn")
}

func (s *panicClusterStub) ListSdnIpamsStatus(_ context.Context, _ string) (*cluster.ListSdnIpamsStatusResponse, error) {
	panic("panicClusterStub.ListSdnIpamsStatus: not configured for SDN test; opt in by setting ListSdnIpamsStatusFn")
}

func (s *panicClusterStub) DeleteSdnLock(_ context.Context, _ *cluster.DeleteSdnLockParams) error {
	panic("panicClusterStub.DeleteSdnLock: not configured for SDN test; opt in by setting DeleteSdnLockFn")
}

func (s *panicClusterStub) CreateSdnLock(_ context.Context, _ *cluster.CreateSdnLockParams) (*cluster.CreateSdnLockResponse, error) {
	panic("panicClusterStub.CreateSdnLock: not configured for SDN test; opt in by setting CreateSdnLockFn")
}

func (s *panicClusterStub) ListSdnPrefixLists(_ context.Context, _ *cluster.ListSdnPrefixListsParams) (*cluster.ListSdnPrefixListsResponse, error) {
	panic("panicClusterStub.ListSdnPrefixLists: not configured for SDN test; opt in by setting ListSdnPrefixListsFn")
}

func (s *panicClusterStub) CreateSdnPrefixLists(_ context.Context, _ *cluster.CreateSdnPrefixListsParams) error {
	panic("panicClusterStub.CreateSdnPrefixLists: not configured for SDN test; opt in by setting CreateSdnPrefixListsFn")
}

func (s *panicClusterStub) DeleteSdnPrefixLists(_ context.Context, _ string, _ *cluster.DeleteSdnPrefixListsParams) error {
	panic("panicClusterStub.DeleteSdnPrefixLists: not configured for SDN test; opt in by setting DeleteSdnPrefixListsFn")
}

func (s *panicClusterStub) GetSdnPrefixLists(_ context.Context, _ string) (*cluster.GetSdnPrefixListsResponse, error) {
	panic("panicClusterStub.GetSdnPrefixLists: not configured for SDN test; opt in by setting GetSdnPrefixListsFn")
}

func (s *panicClusterStub) UpdateSdnPrefixLists(_ context.Context, _ string, _ *cluster.UpdateSdnPrefixListsParams) error {
	panic("panicClusterStub.UpdateSdnPrefixLists: not configured for SDN test; opt in by setting UpdateSdnPrefixListsFn")
}

func (s *panicClusterStub) ListSdnPrefixListsEntries(_ context.Context, _ string) (*cluster.ListSdnPrefixListsEntriesResponse, error) {
	panic("panicClusterStub.ListSdnPrefixListsEntries: not configured for SDN test; opt in by setting ListSdnPrefixListsEntriesFn")
}

func (s *panicClusterStub) CreateSdnPrefixListsEntries(_ context.Context, _ string, _ *cluster.CreateSdnPrefixListsEntriesParams) error {
	panic("panicClusterStub.CreateSdnPrefixListsEntries: not configured for SDN test; opt in by setting CreateSdnPrefixListsEntriesFn")
}

func (s *panicClusterStub) DeleteSdnPrefixListsEntries(_ context.Context, _ string, _ string, _ *cluster.DeleteSdnPrefixListsEntriesParams) error {
	panic("panicClusterStub.DeleteSdnPrefixListsEntries: not configured for SDN test; opt in by setting DeleteSdnPrefixListsEntriesFn")
}

func (s *panicClusterStub) GetSdnPrefixListsEntries(_ context.Context, _ string, _ string) (*cluster.GetSdnPrefixListsEntriesResponse, error) {
	panic("panicClusterStub.GetSdnPrefixListsEntries: not configured for SDN test; opt in by setting GetSdnPrefixListsEntriesFn")
}

func (s *panicClusterStub) UpdateSdnPrefixListsEntries(_ context.Context, _ string, _ string, _ *cluster.UpdateSdnPrefixListsEntriesParams) error {
	panic("panicClusterStub.UpdateSdnPrefixListsEntries: not configured for SDN test; opt in by setting UpdateSdnPrefixListsEntriesFn")
}

func (s *panicClusterStub) CreateSdnRollback(_ context.Context, _ *cluster.CreateSdnRollbackParams) error {
	panic("panicClusterStub.CreateSdnRollback: not configured for SDN test; opt in by setting CreateSdnRollbackFn")
}

func (s *panicClusterStub) ListSdnRouteMaps(_ context.Context, _ *cluster.ListSdnRouteMapsParams) (*cluster.ListSdnRouteMapsResponse, error) {
	panic("panicClusterStub.ListSdnRouteMaps: not configured for SDN test; opt in by setting ListSdnRouteMapsFn")
}

func (s *panicClusterStub) ListSdnRouteMapsEntries(_ context.Context, _ *cluster.ListSdnRouteMapsEntriesParams) (*cluster.ListSdnRouteMapsEntriesResponse, error) {
	panic("panicClusterStub.ListSdnRouteMapsEntries: not configured for SDN test; opt in by setting ListSdnRouteMapsEntriesFn")
}

func (s *panicClusterStub) CreateSdnRouteMapsEntries(_ context.Context, _ *cluster.CreateSdnRouteMapsEntriesParams) error {
	panic("panicClusterStub.CreateSdnRouteMapsEntries: not configured for SDN test; opt in by setting CreateSdnRouteMapsEntriesFn")
}

func (s *panicClusterStub) GetSdnRouteMapsEntries(_ context.Context, _ string, _ *cluster.GetSdnRouteMapsEntriesParams) (*cluster.GetSdnRouteMapsEntriesResponse, error) {
	panic("panicClusterStub.GetSdnRouteMapsEntries: not configured for SDN test; opt in by setting GetSdnRouteMapsEntriesFn")
}

func (s *panicClusterStub) DeleteSdnRouteMapsEntriesEntry(_ context.Context, _ string, _ string, _ *cluster.DeleteSdnRouteMapsEntriesEntryParams) error {
	panic("panicClusterStub.DeleteSdnRouteMapsEntriesEntry: not configured for SDN test; opt in by setting DeleteSdnRouteMapsEntriesEntryFn")
}

func (s *panicClusterStub) GetSdnRouteMapsEntriesEntry(_ context.Context, _ string, _ string) (*cluster.GetSdnRouteMapsEntriesEntryResponse, error) {
	panic("panicClusterStub.GetSdnRouteMapsEntriesEntry: not configured for SDN test; opt in by setting GetSdnRouteMapsEntriesEntryFn")
}

func (s *panicClusterStub) UpdateSdnRouteMapsEntriesEntry(_ context.Context, _ string, _ string, _ *cluster.UpdateSdnRouteMapsEntriesEntryParams) error {
	panic("panicClusterStub.UpdateSdnRouteMapsEntriesEntry: not configured for SDN test; opt in by setting UpdateSdnRouteMapsEntriesEntryFn")
}

func (s *panicClusterStub) ListSdnVnets(_ context.Context, _ *cluster.ListSdnVnetsParams) (*cluster.ListSdnVnetsResponse, error) {
	panic("panicClusterStub.ListSdnVnets: not configured for SDN test; opt in by setting ListSdnVnetsFn")
}

func (s *panicClusterStub) CreateSdnVnets(_ context.Context, _ *cluster.CreateSdnVnetsParams) error {
	panic("panicClusterStub.CreateSdnVnets: not configured for SDN test; opt in by setting CreateSdnVnetsFn")
}

func (s *panicClusterStub) DeleteSdnVnets(_ context.Context, _ string, _ *cluster.DeleteSdnVnetsParams) error {
	panic("panicClusterStub.DeleteSdnVnets: not configured for SDN test; opt in by setting DeleteSdnVnetsFn")
}

func (s *panicClusterStub) GetSdnVnets(_ context.Context, _ string, _ *cluster.GetSdnVnetsParams) (*cluster.GetSdnVnetsResponse, error) {
	panic("panicClusterStub.GetSdnVnets: not configured for SDN test; opt in by setting GetSdnVnetsFn")
}

func (s *panicClusterStub) UpdateSdnVnets(_ context.Context, _ string, _ *cluster.UpdateSdnVnetsParams) error {
	panic("panicClusterStub.UpdateSdnVnets: not configured for SDN test; opt in by setting UpdateSdnVnetsFn")
}

func (s *panicClusterStub) ListSdnVnetsFirewall(_ context.Context, _ string) (*cluster.ListSdnVnetsFirewallResponse, error) {
	panic("panicClusterStub.ListSdnVnetsFirewall: not configured for SDN test; opt in by setting ListSdnVnetsFirewallFn")
}

func (s *panicClusterStub) ListSdnVnetsFirewallOptions(_ context.Context, _ string) (*cluster.ListSdnVnetsFirewallOptionsResponse, error) {
	panic("panicClusterStub.ListSdnVnetsFirewallOptions: not configured for SDN test; opt in by setting ListSdnVnetsFirewallOptionsFn")
}

func (s *panicClusterStub) UpdateSdnVnetsFirewallOptions(_ context.Context, _ string, _ *cluster.UpdateSdnVnetsFirewallOptionsParams) error {
	panic("panicClusterStub.UpdateSdnVnetsFirewallOptions: not configured for SDN test; opt in by setting UpdateSdnVnetsFirewallOptionsFn")
}

func (s *panicClusterStub) ListSdnVnetsFirewallRules(_ context.Context, _ string) (*cluster.ListSdnVnetsFirewallRulesResponse, error) {
	panic("panicClusterStub.ListSdnVnetsFirewallRules: not configured for SDN test; opt in by setting ListSdnVnetsFirewallRulesFn")
}

func (s *panicClusterStub) CreateSdnVnetsFirewallRules(_ context.Context, _ string, _ *cluster.CreateSdnVnetsFirewallRulesParams) error {
	panic("panicClusterStub.CreateSdnVnetsFirewallRules: not configured for SDN test; opt in by setting CreateSdnVnetsFirewallRulesFn")
}

func (s *panicClusterStub) DeleteSdnVnetsFirewallRules(_ context.Context, _ string, _ string, _ *cluster.DeleteSdnVnetsFirewallRulesParams) error {
	panic("panicClusterStub.DeleteSdnVnetsFirewallRules: not configured for SDN test; opt in by setting DeleteSdnVnetsFirewallRulesFn")
}

func (s *panicClusterStub) GetSdnVnetsFirewallRules(_ context.Context, _ string, _ string) (*cluster.GetSdnVnetsFirewallRulesResponse, error) {
	panic("panicClusterStub.GetSdnVnetsFirewallRules: not configured for SDN test; opt in by setting GetSdnVnetsFirewallRulesFn")
}

func (s *panicClusterStub) UpdateSdnVnetsFirewallRules(_ context.Context, _ string, _ string, _ *cluster.UpdateSdnVnetsFirewallRulesParams) error {
	panic("panicClusterStub.UpdateSdnVnetsFirewallRules: not configured for SDN test; opt in by setting UpdateSdnVnetsFirewallRulesFn")
}

func (s *panicClusterStub) DeleteSdnVnetsIps(_ context.Context, _ string, _ *cluster.DeleteSdnVnetsIpsParams) error {
	panic("panicClusterStub.DeleteSdnVnetsIps: not configured for SDN test; opt in by setting DeleteSdnVnetsIpsFn")
}

func (s *panicClusterStub) CreateSdnVnetsIps(_ context.Context, _ string, _ *cluster.CreateSdnVnetsIpsParams) error {
	panic("panicClusterStub.CreateSdnVnetsIps: not configured for SDN test; opt in by setting CreateSdnVnetsIpsFn")
}

func (s *panicClusterStub) UpdateSdnVnetsIps(_ context.Context, _ string, _ *cluster.UpdateSdnVnetsIpsParams) error {
	panic("panicClusterStub.UpdateSdnVnetsIps: not configured for SDN test; opt in by setting UpdateSdnVnetsIpsFn")
}

func (s *panicClusterStub) ListSdnVnetsSubnets(_ context.Context, _ string, _ *cluster.ListSdnVnetsSubnetsParams) (*cluster.ListSdnVnetsSubnetsResponse, error) {
	panic("panicClusterStub.ListSdnVnetsSubnets: not configured for SDN test; opt in by setting ListSdnVnetsSubnetsFn")
}

func (s *panicClusterStub) CreateSdnVnetsSubnets(_ context.Context, _ string, _ *cluster.CreateSdnVnetsSubnetsParams) error {
	panic("panicClusterStub.CreateSdnVnetsSubnets: not configured for SDN test; opt in by setting CreateSdnVnetsSubnetsFn")
}

func (s *panicClusterStub) DeleteSdnVnetsSubnets(_ context.Context, _ string, _ string, _ *cluster.DeleteSdnVnetsSubnetsParams) error {
	panic("panicClusterStub.DeleteSdnVnetsSubnets: not configured for SDN test; opt in by setting DeleteSdnVnetsSubnetsFn")
}

func (s *panicClusterStub) GetSdnVnetsSubnets(_ context.Context, _ string, _ string, _ *cluster.GetSdnVnetsSubnetsParams) (*cluster.GetSdnVnetsSubnetsResponse, error) {
	panic("panicClusterStub.GetSdnVnetsSubnets: not configured for SDN test; opt in by setting GetSdnVnetsSubnetsFn")
}

func (s *panicClusterStub) UpdateSdnVnetsSubnets(_ context.Context, _ string, _ string, _ *cluster.UpdateSdnVnetsSubnetsParams) error {
	panic("panicClusterStub.UpdateSdnVnetsSubnets: not configured for SDN test; opt in by setting UpdateSdnVnetsSubnetsFn")
}

func (s *panicClusterStub) ListSdnZones(_ context.Context, _ *cluster.ListSdnZonesParams) (*cluster.ListSdnZonesResponse, error) {
	panic("panicClusterStub.ListSdnZones: not configured for SDN test; opt in by setting ListSdnZonesFn")
}

func (s *panicClusterStub) CreateSdnZones(_ context.Context, _ *cluster.CreateSdnZonesParams) error {
	panic("panicClusterStub.CreateSdnZones: not configured for SDN test; opt in by setting CreateSdnZonesFn")
}

func (s *panicClusterStub) DeleteSdnZones(_ context.Context, _ string, _ *cluster.DeleteSdnZonesParams) error {
	panic("panicClusterStub.DeleteSdnZones: not configured for SDN test; opt in by setting DeleteSdnZonesFn")
}

func (s *panicClusterStub) GetSdnZones(_ context.Context, _ string, _ *cluster.GetSdnZonesParams) (*cluster.GetSdnZonesResponse, error) {
	panic("panicClusterStub.GetSdnZones: not configured for SDN test; opt in by setting GetSdnZonesFn")
}

func (s *panicClusterStub) UpdateSdnZones(_ context.Context, _ string, _ *cluster.UpdateSdnZonesParams) error {
	panic("panicClusterStub.UpdateSdnZones: not configured for SDN test; opt in by setting UpdateSdnZonesFn")
}

func (s *panicClusterStub) ListStatus(_ context.Context) (*cluster.ListStatusResponse, error) {
	panic("panicClusterStub.ListStatus: not configured for SDN test; opt in by setting ListStatusFn")
}

func (s *panicClusterStub) ListTasks(_ context.Context) (*cluster.ListTasksResponse, error) {
	panic("panicClusterStub.ListTasks: not configured for SDN test; opt in by setting ListTasksFn")
}
