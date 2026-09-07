package configbackup

import (
	"testing"

	"github.com/stratux/stratux/calprofile"
)

func baseToken() ConfirmationToken {
	return ConfirmationToken{
		ContentChecksum:         "content-abc",
		CurrentStateFingerprint: "fp-abc",
		BootSessionID:           "boot-1",
		IssuedAtMonotonic:       100,
		ExpiresAtMonotonic:      160,
	}
}

func TestVerifyToken_ValidWithinWindow(t *testing.T) {
	tok := baseToken()
	if err := VerifyToken(tok, "content-abc", "fp-abc", "boot-1", 120); err != nil {
		t.Fatalf("expected valid token, got %v", err)
	}
}

func TestVerifyToken_Expired(t *testing.T) {
	tok := baseToken()
	if err := VerifyToken(tok, "content-abc", "fp-abc", "boot-1", 200); err != ErrTokenExpired {
		t.Fatalf("expected ErrTokenExpired, got %v", err)
	}
}

func TestVerifyToken_AlreadyUsed(t *testing.T) {
	tok := baseToken()
	tok.Used = true
	if err := VerifyToken(tok, "content-abc", "fp-abc", "boot-1", 120); err != ErrTokenAlreadyUsed {
		t.Fatalf("expected ErrTokenAlreadyUsed, got %v", err)
	}
}

func TestVerifyToken_BootSessionChanged(t *testing.T) {
	tok := baseToken()
	if err := VerifyToken(tok, "content-abc", "fp-abc", "boot-2", 120); err != ErrTokenBootSessionChanged {
		t.Fatalf("expected ErrTokenBootSessionChanged, got %v", err)
	}
}

func TestVerifyToken_ContentChanged(t *testing.T) {
	tok := baseToken()
	if err := VerifyToken(tok, "content-different", "fp-abc", "boot-1", 120); err != ErrTokenContentChanged {
		t.Fatalf("expected ErrTokenContentChanged, got %v", err)
	}
}

func TestVerifyToken_StateChanged(t *testing.T) {
	tok := baseToken()
	if err := VerifyToken(tok, "content-abc", "fp-different", "boot-1", 120); err != ErrTokenStateChanged {
		t.Fatalf("expected ErrTokenStateChanged, got %v", err)
	}
}

func TestFingerprint_DeterministicAndSensitive(t *testing.T) {
	a := testCurrentState()
	fp1, err := Fingerprint(a)
	if err != nil {
		t.Fatal(err)
	}
	fp2, err := Fingerprint(a)
	if err != nil {
		t.Fatal(err)
	}
	if fp1 != fp2 {
		t.Fatalf("expected deterministic fingerprint, got %q vs %q", fp1, fp2)
	}

	b := testCurrentState()
	b.AlertSettings.AudioVolume = 0.9
	fp3, err := Fingerprint(b)
	if err != nil {
		t.Fatal(err)
	}
	if fp1 == fp3 {
		t.Fatal("expected fingerprint to change when alert settings change")
	}
}

func TestFingerprint_ProfileOrderIndependent(t *testing.T) {
	second := testProfile("profile-4444444444444444", "Second")

	a := testCurrentState()
	a.CalibrationProfiles = append(a.CalibrationProfiles, second)

	b := testCurrentState()
	b.CalibrationProfiles = []calprofile.Profile{second, b.CalibrationProfiles[0]}

	fpA, err := Fingerprint(a)
	if err != nil {
		t.Fatal(err)
	}
	fpB, err := Fingerprint(b)
	if err != nil {
		t.Fatal(err)
	}
	if fpA != fpB {
		t.Fatalf("expected profile order to not affect fingerprint, got %q vs %q", fpA, fpB)
	}
}
