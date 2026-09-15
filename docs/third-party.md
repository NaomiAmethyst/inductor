# Third-party notices

Inductor's own code is GPL-3.0-only. Its Go dependencies retain their licenses:

| Component | Version | License and notice |
| --- | --- | --- |
| `golang.org/x/crypto` | v0.39.0 | [BSD license](licenses/x-crypto-LICENSE.txt), [patent grant](licenses/x-crypto-PATENTS.txt) |
| `golang.org/x/text` | v0.26.0 | [BSD license](licenses/x-text-LICENSE.txt), [patent grant](licenses/x-text-PATENTS.txt) |
| `golang.org/x/image` | v0.28.0 | [BSD license](licenses/x-image-LICENSE.txt), [patent grant](licenses/x-image-PATENTS.txt) |
| `golang.org/x/sys` | v0.33.0 | [BSD license](licenses/x-sys-LICENSE.txt), [patent grant](licenses/x-sys-PATENTS.txt) |
| `gopkg.in/yaml.v3` | v3.0.1 | [MIT and Apache licenses](licenses/yaml-LICENSE.txt), [notice](licenses/yaml-NOTICE.txt) |
| Go Bold font, Bigelow & Holmes | bundled with x/image | [Font copyright and BSD license](licenses/go-fonts.txt) |

Include this directory and the project's LICENSE and NOTICE when distributing
binaries. `go.mod` and `go.sum` identify the build's dependency versions.

Python worker dependencies, inference model weights, FFmpeg, and system fonts
are installed separately; they are not included in this repository. Their own
licenses and distribution terms apply. The frozen Python reference is Inductor
source and is covered by the repository's GPL-3.0-only license.
