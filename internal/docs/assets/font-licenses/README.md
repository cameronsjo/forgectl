# Font licenses

`forgectl docs serve` embeds the Artificer web fonts vendored under
`../artificer/assets/fonts/` and serves them to the browser. Both families are
licensed under the SIL Open Font License 1.1, which requires the copyright
notice and license to travel with the font files. The subsetted woff2 files do
not all carry the license in their own metadata, so the texts live here.

| Files | Family | License |
|---|---|---|
| `jetbrains-mono-*.woff2` | JetBrains Mono | [`JetBrainsMono-OFL.txt`](JetBrainsMono-OFL.txt) |
| `ia-writer-quattro-*.woff2` | iA Writer Quattro S and V (based on IBM Plex) | [`iA-Writer-Quattro-OFL.md`](iA-Writer-Quattro-OFL.md) |

Both texts are copied verbatim from the upstream repositories:
[JetBrains/JetBrainsMono](https://github.com/JetBrains/JetBrainsMono/blob/master/OFL.txt) and
[iaolo/iA-Fonts](https://github.com/iaolo/iA-Fonts/blob/master/iA%20Writer%20Quattro/LICENSE.md).
The fonts are unmodified, so the Reserved Font Names ("iA Writer", "Plex") are
not affected.

## KaTeX

`forgectl docs serve` also embeds KaTeX (`katex.min.js`, `katex.min.css`, and
its `KaTeX_*.woff2` fonts, vendored under `../katex/`) to render math. The
package, fonts included, is under the MIT License, whose notice must travel
with every copy: [`KaTeX-MIT.txt`](KaTeX-MIT.txt), copied verbatim from the
`LICENSE` in the katex 0.18.9 npm tarball. It lives in this directory so it
rides the same goreleaser `files` entries as the font licenses above.
`../provenance-katex.json` records the version and a sha256 per file.

## Mermaid

`forgectl docs serve` also embeds mermaid (`mermaid.min.js`, version 11.12.3)
to render diagrams. It is under the MIT License, whose notice must travel with
every copy: [`Mermaid-MIT.txt`](Mermaid-MIT.txt), copied verbatim from the
`LICENSE` in the mermaid 11.12.3 npm tarball (the same tarball whose
`dist/mermaid.min.js` matches the sha256 in `../provenance-mermaid.json`). It
lives in this directory so it rides the same goreleaser `files` entries as the
licenses above. The minified bundle also inlines mermaid's own dependencies (marked, the d3
family, dagre-d3-es, dayjs, DOMPurify, and more), and its inline banner carries
only a few of their notices. [`THIRD_PARTY_NOTICES-mermaid.txt`](THIRD_PARTY_NOTICES-mermaid.txt)
reproduces the LICENSE text of the packages in the dependency tree the bundle
was built from (a superset: it also lists type-only packages). It is generated,
not hand-edited: `scripts/vendor-mermaid.sh` downloads the pinned tarball,
verifies its sha256 and that of `dist/mermaid.min.js` against
`../provenance-mermaid.json`, resolves the tree at the versions mermaid's own
lockfile used (recorded there under `notice_resolution`), regenerates the
notices, and fails if a library marked in `bundle_markers` occurs in the bundle
without a notice. Transitive packages resolve within their ranges, so their
versions can differ by a patch or minor from the inlined copies; the license
texts do not.
