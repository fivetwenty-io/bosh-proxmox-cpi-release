// Package client provides a Go client for the Proxmox Virtual Environment (PVE) API.
//
// The client supports multiple authentication methods including username/password,
// API tokens, and two-factor authentication. It handles SSL/TLS certificate verification,
// connection pooling, and automatic retries.
//
// Basic usage:
//
//	client, err := client.NewClient(client.Options{
//	    Host:     "pve.example.com",
//	    Username: "root@pam",
//	    Password: "secret",
//	})
//	if err != nil {
//	    log.Fatal(err)
//	}
//
//	// Make API calls
//	status, err := client.Get("/cluster/status", nil)
//	if err != nil {
//	    log.Fatal(err)
//	}
package client
