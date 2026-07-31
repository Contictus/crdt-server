// Builds realistic documents for measuring compression. Not a fixture
// generator: nothing here is committed, and nothing verifies against it.
//
//   node tools/fixturegen/corpus.mjs /tmp
//
// The point is entropy. A generator that types the same twenty words in a loop
// reports a compression ratio of sixty; a real document does not. These cases
// bracket the truth: natural English prose from this repository's own
// documentation at one end, and words drawn from a four-thousand-entry
// vocabulary at the other.
import * as Y from 'yjs'
import { writeFileSync, readFileSync } from 'fs'
import { createHash } from 'crypto'
import { fileURLToPath } from 'url'
import { dirname, join } from 'path'

const here = dirname(fileURLToPath(import.meta.url))
const out = process.argv[2] || '/tmp'

let seed = 12345
const rnd = () => (seed = (seed * 1103515245 + 12345) & 0x7fffffff) / 0x7fffffff

const vocab = []
for (let i = 0; i < 4000; i++) {
  const h = createHash('sha256').update('w' + i).digest('hex')
  let w = ''
  for (let j = 0; j < 3 + (i % 8); j++) w += String.fromCharCode(97 + (parseInt(h[j], 16) % 26))
  vocab.push(w)
}
const word = () => vocab[Math.floor(rnd() * vocab.length)]

// Typed one word at a time, because a person types and does not paste: each
// insertion is its own item, which is what makes a snapshot larger than its text.
function typed(words) {
  const doc = new Y.Doc(); const t = doc.getText('body')
  for (let i = 0; i < words; i++) t.insert(t.length, word() + ' ')
  return doc
}
// Revision is what makes a CRDT document grow: deletions leave tombstones, so a
// document that has been worked on is not a document that was typed once.
function revise(doc, rounds, pick) {
  const t = doc.getText('body')
  for (let i = 0; i < rounds; i++) {
    const at = Math.floor(rnd() * Math.max(1, t.length - 40))
    t.delete(at, 4 + Math.floor(rnd() * 12))
    t.insert(at, pick() + ' ')
  }
  return doc
}
function collaborative(rounds, clients) {
  const docs = Array.from({ length: clients }, () => new Y.Doc())
  for (let r = 0; r < rounds; r++) {
    const t = docs[r % clients].getText('body')
    for (let w = 0; w < 25; w++) t.insert(t.length, word() + ' ')
    for (const a of docs) for (const b of docs)
      if (a !== b) Y.applyUpdate(b, Y.encodeStateAsUpdate(a, Y.encodeStateVector(b)))
  }
  return docs[0]
}

const english = (readFileSync(join(here, '../../README.md'), 'utf8')
  + readFileSync(join(here, '../../docs/RUNBOOK.md'), 'utf8')).split(/\s+/).filter(Boolean)
function natural() {
  const doc = new Y.Doc(); const t = doc.getText('body')
  for (const w of english) t.insert(t.length, w + ' ')
  return doc
}
const pickEnglish = () => english[Math.floor(rnd() * english.length)]

const cases = {
  'natural-english': natural(),
  'natural-revised': revise(natural(), 4000, pickEnglish),
  'vocab-typed': typed(12000),
  'vocab-revised': revise(typed(10000), 3000, word),
  'collaborative': collaborative(200, 5),
}
const report = {}
for (const [name, doc] of Object.entries(cases)) {
  const update = Y.encodeStateAsUpdate(doc)
  writeFileSync(join(out, `corpus-${name}.bin`), update)
  report[name] = { bytes: update.length, chars: doc.getText('body').length }
}
console.log(JSON.stringify(report, null, 2))
