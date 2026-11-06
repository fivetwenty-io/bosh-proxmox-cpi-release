package handlers_test

import "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"

// haChoiceOf returns a pointer to the pve.ha enum value v.
func haChoiceOf(v string) *config.HAChoice {
	c := config.HAChoice(v)
	return &c
}

// haModeAuto is a pve.ha block with mode auto and every other key at its
// compiled default. It enables the default DLB sentinel AZ and nothing else, so
// no HA rule is written until a key is set to "on".
func haModeAuto() *config.HAConfig {
	return &config.HAConfig{Mode: haChoiceOf("auto")}
}

// haModeOff is a pve.ha block that sets every writing key to its on value and
// the mode to off, which must leave every VM untouched.
func haModeOff() *config.HAConfig {
	t := true
	return &config.HAConfig{
		Mode:         haChoiceOf("off"),
		AntiAffinity: haChoiceOf("off"),
		PinAZ:        haChoiceOf("off"),
		DLB:          &config.HADLBConfig{Enabled: &t},
	}
}

// haDLBEnabled is a pve.ha block with mode auto and the DLB master switch on.
func haDLBEnabled() *config.HAConfig {
	t := true
	return &config.HAConfig{Mode: haChoiceOf("auto"), DLB: &config.HADLBConfig{Enabled: &t}}
}

// haAntiAffinityOn is a pve.ha block with mode auto and anti_affinity "on".
func haAntiAffinityOn() *config.HAConfig {
	return &config.HAConfig{Mode: haChoiceOf("auto"), AntiAffinity: haChoiceOf("on")}
}
