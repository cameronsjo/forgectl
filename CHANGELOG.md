# Changelog

## [0.19.0](https://github.com/cameronsjo/forgectl/compare/v0.18.0...v0.19.0) (2026-09-30)


### ⚠ BREAKING CHANGES

* **proxy:** refuse a launch profile that proxies with no bypass list
* **proxy:** remove omitted launch-profile variables instead of emptying them

### Features

* **cli:** add --json to launch which, pr list, tmux ls, workflow list, workflow status, and workflow verify ([2a4b281](https://github.com/cameronsjo/forgectl/commit/2a4b281a8115feb944e05aa8345412c8c5482b1a))
* **cli:** add --json to version, k8s ns, ghostty themes and pip path ([649ff53](https://github.com/cameronsjo/forgectl/commit/649ff534cd8c9cea25b5a8e4ead6e112010d4c9b))
* **cli:** bare forgectl opens a hub reaching every command group instead of only the tmux jumper; an unknown top-level verb or subverb (e.g. a typo) now always fails with Cobra's own unknown-command error rather than silently opening a menu ([137f8f1](https://github.com/cameronsjo/forgectl/commit/137f8f11fb5418f9aa12c552515508c383625e64))
* **cli:** docs list gains --timeout (deadline, default 15s) and --limit (bound row count, default unlimited) ([91a907c](https://github.com/cameronsjo/forgectl/commit/91a907cc8e3867860cd93d507803d6d1b7c29f85))
* **docs:** `docs check` now checks vault roots (broken_link, ambiguous_link, broken_anchor via the reader's vault resolver; no orphan findings), reports skipped paths under vaults, and no longer exits 2 for vault-only runs ([55cbcc8](https://github.com/cameronsjo/forgectl/commit/55cbcc845f1ad590f01f7f858bcf6f0c8fc885b9))
* **docs:** `docs search --backend qmd` (or `[docs] search_backend = "qmd"`) runs an opt-in qmd BM25 search, with every hit checked against the docs index ([f827d7a](https://github.com/cameronsjo/forgectl/commit/f827d7aaf40a080de9872a14f9f2b48674ca55d1))
* **docs:** add `forgectl docs read <file>`, which opens an indexed doc in mdroll when it is installed and otherwise in the HTML reader ([9581169](https://github.com/cameronsjo/forgectl/commit/9581169331ad641fcb5d25db955066093dbcfca4))
* **docs:** add `forgectl docs search <query> [--json]`, full-text search over the indexed docs with a ripgrep backend ([5b02770](https://github.com/cameronsjo/forgectl/commit/5b0277004726da30d63973f8e6cf5d5603a987b0))
* **docs:** add an additive severity field to docs check findings; a deprecated page is info and no longer fails the check ([3b61c67](https://github.com/cameronsjo/forgectl/commit/3b61c67af794a5860466f1bbb5d7b886f8fab044))
* **docs:** copying from the docs reader now puts clean HTML (no theme fonts/colours) and formulas as TeX on the clipboard ([a4145c8](https://github.com/cameronsjo/forgectl/commit/a4145c851c0e588545c000bacadae0d3aa4f4316))
* **docs:** docs check honours orphan_ok: true frontmatter and reports summary.ignored_orphans ([33b43ac](https://github.com/cameronsjo/forgectl/commit/33b43acbeb50e2c71ec2d36d216900052ca82133))
* **docs:** docs check link findings carry a 1-based source `line` (JSON key and `path:line` human output), and a directory link counts as inbound to that directory's README/index for the orphan check ([1bf20b5](https://github.com/cameronsjo/forgectl/commit/1bf20b5da36b364ba0c9b147e456ffb0d8cd70ea))
* **docs:** forgectl docs check reports broken links, broken anchors, ambiguous links and orphan pages in docs roots; exits 0 clean, 1 on findings, 2 when it could not run; --json emits a schema_version 1 report ([8cf6953](https://github.com/cameronsjo/forgectl/commit/8cf695390b0b7b4d78f2ec6c310105735e51e540))
* **docs:** render $…$, $$…$$ and ```math blocks as typeset math in docs serve, with a vendored KaTeX ([743a743](https://github.com/cameronsjo/forgectl/commit/743a743596607e7e4560b1b1234cd87a2d94b721))
* **docs:** render Obsidian ==highlights==, %%comments%%, #tag chips and callout aliases in vault roots ([cc1b259](https://github.com/cameronsjo/forgectl/commit/cc1b2598eb4b1d2c58906267675fcd54abeedc41))
* **docs:** report OKF `status: deprecated` and passed `stale_after` as `docs check` findings (exit 1) and badge them in the reader's properties block and status bar ([e43333a](https://github.com/cameronsjo/forgectl/commit/e43333aa66c3e10854784b7247a6c6fa5f15648b))
* **docs:** show plain-text callout titles and attach a standalone ^id line to the preceding list, table or quote ([b05cca4](https://github.com/cameronsjo/forgectl/commit/b05cca496d4013a40e96b4a6031b58157f67106f))
* **docs:** show recently changed docs and per-root counts on the docs reader landing page ([6c82c8c](https://github.com/cameronsjo/forgectl/commit/6c82c8c43a10ea0f81ee9997c1d43977b1c692b9))
* **docs:** stop markdown from mangling $…$, $$…$$ and math-fence TeX in the reader ([8320429](https://github.com/cameronsjo/forgectl/commit/832042922cd54d9c4c93c58a7856f49caabf2ada))
* **docs:** turn single-dollar inline math on for vault roots only, so shell prose like $HOME/bin:$PATH stays literal in docs roots ([3b61c67](https://github.com/cameronsjo/forgectl/commit/3b61c67af794a5860466f1bbb5d7b886f8fab044))
* **docs:** vault [[note#^id]] links jump to the block, which renders as id="^id" with its trailing marker hidden; [[note\|alias]] resolves; a wikilink inside a raw-HTML &lt;a&gt; shows as source; setext heading links match across the line break ([5bb7178](https://github.com/cameronsjo/forgectl/commit/5bb7178d58fe2d10d6adf9ef047df3ac8343dec0))
* **docs:** vault [[wikilinks]] render as links to the note and heading they resolve to; unresolved, ambiguous and out-of-root links show dashed red with the reason on hover ([998cbc6](https://github.com/cameronsjo/forgectl/commit/998cbc683efe9f9a3614714834f3e07a4e079ff8))
* **env:** env set --sops writes one key into a SOPS file, value never in argv ([b42f1a5](https://github.com/cameronsjo/forgectl/commit/b42f1a500348e9121ac2a092d085331bf2cc83d3))
* **env:** env set --sops writes one key into a SOPS file, value never in argv ([8cf2084](https://github.com/cameronsjo/forgectl/commit/8cf2084429fff9ced33243205b4aece94600db95))
* **env:** env set refuses a value containing a carriage return, which python-dotenv would read back as a newline ([5a1fd42](https://github.com/cameronsjo/forgectl/commit/5a1fd4280ebed81c552c8f89c45f000e0ed38684))
* **herdr:** read client and typed errors ([8080713](https://github.com/cameronsjo/forgectl/commit/808071335332ddd916902470da3ab763ee5aafb0))
* **herdr:** session and fork-capability probe, docs ([db19819](https://github.com/cameronsjo/forgectl/commit/db19819c18d4bb472fc7bcfbfa98ae99700adb59))
* **herdr:** shared client, typed errors, and probe ([a3a5a68](https://github.com/cameronsjo/forgectl/commit/a3a5a68549b537fe38f5a2d7c78f76fc989ca744))
* **herdr:** tab moves, workspace moves, focus, declined-move handling ([a85b54e](https://github.com/cameronsjo/forgectl/commit/a85b54e9be127d845c9545004886bc239ea6e974))
* **pr,launch:** inject the launch environment into the clean-room reviewer, and name it in `launch which` ([95687db](https://github.com/cameronsjo/forgectl/commit/95687db8569a1fce82ca4e5ad8da184de8b9d460))
* **pr:** add pr history [--json] for the session audit trail (pr repair --history stays as an alias), report omitted unpaired intents on stderr, add repair_reason and a needs-repair suffix to pr list, and bound teardown's tmux kill so a hung tmux cannot hold the lifecycle lock ([b06ad7e](https://github.com/cameronsjo/forgectl/commit/b06ad7ecf4dfcfa8376e54b3d1dadc682adb4421))
* **pr:** admission cap on every launch path, --queue defers to the drainer ([#472](https://github.com/cameronsjo/forgectl/issues/472) Task 3) ([#516](https://github.com/cameronsjo/forgectl/issues/516)) ([1c47683](https://github.com/cameronsjo/forgectl/commit/1c47683741b4f5b9846e6851e314b2f42615368c))
* **pr:** durable launch phases, slot reservation, and pr repair ([#299](https://github.com/cameronsjo/forgectl/issues/299) Task 2) ([#502](https://github.com/cameronsjo/forgectl/issues/502)) ([ca71948](https://github.com/cameronsjo/forgectl/commit/ca7194882b0f5fa57f113baa70d385612c54927a))
* **pr:** forgectl pr drain posts a macOS notification ("Review started", owner/repo#N) when it launches a queued review; --no-notify turns it off ([9032976](https://github.com/cameronsjo/forgectl/commit/90329767e5f377ac1e4fb1efcb942365b3eda2a6))
* **pr:** lifecycle lock, atomic breadcrumb writer, and v2 record fields ([#299](https://github.com/cameronsjo/forgectl/issues/299)) ([#497](https://github.com/cameronsjo/forgectl/issues/497)) ([2b4c3a0](https://github.com/cameronsjo/forgectl/commit/2b4c3a025191caf8bedd2f41884227907f55b072))
* **projects:** add projects list --strict, which exits 1 when any host degraded ([804e4ad](https://github.com/cameronsjo/forgectl/commit/804e4ad9d3c1a71b69538d68a122b899f2f2f952))
* **proxy:** apply a named launch profile to every launched harness ([2fc6dcd](https://github.com/cameronsjo/forgectl/commit/2fc6dcdf2239633cc0d0629a13979eb736b412a2))
* **proxy:** apply a named profile to every launched harness ([043a8e3](https://github.com/cameronsjo/forgectl/commit/043a8e306c202a2d79f74b0383b1868a252bae94))
* **pr:** pr repair --prune reaps set-aside records and compacts the audit log ([#511](https://github.com/cameronsjo/forgectl/issues/511)) ([e0c3340](https://github.com/cameronsjo/forgectl/commit/e0c33403967f5d97968ffc4c0e1447f83c88225f))
* **pr:** queue and drain verbs ([#473](https://github.com/cameronsjo/forgectl/issues/473) Task 4) ([#518](https://github.com/cameronsjo/forgectl/issues/518)) ([f12019e](https://github.com/cameronsjo/forgectl/commit/f12019e6416fe06f7c03cd718f2bf6b6fd874acd))
* **recipe:** submit --prompt (default /go:afk) instead of a hardcoded /journal, allowlisted like --rename ([7dd9bf2](https://github.com/cameronsjo/forgectl/commit/7dd9bf21dac8a2636d09ba845e0bfc882d60048c))
* **recipe:** submit --prompt (default /go:afk) instead of a hardcoded /journal, allowlisted like --rename ([46eaee2](https://github.com/cameronsjo/forgectl/commit/46eaee21c01f3c189285758497f4d85acca4b66b))
* **sops:** pure domain package — path grammar, value rules, and the line editor ([bbcd762](https://github.com/cameronsjo/forgectl/commit/bbcd7626d2ac7bf3b3c2f50b24aec8d3c3966aa9))
* **tasks:** MCP read tools (list_projects, list_tasks, get_task, ready_tasks) return structuredContent with ids, status, priority, timestamps and counts; board text stays in the fenced text ([d445247](https://github.com/cameronsjo/forgectl/commit/d4452477aeaf1c6ad818497f5767c4c187e422e9))


### Bug Fixes

* **branch:** prune errors quote branch names and paths and no longer echo local git stderr; remote-delete verification checks the remote's push URL, so a fork push URL is no longer verified against upstream ([fba155e](https://github.com/cameronsjo/forgectl/commit/fba155ea3c67fd31a38d4b7c339b643c01e017ac))
* **cask:** emit postflight_steps instead of deprecated postflight ([#525](https://github.com/cameronsjo/forgectl/issues/525)) ([a96b17f](https://github.com/cameronsjo/forgectl/commit/a96b17f6225d87edaa915cf07cd786df4fbe3689)), closes [#523](https://github.com/cameronsjo/forgectl/issues/523)
* **ci:** diff GitHub's merge commit against its first parent in the changelog check ([8cd4cba](https://github.com/cameronsjo/forgectl/commit/8cd4cba3af17d2d6ae5fd9afbfdf940f69c123d7)), closes [#458](https://github.com/cameronsjo/forgectl/issues/458)
* **ci:** judge only the PR's own commits in the changelog-owner check ([1f7fe7e](https://github.com/cameronsjo/forgectl/commit/1f7fe7e6134b0726052320e929bf3f9dfb9b1493)), closes [#458](https://github.com/cameronsjo/forgectl/issues/458)
* **cli:** add --json to every state verb and enforce ADR-0008 ([#537](https://github.com/cameronsjo/forgectl/issues/537)) ([8f5c538](https://github.com/cameronsjo/forgectl/commit/8f5c538fb8aeb3d749b18fcaaea3bf744e515e63))
* **cli:** error messages that start with a flag (for example "--limit must be at least 1") are no longer rendered as "--Limit" ([4267bce](https://github.com/cameronsjo/forgectl/commit/4267bce0c47bd43b72605cef12c0a8f15c4c3b27))
* **cli:** errors keep paths as written; env check documents exit codes; config names init ([#486](https://github.com/cameronsjo/forgectl/issues/486)) ([bf03b23](https://github.com/cameronsjo/forgectl/commit/bf03b23995cf5a56872b8bfdb29651ee2aceba5e))
* **clip:** a failing pbpaste no longer keeps the clipboard contents on the returned error ([c1e468c](https://github.com/cameronsjo/forgectl/commit/c1e468cd4f48a861ebd3d22443fd261be8ab55db))
* **config:** a config.toml that exists but does not parse now exits 2 naming the file, line and column instead of silently using defaults; config, init, doctor, help and version still run ([511dd91](https://github.com/cameronsjo/forgectl/commit/511dd915c51fc7bbcb68cb053b7954e0fa4e869c))
* **config:** drop the quoted value text from toml parse errors, keeping line, column and key ([81be8da](https://github.com/cameronsjo/forgectl/commit/81be8da7e50ca50b07436bd12b27ced1b9d25671))
* **config:** exit 2 when config.toml exists but cannot be read (permission denied, a directory, a FIFO), the same as a parse error ([81be8da](https://github.com/cameronsjo/forgectl/commit/81be8da7e50ca50b07436bd12b27ced1b9d25671))
* **docs:** `docs check` lists link findings in source-line order, and a `^id` inside a `$$` block is no longer a block id in docs roots ([717df38](https://github.com/cameronsjo/forgectl/commit/717df38b7ed4fcd7f6cf30734931482850c7c508))
* **docs:** a doc can no longer plant chrome classes (outline, sidenav, doc-body) that hijack the reader's live-reload swap or sidebar filter ([ad342a7](https://github.com/cameronsjo/forgectl/commit/ad342a715853e83341e2dd8586ca089d313e6443))
* **docs:** a docs-root link ending in "/", "/." or ".." names the directory rather than a same-named .md file, and docs read no longer reveals whether a path outside the root exists through an escaping symlink ([89b0401](https://github.com/cameronsjo/forgectl/commit/89b04017abdd956736d6c228d8f8a837adb0b027))
* **docs:** a document heading or raw-HTML id that collides with a reader chrome id no longer hijacks live status, the sidebar filter or the nav toggle ([87768c4](https://github.com/cameronsjo/forgectl/commit/87768c42bc098a93de72288e7a311817ad84587b))
* **docs:** a document nested past 512 elements no longer lets a stray closer push the page out of the reader's document pane; its divs render as sections ([976a937](https://github.com/cameronsjo/forgectl/commit/976a937b09e0274bf48b892a9e9f758799a8d7c3))
* **docs:** bound parsed heading-fragment work per note (64 KiB); links past it miss instead of resolving by rendered text ([8614650](https://github.com/cameronsjo/forgectl/commit/8614650c35efb93f9a9d0bd852c84c8977dcdb65))
* **docs:** docs list and docs check no longer write text lines to stderr under --json, so stderr is empty or exactly one JSON error object ([4267bce](https://github.com/cameronsjo/forgectl/commit/4267bce0c47bd43b72605cef12c0a8f15c4c3b27))
* **docs:** docs list exits 2 when it cannot run; docs read without mdroll starts the HTML reader only when stdin and stdout are both terminals ([edcf593](https://github.com/cameronsjo/forgectl/commit/edcf5937c8a9c82ad592860e32eb6b0e40df1d4c))
* **docs:** docs roots no longer index [[wikilinks]] as links, matching how they render as text ([3502ba5](https://github.com/cameronsjo/forgectl/commit/3502ba50b354f16fbacfc4ce1c8edc0a9c4183b2))
* **docs:** docs serve setup failures and docs search flag errors now exit 2 (were 1); docs list --json non-deadline failures now emit one JSON error object on stderr ([d6b52dc](https://github.com/cameronsjo/forgectl/commit/d6b52dcadd72f18c2051701bf14f52c55ed74f02))
* **docs:** docs serve shows a notice for documents over 1 MiB, and the index build skips unreadable subdirectories instead of failing ([1848556](https://github.com/cameronsjo/forgectl/commit/1848556247ada4ab76286f652f3cc2e56451b88f))
* **docs:** docs verbs now exit 2 for any failure before work starts and, under --json, write one {"error","code","root"} object to stderr; docs search, serve, open and read pre-work failures that exited 1 now exit 2, and docs search's error objects gain a root key ([2ef612a](https://github.com/cameronsjo/forgectl/commit/2ef612a5ebcac431c9a424768e3a741497bfa70d))
* **docs:** documents over 1 MiB are listed by title only instead of fully parsed; ^block-id markers inside code blocks are no longer indexed; a leading ~ in [docs].roots and root_kinds keys expands to the home directory ([05b1982](https://github.com/cameronsjo/forgectl/commit/05b1982ba5d71829bd6035d7457673a31f577fba))
* **docs:** documents that share a modification time are listed in a stable path order ([5bfd8a5](https://github.com/cameronsjo/forgectl/commit/5bfd8a5c1bd591f2fbdcef18561a2cbf3fef385c))
* **docs:** escape terminal control characters in docs list output ([edcf593](https://github.com/cameronsjo/forgectl/commit/edcf5937c8a9c82ad592860e32eb6b0e40df1d4c))
* **docs:** give every keyboard stop in the docs reader the Artificer focus ring ([6c82c8c](https://github.com/cameronsjo/forgectl/commit/6c82c8c43a10ea0f81ee9997c1d43977b1c692b9))
* **docs:** in docs roots, treat $$ as display math only on its own lines, so shell PIDs and currency in prose stay literal ([fc9d320](https://github.com/cameronsjo/forgectl/commit/fc9d3205773c378ddc9782bd1f50fde6e6c67694))
* **docs:** keep a document's HTML inside the reader's content pane, as a browser parses it, even with stray or unclosed tags or HTML tags inside SVG; render duplicate headings in linear time ([b63a13a](https://github.com/cameronsjo/forgectl/commit/b63a13a0b1971bcf8b8a853231934ecc9f0be193))
* **docs:** keep image alt text containing # in the docs reader ([f46bcd0](https://github.com/cameronsjo/forgectl/commit/f46bcd0fd7e907c523cacb99ee703c213ba127c1))
* **docs:** keep the closed sidebar drawer out of the Tab order and add a skip link ([ae5cbc4](https://github.com/cameronsjo/forgectl/commit/ae5cbc4baf87813f8a5b8f607bfaec2ac7d10d87))
* **docs:** keep the docs status bar on one line at 480px and below by hiding the host, with the full text in a tooltip ([87768c4](https://github.com/cameronsjo/forgectl/commit/87768c42bc098a93de72288e7a311817ad84587b))
* **docs:** keep the page with a banner when the open doc is deleted, and show "disconnected" when live reload gives up ([ae5cbc4](https://github.com/cameronsjo/forgectl/commit/ae5cbc4baf87813f8a5b8f607bfaec2ac7d10d87))
* **docs:** keep the reading position, opened folders, and filter when a doc changes during live reload ([6c82c8c](https://github.com/cameronsjo/forgectl/commit/6c82c8c43a10ea0f81ee9997c1d43977b1c692b9))
* **docs:** leave %% comment text out of a vault page's word count and reading time ([2795c22](https://github.com/cameronsjo/forgectl/commit/2795c2290541ca9ededf8e49759a4da660f285f3))
* **docs:** leave TeX that is over 10,000 characters or nested over 100 levels as source instead of crashing the browser tab, and cap KaTeX sizes at 500em ([100945b](https://github.com/cameronsjo/forgectl/commit/100945bcab5c074b316ac6d13ef392250b50f111))
* **docs:** links to docs whose filenames contain # or ? now resolve correctly in the sidenav and home page ([39cdf6f](https://github.com/cameronsjo/forgectl/commit/39cdf6f11b1e8281820e7ad2dfc3cf73b8f90ca2))
* **docs:** load the Artificer web fonts in the docs reader ([6c82c8c](https://github.com/cameronsjo/forgectl/commit/6c82c8c43a10ea0f81ee9997c1d43977b1c692b9))
* **docs:** over-cap documents no longer take a frontmatter YAML comment as their title ([d07a776](https://github.com/cameronsjo/forgectl/commit/d07a7768fb2144501c0d9b80a06dfdcd97f9ac0f))
* **docs:** raise sidebar folder counts to AA contrast ([ae5cbc4](https://github.com/cameronsjo/forgectl/commit/ae5cbc4baf87813f8a5b8f607bfaec2ac7d10d87))
* **docs:** render code blocks in mono and stop styling headings as links in the docs reader ([6c82c8c](https://github.com/cameronsjo/forgectl/commit/6c82c8c43a10ea0f81ee9997c1d43977b1c692b9))
* **docs:** report a missing file as not found instead of "path escapes its configured root", ignore stale_after values outside RFC 3339's grammar, and name why rg failed when it wrote no stderr ([871815f](https://github.com/cameronsjo/forgectl/commit/871815fcec67675032bc635b0a4ae5249542f998))
* **docs:** resolve a character reference at the end of a line in vault roots (x&[#62](https://github.com/cameronsjo/forgectl/issues/62); rendered as x&amp;[#62](https://github.com/cameronsjo/forgectl/issues/62);, including in callout titles) ([fc9d320](https://github.com/cameronsjo/forgectl/commit/fc9d3205773c378ddc9782bd1f50fde6e6c67694))
* **docs:** show a warning banner when an unclosed &lt;title&gt;, &lt;style&gt;, &lt;image&gt; or similar tag hides the rest of a document ([100945b](https://github.com/cameronsjo/forgectl/commit/100945bcab5c074b316ac6d13ef392250b50f111))
* **docs:** show heading math as TeX source in the "On this page" outline ([f46bcd0](https://github.com/cameronsjo/forgectl/commit/f46bcd0fd7e907c523cacb99ee703c213ba127c1))
* **docs:** status chip is no longer a 44px touch target, the status bar word count no longer wraps at 375px, and note-tier callouts render at body size ([1179e71](https://github.com/cameronsjo/forgectl/commit/1179e71476b650932628cce52d8c0e2808036263))
* **docs:** stop indexing and checking links written inside image alt text ([100945b](https://github.com/cameronsjo/forgectl/commit/100945bcab5c074b316ac6d13ef392250b50f111))
* **docs:** SVG element names such as &lt;path&gt; or &lt;g&gt; written outside an &lt;svg&gt; no longer become HTML elements in the reader; the tag is dropped and its text kept ([9cef30f](https://github.com/cameronsjo/forgectl/commit/9cef30f925d0cad823d11a7818ae4f5ad43c9011))
* **docs:** take docs-root titles from the parsed page, so a "# " line in a code fence, $$ block or frontmatter comment is no longer the title ([2795c22](https://github.com/cameronsjo/forgectl/commit/2795c2290541ca9ededf8e49759a4da660f285f3))
* **docs:** the docs reader no longer drops a link or image title that contains characters such as # : ? % & or a quote ([52cf577](https://github.com/cameronsjo/forgectl/commit/52cf57739a67cbbf9a3ba9fcb00623712bbe168d))
* **docs:** the reader leaves a formula that defines a macro (\def, \newcommand, \let, …) as TeX source instead of rendering it, and caps KaTeX output at 250 DOM levels, so a formula can no longer freeze the tab ([47f8792](https://github.com/cameronsjo/forgectl/commit/47f879211782dbb1fceecdc74c4f126ed0c82502))
* **docs:** vault heading links fold only case and whitespace, so [[Note#snakecase]] no longer reaches "## snake_case" ([2795c22](https://github.com/cameronsjo/forgectl/commit/2795c2290541ca9ededf8e49759a4da660f285f3))
* **doctor:** report sops, brew, trust-store, claude and bench failures as fixed categories instead of raw subprocess or on-disk text ([81be8da](https://github.com/cameronsjo/forgectl/commit/81be8da7e50ca50b07436bd12b27ced1b9d25671))
* **doctor:** the trust store check reports skip instead of fail when no trust anchor or trust store exists ([511dd91](https://github.com/cameronsjo/forgectl/commit/511dd915c51fc7bbcb68cb053b7954e0fa4e869c))
* **env,sops:** a failed restore keeps and names the ciphertext backup instead of deleting it; sops output is no longer saved to $TMPDIR; sops' decrypted temp copy now lives in the guarded work dir, so SIGHUP/SIGQUIT no longer leave the whole document in plaintext ([a7c4b02](https://github.com/cameronsjo/forgectl/commit/a7c4b026ffaf7e8932946d57035c9b9c53f78ef1))
* **env:** `env redact` now masks every `#` comment line, and the trailing comment after a quoted value, to a fixed `# ****`, keeping line alignment with the source ([a10d471](https://github.com/cameronsjo/forgectl/commit/a10d471c85275598f19ea9a9c710099f89cda4a3))
* **env:** `env set --sops` writes a `*` .gitignore into its work directory, so `git add -A` can no longer commit plaintext a killed run leaves behind (the next write still refuses on it) ([683a8db](https://github.com/cameronsjo/forgectl/commit/683a8db7751665f5dbabf5e29e38abe3e73d54fb))
* **env:** a signal during `env set --sops` no longer deletes the ciphertext backup when `.forgectl-sops-<hash>.backup` is already taken; the work directory is kept holding only the backup ([683a8db](https://github.com/cameronsjo/forgectl/commit/683a8db7751665f5dbabf5e29e38abe3e73d54fb))
* **env:** env set --sops no longer leaves a plaintext secret beside the target when interrupted by Ctrl-C, SIGTERM, a closed terminal (SIGHUP) or SIGQUIT ([fd0b83c](https://github.com/cameronsjo/forgectl/commit/fd0b83cb28540c5cbc89471b05cfa5e7810ac2eb))
* **env:** env set and env set --sops now refuse, naming the paths and deleting nothing, when an interrupted run left scratch beside the target; a late signal keeps the SOPS ciphertext backup, and an asynchronous SIGABRT is guarded ([d88040a](https://github.com/cameronsjo/forgectl/commit/d88040a7de18b6a7afe9fcd211b1998879b99ca1))
* **env:** the confirmed --any-file path is the path that gets written ([e4c2009](https://github.com/cameronsjo/forgectl/commit/e4c200937623b83c1bbe3954c4a1ad280742161e))
* **env:** the confirmed --any-file path is the path that gets written ([7bdf6b6](https://github.com/cameronsjo/forgectl/commit/7bdf6b6895121c9de9c4742bbdcdfe87ce4da1a4))
* **env:** the lock open validates its own descriptor, and refusals close theirs ([d40cf53](https://github.com/cameronsjo/forgectl/commit/d40cf53ad4490a60183a24a3d3f43496e90b4924))
* **exec:** cap a failing command's stderr to its last 64 KiB and fail commands whose stdout passes 64 MiB ([fbe01dc](https://github.com/cameronsjo/forgectl/commit/fbe01dc944e690a8356264bdd29872d681ed3662))
* **exec:** mask overlapping marked values completely so no fragment of one survives in error text or logs ([c1e468c](https://github.com/cameronsjo/forgectl/commit/c1e468cd4f48a861ebd3d22443fd261be8ab55db))
* **exec:** stop k8s logs and docs search hanging after cancel when a child's descendant holds the output pipe ([fbe01dc](https://github.com/cameronsjo/forgectl/commit/fbe01dc944e690a8356264bdd29872d681ed3662))
* **githubauth:** pin pr prs/dash searches and doctor's gh auth check to [github] host; branch prune verifies deletes on the origin's host ([804e4ad](https://github.com/cameronsjo/forgectl/commit/804e4ad9d3c1a71b69538d68a122b899f2f2f952))
* herdr target source, changelog-owner check; test: clean-room tolerant reader; chore: dependabot labels ([b32c062](https://github.com/cameronsjo/forgectl/commit/b32c06271fdcc552d02f23ed0c61283806581be3))
* **herdr:** correct the timeout claim, test it with a real killed child ([ab68382](https://github.com/cameronsjo/forgectl/commit/ab68382bd6b0c171c6beffdd406943fb7173e226))
* **herdr:** fail closed on unexpected replies, keep the error cause ([839d36e](https://github.com/cameronsjo/forgectl/commit/839d36e338fc76c6051b49f918743e1341b044bd))
* **herdr:** strip control characters from error text, satisfy repo lint ([7f47feb](https://github.com/cameronsjo/forgectl/commit/7f47feb7d37d978b4f0be1461015cf262fa8d056))
* **pr:** `pr findings cleanup --apply` removes findings dirs through a handle on the store, so a store swapped for a symlink mid-run cannot redirect the removal outside it ([3a4cfcd](https://github.com/cameronsjo/forgectl/commit/3a4cfcd5e6ad77bdce4ac1124284263b4654d8dd))
* **pr:** a findings dir whose owner marker exists but cannot be opened (symlink, permission, fd or I/O error) is now kept with a warning instead of removed as stale ([3a4cfcd](https://github.com/cameronsjo/forgectl/commit/3a4cfcd5e6ad77bdce4ac1124284263b4654d8dd))
* **pr:** a legacy session record that teardown must park is converted to a needs-repair record, so pr repair can settle it ([b1e0d24](https://github.com/cameronsjo/forgectl/commit/b1e0d240520deab13d19c25dc49bfb63b72af761))
* **pr:** bound every tmux call made under the lifecycle lock, and refuse to act on a review window name that more than one window carries ([3bf51f9](https://github.com/cameronsjo/forgectl/commit/3bf51f9de810ea98f8e76ccecb79ec8c9d127528))
* **pr:** cap the needs-repair reason on pr repair and pr dash ([#535](https://github.com/cameronsjo/forgectl/issues/535)) ([292fc15](https://github.com/cameronsjo/forgectl/commit/292fc15d3e00d78f7f1d4c8bcfacdcc161ee4d10))
* **pr:** dash flags needs-repair rows with their reason and stops calling queued records an internal error ([#509](https://github.com/cameronsjo/forgectl/issues/509)) ([dcd29a2](https://github.com/cameronsjo/forgectl/commit/dcd29a2327e67634c6e49e5b01057ef80186814c))
* **pr:** drop rg from PR-mode review permissions; its --pre flag executes arbitrary programs and no deny rule can close quoted spellings ([5f1ed39](https://github.com/cameronsjo/forgectl/commit/5f1ed392f2d7c59a4a170e4d1dfd35b3a0105845))
* **pr:** escape control characters in pr findings list/cleanup paths, and never remove the findings store itself, a nested path, or a non-forgectl-findings-* dir ([6a02a4d](https://github.com/cameronsjo/forgectl/commit/6a02a4db9f7fae09a974deed4c238cc89f026948))
* **pr:** keep review-window environment values out of logs and errors, and refuse URLs with query strings ([6c82c8c](https://github.com/cameronsjo/forgectl/commit/6c82c8c43a10ea0f81ee9997c1d43977b1c692b9))
* **proxy:** refuse a launch profile that proxies with no bypass list ([1103ab3](https://github.com/cameronsjo/forgectl/commit/1103ab391df0416f4381e3bc0b63bdb68d07a650))
* **proxy:** refuse credentials after one or three slashes ([ef0caee](https://github.com/cameronsjo/forgectl/commit/ef0caeefb0d819edd6d103436bacc4d2b9772a0b))
* **proxy:** refuse credentials in a launch profile's proxy URL ([b317a7b](https://github.com/cameronsjo/forgectl/commit/b317a7b78406c097bdc8f683ff5b3d10abbb747c))
* **proxy:** remove omitted launch-profile variables instead of emptying them ([c9559b7](https://github.com/cameronsjo/forgectl/commit/c9559b72cb0544003a3511d73e3871f4f1af30e7))
* **proxy:** resolve the launch profile before anything can be written ([2a4b058](https://github.com/cameronsjo/forgectl/commit/2a4b058721a4dae420d0493af9b838bcec3c7e61))
* **pr:** pr attach's window lookup is bounded, and pr repair --adopt-window says when tmux did not answer instead of reporting a missing window ([b1e0d24](https://github.com/cameronsjo/forgectl/commit/b1e0d240520deab13d19c25dc49bfb63b72af761))
* **pr:** pr cleanup prints one stderr line per session it did not remove, plus a count of what it did ([3bf51f9](https://github.com/cameronsjo/forgectl/commit/3bf51f9de810ea98f8e76ccecb79ec8c9d127528))
* **pr:** pr cleanup shares one tmux budget across its sweep, skips the remaining live sessions once tmux stops answering, and a timed-out kill names the window in the parked record ([3bf51f9](https://github.com/cameronsjo/forgectl/commit/3bf51f9de810ea98f8e76ccecb79ec8c9d127528))
* **pr:** pr findings cleanup --apply records each removal in the repair audit log under the lifecycle lock ([89cd553](https://github.com/cameronsjo/forgectl/commit/89cd553c6f5e354f3ef2696a1aa9d568cdcebb6d))
* **pr:** pr findings cleanup keeps a findings dir whose local review still has a session record, and skips (with a warning) any findings dir without a .forgectl-owner marker, including dirs created before this release ([d435572](https://github.com/cameronsjo/forgectl/commit/d435572e5772fb1759b75a40a3477d9b631b7117))
* **pr:** pr repair --history reports unreadable audit-log lines on stderr, and an append after an unterminated line no longer merges the new row into it ([692801a](https://github.com/cameronsjo/forgectl/commit/692801adeaebb6a861448ab3fbeb4a1aa20f6daf))
* **pr:** pr repair --history shows the newest 2000 rows and no longer fails on an over-long line ([e771795](https://github.com/cameronsjo/forgectl/commit/e77179569761dc2d4775b07267a143fb21521c0e))
* **pr:** pr repair --prune no longer refuses to compact a log holding a line over 8 KiB, and keeps such lines (and any carriage return before a newline) byte-for-byte ([167e8f1](https://github.com/cameronsjo/forgectl/commit/167e8f12a0d6e65668eec7a98d048d7ce90f8058))
* **pr:** refuse a repair audit log that is a symlink, FIFO or device instead of hanging or writing through it ([d5b93e8](https://github.com/cameronsjo/forgectl/commit/d5b93e839209512e36b4f800a1311a428ebd4944))
* **pr:** refuse an empty findings store and bare-prefix names in findings removal ([#578](https://github.com/cameronsjo/forgectl/issues/578)) ([89e4173](https://github.com/cameronsjo/forgectl/commit/89e41736f958fa56effd35d67347087f79035c2a))
* **pr:** refuse credentials in any URL a review window would put on argv ([7389972](https://github.com/cameronsjo/forgectl/commit/738997245970c6e6a7e74c2499845c968c2138c9))
* **pr:** reviewed marks are now keyed by host, so a mark no longer dims a same-named repo's PR on another forge; existing host-less marks are read as the configured [github] host and migrate on next mark ([ae45c0a](https://github.com/cameronsjo/forgectl/commit/ae45c0a945f8e6ea93e430be5436e2404c048df2))
* **pr:** teardown and cleanup write the same intent-then-complete audit rows repair does ([#510](https://github.com/cameronsjo/forgectl/issues/510)) ([cf7e768](https://github.com/cameronsjo/forgectl/commit/cf7e76849fe29274f04dd10da9744c1149e34c4c))
* **pr:** teardown no longer treats an unreadable tmux window list as a gone window; it parks the record in needs-repair and removes nothing ([b1e0d24](https://github.com/cameronsjo/forgectl/commit/b1e0d240520deab13d19c25dc49bfb63b72af761))
* **pr:** the review agent may run only exact base-repo gh reads; GitHub Enterprise review windows pin GH_HOST and carry no gh token variables, and github.com review windows carry no enterprise token variables ([5f1ed39](https://github.com/cameronsjo/forgectl/commit/5f1ed392f2d7c59a4a170e4d1dfd35b3a0105845))
* **recipe:** name the source of a rejected herdr target ([28d3917](https://github.com/cameronsjo/forgectl/commit/28d39176335b21e13363f3dd36b23d34ad47904e)), closes [#464](https://github.com/cameronsjo/forgectl/issues/464)
* **release:** ship mermaid's MIT license in the release archives ([f46bcd0](https://github.com/cameronsjo/forgectl/commit/f46bcd0fd7e907c523cacb99ee703c213ba127c1))
* **review:** make NewGitea host rejection categorical ([#561](https://github.com/cameronsjo/forgectl/issues/561)) ([0ce5527](https://github.com/cameronsjo/forgectl/commit/0ce55279f1750b2b6df35651513e02cbbea31787))
* **sandbox:** a failed clone or worktree add no longer leaves its temp directory behind ([fba155e](https://github.com/cameronsjo/forgectl/commit/fba155ea3c67fd31a38d4b7c339b643c01e017ac))
* **security:** cap the config values and workspace paths echoed in error messages ([81be8da](https://github.com/cameronsjo/forgectl/commit/81be8da7e50ca50b07436bd12b27ced1b9d25671))
* **security:** gh/git/tea failures in projects, pr, branch, sandbox and doctor no longer echo the subprocess's stderr or a server-supplied URL; branch prune output escapes branch names; tasks mcp --ping never prints the probe URL and refuses a host that is not an IP or plain hostname, or a port that is not plain digits; a wing collision names entry numbers instead of wing names ([32762df](https://github.com/cameronsjo/forgectl/commit/32762df9d0359eb0487f49fce41d11ef8ba80f31))
* **security:** rejected config/gh/origin values are no longer echoed (an origin URL token could leak); typed arguments echo capped at 80 runes; gitea host capped at 253 bytes ([804e4ad](https://github.com/cameronsjo/forgectl/commit/804e4ad9d3c1a71b69538d68a122b899f2f2f952))
* **security:** sandbox log lines no longer record a token embedded in a clone URL, and git worktree add failures no longer echo git's stderr ([fba155e](https://github.com/cameronsjo/forgectl/commit/fba155ea3c67fd31a38d4b7c339b643c01e017ac))
* **sops:** the block refusal names the rule, not the key the operator supplied ([281ff61](https://github.com/cameronsjo/forgectl/commit/281ff61b26155b58acd1ecdadf3a3f79591411c3))
* **sops:** the rules check walks the whole path and the verifier resolves it ([59538fc](https://github.com/cameronsjo/forgectl/commit/59538fc13beb01bfc912d4f5b1284fdbe1c65df4))
* **tasks:** list_tasks declares limit bounds (0-200) in its input schema and rejects out-of-range values instead of silently clamping ([d958b6e](https://github.com/cameronsjo/forgectl/commit/d958b6eb5a12c2690c6f43646abc95c85cfe89a1))
* **tasks:** mcp --http requires --pin-ip; pinned dialer copies its list; --ping keeps the body read error ([#488](https://github.com/cameronsjo/forgectl/issues/488)) ([94d2d65](https://github.com/cameronsjo/forgectl/commit/94d2d65c720a411f689c7ee16fa9a0c42ecd9c8c))
* **test:** tree-walking tests skip dot-directories; ignore nested worktrees ([#484](https://github.com/cameronsjo/forgectl/issues/484)) ([f0a3255](https://github.com/cameronsjo/forgectl/commit/f0a32555dd26df6fc7a2d619f0efd6db1d4c8869))
* **theme:** honour the terminal background in auto mode for help and error output ([3c65bf8](https://github.com/cameronsjo/forgectl/commit/3c65bf8909ec9e6a72c1e8a391e8ec0e2de92dbd))
* **tmux:** refuse an env value ending in a semicolon ([2657e85](https://github.com/cameronsjo/forgectl/commit/2657e858017a3ad0987a581368b7449fb1f9530c))
* **tui:** act on the selected tmux-menu row when a filter is applied ([6c82c8c](https://github.com/cameronsjo/forgectl/commit/6c82c8c43a10ea0f81ee9997c1d43977b1c692b9))
* **tui:** tmux menu acts on the selected row, not its filtered position ([fa73978](https://github.com/cameronsjo/forgectl/commit/fa73978973acbd2f12171713ecf2106a02402780)), closes [#496](https://github.com/cameronsjo/forgectl/issues/496)
* **workflow:** a run step whose command prints more than 64 MiB of stdout no longer fails ([c1e468c](https://github.com/cameronsjo/forgectl/commit/c1e468cd4f48a861ebd3d22443fd261be8ab55db))


### Performance Improvements

* **docs:** backlink resolution no longer parses link fragments it discards ([3502ba5](https://github.com/cameronsjo/forgectl/commit/3502ba50b354f16fbacfc4ce1c8edc0a9c4183b2))
* **pr:** pr repair --prune compacts the audit log in two streaming passes, so memory no longer grows with the log's size ([b05bcba](https://github.com/cameronsjo/forgectl/commit/b05bcba136726ed9a03e0fd2330892adbec73dc9))

## [0.18.0](https://github.com/cameronsjo/forgectl/compare/v0.17.3...v0.18.0) (2026-09-08)


### Features

* **tasks:** `forgectl tasks mcp` — an MCP server over the Vikunja board, stdio or streamable HTTP, published as a distroless container image ([c844b88](https://github.com/cameronsjo/forgectl/commit/c844b889e6df1f4e1bfe2253a6140743a48bb4f2))
* **tasks:** read-only Vikunja client with ls/show/ready ([#476](https://github.com/cameronsjo/forgectl/issues/476)) ([2e8406e](https://github.com/cameronsjo/forgectl/commit/2e8406e346e4f8dd83044c2c3616c1ee38e2285f))
* **theme:** Artificer terminal palette, [theme] config with per-role overrides, and theme show/preview ([9f07488](https://github.com/cameronsjo/forgectl/commit/9f074884a124ccd0fa317fabb4268099c049c123))
* **theme:** every coloured surface draws from the Artificer palette; NO_COLOR and pipes stay plain ([2bfbfbf](https://github.com/cameronsjo/forgectl/commit/2bfbfbf3ac87ffaef2355de1ef04fef2d400522e))
* **theme:** style fang's help, version and error output from the palette ([9f07488](https://github.com/cameronsjo/forgectl/commit/9f074884a124ccd0fa317fabb4268099c049c123))


### Bug Fixes

* **cli:** honour NO_COLOR over CLICOLOR_FORCE, including in fang help and error output ([19b5467](https://github.com/cameronsjo/forgectl/commit/19b5467b2c470871ded7a4b025ca7376d2c7f2a3))
* **cli:** send every styled command through a colour-profile writer so NO_COLOR and pipes stay plain ([19b5467](https://github.com/cameronsjo/forgectl/commit/19b5467b2c470871ded7a4b025ca7376d2c7f2a3))
* **recipe:** submit /compact through agent prompt, not the unreleased type-submit ([#475](https://github.com/cameronsjo/forgectl/issues/475)) ([360d2dd](https://github.com/cameronsjo/forgectl/commit/360d2ddbbed678e82597aff900c7acf0ab87cdce))

## [0.17.3](https://github.com/cameronsjo/forgectl/compare/v0.17.2...v0.17.3) (2026-09-05)


### Bug Fixes

* **ci:** run the release job on a hosted runner until fleet signing works ([#462](https://github.com/cameronsjo/forgectl/issues/462)) ([2a3b37b](https://github.com/cameronsjo/forgectl/commit/2a3b37bc55410a39d2d59f38fc2add4df4a2ca34)), closes [#461](https://github.com/cameronsjo/forgectl/issues/461)

## [0.17.2](https://github.com/cameronsjo/forgectl/compare/v0.17.1...v0.17.2) (2026-09-05)


### Bug Fixes

* **ci:** name the signing keychain on every codesign call ([#459](https://github.com/cameronsjo/forgectl/issues/459)) ([fbabaf3](https://github.com/cameronsjo/forgectl/commit/fbabaf381fe8bc6ede40b62d46259afec1f8c20b))

## [0.17.1](https://github.com/cameronsjo/forgectl/compare/v0.17.0...v0.17.1) (2026-09-05)


### Bug Fixes

* **ci:** trust the Developer ID intermediate and assert a valid identity at import time ([#457](https://github.com/cameronsjo/forgectl/issues/457)) ([a3b8433](https://github.com/cameronsjo/forgectl/commit/a3b84330d51fc4ce54f9142c9a7043a776e8eb49))

## [0.17.0](https://github.com/cameronsjo/forgectl/compare/v0.16.0...v0.17.0) (2026-09-05)


### Features

* **docs:** link resolution substrate — `ResolveLink`, `Backlinks`, and per-root link tables in `internal/docs`, with Obsidian vault detection and a `[docs.root_kinds]` config override (`docs` | `vault`). No rendering change yet. ([f2e42c8](https://github.com/cameronsjo/forgectl/commit/f2e42c8269b3a01f3c45148777fce88801ff2021))
* **recipe:** add herdr afk cleanup ([#439](https://github.com/cameronsjo/forgectl/issues/439)) ([223dc01](https://github.com/cameronsjo/forgectl/commit/223dc01b8a9488dc210a5fe47fd4a631fa23fe64))

## [0.16.0](https://github.com/cameronsjo/forgectl/compare/v0.15.0...v0.16.0) (2026-09-01)


### ⚠ BREAKING CHANGES

* **projects:** `projects list --json` `host` is now the full hostname (`github.com`, `git.sjo.lol`) rather than the short tokens `github`/`gitea`, and `--host` takes a hostname or `local` as a closed allowlist. Clones land under the full hostname. Scripts matching the old tokens must be updated.

### Features

* **projects:** `[[projects.wings]]` files named repos at `<projects>/<wing>/<repo>` instead of the host tree, and `projects clone` gains `--dry-run` and `--wing`. `clone` will not create a duplicate checkout across the two layouts. ([9e367a5](https://github.com/cameronsjo/forgectl/commit/9e367a5dcaa6332a0a987f6c2a9307e61583625b))
* **projects:** `projects list --json` `host` is now the full hostname (`github.com`, `git.sjo.lol`) rather than the short tokens `github`/`gitea`, and `--host` takes a hostname or `local` as a closed allowlist. Clones land under the full hostname. Scripts matching the old tokens must be updated. ([9e367a5](https://github.com/cameronsjo/forgectl/commit/9e367a5dcaa6332a0a987f6c2a9307e61583625b))


### Bug Fixes

* **projects:** `projects list`, `pick`, `pull-all`, and `surface launch` did not see repos filed one level under the projects root. Discovery now walks that layout. ([9e367a5](https://github.com/cameronsjo/forgectl/commit/9e367a5dcaa6332a0a987f6c2a9307e61583625b))
* **projects:** `projects worktree` left its base directory behind when any step failed, and that directory's existence is the command's own refuse-if-exists guard — so one transient error made the failure permanent for that repo. ([9e367a5](https://github.com/cameronsjo/forgectl/commit/9e367a5dcaa6332a0a987f6c2a9307e61583625b))
* **projects:** a configured GitHub Enterprise host collapsed to the token `github`, sharing a clone directory and a dedup identity with a github.com repo of the same owner and name. Each host now files and keys under its own hostname. ([9e367a5](https://github.com/cameronsjo/forgectl/commit/9e367a5dcaa6332a0a987f6c2a9307e61583625b))
* **projects:** a remote whose bare hostname was literally `github` was stamped as trusted GitHub inventory and cloned from github.com by owner/name — `canonicalHost` returned short host tokens into the same value space as untrusted hostnames, so the untrusted arm could produce the trusted arm's value. Host identity is now the full hostname everywhere, so there is no token to forge. ([9e367a5](https://github.com/cameronsjo/forgectl/commit/9e367a5dcaa6332a0a987f6c2a9307e61583625b))
* **projects:** repo and owner names arriving from `gh`, `tea`, and clone-target URLs are validated before becoming directories. A repo named `.git` would have made its parent directory read as a repository and hidden every sibling from the inventory. ([9e367a5](https://github.com/cameronsjo/forgectl/commit/9e367a5dcaa6332a0a987f6c2a9307e61583625b))

## [0.15.0](https://github.com/cameronsjo/forgectl/compare/v0.14.1...v0.15.0) (2026-09-01)


### Features

* **docs:** the reader gets its v2 shell — frontmatter renders as an always-visible properties block, GFM alert blockquotes become tiered callouts, an "On this page" outline and a status bar frame the document, the layout responds down to an off-canvas drawer, and mermaid diagrams sit in labeled cards with a reset control ([4965344](https://github.com/cameronsjo/forgectl/commit/4965344a3d1b057bedbf3873e704af4bfb5673cd))


### Bug Fixes

* **ci:** enforce release-please changelog ownership ([#426](https://github.com/cameronsjo/forgectl/issues/426)) ([27332b2](https://github.com/cameronsjo/forgectl/commit/27332b2b21bfd30bdecfc8e48e70ec3d7aac5685))
* **ci:** pin remaining actions to immutable SHAs ([#424](https://github.com/cameronsjo/forgectl/issues/424)) ([9d76e49](https://github.com/cameronsjo/forgectl/commit/9d76e493d23a8915d3aa1ad7b1949ce63db50802))
* **docs:** `docs serve`'s steady-state return now waits for its tracked background goroutines, matching the two startup paths that already did — in practice the `serve` loop, which could still be in flight when the command returned. No race was observed; the invariant simply did not hold on all three paths ([fe32cf6](https://github.com/cameronsjo/forgectl/commit/fe32cf691ecf8d8557cef946467ebd342f2e65e6))
* **docs:** re-vendor the docs reader's Artificer assets 0.19.0 -&gt; 0.25.0, with a committed re-vendor script (`scripts/vendor-artificer.sh`, advisory `--check` drift mode) ([c969979](https://github.com/cameronsjo/forgectl/commit/c96997924851048e0a44f312ecdb6445341555d0))
* **docs:** the reader now renders YAML/TOML frontmatter as a collapsed metadata disclosure instead of leaking it into the body as a broken heading; each indexed root renders as a collapsible directory tree (counts, current-path pre-expanded, filter-aware) instead of a flat list; and syntax highlighting follows the light/dark theme instead of a fixed monokai palette ([46ad601](https://github.com/cameronsjo/forgectl/commit/46ad601dc36ea108df6d13d2d95a7b4b8ff0395f))
* **githubauth:** configured non-default GitHub hosts now launch `gh` with `GH_TOKEN`, `GITHUB_TOKEN`, `GH_ENTERPRISE_TOKEN`, and `GITHUB_ENTERPRISE_TOKEN` absent rather than empty, so the credential boundary no longer depends on GitHub CLI empty-value handling and applies to descendant processes ([5b26cfa](https://github.com/cameronsjo/forgectl/commit/5b26cfa27c110bbbc26deea5dd6b584a9ea7b625))
* **release:** make release-please the changelog writer (phase 1) ([#425](https://github.com/cameronsjo/forgectl/issues/425)) ([cc291ca](https://github.com/cameronsjo/forgectl/commit/cc291ca84c2b9eb37dbf23347a0a7223fa4fd61d))

## [0.14.1](https://github.com/cameronsjo/forgectl/compare/v0.14.0...v0.14.1) (2026-08-28)


### Bug Fixes

* label the legacy launch source, repoint dead godoc, unrace docs serve shutdown ([#419](https://github.com/cameronsjo/forgectl/issues/419)) ([c2cd349](https://github.com/cameronsjo/forgectl/commit/c2cd3498ceb7ecbdac971821b4914cf473952929))
* **launch:** refuse to retire a legacy config forgectl only partly understood ([#418](https://github.com/cameronsjo/forgectl/issues/418)) ([4c864c4](https://github.com/cameronsjo/forgectl/commit/4c864c450b40ef6164032b14e3812d967bd90b83))
* **launch:** the automatic legacy migration — which every launch surface runs, and which backs up and deletes the legacy config — no longer retires a file it only partly decoded. A legacy config carrying settings forgectl cannot represent is now left in place, named on `launch which`/`doctor`, and refused by `launch migrate`, rather than rendered from the modelled subset and deleted ([#417](https://github.com/cameronsjo/forgectl/issues/417))
* **launch:** the "fully superseded" notice now names the backup file, matching its two sibling notices — a message asserting a removal has to carry the recovery pointer ([#417](https://github.com/cameronsjo/forgectl/issues/417))
* **launch:** `launch migrate` no longer reports "Imported 0 launch profile(s)" for a legitimate defaults-only import ([#417](https://github.com/cameronsjo/forgectl/issues/417))
* **launch:** a config file sitting in the legacy directory that forgectl cannot migrate is now named, with its full path, by `launch migrate` and `launch doctor` instead of reported only as an absent `claunch.conf` ([#417](https://github.com/cameronsjo/forgectl/issues/417))
* **launch:** `launch doctor` and `launch which` no longer credit `config.toml` for a launch profile that was read from the legacy `claunch.conf` — including on the documented `FORGECTL_SKIP_LEGACY_MIGRATE=1` path, where the label named a `config.toml` that need not exist at all
* **docs:** `docs serve` no longer exits non-zero on Ctrl-C when a client left an unused connection open. `net/http` will not close a `StateNew` connection until it has sat there five seconds, and the shutdown grace was also five seconds — a dead heat that surfaced as `context deadline exceeded`. The drain window now clears that rule, and a drain that still runs out of time forces the listener closed, warns, and exits zero

## [0.14.0](https://github.com/cameronsjo/forgectl/compare/v0.13.0...v0.14.0) (2026-08-27)


### Features

* configurable GitHub host — the pin stays total ([#412](https://github.com/cameronsjo/forgectl/issues/412)) ([#414](https://github.com/cameronsjo/forgectl/issues/414)) ([7c5b745](https://github.com/cameronsjo/forgectl/commit/7c5b7455ff8cef8954dc5a3762ca25d53067c1c4))
* **projects,review,config:** configurable GitHub host per deployment (`[github] host`) — the projects/review pin stays total, now pointed at the validated configured host; on any non-default host the gh token env vars are scrubbed so only the `gh auth login --hostname <host>` stored credential can be used ([#412](https://github.com/cameronsjo/forgectl/issues/412))

### Bug Fixes

* **projects:** remote-host stamping is now an exact match — a hostname merely *containing* `github.com` (e.g. `evil-github.com.attacker.net`) no longer stamps as trusted `github` inventory ([#412](https://github.com/cameronsjo/forgectl/issues/412))

### Behavior notes

* Flipping `[github] host` leaves old-host reviewed marks inert (never pruned, never re-verified) and leaves old-host clones as unmatched local dirs — deliberate, no migration tooling.
* A config file that fails to decode now makes `projects` and `review` refuse loudly instead of silently defaulting the host to github.com.

## [0.13.0](https://github.com/cameronsjo/forgectl/compare/v0.12.0...v0.13.0) (2026-08-26)


### Features

* **cli:** dispatch external commands from PATH ([#269](https://github.com/cameronsjo/forgectl/issues/269)) ([8374217](https://github.com/cameronsjo/forgectl/commit/837421781da91ba02d58ad34b0463fbcd7884d5f))
* **k8s:** add exec and inspect verbs ([#409](https://github.com/cameronsjo/forgectl/issues/409)) ([9a55d43](https://github.com/cameronsjo/forgectl/commit/9a55d43d655ec1fdc4c289d34c57aa41998b4df5))
* **k8s:** add ns verb for namespace get/set ([#405](https://github.com/cameronsjo/forgectl/issues/405)) ([8563f97](https://github.com/cameronsjo/forgectl/commit/8563f9717ff906a659e67b07400e38298c9cd500))
* **k8s:** add safe streaming logs ([#396](https://github.com/cameronsjo/forgectl/issues/396)) ([9bc1d06](https://github.com/cameronsjo/forgectl/commit/9bc1d06a090a9df37f7d3eacde9c8ec6a1c29038))
* **launch:** add configured Pi harness ([#394](https://github.com/cameronsjo/forgectl/issues/394)) ([1f8a945](https://github.com/cameronsjo/forgectl/commit/1f8a9457b5219a22d48d9c2e890428419607eaa1))
* **launch:** opt-in local launch statistics ([#285](https://github.com/cameronsjo/forgectl/issues/285)) ([c1cc677](https://github.com/cameronsjo/forgectl/commit/c1cc6770cd25997a9192a04f0216a2fe52c81a9d))
* **projects,review:** make owner scope deployment-local and host-pin every gh call ([#292](https://github.com/cameronsjo/forgectl/issues/292)) ([896ffaf](https://github.com/cameronsjo/forgectl/commit/896ffaf0f1d2d244b7a5ba355340e4e6cb784d57))
* **proxy:** add list and status verbs ([#410](https://github.com/cameronsjo/forgectl/issues/410)) ([ff8edc0](https://github.com/cameronsjo/forgectl/commit/ff8edc087c02b7414bdec2dfc147fe0bb926c42e))
* **proxy:** add safe config-defined profiles ([#395](https://github.com/cameronsjo/forgectl/issues/395)) ([90b1aea](https://github.com/cameronsjo/forgectl/commit/90b1aeaf80a3b3dde9f07ce540a29dcb57da5a8a))
* **surface:** a typed core where an ambiguous outcome cannot be hidden ([#345](https://github.com/cameronsjo/forgectl/issues/345)) ([7353570](https://github.com/cameronsjo/forgectl/commit/7353570e42c1ffa85c4678bdf76b7d7decb8af1c))
* **surface:** give cmux and herdr a second witness to a server restart ([#366](https://github.com/cameronsjo/forgectl/issues/366)) ([da16a61](https://github.com/cameronsjo/forgectl/commit/da16a61591cca189acec5c9d706378c5ad95345b))
* **surface:** the bootstrap wire protocol, its nonce, and the peer check ([#349](https://github.com/cameronsjo/forgectl/issues/349)) ([31e3c0d](https://github.com/cameronsjo/forgectl/commit/31e3c0d4a9a20b58514350f49cc9075d028d46bd))
* **surface:** the cmux adapter, and a workspace id that survives contact with cmux ([#360](https://github.com/cameronsjo/forgectl/issues/360)) ([8a35004](https://github.com/cameronsjo/forgectl/commit/8a35004ccb7d6637ee98682c52e8ec467828a447)), closes [#332](https://github.com/cameronsjo/forgectl/issues/332)
* **surface:** the herdr adapter, pinned by the thing that actually selects a server ([#365](https://github.com/cameronsjo/forgectl/issues/365)) ([4d75a61](https://github.com/cameronsjo/forgectl/commit/4d75a61edee4cee4a28b10e495bc142a372616ed)), closes [#332](https://github.com/cameronsjo/forgectl/issues/332)
* **surface:** the launch state machine, the target resolver, and the surface command ([#351](https://github.com/cameronsjo/forgectl/issues/351)) ([4e74780](https://github.com/cameronsjo/forgectl/commit/4e74780aaef40da11831f2f4a78fa6215e71ce45))
* **surface:** the private run directory, and a quoting rule four shells agree on ([#348](https://github.com/cameronsjo/forgectl/issues/348)) ([9519f47](https://github.com/cameronsjo/forgectl/commit/9519f4725ac80ed34d5ec9e1672de490f1e89dc6))
* **surface:** the tmux adapter, and a create whose failure is still answerable ([#355](https://github.com/cameronsjo/forgectl/issues/355)) ([8cddb65](https://github.com/cameronsjo/forgectl/commit/8cddb653cf52ed8c65cd935c52af15ce0c98042e)), closes [#332](https://github.com/cameronsjo/forgectl/issues/332)
* **surface:** the trampoline, and an acknowledgement that cannot be optimistic ([#350](https://github.com/cameronsjo/forgectl/issues/350)) ([de9bd70](https://github.com/cameronsjo/forgectl/commit/de9bd70056c17efbaf18d75963e30dd1ca3b954d))
* **tmux,pr,projects,cli,tui:** migrate every caller to identity targeting ([28981af](https://github.com/cameronsjo/forgectl/commit/28981af0125612955b024a50dd464ddf87a2bda7))
* **tmux:** a socket-pinned client that may create the server it points at ([#352](https://github.com/cameronsjo/forgectl/issues/352)) ([80de23c](https://github.com/cameronsjo/forgectl/commit/80de23cf5e39176a8ff3a7e26b33905b93a932a3)), closes [#332](https://github.com/cameronsjo/forgectl/issues/332)
* **tmux:** target every action by native id, bound to a server generation ([28981af](https://github.com/cameronsjo/forgectl/commit/28981af0125612955b024a50dd464ddf87a2bda7))
* **y:** copy file references and images to the pasteboard ([#406](https://github.com/cameronsjo/forgectl/issues/406)) ([31a3f58](https://github.com/cameronsjo/forgectl/commit/31a3f58c2eca86a4e76b03dd4213d0d6669f491f))
* **y:** read recent zsh commands from $HISTFILE ([#26](https://github.com/cameronsjo/forgectl/issues/26)) ([#318](https://github.com/cameronsjo/forgectl/issues/318)) ([b054fe1](https://github.com/cameronsjo/forgectl/commit/b054fe1558aa769475408dff59e9afd10456928a))


### Bug Fixes

* **bench:** remove the retired Flux status component ([#268](https://github.com/cameronsjo/forgectl/issues/268)) ([36ae6c5](https://github.com/cameronsjo/forgectl/commit/36ae6c5ada5e0e152e48d93fe1113832d88c76f1))
* **ci:** unbreak main — the stale-unlink drift test depended on inode allocation ([#297](https://github.com/cameronsjo/forgectl/issues/297)) ([4e9f1f3](https://github.com/cameronsjo/forgectl/commit/4e9f1f3532adc3fe11f3d8c3992a8003faed7cc0))
* **cli:** gate project and PR pickers on TTY ([#271](https://github.com/cameronsjo/forgectl/issues/271)) ([9ad9c97](https://github.com/cameronsjo/forgectl/commit/9ad9c9728f0d429557ceaec73a56d09b66a1fc39))
* **cli:** preserve safe suggestion structure ([#390](https://github.com/cameronsjo/forgectl/issues/390)) ([4266089](https://github.com/cameronsjo/forgectl/commit/42660898c35ddb3075d51e899495406459346735))
* **cli:** visibly escape unsafe terminal text ([#388](https://github.com/cameronsjo/forgectl/issues/388)) ([376cf5d](https://github.com/cameronsjo/forgectl/commit/376cf5d0b085f8ed0498ada815be78320b5fa420))
* **cmux:** stream workspace listing projection ([#381](https://github.com/cameronsjo/forgectl/issues/381)) ([cbdddbc](https://github.com/cameronsjo/forgectl/commit/cbdddbc2a1c8ce12e73c8014b1b8a903f7715dcd))
* **docker:** keep image name stable before first commit ([#276](https://github.com/cameronsjo/forgectl/issues/276)) ([1794518](https://github.com/cameronsjo/forgectl/commit/179451862a6a7efac481d533e5b0858807247b23))
* **docker:** pass post-dash args through to docker build ([#408](https://github.com/cameronsjo/forgectl/issues/408)) ([e13d60b](https://github.com/cameronsjo/forgectl/commit/e13d60b2c7807418f82b09f09f6e40f11a29b249))
* **pr:** derive review window names from a typed session key ([#301](https://github.com/cameronsjo/forgectl/issues/301)) ([fed6726](https://github.com/cameronsjo/forgectl/commit/fed6726a8f8a896ab6680ebc6bcda2933ac0fdc6))
* **pr:** fail closed when workspace resolution fails ([#273](https://github.com/cameronsjo/forgectl/issues/273)) ([54c753e](https://github.com/cameronsjo/forgectl/commit/54c753ee9954625a0824ad0714cd00f3795f1988))
* **pr:** gate the unconfined reviewer on declared authorship, not locality ([#302](https://github.com/cameronsjo/forgectl/issues/302)) ([09933fe](https://github.com/cameronsjo/forgectl/commit/09933fe5d41c283e21bc15e6d9001c1eb4d888ac))
* **pr:** make a stale breadcrumb removable without deleting the wrong file ([#290](https://github.com/cameronsjo/forgectl/issues/290)) ([489ce72](https://github.com/cameronsjo/forgectl/commit/489ce721e1251f42652a5012882b960e43084d21))
* **projects:** PullAll skips repos with unknown git status ([#264](https://github.com/cameronsjo/forgectl/issues/264)) ([ae9a27c](https://github.com/cameronsjo/forgectl/commit/ae9a27c25cf63b14b74c36a9018f24510b742046))
* **pr:** preserve pinned tmux server for window creation ([#380](https://github.com/cameronsjo/forgectl/issues/380)) ([30febe4](https://github.com/cameronsjo/forgectl/commit/30febe44c4c97e4b8608dd381821fae76213d62d))
* **pr:** verify detached review dispatches against tmux server state ([#282](https://github.com/cameronsjo/forgectl/issues/282)) ([fd46aa8](https://github.com/cameronsjo/forgectl/commit/fd46aa8f0514014a140a7b852d40091d50219d3b))
* **quarantine:** bound editor carrier defaults ([#393](https://github.com/cameronsjo/forgectl/issues/393)) ([642610f](https://github.com/cameronsjo/forgectl/commit/642610feaf3b499e77bbb6678430d039b4cf10c7))
* **release:** flip the release PR's autorelease label after tagging ([#407](https://github.com/cameronsjo/forgectl/issues/407)) ([87a02ac](https://github.com/cameronsjo/forgectl/commit/87a02ac8b2df58954ba8db61408b5961c58fdb33))
* **surface:** fresh-bind reconciled workspaces ([#386](https://github.com/cameronsjo/forgectl/issues/386)) ([a37c943](https://github.com/cameronsjo/forgectl/commit/a37c94392e4042471749fba19276c52b43db22e8))
* **surface:** strip Claude child session marker ([#385](https://github.com/cameronsjo/forgectl/issues/385)) ([d727de3](https://github.com/cameronsjo/forgectl/commit/d727de3f6c076c372d23cfc80486a5e7fef51268))
* **surface:** warn about unsafe socket directories ([#384](https://github.com/cameronsjo/forgectl/issues/384)) ([f70d09a](https://github.com/cameronsjo/forgectl/commit/f70d09aedae70203f01fcd0cbaa74d8f237a2200))
* **termsafe:** enforce module-wide JSON safety ([#389](https://github.com/cameronsjo/forgectl/issues/389)) ([d85dc7d](https://github.com/cameronsjo/forgectl/commit/d85dc7dd34b9b75537c525251281bd2c55dc04e5))
* **termsafe:** neutralize Unicode bidi controls ([#272](https://github.com/cameronsjo/forgectl/issues/272)) ([89ba6cf](https://github.com/cameronsjo/forgectl/commit/89ba6cf2bea66c66de7c0049254783aeb9accc72))
* **tmux:** target every action by native id instead of a fuzzy name ([#296](https://github.com/cameronsjo/forgectl/issues/296)) ([28981af](https://github.com/cameronsjo/forgectl/commit/28981af0125612955b024a50dd464ddf87a2bda7))
* **workflow:** reserve registry export names ([#391](https://github.com/cameronsjo/forgectl/issues/391)) ([be609b9](https://github.com/cameronsjo/forgectl/commit/be609b9ef1575d4a7d1e910c4feacc5b3cba8e86))
* **y:** gate redirected history output ([#387](https://github.com/cameronsjo/forgectl/issues/387)) ([2e5033b](https://github.com/cameronsjo/forgectl/commit/2e5033bb5a614f2f677745af166d54befb492e59))


### Performance Improvements

* **projects:** read repository status with one porcelain v2 probe ([#293](https://github.com/cameronsjo/forgectl/issues/293)) ([170c6fb](https://github.com/cameronsjo/forgectl/commit/170c6fb700e8a85d7161b5866d558df704d33f95))

## [0.12.0](https://github.com/cameronsjo/forgectl/compare/v0.11.0...v0.12.0) (2026-08-07)


### Features

* **launch:** auto-migrate claunch.conf instead of warning about it ([#258](https://github.com/cameronsjo/forgectl/issues/258)) ([ce957f7](https://github.com/cameronsjo/forgectl/commit/ce957f7d33fd7f37d74deb7cd3d029d10bb21a16))

## [0.11.0](https://github.com/cameronsjo/forgectl/compare/v0.10.0...v0.11.0) (2026-08-05)


### Features

* **ci:** automate releases with release-please ([#259](https://github.com/cameronsjo/forgectl/issues/259)) ([744b8e4](https://github.com/cameronsjo/forgectl/commit/744b8e40580c8493c3468a3cc07e18d421728677))


### Bug Fixes

* **ci:** mint an App token for release-please ([#261](https://github.com/cameronsjo/forgectl/issues/261)) ([e90b6c5](https://github.com/cameronsjo/forgectl/commit/e90b6c5a50eec27eef4a2e3ae6852486aa43ade5))
