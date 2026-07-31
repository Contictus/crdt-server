/**
 * Fixture scenarios.
 *
 * Every scenario builds one or more Y.Docs with *hardcoded* client IDs (Y.Doc
 * randomises clientID, which would make fixtures non-reproducible) and returns
 * the converged document. The generator serialises the result to binary.
 *
 * `h` is the harness from createHarness(): h.doc() creates a recorded doc,
 * h.sync() exchanges updates, h.updates collects every update in emission order.
 */
import * as Y from 'yjs'

export const createHarness = () => {
  /** @type {Array<{label: string, clientID: number, doc: Y.Doc}>} */
  const docs = []
  /** @type {Array<{label: string, local: boolean, bytes: Uint8Array}>} */
  const updates = []

  /**
   * @param {string} label
   * @param {number} clientID
   * @param {{gc?: boolean}} [opts]
   */
  const doc = (label, clientID, opts = {}) => {
    const d = new Y.Doc({ gc: opts.gc !== false })
    // Deterministic client id. Must be set before any operation.
    d.clientID = clientID
    d.on('update', (bytes, origin) => {
      updates.push({ label, local: origin == null, bytes })
    })
    docs.push({ label, clientID, gc: d.gc, doc: d })
    return d
  }

  /** One-directional: everything `from` knows that `to` does not. */
  const push = (from, to) => {
    Y.applyUpdate(to, Y.encodeStateAsUpdate(from, Y.encodeStateVector(to)), 'remote')
  }

  /** Bidirectional sync. */
  const sync = (a, b) => {
    push(a, b)
    push(b, a)
  }

  return { doc, docs, updates, push, sync }
}

/**
 * Encode a state vector claiming knowledge of `clock` structs from every client
 * currently in `doc`. Used to build partial updates.
 * @param {Y.Doc} doc
 * @param {number} clock
 */
const svAfter = (doc, clock) => {
  const sv = new Map()
  doc.store.clients.forEach((_structs, client) => sv.set(client, clock))
  return Y.encodeStateVector(sv)
}

/**
 * @typedef {Object} Scenario
 * @property {string} name
 * @property {string} notes    What this fixture is meant to exercise.
 * @property {(h: ReturnType<typeof createHarness>) => {doc: Y.Doc, diffSv?: Uint8Array, extra?: Record<string, Uint8Array>}} build
 */

