/**
 * A thin client for ycollab, on top of the real y-websocket provider.
 *
 * The server is deliberately plain y-websocket, so this package is optional:
 * `new WebsocketProvider(url, name, doc)` works and always will. What it is for
 * is the three things that setup leaves to every application that uses it, each
 * of which is a real gap rather than a convenience:
 *
 *   1. Tokens expire. y-websocket reconnects forever with the parameters it was
 *      given, so an hour after the page loads the document quietly stops
 *      syncing and the reconnect loop turns into a 1008 loop. Nothing in the UI
 *      says so.
 *   2. A refusal is invisible. y-websocket's permission-denied handler is a
 *      module-level constant that calls console.warn (y-websocket.js:105); it
 *      is not an option and cannot be replaced. An application cannot find out
 *      that the server said no, let alone why.
 *   3. Subdocuments need a provider each. Yjs opens subdocuments by guid and
 *      tells the parent through a 'subdocs' event; connecting them is left to
 *      the caller, and each one needs its own token because each one is a
 *      separate document as far as the server's authorisation is concerned.
 *
 * Everything else is delegated. The Y.Doc, the awareness instance and the
 * provider are all exposed, because the moment this package is in the way of
 * something y-websocket can do, the answer is to use the provider directly and
 * not for this file to grow a passthrough.
 */
import { WebsocketProvider } from 'y-websocket'
import * as Y from 'yjs'

/** The close code the server uses to refuse a connection (RFC 6455 policy violation). */
const POLICY_VIOLATION = 1008

/** Refresh a token this long before it expires, so a reconnect never races the clock. */
const REFRESH_MARGIN_MS = 30_000

/** Never schedule a refresh sooner than this, so a pathological `exp` cannot spin. */
const MIN_REFRESH_MS = 1_000

/**
 * A minimal event emitter.
 *
 * yjs ships lib0's Observable and using it would be free, but this package's
 * only hard requirement on its peers is that they exist - reaching into
 * `lib0/observable` would pin a deep import path that is not part of yjs's
 * public surface. Twenty lines is cheaper than that coupling.
 */
class Emitter {
  #handlers = new Map()

  on (event, fn) {
    if (!this.#handlers.has(event)) this.#handlers.set(event, new Set())
    this.#handlers.get(event).add(fn)
    return this
  }

  off (event, fn) {
    this.#handlers.get(event)?.delete(fn)
    return this
  }

  once (event, fn) {
    const wrapped = (...args) => { this.off(event, wrapped); fn(...args) }
    return this.on(event, wrapped)
  }

  emit (event, payload) {
    // A throwing listener must not take down the caller, which is usually a
    // WebSocket event handler: losing the socket because a UI callback failed
    // would turn a rendering bug into a sync outage.
    for (const fn of this.#handlers.get(event) ?? []) {
      try {
        fn(payload)
      } catch (err) {
        if (event === 'error') return
        this.emit('error', err)
      }
    }
  }

  removeAllListeners () {
    this.#handlers.clear()
  }
}

/**
 * Reads the `exp` claim out of a JWT without verifying it.
 *
 * Verification is the server's job and this side holds no key, so the only
 * question here is when to ask for the next token. A token this cannot parse -
 * an opaque session id under -auth-url, an empty string - simply gets no
 * refresh timer, which is correct: there is nothing to schedule against.
 *
 * @param {string} token
 * @returns {number | null} milliseconds since the epoch, or null
 */
export function expiryOf (token) {
  if (typeof token !== 'string') return null
  const parts = token.split('.')
  if (parts.length !== 3) return null
  try {
    const payload = parts[1].replace(/-/g, '+').replace(/_/g, '/')
    const json = typeof atob === 'function'
      ? atob(payload)
      : Buffer.from(payload, 'base64').toString('binary')
    const exp = JSON.parse(json).exp
    return typeof exp === 'number' && Number.isFinite(exp) ? exp * 1000 : null
  } catch {
    return null
  }
}

/**
 * One document's connection: a Y.Doc, its provider, and the token handling
 * around them. Subdocuments get one of these each.
 */
class Connection extends Emitter {
  /**
   * @param {string} name the document name, which is also what a token is minted for
   * @param {Y.Doc} doc
   * @param {object} config normalised options from {@link connect}
   */
  constructor (name, doc, config) {
    super()
    this.name = name
    this.doc = doc
    this.config = config
    this.provider = null
    this.destroyed = false
    this.denied = null
    this.subdocs = new Map()
    /** Set while recovering from a 1008, so a second one is treated as final. */
    this.retried = false
    this.refreshTimer = null
  }

  get awareness () {
    return this.provider?.awareness ?? null
  }

  get synced () {
    return this.provider?.synced ?? false
  }

