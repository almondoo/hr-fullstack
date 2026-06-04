package yearend

import (
	"embed"
	"io/fs"
	"log/slog"
)

// ---------------------------------------------------------------------------
// CJK font loading via embed.FS (graceful fallback when font is absent)
//
// The font binary is NOT committed to the repository.  Place ipaexg.ttf
// (IPAex Gothic, IPA Font License v1.0) in internal/yearend/fonts/ before
// building a production image.  See fonts/README.md for instructions.
//
// Build / test safety:
//   "//go:embed fonts" embeds all non-dot, non-underscore files in the
//   directory.  Note: Go's embed spec excludes files whose names start with
//   '.' or '_', so fonts/.gitkeep is NOT embedded and does NOT anchor the
//   build.  The actual anchor that keeps the embed.FS compiling when no .ttf
//   is present is fonts/README.md (always committed).  The explicit second
//   directive "//go:embed fonts/README.md" makes this dependency visible and
//   prevents a silent build break if README.md were ever removed.
//   loadCJKFont returns nil when ipaexg.ttf is not found, enabling graceful
//   fallback to Helvetica with romanised labels.
// ---------------------------------------------------------------------------

//go:embed fonts
//go:embed fonts/README.md
var fontsFS embed.FS

// cjkFontFamily is the family name registered with fpdf via AddUTF8FontFromBytes.
const cjkFontFamily = "IPAexGothic"

// cjkFontName is the filename expected inside the embedded fonts directory.
const cjkFontName = "fonts/ipaexg.ttf"

// cjkFontBytes caches the loaded font bytes after the first successful read.
// nil means the font is unavailable and the caller should fall back to Helvetica.
var cjkFontBytes []byte

func init() {
	cjkFontBytes = loadCJKFont()
}

// loadCJKFont reads ipaexg.ttf from the embedded filesystem.
// Returns nil (no error) when the file is not present so callers can fall back.
func loadCJKFont() []byte {
	b, err := fs.ReadFile(fontsFS, cjkFontName)
	if err != nil {
		// Font absent — graceful fallback; not an application error.
		slog.Info("yearend: CJK font not found; PDF labels will use romanised Helvetica fallback",
			"path", cjkFontName)
		return nil
	}
	slog.Info("yearend: CJK font loaded", "path", cjkFontName, "bytes", len(b))
	return b
}

// hasCJKFont reports whether the CJK font is available for PDF rendering.
func hasCJKFont() bool { return len(cjkFontBytes) > 0 }
