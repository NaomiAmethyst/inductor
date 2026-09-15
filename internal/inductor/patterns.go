// SPDX-License-Identifier: GPL-3.0-only
package inductor

import "regexp"

// Patterns retained from the reference implementation.
var retitleAUDIENCE = regexp.MustCompile(`(?i)^\s*\[?\s*[fma]4[fma]\+?\s*\]\s*`)
var retitleSCRIPTBY = regexp.MustCompile(`(?i)[\[(]?\s*script(?:\s+fill)?\s+by\s*:?\s*(?:/u/|u/)?([A-Za-z0-9_.\-]+)\s*[\])]?`)
var retitleDURATION = regexp.MustCompile(`\s*[\[(]\s*\d{1,2}:\d{2}(?::\d{2})?\s*[\])]\s*$`)
var retitleGROUP = regexp.MustCompile(`\[[^\[\]]*\]`)
var retitleFRAGMENT = regexp.MustCompile(`\s*\(\s*[a-z]+\s*\)\s*$`)
var retitleEXTENSION = regexp.MustCompile(`(?i)[\s._-]+(?:mp3|mp4|m4a|m4v|wav|flac|ogg|wma|wmv|aac)\s*$`)
var retitleENCODE = regexp.MustCompile(`(?i)[\s._-]*(?:video|audio)?[\s._-]*(?:h\.?26[45]|x26[45]|hq|lq|sq|\d{3,4}p|\d+kbps|mixdown|remux|reencode)\b`)
var retitleSITE = regexp.MustCompile(`(?i)^\s*(?:femdom\s+erotic\s+hypnosis|fdhypno(?:\.com)?|[\w-]+\.(?:com|net|org|nl))\s*[-–—:]*\s*`)
var retitleMACHINE = regexp.MustCompile(`^[\p{L}\p{N}_.\-]+$`)
var retitleRUNTOGETHER = regexp.MustCompile(`^[A-Za-z0-9]{8,}$`)
var foldSOFTENERS = regexp.MustCompile(`(?i)^(?:a\s+little|a\s+bit\s+of|a\s+touch\s+of|slightly|slight|mild|mildly|some|light|lightly|gentle\s+amount\s+of|minor)\s+`)
var foldMENTIONS = regexp.MustCompile(`(?i)^(?:a\s+mention\s+of|mentions?\s+of|brief|briefly|passing|implied|implies|hint\s+of|hints\s+of|references?\s+to|allusions?\s+to|suggestions?\s+of)\s+`)
var foldNOTATAG = regexp.MustCompile(`(?i)^(?:adults?\s+only|18\+|nsfw|sfw|audio\s*\d*\s*(?:of\s*\d+)?$|(?:part|track|file|disc)\s+\d+(?:\s+of\s+\d+)?\b|\d+\s+of\s+\d+$|mp3|wav|m4a|full\s+file|complete\s+file|part\s+\d+\s+of\s+the\s+ongoing\s+series\b)`)
var authorsEXPLICIT = regexp.MustCompile(`(?i)\b(nude|nudity|naked|topless|bottomless|undressed|unclothed|bare[- ]?(?:breast|chest|skin)\w*|breasts?|nipples?|areola\w*|cleavage|genital\w*|cock|penis|phallus|vagina|pussy|vulva|anal|anus|enema|buttocks|crotch|masturbat\w*|orgasm\w*|climax|cum\b|ejaculat\w*|fellatio|blowjob|oral sex|intercourse|penetrat\w*|explicit|pornograph\w*|nsfw|lingerie|underwear|panties|thong|negligee|camisole|bustier|babydoll|sheer|see[- ]through|bdsm|bondage|dildo|strap[- ]?on|butt plug|slut|whore)\b`)
