package crdt

import "strings"

// YText is a read-oriented view of a shared text type.
//
// The server does not edit documents, so the API is deliberately smaller than
// Yjs's: it reads text out for search, previews and tests, and everything else
// travels as opaque updates.
type YText struct{ t *AbstractType }

// Text returns the root type called name as a YText.
func (d *Doc) Text(name string) *YText {
	t := d.Get(name)
	if t.TypeRef == typeRefUnknown {
		t.TypeRef = TypeRefText
	}
	return &YText{t: t}
}

// AsText views an existing type as text.
func AsText(t *AbstractType) *YText { return &YText{t: t} }

// String returns the visible text: live items with countable content, in
// document order. Format markers contribute nothing, deleted items are skipped.
func (y *YText) String() string {
	var b strings.Builder
	for it := y.t.start; it != nil; it = it.right {
		if it.deleted {
			continue
		}
		switch c := it.Content.(type) {
		case *ContentString:
			b.WriteString(c.Str)
		case *ContentEmbed, *ContentFormat, *ContentType, *ContentBinary, *ContentDoc:
			// Not text; an embed occupies a position but has no characters.
		case *ContentAny:
			for _, v := range c.Values {
				if s, ok := v.(string); ok {
					b.WriteString(s)
				}
			}
		}
	}
	return b.String()
}

// Len returns the length of the text in UTF-16 code units, which is what
// clients index by.
func (y *YText) Len() int {
	n := 0
	for it := y.t.start; it != nil; it = it.right {
		if it.Countable() {
			n += it.StructLen()
		}
	}
	return n
}

// Type returns the underlying shared type.
func (y *YText) Type() *AbstractType { return y.t }
