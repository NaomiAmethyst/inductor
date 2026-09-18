// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"golang.org/x/crypto/blake2b"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"image"
	"image/color"
	"image/draw"
	_ "image/jpeg"
	"image/png"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var Fonts = map[string][2]string{"heavy-sans": {"/usr/share/fonts/truetype/lato/Lato-Black.ttf", "heavy grotesque; blunt, modern, loud"}, "condensed": {"/usr/share/texmf/fonts/opentype/public/tex-gyre/texgyreheroscn-bold.otf", "tall condensed sans; urgent, editorial, tight"}, "geometric": {"/usr/share/texmf/fonts/opentype/public/tex-gyre/texgyreadventor-bold.otf", "geometric sans; clean circles, retro-futurist, clinical"}, "script": {"/usr/share/texmf/fonts/opentype/public/tex-gyre/texgyrechorus-mediumitalic.otf", "flowing chancery script; intimate, handwritten, romantic"}, "elegant-serif": {"/usr/share/texmf/fonts/opentype/public/tex-gyre/texgyrepagella-bold.otf", "refined humanist serif; poised, classical, expensive"}, "bookish": {"/usr/share/texmf/fonts/opentype/public/tex-gyre/texgyrebonum-bold.otf", "warm old-style serif; storybook, solid, friendly"}}

func FontChoices() string {
	lines := []string{}
	for _, name := range []string{"heavy-sans", "condensed", "geometric", "script", "elegant-serif", "bookish"} {
		lines = append(lines, "  "+name+" -- "+Fonts[name][1])
	}
	return strings.Join(lines, "\n")
}
func wrapWords(words []string, lines int) []string {
	if lines <= 1 || len(words) < 2 {
		return []string{strings.Join(words, " ")}
	}
	per := max(1, int(math.RoundToEven(float64(len(words))/float64(lines))))
	out := []string{}
	for i := 0; i < len(words); i += per {
		out = append(out, strings.Join(words[i:min(i+per, len(words))], " "))
	}
	if len(out) > lines {
		out[lines-1] = strings.Join(out[lines-1:], " ")
		out = out[:lines]
	}
	return out
}
func DrawNameplate(path, text, faceName string, title bool) (bool, error) {
	text = strings.Join(pythonFields(text), " ")
	if text == "" {
		return false, nil
	}
	b, e := os.ReadFile(path)
	if e != nil {
		return false, e
	}
	img, _, e := image.Decode(bytes.NewReader(b))
	if e != nil {
		return false, e
	}
	entry, ok := Fonts[strings.ToLower(faceName)]
	if !ok {
		entry = Fonts["heavy-sans"]
	}
	fontData, e := os.ReadFile(entry[0])
	if e != nil {
		fontData = gobold.TTF
	}
	parsed, e := opentype.Parse(fontData)
	if e != nil {
		return false, e
	}
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	width, height, cap, centre := .84, .46, .34, .60
	if title {
		width, height, cap, centre = .86, .32, .17, .76
	}
	bestSize := 0
	bestRows := []string{}
	words := pythonFields(text)
	for count := 1; count <= min(3, len(words)); count++ {
		rows := wrapWords(words, count)
		size := int(float64(h) * cap)
		for size > 8 {
			face, e := opentype.NewFace(parsed, &opentype.FaceOptions{Size: float64(size), DPI: 72, Hinting: font.HintingFull})
			if e != nil {
				return false, e
			}
			maxW := 0.
			for _, row := range rows {
				bounds, _ := font.BoundString(face, row)
				maxW = math.Max(maxW, float64(bounds.Max.X-bounds.Min.X)/64)
			}
			_ = face.Close()
			if maxW <= float64(w)*width && float64(size)*1.06*float64(len(rows)) <= float64(h)*height {
				break
			}
			size = int(float64(size) * .94)
		}
		if size > bestSize {
			bestSize = size
			bestRows = rows
		}
	}
	if bestSize == 0 {
		return false, nil
	}
	face, e := opentype.NewFace(parsed, &opentype.FaceOptions{Size: float64(bestSize), DPI: 72, Hinting: font.HintingFull})
	if e != nil {
		return false, e
	}
	defer face.Close()
	canvas := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(canvas, canvas.Bounds(), img, img.Bounds().Min, draw.Src)
	lineH := float64(bestSize) * 1.06
	blockH := lineH * float64(len(bestRows))
	top := math.Min(float64(h)*centre-blockH/2, float64(h)-blockH-float64(h)*.04)
	stroke := max(2, bestSize/12)
	pen := font.Drawer{Dst: canvas, Face: face}
	for i, row := range bestRows {
		bounds, _ := font.BoundString(face, row)
		x := (float64(w)-float64(bounds.Max.X-bounds.Min.X)/64)/2 - float64(bounds.Min.X)/64
		y := top + float64(i)*lineH - float64(bounds.Min.Y)/64
		pen.Src = image.NewUniform(color.RGBA{0, 0, 0, 235})
		for dy := -stroke; dy <= stroke; dy++ {
			for dx := -stroke; dx <= stroke; dx++ {
				if dx*dx+dy*dy > stroke*stroke {
					continue
				}
				pen.Dot = fixed.P(int(x)+dx, int(y)+dy)
				pen.DrawString(row)
			}
		}
		pen.Src = image.White
		pen.Dot = fixed.P(int(x), int(y))
		pen.DrawString(row)
	}
	var out bytes.Buffer
	if e = png.Encode(&out, canvas); e != nil {
		return false, e
	}
	return AtomicWrite(path, out.Bytes(), 0644)
}

