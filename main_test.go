package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"image"
	_ "image/jpeg"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func testApp(t *testing.T) http.Handler {
	t.Helper()
	abs, _ := filepath.Abs("imagemagick")
	t.Setenv("MAGICK_CONFIGURE_PATH", abs) // the same policy as in the image
	return newApp().routes()
}

func needMagick(t *testing.T) {
	if _, err := exec.LookPath("magick"); err != nil {
		t.Skip("ImageMagick (magick) is not installed")
	}
}

func upload(t *testing.T, h http.Handler, files ...[]byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for i, f := range files {
		part, _ := mw.CreateFormFile("photo", "photo"+string(rune('1'+i))+".jpg")
		part.Write(f)
	}
	mw.Close()
	req := httptest.NewRequest("POST", "/api/strip", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// pngHeader is a PNG whose header claims w x h pixels (no pixel data follows).
func pngHeader(w, h uint32) []byte {
	ihdr := make([]byte, 17)
	copy(ihdr, "IHDR")
	binary.BigEndian.PutUint32(ihdr[4:], w)
	binary.BigEndian.PutUint32(ihdr[8:], h)
	ihdr[12], ihdr[13] = 8, 2 // 8-bit RGB
	b := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0d")
	b = append(b, ihdr...)
	return binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE(ihdr))
}

func samplePhoto(t *testing.T) []byte {
	b, err := os.ReadFile("static/sample/pose-1.jpg")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestHealthTouchesNothing(t *testing.T) {
	rec := httptest.NewRecorder()
	testApp(t).ServeHTTP(rec, httptest.NewRequest("GET", "/health", nil))
	if rec.Code != 200 || rec.Body.String() != "ok\n" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("security headers missing")
	}
}

func TestPageShowsOnlyAllowedVariables(t *testing.T) {
	t.Setenv("CC_APP_NAME", "demo-docker")
	t.Setenv("CC_COMMIT_ID", "0123456789abcdef0123456789abcdef01234567")
	t.Setenv("SOME_SECRET", "do-not-print-me")
	rec := httptest.NewRecorder()
	testApp(t).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	page := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(page, "demo-docker") || !strings.Contains(page, "0123456<") {
		t.Fatalf("page missing platform values (code %d)", rec.Code)
	}
	if strings.Contains(page, "do-not-print-me") || strings.Contains(page, "0123456789abcdef") {
		t.Fatal("page leaks a variable it must not print")
	}
}

func TestStripRejectsBadInput(t *testing.T) {
	h := testApp(t)
	photo := samplePhoto(t)
	cases := []struct {
		name  string
		files [][]byte
		code  int
	}{
		{"no photo", nil, 400},
		{"two photos", [][]byte{photo, photo}, 400},
		{"text file", [][]byte{[]byte("hello, I am not an image")}, 415},
		{"svg", [][]byte{[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><image href="file:///etc/passwd"/></svg>`)}, 415},
		{"mvg disguised as jpg", [][]byte{[]byte("push graphic-context\nviewbox 0 0 640 480\nimage over 0,0 0,0 'https://example.com/x.jpg'\n")}, 415},
		{"just over 8 MB", [][]byte{append(photo, make([]byte, maxUpload)...)}, 413},
		{"far over 8 MB", [][]byte{make([]byte, 3*maxUpload)}, 413},
		{"pixel bomb", [][]byte{pngHeader(12000, 12000)}, 413},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := upload(t, h, c.files...)
			if rec.Code != c.code {
				t.Fatalf("got %d, want %d: %.120q", rec.Code, c.code, rec.Body.String())
			}
			var e map[string]string
			if json.Unmarshal(rec.Body.Bytes(), &e) != nil || e["error"] == "" {
				t.Fatalf("expected a JSON error message, got %q", rec.Body.String())
			}
		})
	}
}

func TestStripArgsAreAnArgumentList(t *testing.T) {
	args := stripArgs([]string{"jpeg:pose-1.jpeg[0]"}, "/fonts/x.ttf", [2]string{"CLEVER CLOUD PHOTO BOOTH", "01 NOV 2026  10:00"})
	if args[0] != "-respect-parentheses" || args[len(args)-1] != "jpg:-" {
		t.Fatalf("unexpected bounds: %q ... %q", args[0], args[len(args)-1])
	}
	groups, captions := 0, 0
	for i, a := range args {
		if a == "(" && args[i+1] == "mpr:photo" {
			groups++
		}
		if strings.HasPrefix(a, "CLEVER CLOUD") || strings.HasPrefix(a, "01 NOV") {
			captions++ // each caption line is one argument, spaces included
		}
	}
	if groups != 4 || captions != 2 {
		t.Fatalf("got %d frame groups and %d caption args", groups, captions)
	}
	lines := cmdLines(Pretty(args))
	framed := 0
	for _, l := range lines {
		if l.Frame > 0 {
			framed++
		}
	}
	if framed != 8 {
		t.Fatalf("pretty command: %d framed lines, want 8 (2 per frame)\n%s", framed, Pretty(args))
	}
}

func TestStripEndToEnd(t *testing.T) {
	needMagick(t)
	h := testApp(t)
	for _, n := range []int{1, 4} {
		files := make([][]byte, n)
		for i := range files {
			files[i] = samplePhoto(t)
		}
		rec := upload(t, h, files...)
		if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/jpeg" {
			t.Fatalf("%d photo(s): got %d %s", n, rec.Code, rec.Body.String())
		}
		cfg, format, err := image.DecodeConfig(bytes.NewReader(rec.Body.Bytes()))
		if err != nil || format != "jpeg" || cfg.Width != 596 || cfg.Height != 1906 {
			t.Fatalf("strip is %s %dx%d (%v), want jpeg 596x1906", format, cfg.Width, cfg.Height, err)
		}
		if rec.Body.Len() > maxStrip {
			t.Fatalf("strip is %d bytes, over the cap", rec.Body.Len())
		}
		cmd, _ := base64.StdEncoding.DecodeString(rec.Header().Get("X-Magick-Command"))
		if !strings.HasPrefix(string(cmd), "magick ") || !strings.Contains(string(cmd), "-sepia-tone") {
			t.Fatalf("command header: %q", cmd)
		}
	}
}

func TestHealthDeepRendersWithMagick(t *testing.T) {
	needMagick(t)
	rec := httptest.NewRecorder()
	testApp(t).ServeHTTP(rec, httptest.NewRequest("GET", "/health/deep", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

func TestPolicyBlocksOtherCoders(t *testing.T) {
	needMagick(t)
	abs, _ := filepath.Abs("imagemagick")
	cmd := exec.CommandContext(context.Background(), "magick", "-size", "1x1", "xc:white", "txt:-")
	cmd.Env = append(os.Environ(), "MAGICK_CONFIGURE_PATH="+abs)
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "security policy") {
		t.Fatalf("the TXT coder should be refused by policy.xml, got %v: %s", err, out)
	}
}

func TestDockerfileKeyLinesAreAnnotated(t *testing.T) {
	want := map[string]bool{"USER booth": false, "EXPOSE 8080": false, "CMD": false, "FROM alpine": false, "FROM golang": false}
	for _, l := range dockerLines(dockerfile) {
		for k := range want {
			if strings.HasPrefix(l.Text, k) && l.Note != "" {
				want[k] = true
			}
		}
	}
	for k, ok := range want {
		if !ok {
			t.Errorf("no note on the Dockerfile line %q", k)
		}
	}
}
