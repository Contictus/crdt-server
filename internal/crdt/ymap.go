package crdt

import "sort"

// YMap is a read-oriented view of a shared map type.
//
// A key's value is the newest live item for that key. Older items for the same
// key stay in the store as tombstones - that is what makes concurrent writes
// converge instead of one silently winning.
type YMap struct{ t *AbstractType }

// Map returns the root type called name as a YMap.
func (d *Doc) Map(name string) *YMap {
	t := d.Get(name)
	if t.TypeRef == typeRefUnknown {
		t.TypeRef = TypeRefMap
	}
	return &YMap{t: t}
}

// AsMap views an existing type as a map.
func AsMap(t *AbstractType) *YMap { return &YMap{t: t} }

// Type returns the underlying shared type.
func (m *YMap) Type() *AbstractType { return m.t }

// Keys returns the live keys, sorted.
func (m *YMap) Keys() []string {
	keys := make([]string, 0, len(m.t.mapItems))
	for key, it := range m.t.mapItems {
		if it != nil && !it.deleted {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

// Get returns the value stored under key.
//
// Values are the Go forms of lib0 any (see lib0.ReadAny), or *AbstractType for
// a nested shared type.
func (m *YMap) Get(key string) (any, bool) {
	it := m.t.mapItems[key]
	if it == nil || it.deleted {
		return nil, false
	}
	switch c := it.Content.(type) {
	case *ContentAny:
		if len(c.Values) == 0 {
			return nil, false
		}
		// A map entry holds exactly one value; the last one is the current one
		// (yjs/src/types/AbstractType.js typeMapGet).
		return c.Values[len(c.Values)-1], true
	case *ContentType:
		return c.Type, true
	case *ContentJSON:
		if len(c.Values) == 0 {
			return nil, false
		}
		return c.Values[len(c.Values)-1], true
	case *ContentBinary:
		return c.Data, true
	case *ContentDoc:
		return c, true
	case *ContentString:
		return c.Str, true
	default:
		return nil, false
	}
}

// Len returns the number of live keys.
func (m *YMap) Len() int { return len(m.Keys()) }
