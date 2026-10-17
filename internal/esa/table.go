package esa

import (
	"bytes"
	"encoding/json"
	"hash/maphash"
	"iter"
	"math/bits"
	"slices"
	"strings"
)

// Table is a persistent string-keyed map, a hash array mapped trie (HAMT).
// Copying a Table is O(1) and every copy is independent: Put and Delete copy
// only the path to the changed key (about log32(n) nodes) and share the rest,
// so a published State is never modified while an Update edits its copy.
// Unchanged subtrees stay pointer-identical, which lets diffTable find the
// changes between two versions in time proportional to the changes.
//
// The zero Table is empty and ready to use. Get returns the zero value for a
// missing key, like a map index. Iteration order is unspecified, as for maps.
// A Table encodes to and from the same JSON object as map[string]V.
type Table[V any] struct {
	root *tableNode[V]
	size int
}

type tableEntry[V any] struct {
	key string
	val V
}

// tableNode is a bitmap-indexed node: slots holds one item per set bit of
// bitmap, in bit order. Below 64 bits of hash, keys that still collide live
// in a collision node, which uses only collision.
type tableNode[V any] struct {
	bitmap    uint32
	slots     []tableSlot[V]
	collision []*tableEntry[V]
}

// tableSlot holds either an entry or a child node.
type tableSlot[V any] struct {
	entry *tableEntry[V]
	child *tableNode[V]
}

const tableBits = 5

var tableSeed = maphash.MakeSeed()

// tableHash is a variable so tests can force collisions.
var tableHash = func(key string) uint64 { return maphash.String(tableSeed, key) }

// TableOf builds a Table holding the entries of m.
func TableOf[V any](m map[string]V) Table[V] {
	var t Table[V]
	for k, v := range m {
		t.root = tableInsertOwned(t.root, 0, tableHash(k), &tableEntry[V]{key: k, val: v}, &t.size)
	}
	return t
}

// Len returns the number of entries.
func (t Table[V]) Len() int { return t.size }

// IsZero reports whether the table is empty; it makes `omitzero` omit an
// empty table, as `omitempty` did for maps.
func (t Table[V]) IsZero() bool { return t.size == 0 }

// Get returns the value for key, or the zero value.
func (t Table[V]) Get(key string) V {
	v, _ := t.Lookup(key)
	return v
}

// Lookup returns the value for key and whether it is present.
func (t Table[V]) Lookup(key string) (V, bool) {
	if e := t.find(key); e != nil {
		return e.val, true
	}
	var zero V
	return zero, false
}

func (t Table[V]) find(key string) *tableEntry[V] {
	n, h := t.root, tableHash(key)
	for shift := uint(0); n != nil; shift += tableBits {
		if shift >= 64 {
			for _, e := range n.collision {
				if e.key == key {
					return e
				}
			}
			return nil
		}
		bit := uint32(1) << ((h >> shift) & 31)
		if n.bitmap&bit == 0 {
			return nil
		}
		s := n.slots[bits.OnesCount32(n.bitmap&(bit-1))]
		if s.child == nil {
			if s.entry.key == key {
				return s.entry
			}
			return nil
		}
		n = s.child
	}
	return nil
}

// Put sets key to val. It changes only this Table value, never a copy of it.
func (t *Table[V]) Put(key string, val V) {
	var added bool
	t.root, added = tablePut(t.root, 0, tableHash(key), &tableEntry[V]{key: key, val: val})
	if added {
		t.size++
	}
}

// Delete removes key if present.
func (t *Table[V]) Delete(key string) {
	root, removed := tableDelete(t.root, 0, tableHash(key), key)
	if removed {
		t.root = root
		t.size--
	}
}

// All iterates over every entry.
func (t Table[V]) All() iter.Seq2[string, V] {
	return func(yield func(string, V) bool) { tableWalk(t.root, yield) }
}

