package epaper

import "testing"

func TestGPIOMapping_DefaultIsValid(t *testing.T) {
	if err := DefaultGPIOMapping().Validate(); err != nil {
		t.Fatalf("the shipped default mapping must validate cleanly, got: %v", err)
	}
}

func TestGPIOMapping_RejectsEveryReservedPin(t *testing.T) {
	base := DefaultGPIOMapping()
	for bcm, reason := range reservedBCM {
		m := base
		m.DC = bcm // overwrite one field at a time with a known-reserved pin
		if err := m.Validate(); err == nil {
			t.Errorf("expected rejection of reserved BCM%d (%s), got none", bcm, reason)
		}
	}
}

func TestGPIOMapping_RejectsSPI0HardwarePins(t *testing.T) {
	base := DefaultGPIOMapping()
	for bcm := range spi0BCM {
		m := base
		m.Rst = bcm
		if err := m.Validate(); err == nil {
			t.Errorf("expected rejection of SPI0 pin BCM%d reused as plain GPIO, got none", bcm)
		}
	}
}

func TestGPIOMapping_RejectsDuplicatePinsAmongItself(t *testing.T) {
	m := GPIOMapping{DC: 25, Busy: 25, Rst: 27, Pwr: 22}
	if err := m.Validate(); err == nil {
		t.Errorf("expected rejection of DC/Busy sharing the same pin, got none")
	}
}

func TestGPIOMapping_RejectsNonPositivePins(t *testing.T) {
	m := GPIOMapping{DC: 0, Busy: 24, Rst: 27, Pwr: 22}
	if err := m.Validate(); err == nil {
		t.Errorf("expected rejection of a zero/unset pin number, got none")
	}
}

func TestGPIOMapping_AcceptsAValidRemapping(t *testing.T) {
	// A legitimate alternate mapping using different, still-free pins
	// (BCM5, BCM6, BCM12, BCM13 - none reserved, none SPI0) must be
	// accepted, proving Validate does not simply hardcode the default.
	m := GPIOMapping{DC: 5, Busy: 6, Rst: 12, Pwr: 13}
	if err := m.Validate(); err != nil {
		t.Errorf("expected an alternate, conflict-free mapping to validate, got: %v", err)
	}
}
