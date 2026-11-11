package config

import (
	"encoding/json"
	"slices"
	"testing"
)

// coldStartJob returns a valid job config that meets every cold-start
// condition: a single token-authenticated endpoint that verifies TLS against
// its own CA, with a two-AZ map.
func coldStartJob(t *testing.T) *CPIConfig {
	t.Helper()
	job := discoveryOverrideBase()
	job.PVECACertPEM = discoveryTestCA(t)
	job.Placement = &PlacementConfig{AZMap: map[string][]string{
		"az2": {"pve3", "pve4"},
		"az1": {"pve1", "pve2"},
	}}
	job.ApplyDefaults()
	if err := job.Validate(); err != nil {
		t.Fatalf("the cold-start job fixture does not validate: %v", err)
	}
	return job
}

// coldStartEntry applies a request context to the job the way a cpi-config
// entry does and returns the effective config.
func coldStartEntry(t *testing.T, job *CPIConfig, context map[string]any) *CPIConfig {
	t.Helper()
	raw, err := json.Marshal(context)
	if err != nil {
		t.Fatalf("encode context: %v", err)
	}
	eff, _, _, err := ApplyContextOverrides(job, requestExtra(t, string(raw)))
	if err != nil {
		t.Fatalf("ApplyContextOverrides(%v): %v", context, err)
	}
	return eff
}

// wantNames fails the test when got is not exactly want, in order.
func wantNames(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("ColdStartNodeNames = %v, want %v", got, want)
	}
}

// TestColdStartNodeNames_JobLevelBlockUsesItsOwnMapInAZOrder asserts that the
// job-level block returns its own map flattened in AZ-name order and then the
// operator's order, without duplicates or empty names.
func TestColdStartNodeNames_JobLevelBlockUsesItsOwnMapInAZOrder(t *testing.T) {
	t.Parallel()
	job := coldStartJob(t)
	wantNames(t, ColdStartNodeNames(job, job), "pve1", "pve2", "pve3", "pve4")

	job.Placement.AZMap = map[string][]string{
		"b": {"n2", "n1", "n2"},
		"a": {"n1", "", "n0"},
	}
	wantNames(t, ColdStartNodeNames(job, job), "n1", "n0", "n2")
}

// TestColdStartNodeNames_EachConditionTurnsItOff asserts that every condition
// on the config, taken alone, makes the function answer with no names.
func TestColdStartNodeNames_EachConditionTurnsItOff(t *testing.T) {
	t.Parallel()
	off := false
	cases := []struct {
		name   string
		mutate func(c *CPIConfig)
	}{
		{"discovery off", func(c *CPIConfig) { c.APIEndpointDiscovery = DiscoveryModeOff }},
		{"discovery mode unusable", func(c *CPIConfig) { c.APIEndpointDiscovery = "sometimes" }},
		{"auto with a second endpoint", func(c *CPIConfig) {
			c.APIEndpointDiscovery = DiscoveryModeAuto
			c.Endpoints = []string{"pve-a.example", "pve-b.example"}
		}},
		{"inherited credentials under looser TLS", func(c *CPIConfig) { c.inheritedCredsLooserTLS = true }},
		{"verify_ssl false", func(c *CPIConfig) { c.VerifySSL = &off }},
		{"no ca_cert", func(c *CPIConfig) { c.PVECACertPEM = "" }},
		{"blank ca_cert", func(c *CPIConfig) { c.PVECACertPEM = " \n" }},
		{"password auth", func(c *CPIConfig) {
			c.APIToken = ""
			c.User = "root"
			c.Realm = "pam"
			c.Password = "secret"
		}},
		{"password auth with discovery forced on", func(c *CPIConfig) {
			c.APIToken = ""
			c.Password = "secret"
			c.APIEndpointDiscovery = DiscoveryModeOn
		}},
		{"no credentials", func(c *CPIConfig) { c.APIToken = "" }},
		{"no placement block", func(c *CPIConfig) { c.Placement = nil }},
		{"nil az_map", func(c *CPIConfig) { c.Placement.AZMap = nil }},
		{"empty az_map", func(c *CPIConfig) { c.Placement.AZMap = map[string][]string{} }},
		{"az_map of empty lists", func(c *CPIConfig) { c.Placement.AZMap = map[string][]string{"az1": {}} }},
		{"az_map of empty names", func(c *CPIConfig) { c.Placement.AZMap = map[string][]string{"az1": {"", ""}} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			job := coldStartJob(t)
			wantNames(t, ColdStartNodeNames(job, job), "pve1", "pve2", "pve3", "pve4")
			tc.mutate(job)
			if got := ColdStartNodeNames(job, job); got != nil {
				t.Errorf("ColdStartNodeNames = %v, want none", got)
			}
		})
	}
}

