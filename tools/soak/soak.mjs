#!/usr/bin/env node
/**
 * Drives real y-websocket clients against a running ycollab server and checks
 * that they all end up with the same document.
 *
 * This is the automated half of the Phase 2 acceptance criterion: "no
 * divergence after 5 minutes of aggressive concurrent typing". The clients are
 * the real WebsocketProvider over real Y.Doc instances, so the only thing being
 * tested is the server.
 *
 *   go run ./cmd/server -addr 127.0.0.1:8080 &
 *   node tools/soak/soak.mjs --clients 6 --duration 300
 *   node tools/soak/soak.mjs --clients 4 --duration 10    # development run
 *
 * With --urls it is also the Phase 4 criterion: the clients are spread over
 * several replicas, so every edit has to cross Redis to be seen, and --stats
 * reads each replica's counters to show that no update looped.
 *
 *   docker compose -f deploy/docker-compose.cluster.yml up -d --build
 *   node tools/soak/soak.mjs --clients 6 --duration 300 \
 *     --urls ws://127.0.0.1:8081,ws://127.0.0.1:8082,ws://127.0.0.1:8083 \
 *     --stats http://127.0.0.1:8081,http://127.0.0.1:8082,http://127.0.0.1:8083
 *
 * Exit code 0 = every client converged, 1 = divergence or setup failure.
 *
 * yjs is imported through tools/fixturegen/dump.mjs and y-websocket from the
 * same node_modules, so both halves of the client are the versions the fixtures
 * were generated with, and there is exactly one yjs instance in the process.
 */
import { Y } from '../fixturegen/dump.mjs'
import { WebsocketProvider } from '../fixturegen/node_modules/y-websocket/src/y-websocket.js'

const usage = `usage: node soak.mjs [options]

  --clients N     concurrent editors (default 6)
  --duration S    seconds of editing (default 300)
  --url URL       server (default ws://127.0.0.1:8080)
  --urls A,B,C    several servers; clients are spread over them round-robin
  --stats A,B,C   http endpoints to read /statsz from, one per replica
  --room NAME     document name (default soak-<timestamp>)
  --token TOKEN   JWT to present, when the server requires one
  --interval MS   milliseconds between one client's edits (default 25)
  --seed N        PRNG seed, for a reproducible run (default 1)
  --churn         disconnect and reconnect one client periodically (default on)
  --no-churn      keep every client connected throughout
  --quiet         only print the verdict
`

const parseArgs = (argv) => {
  const opts = {
    clients: 6,
    duration: 300,
    urls: ['ws://127.0.0.1:8080'],
    stats: [],
    room: `soak-${Date.now()}`,
    token: '',
    interval: 25,
    seed: 1,
    churn: true,
    quiet: false
  }
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i]
    const next = () => {
      const v = argv[++i]
      if (v === undefined) throw new Error(`${arg} needs a value`)
      return v
    }
    switch (arg) {
      case '--clients': opts.clients = Number(next()); break
      case '--duration': opts.duration = Number(next()); break
      case '--url': opts.urls = [next()]; break
      case '--urls': opts.urls = next().split(',').filter(Boolean); break
      case '--stats': opts.stats = next().split(',').filter(Boolean); break
      case '--room': opts.room = next(); break
      case '--token': opts.token = next(); break
      case '--interval': opts.interval = Number(next()); break
      case '--seed': opts.seed = Number(next()); break
      case '--churn': opts.churn = true; break
      case '--no-churn': opts.churn = false; break
      case '--quiet': opts.quiet = true; break
      case '--help': case '-h': process.stdout.write(usage); process.exit(0); break
      default: throw new Error(`unknown option ${arg}`)
    }
  }
  if (!(opts.clients >= 2)) throw new Error('--clients must be at least 2')
  if (opts.urls.length === 0) throw new Error('at least one --url is needed')
  return opts
}

/** Deterministic PRNG, so a divergence can be reproduced from the seed alone. */
const mulberry32 = (seed) => () => {
  seed = (seed + 0x6d2b79f5) | 0
  let t = Math.imul(seed ^ (seed >>> 15), 1 | seed)
  t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t
  return ((t ^ (t >>> 14)) >>> 0) / 4294967296
}

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))
const hex = (bytes) => Buffer.from(bytes).toString('hex')

const alphabet = 'abcdefghijklmnopqrstuvwxyz '

class Client {
  constructor (label, opts, rng, url) {
    this.label = label
    this.opts = opts
    this.rng = rng
    // Which replica this client talks to. With several, neighbouring clients
    // land on different ones, so every edit has to cross the cluster to be seen.
    this.url = url
    this.doc = new Y.Doc()
    this.text = this.doc.getText('content')
    this.ops = 0
    this.connect()
  }

