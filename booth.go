package main

// The booth: turns 1 to 4 photos into a 4-frame strip with one ImageMagick command.
// ImageMagick is called with an argument list (exec.Command), never through a shell.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	maxUpload   = 8 << 20          // 8 MB for all photos of one strip
	maxStrip    = 3 << 20          // hard cap on the JPEG we send back
	magickLimit = 25 * time.Second // the process is killed after this
	frameW      = 540
	frameH      = 420
)

var (
	errBusy     = errors.New("the booth is busy")
	errTooLarge = errors.New("the strip came out too large")
	errTimeout  = errors.New("developing took too long")
)

// The 4 frames of a strip, in print order. Each one is an ImageMagick recipe.
var recipes = [4]struct {
	Name string
	Ops  []string
}{
	{"Sepia", []string{"-sepia-tone", "80%", "-modulate", "100,88",
		"(", "-size", "540x420", "radial-gradient:#fff8ee-#5a3a22", ")", "-compose", "multiply", "-composite"}},
	{"Halftone", []string{"-colorspace", "Gray", "-normalize", "-sigmoidal-contrast", "4x50%",
		"-ordered-dither", "h8x8a", "+level-colors", "#1b1f3a,#f6f1e7"}},
	{"Posterize", []string{"-colorspace", "Gray", "-blur", "0x1.5", "-equalize", "+dither", "-posterize", "4",
		"(", "-size", "1x1", "xc:#13172e", "xc:#a51050", "xc:#f57461", "xc:#ffd9a8", "+append", ")", "-clut"}},
	{"Film grain", []string{"-colorspace", "Gray", "-normalize", "-sigmoidal-contrast", "4x45%",
		"-attenuate", "0.6", "+noise", "Gaussian",
		"(", "-size", "540x420", "radial-gradient:#ffffff-#262626", ")", "-compose", "multiply", "-composite"}},
}

// With a single photo, each frame gets its own crop, like four slightly different poses.
var poseZoom = [4]struct {
	Zoom    float64
	Gravity string
}{{1.0, "center"}, {1.15, "north"}, {1.3, "center"}, {1.08, "center"}}

// Shot is one validated input photo.
type Shot struct {
	Data []byte
	Kind string // jpeg, png, webp or gif: also the ImageMagick coder we force
}

// Print is a developed strip and how it was made.
type Print struct {
	JPEG     []byte
	Args     []string
	Duration time.Duration
	Inputs   int
	InBytes  int
}

type Booth struct {
	magick string
	font   string
	slots  chan struct{}
}

func newBooth() *Booth {
	bin, err := exec.LookPath("magick")
	if err != nil {
		bin = "magick" // reported by /health/deep
	}
	// One strip at a time, like a real booth: a nano instance has under 300 MiB.
	return &Booth{magick: bin, font: findFont(), slots: make(chan struct{}, 1)}
}

