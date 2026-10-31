package manifest

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

func TestEffective(t *testing.T) {
	one, zero, five := 1, 0, 5

	cases := []struct {
		name     string
		spec     AppSpec
		replicas int
		warm     int
		policy   string
		copies   int
	}{
		{"all defaults", AppSpec{}, 1, 1, LBLeastConn, 2},
		{"replicas 1", AppSpec{Replicas: 1}, 1, 1, LBLeastConn, 2},
		{"no spares", AppSpec{WarmReplicas: &zero}, 1, 0, LBLeastConn, 1},
		{"explicit one spare", AppSpec{WarmReplicas: &one}, 1, 1, LBLeastConn, 2},
		{"several of both", AppSpec{Replicas: 3, WarmReplicas: &five, LBPolicy: LBRoundRobin}, 3, 5, LBRoundRobin, 8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.spec.EffectiveReplicas(); got != tc.replicas {
				t.Errorf("EffectiveReplicas = %d, want %d", got, tc.replicas)
			}
			if got := tc.spec.EffectiveWarm(); got != tc.warm {
				t.Errorf("EffectiveWarm = %d, want %d", got, tc.warm)
			}
			if got := tc.spec.EffectiveLBPolicy(); got != tc.policy {
				t.Errorf("EffectiveLBPolicy = %q, want %q", got, tc.policy)
			}
			if got := tc.spec.CopyCount(); got != tc.copies {
				t.Errorf("CopyCount = %d, want %d", got, tc.copies)
			}
		})
	}
}

func TestLoad_WarmReplicasKeepsZero(t *testing.T) {
	dir := t.TempDir()
	entry := filepath.Join(dir, "app.R")
	os.WriteFile(entry, nil, 0o644)

	load := func(extra string) AppSpec {
		t.Helper()
		spec, err := Load(writeManifest(t, dir, "app-group: g\napp-name: n\nentrypoint-file: app.R\nport: 8881\n"+extra))
		if err != nil {
			t.Fatal(err)
		}
		return spec
	}

	if got := load("warm-replicas: 0\n"); got.WarmReplicas == nil || *got.WarmReplicas != 0 || got.EffectiveWarm() != 0 {
		t.Errorf("warm-replicas: 0 read as %v, want a pointer to 0", got.WarmReplicas)
	}
	if got := load(""); got.WarmReplicas != nil || got.EffectiveWarm() != DefaultWarmReplicas {
		t.Errorf("no warm-replicas read as %v, want nil and the default", got.WarmReplicas)
	}
	if got := load("replicas: 2\nwarm-replicas: 3\nlb-policy: first\n"); got.EffectiveReplicas() != 2 || got.EffectiveWarm() != 3 || got.LBPolicy != LBFirst {
		t.Errorf("read %+v", got)
	}
}

func TestEffectiveDrainTimeout(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{"", 15 * time.Minute},
		{"0", 0},
		{"0s", 0},
		{"90s", 90 * time.Second},
		{"1h30m", 90 * time.Minute},
		{"bad", 15 * time.Minute},
		{"-5m", 15 * time.Minute},
		{"48h", 24 * time.Hour},
	} {
		if got := (AppSpec{DrainTimeout: tc.in}).EffectiveDrainTimeout(); got != tc.want {
			t.Errorf("EffectiveDrainTimeout(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestEffectiveMaxMemory(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
	}{
		{"", 0},
		{"512M", 512 << 20},
		{"2g", 2 << 30},
		{"512", 0},
		{"bad", 0},
		{"-5M", 0},
		{"99999999999T", 0},
	} {
		if got := (AppSpec{MaxMemory: tc.in}).EffectiveMaxMemory(); got != tc.want {
			t.Errorf("EffectiveMaxMemory(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// YAML reads "drain-timeout: 0" as a number, which a string field takes as
// "0", and that parses.
func TestDrainTimeout_YAMLZero(t *testing.T) {
	var spec AppSpec
	if err := yaml.Unmarshal([]byte("drain-timeout: 0\n"), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.DrainTimeout != "0" || spec.EffectiveDrainTimeout() != 0 {
		t.Errorf("drain-timeout: 0 -> %q, %v; want \"0\" and 0", spec.DrainTimeout, spec.EffectiveDrainTimeout())
	}
}
