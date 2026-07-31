# ycollab-client

A thin client for [ycollab](https://github.com/mesutokul/ycollab), on top of the real
`y-websocket` provider.

```sh
npm install ycollab-client yjs y-websocket
```

```js
import { connect } from 'ycollab-client'

const client = connect({
  url: 'wss://collab.example.com',
  name: 'notes',
  token: (document) => fetch(`/api/collab-token?doc=${document}`).then((r) => r.text())
})

client.on('denied', ({ reason }) => showBanner(reason))

const text = client.doc.getText('body')   // an ordinary Y.Doc
```

## You may not need this

The server is plain `y-websocket`. This works, and always will:

```js
new WebsocketProvider('wss://collab.example.com', 'notes', new Y.Doc(), { params: { token } })
```

If your tokens do not expire, if you never show the user why a document would not open, and if
you do not use subdocuments — use that and install nothing. This package exists for the three
places that setup leaves something to you, each of which is a real gap rather than a
convenience.

**Tokens expire.** `y-websocket` reconnects forever with the parameters it was constructed
with, so an hour after the page loads the document quietly stops syncing and the reconnect loop
becomes a 1008 loop. Nothing in your UI says so. Pass a *function* as `token` and it is called
again 30 seconds before the current token expires — read out of the JWT's `exp`, without
verifying it, because that is the server's job.

The socket is not dropped to do this. The server authorises at the WebSocket upgrade and never
again, so a token that expires under an open connection costs nothing; what has to be fresh is
the *next* reconnect. The new token is written into `provider.params`, which `y-websocket`
documents as safely updatable and reads on every connect.

**A refusal is invisible.** `y-websocket`'s permission-denied handler is a module-level constant
that calls `console.warn`; it is not an option and cannot be replaced. Your application cannot
find out that the server said no, or why. Here it is a `denied` event carrying the server's own
reason — "your trial ended", "token is not valid for this document" — and it is a good place to
put a banner.

A refusal is also where "your token expired" and "you may not open this document" arrive as the
same 1008. They are separated by asking the token source once more: if a freshly minted token is
refused too, the answer is about the caller rather than the clock, and retrying is noise on
somebody's server. So one retry, then `denied`, then it stops.

**Subdocuments need a provider each.** Yjs opens subdocuments by guid and announces them through
an event; connecting them is left to you, and each one is a separate document to the server —
its own room, its own authorisation decision, its own token. That is why the token function
receives a document name: a single token minted at page load cannot cover documents nobody had
heard of yet.

## API

### `connect(options) => YcollabClient`

| Option | Type | Default | |
|---|---|---|---|
| `url` | `string` | — | `ws://host:port` or `wss://host` |
| `name` | `string` | — | the document to open |
| `token` | `string \| (document) => string \| Promise<string>` | none | a function is refreshed and retried; a string is neither |
| `doc` | `Y.Doc` | a new one | not destroyed by `destroy()` if you passed it in |
| `awareness` | awareness instance | a new one | share one across providers |
| `params` | `Record<string,string>` | `{}` | extra query parameters |
| `subdocs` | `boolean` | `true` | connect subdocuments as Yjs loads them |
| `connect` | `boolean` | `true` | open the socket immediately |
| `disableBc` | `boolean` | `false` | stop same-browser tabs syncing locally |
| `resyncInterval` | `number` | `-1` | re-send the state vector this often, ms |
| `maxBackoffTime` | `number` | `2500` | cap on the reconnect backoff, ms |
| `WebSocketPolyfill` | `typeof WebSocket` | the global one | for environments without one |

### The client

`doc`, `provider`, `awareness`, `synced`, `denied`, `subdocs`, `ready` — and `connect()`,
`disconnect()`, `destroy()`.

`provider` is the real `WebsocketProvider`. The moment this package is in the way of something
`y-websocket` can do, reach through it; that is better than this file growing a passthrough.

### Events

| Event | Payload | |
|---|---|---|
| `status` | `'connecting' \| 'connected' \| 'disconnected'` | the socket |
| `sync` | `boolean` | true once the document has caught up |
| `denied` | `{ document, reason }` | refused for good; retrying will not help |
| `subdoc` | `{ guid, doc, connection }` | a subdocument was connected |
| `error` | `unknown` | a connection error, or a throw from one of your own listeners |

`document` on `denied` is the parent's name or a subdocument's guid, which is how you tell which
one was refused.

### `expiryOf(token) => number | null`

The `exp` claim, in milliseconds, read without verifying anything. `null` for whatever is not a
JWT — an opaque session id is a perfectly good token under the server's `-auth-url` mode and
simply gets no refresh timer.

## Cursors

Nothing here is specific to this package; `client.awareness` is the standard instance every Yjs
editor binding expects.

```js
import { CollaborationCaret } from '@tiptap/extension-collaboration-caret'

CollaborationCaret.configure({ provider: client, user: { name: 'ada', color: '#ff8800' } })
```

## Tests

```sh
npm test
```

They build `cmd/server` and run against real server processes: two clients converging, a
refusal being reported and stopping, an expired token being replaced, a refresh that does not
drop the socket, and a subdocument crossing the server between two clients. There is no mock —
a mock would test this package against an idea of the server, which is the thing already known
to be wrong whenever a test fails.

## Licence

MIT.
