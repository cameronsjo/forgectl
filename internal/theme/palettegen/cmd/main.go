// Command palettegen regenerates internal/theme/artificer_gen.go from the
// vendored Artificer palette. It is invoked by internal/theme's
// //go:generate directive, run from the internal/theme directory — so its
// default paths are relative to that directory, matching
// TestArtificerGen_IsCurrent.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/cameronsjo/forgectl/internal/theme"
	"github.com/cameronsjo/forgectl/internal/theme/palettegen"
)

func main() {
	palettePath := flag.String("palette", theme.PalettePath, "path to the vendored _palette.json")
	outPath := flag.String("out", "artificer_gen.go", "output path for the generated Go source")
	flag.Parse()

	if err := run(*palettePath, *outPath); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(palettePath, outPath string) error {
	// #nosec G304 -- palettePath is a developer-supplied -palette flag on a
	// go:generate-invoked code generator, never untrusted input.
	raw, err := os.ReadFile(palettePath)
	if err != nil {
		return fmt.Errorf("palettegen: read %s: %w", palettePath, err)
	}

	out, err := palettegen.Render(raw)
	if err != nil {
		return err
	}

	// #nosec G703 -- outPath is a developer-supplied -out flag on the same
	// generator; there is no untrusted taint to traverse.
	if err := os.WriteFile(outPath, out, 0o600); err != nil {
		return fmt.Errorf("palettegen: write %s: %w", outPath, err)
	}
	return nil
}
