package config

// stripLegacyUsageOptIn clears usage_stats on any LaunchConfig read from the
// legacy compatibility file.
//
// Collection is an informed opt-in the operator makes in config.toml, next to
// the disclosure that says what a row contains. A `usage_stats = true` sitting
// in a claunch.conf was never that choice — and without this, the automatic
// migration would carry it into config.toml and switch collection on for
// somebody who only ever ran an upgrade.
//
// Every legacy decode site funnels through here so the guarantee cannot be
// lost by adding a fourth one that forgets.
func stripLegacyUsageOptIn(lc LaunchConfig) LaunchConfig {
	lc.UsageStats = false
	return lc
}
