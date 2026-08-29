package docs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"image"
	"os"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"
)

const (
	maxGraphicColumns = 96
	maxGraphicRows    = 48
)

// GraphicsMode controls terminal image output. Auto enables Kitty graphics in
// terminals known to implement the protocol; Kitty forces it for terminals
// whose environment cannot advertise the outer emulator; Off is always text.
type GraphicsMode string

const (
	GraphicsAuto  GraphicsMode = "auto"
	GraphicsKitty GraphicsMode = "kitty"
	GraphicsOff   GraphicsMode = "off"
)

func ParseGraphicsMode(value string) (GraphicsMode, error) {
	mode := GraphicsMode(value)
	switch mode {
	case GraphicsAuto, GraphicsKitty, GraphicsOff:
		return mode, nil
	default:
		return "", fmt.Errorf("invalid graphics mode %q (want auto, kitty, or off)", value)
	}
}

// KittyGraphicsEnabled is deliberately conservative. Unknown terminals get
// readable fallbacks; --graphics=kitty is the explicit escape hatch.
func KittyGraphicsEnabled(mode GraphicsMode, getenv func(string) string) bool {
	if mode == GraphicsOff {
		return false
	}
	if mode == GraphicsKitty {
		return true
	}
	term := strings.ToLower(getenv("TERM"))
	program := strings.ToLower(getenv("TERM_PROGRAM"))
	return strings.Contains(term, "kitty") || strings.Contains(term, "ghostty") ||
		strings.Contains(term, "wezterm") || strings.Contains(term, "konsole") ||
		strings.Contains(program, "kitty") || strings.Contains(program, "ghostty") ||
		strings.Contains(program, "wezterm") || strings.Contains(program, "konsole")
}

// KittyImageBlock encodes img for transmission and returns separate placeholder
// rows that a TUI can scroll like ordinary text. Keeping the protocol bytes out
// of layout composition is important: width-aware renderers may deliberately
// discard an APC payload while measuring it. The returned IDs are deterministic
// for a given image and size, so redraws replace rather than accumulate.
func KittyImageBlock(img image.Image, columns int) (transmission, placeholders string, id uint32, err error) {
	if columns < 1 {
		columns = 1
	}
	if columns > maxGraphicColumns {
		columns = maxGraphicColumns
	}
	bounds := img.Bounds()
	if bounds.Dx() < 1 || bounds.Dy() < 1 {
		return "", "", 0, fmt.Errorf("image has no pixels")
	}
	// Terminal cells are approximately twice as tall as they are wide.
	rows := (columns*bounds.Dy() + (bounds.Dx() * 2) - 1) / (bounds.Dx() * 2)
	if rows < 1 {
		rows = 1
	}
	if rows > maxGraphicRows {
		rows = maxGraphicRows
	}

	h := fnv.New32a()
	_, _ = fmt.Fprintf(h, "%dx%d:%dx%d", bounds.Dx(), bounds.Dy(), columns, rows)
	var pixel [16]byte
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, a := img.At(x, y).RGBA()
			binary.BigEndian.PutUint32(pixel[0:4], r)
			binary.BigEndian.PutUint32(pixel[4:8], g)
			binary.BigEndian.PutUint32(pixel[8:12], b)
			binary.BigEndian.PutUint32(pixel[12:16], a)
			_, _ = h.Write(pixel[:])
		}
	}
	id = h.Sum32()
	// The reader caps placeholders at 96 cells, so keep the most-significant
	// byte within the same range of Kitty's published diacritic table.
	id &= 0x5fffffff
	if id>>24 == 0 {
		id |= 1 << 24
	}

	var tx bytes.Buffer
	opts := &kitty.Options{
		Action:           kitty.TransmitAndPut,
		Quite:            2,
		ID:               int(id),
		Format:           kitty.PNG,
		Transmission:     kitty.Direct,
		Chunk:            true,
		Columns:          columns,
		Rows:             rows,
		VirtualPlacement: true,
		DoNotMoveCursor:  true,
	}
	if os.Getenv("TMUX") != "" {
		opts.ChunkFormatter = tmuxPassthrough
	}
	if err := kitty.EncodeGraphics(&tx, img, opts); err != nil {
		return "", "", 0, err
	}

	r := int(id & 0xff)
	g := int((id >> 8) & 0xff)
	b := int((id >> 16) & 0xff)
	most := int((id >> 24) & 0xff)
	var out strings.Builder
	_, _ = fmt.Fprintf(&out, "\x1b[38;2;%d;%d;%dm", r, g, b)
	for row := 0; row < rows; row++ {
		for col := 0; col < columns; col++ {
			out.WriteRune(kitty.Placeholder)
			out.WriteRune(kitty.Diacritic(row))
			out.WriteRune(kitty.Diacritic(col))
			out.WriteRune(kitty.Diacritic(most))
		}
		out.WriteString("\x1b[39m")
		if row != rows-1 {
			out.WriteByte('\n')
			_, _ = fmt.Fprintf(&out, "\x1b[38;2;%d;%d;%dm", r, g, b)
		}
	}
	out.WriteString("\x1b[39m")
	return tx.String(), out.String(), id, nil
}

func tmuxPassthrough(sequence string) string {
	return "\x1bPtmux;" + strings.ReplaceAll(sequence, "\x1b", "\x1b\x1b") + "\x1b\\"
}

// KittyCleanupSequence removes image data owned by the reader on teardown.
func KittyCleanupSequence(ids []uint32) string {
	var out strings.Builder
	for _, id := range ids {
		seq := ansi.KittyGraphics(nil, "a=d", "d=I", fmt.Sprintf("i=%d", id))
		if os.Getenv("TMUX") != "" {
			seq = tmuxPassthrough(seq)
		}
		out.WriteString(seq)
	}
	return out.String()
}