  async start () {
    const token = await this.#token()
    if (this.destroyed) return
    const params = { ...this.config.params }
    if (token) params.token = token

    this.provider = new WebsocketProvider(this.config.url, this.name, this.doc, {
      params,
      connect: this.config.connect,
      awareness: this.config.awareness,
      WebSocketPolyfill: this.config.WebSocketPolyfill,
      // Two tabs of one browser sync through BroadcastChannel by default, which
      // is faster and entirely local. It is left on: turning it off is a demo
      // concern (proving the server works), not an application one.
      disableBc: this.config.disableBc,
      resyncInterval: this.config.resyncInterval,
      maxBackoffTime: this.config.maxBackoffTime
    })

    this.provider.on('status', (event) => this.emit('status', event.status))
    this.provider.on('sync', (state) => {
      // A completed sync is proof the credential was accepted, so the next
      // refusal is a new fact and not an echo of this connection's history.
      if (state) this.retried = false
      this.emit('sync', state)
    })
    this.provider.on('connection-error', (event) => this.emit('error', event))
    this.provider.on('connection-close', (event) => this.#onClose(event))

    this.#scheduleRefresh(token)
    if (this.config.subdocs) this.#watchSubdocs()
  }

  /**
   * A refused connection.
   *
   * y-websocket cannot tell "your token expired" from "you may not open this
   * document": both arrive as a 1008 and both are retried forever with the same
   * dead credential. One retry with a freshly fetched token separates them - if
   * the second attempt is refused too, the answer is about the caller and not
   * about the clock, and retrying is just noise on somebody's server.
   *
   * The reason comes from the close frame rather than from the
   * permission-denied message, because y-websocket routes that message to a
   * console.warn this package has no way to intercept. The server puts the same
   * text in both.
   */
  #onClose (event) {
    if (this.destroyed || event?.code !== POLICY_VIOLATION) return
    const reason = event.reason || 'the server refused the connection'

    // Stop the reconnect that y-websocket has already scheduled, before doing
    // anything asynchronous: otherwise the backoff fires with the old token
    // while the new one is still being fetched.
    this.provider.disconnect()

    if (this.retried || !this.config.hasTokenSource) {
      this.denied = reason
      this.emit('denied', { document: this.name, reason })
      return
    }
    this.retried = true
    this.#refresh()
      .then((token) => {
        if (this.destroyed) return
        if (!token) {
          this.denied = reason
          this.emit('denied', { document: this.name, reason })
          return
        }
        this.provider.connect()
      })
      .catch((err) => this.emit('error', err))
  }

  /**
   * Fetches a token and puts it where the next connection will pick it up.
   *
   * The socket is not touched. The server authorises at the upgrade and never
   * again, so a token that expires under an open connection costs nothing;
   * what matters is that the *next* reconnect carries a live one. y-websocket
   * documents `params` as safely updatable and recomputes the URL on every
   * connect, so writing to it is the supported way to say this.
   */
  async #refresh () {
    const token = await this.#token()
    if (this.destroyed || !token || !this.provider) return token
    this.provider.params = { ...this.provider.params, token }
    this.#scheduleRefresh(token)
    return token
  }

  #scheduleRefresh (token) {
    clearTimeout(this.refreshTimer)
    this.refreshTimer = null
    if (!this.config.hasTokenSource || typeof this.config.token !== 'function') return
    const expiry = expiryOf(token)
    if (expiry === null) return
    const delay = Math.max(expiry - Date.now() - REFRESH_MARGIN_MS, MIN_REFRESH_MS)
    this.refreshTimer = setTimeout(() => {
      this.#refresh().catch((err) => this.emit('error', err))
    }, delay)
    // Node keeps the process alive for a pending timer, which would stop a
    // script from exiting for as long as a token happens to be valid.
    this.refreshTimer?.unref?.()
  }

  async #token () {
    const source = this.config.token
    if (!source) return null
    return typeof source === 'function' ? await source(this.name) : source
  }

  /**
   * Connects subdocuments as Yjs loads them.
   *
   * A subdocument is a separate document to the server - its own room, its own
   * row, its own authorisation decision - and its name is its guid, which the
   * application does not know until Yjs hands it over. That is why the token
   * source takes a document name: minting one token at page load cannot cover
   * documents nobody has heard of yet.
   */
  #watchSubdocs () {
    this.onSubdocs = ({ loaded, removed }) => {
      for (const sub of loaded) this.#openSubdoc(sub)
      for (const sub of removed) this.#closeSubdoc(sub.guid)
    }
    this.doc.on('subdocs', this.onSubdocs)
    // Subdocuments already loaded before this ran - a doc restored from an
    // update, or one the caller loaded eagerly - never fire the event.
    for (const sub of this.doc.getSubdocs()) {
      if (sub.shouldLoad) this.#openSubdoc(sub)
    }
  }

  #openSubdoc (sub) {
    if (this.subdocs.has(sub.guid) || this.destroyed) return
    const child = new Connection(sub.guid, sub, this.config)
    this.subdocs.set(sub.guid, child)
    // A subdocument's problems are the caller's problems, so they surface on
    // the client rather than on an object the caller never sees. The document
    // name distinguishes them, which is why 'denied' carries one.
    child.on('denied', (info) => this.emit('denied', info))
    child.on('error', (err) => this.emit('error', err))
    child.start()
      .then(() => this.emit('subdoc', { guid: sub.guid, doc: sub, connection: child }))
      .catch((err) => this.emit('error', err))
  }

  #closeSubdoc (guid) {
    this.subdocs.get(guid)?.destroy()
    this.subdocs.delete(guid)
  }

  disconnect () {
    this.provider?.disconnect()
    for (const child of this.subdocs.values()) child.disconnect()
  }

  connect () {
    this.retried = false
    this.denied = null
    this.provider?.connect()
    for (const child of this.subdocs.values()) child.connect()
  }

  /**
   * Tears everything down. The Y.Doc is left alone unless this package created
   * it: destroying a document the caller owns would take its data with it.
   */
  destroy () {
    if (this.destroyed) return
    this.destroyed = true
    clearTimeout(this.refreshTimer)
    if (this.onSubdocs) this.doc.off('subdocs', this.onSubdocs)
    for (const child of this.subdocs.values()) child.destroy()
    this.subdocs.clear()
    this.provider?.destroy()
    this.removeAllListeners()
  }
}

