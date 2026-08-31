# forgectl k8s — safely stream ordinary kubectl logs, plus bounded namespace/exec/inspect helpers

> Part of [forgectl](../../README.md) — see the [command roster](../../README.md#command-groups).

```sh
forgectl k8s logs deployment/api -f                     # forward resource/follow args directly to kubectl logs
forgectl k8s logs -n prod -l app=api -f --log-level warn # keep WARN+ JSON logs plus every unrecognized line
forgectl k8s logs pod/api --color never                  # force color policy: auto | always | never
forgectl k8s ns                                          # print the current context's namespace (default when unset)
forgectl k8s ns staging                                  # switch the current context to the staging namespace
forgectl k8s exec -it pod/api -- sh                       # kubectl exec argv forwarded verbatim, real TTY wired through
forgectl k8s inspect deployment/api                       # describe + get -o wide + events, in that fixed order
forgectl k8s inspect pod/api-7f6c9 -n prod                # extra args forward to all three kubectl calls unchanged
```

`forgectl k8s logs` forwards ordinary `kubectl logs` arguments token-for-token;
it does not invoke a shell, choose a pod, define a deployment manifest, or add
an `inspect` abstraction. forgectl consumes only `--log-level` and `--color`
before the first `--`. A separator lets kubectl receive a same-named flag.

Recognized top-level JSON `level` or `severity` strings use the
trace/debug/info/warn/error/fatal ordering. A floor suppresses only recognized
JSON below it: startup banners, malformed JSON, and unknown severity shapes
still print. Output is transformed line-by-line with a fixed memory bound;
oversized lines stream through safely instead of being accumulated.

Pod text and kubectl diagnostics are untrusted terminal input. Control and
invisible formatting runes render as visible escapes before forgectl adds its
own severity color. Ordinary text stays byte-identical. Color is automatic for
a terminal, disabled for redirected output or whenever `NO_COLOR` is present,
and overrideable with `--color always|never`. Cancellation and kubectl's
nonzero exit status propagate through forgectl.