  connect () {
    this.provider = new WebsocketProvider(this.url, this.opts.room, this.doc, {
      // Without this, clients in one Node process would sync through
      // BroadcastChannel and the server could be broken without the test
      // noticing. The whole point is to exercise the server.
      disableBc: true,
      params: this.opts.token ? { token: this.opts.token } : {}
    })
    this.provider.awareness.setLocalStateField('user', {
      name: this.label,
      color: `#${((this.rng() * 0xffffff) | 0).toString(16).padStart(6, '0')}`
    })
  }

  /** Resolves once the server has answered our SyncStep1. */
  synced () {
    if (this.provider.synced) return Promise.resolve()
    return new Promise((resolve) => this.provider.once('synced', () => resolve()))
  }

  edit () {
    const len = this.text.length
    const r = this.rng()
    if (len > 0 && r < 0.3) {
      const index = Math.floor(this.rng() * len)
      this.text.delete(index, Math.min(1 + Math.floor(this.rng() * 3), len - index))
    } else {
      // Bias towards the two hot spots every collaborative editor actually
      // hits: the very start and the very end. Concurrent inserts at the same
      // index are where YATA tie-breaks decide the outcome.
      const where = this.rng()
      const index = where < 0.3 ? 0 : where < 0.6 ? len : Math.floor(this.rng() * (len + 1))
      let s = ''
      for (let i = 0; i < 1 + Math.floor(this.rng() * 4); i++) {
        s += alphabet[Math.floor(this.rng() * alphabet.length)]
      }
      this.text.insert(index, s)
    }
    this.ops++
  }

  moveCursor () {
    const len = this.text.length
    const anchor = len === 0 ? 0 : Math.floor(this.rng() * len)
    this.provider.awareness.setLocalStateField('cursor', { anchor, head: anchor })
  }

  async reconnect () {
    this.provider.disconnect()
    await sleep(200)
    this.provider.connect()
    await this.synced()
  }

  fingerprint () {
    return {
      text: this.text.toString(),
      sv: hex(Y.encodeStateVector(this.doc))
    }
  }

  destroy () {
    this.provider.destroy()
    this.doc.destroy()
  }
}

/**
 * Waits until every client reports the same state vector twice in a row.
 *
 * Two rounds are required rather than one because a deletion whose structs a
 * replica has not yet integrated is held pending and travels one exchange
 * behind them (DECISIONS C5). A single agreeing sample could be luck.
 */
const waitForQuiescence = async (clients, timeoutMs, log) => {
  const deadline = Date.now() + timeoutMs
  let previous = null
  let stable = 0
  while (Date.now() < deadline) {
    const sample = clients.map((c) => c.fingerprint().sv).join('|')
    const agreed = new Set(clients.map((c) => c.fingerprint().sv)).size === 1
    if (agreed && sample === previous) {
      if (++stable >= 2) return true
    } else {
      stable = 0
    }
    previous = sample
    await sleep(250)
  }
  log('quiescence timed out')
  return false
}

/** Reads /statsz from every replica and sums the counters. */
const clusterTotals = async (endpoints) => {
  const totals = {}
  for (const endpoint of endpoints) {
    const resp = await fetch(new URL('/statsz', endpoint))
    if (!resp.ok) throw new Error(`${endpoint}/statsz: ${resp.status}`)
    const body = await resp.json()
    for (const [name, value] of Object.entries(body.cluster ?? {})) {
      totals[name] = (totals[name] ?? 0) + value
    }
  }
  return totals
}

/**
 * Checks the cluster's own account of what it did.
 *
 * The claim being tested is the Phase 4 acceptance criterion's second half:
 * origin filtering keeps update loops at zero. With the clients quiet, update
 * traffic between replicas has to stop entirely. Anti-entropy announcements
 * carry on - that is what they are for - so only the update counters are held
 * still.
 */
const checkNoLoops = async (endpoints, log) => {
  const before = await clusterTotals(endpoints)
  await sleep(3000)
  const after = await clusterTotals(endpoints)
  log(`cluster: ${before.published_update ?? 0} updates published, ` +
      `${before.self_filtered ?? 0} own envelopes filtered, ` +
      `${before.published_diff ?? 0} repair diffs, ` +
      `${before.answered_state_vector ?? 0} state vectors answered`)
  for (const name of ['published_update', 'published_diff']) {
    if ((after[name] ?? 0) !== (before[name] ?? 0)) {
      console.error(`${name} grew from ${before[name]} to ${after[name]} with nobody typing: updates are looping`)
      return false
    }
  }
  for (const name of ['remote_dropped', 'publish_dropped', 'publish_failed', 'remote_rejected']) {
    if ((after[name] ?? 0) !== 0) {
      console.error(`${name} is ${after[name]}: the cluster lost or refused traffic`)
      return false
    }
  }
  return true
}

