#!/usr/bin/env node
/**
 * Applies update bytes produced by the Go implementation into a real Yjs
 * document and checks that the resulting state matches a fixture.
 *
 * This is the acceptance test for wire compatibility: Go encodes, Yjs decodes.
 *
 *   node tools/verify/apply.mjs --fixture text-delete --update out.bin
 *   node tools/verify/apply.mjs --fixture map-set-overwrite --update a.bin --update b.bin
 *   cat out.bin | node tools/verify/apply.mjs --fixture text-delete --update -
 *   node tools/verify/apply.mjs --self-test
 *   node tools/verify/apply.mjs --fixture xml-prosemirror --update out.bin --json
 *
 * Exit code 0 = state matches, 1 = mismatch or error.
 *
 * yjs is imported through tools/fixturegen/dump.mjs so that the verifier always
 * uses the same yjs installation (and version) that generated the fixtures.
 */
import * as fs from 'node:fs'
import * as path from 'node:path'
import { fileURLToPath } from 'node:url'

import { Y, dumpDoc } from '../fixturegen/dump.mjs'

const here = path.dirname(fileURLToPath(import.meta.url))
const repoRoot = path.resolve(here, '..', '..')
const fixturesRoot = path.join(repoRoot, 'testdata', 'fixtures')

const parseArgs = (argv) => {
  const args = { updates: [], fixture: null, selfTest: false, json: false }
  for (let i = 0; i < argv.length; i++) {
    switch (argv[i]) {
      case '--fixture': args.fixture = argv[++i]; break
      case '--update': args.updates.push(argv[++i]); break
      case '--self-test': args.selfTest = true; break
      case '--json': args.json = true; break
      case '--help': case '-h': args.help = true; break
      default: throw new Error(`unknown argument: ${argv[i]}`)
    }
  }
  return args
}

const readStdin = () => fs.readFileSync(0)

const readUpdate = (p) => new Uint8Array(p === '-' ? readStdin() : fs.readFileSync(p))

const loadExpected = (fixture) => {
  const file = path.join(fixturesRoot, fixture, 'expected.json')
  if (!fs.existsSync(file)) throw new Error(`no such fixture: ${fixture} (${file})`)
  return JSON.parse(fs.readFileSync(file, 'utf8'))
}

/**
 * Root types exist in doc.share after applying an update, but stay untyped
 * until something asks for them with a concrete constructor. Instantiate them
 * according to the fixture so the dump can render their content.
 */
const materialize = (doc, expectedTypes) => {
  for (const [name, spec] of Object.entries(expectedTypes)) {
    switch (spec.kind) {
      case 'text': doc.getText(name); break
      case 'map': doc.getMap(name); break
      case 'array': doc.getArray(name); break
      case 'xml': doc.getXmlFragment(name); break
      case 'xmltext': doc.get(name, Y.XmlText); break
      default: throw new Error(`fixture declares unknown root type kind: ${spec.kind}`)
    }
  }
}

/** Apply updates in order into a fresh document and dump the resulting state. */
export const applyAndDump = (expected, updates) => {
  const doc = new Y.Doc({ gc: expected.gc !== false })
  for (const update of updates) {
    Y.applyUpdate(doc, update, 'verify')
  }
  materialize(doc, expected.state.types)
  const state = dumpDoc(doc)
  doc.destroy()
  return state
}

/** Recursive structural diff. Returns human readable difference paths. */
const diff = (actual, expected, at = '', out = []) => {
  if (out.length >= 20) return out
  const ta = Array.isArray(actual) ? 'array' : actual === null ? 'null' : typeof actual
  const te = Array.isArray(expected) ? 'array' : expected === null ? 'null' : typeof expected
  if (ta !== te) {
    out.push(`${at || '<root>'}: type ${ta} != ${te}`)
    return out
  }
  if (te === 'array') {
    if (actual.length !== expected.length) {
      out.push(`${at}: length ${actual.length} != ${expected.length}`)
    }
    for (let i = 0; i < Math.min(actual.length, expected.length); i++) {
      diff(actual[i], expected[i], `${at}[${i}]`, out)
    }
    return out
  }
  if (te === 'object') {
    const keys = new Set([...Object.keys(actual), ...Object.keys(expected)])
    for (const k of [...keys].sort()) {
      if (!(k in actual)) { out.push(`${at}.${k}: missing`); continue }
      if (!(k in expected)) { out.push(`${at}.${k}: unexpected`); continue }
      diff(actual[k], expected[k], `${at}.${k}`, out)
    }
    return out
  }
  if (actual !== expected) {
    out.push(`${at}: ${JSON.stringify(actual)} != ${JSON.stringify(expected)}`)
  }
  return out
}

const truncate = (s, n = 120) => (s.length > n ? `${s.slice(0, n)}…` : s)

const checkOne = (fixture, updatePaths, { json = false, quiet = false, label = fixture } = {}) => {
  const expected = loadExpected(fixture)
  const updates = updatePaths.map(readUpdate)
  const actual = applyAndDump(expected, updates)
  if (json) process.stdout.write(JSON.stringify(actual, null, 2) + '\n')
  const differences = diff(actual, expected.state)
  if (differences.length === 0) {
    if (!quiet) process.stdout.write(`ok    ${label}\n`)
    return true
  }
  process.stderr.write(`FAIL  ${label}\n`)
  for (const d of differences) process.stderr.write(`        ${truncate(d)}\n`)
  return false
}

const selfTest = () => {
  const fixtures = fs.readdirSync(fixturesRoot, { withFileTypes: true })
    .filter((e) => e.isDirectory() && e.name !== 'awareness')
    .map((e) => e.name)
    .sort()
  let failed = 0
  for (const fixture of fixtures) {
    const dir = path.join(fixturesRoot, fixture)
    // 1. the full state update must reproduce the expected state
    if (!checkOne(fixture, [path.join(dir, 'state.bin')])) failed++
    // 2. so must replaying every incremental update in emission order
    const meta = JSON.parse(fs.readFileSync(path.join(dir, 'updates.json'), 'utf8'))
    const incremental = meta.map((m) => path.join(dir, m.file))
    if (incremental.length > 0) {
      if (!checkOne(fixture, incremental, { label: `${fixture} (incremental x${incremental.length})` })) failed++
    }
  }
  process.stdout.write(failed === 0
    ? `\nself-test passed: ${fixtures.length} fixtures\n`
    : `\nself-test FAILED: ${failed} check(s)\n`)
  return failed === 0
}

const usage = `usage:
  apply.mjs --fixture <name> --update <file|-> [--update <file> ...] [--json]
  apply.mjs --self-test
`

const main = () => {
  let args
  try {
    args = parseArgs(process.argv.slice(2))
  } catch (err) {
    process.stderr.write(`${err.message}\n${usage}`)
    process.exit(1)
  }
  if (args.help) { process.stdout.write(usage); return }
  if (args.selfTest) {
    process.exit(selfTest() ? 0 : 1)
  }
  if (!args.fixture || args.updates.length === 0) {
    process.stderr.write(usage)
    process.exit(1)
  }
  try {
    process.exit(checkOne(args.fixture, args.updates, { json: args.json }) ? 0 : 1)
  } catch (err) {
    process.stderr.write(`error: ${err.message}\n`)
    process.exit(1)
  }
}

main()
