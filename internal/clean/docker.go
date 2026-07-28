package clean

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
)

// DockerKind identifies one docker prune category. Opt-in (the CLI's
// --docker flag) — mirrors ~/.dotfiles/infrastructure/unraid/scripts/
// docker-cleanup.sh's shape (containers, then images, then volumes, then
// build cache), reimplemented here behind the same dry-run/confirm/apply
// gate every other clean target uses, instead of a standalone cron script.
type DockerKind string

const (
	DockerContainers DockerKind = "containers"
	DockerImages     DockerKind = "images"
	DockerVolumes    DockerKind = "volumes"
	DockerBuildCache DockerKind = "build-cache"
)

// dockerKindOrder is dockerPruneArgs' display/iteration order — map
// iteration in Go is randomized, and both the preview and apply passes need
// a deterministic, human-sensible order (containers → images → volumes →
// build cache, matching docker-cleanup.sh).
var dockerKindOrder = []DockerKind{DockerContainers, DockerImages, DockerVolumes, DockerBuildCache}

// dockerPruneArgs is one prune command per category. `-f`/`-af` skips
// docker's OWN interactive confirmation — forgectl's confirm() prompt
// already gated this before PruneDocker is ever called. The `until=`
// filters mirror docker-cleanup.sh, so containers/images/build-cache only
// target aged-out resources — EXCEPT volumes: `docker volume prune` has NO
// `until=` support at all (its only filter is `label=`), so this category
// removes every currently-unused volume regardless of age, not just old
// ones. Separately, on Docker Engine < 23 `volume prune` removed ALL unused
// volumes including named ones, while 23+ narrowed the unfiltered default to
// anonymous volumes only (named volumes need `-a` there). forgectl doesn't
// detect or gate on the Engine version — this simply runs whatever the
// local `docker` binary's own default does.
var dockerPruneArgs = map[DockerKind][]string{
	DockerContainers: {"container", "prune", "-f", "--filter", "until=24h"},
	DockerImages:     {"image", "prune", "-af", "--filter", "until=168h"},
	DockerVolumes:    {"volume", "prune", "-f"},
	DockerBuildCache: {"builder", "prune", "-f", "--filter", "until=168h"},
}

// dockerTypeToKind maps `docker system df`'s "Type" column to a DockerKind.
var dockerTypeToKind = map[string]DockerKind{
	"Containers":    DockerContainers,
	"Images":        DockerImages,
	"Local Volumes": DockerVolumes,
	"Build Cache":   DockerBuildCache,
}

// dockerKindToType is dockerTypeToKind inverted — parseDockerDF looks up a
// kind's expected df "Type" label directly rather than reverse-scanning the
// map on every row.
var dockerKindToType = map[DockerKind]string{
	DockerContainers: "Containers",
	DockerImages:     "Images",
	DockerVolumes:    "Local Volumes",
	DockerBuildCache: "Build Cache",
}

// dockerSizeUnits converts docker's human-readable size unit suffixes
// ("800MB", "1.2GB") to a byte multiplier. docker's go-units HumanSize
// formatter is DECIMAL (base 1000: kB/MB/GB), not IEC/binary (base 1024:
// KiB/MiB/GiB) — confirmed against live `docker system df` output, whose
// unit strings are lowercase-k ("kB"), the go-units decimal convention;
// IEC units would render uppercase-K ("KiB"). An earlier version of this
// map used 1024-based multipliers, which inflated every parsed figure by
// up to ~10% (compounding at TB scale) — every prompt/report byte count
// this package prints was silently wrong until this fix.
var dockerSizeUnits = map[string]int64{
	"B":  1,
	"KB": 1000,
	"MB": 1000 * 1000,
	"GB": 1000 * 1000 * 1000,
	"TB": 1000 * 1000 * 1000 * 1000,
	"PB": 1000 * 1000 * 1000 * 1000 * 1000,
}

// dockerDFRow is the on-the-wire shape of one `docker system df --format
// '{{json .}}'` line — the command emits one JSON object per row (NOT a
// JSON array), one row per category.
type dockerDFRow struct {
	Type        string `json:"Type"`
	Reclaimable string `json:"Reclaimable"`
}

// DockerItem is one docker prune category's reported reclaimable size, plus
// what PruneDocker did. Mirrors CacheItem's Skipped/Applied/Err shape for
// the CLI's shared preview/confirm/apply pattern.
type DockerItem struct {
	Kind DockerKind

	// Reclaimable is docker's own reported reclaimable size for this
	// category (parsed from `docker system df`), in bytes — a best-effort
	// parse of docker's own accounting, never independently re-measured.
	// Zero when the parse failed or docker reported nothing.
	Reclaimable int64
	// Reported is docker's raw size string ("1.2GB (66%)") for display when
	// the byte parse didn't succeed cleanly, so the user still sees
	// SOMETHING concrete rather than a silently wrong zero.
	Reported string

	// Skipped is true when docker itself is unreachable (df failed) or this
	// category didn't appear in df's output — every category skips
	// TOGETHER on a df failure, since one call backs all four.
	Skipped    bool
	SkipReason string

	Applied bool
	Err     error

	// Reclaimed is the ACTUAL bytes docker itself reported freeing, parsed
	// from the prune command's own stdout summary line ("Total reclaimed
	// space: 42B" for container/image/volume prune, "Total: 500MB" for
	// builder prune) — never independently re-measured, and never the
	// pre-prune Reclaimable estimate. ReclaimedKnown is false when Applied
	// is false, OR when the prune succeeded but its output had no
	// recognizable summary line (docker version differences, an empty
	// prune) — callers must not treat a zero Reclaimed as "freed nothing"
	// unless ReclaimedKnown is also true.
	Reclaimed      int64
	ReclaimedKnown bool
}

