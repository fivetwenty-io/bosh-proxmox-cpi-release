// Package configdrive authors OpenStack-style ConfigDrive ISO images used to
// deliver BOSH agent settings to a newly created VM. The ISO 9660 + Rock Ridge
// volume is labeled "config-2" and contains:
//
//	/openstack/latest/user_data      — raw settings.json bytes
//	/openstack/latest/meta_data.json — minimal OpenStack metadata stub
//	/ec2/latest/user-data            — same payload (EC2 datasource fallback)
//	/ec2/latest/meta-data.json       — same payload (EC2 datasource fallback)
//
// BOSH openstack-kvm stemcells configure the agent's ConfigDrive datasource
// against the /openstack/latest/ paths (matching bosh-openstack-cpi). The
// /ec2/latest/ paths remain for stemcells that fall back to the EC2 datasource.
package configdrive
