package physical

import (
	"strconv"

	"github.com/shell-mp3/pebbledb/internal/types"
)

// Keys for hashing are encoded as strings rather than compared field by
// field, so a group or join key of any width is one map lookup.
//
// Each value is written with a one-byte tag followed by a self-delimiting
// body: digits for numbers (a tag letter always follows, so the boundary is
// unambiguous) and a length prefix for strings.
//
// Numbers are normalised: an integer and a float with the same integral value
// encode identically, so joining a column inferred as int against one
// inferred as float does what the user meant. The normalisation is exact
// within int64, since it is done on the decimal form rather than by casting
// to float64.
func appendKey(dst []byte, v types.Value) []byte {
	switch v.Kind {
	case types.KindNull:
		return append(dst, 'n')
	case types.KindInt:
		dst = append(dst, 'i')
		return strconv.AppendInt(dst, v.Int, 10)
	case types.KindFloat:
		if i := int64(v.Flt); float64(i) == v.Flt {
			dst = append(dst, 'i')
			return strconv.AppendInt(dst, i, 10)
		}
		dst = append(dst, 'f')
		return strconv.AppendFloat(dst, v.Flt, 'b', -1, 64)
	case types.KindBool:
		if v.Bool {
			return append(dst, 'b', '1')
		}
		return append(dst, 'b', '0')
	}
	dst = append(dst, 's')
	dst = strconv.AppendInt(dst, int64(len(v.Str)), 10)
	dst = append(dst, ':')
	return append(dst, v.Str...)
}

// GroupKey encodes group-by values. NULL is a value here: SQL puts all the
// NULLs in one group rather than discarding them.
func GroupKey(buf []byte, vals types.Row) []byte {
	buf = buf[:0]
	for _, v := range vals {
		buf = appendKey(buf, v)
	}
	return buf
}

// JoinKey encodes equi-join values and reports whether the key is usable. A
// NULL join key never matches anything, not even another NULL, so a row with
// one is not put in the hash table and does not probe it.
func JoinKey(buf []byte, vals types.Row) ([]byte, bool) {
	buf = buf[:0]
	for _, v := range vals {
		if v.IsNull() {
			return buf, false
		}
		buf = appendKey(buf, v)
	}
	return buf, true
}
