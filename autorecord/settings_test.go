package autorecord

import "testing"

func TestDefaultSettings_DisabledAndValid(t *testing.T) {
	s := DefaultSettings()
	if s.Enabled {
		t.Fatalf("DefaultSettings().Enabled = true, want false (disabled by default)")
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("DefaultSettings() failed its own Validate(): %v", err)
	}
	if s.SchemaVersion != SettingsSchemaVersion {
		t.Fatalf("SchemaVersion=%d, want %d", s.SchemaVersion, SettingsSchemaVersion)
	}
}

func TestSettings_Validate(t *testing.T) {
	valid := DefaultSettings()

	cases := []struct {
		name    string
		mutate  func(*Settings)
		wantErr bool
	}{
		{"valid default", func(s *Settings) {}, false},
		{"negative start speed", func(s *Settings) { s.StartGroundspeedKnots = -1 }, true},
		{"start speed too large", func(s *Settings) { s.StartGroundspeedKnots = 1000 }, true},
		{"negative stop speed", func(s *Settings) { s.StopGroundspeedKnots = -1 }, true},
		{"stop speed equal to start speed", func(s *Settings) { s.StopGroundspeedKnots = s.StartGroundspeedKnots }, true},
		{"stop speed above start speed", func(s *Settings) { s.StopGroundspeedKnots = s.StartGroundspeedKnots + 1 }, true},
		{"negative start dwell", func(s *Settings) { s.StartDwellSeconds = -1 }, true},
		{"start dwell too large", func(s *Settings) { s.StartDwellSeconds = maxBoundSeconds + 1 }, true},
		{"negative stop dwell", func(s *Settings) { s.StopDwellSeconds = -1 }, true},
		{"negative gps loss grace", func(s *Settings) { s.GPSLossGraceSeconds = -1 }, true},
		{"negative restart cooldown", func(s *Settings) { s.RestartCooldownSeconds = -1 }, true},
		{"negative minimum duration", func(s *Settings) { s.MinimumRecordingDurationSeconds = -1 }, true},
		{"zero minimum duration allowed (disables it)", func(s *Settings) { s.MinimumRecordingDurationSeconds = 0 }, false},
		{"bound edge exactly at max is allowed", func(s *Settings) { s.StartDwellSeconds = maxBoundSeconds }, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := valid
			tc.mutate(&s)
			err := s.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("Validate(): want error, got nil (settings=%+v)", s)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Validate(): want nil, got %v (settings=%+v)", err, s)
			}
		})
	}
}