// TestColdStartNodeNames_NilInputsGiveNoNames asserts the fail-closed answers
// for a nil config and for a nil job.
func TestColdStartNodeNames_NilInputsGiveNoNames(t *testing.T) {
	t.Parallel()
	job := coldStartJob(t)
	if got := ColdStartNodeNames(nil, job); got != nil {
		t.Errorf("nil config: got %v, want none", got)
	}
	if got := ColdStartNodeNames(nil, nil); got != nil {
		t.Errorf("nil config and job: got %v, want none", got)
	}
	// A config that is not the job and has no job to compare with owns its
	// map only by naming it.
	if got := ColdStartNodeNames(job, nil); got != nil {
		t.Errorf("nil job: got %v, want none", got)
	}
	entry := coldStartEntry(t, job, map[string]any{"pve_vm_storage": "other"})
	if got := ColdStartNodeNames(entry, nil); got != nil {
		t.Errorf("inheriting entry with a nil job: got %v, want none", got)
	}
	named := coldStartEntry(t, job, map[string]any{"pve_placement": map[string]any{
		"az_map": map[string]any{"x": []any{"nx1"}},
	}})
	wantNames(t, ColdStartNodeNames(named, nil), "nx1")
}

// TestColdStartNodeNames_EntryInheritingTheJobEndpointListUsesTheJobMap
// asserts that an entry on the job's endpoint list keeps the job's map, with
// or without a list written out, and that a copy of the job is not special.
func TestColdStartNodeNames_EntryInheritingTheJobEndpointListUsesTheJobMap(t *testing.T) {
	t.Parallel()
	job := coldStartJob(t)
	all := []string{"pve1", "pve2", "pve3", "pve4"}

	untouched, _, _, err := ApplyContextOverrides(job, map[string]any{"pve_unknown_key": "x"})
	if err != nil {
		t.Fatalf("ApplyContextOverrides: %v", err)
	}
	wantNames(t, ColdStartNodeNames(untouched, job), all...)

	other := coldStartEntry(t, job, map[string]any{"pve_vm_storage": "other"})
	if other == job {
		t.Fatal("the entry returned the job itself, so it cannot show the same-list path")
	}
	wantNames(t, ColdStartNodeNames(other, job), all...)

	sameList := coldStartEntry(t, job, map[string]any{"pve_endpoints": []any{"pve-a.example"}})
	wantNames(t, ColdStartNodeNames(sameList, job), all...)

	copyOfJob := *job
	wantNames(t, ColdStartNodeNames(&copyOfJob, job), all...)
	moved := *job
	moved.Host = "pve-z.example"
	if got := ColdStartNodeNames(&moved, job); got != nil {
		t.Errorf("a copy of the job on another host: got %v, want none", got)
	}
}

