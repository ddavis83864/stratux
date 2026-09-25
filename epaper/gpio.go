package epaper

import "fmt"

// GPIOMapping is the set of BCM GPIO numbers used to drive the panel's
// control lines. DIN/CLK/CS are the Pi's own hardware SPI0 peripheral
// (BCM10/11/8) and are not configurable - only the four plain-GPIO
// control lines (DC/BUSY/RST/PWR) can be remapped, since those are the
// pins the AHRS board's physical obstruction actually forces off the
// Waveshare example wiring (see docs/waveshare-epaper-display.md's GPIO
// ownership matrix for the full evidence).
type GPIOMapping struct {
	DC   int `json:"dc"`
	Busy int `json:"busy"`
	Rst  int `json:"rst"`
	Pwr  int `json:"pwr"`
}

// DefaultGPIOMapping is the mapping validated against this project's own
// GPIO ownership audit for a Raspberry Pi 4B with a Stratux AHRS v2.0
// board installed (which obstructs physical pins 1 and 6, and whose own
// I2C/UART/fan/SPI0 claims are documented in
// docs/waveshare-epaper-display.md). Physical pin numbers, for reference:
// DC=BCM25 (physical 22), Busy=BCM24 (physical 18), Rst=BCM27 (physical
// 13), Pwr=BCM22 (physical 15).
func DefaultGPIOMapping() GPIOMapping {
	return GPIOMapping{DC: 25, Busy: 24, Rst: 27, Pwr: 22}
}

// reservedBCM lists every BCM GPIO this project's own software or
// device-tree configuration claims on the target hardware (Raspberry Pi
// 4B + Stratux AHRS v2.0), independent of any e-paper display - see
// docs/waveshare-epaper-display.md's GPIO ownership matrix for the exact
// source citation behind every entry. SPI0's own three hardware pins
// (BCM7/8/9/10/11) are handled separately in Validate, since two of them
// (8 and 10/11) are the display's own intended CS/CLK/DIN use, not a
// conflict.
var reservedBCM = map[int]string{
	2:  "AHRS/barometer I2C bus 1 (SDA)",
	3:  "AHRS/barometer I2C bus 1 (SCL)",
	4:  "sc16is752-i2c device-tree overlay interrupt line",
	14: "primary UART TXD (reserved for GPIO-attached GPS fallback)",
	15: "primary UART RXD (reserved for GPIO-attached GPS fallback)",
	18: "cooling fan PWM output",
}

// spi0BCM are the Pi's own hardware SPI0 peripheral pins, enabled by this
// project's device-tree config but (per the GPIO audit) not driven by any
// existing Go code - CE0 (8), MOSI (10), and SCLK (11) are exactly what
// this display's CS/DIN/CLK lines are meant to use in 4-line SPI mode.
// CE1 (7) and MISO (9) are not used by this display and remain free.
var spi0BCM = map[int]bool{7: true, 8: true, 9: true, 10: true, 11: true}

// spi0Assigned are the three SPI0 pins this display actually occupies
// (CS=CE0, DIN=MOSI, CLK=SCLK) - fixed, not user-configurable, since they
// are the Pi's dedicated SPI0 hardware peripheral, not plain GPIO.
const (
	spi0CS  = 8
	spi0DIN = 10
	spi0CLK = 11
)

// Validate reports whether m is internally consistent (no duplicate
// pins) and conflict-free against every pin this project's own software
// or device-tree configuration claims on the supported hardware
// (physical pins 1 and 6 - always obstructed by the AHRS board on this
// hardware - are rejected outright, matching the mission's own hardware
// constraint; BCM-to-physical-pin correspondence is fixed by the
// Raspberry Pi 40-pin header standard, not configurable, so Validate
// works in BCM numbers throughout).
func (m GPIOMapping) Validate() error {
	pins := map[string]int{"dc": m.DC, "busy": m.Busy, "rst": m.Rst, "pwr": m.Pwr}

	seen := map[int]string{spi0CS: "cs (fixed, SPI0 CE0)", spi0DIN: "din (fixed, SPI0 MOSI)", spi0CLK: "clk (fixed, SPI0 SCLK)"}
	for name, bcm := range pins {
		if bcm <= 0 {
			return fmt.Errorf("gpio %s: BCM pin number must be positive, got %d", name, bcm)
		}
		if reason, reserved := reservedBCM[bcm]; reserved {
			return fmt.Errorf("gpio %s: BCM%d is already claimed by %s", name, bcm, reason)
		}
		if spi0BCM[bcm] {
			return fmt.Errorf("gpio %s: BCM%d is an SPI0 hardware pin and cannot be reused as a plain GPIO line", name, bcm)
		}
		if other, dup := seen[bcm]; dup {
			return fmt.Errorf("gpio %s: BCM%d is already assigned to %s", name, bcm, other)
		}
		seen[bcm] = name
	}
	return nil
}

// PhysicalPinExcluded reports whether physical is one of the header pins
// this hardware configuration always obstructs (see the mission's own
// hardware note: the AHRS v2.0 board physically covers pins 1 and 6).
// Exposed so tests and documentation can assert the shipped default and
// any future mapping never depends on these pins, without duplicating the
// magic numbers.
func PhysicalPinExcluded(physical int) bool {
	return physical == 1 || physical == 6
}
