/**
 * Starts a real ycollab server for the client tests.
 *
 * A mock would test this package against my idea of the server, which is the
 * one thing already known to be wrong when a test fails. The binary is built
 * once per run and each test gets its own process on its own port.
 */
import { spawn, execFileSync } from 'node:child_process'
import { createHmac, randomBytes } from 'node:crypto'
import { createServer } from 'node:net'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'

const repo = join(dirname(fileURLToPath(import.meta.url)), '..', '..')
let binary = null

/** Builds cmd/server once, and returns the path to it. */
export function buildServer () {
  if (binary) return binary
  const out = join(mkdtempSync(join(tmpdir(), 'ycollab-client-')),
    process.platform === 'win32' ? 'server.exe' : 'server')
  execFileSync('go', ['build', '-o', out, './cmd/server'], { cwd: repo, stdio: 'inherit' })
  binary = out
  return binary
}

/** An unused TCP port. Racy in principle, and the server would fail loudly. */
export function freePort () {
  return new Promise((resolve, reject) => {
    const probe = createServer()
    probe.on('error', reject)
    probe.listen(0, '127.0.0.1', () => {
      const { port } = probe.address()
      probe.close(() => resolve(port))
    })
  })
}

/**
 * Starts a server and waits for it to say it is listening.
 *
 * @param {string[]} args extra flags
 * @returns {Promise<{url: string, logs: () => string, stop: () => void}>}
 */
export async function startServer (args = []) {
  const port = await freePort()
  const child = spawn(buildServer(), ['-addr', `127.0.0.1:${port}`, '-admin-addr', '', ...args],
    { cwd: repo, stdio: ['ignore', 'pipe', 'pipe'] })

  let output = ''
  const collect = (chunk) => { output += chunk }
  child.stdout.on('data', collect)
  child.stderr.on('data', collect)

  const exited = new Promise((_, reject) => {
    child.once('exit', (code) => reject(new Error(`the server exited with ${code}:\n${output}`)))
  })
  const listening = new Promise((resolve, reject) => {
    const deadline = setTimeout(() => reject(new Error(`the server never listened:\n${output}`)), 20_000)
    const check = () => {
      if (/serving|listening/i.test(output)) { clearTimeout(deadline); resolve() }
    }
    child.stdout.on('data', check)
    child.stderr.on('data', check)
    check()
  })
  await Promise.race([listening, exited])
  exited.catch(() => {}) // the stop() below is an expected exit

  return {
    url: `ws://127.0.0.1:${port}`,
    logs: () => output,
    stop: () => child.kill()
  }
}

/** A random HS256 secret, in the form -jwt-secret wants. */
export function secret () {
  return randomBytes(32).toString('hex')
}

/**
 * Mints the token the server expects: HS256 over { doc, perm, exp }.
 *
 * Written out rather than pulled from a JWT library so the test depends on the
 * same thing a real integrator does - the documented claim shape - instead of
 * on a library agreeing with itself.
 */
export function mintToken (key, { doc, perm = 'write', ttlSeconds = 3600 }) {
  const b64 = (obj) => Buffer.from(JSON.stringify(obj)).toString('base64url')
  const body = `${b64({ alg: 'HS256', typ: 'JWT' })}.${b64({
    doc,
    perm,
    exp: Math.floor(Date.now() / 1000) + ttlSeconds
  })}`
  const sig = createHmac('sha256', key).update(body).digest('base64url')
  return `${body}.${sig}`
}

/** Resolves when `predicate` holds, or rejects after `timeout` ms. */
export async function waitFor (predicate, message, timeout = 15_000) {
  const deadline = Date.now() + timeout
  for (;;) {
    if (await predicate()) return
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${message}`)
    await new Promise((r) => setTimeout(r, 25))
  }
}