// TestColdStartNodeNames_EntryInheritingAMapUnderAnotherListGetsNone asserts
// that an entry which does not name pve_placement gets no names once its
// endpoint list differs from the job's, whatever else it names.
func TestColdStartNodeNames_EntryInheritingAMapUnderAnotherListGetsNone(t *testing.T) {
	t.Parallel()
	ownToken := "u@pve!cpi=other-token"
	cases := []struct {
		name    string
		context func(ca string) map[string]any
	}{
		{"another host with its own CA and token", func(ca string) map[string]any {
			return map[string]any{"pve_host": "pve-b.example", "pve_ca_cert": ca, "pve_api_token": ownToken}
		}},
		{"another port with its own CA and token", func(ca string) map[string]any {
			return map[string]any{"pve_port": 8007, "pve_ca_cert": ca, "pve_api_token": ownToken}
		}},
		{"the job's host plus an endpoint the job never named", func(ca string) map[string]any {
			return map[string]any{
				"pve_endpoints": []any{"pve-a.example", "pve-b.example"}, "pve_api_endpoint_discovery": "on",
				"pve_ca_cert": ca, "pve_api_token": ownToken,
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			job := coldStartJob(t)
			entry := coldStartEntry(t, job, tc.context(job.PVECACertPEM))
			if entry.Placement == nil || len(entry.Placement.AZMap) == 0 {
				t.Fatal("the entry lost the inherited map, so the case proves nothing")
			}
			if !entry.VerifySSLValue() || entry.APIToken == "" || entry.PVECACertPEM == "" {
				t.Fatal("the entry fails a condition other than ownership, so the case proves nothing")
			}
			if mode, _ := EffectiveDiscoveryMode(entry); mode != DiscoveryModeOn {
				t.Fatalf("discovery resolves to %q, so the case proves nothing", mode)
			}
			if got := ColdStartNodeNames(entry, job); got != nil {
				t.Errorf("ColdStartNodeNames = %v, want none", got)
			}
		})
	}
}

// TestColdStartNodeNames_EntryNamingItsPlacementUsesItsOwnMap asserts that an
// entry which names pve_placement gets its own map even under another list,
// and that the names are the entry's and not the job's.
func TestColdStartNodeNames_EntryNamingItsPlacementUsesItsOwnMap(t *testing.T) {
	t.Parallel()
	job := coldStartJob(t)
	placement := map[string]any{"az_map": map[string]any{"east": []any{"e2", "e1"}, "azz": []any{"e1", "e3"}}}
	entry := coldStartEntry(t, job, map[string]any{
		"pve_host": "pve-b.example", "pve_ca_cert": job.PVECACertPEM, "pve_api_token": "u@pve!cpi=other-token",
		"pve_placement": placement,
	})
	wantNames(t, ColdStartNodeNames(entry, job), "e1", "e3", "e2")

	sameHost := coldStartEntry(t, job, map[string]any{"pve_placement": placement})
	wantNames(t, ColdStartNodeNames(sameHost, job), "e1", "e3", "e2")

	if got := ColdStartNodeNames(job, job); !slices.Equal(got, []string{"pve1", "pve2", "pve3", "pve4"}) {
		t.Errorf("the job's own names changed to %v after an entry named its placement", got)
	}
}

// TestColdStartNodeNames_EntryPassedAsItsOwnJobGainsNothing asserts that
// passing one config for both arguments grants ownership only to the
// job-level block. An entry that inherits the job's map under another list
// gets none, so a caller that has no job to hand over cannot turn an
// inherited map into the entry's own by repeating the entry.
func TestColdStartNodeNames_EntryPassedAsItsOwnJobGainsNothing(t *testing.T) {
	t.Parallel()
	job := coldStartJob(t)
	all := []string{"pve1", "pve2", "pve3", "pve4"}
	wantNames(t, ColdStartNodeNames(job, job), all...)

	entry := coldStartEntry(t, job, map[string]any{
		"pve_host": "pve-b.example", "pve_ca_cert": job.PVECACertPEM, "pve_api_token": "u@pve!cpi=other-token",
	})
	if entry.Placement == nil || len(entry.Placement.AZMap) == 0 || entry.placementNamedByEntry {
		t.Fatal("the entry must inherit the job's map without naming it, or the case proves nothing")
	}
	wantNames(t, ColdStartNodeNames(entry, job))
	wantNames(t, ColdStartNodeNames(entry, entry))

	// An entry on the job's own list keeps the job's map under the real job,
	// and gets none when it is passed in the job's place, because nothing then
	// proves it is the job's list.
	sameList := coldStartEntry(t, job, map[string]any{"pve_vm_storage": "other"})
	wantNames(t, ColdStartNodeNames(sameList, job), all...)
	wantNames(t, ColdStartNodeNames(sameList, sameList))

	// An entry that names its placement owns its map under any job argument.
	named := coldStartEntry(t, job, map[string]any{
		"pve_host": "pve-b.example", "pve_ca_cert": job.PVECACertPEM, "pve_api_token": "u@pve!cpi=other-token",
		"pve_placement": map[string]any{"az_map": map[string]any{"east": []any{"e1"}}},
	})
	wantNames(t, ColdStartNodeNames(named, named), "e1")

	// The job-level block owns its map through its endpoint list, whether it
	// writes the list out or leaves the primary alone.
	listed := coldStartJob(t)
	listed.Endpoints = []string{"pve-a.example", "pve-c.example"}
	listed.APIEndpointDiscovery = DiscoveryModeOn
	if err := listed.Validate(); err != nil {
		t.Fatalf("the listed job does not validate: %v", err)
	}
	wantNames(t, ColdStartNodeNames(listed, listed), all...)
}

// TestColdStartNodeNames_EntryRestatingTheJobMapUnderItsOwnHostOwnsIt asserts
// that an entry which writes pve_placement owns the map even when the value
// equals the job's, because the entry names the key itself. An entry that
// leaves the key out on the same host gets none, so the two differ.
func TestColdStartNodeNames_EntryRestatingTheJobMapUnderItsOwnHostOwnsIt(t *testing.T) {
	t.Parallel()
	job := coldStartJob(t)
	restated := map[string]any{"az_map": map[string]any{
		"az1": []any{"pve1", "pve2"},
		"az2": []any{"pve3", "pve4"},
	}}
	own := map[string]any{
		"pve_host": "pve-b.example", "pve_ca_cert": job.PVECACertPEM, "pve_api_token": "u@pve!cpi=other-token",
	}
	withMap := map[string]any{"pve_placement": restated}
	for k, v := range own {
		withMap[k] = v
	}
	named := coldStartEntry(t, job, withMap)
	inherited := coldStartEntry(t, job, own)
	wantNames(t, ColdStartNodeNames(named, job), "pve1", "pve2", "pve3", "pve4")
	wantNames(t, ColdStartNodeNames(inherited, job))
}

// TestColdStartNodeNames_EntryNamingItsPlacementStillMeetsEveryOtherCondition
// asserts that naming pve_placement settles ownership only, and that the
// entry's own credential, CA, TLS, and mode settings still decide the rest.
func TestColdStartNodeNames_EntryNamingItsPlacementStillMeetsEveryOtherCondition(t *testing.T) {
	t.Parallel()
	placement := map[string]any{"az_map": map[string]any{"east": []any{"e1"}}}
	cases := []struct {
		name    string
		context func(ca string) map[string]any
		want    []string
	}{
		{"complete entry", func(ca string) map[string]any {
			return map[string]any{
				"pve_host": "pve-b.example", "pve_ca_cert": ca, "pve_api_token": "u@pve!cpi=t",
				"pve_placement": placement,
			}
		}, []string{"e1"}},
		{"new host drops the job's CA", func(string) map[string]any {
			return map[string]any{
				"pve_host": "pve-b.example", "pve_api_token": "u@pve!cpi=t", "pve_placement": placement,
			}
		}, nil},
		{"new host with a password", func(ca string) map[string]any {
			return map[string]any{
				"pve_host": "pve-b.example", "pve_ca_cert": ca, "pve_user": "root", "pve_password": "pw",
				"pve_placement": placement,
			}
		}, nil},
		{"new host with verify_ssl false", func(ca string) map[string]any {
			return map[string]any{
				"pve_host": "pve-b.example", "pve_ca_cert": ca, "pve_api_token": "u@pve!cpi=t",
				"pve_verify_ssl": false, "pve_placement": placement,
			}
		}, nil},
		{"new host with discovery off", func(ca string) map[string]any {
			return map[string]any{
				"pve_host": "pve-b.example", "pve_ca_cert": ca, "pve_api_token": "u@pve!cpi=t",
				"pve_api_endpoint_discovery": "off", "pve_placement": placement,
			}
		}, nil},
		{"a password replaces the job's token on the job host", func(string) map[string]any {
			return map[string]any{"pve_user": "root", "pve_password": "pw", "pve_placement": placement}
		}, nil},
		{"an empty map", func(ca string) map[string]any {
			return map[string]any{
				"pve_host": "pve-b.example", "pve_ca_cert": ca, "pve_api_token": "u@pve!cpi=t",
				"pve_placement": map[string]any{"az_map": map[string]any{}},
			}
		}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			job := coldStartJob(t)
			entry := coldStartEntry(t, job, tc.context(job.PVECACertPEM))
			if !entry.placementNamedByEntry {
				t.Fatal("the entry named pve_placement but the flag is down")
			}
			if tc.want == nil {
				if got := ColdStartNodeNames(entry, job); got != nil {
					t.Errorf("ColdStartNodeNames = %v, want none", got)
				}
				return
			}
			wantNames(t, ColdStartNodeNames(entry, job), tc.want...)
		})
	}
}

