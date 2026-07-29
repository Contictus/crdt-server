/**
 * The Phase 2 acceptance criterion, made clickable: TipTap in two tabs, live
 * cursors, and a running readout of the document's state vector so divergence
 * would be visible rather than merely felt.
 *
 * Nothing here is ycollab-specific. It is the standard y-websocket setup, which
 * is the point: the server has to work with clients that know nothing about it.
 */
import { Editor } from '@tiptap/core'
import { StarterKit } from '@tiptap/starter-kit'
import { Collaboration } from '@tiptap/extension-collaboration'
import { CollaborationCaret } from '@tiptap/extension-collaboration-caret'
import * as Y from 'yjs'
import { WebsocketProvider } from 'y-websocket'

const serverUrl = import.meta.env.VITE_YCOLLAB_URL ?? 'ws://127.0.0.1:8080'
const room = location.hash.replace(/^#/, '') || 'demo'

const names = ['ada', 'grace', 'alan', 'edsger', 'barbara', 'donald', 'ken', 'rob']
const colors = ['#ff8800', '#0088ff', '#22aa55', '#cc3366', '#8855dd', '#00a2a2']
const pick = (xs) => xs[Math.floor(Math.random() * xs.length)]
const user = { name: pick(names), color: pick(colors) }

const doc = new Y.Doc()
const provider = new WebsocketProvider(serverUrl, room, doc, {
  // Two tabs of the same browser would otherwise sync directly through
  // BroadcastChannel, which would demo the browser rather than the server.
  disableBc: true
})

const editor = new Editor({
  element: document.querySelector('#editor'),
  extensions: [
    // Collaboration brings its own undo stack; the built-in one would fight it.
    StarterKit.configure({ undoRedo: false }),
    Collaboration.configure({ document: doc }),
    CollaborationCaret.configure({ provider, user })
  ],
  autofocus: true
})

const el = (id) => document.getElementById(id)
el('room').textContent = room

provider.on('status', ({ status }) => {
  el('connection').textContent = status
  el('connection').dataset.state = status
})
provider.on('sync', (synced) => {
  if (synced) el('connection').textContent = 'synced'
})

const refresh = () => {
  el('peers').textContent = String(provider.awareness.getStates().size)
  el('sv').textContent = Array.from(Y.encodeStateVector(doc))
    .map((b) => b.toString(16).padStart(2, '0'))
    .join('')
}
provider.awareness.on('change', refresh)
doc.on('update', refresh)
refresh()

// Handy when poking at the server from the browser console.
Object.assign(window, { doc, provider, editor, Y })
