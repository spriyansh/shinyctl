package manifest

import (
	"os"
	"path/filepath"
	"testing"
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