// TestColdStartNodeNames_EverySourceOfACredentialAndACA asserts that the
// conditions follow the effective config for each place a CA or a token can
// come from, which are the job, the entry, and the job's own after an
// endpoint change cleared them.
func TestColdStartNodeNames_EverySourceOfACredentialAndACA(t *testing.T) {
	t.Parallel()
	all := []string{"pve1", "pve2", "pve3", "pve4"}
	t.Run("token and CA both inherited from the job", func(t *testing.T) {
		t.Parallel()
		job := coldStartJob(t)
		entry := coldStartEntry(t, job, map[string]any{"pve_vm_storage": "other"})
		wantNames(t, ColdStartNodeNames(entry, job), all...)
	})
	t.Run("token named by the entry, CA inherited", func(t *testing.T) {
		t.Parallel()
		job := coldStartJob(t)
		entry := coldStartEntry(t, job, map[string]any{"pve_api_token": "u@pve!cpi=own"})
		wantNames(t, ColdStartNodeNames(entry, job), all...)
	})
	t.Run("token and CA both named by the entry on the job's endpoints", func(t *testing.T) {
		t.Parallel()
		job := coldStartJob(t)
		entry := coldStartEntry(t, job, map[string]any{
			"pve_api_token": "u@pve!cpi=own", "pve_ca_cert": discoveryTestCA(t),
		})
		wantNames(t, ColdStartNodeNames(entry, job), all...)
	})
	t.Run("job without a CA, entry supplies one with its own token", func(t *testing.T) {
		t.Parallel()
		job := coldStartJob(t)
		job.PVECACertPEM = ""
		wantNames(t, ColdStartNodeNames(job, job))
		entry := coldStartEntry(t, job, map[string]any{
			"pve_api_token": "u@pve!cpi=own", "pve_ca_cert": discoveryTestCA(t),
		})
		wantNames(t, ColdStartNodeNames(entry, job), all...)
	})
	t.Run("job without a CA, entry inherits", func(t *testing.T) {
		t.Parallel()
		job := coldStartJob(t)
		job.PVECACertPEM = ""
		entry := coldStartEntry(t, job, map[string]any{"pve_vm_storage": "other"})
		wantNames(t, ColdStartNodeNames(entry, job))
	})
	t.Run("entry clears the CA with an empty string", func(t *testing.T) {
		t.Parallel()
		job := coldStartJob(t)
		entry := coldStartEntry(t, job, map[string]any{"pve_api_token": "u@pve!cpi=own", "pve_ca_cert": ""})
		wantNames(t, ColdStartNodeNames(entry, job))
	})
	t.Run("job on a password, entry supplies a token", func(t *testing.T) {
		t.Parallel()
		job := coldStartJob(t)
		job.APIToken = ""
		job.User, job.Realm, job.Password = "root", "pam", "pw"
		wantNames(t, ColdStartNodeNames(job, job))
		entry := coldStartEntry(t, job, map[string]any{"pve_api_token": "u@pve!cpi=own"})
		wantNames(t, ColdStartNodeNames(entry, job), all...)
	})
	t.Run("entry looser TLS under inherited credentials", func(t *testing.T) {
		t.Parallel()
		job := coldStartJob(t)
		entry := coldStartEntry(t, job, map[string]any{"pve_verify_ssl": false})
		wantNames(t, ColdStartNodeNames(entry, job))
	})
	t.Run("entry names another CA under inherited credentials", func(t *testing.T) {
		t.Parallel()
		job := coldStartJob(t)
		entry := coldStartEntry(t, job, map[string]any{"pve_ca_cert": discoveryTestCA(t)})
		wantNames(t, ColdStartNodeNames(entry, job))
	})
	t.Run("entry sets discovery off and then on", func(t *testing.T) {
		t.Parallel()
		job := coldStartJob(t)
		entry := coldStartEntry(t, job, map[string]any{"pve_api_endpoint_discovery": "off"})
		wantNames(t, ColdStartNodeNames(entry, job))
		job.APIEndpointDiscovery = DiscoveryModeOff
		wantNames(t, ColdStartNodeNames(job, job))
		entry = coldStartEntry(t, job, map[string]any{"pve_api_endpoint_discovery": "on"})
		wantNames(t, ColdStartNodeNames(entry, job), all...)
	})
}

