package config_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

const (
	epHostA = "pve-a.example"
	epHostB = "pve-b.example"
	epHostC = "pve-c.example"
)

// endpointsTokenJob returns the token job from the credential tests with the
// two-machine list [pve-a.example:8006, pve-b.example:8006] a job config holds
// after Load.
func endpointsTokenJob() *config.CPIConfig {
	base := credTestTokenJob()
	base.Endpoints = []string{epHostA + ":8006", epHostB + ":8006"}
	return base
}

// endpointsPasswordJob is endpointsTokenJob for a job that authenticates with
// a user and password.
func endpointsPasswordJob() *config.CPIConfig {
	base := credTestPasswordJob()
	base.Endpoints = []string{epHostA + ":8006", epHostB + ":8006"}
	return base
}

func TestContextOverrideFieldOrder_HoldsEndpoints(t *testing.T) {
	t.Parallel()
	if !slices.Contains(config.ContextOverrideFieldOrderForTest(), "pve_endpoints") {
		t.Fatal("pve_endpoints is not an overridable context key")
	}
	if !slices.Contains(config.ContextOverrideFieldKeysForTest(), "pve_endpoints") {
		t.Fatal("pve_endpoints has no apply function")
	}
}

func TestApplyContextOverrides_Endpoints_Coercion(t *testing.T) {
	t.Parallel()
	base := credTestTokenJob()

	t.Run("a list of strings applies", func(t *testing.T) {
		t.Parallel()
		eff, applied, unknown, err := config.ApplyContextOverrides(base, map[string]any{
			"pve_endpoints": []any{epHostA, epHostB + ":9000"},
			"pve_api_token": credTestEntryToken,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !slices.Contains(applied, "pve_endpoints") || len(unknown) != 0 {
			t.Errorf("applied=%v unknown=%v, want pve_endpoints applied and nothing unknown", applied, unknown)
		}
		want := []string{epHostA + ":8006", epHostB + ":9000"}
		if !slices.Equal(eff.Endpoints, want) {
			t.Errorf("Endpoints = %v, want %v", eff.Endpoints, want)
		}
	})

	t.Run("the nested pve shape applies", func(t *testing.T) {
		t.Parallel()
		eff, applied, _, err := config.ApplyContextOverrides(base, map[string]any{
			"pve": map[string]any{
				"endpoints": []any{epHostA, epHostC},
				"api_token": credTestEntryToken,
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !slices.Contains(applied, "pve_endpoints") {
			t.Errorf("applied = %v, want it to hold pve_endpoints", applied)
		}
		if !slices.Equal(eff.Endpoints, []string{epHostA + ":8006", epHostC + ":8006"}) {
			t.Errorf("Endpoints = %v", eff.Endpoints)
		}
	})

	t.Run("a Go string slice applies", func(t *testing.T) {
		t.Parallel()
		eff, _, _, err := config.ApplyContextOverrides(base, map[string]any{
			"pve_endpoints": []string{epHostA, epHostC},
			"pve_api_token": credTestEntryToken,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(eff.Endpoints) != 2 {
			t.Errorf("Endpoints = %v, want two entries", eff.Endpoints)
		}
	})

	for name, val := range map[string]any{
		"a single string":      epHostA + "," + epHostB,
		"a number":             float64(1),
		"a list with a number": []any{epHostA, float64(2)},
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			t.Parallel()
			eff, _, _, err := config.ApplyContextOverrides(base, map[string]any{"pve_endpoints": val})
			if err == nil || eff != nil {
				t.Fatalf("eff=%v err=%v, want a coercion error", eff, err)
			}
			if !strings.Contains(err.Error(), `context override "pve_endpoints"`) {
				t.Errorf("error = %q, want it to name pve_endpoints", err.Error())
			}
		})
	}

	t.Run("null and an empty list declare no list", func(t *testing.T) {
		t.Parallel()
		for _, val := range []any{nil, []any{}} {
			eff, _, _, err := config.ApplyContextOverrides(base, map[string]any{
				"pve_endpoints": val,
			})
			if err != nil {
				t.Fatalf("unexpected error for %#v: %v", val, err)
			}
			if len(eff.Endpoints) != 0 || eff.Host != credTestJobHost {
				t.Errorf("Endpoints=%v Host=%q, want no list and the job's host", eff.Endpoints, eff.Host)
			}
		}
	})

	t.Run("an entry never writes through the job's slice", func(t *testing.T) {
		t.Parallel()
		job := endpointsTokenJob()
		eff, _, _, err := config.ApplyContextOverrides(job, map[string]any{
			"pve_endpoints": []any{epHostA, epHostC},
			"pve_api_token": credTestEntryToken,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		eff.Endpoints[0] = "mutated:1"
		if job.Endpoints[0] != epHostA+":8006" {
			t.Errorf("job list changed to %v", job.Endpoints)
		}
	})
}

// endpointsEntryWithToken returns extra plus the entry's own token, so a row
// that changes the endpoint list carries a complete credential group.
func endpointsEntryWithToken(extra map[string]any) map[string]any {
	out := map[string]any{"pve_api_token": credTestEntryToken}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func TestApplyContextOverrides_Endpoints_HostFromFirstElement(t *testing.T) {
	t.Parallel()
	base := credTestTokenJob()
	with := endpointsEntryWithToken

	t.Run("a list with no host takes host and port from the first element", func(t *testing.T) {
		t.Parallel()
		eff, _, _, err := config.ApplyContextOverrides(base, with(map[string]any{
			"pve_endpoints": []any{epHostB + ":9000", epHostC},
		}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if eff.Host != epHostB || eff.Port != 9000 {
			t.Errorf("host=%q port=%d, want %s and 9000", eff.Host, eff.Port, epHostB)
		}
		if !slices.Equal(eff.Endpoints, []string{epHostB + ":9000", epHostC + ":8006"}) {
			t.Errorf("Endpoints = %v", eff.Endpoints)
		}
	})

	t.Run("a bare first element takes the entry's pve_port", func(t *testing.T) {
		t.Parallel()
		eff, _, _, err := config.ApplyContextOverrides(base, with(map[string]any{
			"pve_endpoints": []any{epHostB, epHostC},
			"pve_port":      float64(9100),
		}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if eff.Host != epHostB || eff.Port != 9100 {
			t.Errorf("host=%q port=%d, want %s and 9100", eff.Host, eff.Port, epHostB)
		}
		if !slices.Equal(eff.Endpoints, []string{epHostB + ":9100", epHostC + ":9100"}) {
			t.Errorf("Endpoints = %v", eff.Endpoints)
		}
	})

	t.Run("a bare IPv6 first element becomes the host", func(t *testing.T) {
		t.Parallel()
		eff, _, _, err := config.ApplyContextOverrides(base, with(map[string]any{
			"pve_endpoints": []any{"2001:db8::6", "[2001:db8::7]:8007"},
		}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if eff.Host != "2001:db8::6" || eff.Port != 8006 {
			t.Errorf("host=%q port=%d, want 2001:db8::6 and 8006", eff.Host, eff.Port)
		}
		if !slices.Equal(eff.Endpoints, []string{"[2001:db8::6]:8006", "[2001:db8::7]:8007"}) {
			t.Errorf("Endpoints = %v", eff.Endpoints)
		}
	})

}

func TestApplyContextOverrides_Endpoints_HostAgreement(t *testing.T) {
	t.Parallel()
	base := credTestTokenJob()
	with := endpointsEntryWithToken

	t.Run("a host that matches the first element passes", func(t *testing.T) {
		t.Parallel()
		eff, _, _, err := config.ApplyContextOverrides(base, with(map[string]any{
			"pve_host":      "PVE-B.example.",
			"pve_endpoints": []any{epHostB, epHostC},
		}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if eff.Port != 8006 || len(eff.Endpoints) != 2 {
			t.Errorf("port=%d Endpoints=%v", eff.Port, eff.Endpoints)
		}
	})

	t.Run("a host and port that match an explicit first-element port pass", func(t *testing.T) {
		t.Parallel()
		_, _, _, err := config.ApplyContextOverrides(base, with(map[string]any{
			"pve_host":      epHostB,
			"pve_port":      float64(9000),
			"pve_endpoints": []any{epHostB + ":9000", epHostC + ":9000"},
		}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("an empty host with a list takes the first element", func(t *testing.T) {
		t.Parallel()
		eff, _, _, err := config.ApplyContextOverrides(base, with(map[string]any{
			"pve_host":      "",
			"pve_endpoints": []any{epHostB, epHostC},
		}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if eff.Host != epHostB {
			t.Errorf("host = %q, want %s", eff.Host, epHostB)
		}
	})

	t.Run("a host that disagrees with the first element fails and names both keys", func(t *testing.T) {
		t.Parallel()
		eff, _, _, err := config.ApplyContextOverrides(base, with(map[string]any{
			"pve_host":      epHostC,
			"pve_endpoints": []any{epHostB, epHostA},
		}))
		if err == nil || eff != nil {
			t.Fatalf("eff=%v err=%v, want a disagreement error", eff, err)
		}
		for _, want := range []string{"pve.host", "pve.endpoints", epHostC, epHostB + ":8006"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), want)
			}
		}
		assertNoSecretInError(t, err)
	})

	t.Run("the same host at another port than the first element fails", func(t *testing.T) {
		t.Parallel()
		_, _, _, err := config.ApplyContextOverrides(base, with(map[string]any{
			"pve_host":      epHostB,
			"pve_endpoints": []any{epHostB + ":9000", epHostC},
		}))
		if err == nil || !strings.Contains(err.Error(), "pve.port: 9000") {
			t.Fatalf("err = %v, want it to say set pve.port: 9000", err)
		}
	})

	t.Run("a malformed element, a repeat, and an oversized list fail", func(t *testing.T) {
		t.Parallel()
		tooMany := make([]any, 17)
		for i := range tooMany {
			tooMany[i] = "h" + string(rune('a'+i)) + ".example"
		}
		for name, tc := range map[string]struct {
			list any
			want string
		}{
			"malformed": {[]any{epHostB, "bad host"}, "endpoints[1]"},
			"repeat":    {[]any{epHostB, "PVE-B.example.:8006"}, "repeats endpoints[0]"},
			"oversized": {tooMany, "at most 16"},
		} {
			_, _, _, err := config.ApplyContextOverrides(base, with(map[string]any{"pve_endpoints": tc.list}))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s: err = %v, want it to contain %q", name, err, tc.want)
			}
		}
	})
}

func TestApplyContextOverrides_Endpoints_ResetOnPrimaryChange(t *testing.T) {
	t.Parallel()
	base := endpointsTokenJob()

	t.Run("a new host clears the inherited list", func(t *testing.T) {
		t.Parallel()
		eff, _, _, err := config.ApplyContextOverrides(base, map[string]any{
			"pve_host":      epHostC,
			"pve_api_token": credTestEntryToken,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(eff.Endpoints) != 0 {
			t.Errorf("Endpoints = %v, want the entry's list to be just its own host", eff.Endpoints)
		}
	})

	t.Run("the job's second endpoint as host still clears the list", func(t *testing.T) {
		t.Parallel()
		eff, _, _, err := config.ApplyContextOverrides(base, map[string]any{
			"pve_host":      epHostB,
			"pve_api_token": credTestEntryToken,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(eff.Endpoints) != 0 {
			t.Errorf("Endpoints = %v, want none", eff.Endpoints)
		}
	})

	t.Run("a new port clears the inherited list", func(t *testing.T) {
		t.Parallel()
		eff, _, _, err := config.ApplyContextOverrides(base, map[string]any{
			"pve_port":      float64(8443),
			"pve_api_token": credTestEntryToken,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(eff.Endpoints) != 0 {
			t.Errorf("Endpoints = %v, want none", eff.Endpoints)
		}
	})

	t.Run("a new host that names its own list keeps it", func(t *testing.T) {
		t.Parallel()
		eff, _, _, err := config.ApplyContextOverrides(base, map[string]any{
			"pve_host":      epHostC,
			"pve_endpoints": []any{epHostC, epHostA},
			"pve_api_token": credTestEntryToken,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !slices.Equal(eff.Endpoints, []string{epHostC + ":8006", epHostA + ":8006"}) {
			t.Errorf("Endpoints = %v", eff.Endpoints)
		}
	})

	t.Run("the same primary keeps the inherited list and the job's credentials", func(t *testing.T) {
		t.Parallel()
		eff, _, _, err := config.ApplyContextOverrides(base, map[string]any{
			"pve_host":           "PVE-A.example.",
			"pve_node":           "pve9",
			"pve_network_bridge": "vmbr9",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !slices.Equal(eff.Endpoints, base.Endpoints) {
			t.Errorf("Endpoints = %v, want the job's list %v", eff.Endpoints, base.Endpoints)
		}
		if eff.APIToken != credTestJobToken {
			t.Error("an entry on the job's whole list lost the job's token")
		}
	})

	t.Run("the job is never mutated", func(t *testing.T) {
		t.Parallel()
		job := endpointsTokenJob()
		if _, _, _, err := config.ApplyContextOverrides(job, map[string]any{
			"pve_host":      epHostC,
			"pve_api_token": credTestEntryToken,
		}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(job.Endpoints) != 2 {
			t.Errorf("job Endpoints = %v", job.Endpoints)
		}
	})
}

func TestApplyContextOverrides_Endpoints_CredentialsFollowTheWholeList(t *testing.T) {
	t.Parallel()

	t.Run("the same list in other spellings keeps the job's token", func(t *testing.T) {
		t.Parallel()
		eff, _, _, err := config.ApplyContextOverrides(endpointsTokenJob(), map[string]any{
			"pve_endpoints": []any{"PVE-A.example.", epHostB + ":8006"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if eff.APIToken != credTestJobToken {
			t.Error("an entry that restates the job's list lost the job's token")
		}
	})

	t.Run("a list on a job without one keeps the credentials only when it is just the primary", func(t *testing.T) {
		t.Parallel()
		job := credTestTokenJob()
		eff, _, _, err := config.ApplyContextOverrides(job, map[string]any{"pve_endpoints": []any{epHostA}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if eff.APIToken != credTestJobToken {
			t.Error("a list holding only the job's primary lost the job's token")
		}
		_, _, _, err = config.ApplyContextOverrides(job, map[string]any{"pve_endpoints": []any{epHostA, epHostC}})
		if err == nil || !strings.Contains(err.Error(), "changes pve.endpoints but supplies no credentials") {
			t.Errorf("err = %v, want a pve.endpoints credential error", err)
		}
	})

}

func TestApplyContextOverrides_Endpoints_ChangedListSendsNoInheritedCredential(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		list []any
	}{
		"an added endpoint":                       {[]any{epHostA, epHostB, epHostC}},
		"a dropped endpoint":                      {[]any{epHostA}},
		"a replaced endpoint":                     {[]any{epHostA, epHostC}},
		"an added port":                           {[]any{epHostA, epHostB + ":9000"}},
		"a repeat of the primary at another port": {[]any{epHostA + ":8006", epHostB + ":8006", epHostA + ":9000"}},
	} {
		t.Run(name+" sends no inherited credential", func(t *testing.T) {
			t.Parallel()
			for jobName, job := range map[string]*config.CPIConfig{
				"token": endpointsTokenJob(), "password": endpointsPasswordJob(),
			} {
				eff, _, _, err := config.ApplyContextOverrides(job, map[string]any{"pve_endpoints": tc.list})
				if err == nil || eff != nil {
					t.Fatalf("%s job: eff=%v err=%v, want a credential error", jobName, eff, err)
				}
				want := `context entry for host "pve-a.example" changes pve.endpoints but supplies no credentials; ` +
					"set pve.api_token, or pve.user and pve.password"
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%s job: error = %q, want it to contain %q", jobName, err.Error(), want)
				}
				if strings.Contains(err.Error(), "pve.ca_cert") {
					t.Errorf("%s job: the error offers pve.ca_cert although the CA stays with the primary", jobName)
				}
				assertNoSecretInError(t, err)
			}
		})
	}

}

func TestApplyContextOverrides_Endpoints_EntryCredentialsReplaceTheGroup(t *testing.T) {
	t.Parallel()

	t.Run("an entry with its own token replaces the whole group", func(t *testing.T) {
		t.Parallel()
		for jobName, job := range map[string]*config.CPIConfig{
			"token": endpointsTokenJob(), "password": endpointsPasswordJob(),
		} {
			eff, applied, _, err := config.ApplyContextOverrides(job, map[string]any{
				"pve_endpoints": []any{epHostA, epHostB, epHostC},
				"pve_api_token": credTestEntryToken,
			})
			if err != nil {
				t.Fatalf("%s job: unexpected error: %v", jobName, err)
			}
			if eff.APIToken != credTestEntryToken || eff.User != "" || eff.Password != "" {
				t.Errorf("%s job: effective group is not exactly the entry's token", jobName)
			}
			if got := config.OverrideCredentialGroup(job, eff, applied); got != config.CredentialGroupEntry {
				t.Errorf("%s job: group = %q, want entry", jobName, got)
			}
		}
	})

	t.Run("an entry with its own user and password drops the job's token", func(t *testing.T) {
		t.Parallel()
		eff, _, _, err := config.ApplyContextOverrides(endpointsTokenJob(), map[string]any{
			"pve_endpoints": []any{epHostA, epHostC},
			"pve_user":      credTestEntryUser,
			"pve_password":  credTestEntryPassword,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if eff.APIToken != "" || eff.User != credTestEntryUser || eff.Password != credTestEntryPassword {
			t.Error("the job's token survived an added endpoint, or the entry's group did not apply")
		}
	})

	t.Run("a partial group on a changed list never borrows the job's", func(t *testing.T) {
		t.Parallel()
		_, _, _, err := config.ApplyContextOverrides(endpointsPasswordJob(), map[string]any{
			"pve_endpoints": []any{epHostA, epHostC},
			"pve_user":      credTestEntryUser,
		})
		if err == nil || !strings.Contains(err.Error(), "credential group is incomplete") {
			t.Fatalf("err = %v, want an incomplete-group error", err)
		}
		assertNoSecretInError(t, err)
	})

	t.Run("a reordered list changes the primary and reports pve.host", func(t *testing.T) {
		t.Parallel()
		_, _, _, err := config.ApplyContextOverrides(endpointsTokenJob(), map[string]any{
			"pve_endpoints": []any{epHostB, epHostA},
		})
		if err == nil || !strings.Contains(err.Error(), `for host "pve-b.example" changes pve.host but supplies no credentials`) {
			t.Fatalf("err = %v, want a pve.host credential error", err)
		}
	})

	t.Run("a bare element in the entry reads at the entry's port", func(t *testing.T) {
		t.Parallel()
		job := endpointsTokenJob()
		job.Port = 9000
		job.Endpoints = []string{epHostA + ":9000", epHostB + ":9000"}
		eff, _, _, err := config.ApplyContextOverrides(job, map[string]any{
			"pve_endpoints": []any{epHostA, epHostB},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if eff.APIToken != credTestJobToken {
			t.Error("bare elements at the job's port should equal the job's list")
		}
	})

	t.Run("an inherited stale list never lets credentials through", func(t *testing.T) {
		t.Parallel()
		job := endpointsTokenJob()
		stale := *job
		stale.Host = epHostB
		if config.SameEndpointSet(job, &stale) {
			t.Error("a list whose first element is not the primary compared equal to the job's list")
		}
	})
}

func TestApplyContextOverrides_Endpoints_CAFollowsThePrimary(t *testing.T) {
	t.Parallel()
	ca := selfSignedCAPEMForConfigTest(t)
	newJob := func() *config.CPIConfig {
		job := endpointsTokenJob()
		job.PVECACertPEM = ca
		return job
	}

	t.Run("an added endpoint keeps the job's CA", func(t *testing.T) {
		t.Parallel()
		eff, _, _, err := config.ApplyContextOverrides(newJob(), map[string]any{
			"pve_endpoints": []any{epHostA, epHostB, epHostC},
			"pve_api_token": credTestEntryToken,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if eff.PVECACertPEM != ca {
			t.Error("an entry that kept the job's primary fell back to the system trust pool")
		}
		if eff.APIToken != credTestEntryToken {
			t.Error("the entry's own token did not apply")
		}
	})

	t.Run("a dropped endpoint keeps the job's CA", func(t *testing.T) {
		t.Parallel()
		eff, _, _, err := config.ApplyContextOverrides(newJob(), map[string]any{
			"pve_endpoints": []any{epHostA},
			"pve_api_token": credTestEntryToken,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if eff.PVECACertPEM != ca {
			t.Error("the job's CA was dropped although the primary matches")
		}
	})

	t.Run("a new primary drops the job's CA", func(t *testing.T) {
		t.Parallel()
		eff, _, _, err := config.ApplyContextOverrides(newJob(), map[string]any{
			"pve_endpoints": []any{epHostC, epHostA},
			"pve_api_token": credTestEntryToken,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if eff.PVECACertPEM != "" {
			t.Error("the job's private CA followed a request to another primary")
		}
	})

	t.Run("a new primary with no credentials still offers pve.ca_cert", func(t *testing.T) {
		t.Parallel()
		_, _, _, err := config.ApplyContextOverrides(newJob(), map[string]any{
			"pve_endpoints": []any{epHostC},
		})
		if err == nil || !strings.Contains(err.Error(), "pve.ca_cert") {
			t.Fatalf("err = %v, want it to offer pve.ca_cert", err)
		}
	})
}

func TestSameEndpointSet(t *testing.T) {
	t.Parallel()
	mk := func(host string, port int, list ...string) *config.CPIConfig {
		return &config.CPIConfig{Host: host, Port: port, Endpoints: list}
	}
	tests := []struct {
		name string
		a, b *config.CPIConfig
		want bool
	}{
		{"no lists, same primary", mk("a", 8006), mk("A.", 0), true},
		{"no lists, other host", mk("a", 8006), mk("b", 8006), false},
		{"no lists, other port", mk("a", 8006), mk("a", 9000), false},
		{"a list of just the primary equals no list", mk("a", 8006), mk("a", 8006, "a:8006"), true},
		{"a bare element reads at the config's port", mk("a", 9000), mk("a", 9000, "a"), true},
		{"equal lists", mk("a", 8006, "a:8006", "b:8006"), mk("a", 8006, "A:8006", "b.:8006"), true},
		{"equal IPv6 spellings", mk("2001:db8::1", 8006, "[2001:db8::1]:8006", "[2001:db8::2]:8006"),
			mk("2001:DB8:0:0:0:0:0:1", 8006, "[2001:db8:0::1]:8006", "2001:db8::2"), true},
		{"an added element", mk("a", 8006, "a:8006"), mk("a", 8006, "a:8006", "b:8006"), false},
		{"a dropped element", mk("a", 8006, "a:8006", "b:8006"), mk("a", 8006), false},
		{"a different second element", mk("a", 8006, "a:8006", "b:8006"), mk("a", 8006, "a:8006", "c:8006"), false},
		{"a reordered tail", mk("a", 8006, "a:8006", "b:8006", "c:8006"), mk("a", 8006, "a:8006", "c:8006", "b:8006"), false},
		{"a different port on the second element", mk("a", 8006, "a:8006", "b:8006"), mk("a", 8006, "a:8006", "b:9000"), false},
		{"a first element that is not the primary", mk("a", 8006, "b:8006", "a:8006"), mk("a", 8006, "b:8006", "a:8006"), false},
		{"a malformed element", mk("a", 8006, "a:8006", "bad host"), mk("a", 8006, "a:8006", "bad host"), false},
		{"a signed port", mk("a", 8006, "a:8006", "b:+80"), mk("a", 8006, "a:8006", "b:+80"), false},
		{"a nil left side", nil, mk("a", 8006), false},
		{"a nil right side", mk("a", 8006), nil, false},
		{"two nils", nil, nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := config.SameEndpointSet(tc.a, tc.b); got != tc.want {
				t.Errorf("SameEndpointSet = %t, want %t", got, tc.want)
			}
			if tc.a != nil && tc.b != nil {
				if got := config.SameEndpointSet(tc.b, tc.a); got != tc.want {
					t.Errorf("SameEndpointSet reversed = %t, want %t", got, tc.want)
				}
			}
		})
	}
}

func TestSameEndpointSet_MatchesSameEndpointWithoutLists(t *testing.T) {
	t.Parallel()
	hosts := []string{"a", "A.", "b", "2001:db8::1", " a "}
	ports := []int{0, 8006, 9000}
	for _, ah := range hosts {
		for _, ap := range ports {
			for _, bh := range hosts {
				for _, bp := range ports {
					a := &config.CPIConfig{Host: ah, Port: ap}
					b := &config.CPIConfig{Host: bh, Port: bp}
					if config.SameEndpointSet(a, b) != config.SameEndpoint(a, b) {
						t.Errorf("SameEndpointSet and SameEndpoint disagree for %q:%d and %q:%d", ah, ap, bh, bp)
					}
				}
			}
		}
	}
}

func TestOverrideCredentialGroup_FollowsTheList(t *testing.T) {
	t.Parallel()
	job := endpointsTokenJob()
	same := *job
	if got := config.OverrideCredentialGroup(job, &same, nil); got != config.CredentialGroupInherited {
		t.Errorf("same list: group = %q, want inherited", got)
	}
	added := *job
	added.Endpoints = append(slices.Clone(job.Endpoints), epHostC+":8006")
	if got := config.OverrideCredentialGroup(job, &added, nil); got != config.CredentialGroupEntry {
		t.Errorf("added endpoint: group = %q, want entry", got)
	}
	plain := credTestTokenJob()
	plainSame := *plain
	if got := config.OverrideCredentialGroup(plain, &plainSame, nil); got != config.CredentialGroupInherited {
		t.Errorf("no lists: group = %q, want inherited", got)
	}
}

// endpointsDefaultedJob returns a token job built the way Load builds one:
// the host, port, and list are set as the operator wrote them and then go
// through ApplyDefaults and Validate. A job with no host and a first element
// that carries its own port ends up with Port set to that element's port.
func endpointsDefaultedJob(t *testing.T, host string, port int, list []string, caPEM string) *config.CPIConfig {
	t.Helper()
	job := credTestTokenJob()
	job.Host = host
	job.Port = port
	job.Endpoints = slices.Clone(list)
	job.PVECACertPEM = caPEM
	job.ApplyDefaults()
	if err := job.Validate(); err != nil {
		t.Fatalf("job config is invalid: %v", err)
	}
	return job
}

// The job in the tests below names no host, sets pve.port 8006, and writes a
// first element with port 9000. Its bare element reads at 8006, and its Port
// becomes 9000.
var (
	endpointsHostlessList = []string{epHostA + ":9000", epHostB}
	endpointsHostlessWant = []string{epHostA + ":9000", epHostB + ":8006"}
)

func TestApplyContextOverrides_Endpoints_BareElementsReadAtTheJobsPort(t *testing.T) {
	t.Parallel()
	list, wantList := endpointsHostlessList, endpointsHostlessWant

	t.Run("the fixture resolves as the job reads it", func(t *testing.T) {
		t.Parallel()
		job := endpointsDefaultedJob(t, "", 8006, list, "")
		if job.Host != epHostA || job.Port != 9000 || !slices.Equal(job.Endpoints, wantList) {
			t.Fatalf("job = %q:%d %v, want %q:9000 %v", job.Host, job.Port, job.Endpoints, epHostA, wantList)
		}
	})

	t.Run("an entry that copies the job's list keeps the job's credentials", func(t *testing.T) {
		t.Parallel()
		for name, jobPort := range map[string]int{"pve.port 8006": 8006, "pve.port unset": 0} {
			job := endpointsDefaultedJob(t, "", jobPort, list, "")
			eff, _, _, err := config.ApplyContextOverrides(job, map[string]any{"pve_endpoints": slices.Clone(list)})
			if err != nil {
				t.Fatalf("%s: unexpected error: %v", name, err)
			}
			if eff.APIToken != credTestJobToken {
				t.Errorf("%s: token = %q, want the job's", name, eff.APIToken)
			}
			if eff.Host != epHostA || eff.Port != 9000 || !slices.Equal(eff.Endpoints, wantList) {
				t.Errorf("%s: entry = %q:%d %v, want %q:9000 %v", name, eff.Host, eff.Port, eff.Endpoints,
					epHostA, wantList)
			}
			if got := config.OverrideCredentialGroup(job, eff, []string{"pve_endpoints"}); got != config.CredentialGroupInherited {
				t.Errorf("%s: group = %q, want inherited", name, got)
			}
		}
	})

	t.Run("an entry with its own token reads the bare element at the job's port", func(t *testing.T) {
		t.Parallel()
		job := endpointsDefaultedJob(t, "", 8006, list, "")
		eff, _, _, err := config.ApplyContextOverrides(job, endpointsEntryWithToken(map[string]any{
			"pve_endpoints": []any{epHostA + ":9000", epHostB},
		}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !slices.Equal(eff.Endpoints, wantList) {
			t.Errorf("Endpoints = %v, want %v", eff.Endpoints, wantList)
		}
		if eff.APIToken != credTestEntryToken {
			t.Errorf("token = %q, want the entry's", eff.APIToken)
		}
	})

	t.Run("naming the job's port reads the same list", func(t *testing.T) {
		t.Parallel()
		job := endpointsDefaultedJob(t, "", 8006, list, "")
		eff, _, _, err := config.ApplyContextOverrides(job, map[string]any{
			"pve_endpoints": slices.Clone(list),
			"pve_port":      8006,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if eff.APIToken != credTestJobToken || !slices.Equal(eff.Endpoints, wantList) {
			t.Errorf("token=%q Endpoints=%v, want the job's token and %v", eff.APIToken, eff.Endpoints, wantList)
		}
	})

}

func TestApplyContextOverrides_Endpoints_PortRulesForAnEntry(t *testing.T) {
	t.Parallel()
	list := endpointsHostlessList

	t.Run("an entry's own port fills the bare element and sends no job credential", func(t *testing.T) {
		t.Parallel()
		job := endpointsDefaultedJob(t, "", 8006, list, "")
		extra := map[string]any{"pve_endpoints": slices.Clone(list), "pve_port": 9100}
		_, _, _, err := config.ApplyContextOverrides(job, extra)
		if err == nil || !strings.Contains(err.Error(), "changes pve.endpoints") {
			t.Fatalf("err = %v, want the changes pve.endpoints refusal", err)
		}
		extra["pve_api_token"] = credTestEntryToken
		eff, _, _, err := config.ApplyContextOverrides(job, extra)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []string{epHostA + ":9000", epHostB + ":9100"}
		if !slices.Equal(eff.Endpoints, want) || eff.APIToken != credTestEntryToken {
			t.Errorf("Endpoints=%v token=%q, want %v and the entry's token", eff.Endpoints, eff.APIToken, want)
		}
	})

	t.Run("a new host reads at the job's port, not the first element's port", func(t *testing.T) {
		t.Parallel()
		job := endpointsDefaultedJob(t, "", 8006, list, "")
		eff, _, _, err := config.ApplyContextOverrides(job, map[string]any{
			"pve_host":      epHostC,
			"pve_api_token": credTestEntryToken,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if eff.Host != epHostC || eff.Port != 8006 {
			t.Errorf("entry = %q:%d, want %q:8006", eff.Host, eff.Port, epHostC)
		}
		if len(eff.Endpoints) != 0 {
			t.Errorf("Endpoints = %v, want the job's list dropped", eff.Endpoints)
		}
		named, _, _, err := config.ApplyContextOverrides(job, map[string]any{
			"pve_host":      epHostC,
			"pve_port":      9100,
			"pve_api_token": credTestEntryToken,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if named.Port != 9100 {
			t.Errorf("Port = %d, want the entry's 9100", named.Port)
		}
	})

	t.Run("a new host without credentials is refused", func(t *testing.T) {
		t.Parallel()
		job := endpointsDefaultedJob(t, "", 8006, list, "")
		_, _, _, err := config.ApplyContextOverrides(job, map[string]any{"pve_host": epHostC})
		if err == nil || !strings.Contains(err.Error(), "changes pve.host") {
			t.Fatalf("err = %v, want the changes pve.host refusal", err)
		}
	})

	t.Run("an entry that leaves the endpoint alone keeps the job's primary", func(t *testing.T) {
		t.Parallel()
		job := endpointsDefaultedJob(t, "", 8006, list, "")
		eff, _, _, err := config.ApplyContextOverrides(job, map[string]any{"pve_vm_storage": "other"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if eff.Host != epHostA || eff.Port != 9000 || eff.APIToken != credTestJobToken {
			t.Errorf("entry = %q:%d token %q, want the job's primary and token", eff.Host, eff.Port, eff.APIToken)
		}
	})

	t.Run("a job with a host reads exactly as before", func(t *testing.T) {
		t.Parallel()
		job := endpointsDefaultedJob(t, epHostA, 9000, []string{epHostA, epHostB + ":8006"}, "")
		eff, _, _, err := config.ApplyContextOverrides(job, map[string]any{
			"pve_endpoints": []any{epHostA, epHostB + ":8006"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if eff.APIToken != credTestJobToken || eff.Port != 9000 {
			t.Errorf("token=%q port=%d, want the job's token and port 9000", eff.APIToken, eff.Port)
		}
	})
}

func TestCanonicalEndpointHost_FoldsASCIICaseOnly(t *testing.T) {
	t.Parallel()
	const kelvinHost = "Kube.example"
	list := []string{"kube.example", epHostB}
	ca := selfSignedCAPEMForConfigTest(t)
	job := endpointsDefaultedJob(t, "kube.example", 0, list, ca)

	a := &config.CPIConfig{Host: "KUBE.Example.", Port: 8006}
	b := &config.CPIConfig{Host: "kube.example", Port: 0}
	if !config.SameEndpoint(a, b) {
		t.Error("ASCII case and a trailing dot must still compare equal")
	}
	if config.SameEndpoint(&config.CPIConfig{Host: kelvinHost}, b) {
		t.Error("U+212A KELVIN SIGN must not compare equal to k")
	}

	t.Run("a Kelvin-sign pve_host inherits neither the token nor the CA", func(t *testing.T) {
		t.Parallel()
		_, _, _, err := config.ApplyContextOverrides(job, map[string]any{"pve_host": kelvinHost})
		if err == nil || !strings.Contains(err.Error(), "changes pve.host") {
			t.Fatalf("err = %v, want the changes pve.host refusal", err)
		}
		eff, _, _, err := config.ApplyContextOverrides(job, map[string]any{
			"pve_host":      kelvinHost,
			"pve_api_token": credTestEntryToken,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if eff.APIToken != credTestEntryToken {
			t.Errorf("token = %q, want the entry's", eff.APIToken)
		}
		if eff.PVECACertPEM != "" {
			t.Error("the job's CA must not follow a primary that is not the job's")
		}
		if got := config.OverrideCredentialGroup(job, eff, []string{"pve_host", "pve_api_token"}); got != config.CredentialGroupEntry {
			t.Errorf("group = %q, want entry", got)
		}
	})

	t.Run("a Kelvin-sign pve_host beside the job's own list inherits nothing", func(t *testing.T) {
		t.Parallel()
		_, _, _, err := config.ApplyContextOverrides(job, map[string]any{
			"pve_host":      kelvinHost,
			"pve_endpoints": slices.Clone(list),
		})
		if err == nil {
			t.Fatal("want an error, got none")
		}
		assertNoSecretInError(t, err)
	})

	t.Run("an ASCII upper-case pve_host still inherits", func(t *testing.T) {
		t.Parallel()
		eff, _, _, err := config.ApplyContextOverrides(job, map[string]any{"pve_host": "KUBE.EXAMPLE"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if eff.APIToken != credTestJobToken || eff.PVECACertPEM != ca {
			t.Error("a host that differs only in ASCII case must keep the job's token and CA")
		}
	})
}

func TestApplyContextOverrides_Endpoints_NullIsUnset(t *testing.T) {
	t.Parallel()
	job := endpointsTokenJob()

	for name, extra := range map[string]map[string]any{
		"flat":   {"pve_endpoints": nil, "pve_vm_storage": "other"},
		"nested": {"pve": map[string]any{"endpoints": nil, "vm_storage": "other"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			eff, applied, _, err := config.ApplyContextOverrides(job, extra)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if slices.Contains(applied, "pve_endpoints") {
				t.Errorf("applied = %v, want pve_endpoints left out", applied)
			}
			if eff.APIToken != credTestJobToken || !slices.Equal(eff.Endpoints, job.Endpoints) {
				t.Errorf("token=%q Endpoints=%v, want the job's token and list", eff.APIToken, eff.Endpoints)
			}
		})
	}

	t.Run("null alone leaves the job config untouched", func(t *testing.T) {
		t.Parallel()
		eff, applied, _, err := config.ApplyContextOverrides(job, map[string]any{"pve_endpoints": nil})
		if err != nil || eff != job || len(applied) != 0 {
			t.Errorf("eff==job: %v, applied=%v, err=%v", eff == job, applied, err)
		}
	})

	t.Run("the caller's map keeps its null", func(t *testing.T) {
		t.Parallel()
		extra := map[string]any{"pve_endpoints": nil, "pve_vm_storage": "other"}
		if _, _, _, err := config.ApplyContextOverrides(job, extra); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, still := extra["pve_endpoints"]; !still {
			t.Error("ApplyContextOverrides removed a key from the caller's map")
		}
	})

	t.Run("an empty list is still a list", func(t *testing.T) {
		t.Parallel()
		_, _, _, err := config.ApplyContextOverrides(job, map[string]any{"pve_endpoints": []any{}})
		if err == nil || !strings.Contains(err.Error(), "changes pve.endpoints") {
			t.Fatalf("err = %v, want the changes pve.endpoints refusal", err)
		}
	})
}

func TestEndpointErrors_NeverEchoUserInfo(t *testing.T) {
	t.Parallel()
	secrets := []string{"SECRETPW", "pw@", "token=SECRET"}
	elements := []string{
		"https://u:SECRETPW@" + epHostA,
		"u:SECRETPW@" + epHostA,
		epHostA + ":pw@x",
		epHostA + ":8006:SECRETPW@x",
		epHostA + "?token=SECRET",
		"[u:SECRETPW@fd00::1]:8006",
		epHostA + ":8006#SECRETPW@frag",
	}
	check := func(t *testing.T, where string, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("%s: want an error, got none", where)
			return
		}
		for _, secret := range secrets {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s: error %q echoes %q", where, err.Error(), secret)
			}
		}
	}
	for _, el := range elements {
		job := endpointsTokenJob()
		_, _, _, err := config.ApplyContextOverrides(job, endpointsEntryWithToken(map[string]any{
			"pve_endpoints": []any{epHostA, el},
		}))
		check(t, "entry list "+el, err)

		bad := credTestTokenJob()
		bad.Endpoints = []string{epHostA, el}
		check(t, "job list "+el, bad.Validate())

		hostile := credTestTokenJob()
		hostile.Host = el
		hostile.Endpoints = []string{epHostA}
		check(t, "job host "+el, hostile.Validate())
	}
}

func TestApplyContextOverrides_Endpoints_HostDisagreementNamesTheInheritedPort(t *testing.T) {
	t.Parallel()
	job := credTestTokenJob()

	_, _, _, err := config.ApplyContextOverrides(job, map[string]any{
		"pve_host":      epHostB,
		"pve_endpoints": []any{epHostB + ":9000"},
		"pve_api_token": credTestEntryToken,
	})
	if err == nil {
		t.Fatal("want a disagreement error, got none")
	}
	if !strings.Contains(err.Error(), "with port 8006 (inherited from the job's pve.port)") {
		t.Errorf("error = %q, want it to say the port is inherited from the job", err.Error())
	}

	_, _, _, err = config.ApplyContextOverrides(job, map[string]any{
		"pve_host":      epHostB,
		"pve_port":      8006,
		"pve_endpoints": []any{epHostB + ":9000"},
		"pve_api_token": credTestEntryToken,
	})
	if err == nil {
		t.Fatal("want a disagreement error, got none")
	}
	if strings.Contains(err.Error(), "inherited") {
		t.Errorf("error = %q, want no inheritance note when the entry names pve.port", err.Error())
	}
}
