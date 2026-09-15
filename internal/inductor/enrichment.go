// SPDX-License-Identifier: GPL-3.0-only
package inductor

import (
	"embed"
	"fmt"
	"sort"
	"strings"
)

//go:embed prompts/*.txt prompts/*.json
var prompts embed.FS

func prompt(name string) string {
	b, e := prompts.ReadFile("prompts/" + name + ".txt")
	if e != nil {
		panic("missing embedded prompt: " + name)
	}
	return string(b)
}
func messages(system, user string) []any {
	return []any{Record{"role": "system", "content": system}, Record{"role": "user", "content": user}}
}
func metadataHeader(meta Record, review bool) []string {
	out := []string{"Title: " + str(first(meta["title"], "unknown")), "Creator: " + str(first(meta["author_name"], "unknown"))}
	if truth(meta["duration"]) {
		out = append(out, "Duration: "+Timecode(number(meta["duration"])))
	}
	if truth(meta["series"]) {
		out = append(out, "Series: "+str(meta["series"]))
	}
	if review {
		if truth(meta["duration"]) {
			out = append(out, fmt.Sprintf("Running time: %.0f minutes", number(meta["duration"])/60))
		}
		out = append(out, MeasuredBlock(record(meta["measured"]))...)
	}
	if truth(meta["existing_description"]) {
		intro := "\nThe creator's own write-up (may be marketing copy, may omit things, treat as a claim not a fact):\n"
		if review {
			intro = "\nThe description this entry will carry. Anything it states is already disclosed to whoever reads the listing, so grade disclosure against it — but it is a claim, not evidence: the transcript wins where they disagree.\n"
		}
		out = append(out, intro+truncate(str(meta["existing_description"]), 2500))
	}
	return out
}
func AnalysisMessages(s []Sentence, meta Record, r *Registry) []any {
	return messages(prompt("analyse_system"), strings.Join(metadataHeader(meta, false), "\n")+"\n\nTAG VOCABULARY — prefer these, copied exactly:\n"+r.Block()+"\n\nReturn JSON in exactly this shape:\n"+prompt("analyse_shape")+"\n\nTRANSCRIPT:\n"+Numbered(s))
}
func ClaimSpan(s []Sentence, claim Record, pad, cap int) [][2]int {
	index := map[string]int{}
	for i, v := range s {
		index[v.ID] = i
	}
	ids := []int{}
	for _, k := range []string{"start_id", "end_id"} {
		if i, ok := index[str(claim[k])]; ok {
			ids = append(ids, i)
		}
	}
	if len(ids) == 0 {
		for _, k := range []string{"install_ids", "reinforce_ids", "ids", "evidence_ids"} {
			for _, id := range texts(claim[k]) {
				if i, ok := index[id]; ok {
					ids = append(ids, i)
				}
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	sort.Ints(ids)
	lo, hi := max(0, ids[0]-pad), min(len(s)-1, ids[len(ids)-1]+pad)
	if hi-lo+1 <= cap {
		return [][2]int{{lo, hi}}
	}
	return [][2]int{{lo, lo + cap/2}, {hi - cap/2, hi}}
}
func renderEvidence(s []Sentence, ranges [][2]int) string {
	out := []string{}
	for i, span := range ranges {
		if i > 0 {
			out = append(out, "      [...]")
		}
		body := []string{}
		for _, v := range s[span[0] : span[1]+1] {
			if strings.TrimSpace(v.Text) != "" {
				body = append(body, "("+Timecode(v.Start)+") "+strings.TrimSpace(v.Text))
			}
		}
		if len(body) > 0 {
			out = append(out, "      "+strings.Join(body, " "))
		}
	}
	return strings.Join(out, "\n")
}
func EvidenceFor(a Record, s []Sentence) string {
	blocks := []string{}
	index := map[string]int{}
	for i, v := range s {
		index[v.ID] = i
	}
	for _, section := range []string{"triggers", "compulsions", "other_suggestions"} {
		for i, v := range array(a[section]) {
			claim := record(v)
			label := str(first(claim["name"], claim["effect"], fmt.Sprintf("%s#%d", section, i+1)))
			detail := []string{}
			for _, k := range []string{"effect", "permanence", "severity", "target", "kind", "confidence", "why_unsure"} {
				if truth(claim[k]) {
					detail = append(detail, k+"="+str(claim[k]))
				}
			}
			body := renderEvidence(s, ClaimSpan(s, claim, 1, 60))
			if body == "" {
				body = "      (no usable ids cited)"
			}
			blocks = append(blocks, fmt.Sprintf("%s %d: %s\n    claimed: %s\n%s", strings.TrimSuffix(section, "s"), i+1, label, strings.Join(detail, "; "), body))
		}
	}
	loose := []string{}
	for _, k := range []string{"audience", "anatomy", "sexual_content", "induction", "wakener", "speaker", "audio_notes", "amnesia", "resistance", "compounds_with_repetition"} {
		b := record(a[k])
		ids := []int{}
		for _, f := range []string{"evidence_ids", "orgasm_evidence_ids"} {
			for _, id := range texts(b[f]) {
				if i, ok := index[id]; ok {
					ids = append(ids, i)
				}
			}
		}
		sort.Ints(ids)
		last := -1
		lines := []string{}
		for _, i := range ids {
			if i == last || len(lines) >= 12 {
				continue
			}
			last = i
			lines = append(lines, "      ("+Timecode(s[i].Start)+") "+s[i].Text)
		}
		if len(lines) > 0 {
			loose = append(loose, "    "+k+":\n"+strings.Join(lines, "\n"))
		}
	}
	if len(loose) > 0 {
		blocks = append(blocks, "descriptive claims\n"+strings.Join(loose, "\n"))
	}
	if len(blocks) == 0 {
		return "(the analysis cited no sentences)"
	}
	return strings.Join(blocks, "\n\n")
}
func ReviewMessages(a Record, s []Sentence, meta Record, r *Registry) []any {
	tags := record(a["tags"])
	if len(tags) == 0 {
		tags = Record{"from_vocabulary": texts(a["tags"])}
	}
	firstPass := []string{"SYNOPSIS (first pass): " + strings.TrimSpace(str(a["synopsis"]))}
	if truth(a["premise"]) {
		firstPass = append(firstPass, "PREMISE: "+strings.TrimSpace(str(a["premise"])))
	}
	for _, spec := range [][3]string{{"kinks", "KINKS: ", ", "}, {"risk_notes", "RISK NOTES: ", "; "}} {
		if truth(a[spec[0]]) {
			firstPass = append(firstPass, spec[1]+strings.Join(texts(a[spec[0]]), spec[2]))
		}
	}
	if truth(tags["from_vocabulary"]) {
		firstPass = append(firstPass, "TAGS PROPOSED (from vocabulary): "+strings.Join(texts(tags["from_vocabulary"]), ", "))
	}
	proposed := []string{}
	for _, v := range array(tags["proposed"]) {
		e := record(v)
		if !truth(e["tag"]) {
			continue
		}
		cited := strings.Join(texts(e["ids"]), ", ")
		if cited == "" {
			cited = "no citation"
		}
		proposed = append(proposed, fmt.Sprintf("  - %s  [%s]\n      first pass says: %s", str(e["tag"]), cited, str(first(e["why"], "(no reason given)"))))
	}
	if len(proposed) > 0 {
		firstPass = append(firstPass, "TAGS THE FIRST PASS WANTS TO ADD — rule on each, with reasons:\n"+strings.Join(proposed, "\n"))
	}
	if t := record(a["thumbnail"]); truth(t["description"]) {
		firstPass = append(firstPass, "THUMBNAIL IDEA: "+str(t["description"]))
	}
	if truth(a["notes"]) {
		firstPass = append(firstPass, "FIRST-PASS NOTES: "+str(a["notes"]))
	}
	if truth(a["transcript_quality"]) {
		firstPass = append(firstPass, "TRANSCRIPT QUALITY (first pass): "+str(a["transcript_quality"]))
	}
	user := strings.Join(metadataHeader(meta, true), "\n") + "\n\nTAG REGISTRY — each tag with what it means. Copy exactly where one fits; a definition that covers your meaning settles it.\n" + r.Block() + "\n\n" + strings.Join(firstPass, "\n") + "\n\nCLAIMS AND THEIR EVIDENCE — each claim is followed by the stretch of transcript it points at, plus a sentence either side. This is all you get of the recording, and it is what the claims must be checked against. You are not shown the passages nobody cited, so do not assert anything about them:\n" + EvidenceFor(a, s) + "\n\nReturn JSON in exactly this shape:\n" + prompt("finalise_shape")
	return messages(prompt("finalise_system"), user)
}
func SoloMessages(s []Sentence, meta Record, r *Registry) []any {
	body := []string{}
	for _, v := range s {
		if strings.TrimSpace(v.Text) != "" {
			body = append(body, "("+Timecode(v.Start)+") "+strings.TrimSpace(v.Text))
		}
	}
	return messages(prompt("finalise_solo_system"), strings.Join(metadataHeader(meta, true), "\n")+"\n\nTAG REGISTRY — each tag with what it means. Copy exactly where one fits; a definition that covers your meaning settles it.\n"+r.Block()+"\n\nTRANSCRIPT:\n"+strings.Join(body, " ")+"\n\nReturn JSON in exactly this shape:\n"+prompt("finalise_shape"))
}
func MeasuredBlock(m Record) []string {
	if len(m) == 0 {
		return []string{}
	}
	out := []string{"\nMeasured from the audio. These are facts about the recording that the transcript cannot tell you; use them when describing how it is delivered, and do not use them to decide who is speaking."}
	if wpm := number(m["words_per_minute"]); wpm > 0 {
		pace := "brisk"
		if wpm < 70 {
			pace = "very slow"
		} else if wpm < 100 {
			pace = "slow"
		} else if wpm < 150 {
			pace = "conversational"
		}
		line := fmt.Sprintf("  Pace: %.0f words a minute while actually speaking (%s)", wpm, pace)
		if share := number(m["speaking_share"]); share > 0 {
			line += fmt.Sprintf(", and speech fills %.0f%% of the running time", share*100)
		}
		out = append(out, line)
	}
	if truth(m["median_pause_s"]) {
		line := fmt.Sprintf("  Pauses: %.1fs typically", number(m["median_pause_s"]))
		if truth(m["longest_pause_s"]) {
			line += fmt.Sprintf(", longest %.0fs", number(m["longest_pause_s"]))
		}
		out = append(out, line)
	}
	if m["hnr_db"] != nil {
		h := number(m["hnr_db"])
		how := "clear, full tone"
		if h < 8 {
			how = "breathy, close to a whisper"
		} else if h < 14 {
			how = "soft but voiced"
		}
		out = append(out, fmt.Sprintf("  Voice quality: harmonics-to-noise %.1f dB — %s", h, how))
	}
	if b := number(m["brightness_hz"]); b >= 90 && b <= 6000 {
		out = append(out, fmt.Sprintf("  Brightness: energy centred at %.0f Hz (low is dark and chesty, high is sibilant and close)", b))
	}
	if m["level_variation"] != nil {
		out = append(out, fmt.Sprintf("  Dynamics: level moves by %.1f dB", number(m["level_variation"])))
	}
	if m["silence_ratio"] != nil {
		out = append(out, fmt.Sprintf("  Silence: %.0f%% of the file is near-silent", 100*number(m["silence_ratio"])))
	}
	return out
}
