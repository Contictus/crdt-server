/**
 * JSON serialisation of Yjs state and of decoded update bytes.
 *
 * Shared by the fixture generator and by tools/verify, so that "what Yjs thinks
 * the document is" is computed by exactly one piece of code.
 */
import * as Y from 'yjs'

// Re-exported so that tools/verify uses the exact same yjs installation as the
// generator: there is only one node_modules in this repo.
export { Y }

/** Values Yjs can hold that JSON.stringify cannot represent losslessly. */
export const jsonSafe = (v) => {
  if (v === undefined) return { $undefined: true }
  if (v === null) return null
  if (typeof v === 'bigint') return { $bigint: v.toString() }
  if (v instanceof Uint8Array) return { $bytes: Array.from(v) }
  if (Array.isArray(v)) return v.map(jsonSafe)
  if (v instanceof Y.Doc) return { $subdoc: v.guid }
  if (v instanceof Y.AbstractType) return jsonSafe(v.toJSON())
  if (typeof v === 'number' && !Number.isFinite(v)) return { $number: String(v) }
  if (typeof v === 'object') {
    const out = {}
    for (const k of Object.keys(v).sort()) out[k] = jsonSafe(v[k])
    return out
  }
  return v
}

const idJSON = (id) => (id == null ? null : { client: id.client, clock: id.clock })

/** Numeric-key map to an object with ascending, stringified keys. */
const mapJSON = (m, valueFn = (v) => v) => {
  const out = {}
  for (const k of Array.from(m.keys()).sort((a, b) => a - b)) out[String(k)] = valueFn(m.get(k))
  return out
}

/** Next expected clock per client, i.e. the document's state vector. */
const svMap = (doc) => {
  const sv = new Map()
  doc.store.clients.forEach((structs, client) => {
    const last = structs[structs.length - 1]
    sv.set(client, last.id.clock + last.length)
  })
  return sv
}

/**
 * Delete set of a document, mirroring createDeleteSetFromStructStore
 * (yjs/src/utils/DeleteSet.js) which is not part of the public API.
 */
export const deleteSetJSON = (doc) => {
  const out = {}
  const clients = Array.from(doc.store.clients.keys()).sort((a, b) => a - b)
  for (const client of clients) {
    const structs = doc.store.clients.get(client)
    const ranges = []
    for (let i = 0; i < structs.length; i++) {
      const struct = structs[i]
      if (!struct.deleted) continue
      const clock = struct.id.clock
      let len = struct.length
      while (i + 1 < structs.length && structs[i + 1].deleted) {
        len += structs[++i].length
      }
      ranges.push([clock, len])
    }
    if (ranges.length > 0) out[String(client)] = ranges
  }
  return out
}

const typeJSON = (name, type) => {
  if (type instanceof Y.XmlElement || type instanceof Y.XmlFragment) {
    return { kind: 'xml', xml: type.toString() }
  }
  if (type instanceof Y.XmlText) return { kind: 'xmltext', string: type.toString(), delta: jsonSafe(type.toDelta()) }
  if (type instanceof Y.Text) return { kind: 'text', string: type.toString(), delta: jsonSafe(type.toDelta()) }
  if (type instanceof Y.Map) return { kind: 'map', json: jsonSafe(type.toJSON()) }
  if (type instanceof Y.Array) return { kind: 'array', json: jsonSafe(type.toJSON()) }
  return { kind: 'unknown', json: jsonSafe(type.toJSON()) }
}

/** Full observable state of a document: what the Go implementation must reproduce. */
export const dumpDoc = (doc) => {
  const types = {}
  for (const name of Array.from(doc.share.keys()).sort()) {
    types[name] = typeJSON(name, doc.share.get(name))
  }
  let structCount = 0
  doc.store.clients.forEach((structs) => { structCount += structs.length })
  return {
    types,
    stateVector: mapJSON(svMap(doc)),
    deleteSet: deleteSetJSON(doc),
    structCount
  }
}

const contentJSON = (content) => {
  const ref = content.getRef()
  const base = { ref, kind: content.constructor.name }
  switch (ref) {
    case 1: return { ...base, len: content.len }
    case 2: return { ...base, values: jsonSafe(content.arr) }
    case 3: return { ...base, bytes: Array.from(content.content) }
    case 4: return { ...base, str: content.str }
    case 5: return { ...base, embed: jsonSafe(content.embed) }
    case 6: return { ...base, key: content.key, value: jsonSafe(content.value) }
    case 7: {
      const t = content.type
      const typeRef = t instanceof Y.XmlText
        ? 6
        : t instanceof Y.XmlHook
          ? 5
          : t instanceof Y.XmlFragment && !(t instanceof Y.XmlElement)
            ? 4
            : t instanceof Y.XmlElement
              ? 3
              : t instanceof Y.Text ? 2 : t instanceof Y.Map ? 1 : 0
      const extra = {}
      if (t instanceof Y.XmlElement) extra.nodeName = t.nodeName
      if (t instanceof Y.XmlHook) extra.hookName = t.hookName
      return { ...base, typeRef, ...extra }
    }
    case 8: return { ...base, values: jsonSafe(content.arr) }
    case 9: return { ...base, guid: content.doc.guid }
    default: return base
  }
}

/**
 * Decode update bytes exactly as Yjs reads them off the wire and describe every
 * struct in wire order. This is the comparison target for the Go decoder.
 */
export const dumpUpdate = (update) => {
  const { structs, ds } = Y.decodeUpdate(update)
  return {
    structs: structs.map((s) => {
      const kind = s.constructor.name // Item | GC | Skip
      if (kind !== 'Item') {
        return { kind, id: idJSON(s.id), len: s.length }
      }
      const parent = s.parent == null
        ? null
        : typeof s.parent === 'string' ? { key: s.parent } : { id: idJSON(s.parent) }
      return {
        kind,
        id: idJSON(s.id),
        len: s.length,
        origin: idJSON(s.origin),
        rightOrigin: idJSON(s.rightOrigin),
        parent,
        parentSub: s.parentSub,
        content: contentJSON(s.content)
      }
    }),
    deleteSet: mapJSON(ds.clients, (items) => items.map((di) => [di.clock, di.len]))
  }
}
