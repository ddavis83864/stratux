package main

import (
	"context"
	"fmt"
	"time"

	rpio "github.com/stianeikeland/go-rpio/v4"
	"github.com/stratux/stratux/epaper"
)

// gpioBus implements Bus against real hardware via go-rpio - the same
// dependency this codebase already uses for the fan controller's own
// GPIO/PWM access (fancontrol_main/fancontrol.go), so this introduces no
// new third-party dependency. DIN/CLK/CS are the Pi's own SPI0 hardware
// peripheral (opened via rpio.SpiBegin); DC/BUSY/RST/PWR are plain GPIO
// lines per the configured epaper.GPIOMapping.
type gpioBus struct {
	dc, busy, rst, pwr rpio.Pin
}

// openGPIOBus opens the go-rpio memory map, configures SPI0 and the four
// plain-GPIO control lines per m, and returns a ready Bus. Callers must
// call closeGPIOBus when done to release the mapping cleanly (Phase 4's
// "cleanly release GPIO/SPI resources on stop" requirement).
func openGPIOBus(m epaper.GPIOMapping) (*gpioBus, error) {
	if err := rpio.Open(); err != nil {
		return nil, fmt.Errorf("gpio: %w", err)
	}
	if err := rpio.SpiBegin(rpio.Spi0); err != nil {
		rpio.Close()
		return nil, fmt.Errorf("spi: %w", err)
	}
	rpio.SpiChipSelect(0) // CE0 - BCM8 / physical pin 24, this display's CS line
	rpio.SpiSpeed(2000000)
	rpio.SpiMode(0, 0) // SPI mode 0, per the Driver HAT's documented CPOL=0/CPHA=0

	b := &gpioBus{
		dc:   rpio.Pin(m.DC),
		busy: rpio.Pin(m.Busy),
		rst:  rpio.Pin(m.Rst),
		pwr:  rpio.Pin(m.Pwr),
	}
	b.dc.Output()
	b.rst.Output()
	b.pwr.Output()
	b.busy.Input()
	b.busy.PullUp()
	return b, nil
}

func closeGPIOBus() {
	rpio.SpiEnd(rpio.Spi0)
	rpio.Close()
}

func (b *gpioBus) SetPower(on bool) error {
	if on {
		b.pwr.High()
	} else {
		b.pwr.Low()
	}
	return nil
}

func (b *gpioBus) Reset(ctx context.Context) error {
	b.rst.High()
	if err := sleepCtx(ctx, 30*time.Millisecond); err != nil {
		return err
	}
	b.rst.Low()
	if err := sleepCtx(ctx, 3*time.Millisecond); err != nil {
		return err
	}
	b.rst.High()
	return sleepCtx(ctx, 30*time.Millisecond)
}

// WaitIdle polls BUSY until it reaches its idle level - see driver.go's
// busyMeansBusy doc comment for why this waits while BUSY reads HIGH,
// the opposite of the Driver HAT's own generic "low active" pin
// description text.
func (b *gpioBus) WaitIdle(ctx context.Context) error {
	for {
		busy := b.busy.Read() == rpio.High
		if busy != busyMeansBusy {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (b *gpioBus) SendCommand(c byte) error {
	b.dc.Low()
	rpio.SpiTransmit(c)
	return nil
}

func (b *gpioBus) SendData(data ...byte) error {
	b.dc.High()
	rpio.SpiTransmit(data...)
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
