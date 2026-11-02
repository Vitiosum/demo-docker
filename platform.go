package main

// What the page shows about where it runs: Clever Cloud platform variables
// (an allow-list, nothing else is ever read) and facts about the container.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type PlatformVar struct{ Name, Value string }

// buildInfoDir holds the sizes measured by the Dockerfile (see its RUN du lines).
var buildInfoDir = "/etc/booth"

// platformVars returns only these 7 variables, in this order.
func platformVars() []PlatformVar {
	names := []string{"CC_APP_NAME", "APP_ID", "INSTANCE_NUMBER", "INSTANCE_TYPE",
		"CC_PRETTY_INSTANCE_NAME", "CC_COMMIT_ID", "CC_DEPLOYMENT_ID"}
	vars := make([]PlatformVar, 0, len(names))
	for _, n := range names {
		v := os.Getenv(n)
		if n == "CC_COMMIT_ID" && len(v) > 7 {
			v = v[:7]
		}
		vars = append(vars, PlatformVar{n, v})
	}
	return vars
}

// Facts is what the darkroom panel says about the image and the container.
type Facts struct {
	MagickVersion string  // e.g. "7.1.2-34 Q16-HDRI x86_64"
	Alpine        string  // /etc/alpine-release, empty outside the image
	User          string  // e.g. "booth (uid 10001)"
	Root          bool    // true if the process runs as root
	InContainer   bool    // /.dockerenv exists
	MemLimit      string  // cgroup memory limit of the container
	BuildStageMB  float64 // measured by du during the Docker build
	FinalImageMB  float64
	Font          string
	GoVersion     string
	Platform      string
}

func gatherFacts(b *Booth) Facts {
	f := Facts{GoVersion: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH, Font: b.font}
	f.MagickVersion = magickVersion(b.magick)
	if v, err := os.ReadFile("/etc/alpine-release"); err == nil {
		f.Alpine = strings.TrimSpace(string(v))
	}
	uid := os.Getuid()
	f.Root = uid == 0
	f.User = fmt.Sprintf("uid %d", uid)
	if u, err := user.Current(); err == nil {
		f.User = fmt.Sprintf("%s (uid %d)", u.Username, uid)
	}
	_, err := os.Stat("/.dockerenv")
	f.InContainer = err == nil
	f.MemLimit = memLimit()
	f.BuildStageMB = readKB(filepath.Join(buildInfoDir, "build-stage.kb"))
	f.FinalImageMB = readKB(filepath.Join(buildInfoDir, "final-image.kb"))
	return f
}

// magickVersion reads the first line of `magick -version`.
func magickVersion(bin string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "-version").Output()
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(out), "\n")
	fields := strings.Fields(line) // Version: ImageMagick 7.1.2-34 Q16-HDRI x86_64 ...
	if len(fields) >= 5 {
		return strings.Join(fields[2:5], " ")
	}
	return strings.TrimSpace(line)
}

// memLimit reads the cgroup v2 (or v1) memory limit: the instance memory on Clever Cloud.
func memLimit() string {
	for _, p := range []string{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"} {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		s := strings.TrimSpace(string(raw))
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n <= 0 || n > 1<<50 {
			return "no limit"
		}
		return fmt.Sprintf("%d MiB", n>>20)
	}
	return ""
}

// readKB reads a "<kilobytes>\t/" line written by `du -skx /` and returns megabytes.
func readKB(path string) float64 {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return 0
	}
	kb, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return kb / 1024
}

// DockerLine is one line of the embedded Dockerfile, with a margin note on key lines.
type DockerLine struct {
	Text, Note, Kind string
}

func dockerLines(src string) []DockerLine {
	notes := []struct{ prefix, kind, note string }{
		{"FROM golang", "is-stage", "Stage 1 has the whole Go toolchain. Nothing from it ships but one static binary."},
		{"FROM alpine", "is-stage", "Stage 2 is what runs on Clever Cloud: a small Alpine base."},
		{"RUN apk add", "is-key", "The system layer this app picks for itself: ImageMagick 7 with its JPEG and WebP coders, and the caption font."},
		{"COPY imagemagick/policy.xml", "is-key", "Our own ImageMagick security policy replaces the default one."},
		{"COPY --from=build /out/booth", "is-key", "Multi-stage: only the compiled binary crosses over."},
		{"RUN du", "is-key", "Measures this image while it is built. The panel reads the number back."},
		{"USER booth", "is-key", "The server runs as uid 10001, never as root."},
		{"EXPOSE 8080", "is-key", "Clever Cloud routes HTTP to 8080 by default (CC_DOCKER_EXPOSED_HTTP_PORT)."},
		{"CMD", "is-key", "The one instruction the Docker runtime requires."},
	}
	var lines []DockerLine
	for _, l := range strings.Split(strings.TrimRight(src, "\n"), "\n") {
		dl := DockerLine{Text: l}
		if strings.HasPrefix(l, "#") {
			dl.Kind = "is-comment"
		}
		for _, n := range notes {
			if strings.HasPrefix(l, n.prefix) {
				dl.Kind, dl.Note = n.kind, n.note
				break
			}
		}
		lines = append(lines, dl)
	}
	return lines
}
