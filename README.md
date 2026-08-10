# quack-extensions

The extension SDK for [quack](https://github.com/fagerbergj/quack), plus
quack's first-party extensions. Full design:
[`docs/design/extension-modules.md`](https://github.com/fagerbergj/quack/blob/main/docs/design/extension-modules.md)
in the quack repo.

## The invariant

**Nothing in this repo may import `github.com/fagerbergj/quack`.** The SDK is
self-contained (chi, slog, adk's `tool.Tool`); extensions import only the
SDK. quack imports this repo - the SDK for the seam types, extension
packages for registration - never the reverse. This is what lets an
extension live outside quack's repo, compile into quack's binary, and still
be enabled or left dormant per deployment by config alone.

A consequence: extensions shape a run only at dispatch time
(`DispatchRequest`'s `Chat`/`Ask`/`Run`/`Delivery` groups). There are no
agent-loop hooks - enforcement stays gate-owned inside quack.

## SDK v0.2.0 (breaking, per the 0.x policy above)

Regroups `DispatchRequest` into `Chat`/`Ask`/`Run`/`Delivery` (v0.1.0's flat
`ChatID`/`Message`/`Background`/`Workflow`/`Attachments` collapse - `Message`
and `Background` had no distinct behavior once serve's adapter just
concatenated them); adds `ChatOrigin.Facets` for multi-valued provenance
dimensions (repo, state, tags); reworks `RunObserver.RunEnded` to carry a
full `RunOutcome` instead of a bare status; and adds two inverse-capability
interfaces, `Deliverer` and `GitCredentialSource`, so quack can call back
into an extension to deliver gated, pushed work and to fetch git
credentials. `ChatRef.LocalID` replaces `ChatID`: it's extension-scoped,
and quack namespaces it into the global chat id as
`ext:<extension>:<localID>` so extensions can never collide with each other
or with user chats. Full rationale in quack's
`.quack/design/sdk-v2-github.md`.

## Module layout

Multi-module monorepo, one Go module per directory, each independently
tagged (the otel-contrib shape):

```text
sdk/       github.com/fagerbergj/quack-extensions/sdk     - the Extension API
noop/      github.com/fagerbergj/quack-extensions/noop    - proves the loop end to end
```

Future extensions (`remarkable/`, and eventually a migrated `github/`) land
as sibling modules the same way.

## Tagging

Each module is tagged independently: `sdk/vX.Y.Z`, `noop/vX.Y.Z`, and so on -
the module prefix distinguishes which module a tag versions, the way
`go.opentelemetry.io/contrib` tags its many modules out of one repo. A
change to only one module gets one tag; a coordinated SDK bump across every
extension is one PR but still lands as separate tags per module.

**All modules stay on 0.x** (no stability promises) until quack itself
reaches 1.0. Treat every SDK minor bump as potentially breaking and pin
exact versions in consumers - don't rely on `^0.x` caret-style ranges.

No `go.work` is committed: each module resolves its siblings as normal
tagged dependencies, the same way an external consumer would, so nothing
about local development leaks into how quack (or anyone else) builds against
this repo.

## How quack consumes this

quack's own `go.mod` pins the blessed extensions and a registry file
blank-imports them (each extension package registers itself via
`sdk.Register` from `init()`). Compiled but unconfigured is dormant;
configured but not compiled is a loud startup error. Deployments enable and
configure extensions through `extensions:` blocks in `quack.yaml`, which
quack hands each extension's `Factory` as opaque config bytes.

See the design doc's "Model" section for the full picture, including the
default batteries-included image and the escape hatch for a minimal/custom
build.
