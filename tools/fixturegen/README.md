# fixturegen

Generates the binary Yjs wire-protocol fixtures the Go CRDT core is tested against.

```
npm ci
npm run generate          # writes testdata/fixtures/
```

Output is deterministic — regenerating must leave `git status` clean. If it does not, a
scenario is using randomness (most likely a `Y.Doc` whose `clientID` was not pinned).

## Per-scenario files

| File | Contents |
|---|---|
| `state.bin` | `Y.encodeStateAsUpdate(doc)` — the whole document as one v1 update |
| `sv.bin` | `Y.encodeStateVector(doc)` |
| `update-NNN.bin` | every update emitted while the scenario ran, in emission order |
| `updates.json` | metadata for the above, including each update decoded struct by struct |
| `diff-sv.bin` / `diff.bin` | a peer's state vector and the update that brings it up to date |
| `msg-sync-step1.bin` | full websocket frame: `sync` + `SyncStep1` |
| `msg-sync-step2.bin` | full websocket frame: `sync` + `SyncStep2` against an empty state vector |
| `msg-update.bin` | full websocket frame: `sync` + `Update` |
| `expected.json` | the document state Yjs ends up with, plus `state.bin` decoded struct by struct |

`testdata/fixtures/awareness/` holds awareness-protocol fixtures instead, and
`manifest.json` records the library versions the binaries were generated with.

The generator refuses to finish if the fixture set stops covering a content ref or struct
kind it claims to cover (`assertCoverage`).

## Verifying Go output

`tools/verify/apply.mjs` applies Go-produced bytes into a real `Y.Doc` and diffs the result
against `expected.json`:

```
node ../verify/apply.mjs --fixture text-delete --update /path/to/go-output.bin
node ../verify/apply.mjs --self-test
```
