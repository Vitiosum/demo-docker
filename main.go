// The Photo Booth: a Clever Cloud Docker runtime demo.
// A Go server (standard library only) that prints 4-frame photo strips with
// ImageMagick, which the Docker image installs next to it.
package main

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata" // Europe/Paris for the date printed on the strip, without tzdata in the image
)

//go:embed static
var staticFS embed.FS

//go:embed Dockerfile
var dockerfile string

type App struct {
	booth *Booth
	facts Facts
	page  *template.Template
	paris *time.Location

	mu     sync.RWMutex
	sample *Print // printed at boot from the embedded sample photos
}

func main() {
	app := newApp()
	app.printSample()

	port := os.Getenv("PORT") // set to 8080 by Clever Cloud
	if port == "" {
		port = "8080"
	}
	srv := &http.Server{
		Addr:              ":" + port, // all interfaces: Clever Cloud checks 0.0.0.0:8080
		Handler:           app.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		log.Printf("photo booth listening on :%s (magick %q, font %q, %s)", port, app.facts.MagickVersion, app.booth.font, app.facts.User)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, os.Interrupt)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func newApp() *App {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		paris = time.UTC
	}
	b := newBooth()
	return &App{
		booth: b,
		facts: gatherFacts(b),
		paris: paris,
		page:  template.Must(template.New("index.html").Funcs(template.FuncMap{"mb": mb, "inc": func(i int) int { return i + 1 }}).ParseFS(staticFS, "static/index.html")),
	}
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.index)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /health/deep", a.healthDeep)
	mux.HandleFunc("POST /api/strip", a.strip)
	mux.HandleFunc("GET /sample-strip.jpg", a.sampleStrip)
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	return secure(mux)
}

// secure adds the security headers. The CSP allows no inline script or style.
func secure(next http.Handler) http.Handler {
	csp := strings.Join([]string{
		"default-src 'self'", "script-src 'self'", "style-src 'self' https://fonts.googleapis.com",
		"font-src https://fonts.gstatic.com", "img-src 'self' data: blob:", "media-src 'self' blob: mediastream:",
		"connect-src 'self'", "frame-ancestors 'none'", "base-uri 'none'", "form-action 'self'",
	}, "; ")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", "camera=(self), microphone=(), geolocation=()")
		next.ServeHTTP(w, r)
	})
}

func (a *App) index(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	sample := a.sample
	a.mu.RUnlock()
	data := map[string]any{
		"Vars":     platformVars(),
		"Facts":    a.facts,
		"Docker":   dockerLines(dockerfile),
		"Recipes":  recipes,
		"Sample":   sample,
		"Logo":     a.svg("cc-logo.svg"),
		"Badge":    a.svg("cc-badge.svg"),
		"Instance": os.Getenv("INSTANCE_NUMBER"),
		"Commit":   platformVars()[5].Value,
	}
	if f := a.facts; f.BuildStageMB > 0 && f.FinalImageMB > 0 {
		data["FinalPct"] = max(4, int(100*f.FinalImageMB/f.BuildStageMB))
	}
	if sample != nil {
		data["Command"] = cmdLines(Pretty(sample.Args))
		data["SampleKB"] = (sample.InBytes + 512) / 1024
		data["StripKB"] = (len(sample.JPEG) + 512) / 1024
		data["SampleMs"] = sample.Duration.Milliseconds()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := a.page.Execute(w, data); err != nil {
		log.Printf("template: %v", err)
	}
}

// healthDeep checks the system dependency, ImageMagick, with a tiny real render.
func (a *App) healthDeep(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	start := time.Now()
	out, err := a.booth.quick(ctx, "-size", "16x16", "xc:#a51050", "-sepia-tone", "80%", "png:-")
	res := map[string]any{"magick": a.facts.MagickVersion, "ms": time.Since(start).Milliseconds()}
	if err != nil || len(out) < 8 || string(out[1:4]) != "PNG" {
		res["status"] = "error"
		writeJSON(w, http.StatusServiceUnavailable, res)
		return
	}
	res["status"], res["png_bytes"] = "ok", len(out)
	writeJSON(w, http.StatusOK, res)
}

// strip receives 1 to 4 photos (multipart field "photo") and answers with the strip as a JPEG.
func (a *App) strip(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload+64<<10)
	if err := r.ParseMultipartForm(maxUpload); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			fail(w, http.StatusRequestEntityTooLarge, "Photos are limited to 8 MB in total. Choose a smaller photo.")
			return
		}
		fail(w, http.StatusBadRequest, "Send the photos as multipart form data, in a field named photo.")
		return
	}
	defer r.MultipartForm.RemoveAll()

	files := r.MultipartForm.File["photo"]
	if len(files) != 1 && len(files) != 4 {
		fail(w, http.StatusBadRequest, "A strip needs 1 photo or 4 photos.")
		return
	}
	total := int64(0)
	for _, fh := range files {
		total += fh.Size
	}
	if total > maxUpload {
		fail(w, http.StatusRequestEntityTooLarge, "Photos are limited to 8 MB in total. Choose a smaller photo.")
		return
	}
	shots := make([]Shot, 0, len(files))
	for _, fh := range files {
		f, err := fh.Open()
		if err != nil {
			fail(w, http.StatusBadRequest, "A photo could not be read. Try again.")
			return
		}
		data, err := io.ReadAll(io.LimitReader(f, maxUpload+1))
		f.Close()
		if err != nil || len(data) == 0 {
			fail(w, http.StatusBadRequest, "A photo is empty. Choose another one.")
			return
		}
		kind, ok := sniff(data)
		if !ok {
			fail(w, http.StatusUnsupportedMediaType, "This file is not a photo the booth can print. Use JPEG, PNG, WebP or GIF.")
			return
		}
		shot := Shot{Data: data, Kind: kind}
		if tooManyPixels(shot) {
			fail(w, http.StatusRequestEntityTooLarge, "This photo has too many pixels: 50 megapixels at most for JPEG, 20 for other formats.")
			return
		}
		shots = append(shots, shot)
	}

	p, err := a.booth.Develop(r.Context(), shots, a.caption(time.Now()))
	switch {
	case errors.Is(err, errBusy):
		w.Header().Set("Retry-After", "5")
		fail(w, http.StatusServiceUnavailable, "The booth is busy with other strips. Try again in a few seconds.")
		return
	case errors.Is(err, errTimeout):
		fail(w, http.StatusGatewayTimeout, "Developing took too long and was stopped. Try a smaller photo.")
		return
	case errors.Is(err, errTooLarge):
		fail(w, http.StatusInternalServerError, "The strip came out larger than 3 MB and was discarded.")
		return
	case err != nil:
		log.Printf("develop: %v", err)
		fail(w, http.StatusUnprocessableEntity, "ImageMagick could not print this photo. Try another one.")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "image/jpeg")
	h.Set("Content-Disposition", `attachment; filename="photo-strip-`+time.Now().In(a.paris).Format("20060102-150405")+`.jpg"`)
	h.Set("Cache-Control", "no-store")
	h.Set("X-Magick-Command", base64.StdEncoding.EncodeToString([]byte(Pretty(p.Args))))
	h.Set("X-Magick-Ms", strconv.FormatInt(p.Duration.Milliseconds(), 10))
	h.Set("X-Input-Bytes", strconv.Itoa(p.InBytes))
	h.Set("X-Input-Count", strconv.Itoa(p.Inputs))
	h.Set("X-Developed-By", instanceLabel())
	h.Set("Content-Length", strconv.Itoa(len(p.JPEG)))
	w.Write(p.JPEG)
}