// ScanDocker reports each category's CURRENT reclaimable size via `docker
// system df` — a read-only query, never a prune — so the dry-run preview
// reflects docker's own accounting rather than forgectl re-measuring
// anything. A df failure (docker not installed, daemon not running) skips
// every category together, with the failure reason attached to each.
func (c *Client) ScanDocker(ctx context.Context) []DockerItem {
	out, err := c.run.Run(ctx, "docker", "system", "df", "--format", "{{json .}}")
	if err != nil {
		items := make([]DockerItem, 0, len(dockerKindOrder))
		for _, k := range dockerKindOrder {
			items = append(items, DockerItem{Kind: k, Skipped: true, SkipReason: "docker unreachable: " + err.Error()})
		}
		return items
	}
	return parseDockerDF(out)
}

// parseDockerDF parses `docker system df --format '{{json .}}'`'s ndjson
// output (one JSON object per line) into DockerItems, in dockerKindOrder. A
// malformed or unrecognized row is skipped rather than treated as fatal —
// docker's own CLI output is trusted less than a locally-run test fixture,
// but a parse hiccup on one row shouldn't lose the others.
func parseDockerDF(out string) []DockerItem {
	byType := make(map[string]dockerDFRow)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var row dockerDFRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			slog.Warn("Skipping unparseable docker system df row.", "line", line, "error", err)
			continue
		}
		byType[row.Type] = row
	}

	items := make([]DockerItem, 0, len(dockerKindOrder))
	for _, kind := range dockerKindOrder {
		row, ok := byType[dockerKindToType[kind]]
		if !ok {
			items = append(items, DockerItem{Kind: kind, Skipped: true, SkipReason: "docker system df did not report this category"})
			continue
		}
		item := DockerItem{Kind: kind, Reported: row.Reclaimable}
		if fields := strings.Fields(row.Reclaimable); len(fields) > 0 {
			if n, ok := parseDockerSize(fields[0]); ok {
				item.Reclaimable = n
			}
		}
		items = append(items, item)
	}
	return items
}

// parseDockerSize best-effort parses a docker size token ("800MB", "1.2GB",
// "0B" — the percentage suffix, e.g. "(66%)", must already be stripped by
// the caller) into bytes. An unrecognized unit or unparseable number
// returns (0, false) rather than guessing.
func parseDockerSize(s string) (int64, bool) {
	i := 0
	for i < len(s) && (s[i] == '.' || (s[i] >= '0' && s[i] <= '9')) {
		i++
	}
	if i == 0 {
		return 0, false
	}
	f, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0, false
	}
	mult, ok := dockerSizeUnits[strings.ToUpper(strings.TrimSpace(s[i:]))]
	if !ok {
		return 0, false
	}
	return int64(f * float64(mult)), true
}

// parseReclaimedFromPruneOutput extracts the actual bytes docker itself
// reports freeing from a prune command's own stdout. Every docker prune
// subcommand ends its output with a summary line containing "otal"
// (docker's exact wording varies across subcommands and versions — e.g.
// "Total reclaimed space: 42B" for container/image/volume prune, "Total:
// 500MB" for builder prune — matching on the substring rather than a fixed
// phrase covers both) followed by a size token as its last field. Returns
// (0, false) when no such line is found or its trailing token doesn't
// parse — callers must then label the amount unknown rather than reporting
// a fabricated zero as "nothing reclaimed".
func parseReclaimedFromPruneOutput(out string) (int64, bool) {
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(strings.ToLower(line), "otal") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if n, ok := parseDockerSize(fields[len(fields)-1]); ok {
			return n, true
		}
	}
	return 0, false
}

// PruneDocker runs each non-skipped category's own prune command, ISOLATED
// per category — mirrors docker-cleanup.sh's shape and PruneCaches' per-
// target isolation: one category failing (docker restarting mid-run, a
// permissions issue) never blocks the rest.
//
// Reclaimed is parsed from the prune command's own stdout (see
// parseReclaimedFromPruneOutput), never taken from the pre-prune
// Reclaimable estimate — issue #4's own acceptance criterion is "reports
// actual reclaimed bytes".
func (c *Client) PruneDocker(ctx context.Context, items []DockerItem) []DockerItem {
	out := make([]DockerItem, len(items))
	copy(out, items)

	for i := range out {
		if out[i].Skipped {
			continue
		}
		args, ok := dockerPruneArgs[out[i].Kind]
		if !ok {
			continue
		}
		pruneOut, err := c.run.Run(ctx, "docker", args...)
		if err != nil {
			out[i].Err = err
			slog.Error("Failed to prune docker category.", "kind", out[i].Kind, "error", err)
			continue
		}
		out[i].Applied = true
		if n, ok := parseReclaimedFromPruneOutput(pruneOut); ok {
			out[i].Reclaimed = n
			out[i].ReclaimedKnown = true
		}
		slog.Info("Successfully pruned docker category.", "kind", out[i].Kind, "reclaimed", out[i].Reclaimed, "reclaimedKnown", out[i].ReclaimedKnown)
	}
	return out
}
