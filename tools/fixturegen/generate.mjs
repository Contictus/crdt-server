/**
 * Fixture generator.
 *
 * Writes binary Yjs wire-protocol fixtures plus the expected document state to
 * testdata/fixtures/. Output is deterministic: running this twice must produce a
 * byte-identical tree (client ids are hardcoded, no timestamps are emitted).
 *
 *   npm run generate
 */
import * as fs from 'node:fs'
import * as path from 'node:path'
import { fileURLToPath } from 'node:url'

import * as Y from 'yjs'
import * as encoding from 'lib0/encoding'
import * as decoding from 'lib0/decoding'
import * as syncProtocol from 'y-protocols/sync'
import {
  Awareness,
  applyAwarenessUpdate,
  encodeAwarenessUpdate,
  removeAwarenessStates
} from 'y-protocols/awareness'

import { scenarios, createHarness } from './scenarios.mjs'
import { buildLib0Vectors, selfCheckLib0Vectors } from './lib0.mjs'
import { dumpDoc, dumpUpdate, scanUpdate, jsonSafe } from './dump.mjs'

const here = path.dirname(fileURLToPath(import.meta.url))
const repoRoot = path.resolve(here, '..', '..')
const fixturesRoot = path.join(repoRoot, 'testdata', 'fixtures')

// Outer websocket framing, from y-websocket/src/y-websocket.js.
const messageSync = 0
const messageAwareness = 1
const messageQueryAwareness = 3

const pkg = JSON.parse(fs.readFileSync(path.join(here, 'package.json'), 'utf8'))

const writeBin = (dir, name, bytes) => {
  fs.writeFileSync(path.join(dir, name), Buffer.from(bytes))
  return { name, bytes: bytes.length }
}

const writeJSON = (dir, name, value) => {
  fs.writeFileSync(path.join(dir, name), JSON.stringify(value, null, 2) + '\n')
  return { name }
}

/** varUint(messageSync) + syncStep1(sv) — exactly what y-websocket puts on the wire. */
const frameSyncStep1 = (doc) => {
  const encoder = encoding.createEncoder()
  encoding.writeVarUint(encoder, messageSync)
  syncProtocol.writeSyncStep1(encoder, doc)
  return encoding.toUint8Array(encoder)
}

const frameSyncStep2 = (doc, encodedStateVector) => {
  const encoder = encoding.createEncoder()
  encoding.writeVarUint(encoder, messageSync)
  syncProtocol.writeSyncStep2(encoder, doc, encodedStateVector)
  return encoding.toUint8Array(encoder)
}

const frameUpdate = (update) => {
  const encoder = encoding.createEncoder()
  encoding.writeVarUint(encoder, messageSync)
  syncProtocol.writeUpdate(encoder, update)
  return encoding.toUint8Array(encoder)
}

const frameAwareness = (awarenessUpdate) => {
  const encoder = encoding.createEncoder()
  encoding.writeVarUint(encoder, messageAwareness)
  encoding.writeVarUint8Array(encoder, awarenessUpdate)
  return encoding.toUint8Array(encoder)
}

const emptyStateVector = Y.encodeStateVector(new Y.Doc())

const generateScenario = (scenario) => {
  const dir = path.join(fixturesRoot, scenario.name)
  fs.mkdirSync(dir, { recursive: true })

  const h = createHarness()
  const result = scenario.build(h)
  const doc = result.doc

  const files = []
  const state = Y.encodeStateAsUpdate(doc)
  files.push(writeBin(dir, 'state.bin', state))
  files.push(writeBin(dir, 'sv.bin', Y.encodeStateVector(doc)))

  // Every update emitted while the scenario ran, in emission order.
  const updateMeta = h.updates.map((u, i) => {
    const name = `update-${String(i).padStart(3, '0')}.bin`
    files.push(writeBin(dir, name, u.bytes))
    return { file: name, doc: u.label, local: u.local, decoded: dumpUpdate(u.bytes) }
  })
  writeJSON(dir, 'updates.json', updateMeta)

  // A partial update: what a replica holding `diffSvFrom`'s state still misses.
  if (result.diffSvFrom) {
    const peer = h.docs.find((d) => d.label === result.diffSvFrom)
    if (!peer) throw new Error(`${scenario.name}: diffSvFrom refers to unknown doc ${result.diffSvFrom}`)
    const sv = Y.encodeStateVector(peer.doc)
    files.push(writeBin(dir, 'diff-sv.bin', sv))
    files.push(writeBin(dir, 'diff.bin', Y.encodeStateAsUpdate(doc, sv)))
  }

  for (const [name, bytes] of Object.entries(result.extra || {})) {
    files.push(writeBin(dir, name, bytes))
  }

  // Full websocket frames.
  files.push(writeBin(dir, 'msg-sync-step1.bin', frameSyncStep1(doc)))
  files.push(writeBin(dir, 'msg-sync-step2.bin', frameSyncStep2(doc, emptyStateVector)))
  if (h.updates.length > 0) {
    files.push(writeBin(dir, 'msg-update.bin', frameUpdate(h.updates[h.updates.length - 1].bytes)))
  }

  const expected = {
    scenario: scenario.name,
    notes: scenario.notes,
    clients: h.docs.map((d) => ({ label: d.label, clientID: d.clientID, gc: d.gc })),
    // Garbage collection setting of the document this fixture describes. A
    // verifier must use the same setting or tombstones collapse differently.
    gc: doc.gc,
    state: dumpDoc(doc),
    stateBin: dumpUpdate(state)
  }
  writeJSON(dir, 'expected.json', expected)

  return {
    name: scenario.name,
    notes: scenario.notes,
    clients: expected.clients,
    files: files.map((f) => f.name).sort()
  }
}