// Keys returns the keys in sorted order.
func (t Table[V]) Keys() []string {
	keys := make([]string, 0, t.size)
	for k := range t.All() {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// MarshalJSON writes the object json.Marshal writes for map[string]V: keys
// sorted, each key and value encoded by encoding/json.
// sortedEntries returns the entries ordered by key, in one walk.
func (t Table[V]) sortedEntries() []tableEntry[V] {
	entries := make([]tableEntry[V], 0, t.size)
	for k, v := range t.All() {
		entries = append(entries, tableEntry[V]{k, v})
	}
	slices.SortFunc(entries, func(a, b tableEntry[V]) int { return strings.Compare(a.key, b.key) })
	return entries
}

func (t Table[V]) MarshalJSON() ([]byte, error) {
	entries := t.sortedEntries()
	// One Encoder streams every key and value into buf; it escapes exactly
	// as json.Marshal does. Encode ends each value with a newline, dropped here.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	buf.WriteByte('{')
	for i := range entries {
		if i > 0 {
			buf.WriteByte(',')
		}
		if err := enc.Encode(entries[i].key); err != nil {
			return nil, err
		}
		buf.Truncate(buf.Len() - 1)
		buf.WriteByte(':')
		if err := enc.Encode(entries[i].val); err != nil {
			return nil, err
		}
		buf.Truncate(buf.Len() - 1)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// UnmarshalJSON reads a JSON object, or null as an empty table.
func (t *Table[V]) UnmarshalJSON(data []byte) error {
	var m map[string]V
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	*t = TableOf(m)
	return nil
}

func tableWalk[V any](n *tableNode[V], yield func(string, V) bool) bool {
	if n == nil {
		return true
	}
	for _, e := range n.collision {
		if !yield(e.key, e.val) {
			return false
		}
	}
	for _, s := range n.slots {
		if s.child != nil {
			if !tableWalk(s.child, yield) {
				return false
			}
		} else if !yield(s.entry.key, s.entry.val) {
			return false
		}
	}
	return true
}

// tablePut returns a copy of n with e set, and whether the key is new.
func tablePut[V any](n *tableNode[V], shift uint, h uint64, e *tableEntry[V]) (*tableNode[V], bool) {
	if shift >= 64 {
		c := &tableNode[V]{}
		if n != nil {
			c.collision = slices.Clone(n.collision)
		}
		for i, old := range c.collision {
			if old.key == e.key {
				c.collision[i] = e
				return c, false
			}
		}
		c.collision = append(c.collision, e)
		return c, true
	}
	c := &tableNode[V]{}
	if n != nil {
		c.bitmap, c.slots = n.bitmap, slices.Clone(n.slots)
	}
	bit := uint32(1) << ((h >> shift) & 31)
	i := bits.OnesCount32(c.bitmap & (bit - 1))
	if c.bitmap&bit == 0 {
		c.bitmap |= bit
		c.slots = slices.Insert(c.slots, i, tableSlot[V]{entry: e})
		return c, true
	}
	switch s := c.slots[i]; {
	case s.child != nil:
		child, added := tablePut(s.child, shift+tableBits, h, e)
		c.slots[i] = tableSlot[V]{child: child}
		return c, added
	case s.entry.key == e.key:
		c.slots[i] = tableSlot[V]{entry: e}
		return c, false
	default:
		child, _ := tablePut(nil, shift+tableBits, tableHash(s.entry.key), s.entry)
		child, _ = tablePut(child, shift+tableBits, h, e)
		c.slots[i] = tableSlot[V]{child: child}
		return c, true
	}
}

// tableInsertOwned inserts into a trie that nothing else references yet,
// changing nodes in place; TableOf uses it to build tables without copies.
func tableInsertOwned[V any](n *tableNode[V], shift uint, h uint64, e *tableEntry[V], size *int) *tableNode[V] {
	if n == nil {
		n = &tableNode[V]{}
	}
	if shift >= 64 {
		for i, old := range n.collision {
			if old.key == e.key {
				n.collision[i] = e
				return n
			}
		}
		n.collision = append(n.collision, e)
		*size++
		return n
	}
	bit := uint32(1) << ((h >> shift) & 31)
	i := bits.OnesCount32(n.bitmap & (bit - 1))
	if n.bitmap&bit == 0 {
		n.bitmap |= bit
		n.slots = slices.Insert(n.slots, i, tableSlot[V]{entry: e})
		*size++
		return n
	}
	switch s := n.slots[i]; {
	case s.child != nil:
		n.slots[i].child = tableInsertOwned(s.child, shift+tableBits, h, e, size)
	case s.entry.key == e.key:
		n.slots[i].entry = e
	default:
		var ignored int
		child := tableInsertOwned(nil, shift+tableBits, tableHash(s.entry.key), s.entry, &ignored)
		n.slots[i] = tableSlot[V]{child: tableInsertOwned(child, shift+tableBits, h, e, size)}
	}
	return n
}

// tableDelete returns a copy of n without key, and whether it was present.
// Nodes left with a single entry collapse into that entry, keeping the trie
// shallow.
func tableDelete[V any](n *tableNode[V], shift uint, h uint64, key string) (*tableNode[V], bool) {
	if n == nil {
		return nil, false
	}
	if shift >= 64 {
		for i, e := range n.collision {
			if e.key == key {
				if len(n.collision) == 1 {
					return nil, true
				}
				return &tableNode[V]{collision: slices.Delete(slices.Clone(n.collision), i, i+1)}, true
			}
		}
		return n, false
	}
	bit := uint32(1) << ((h >> shift) & 31)
	if n.bitmap&bit == 0 {
		return n, false
	}
	i := bits.OnesCount32(n.bitmap & (bit - 1))
	s := n.slots[i]
	c := &tableNode[V]{bitmap: n.bitmap, slots: slices.Clone(n.slots)}
	if s.child != nil {
		child, removed := tableDelete(s.child, shift+tableBits, h, key)
		if !removed {
			return n, false
		}
		switch {
		case child == nil:
			c.bitmap &^= bit
			c.slots = slices.Delete(c.slots, i, i+1)
		case len(child.collision) == 1:
			c.slots[i] = tableSlot[V]{entry: child.collision[0]}
		case len(child.collision) == 0 && len(child.slots) == 1 && child.slots[0].entry != nil:
			c.slots[i] = tableSlot[V]{entry: child.slots[0].entry}
		default:
			c.slots[i] = tableSlot[V]{child: child}
		}
	} else {
		if s.entry.key != key {
			return n, false
		}
		c.bitmap &^= bit
		c.slots = slices.Delete(c.slots, i, i+1)
	}
	if len(c.slots) == 0 {
		return nil, true
	}
	return c, true
}

// diffTable lists the keys whose presence or value differs between old and
// next. Subtrees the two versions share are skipped, so the cost follows the
// changes, not the size. A key re-put with bit-identical value is unchanged.
func diffTable[V any](old, next Table[V]) MapChanges {
	var c MapChanges
	tableDiffNodes(old.root, next.root, 0, &c)
	slices.Sort(c.Upserted)
	slices.Sort(c.Deleted)
	return c
}

func tableDiffNodes[V any](a, b *tableNode[V], shift uint, c *MapChanges) {
	if a == b {
		return
	}
	if a == nil || b == nil || shift >= 64 || a.collision != nil || b.collision != nil {
		tableDiffEntries(tableEntries(a), tableEntries(b), c)
		return
	}
	for set := a.bitmap | b.bitmap; set != 0; set &= set - 1 {
		bit := set & -set
		sa, okA := tableSlotAt(a, bit)
		sb, okB := tableSlotAt(b, bit)
		switch {
		case okA && okB && sa.child != nil && sb.child != nil:
			tableDiffNodes(sa.child, sb.child, shift+tableBits, c)
		case okA && okB && sa.entry != nil && sb.entry != nil && sa.entry == sb.entry:
		default:
			tableDiffEntries(tableSlotEntries(sa, okA), tableSlotEntries(sb, okB), c)
		}
	}
}

func tableSlotAt[V any](n *tableNode[V], bit uint32) (tableSlot[V], bool) {
	if n.bitmap&bit == 0 {
		return tableSlot[V]{}, false
	}
	return n.slots[bits.OnesCount32(n.bitmap&(bit-1))], true
}

func tableSlotEntries[V any](s tableSlot[V], ok bool) map[string]*tableEntry[V] {
	switch {
	case !ok:
		return nil
	case s.child != nil:
		return tableEntries(s.child)
	default:
		return map[string]*tableEntry[V]{s.entry.key: s.entry}
	}
}

func tableEntries[V any](n *tableNode[V]) map[string]*tableEntry[V] {
	out := map[string]*tableEntry[V]{}
	var collect func(*tableNode[V])
	collect = func(n *tableNode[V]) {
		if n == nil {
			return
		}
		for _, e := range n.collision {
			out[e.key] = e
		}
		for _, s := range n.slots {
			if s.child != nil {
				collect(s.child)
			} else {
				out[s.entry.key] = s.entry
			}
		}
	}
	collect(n)
	return out
}

func tableDiffEntries[V any](a, b map[string]*tableEntry[V], c *MapChanges) {
	for k, eb := range b {
		if ea, ok := a[k]; !ok || ea != eb && !sameBits(&ea.val, &eb.val) {
			c.Upserted = append(c.Upserted, k)
		}
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			c.Deleted = append(c.Deleted, k)
		}
	}
}

// stateTable is implemented by every *Table; the test guard and reflection
// coverage tests use it without knowing V.
type stateTable interface {
	tableKeys() []string
	tableEncode(key string) (string, bool)
	tablePutZero(key string)
}

func (t *Table[V]) tableKeys() []string { return t.Keys() }

func (t *Table[V]) tableEncode(key string) (string, bool) {
	v, ok := t.Lookup(key)
	if !ok {
		return "", false
	}
	b, _ := json.Marshal(v)
	return string(b), true
}

func (t *Table[V]) tablePutZero(key string) {
	var zero V
	t.Put(key, zero)
}
