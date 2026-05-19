# Troubleshooting

## Authentication Errors

- **Symptom**: "Authentication failed" in logs.
- **Solution**: Verify `pve.user`, `pve.password`, and `pve.realm`. Ensure the user has API access.

## VM Creation Failures

- **Symptom**: VM fails to start.
- **Solution**: Check storage availability (`pve.vm_storage`) and ensure unique VMIDs.

## Network Issues

- **Symptom**: VM lacks connectivity.
- **Solution**: Confirm `pve.network_bridge` matches an existing PVE bridge (e.g., `vmbr0`).

