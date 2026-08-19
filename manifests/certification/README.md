# Certification manifests

PVE-side glue for the [BOSH Director Upgrade Test](../../docs/certification/upgrade.md).

The certification BOSH release and its deployment manifest are **not** vendored here. They
come from a managed shallow clone of
[`cloudfoundry/bosh-cpi-certification`](https://github.com/cloudfoundry/bosh-cpi-certification)
under `.deps/`, resolved by `scripts/_checkouts.py` the same way the BATS and
bosh-deployment checkouts are. `scripts/certify` builds the release straight from that
checkout's `shared/assets/certification-release/` and interpolates its
`certification.yml` deployment manifest. Set `BOSH_CPI_CERTIFICATION_DIR` to point at a
local fork or a pinned revision instead.

What lives here is only what upstream cannot supply, because it is specific to this repo's
lab shape:

- `cloud-config-ops.yml`

  Adds the `certification` vm_type, sized from the `certification:` section of
  `ci/integration.yml`. The PVE equivalent of upstream's
  `<iaas>/certification/cloud-config-ops.yml`.

- `cloud-config-cpi-ops.yml`

  Binds the cloud config's AZ to a cpi-config entry. Applied only when
  `certification.cpi_id` is set, because a multi-CPI director rejects AZs that name no
  entry and a single-CPI director rejects AZs that name one.

The deployment-side ops that reshape upstream's `azs: [z1, z2, z3]` down to this lab's
single AZ are generated into the run's results directory rather than committed, since they
are derived entirely from the active env bundle.
