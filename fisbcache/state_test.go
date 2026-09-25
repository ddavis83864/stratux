package fisbcache

import "testing"

func TestDetermineState_DisabledAlwaysWins(t *testing.T) {
	in := StateInputs{
		Enabled:       false,
		RecoveryError: true, // even a hard error must not override "disabled"
		TrustedTime:   false,
	}
	if got := DetermineState(in); got != StateDisabled {
		t.Errorf("got %q, want DISABLED regardless of every other input", got)
	}
}

func TestDetermineState_Transitions(t *testing.T) {
	cases := []struct {
		name string
		in   StateInputs
		want State
	}{
		{"error", StateInputs{Enabled: true, RecoveryError: true}, StateError},
		{"startup grace", StateInputs{Enabled: true, StartupRecoveryComplete: false}, StateStartupGrace},
		{"waiting for trusted time", StateInputs{Enabled: true, StartupRecoveryComplete: true, TrustedTime: false}, StateWaitingForTrustedTime},
		{"degraded", StateInputs{Enabled: true, StartupRecoveryComplete: true, TrustedTime: true, HasNonFatalErrors: true}, StateDegraded},
		{"read only", StateInputs{Enabled: true, StartupRecoveryComplete: true, TrustedTime: true, ReadOnly: true}, StateReadOnly},
		{"pressure inhibited", StateInputs{Enabled: true, StartupRecoveryComplete: true, TrustedTime: true, StoragePressureProhibited: true}, StatePressureInhibited},
		{"live", StateInputs{Enabled: true, StartupRecoveryComplete: true, TrustedTime: true}, StateLive},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DetermineState(c.in); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
