# Security Policy

The CPI holds Proxmox VE API credentials and participates in the BOSH control plane, so a vulnerability here can reach every VM the Director manages. Here is how to report one and what happens after you do.

## Reporting a vulnerability

Please report vulnerabilities privately through [GitHub private vulnerability reporting](https://github.com/fivetwenty-io/bosh-pve-cpi-release/security/advisories/new). Open the Security tab of this repository and choose "Report a vulnerability".

Do not report vulnerabilities through public GitHub issues, pull requests, or discussions. A public report exposes every deployment before a fix exists.

A good report lets us reproduce the problem without guessing. Please include:

- The CPI release version, or the commit hash if you built from source.

- The Proxmox VE version and the BOSH Director version you are running.

- The steps that trigger the problem, as precisely as you can.

- The impact you believe the problem has, and any configuration it depends on.

- Relevant CPI log lines. The CPI redacts credentials from its logs, but please double-check before you paste.

You do not need a proof-of-concept exploit. A credible description of the problem is enough to start.

## What to expect

- We acknowledge your report within three business days.

- Within ten business days, usually sooner, we tell you whether we consider it a vulnerability. If the assessment takes longer, we keep you informed.

- We develop a fix privately, and we welcome your review of it.

- The fix ships in a new release, along with a GitHub security advisory that describes the problem, the affected versions, and the upgrade path.

- The advisory credits you unless you prefer to stay anonymous.

## Coordinated disclosure

We ask that you keep the details of a report private until we release a fix. In return, we commit to releasing a fix promptly and to publishing the advisory no later than 90 days after your report, even if a complete fix is not ready by then. If we need more time and you agree, we can extend that window.

## Supported versions

We fix vulnerabilities in the latest tagged release. We do not backport fixes to earlier releases. If you run an older version, the upgrade path is the current release.

## Scope

This policy covers the code in this repository: the CPI binary under `src/pve_cpi/`, the BOSH release packaging (jobs, packages, and scripts), and the GitHub Actions workflows.

Vulnerabilities in the platforms the CPI talks to belong upstream:

- For BOSH and other Cloud Foundry Foundation projects, follow the [Cloud Foundry security policy](https://www.cloudfoundry.org/security/) or write to security@cloudfoundry.org.

- For Proxmox VE, write to the Proxmox security team at security@proxmox.com.

If you are unsure where a problem belongs, report it here and we will help route it.