func (a *App) sampleStrip(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	sample := a.sample
	a.mu.RUnlock()
	if sample == nil {
		http.Error(w, "The sample strip is not printed yet.", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(sample.JPEG)
}

// printSample develops the embedded sample photos at boot, so the booth
// never opens empty. It is idempotent: same photos, same strip, every start.
func (a *App) printSample() {
	shots := make([]Shot, 0, 4)
	for i := 1; i <= 4; i++ {
		data, err := staticFS.ReadFile("static/sample/pose-" + strconv.Itoa(i) + ".jpg")
		if err != nil {
			log.Printf("sample: %v", err)
			return
		}
		shots = append(shots, Shot{Data: data, Kind: "jpeg"})
	}
	p, err := a.booth.Develop(context.Background(), shots, a.caption(time.Now()))
	if err != nil {
		log.Printf("sample strip not printed: %v", err)
		return
	}
	a.mu.Lock()
	a.sample = p
	a.mu.Unlock()
	log.Printf("sample strip printed in %d ms (%d KB)", p.Duration.Milliseconds(), len(p.JPEG)/1024)
}

// caption is the two lines printed under the frames. No user text ever goes there.
func (a *App) caption(t time.Time) [2]string {
	line2 := strings.ToUpper(t.In(a.paris).Format("02 Jan 2006  15:04"))
	if n := os.Getenv("INSTANCE_NUMBER"); n != "" {
		line2 += "  BOOTH " + n
	}
	return [2]string{"CLEVER CLOUD PHOTO BOOTH", line2}
}

func instanceLabel() string {
	n, name := os.Getenv("INSTANCE_NUMBER"), os.Getenv("CC_PRETTY_INSTANCE_NAME")
	if n == "" {
		return "local"
	}
	if name != "" {
		return "#" + n + " " + name
	}
	return "#" + n
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func mb(v float64) string { return strconv.FormatFloat(v, 'f', 0, 64) }

// svg inlines one of the brand kit SVG files into the page.
func (a *App) svg(name string) template.HTML {
	b, _ := staticFS.ReadFile("static/" + name)
	return template.HTML(b)
}

// CmdLine is one line of the pretty command; Frame is 1-4 on the line of that frame.
type CmdLine struct {
	Text  string
	Frame int
}

func cmdLines(pretty string) []CmdLine {
	var out []CmdLine
	frame, current := 0, 0
	for _, l := range strings.Split(pretty, "\n") {
		switch {
		case strings.HasPrefix(strings.TrimSpace(l), `\(`):
			frame++
			current = frame
		case !strings.HasPrefix(l, "     "):
			current = 0
		}
		out = append(out, CmdLine{Text: l, Frame: current})
	}
	return out
}
