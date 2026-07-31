/**
 * The client against a real server process.
 *
 * Each test is about one of the three things this package exists for - tokens
 * that expire, refusals nothing surfaces, and subdocuments nothing connects -
 * plus one that checks the ordinary path still works, because a wrapper whose
 * only tested behaviour is its extras is a wrapper nobody should install.
 */
import { test, before, after } from 'node:test'
import assert from 'node:assert/strict'
import * as Y from 'yjs'

import { connect, expiryOf } from '../index.js'
import { startServer, mintToken, secret, waitFor, buildServer } from './server.mjs'

const servers = []
const clients = []

const serve = async (args) => {
  const s = await startServer(args)
  servers.push(s)
  return s
}
const open = (options) => {
  const c = connect(options)
  clients.push(c)
  return c
}

before(() => { buildServer() })
after(() => {
  for (const c of clients) c.destroy()
  for (const s of servers) s.stop()
})

test('two clients converge', async () => {
  const server = await serve()
  const a = open({ url: server.url, name: 'converge' })
  const b = open({ url: server.url, name: 'converge' })

  await waitFor(() => a.synced && b.synced, 'both clients to sync')
  a.doc.getText('body').insert(0, 'from a. ')
  b.doc.getText('body').insert(0, 'from b. ')

  await waitFor(
    () => a.doc.getText('body').toString() === b.doc.getText('body').toString(),
    'the two documents to converge')
  assert.match(a.doc.getText('body').toString(), /from a\./)
  assert.match(a.doc.getText('body').toString(), /from b\./)
})

test('a refusal is reported instead of retried forever', async () => {
  const key = secret()
  const server = await serve(['-jwt-secret', key])

  // A perfectly valid token - for a document this client is not opening. The
  // server refuses with 1008 and a reason; y-websocket on its own would
  // reconnect into that refusal until the tab closes.
  const wrongDoc = mintToken(key, { doc: 'somewhere-else' })
  const denials = []
  const client = open({
    url: server.url,
    name: 'guarded',
    token: () => wrongDoc
  })
  client.on('denied', (info) => denials.push(info))

  await waitFor(() => denials.length > 0, 'a denied event')
  assert.equal(denials[0].document, 'guarded')
  assert.ok(denials[0].reason.length > 0, 'the refusal carried no reason')

  // And it stopped. y-websocket only reconnects while shouldConnect is set.
  assert.equal(client.provider.shouldConnect, false)
  const seen = denials.length
  await new Promise((r) => setTimeout(r, 1500))
  assert.equal(denials.length, seen, 'it kept retrying after giving up')
  assert.equal(client.denied, denials[0].reason)
})

test('an expired token is replaced rather than retried', async () => {
  const key = secret()
  // No leeway, so a token that expired a minute ago is actually expired.
  const server = await serve(['-jwt-secret', key, '-jwt-leeway', '0'])

  let asked = 0
  const client = open({
    url: server.url,
    name: 'stale',
    token: (doc) => {
      asked += 1
      // The first attempt carries a token that expired a minute ago, which is
      // exactly what a tab left open overnight sends on its next reconnect.
      return asked === 1
        ? mintToken(key, { doc, ttlSeconds: -60 })
        : mintToken(key, { doc, ttlSeconds: 3600 })
    }
  })
  const denials = []
  client.on('denied', (info) => denials.push(info))

  await waitFor(() => client.synced, 'the client to recover and sync')
  assert.equal(asked, 2, 'the token source was not asked again after the refusal')
  assert.deepEqual(denials, [], 'a recoverable refusal was reported as final')
})

test('a token is refreshed before it expires, without dropping the socket', async () => {
  const key = secret()
  const server = await serve(['-jwt-secret', key])

  const issued = []
  const client = open({
    url: server.url,
    name: 'refreshed',
    // Just past the 30 s refresh margin, so the timer fires about two seconds
    // from now instead of an hour from now.
    token: (doc) => {
      const t = mintToken(key, { doc, ttlSeconds: 32 })
      issued.push(t)
      return t
    }
  })

  await waitFor(() => client.synced, 'the client to sync')
  const socket = client.provider.ws
  assert.equal(issued.length, 1)

  await waitFor(() => issued.length > 1, 'the token to be refreshed', 20_000)
  assert.equal(client.provider.params.token, issued.at(-1),
    'the fresh token was not put where the next connection reads it')
  // The server authorises at the upgrade and never again, so refreshing must
  // not cost a reconnect: the same socket is still open and still synced.
  assert.equal(client.provider.ws, socket, 'refreshing the token dropped the connection')
  assert.equal(client.synced, true)
})

test('subdocuments are connected, each with its own token', async () => {
  const key = secret()
  const server = await serve(['-jwt-secret', key])

  const asked = []
  const token = (doc) => { asked.push(doc); return mintToken(key, { doc }) }

  const a = open({ url: server.url, name: 'book', token })
  await waitFor(() => a.synced, 'the parent to sync')

  const child = new Y.Doc({ guid: 'chapter-one' })
  a.doc.getMap('chapters').set('one', child)
  child.load()

  await waitFor(() => a.subdocs.has('chapter-one'), 'the subdocument to be connected')
  assert.ok(asked.includes('chapter-one'),
    `the token source was never asked about the subdocument: ${asked.join(', ')}`)

  // A second client, which learns about the subdocument the way any client
  // does: from the parent document it just synced.
  const b = open({ url: server.url, name: 'book', token })
  await waitFor(() => b.synced, 'the second parent to sync')
  const mirror = b.doc.getMap('chapters').get('one')
  assert.ok(mirror instanceof Y.Doc, 'the subdocument did not arrive as a document')
  mirror.load()

  await waitFor(() => b.subdocs.has('chapter-one'), 'the second subdocument to connect')
  child.getText('text').insert(0, 'It was a dark and stormy night.')
  await waitFor(
    () => mirror.getText('text').toString() === 'It was a dark and stormy night.',
    'the subdocument contents to cross the server')
})

test('expiryOf reads a JWT and shrugs at anything else', () => {
  const at = Math.floor(Date.now() / 1000) + 600
  const token = mintToken('irrelevant', { doc: 'x', ttlSeconds: 600 })
  assert.ok(Math.abs(expiryOf(token) - at * 1000) < 2000)

  // An opaque session id under -auth-url is a perfectly good token, and the
  // right answer is "no refresh timer" rather than a thrown error.
  for (const bad of ['', 'session=abc', 'a.b', 'a.b.c', 'a.!!!.c', null, undefined, 42]) {
    assert.equal(expiryOf(bad), null, `expiryOf(${JSON.stringify(bad)})`)
  }
})
