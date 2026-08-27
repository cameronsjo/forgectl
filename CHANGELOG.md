# Changelog

## [0.14.0](https://github.com/cameronsjo/forgectl/compare/v0.13.0...v0.14.0) (2026-08-27)


### Features

* configurable GitHub host — the pin stays total ([#412](https://github.com/cameronsjo/forgectl/issues/412)) ([#414](https://github.com/cameronsjo/forgectl/issues/414)) ([7c5b745](https://github.com/cameronsjo/forgectl/commit/7c5b7455ff8cef8954dc5a3762ca25d53067c1c4))

## [Unreleased]

### Features

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
