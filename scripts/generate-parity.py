#!/usr/bin/env python3
# SPDX-License-Identifier: GPL-3.0-only
"""Regenerate offline Go parity fixtures using the frozen Python reference.

Usage: PYTHONPATH=reference/python python scripts/generate-parity.py
Requires the reference's pyyaml, numpy, and av dependencies. No model calls.
"""
import ast,json,pathlib,sys
import numpy as np
from inductor import naming, tags, retitle, transcript, analysis_store, duplicates, acoustic, finalise
out=pathlib.Path('internal/inductor/testdata');out.mkdir(parents=True,exist_ok=True)
strings=set(['Straße','ẞ İ Σς σ','é café','one\x1ctwo','one\u0085two','one\u00a0two','Hello! "Next." End.','Really? (Yes.) Now go.','Wait... lower case.','F4M','FM4A'])
for n in ast.walk(ast.parse(pathlib.Path('reference/python/tests/test_inductor.py').read_text())):
 if isinstance(n,ast.Constant) and isinstance(n.value,str) and 0<len(n.value)<400:strings.add(n.value)
functions={'slug':naming.slug,'fold':tags.fold,'unrun':retitle.unrun,'machine':retitle.looks_machine_made,'not_title':retitle.not_really_a_title,'tidy_case':retitle.tidy_case,'title_quality':duplicates.title_quality,'split':transcript._split,'transcript_key':analysis_store.transcript_key}
cases=[]
for text in sorted(strings):
 for op,fn in functions.items():cases.append({'op':op,'input':text,'want':fn(text)})
 title,notes=retitle.tidy(text);cases.append({'op':'tidy','input':text,'want':{'title':title,'notes':notes}})
payloads=[{'duration':30,'text':'One sentence. A second sentence!','segments':[{'start':0,'end':8,'text':'One sentence. A second sentence!'}]}, {'duration':12,'text':'word '*40,'segments':[{'start':1,'end':3,'text':'word '*15},{'start':4,'end':6,'text':'word '*15},{'start':9,'end':12,'text':'word '*10}]}, {'segments':[{'text':'Hello! "Next." End. Café is here.','start':2.57,'end':19.04},{'text':'word '*100,'start':20,'end':40}]}]
for p in payloads:
 cases.append({'op':'sentences','input':p,'want':transcript.to_dicts(transcript.sentences(p))})
 cases.append({'op':'delivery','input':p,'want':transcript.delivery(p)})
measures=[{}, {'words_per_minute':65,'speaking_share':.45,'median_pause_s':1.5,'longest_pause_s':10,'f0_median_hz':200,'hnr_db':6.5,'brightness_hz':80,'silence_ratio':.13,'level_variation':.31},{'words_per_minute':125,'brightness_hz':2500,'hnr_db':15,'level_variation':.9}]
for m in measures:cases.append({'op':'measured_block','input':m,'want':finalise.measured_block(m)})
(out/'parity.json').write_text(json.dumps(cases,ensure_ascii=False,indent=1)+'\n')
# Fixed PCM is the contract boundary: the native implementation must measure
# the same signal, independently of the installed FFmpeg decoder version.
rng=np.random.default_rng(12345);t=np.arange(acoustic.RATE*8)/acoustic.RATE
phase=2*np.pi*(135*t+20/3*np.sin(3*t))
samples=(.3*np.sin(phase)+.03*rng.normal(size=t.size)).astype('float32')
samples.tofile(out/'signal.f32')
measure=acoustic.measure(samples);measure.update(acoustic.pitch(samples));measure.update(acoustic.timbre(samples))
(out/'acoustic.json').write_text(json.dumps(measure,indent=1)+'\n')
acoustic.fingerprint_of(samples).tofile(out/'signal.u32')
print(f'{len(cases)} parity cases and one acoustic reference signal written')

# Python comparison samples must retain the same seeded shuffle across ports.
import random
shuffles = []
for seed in (0, 1, 11, -1, 4294967301):
    for size in (1, 2, 10, 623, 625, 1000):
        values = list(range(size))
        random.Random(seed).shuffle(values)
        shuffles.append({"seed": seed, "n": size, "want": values})
(out / "shuffle.json").write_text(json.dumps(shuffles) + "\n")
