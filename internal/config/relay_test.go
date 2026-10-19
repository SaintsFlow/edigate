package config

import (
	"testing"
	"time"
)

func TestLoadRelayAndChaosSettings(t *testing.T) {
	t.Setenv("EDIGATE_DB_URL", "postgres://example/db")
	for _, name := range []string{
		"EDIGATE_ONEC_URL", "EDIGATE_RELAY_WORKERS", "EDIGATE_TIME_SCALE", "EDIGATE_CHAOS_EXIT_BEFORE_SEND",
		"EDIGATE_CHAOS_EXIT_AFTER_APPLY", "EDIGATE_CHAOS_CRASH_AFTER", "EDIGATE_CHAOS_SEED",
	} {
		t.Setenv(name, "")
	}
	c, err := Load("")
	if err != nil || c.RelayWorkers != 2 || c.TimeScale != 1 || c.OnecURL != "" ||
		c.ChaosExitBeforeSend != 0 || c.ChaosExitAfterApply != 0 || c.ChaosCrashAfter != 0 || c.ChaosSeed != nil {
		t.Fatalf("defaults: %+v %v", c, err)
	}

	t.Setenv("EDIGATE_ONEC_URL", "http://mock-onec:8081")
	t.Setenv("EDIGATE_RELAY_WORKERS", "4")
	t.Setenv("EDIGATE_TIME_SCALE", "20")
	t.Setenv("EDIGATE_CHAOS_EXIT_BEFORE_SEND", "0.2")
	t.Setenv("EDIGATE_CHAOS_EXIT_AFTER_APPLY", "0.3")
	t.Setenv("EDIGATE_CHAOS_CRASH_AFTER", "11s")
	t.Setenv("EDIGATE_CHAOS_SEED", "42")
	c, err = Load("")
	if err != nil || c.OnecURL != "http://mock-onec:8081" || c.RelayWorkers != 4 || c.TimeScale != 20 ||
		c.ChaosExitBeforeSend != 0.2 || c.ChaosExitAfterApply != 0.3 || c.ChaosCrashAfter != 11*time.Second ||
		c.ChaosSeed == nil || *c.ChaosSeed != 42 {
		t.Fatalf("settings: %+v %v", c, err)
	}

	for name, bad := range map[string]string{
		"EDIGATE_RELAY_WORKERS": "0", "EDIGATE_TIME_SCALE": "-1", "EDIGATE_CHAOS_EXIT_AFTER_APPLY": "1.5",
		"EDIGATE_CHAOS_CRASH_AFTER": "soon", "EDIGATE_CHAOS_SEED": "x",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("EDIGATE_ONEC_URL", "")
			t.Setenv(name, bad)
			if _, err := Load(""); err == nil {
				t.Errorf("%s=%s must fail", name, bad)
			}
		})
	}
}