// ArtKey digests the instruction that drew a picture -- the prompt as the
// renderer received it, the negative beside it, and the engine that chose
// between the tagged and natural wordings.
func ArtKey(text, negative, engine string) string {
	h, _ := blake2b.New(16, nil)
	_, _ = h.Write([]byte(strings.TrimSpace(text) + "\x00" + strings.TrimSpace(negative) + "\x00" + strings.ToLower(strings.TrimSpace(engine))))
	return hex.EncodeToString(h.Sum(nil))
}

// StampArt records what actually drew the picture, beside the `synopsis_from`
// that has always done the same for the written half. Without it a document
// carries a prompt and a picture with nothing to say they belong together, and
// a prompt rewritten later leaves the two describing different things silently.
func StampArt(prov Record, text, negative, engine, nameplate string) {
	prov["image_from"] = Record{"prompt": ArtKey(text, negative, engine), "engine": engine,
		"nameplate": nameplate}
}

// ArtStale says whether the picture was drawn from words that have since moved
// on. An unstamped picture is *not* called stale: every image made before this
// was recorded is unstamped, and answering otherwise would order the whole
// library redrawn on the strength of a missing field rather than a changed one.
func ArtStale(prov Record, text, negative, engine, nameplate string) bool {
	was := record(prov["image_from"])
	if len(was) == 0 {
		return false
	}
	// The title is painted onto the picture, so renaming a recording leaves the
	// cover announcing what it used to be called. Compared as a field rather
	// than folded into the digest: a stamp written before this existed has no
	// nameplate to compare, and should not be called stale for lacking one.
	if plate, ok := was["nameplate"]; ok && str(plate) != nameplate {
		return true
	}
	return str(was["prompt"]) != ArtKey(text, negative, engine)
}

