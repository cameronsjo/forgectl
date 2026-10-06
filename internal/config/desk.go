package config

// DeskConfig is the [desk] section: how `forgectl desk add` tells the
// operator an item is waiting. Both signals are on unless turned off here.
type DeskConfig struct {
	// NotifyHerdr turns off the herdr signal (a notification, and the
	// queuing session's pane in its needs-you state) when set to false.
	NotifyHerdr *bool `toml:"notify_herdr"`
	// NotifyMacOS turns off the macOS notification when set to false.
	NotifyMacOS *bool `toml:"notify_macos"`
}

// HerdrSignal reports whether a queued item raises the herdr signal.
func (c DeskConfig) HerdrSignal() bool { return c.NotifyHerdr == nil || *c.NotifyHerdr }

// MacOSSignal reports whether a queued item raises a macOS notification.
func (c DeskConfig) MacOSSignal() bool { return c.NotifyMacOS == nil || *c.NotifyMacOS }
