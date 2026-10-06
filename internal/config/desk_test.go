package config

import "testing"

func TestDeskConfig_SignalsAreOnUnlessTurnedOff(t *testing.T) {
	if c := (DeskConfig{}); !c.HerdrSignal() || !c.MacOSSignal() {
		t.Error("an absent [desk] section must leave both signals on")
	}
	f := false
	c := DeskConfig{NotifyHerdr: &f}
	if c.HerdrSignal() || !c.MacOSSignal() {
		t.Error("notify_herdr = false must turn only the herdr signal off")
	}
	c = DeskConfig{NotifyMacOS: &f}
	if !c.HerdrSignal() || c.MacOSSignal() {
		t.Error("notify_macos = false must turn only the macOS signal off")
	}
}
