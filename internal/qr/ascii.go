package qr

import (
	"fmt"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

// RenderHalfBlock generates a compact, high-contrast UTF-8 half-block QR code
// using '▀', '▄', '█', and ' ' so it renders cleanly and scans reliably in
// Windows PowerShell, CMD, macOS Terminal, and Linux terminals.
func RenderHalfBlock(content string) (string, error) {
	q, err := qrcode.New(content, qrcode.Medium)
	if err != nil {
		return "", fmt.Errorf("failed to encode QR code: %w", err)
	}

	// Bitmap includes a 4-module quiet zone on each side.
	// Trim 2 modules per side to keep a compact, fully scannable 2-module quiet zone.
	bm := trimBorder(q.Bitmap(), 2)
	rows := len(bm)
	if rows == 0 {
		return "", fmt.Errorf("empty QR bitmap")
	}
	cols := len(bm[0])

	var sb strings.Builder
	// Pre-allocate approximate buffer size (3 bytes per UTF-8 block + newline).
	sb.Grow((rows/2 + 1) * (cols*3 + 2))

	for y := 0; y < rows; y += 2 {
		for x := 0; x < cols; x++ {
			// On dark terminals, foreground characters appear bright (white) and spaces appear dark (black).
			// In go-qrcode, true = dark module, false = light module (quiet zone).
			topLight := !bm[y][x]
			botLight := true
			if y+1 < rows {
				botLight = !bm[y+1][x]
			}

			switch {
			case topLight && botLight:
				sb.WriteRune('█')
			case topLight && !botLight:
				sb.WriteRune('▀')
			case !topLight && botLight:
				sb.WriteRune('▄')
			default:
				sb.WriteRune(' ')
			}
		}
		if y+2 < rows {
			sb.WriteByte('\n')
		}
	}

	return sb.String(), nil
}

// RenderPNG generates a crisp PNG QR code image for the /api/qr.png endpoint.
func RenderPNG(content string, size int) ([]byte, error) {
	if size <= 0 {
		size = 320
	}
	pngBytes, err := qrcode.Encode(content, qrcode.Medium, size)
	if err != nil {
		return nil, fmt.Errorf("failed to encode QR PNG: %w", err)
	}
	return pngBytes, nil
}

// trimBorder trims up to trimCount modules from each edge of the bitmap if the bitmap is large enough.
func trimBorder(bm [][]bool, trimCount int) [][]bool {
	h := len(bm)
	if h <= trimCount*2+8 {
		return bm
	}
	out := make([][]bool, 0, h-trimCount*2)
	for y := trimCount; y < h-trimCount; y++ {
		row := bm[y]
		if len(row) <= trimCount*2+8 {
			return bm
		}
		out = append(out, row[trimCount:len(row)-trimCount])
	}
	return out
}