/** Waits for every client to see the same number of peers. */
const waitForAwareness = async (clients, want, timeoutMs) => {
  const deadline = Date.now() + timeoutMs
  let seen = clients.map((c) => c.provider.awareness.getStates().size)
  while (Date.now() < deadline) {
    seen = clients.map((c) => c.provider.awareness.getStates().size)
    if (new Set(seen).size === 1 && seen[0] === want) return seen
    await sleep(250)
  }
  return seen
}

const main = async () => {
  const opts = parseArgs(process.argv.slice(2))
  const log = opts.quiet ? () => {} : (...args) => console.log(...args)
  const rng = mulberry32(opts.seed)

  log(`soak: ${opts.clients} clients, ${opts.duration}s, ${opts.urls.join(' ')} room ${opts.room}`)

  const clients = []
  for (let i = 0; i < opts.clients; i++) {
    const url = opts.urls[i % opts.urls.length]
    clients.push(new Client(String.fromCharCode(97 + i), opts, mulberry32(opts.seed * 1000 + i), url))
  }
  if (opts.urls.length > 1) {
    log(`  ${clients.map((c) => `${c.label}=${c.url}`).join(' ')}`)
  }

  const connectDeadline = sleep(15000).then(() => { throw new Error('timed out waiting for the initial sync') })
  await Promise.race([Promise.all(clients.map((c) => c.synced())), connectDeadline])
  log('all clients synced')

  const stop = Date.now() + opts.duration * 1000
  const timers = clients.map((c) => setInterval(() => {
    try {
      c.edit()
    } catch (err) {
      console.error(`client ${c.label}: edit failed:`, err)
    }
  }, opts.interval))
  const cursors = clients.map((c) => setInterval(() => c.moveCursor(), 500))

  let churnTimer = null
  if (opts.churn) {
    let n = 0
    churnTimer = setInterval(async () => {
      const victim = clients[n++ % clients.length]
      log(`  churn: reconnecting ${victim.label}`)
      try {
        await victim.reconnect()
      } catch (err) {
        console.error(`client ${victim.label}: reconnect failed:`, err)
      }
    }, 20000)
  }

  let lastReport = 0
  while (Date.now() < stop) {
    await sleep(1000)
    const left = Math.round((stop - Date.now()) / 1000)
    if (!opts.quiet && left !== lastReport && left % 15 === 0) {
      lastReport = left
      const ops = clients.reduce((sum, c) => sum + c.ops, 0)
      log(`  ${left}s left, ${ops} ops, ${clients[0].text.length} chars`)
    }
  }

  for (const t of timers) clearInterval(t)
  if (churnTimer) clearInterval(churnTimer)
  // The cursor timers keep running: awareness states are refreshed by the
  // people holding them, and a client that just reconnected is invisible to
  // its peers until its own clock moves past what they last saw. Freezing the
  // cursors would be testing a situation no editing session is ever in.
  log('editing stopped, waiting for convergence')

  const quiesced = await waitForQuiescence(clients, 60000, log)

  const prints = clients.map((c) => ({ label: c.label, ...c.fingerprint() }))
  const texts = new Set(prints.map((p) => p.text))
  const svs = new Set(prints.map((p) => p.sv))
  const ops = clients.reduce((sum, c) => sum + c.ops, 0)

  let failed = false
  if (texts.size !== 1 || svs.size !== 1) {
    failed = true
    console.error('DIVERGED')
    for (const p of prints) {
      console.error(`  ${p.label}: ${p.text.length} chars, sv ${p.sv}`)
      console.error(`    ${JSON.stringify(p.text.slice(0, 200))}`)
    }
  } else if (!quiesced) {
    failed = true
    console.error('clients agree but never went quiet; the server is still pushing updates')
  } else {
    log(`converged: ${ops} ops, ${prints[0].text.length} chars, sv ${prints[0].sv}`)
  }

  // Awareness must have converged too, or the demo shows ghost cursors. It is
  // allowed to take longer than the document: after a reconnect a client is
  // invisible to its peers until it publishes a state at a clock past the one
  // they last saw, which is the cursor timer's next tick.
  const seen = await waitForAwareness(clients, opts.clients, 30000)
  if (new Set(seen).size !== 1 || seen[0] !== opts.clients) {
    failed = true
    console.error(`awareness diverged: ${seen.join(', ')} peers seen, want ${opts.clients} everywhere`)
  } else {
    log(`awareness: every client sees ${seen[0]} peers`)
  }

  for (const t of cursors) clearInterval(t)

  if (opts.stats.length > 0 && !(await checkNoLoops(opts.stats, log))) {
    failed = true
  }

  for (const c of clients) c.destroy()
  process.exit(failed ? 1 : 0)
}

main().catch((err) => {
  console.error('soak failed:', err)
  process.exit(1)
})