/** Awareness is a separate protocol with its own encoding and no persistence. */
const generateAwareness = () => {
  const dir = path.join(fixturesRoot, 'awareness')
  fs.mkdirSync(dir, { recursive: true })

  const docA = new Y.Doc(); docA.clientID = 1001
  const docB = new Y.Doc(); docB.clientID = 2002
  const awA = new Awareness(docA) // constructor sets local state {} at clock 0
  const awB = new Awareness(docB)

  const entries = []
  const files = []

  awA.setLocalState({ user: { name: 'ada', color: '#ff8800' }, cursor: { anchor: 3, head: 7 } })
  const single = encodeAwarenessUpdate(awA, [docA.clientID])
  files.push(writeBin(dir, 'update-single.bin', single))
  entries.push({ file: 'update-single.bin', states: decodeAwareness(single) })

  awB.setLocalState({ user: { name: 'grace', color: '#0088ff' }, cursor: null })
  const fromB = encodeAwarenessUpdate(awB, [docB.clientID])
  // A learns about B, then re-broadcasts both states (what a server does on join).
  applyAwarenessUpdate(awA, fromB, 'test')
  const multi = encodeAwarenessUpdate(awA, [docA.clientID, docB.clientID])
  files.push(writeBin(dir, 'update-multi.bin', multi))
  entries.push({ file: 'update-multi.bin', states: decodeAwareness(multi) })

  // Client B goes away: state null at an incremented clock.
  removeAwarenessStates(awA, [docB.clientID], 'test')
  const removal = encodeAwarenessUpdate(awA, [docB.clientID])
  files.push(writeBin(dir, 'update-remove.bin', removal))
  entries.push({ file: 'update-remove.bin', states: decodeAwareness(removal) })

  files.push(writeBin(dir, 'msg-awareness.bin', frameAwareness(multi)))
  const q = encoding.createEncoder()
  encoding.writeVarUint(q, messageQueryAwareness)
  files.push(writeBin(dir, 'msg-query-awareness.bin', encoding.toUint8Array(q)))

  writeJSON(dir, 'expected.json', {
    scenario: 'awareness',
    notes: 'y-protocols/awareness encoding: varUint count, then (clientID, clock, JSON state) triples.',
    clients: [{ label: 'a', clientID: 1001 }, { label: 'b', clientID: 2002 }],
    updates: entries
  })

  awA.destroy()
  awB.destroy()
  docA.destroy()
  docB.destroy()

  return { name: 'awareness', notes: 'awareness protocol', clients: [], files: [...files.map((f) => f.name), 'expected.json'].sort() }
}

/** Decode an awareness update the same way applyAwarenessUpdate reads it. */
const decodeAwareness = (update) => {
  const decoder = decoding.createDecoder(update)
  const len = decoding.readVarUint(decoder)
  const out = []
  for (let i = 0; i < len; i++) {
    const clientID = decoding.readVarUint(decoder)
    const clock = decoding.readVarUint(decoder)
    const state = JSON.parse(decoding.readVarString(decoder))
    out.push({ clientID, clock, state: jsonSafe(state) })
  }
  return out
}