/**
 * The object {@link connect} returns.
 *
 * @extends {Emitter}
 */
export class YcollabClient extends Emitter {
  constructor (connection, ownsDoc) {
    super()
    this.connection = connection
    this.ownsDoc = ownsDoc
    for (const event of ['status', 'sync', 'denied', 'subdoc', 'error']) {
      connection.on(event, (payload) => this.emit(event, payload))
    }
    this.ready = connection.start()
    // An unobserved rejection here would be an unhandled promise rejection,
    // which in Node is a process exit. Callers who want it can await `ready`.
    this.ready.catch(() => {})
  }

  /** The document. */
  get doc () { return this.connection.doc }
  /** The underlying y-websocket provider, or null until the first token resolves. */
  get provider () { return this.connection.provider }
  /** The awareness instance, for cursors and presence. */
  get awareness () { return this.connection.awareness }
  /** Whether the initial sync has completed. */
  get synced () { return this.connection.synced }
  /** The refusal reason, once the server has refused for good. */
  get denied () { return this.connection.denied }
  /** Connected subdocuments, by guid. */
  get subdocs () { return this.connection.subdocs }

  disconnect () { this.connection.disconnect() }
  connect () { this.connection.connect() }

  destroy () {
    const doc = this.connection.doc
    this.connection.destroy()
    this.removeAllListeners()
    if (this.ownsDoc) doc.destroy()
  }
}

/**
 * Opens a document on a ycollab server.
 *
 * ```js
 * const client = connect({
 *   url: 'wss://collab.example.com',
 *   name: 'notes',
 *   token: (document) => fetch(`/api/collab-token?doc=${document}`).then((r) => r.text())
 * })
 * client.on('denied', ({ reason }) => showBanner(reason))
 * const text = client.doc.getText('body')
 * ```
 *
 * @param {object} options
 * @param {string} options.url the server, `ws://host:port` or `wss://host`
 * @param {string} options.name the document to open
 * @param {string | ((document: string) => string | Promise<string>)} [options.token]
 *   a token, or - preferably - a function that returns one. A function is
 *   called again before the token expires and after a refusal, and is the only
 *   form that works with subdocuments, which are named at runtime.
 * @param {Y.Doc} [options.doc] a document to use instead of a fresh one
 * @param {object} [options.awareness] an awareness instance to share
 * @param {Object<string,string>} [options.params] extra query parameters
 * @param {boolean} [options.subdocs=true] connect subdocuments as Yjs loads them
 * @param {boolean} [options.connect=true] open the socket immediately
 * @param {boolean} [options.disableBc=false] do not sync same-browser tabs locally
 * @param {number} [options.resyncInterval=-1] re-send the state vector this often, ms
 * @param {number} [options.maxBackoffTime=2500] cap on the reconnect backoff, ms
 * @param {typeof WebSocket} [options.WebSocketPolyfill] for environments without one
 * @returns {YcollabClient}
 */
export function connect (options) {
  if (!options?.url) throw new TypeError('ycollab: url is required')
  if (!options.name) throw new TypeError('ycollab: name is required')
  if (options.token !== undefined &&
      typeof options.token !== 'string' &&
      typeof options.token !== 'function') {
    throw new TypeError('ycollab: token must be a string or a function returning one')
  }

  const ownsDoc = !options.doc
  const doc = options.doc ?? new Y.Doc()
  const config = {
    url: options.url.replace(/\/+$/, ''),
    token: options.token,
    // A token of '' or a function that returns nothing is still a token
    // *source*: the difference decides whether a 1008 is worth one retry.
    hasTokenSource: options.token !== undefined,
    params: options.params ?? {},
    awareness: options.awareness,
    subdocs: options.subdocs ?? true,
    connect: options.connect ?? true,
    disableBc: options.disableBc ?? false,
    resyncInterval: options.resyncInterval ?? -1,
    maxBackoffTime: options.maxBackoffTime ?? 2500,
    WebSocketPolyfill: options.WebSocketPolyfill
  }
  return new YcollabClient(new Connection(options.name, doc, config), ownsDoc)
}

export { Y }