func PromptFor(final Record, engine string, item Record) (string, string) {
	kept := record(item["cover_prompts"])
	tagged := strings.TrimSpace(str(first(kept["tagged"], final["thumbnail_prompt"])))
	natural := strings.TrimSpace(str(first(kept["natural"], final["thumbnail_prompt_natural"])))
	if contains([]string{"flux", "sd3", "natural"}, strings.ToLower(engine)) {
		if natural != "" {
			return natural, "natural"
		}
		return tagged, "tagged"
	}
	if tagged != "" {
		return tagged, "tagged"
	}
	return natural, "natural"
}
func imageSize(path string) (int, int) {
	f, e := os.Open(path)
	if e != nil {
		return 0, 0
	}
	defer f.Close()
	c, _, e := image.DecodeConfig(f)
	if e != nil {
		return 0, 0
	}
	return c.Width, c.Height
}
func RenderGraph(engine, promptText, negative string, seed, width, height int) Record {
	if engine != "flux" && engine != "turbo" {
		engine = "sdxl"
	}
	b, e := prompts.ReadFile("prompts/" + engine + "_graph.json")
	if e != nil {
		panic(e)
	}
	var graph Record
	_ = json.Unmarshal(b, &graph)
	for _, v := range graph {
		node := record(v)
		in := record(node["inputs"])
		switch str(node["class_type"]) {
		case "CLIPTextEncode":
			if str(in["text"]) == "__PROMPT__" {
				in["text"] = promptText
			} else if negative != "" {
				in["text"] = negative
			}
		case "EmptySD3LatentImage", "EmptyLatentImage":
			in["width"] = width
			in["height"] = height
		case "KSampler":
			in["seed"] = seed
		}
	}
	return graph
}
func (e *Engine) Generate(ctx context.Context, text, dest, negative string) error {
	// An image is a file this writes, never a link to one, and writing through a
	// link that points at itself fails before the rename that would have
	// replaced it. Cleared here rather than at each call site because this is
	// the one place the bytes land.
	if err := ClearLink(dest); err != nil {
		return err
	}
	c := e.Config
	host := strings.TrimRight(str(first(c.Enrich.ComfyURL, os.Getenv("COMFY_URL"), "http://127.0.0.1:8188")), "/")
	client := &http.Client{Timeout: 30 * time.Second}
	request := func(method, path string, payload any) ([]byte, error) {
		var body io.Reader
		if payload != nil {
			b, err := json.Marshal(payload)
			if err != nil {
				return nil, err
			}
			body = bytes.NewReader(b)
		}
		req, err := http.NewRequestWithContext(ctx, method, host+path, body)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			return nil, fmt.Errorf("ComfyUI HTTP %d", res.StatusCode)
		}
		return io.ReadAll(io.LimitReader(res.Body, 64<<20))
	}
	graph := RenderGraph(c.Enrich.CoverEngine, text, negative, rand.IntN(1<<31-1)+1, c.Enrich.CoverWidth, c.Enrich.CoverHeight)
	b, err := request("POST", "/prompt", Record{"prompt": graph, "client_id": fmt.Sprintf("hypnotica-%d", rand.IntN(1<<30))})
	if err != nil {
		return err
	}
	var r Record
	if err = json.Unmarshal(b, &r); err != nil {
		return err
	}
	pid := str(r["prompt_id"])
	if pid == "" {
		return fmt.Errorf("ComfyUI rejected the graph: %s", truncate(string(b), 800))
	}
	deadline := time.Now().Add(15 * time.Minute)
	for time.Now().Before(deadline) {
		b, err = request("GET", "/history/"+url.PathEscape(pid), nil)
		if err == nil {
			var history Record
			_ = json.Unmarshal(b, &history)
			entry := record(history[pid])
			if len(entry) > 0 {
				status := record(entry["status"])
				if str(status["status_str"]) == "error" {
					return fmt.Errorf("render failed: %v", status["messages"])
				}
				for _, v := range record(entry["outputs"]) {
					for _, iv := range array(record(v)["images"]) {
						im := record(iv)
						q := url.Values{"filename": {str(im["filename"])}, "subfolder": {str(im["subfolder"])}, "type": {str(first(im["type"], "output"))}}
						blob, err := request("GET", "/view?"+q.Encode(), nil)
						if err != nil {
							return err
						}
						if _, _, err = image.DecodeConfig(bytes.NewReader(blob)); err != nil {
							return fmt.Errorf("renderer returned invalid image: %w", err)
						}
						_, err = AtomicWrite(dest, blob, 0644)
						return err
					}
				}
				if truth(status["completed"]) {
					return fmt.Errorf("render finished with no image")
				}
			}
		}
		if err = pause(ctx, 3*time.Second); err != nil {
			return err
		}
	}
	return fmt.Errorf("render did not finish within 900s")
}
func (e *Engine) CoverFont(author string) string {
	docs, _ := Documents(e.Config.Content, "author")
	for _, d := range docs {
		if str(d.Data["id"]) == author {
			f := strings.ToLower(str(record(d.Data["cover_prompts"])["font"]))
			if _, ok := Fonts[f]; ok {
				return f
			}
		}
	}
	return "heavy-sans"
}
func (e *Engine) RenderCover(ctx context.Context, item, final Record, redraw bool) (string, error) {
	p, _ := PromptFor(final, e.Config.Enrich.CoverEngine, item)
	if p == "" || !e.Config.Enrich.Covers || (truth(item["cover"]) && !redraw) {
		return "", nil
	}
	dest := filepath.Join(e.Config.Covers, str(item["author"]), str(item["id"])+".png")
	if exists(dest) && !redraw {
		return dest, nil
	}
	negative := str(record(item["cover_prompts"])["negative"])
	if err := e.Generate(ctx, p, dest, negative); err != nil {
		return "", err
	}
	StampArt(nested(item, "provenance"), p, negative, e.Config.Enrich.CoverEngine, str(item["title"]))
	_, err := DrawNameplate(dest, str(item["title"]), e.CoverFont(str(item["author"])), true)
	return dest, err
}