// findFont returns the caption font: the Alpine package path in the image,
// a local install for development, or "" (the strip is printed without caption).
func findFont() string {
	home, _ := os.UserHomeDir()
	for _, p := range []string{
		"/usr/share/fonts/jetbrains-mono/JetBrainsMono-Bold.ttf",
		filepath.Join(home, "Library/Fonts/JetBrainsMono-Bold.ttf"),
		"/System/Library/Fonts/Supplemental/Courier New Bold.ttf",
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// sniff accepts only real images, whatever the file name or Content-Type says.
func sniff(data []byte) (string, bool) {
	switch http.DetectContentType(data) {
	case "image/jpeg":
		return "jpeg", true
	case "image/png":
		return "png", true
	case "image/webp":
		return "webp", true
	case "image/gif":
		return "gif", true
	}
	return "", false
}

// tooManyPixels reads the image header only. JPEGs are decoded downscaled
// (jpeg:size), other formats at full size, hence a lower limit for them.
// WebP headers are not parsed here: the ImageMagick policy limits them.
func tooManyPixels(s Shot) bool {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(s.Data))
	if err != nil {
		return false
	}
	limit := 20_000_000
	if s.Kind == "jpeg" {
		limit = 50_000_000
	}
	return cfg.Width*cfg.Height > limit
}

// stripArgs builds the full ImageMagick command line for one strip.
// inputs are file names relative to the working directory, with a forced coder.
func stripArgs(inputs []string, font string, caption [2]string) []string {
	args := []string{"-respect-parentheses", "-limit", "memory", "160MiB", "-limit", "time", "20",
		"-define", "jpeg:size=1400x1100"}
	if len(inputs) == 1 {
		// Decode the photo once, shrink it, keep it in a memory register for the 4 frames.
		args = append(args, inputs[0], "-auto-orient", "-resize", "1200x900>", "-write", "mpr:photo", "+delete")
	}
	for i, r := range recipes {
		in, pose := "mpr:photo", poseZoom[i]
		if len(inputs) == 4 {
			in, pose = inputs[i], poseZoom[0]
		}
		w, h := int(frameW*pose.Zoom), int(frameH*pose.Zoom)
		args = append(args, "(", in, "-auto-orient",
			"-resize", fmt.Sprintf("%dx%d^", w, h), "-gravity", pose.Gravity,
			"-extent", fmt.Sprintf("%dx%d", frameW, frameH))
		args = append(args, r.Ops...)
		args = append(args, ")")
	}
	args = append(args, "-background", "#f6f1e7", "-smush", "22",
		"-bordercolor", "#f6f1e7", "-border", "28", "-gravity", "south", "-splice", "0x104")
	if font != "" {
		args = append(args, "-font", font,
			"-fill", "#1b1f3a", "-pointsize", "26", "-annotate", "+0+58", caption[0],
			"-fill", "#a51050", "-pointsize", "18", "-annotate", "+0+28", caption[1])
	}
	return append(args, "-strip", "-quality", "86", "-define", "jpeg:extent=1500KB", "jpg:-")
}

// Develop writes the shots to a private temp folder, runs ImageMagick once,
// and deletes everything: the strip only exists in the response.
func (b *Booth) Develop(ctx context.Context, shots []Shot, caption [2]string) (*Print, error) {
	select {
	case b.slots <- struct{}{}:
		defer func() { <-b.slots }()
	case <-time.After(10 * time.Second):
		return nil, errBusy
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	dir, err := os.MkdirTemp("", "booth-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	inputs, inBytes := make([]string, len(shots)), 0
	for i, s := range shots {
		name := fmt.Sprintf("pose-%d.%s", i+1, s.Kind)
		if err := os.WriteFile(filepath.Join(dir, name), s.Data, 0o600); err != nil {
			return nil, err
		}
		inputs[i] = s.Kind + ":" + name + "[0]" // forced coder, first frame only
		inBytes += len(s.Data)
	}

	args := stripArgs(inputs, b.font, caption)
	ctx, cancel := context.WithTimeout(ctx, magickLimit)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.magick, args...)
	cmd.Dir = dir
	cmd.WaitDelay = 2 * time.Second
	out, stderr := &cappedBuffer{max: maxStrip}, &cappedBuffer{max: 4 << 10, truncate: true}
	cmd.Stdout, cmd.Stderr = out, stderr

	start := time.Now()
	err = cmd.Run()
	took := time.Since(start)
	switch {
	case out.overflow:
		return nil, errTooLarge
	case ctx.Err() == context.DeadlineExceeded:
		return nil, errTimeout
	case err != nil:
		return nil, fmt.Errorf("magick: %v: %s", err, strings.TrimSpace(stderr.String()))
	case out.Len() == 0:
		return nil, errors.New("magick returned an empty strip")
	}
	return &Print{JPEG: out.Bytes(), Args: args, Duration: took, Inputs: len(shots), InBytes: inBytes}, nil
}

// Pretty prints the command as it would be typed in a shell: each frame on
// two lines (crop, then recipe), the final assembly on its own lines.
func Pretty(args []string) string {
	var sb strings.Builder
	sb.WriteString("magick")
	depth, prev := 0, ""
	for _, a := range args {
		switch {
		case a == "(" && depth == 0:
			sb.WriteString(" \\\n  \\(")
			depth, prev = 1, a
			continue
		case a == "(":
			depth++
		case a == ")":
			depth--
		case depth == 0 && (a == "-background" || a == "-font" || a == "-strip" || strings.Contains(a, ":pose-")):
			sb.WriteString(" \\\n ")
		}
		sb.WriteString(" " + shellQuote(a))
		if depth == 1 && prev == "-extent" {
			sb.WriteString(" \\\n     ")
		}
		prev = a
	}
	return sb.String()
}

func shellQuote(s string) string {
	switch s {
	case "(":
		return `\(`
	case ")":
		return `\)`
	}
	if s != "" && !strings.ContainsAny(s, " #^[]'\"$&;|<>*?!~`\\") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// cappedBuffer stops accepting bytes past max. For the strip, that makes
// ImageMagick fail fast; for stderr (truncate), the rest is silently dropped.
type cappedBuffer struct {
	bytes.Buffer
	max      int
	truncate bool
	overflow bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.Len()+len(p) > c.max {
		c.overflow = true
		if c.truncate {
			c.Buffer.Write(p[:max(0, c.max-c.Len())])
			return len(p), nil
		}
		return 0, errors.New("output cap reached")
	}
	return c.Buffer.Write(p)
}

// quick runs a small ImageMagick command and returns its output (used by /health/deep).
func (b *Booth) quick(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, b.magick, args...).Output()
}