const main = () => {
  fs.rmSync(fixturesRoot, { recursive: true, force: true })
  fs.mkdirSync(fixturesRoot, { recursive: true })

  const lib0Vectors = buildLib0Vectors()
  selfCheckLib0Vectors(lib0Vectors)
  fs.mkdirSync(path.join(fixturesRoot, 'lib0'), { recursive: true })
  writeJSON(path.join(fixturesRoot, 'lib0'), 'vectors.json', lib0Vectors)
  process.stdout.write('  lib0 (primitive codec vectors)\n')

  const generated = []
  for (const scenario of scenarios) {
    generated.push(generateScenario(scenario))
    process.stdout.write(`  ${scenario.name}\n`)
  }
  generated.push(generateAwareness())
  process.stdout.write('  awareness\n')

  writeJSON(fixturesRoot, 'manifest.json', {
    generator: 'tools/fixturegen',
    versions: { ...pkg.dependencies, 'y-websocket': pkg.devDependencies['y-websocket'] },
    updateFormat: 'v1',
    lib0Vectors: 'lib0/vectors.json',
    fixtures: generated
  })

  assertCoverage()
}

/**
 * Fail loudly if a fixture we claim covers something in fact does not. A fixture
 * set that silently stops covering GC or Skip structs is worse than no fixture.
 */
const assertCoverage = () => {
  const seenRefs = new Set()
  const seenKinds = new Set()
  // Info-byte shapes we must keep covering, because they are the ones a decoder
  // written from intuition gets wrong (DECISIONS.md §2.3).
  let parentSubOnWire = 0 // bit 5 set, no origin/rightOrigin -> string follows
  let parentSubInherited = 0 // bit 5 set, origin present -> nothing follows
  const walk = (dir) => {
    for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
      const p = path.join(dir, entry.name)
      if (entry.isDirectory()) { walk(p); continue }
      if (!entry.name.endsWith('.bin')) continue
      if (entry.name.startsWith('sv') || entry.name.startsWith('diff-sv') || entry.name.startsWith('msg-')) continue
      if (dir.endsWith('awareness')) continue
      let decoded
      try {
        decoded = dumpUpdate(new Uint8Array(fs.readFileSync(p)))
      } catch (err) {
        throw new Error(`fixture ${p} does not decode as a v1 update: ${err.message}`)
      }
      for (const s of decoded.structs) {
        seenKinds.add(s.kind)
        if (s.kind === 'Item') seenRefs.add(s.content.ref)
      }

      // The independent grammar scan must agree with yjs on every struct, and
      // must consume the file exactly.
      const scanned = scanUpdate(new Uint8Array(fs.readFileSync(p)))
      if (scanned.consumed !== scanned.total) {
        throw new Error(`${p}: grammar scan consumed ${scanned.consumed} of ${scanned.total} bytes`)
      }
      if (scanned.structs.length !== decoded.structs.length) {
        throw new Error(`${p}: grammar scan found ${scanned.structs.length} structs, yjs found ${decoded.structs.length}`)
      }
      for (let i = 0; i < scanned.structs.length; i++) {
        const a = scanned.structs[i]
        const b = decoded.structs[i]
        if (a.kind !== b.kind || a.id.client !== b.id.client || a.id.clock !== b.id.clock || a.len !== b.len) {
          throw new Error(`${p}: struct ${i} mismatch: scan ${JSON.stringify(a.id)}/${a.kind}/${a.len} vs yjs ${JSON.stringify(b.id)}/${b.kind}/${b.len}`)
        }
        if (a.kind === 'Item' && a.parentSubFlag) {
          if (a.parentSubOnWire) parentSubOnWire++
          else parentSubInherited++
        }
      }
    }
  }
  walk(fixturesRoot)

  // ContentJSON (ref 2) is legacy: current yjs never emits it, so it cannot be
  // fixture-generated. The Go decoder must still support it (hand-built bytes).
  const requiredRefs = [1, 3, 4, 5, 6, 7, 8, 9] // Deleted, Binary, String, Embed, Format, Type, Any, Doc
  const missingRefs = requiredRefs.filter((r) => !seenRefs.has(r))
  const requiredKinds = ['Item', 'GC', 'Skip']
  const missingKinds = requiredKinds.filter((k) => !seenKinds.has(k))
  if (missingRefs.length > 0 || missingKinds.length > 0) {
    throw new Error(
      `fixture coverage gap: missing content refs [${missingRefs}], missing struct kinds [${missingKinds}]`
    )
  }
  if (parentSubOnWire === 0 || parentSubInherited === 0) {
    throw new Error(
      `fixture coverage gap: parentSub cases (on-wire=${parentSubOnWire}, inherited=${parentSubInherited}); both must occur`
    )
  }
  process.stdout.write(
    `coverage: content refs {${[...seenRefs].sort((a, b) => a - b).join(',')}} struct kinds {${[...seenKinds].sort().join(',')}}\n` +
    `          parentSub structs: ${parentSubOnWire} written on the wire, ${parentSubInherited} inherited from left\n`
  )
}

main()
