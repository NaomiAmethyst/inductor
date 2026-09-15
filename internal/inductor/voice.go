// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"math"
	"path/filepath"
	"sort"
	"strings"
)

const SameVoice = .55

func IsBin(author string) bool {
	a := strings.ToLower(strings.TrimSpace(author))
	for _, prefix := range []string{"unknown", "various", "assorted", "misc"} {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return a == "" || a == "compilation"
}
func vector(v any) []float64 {
	a := array(v)
	out := make([]float64, len(a))
	for i, x := range a {
		out[i] = number(x)
	}
	return out
}
func Cosine(a, b []float64) float64 {
	sum := 0.
	for i := 0; i < min(len(a), len(b)); i++ {
		sum += a[i] * b[i]
	}
	return sum
}
func MeanVector(vectors [][]float64) []float64 {
	if len(vectors) == 0 {
		return nil
	}
	out := make([]float64, len(vectors[0]))
	for _, v := range vectors {
		if len(v) != len(out) {
			continue
		}
		for i, x := range v {
			out[i] += x
		}
	}
	norm := math.Sqrt(Cosine(out, out))
	if norm == 0 {
		return nil
	}
	for i := range out {
		out[i] /= norm
	}
	return out
}
func Centroids(c Config) (map[string][]float64, []Document, error) {
	docs, err := Documents(c.Content, "item")
	if err != nil {
		return nil, nil, err
	}
	byAuthor := map[string][][]float64{}
	for _, d := range docs {
		a := str(d.Data["author"])
		if IsBin(a) {
			continue
		}
		fp := str(record(d.Data["provenance"])["fingerprint"])
		r := readJSON(filepath.Join(c.Cache, "voiceprints", fp+".json"))
		if truth(r["voice"]) {
			byAuthor[a] = append(byAuthor[a], vector(r["voice"]))
		}
	}
	out := map[string][]float64{}
	for a, vs := range byAuthor {
		if v := MeanVector(vs); len(v) > 0 {
			out[a] = v
		}
	}
	return out, docs, nil
}

type voiceScore struct {
	Score  float64
	Author string
}

func voiceRivals(v []float64, where map[string][]float64, exclude string) []voiceScore {
	out := []voiceScore{}
	for a, w := range where {
		if a != exclude {
			out = append(out, voiceScore{Cosine(v, w), a})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Author > out[j].Author
	})
	return out
}
func VoiceReport(c Config, floor float64, cameos bool) (Record, error) {
	where, docs, err := Centroids(c)
	if err != nil {
		return nil, err
	}
	verify, attribution, guests := []Record{}, []Record{}, []Record{}
	for _, d := range docs {
		fp := str(record(d.Data["provenance"])["fingerprint"])
		r := readJSON(filepath.Join(c.Cache, "voiceprints", fp+".json"))
		v := vector(r["voice"])
		if len(v) == 0 {
			continue
		}
		author := str(d.Data["author"])
		rivals := voiceRivals(v, where, author)
		if !IsBin(author) && len(where[author]) > 0 && len(rivals) > 0 {
			own := Cosine(v, where[author])
			best := rivals[0]
			if !(own >= floor && own >= best.Score) && best.Score >= .7 && best.Score-own >= .3 {
				verify = append(verify, Record{"id": str(d.Data["id"]), "path": d.Path, "author": author, "own": rounded(own, 3), "sounds_like": best.Author, "score": rounded(best.Score, 3)})
			}
		}
		if IsBin(author) && len(rivals) > 0 {
			best := rivals[0]
			second := voiceScore{}
			if len(rivals) > 1 {
				second = rivals[1]
			}
			if best.Score >= .75 && best.Score-second.Score >= .15 {
				attribution = append(attribution, Record{"id": str(d.Data["id"]), "path": d.Path, "filed_under": author, "sounds_like": best.Author, "score": rounded(best.Score, 3), "runner_up": first(second.Author), "runner_up_score": rounded(second.Score, 3)})
			}
		}
		if cameos {
			byGuest := map[string][]int{}
			span := number(first(r["window_seconds"], 20))
			for n, window := range array(r["windows"]) {
				w := vector(window)
				if Cosine(w, v) >= .45 {
					continue
				}
				rs := voiceRivals(w, where, author)
				who := ""
				if len(rs) > 0 && rs[0].Score >= .6 {
					who = rs[0].Author
				}
				byGuest[who] = append(byGuest[who], n)
			}
			for _, who := range sortedKeys(byGuest) {
				windows := byGuest[who]
				if len(windows) < 2 {
					continue
				}
				at := []float64{}
				for _, n := range windows[:min(6, len(windows))] {
					at = append(at, rounded(float64(n)*span, 1))
				}
				guests = append(guests, Record{"id": str(d.Data["id"]), "author": author, "guest": first(who, "(not in the library)"), "named": who != "", "seconds": rounded(float64(len(windows))*span, 1), "at": at})
			}
		}
	}
	sort.Slice(verify, func(i, j int) bool {
		return number(verify[i]["own"])-number(verify[i]["score"]) < number(verify[j]["own"])-number(verify[j]["score"])
	})
	sort.Slice(attribution, func(i, j int) bool { return number(attribution[i]["score"]) > number(attribution[j]["score"]) })
	sort.Slice(guests, func(i, j int) bool { return number(guests[i]["seconds"]) > number(guests[j]["seconds"]) })
	return Record{"verify": verify, "attribution": attribution, "cameos": guests}, nil
}
func SimilarVoices(c Config, floor float64, most int, write bool) (Record, error) {
	where, docs, err := Centroids(c)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, d := range docs {
		counts[str(d.Data["author"])]++
	}
	near := Record{}
	for _, a := range sortedKeys(where) {
		if counts[a] < 2 {
			continue
		}
		rows := []any{}
		for _, r := range voiceRivals(where[a], where, a) {
			if counts[r.Author] < 2 || r.Score < floor {
				continue
			}
			rows = append(rows, Record{"id": r.Author, "score": rounded(r.Score, 3), "same_person": r.Score >= .85})
			if len(rows) >= most {
				break
			}
		}
		near[a] = rows
	}
	authors, err := Documents(c.Content, "author")
	if err != nil {
		return nil, err
	}
	changed := 0
	for _, d := range authors {
		rows := array(near[str(d.Data["id"])])
		if equivalent(array(d.Data["similar"]), rows) {
			continue
		}
		changed++
		if len(rows) > 0 {
			d.Data["similar"] = rows
		} else {
			delete(d.Data, "similar")
		}
		if write {
			if _, err = SaveDocument(d.Path, d.Data); err != nil {
				return nil, err
			}
		}
	}
	return Record{"changed": changed, "similar": near}, nil
}
