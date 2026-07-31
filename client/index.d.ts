import * as Y from 'yjs'
import type { WebsocketProvider } from 'y-websocket'

/** A token, or a function asked for one per document and again before it expires. */
export type TokenSource =
  | string
  | ((document: string) => string | Promise<string>)

export interface ConnectOptions {
  /** The server: `ws://host:port` or `wss://host`. */
  url: string
  /** The document to open. */
  name: string
  /**
   * A token, or - preferably - a function that returns one. A function is
   * called again before the token expires and once after a refusal, and is the
   * only form that works with subdocuments, whose names appear at runtime.
   */
  token?: TokenSource
  /** A document to use instead of a fresh one. It is not destroyed on `destroy()`. */
  doc?: Y.Doc
  /** An awareness instance to share, for presence across several providers. */
  awareness?: any
  /** Extra query parameters on the WebSocket URL. */
  params?: Record<string, string>
  /** Connect subdocuments as Yjs loads them. Default true. */
  subdocs?: boolean
  /** Open the socket immediately. Default true. */
  connect?: boolean
  /** Do not sync same-browser tabs through BroadcastChannel. Default false. */
  disableBc?: boolean
  /** Re-send the state vector this often, in ms. Default -1 (never). */
  resyncInterval?: number
  /** Cap on the reconnect backoff, in ms. Default 2500. */
  maxBackoffTime?: number
  /** A WebSocket implementation, for environments without a global one. */
  WebSocketPolyfill?: typeof WebSocket
}

/** The server refused a document, for good: retrying will not help. */
export interface DeniedEvent {
  /** The document that was refused - the parent's name, or a subdocument's guid. */
  document: string
  /** What the server said, from the close frame. */
  reason: string
}

export interface SubdocEvent {
  guid: string
  doc: Y.Doc
  connection: unknown
}

export interface ClientEvents {
  status: 'connected' | 'disconnected' | 'connecting'
  sync: boolean
  denied: DeniedEvent
  subdoc: SubdocEvent
  error: unknown
}

export declare class YcollabClient {
  /** Resolves once the first token has been fetched and the provider exists. */
  readonly ready: Promise<void>
  /** The document. */
  readonly doc: Y.Doc
  /** The underlying y-websocket provider, or null until the first token resolves. */
  readonly provider: WebsocketProvider | null
  /** The awareness instance, for cursors and presence. */
  readonly awareness: any
  /** Whether the initial sync has completed. */
  readonly synced: boolean
  /** The refusal reason, once the server has refused for good; otherwise null. */
  readonly denied: string | null
  /** Connected subdocuments, by guid. */
  readonly subdocs: Map<string, unknown>

  on<K extends keyof ClientEvents>(event: K, fn: (payload: ClientEvents[K]) => void): this
  off<K extends keyof ClientEvents>(event: K, fn: (payload: ClientEvents[K]) => void): this
  once<K extends keyof ClientEvents>(event: K, fn: (payload: ClientEvents[K]) => void): this

  connect(): void
  disconnect(): void
  /** Tears everything down. The Y.Doc is destroyed only if this package created it. */
  destroy(): void
}

/** Opens a document on a ycollab server. */
export declare function connect(options: ConnectOptions): YcollabClient

/**
 * Reads the `exp` claim out of a JWT without verifying it, in milliseconds
 * since the epoch. Returns null for anything that is not a JWT - an opaque
 * session id is a perfectly good token and simply gets no refresh timer.
 */
export declare function expiryOf(token: string): number | null

export { Y }