// TestApplyContextOverrides_PlacementOwnershipFlag asserts that the flag
// rises for an entry that names pve_placement, stays down for one that does
// not, and does not pass from a base to the config built from it.
func TestApplyContextOverrides_PlacementOwnershipFlag(t *testing.T) {
	t.Parallel()
	job := coldStartJob(t)
	if job.placementNamedByEntry {
		t.Fatal("the job-level config starts with the flag raised")
	}
	named := coldStartEntry(t, job, map[string]any{"pve_placement": map[string]any{
		"az_map": map[string]any{"x": []any{"nx1"}},
	}})
	if !named.placementNamedByEntry {
		t.Error("the flag stayed down for an entry that named pve_placement")
	}
	if job.placementNamedByEntry {
		t.Error("the entry raised the flag on the job-level config")
	}
	unnamed := coldStartEntry(t, job, map[string]any{"pve_vm_storage": "other"})
	if unnamed.placementNamedByEntry {
		t.Error("the flag rose for an entry that did not name pve_placement")
	}
	if job.entryEffective || !named.entryEffective || !unnamed.entryEffective {
		t.Errorf("entryEffective = job %v, named %v, unnamed %v, want false, true, true",
			job.entryEffective, named.entryEffective, unnamed.entryEffective)
	}

	// An effective config used as a base must not pass its ownership on to an
	// entry that moves to another host without naming its own placement.
	moved := coldStartEntry(t, named, map[string]any{
		"pve_host": "pve-b.example", "pve_ca_cert": job.PVECACertPEM, "pve_api_token": "u@pve!cpi=t",
	})
	if moved.placementNamedByEntry {
		t.Error("the flag passed from a base to the entry built on it")
	}
	if got := ColdStartNodeNames(moved, job); got != nil {
		t.Errorf("ColdStartNodeNames = %v, want none", got)
	}
}