/** @type {Array<Scenario>} */
export const scenarios = [
  {
    name: 'text-insert-single',
    notes: 'Smallest possible update: one client, one ContentString insert (ref 4).',
    build: (h) => {
      const a = h.doc('a', 1001)
      a.getText('text').insert(0, 'Hello')
      return { doc: a }
    }
  },

  {
    name: 'text-concurrent-same-index',
    notes: 'Two clients insert at index 0 with identical (null) origin. YATA tie-break by client id.',
    build: (h) => {
      const a = h.doc('a', 1001)
      const b = h.doc('b', 2002)
      h.sync(a, b)
      a.getText('text').insert(0, 'aaa')
      b.getText('text').insert(0, 'bbb')
      h.sync(a, b)
      return { doc: a, extra: { 'sv-b.bin': Y.encodeStateVector(b) } }
    }
  },

  {
    name: 'text-concurrent-after-shared-origin',
    notes: 'Both clients insert directly after the same existing item: identical origin AND identical rightOrigin.',
    build: (h) => {
      const a = h.doc('a', 1001)
      const b = h.doc('b', 2002)
      a.getText('text').insert(0, 'XY')
      h.sync(a, b)
      a.getText('text').insert(1, 'aaa')
      b.getText('text').insert(1, 'bbb')
      h.sync(a, b)
      return { doc: a }
    }
  },

  {
    name: 'text-delete',
    notes: 'gc disabled, so deleted items stay as ContentDeleted (ref 1) plus a delete set.',
    build: (h) => {
      const a = h.doc('a', 1001, { gc: false })
      const t = a.getText('text')
      t.insert(0, 'Hello World')
      t.delete(5, 6) // ' World'
      t.insert(5, '!')
      return { doc: a }
    }
  },

  {
    name: 'text-three-client-interleaved',
    notes: 'Three clients, partial syncs, out-of-order integration, multi-client struct grouping.',
    build: (h) => {
      const a = h.doc('a', 1001)
      const b = h.doc('b', 2002)
      const c = h.doc('c', 3003)
      a.getText('text').insert(0, 'one')
      h.sync(a, b)
      b.getText('text').insert(3, 'two')
      c.getText('text').insert(0, 'three')
      h.sync(b, c)
      a.getText('text').insert(0, 'zero')
      h.sync(a, b)
      h.sync(b, c)
      h.sync(a, c)
      h.sync(a, b)
      return { doc: a, diffSvFrom: 'b' }
    }
  },

  {
    name: 'map-set-overwrite',
    notes: 'YMap: same-key overwrite by one client, and concurrent same-key set by two clients.',
    build: (h) => {
      const a = h.doc('a', 1001, { gc: false })
      const b = h.doc('b', 2002, { gc: false })
      const ma = a.getMap('map')
      ma.set('k', 'v1')
      ma.set('k', 'v2') // overwrite: deletes the previous item
      ma.set('other', 42)
      h.sync(a, b)
      a.getMap('map').set('conflict', 'from-a')
      b.getMap('map').set('conflict', 'from-b')
      h.sync(a, b)
      return { doc: a }
    }
  },

  {
    name: 'map-nested-type',
    notes: 'ContentType (ref 7) inside a map: nested Y.Text, Y.Map and Y.Array.',
    build: (h) => {
      const a = h.doc('a', 1001)
      const m = a.getMap('map')
      const nestedText = new Y.Text()
      m.set('rich', nestedText)
      nestedText.insert(0, 'nested')
      const nestedMap = new Y.Map()
      m.set('child', nestedMap)
      nestedMap.set('deep', true)
      const arr = new Y.Array()
      m.set('list', arr)
      arr.insert(0, [1, 'two', { three: 3 }])
      return { doc: a }
    }
  },

  {
    name: 'text-format-marks',
    notes: 'ContentFormat (ref 6) and ContentEmbed (ref 5) as produced by rich text editors.',
    build: (h) => {
      const a = h.doc('a', 1001)
      const t = a.getText('text')
      t.insert(0, 'Hello World')
      t.format(0, 5, { bold: true })
      t.format(6, 5, { italic: true, color: '#ff0000' })
      t.insertEmbed(11, { image: 'https://example.com/y.png' }, { alt: 'logo' })
      t.insert(11, ' plain')
      return { doc: a }
    }
  },

  {
    name: 'xml-prosemirror',
    notes: 'y-prosemirror/TipTap document shape: XmlFragment > XmlElement > XmlText with attributes.',
    build: (h) => {
      const a = h.doc('a', 1001)
      const frag = a.getXmlFragment('prosemirror')
      const p1 = new Y.XmlElement('paragraph')
      const t1 = new Y.XmlText()
      p1.insert(0, [t1])
      frag.insert(0, [p1])
      t1.insert(0, 'Hello ')
      t1.insert(6, 'world', { bold: true })
      const heading = new Y.XmlElement('heading')
      heading.setAttribute('level', '2')
      const t2 = new Y.XmlText()
      heading.insert(0, [t2])
      t2.insert(0, 'Title')
      frag.insert(0, [heading])
      return { doc: a }
    }
  },

  {
    name: 'varint-boundaries',
    notes: 'Large client ids, clocks and lengths crossing 7/14/21/28-bit varUint boundaries; every lib0 writeAny type.',
    build: (h) => {
      const a = h.doc('a', 268435456) // 2^28 -> 5-byte varUint client id
      const b = h.doc('b', 268435455) // 2^28-1 -> 4-byte varUint client id
      const t = a.getText('text')
      t.insert(0, 'x'.repeat(20000)) // length and following clocks > 16383
      t.insert(10000, 'MIDDLE') // origin/rightOrigin clocks in the 3-byte range
      h.sync(a, b)
      b.getText('text').insert(20006, 'tail')
      h.sync(a, b)
      const m = a.getMap('any')
      m.set('int-small', 63)
      m.set('int-boundary', 64)
      m.set('int-neg', -12345)
      m.set('int-max32', 2147483647)
      m.set('float32', 1.5)
      m.set('float64', 0.1)
      m.set('bigint', BigInt('9007199254740993'))
      m.set('bool-true', true)
      m.set('bool-false', false)
      m.set('null', null)
      m.set('undefined', undefined)
      m.set('string-long', 'ünïcödé-'.repeat(40))
      m.set('array', [1, -1, 'x', null])
      m.set('object', { a: 1, b: [2, 3], c: { d: 'e' } })
      m.set('binary', new Uint8Array([0, 1, 127, 128, 255]))
      h.sync(a, b)
      return { doc: a, diffSvFrom: 'b' }
    }
  },

  {
    name: 'deleteset-fragmented',
    notes: 'Many disjoint delete ranges per client, across three clients, gc disabled.',
    build: (h) => {
      const a = h.doc('a', 1001, { gc: false })
      const b = h.doc('b', 2002, { gc: false })
      const c = h.doc('c', 3003, { gc: false })
      // each character is its own struct, so deletes produce fragmented ranges
      for (const ch of 'abcdefghij') a.getText('text').insert(a.getText('text').length, ch)
      h.sync(a, b)
      h.sync(b, c)
      for (const ch of 'KLMNOP') b.getText('text').insert(0, ch)
      h.sync(a, b)
      h.sync(b, c)
      for (const ch of 'uvwxyz') c.getText('text').insert(c.getText('text').length, ch)
      h.sync(b, c)
      h.sync(a, b)
      // interleaved deletes from all three replicas
      const del = (doc, idx, len) => doc.getText('text').delete(idx, len)
      del(a, 1, 1)
      del(a, 3, 1)
      del(a, 5, 1)
      h.sync(a, b)
      del(b, 0, 2)
      del(b, 4, 3)
      h.sync(b, c)
      del(c, 2, 1)
      del(c, 6, 2)
      h.sync(a, b)
      h.sync(b, c)
      h.sync(a, c)
      h.sync(a, b)
      return { doc: a }
    }
  },

  {
    name: 'content-doc',
    notes: 'ContentDoc (ref 9). Subdocuments are out of scope as a feature, but the decoder must not choke on one.',
    build: (h) => {
      const a = h.doc('a', 1001)
      const sub = new Y.Doc({ guid: 'fixture-subdoc-0000', shouldLoad: false, autoLoad: false })
      a.getMap('map').set('sub', sub)
      a.getText('text').insert(0, 'host')
      return { doc: a }
    }
  },

  {
    name: 'gc-and-skip',
    notes: 'gc enabled: deleted subtrees collapse into GC structs (ref 0). Also emits merged-with-skip.bin containing a Skip struct (ref 10).',
    build: (h) => {
      const a = h.doc('a', 1001)
      const m = a.getMap('map')
      const nested = new Y.Map()
      m.set('doomed', nested)
      nested.set('x', 'y')
      const t = a.getText('text')
      t.insert(0, 'aaaaa') // clock block 1
      const afterFirst = Y.encodeStateAsUpdate(a)
      t.insert(5, 'bbbbb') // clock block 2 (the gap that becomes a Skip)
      t.insert(10, 'ccccc') // clock block 3
      const full = Y.encodeStateAsUpdate(a)
      // update covering only the third block
      const thirdOnly = Y.diffUpdate(full, svAfter(a, 10))
      m.delete('doomed') // collapses the nested type into a GC struct
      t.delete(0, 5)
      return {
        doc: a,
        extra: {
          'merged-with-skip.bin': Y.mergeUpdates([afterFirst, thirdOnly])
        }
      }
    }
  },

  {
    name: 'subdocument',
    notes: 'ContentDoc (ref 9): a parent document embedding two subdocuments, one of them removed. The guids are fixed so the Go side can assert on them; Yjs would otherwise generate a uuid per run.',
    build: (h) => {
      const a = h.doc('a', 1001)
      const outline = a.getMap('outline')
      // Y.Doc takes a guid, so the fixture is reproducible. This is what a real
      // client does when it names a subdocument itself rather than letting Yjs
      // invent one.
      const chapter = new Y.Doc({ guid: 'chapter-one' })
      const appendix = new Y.Doc({ guid: 'appendix' })
      outline.set('chapter', chapter)
      outline.set('appendix', appendix)
      a.getText('title').insert(0, 'A book')
      // One is removed again, so the Go side has to tell a live subdocument
      // reference from a deleted one.
      outline.delete('appendix')
      return { doc: a }
    }
  },
]
