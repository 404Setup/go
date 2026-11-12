// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Deep equality test via reflection

package reflect

import (
	"internal/abi"
	"internal/bytealg"
	"unsafe"
)

// During deepValueEqual, must keep track of checks that are
// in progress. The comparison algorithm assumes that all
// checks in progress are true when it reencounters them.
// Visited comparisons are stored in a visitSet indexed by visit.
type visit struct {
	a1  unsafe.Pointer
	a2  unsafe.Pointer
	typ *abi.Type
}

// A visitSet is a set of visits. Almost all comparisons visit only a few
// references, so the first ones are kept in an array that is searched
// linearly, and the map, which has to be allocated, is only used for the rest.
type visitSet struct {
	n     int
	small [8]visit
	m     map[visit]struct{}
}

func (s *visitSet) has(v visit) bool {
	for i := 0; i < s.n; i++ {
		if s.small[i] == v {
			return true
		}
	}
	if s.m != nil {
		_, ok := s.m[v]
		return ok
	}
	return false
}

func (s *visitSet) add(v visit) {
	if s.n < len(s.small) {
		s.small[s.n] = v
		s.n++
		return
	}
	if s.m == nil {
		s.m = make(map[visit]struct{})
	}
	s.m[v] = struct{}{}
}

// mayCycle reports whether comparing values of type t with deepValueEqual
// can reach a reference cycle, that is, whether a value of type t can hold
// pointers, maps, slices, or interfaces. Values of other types, including
// strings, channels and funcs (which are not traversed), cannot.
func mayCycle(t *abi.Type) bool {
	if !t.Pointers() {
		return false
	}
	switch t.Kind() {
	case abi.String, abi.Chan, abi.Func, abi.UnsafePointer:
		return false
	case abi.Array:
		return mayCycle((*abi.ArrayType)(unsafe.Pointer(t)).Elem)
	case abi.Struct:
		for _, f := range (*abi.StructType)(unsafe.Pointer(t)).Fields {
			if mayCycle(f.Typ) {
				return true
			}
		}
		return false
	}
	return true
}

// Tests for deep equality using reflected types. The map argument tracks
// comparisons that have already been seen, which allows short circuiting on
// recursive types.
func deepValueEqual(v1, v2 Value, visited *visitSet) bool {
	if !v1.IsValid() || !v2.IsValid() {
		return v1.IsValid() == v2.IsValid()
	}
	if v1.flag&flagMethod == 0 && v2.flag&flagMethod == 0 {
		if v1.typ() != v2.typ() {
			return false
		}
	} else if v1.Type() != v2.Type() {
		return false
	}

	// We want to avoid putting more in the visited map than we need to.
	// For any possible reference cycle that might be encountered,
	// hard(v1, v2) needs to return true for at least one of the types in the cycle,
	// and it's safe and valid to get Value's internal pointer.
	hard := func(v1, v2 Value) bool {
		switch v1.Kind() {
		case Pointer:
			if !v1.typ().Pointers() {
				// not-in-heap pointers can't be cyclic.
				// At least, all of our current uses of internal/runtime/sys.NotInHeap
				// have that property. The runtime ones aren't cyclic (and we don't use
				// DeepEqual on them anyway), and the cgo-generated ones are
				// all empty structs.
				return false
			}
			if !mayCycle(v1.typ().Elem()) {
				return false
			}
			// Nil pointers cannot be cyclic. Avoid putting them in the visited map.
			return !v1.IsNil() && !v2.IsNil()
		case Slice:
			if !mayCycle(v1.typ().Elem()) {
				return false
			}
			return !v1.IsNil() && !v2.IsNil()
		case Map:
			tt := (*abi.MapType)(unsafe.Pointer(v1.typ()))
			if !mayCycle(tt.Key) && !mayCycle(tt.Elem) {
				return false
			}
			return !v1.IsNil() && !v2.IsNil()
		case Interface:
			// Nil pointers cannot be cyclic. Avoid putting them in the visited map.
			return !v1.IsNil() && !v2.IsNil()
		}
		return false
	}

	if hard(v1, v2) {
		// For a Pointer or Map value, we need to check flagIndir,
		// which we do by calling the pointer method.
		// For Slice or Interface, flagIndir is always set,
		// and using v.ptr suffices.
		ptrval := func(v Value) unsafe.Pointer {
			switch v.Kind() {
			case Pointer, Map:
				return v.pointer()
			default:
				return v.ptr
			}
		}
		addr1 := ptrval(v1)
		addr2 := ptrval(v2)
		if uintptr(addr1) > uintptr(addr2) {
			// Canonicalize order to reduce number of entries in visited.
			// Assumes non-moving garbage collector.
			addr1, addr2 = addr2, addr1
		}

		// Short circuit if references are already seen.
		v := visit{addr1, addr2, v1.typ()}
		if visited.has(v) {
			return true
		}

		// Remember for later.
		visited.add(v)
	}

	switch v1.Kind() {
	case Array:
		if !v1.typ().Pointers() && v1.typ().TFlag&abi.TFlagRegularMemory != 0 {
			return bytealg.Equal(unsafe.Slice((*byte)(v1.ptr), v1.typ().Size()), unsafe.Slice((*byte)(v2.ptr), v2.typ().Size()))
		}
		for i := 0; i < v1.Len(); i++ {
			if !deepValueEqual(v1.Index(i), v2.Index(i), visited) {
				return false
			}
		}
		return true
	case Slice:
		if v1.IsNil() != v2.IsNil() {
			return false
		}
		if v1.Len() != v2.Len() {
			return false
		}
		if v1.UnsafePointer() == v2.UnsafePointer() {
			return true
		}
		// Special case for slices of types without pointers and without
		// padding or floating point (such as []byte), which are common:
		// elements are equal exactly when their memory is.
		if et := v1.typ().Elem(); et.TFlag&abi.TFlagRegularMemory != 0 && !et.Pointers() {
			size := uintptr(v1.Len()) * et.Size()
			return bytealg.Equal(unsafe.Slice((*byte)(v1.UnsafePointer()), size), unsafe.Slice((*byte)(v2.UnsafePointer()), size))
		}
		for i := 0; i < v1.Len(); i++ {
			if !deepValueEqual(v1.Index(i), v2.Index(i), visited) {
				return false
			}
		}
		return true
	case Interface:
		if v1.IsNil() || v2.IsNil() {
			return v1.IsNil() == v2.IsNil()
		}
		return deepValueEqual(v1.Elem(), v2.Elem(), visited)
	case Pointer:
		if v1.UnsafePointer() == v2.UnsafePointer() {
			return true
		}
		return deepValueEqual(v1.Elem(), v2.Elem(), visited)
	case Struct:
		if !v1.typ().Pointers() && v1.typ().TFlag&abi.TFlagRegularMemory != 0 {
			return bytealg.Equal(unsafe.Slice((*byte)(v1.ptr), v1.typ().Size()), unsafe.Slice((*byte)(v2.ptr), v2.typ().Size()))
		}
		for i, n := 0, v1.NumField(); i < n; i++ {
			if !deepValueEqual(v1.Field(i), v2.Field(i), visited) {
				return false
			}
		}
		return true
	case Map:
		if v1.IsNil() != v2.IsNil() {
			return false
		}
		if v1.Len() != v2.Len() {
			return false
		}
		if v1.UnsafePointer() == v2.UnsafePointer() {
			return true
		}
		// The entries are only read and the map is not modified, so
		// look at them in place rather than copying each one out.
		tt := (*abi.MapType)(unsafe.Pointer(v1.typ()))
		ro := v1.flag.ro()
		iter := v1.MapRange()
		for iter.Next() {
			key := viewVal(tt.Key, ro|flag(tt.Key.Kind()), iter.hiter.Key())
			val1 := viewVal(tt.Elem, ro|flag(tt.Elem.Kind()), iter.hiter.Elem())
			e2, fl2 := v2.mapIndexPtr(key)
			if e2 == nil || !deepValueEqual(val1, viewVal(tt.Elem, fl2, e2), visited) {
				return false
			}
		}
		return true
	case Func:
		if v1.IsNil() && v2.IsNil() {
			return true
		}
		// Can't do better than this:
		return false
	case Int, Int8, Int16, Int32, Int64:
		return v1.Int() == v2.Int()
	case Uint, Uint8, Uint16, Uint32, Uint64, Uintptr:
		return v1.Uint() == v2.Uint()
	case String:
		return v1.String() == v2.String()
	case Bool:
		return v1.Bool() == v2.Bool()
	case Float32, Float64:
		return v1.Float() == v2.Float()
	case Complex64, Complex128:
		return v1.Complex() == v2.Complex()
	default:
		// Normal equality suffices
		return valueInterface(v1, false) == valueInterface(v2, false)
	}
}

// DeepEqual reports whether x and y are “deeply equal,” defined as follows.
// Two values of identical type are deeply equal if one of the following cases applies.
// Values of distinct types are never deeply equal.
//
// Array values are deeply equal when their corresponding elements are deeply equal.
//
// Struct values are deeply equal if their corresponding fields,
// both exported and unexported, are deeply equal.
//
// Func values are deeply equal if both are nil; otherwise they are not deeply equal.
//
// Interface values are deeply equal if they hold deeply equal concrete values.
//
// Map values are deeply equal when all of the following are true:
// they are both nil or both non-nil, they have the same length,
// and either they are the same map object or their corresponding keys
// (matched using Go equality) map to deeply equal values.
//
// Pointer values are deeply equal if they are equal using Go's == operator
// or if they point to deeply equal values.
//
// Slice values are deeply equal when all of the following are true:
// they are both nil or both non-nil, they have the same length,
// and either they point to the same initial entry of the same underlying array
// (that is, &x[0] == &y[0]) or their corresponding elements (up to length) are deeply equal.
// Note that a non-nil empty slice and a nil slice (for example, []byte{} and []byte(nil))
// are not deeply equal.
//
// Other values - numbers, bools, strings, and channels - are deeply equal
// if they are equal using Go's == operator.
//
// In general DeepEqual is a recursive relaxation of Go's == operator.
// However, this idea is impossible to implement without some inconsistency.
// Specifically, it is possible for a value to be unequal to itself,
// either because it is of func type (uncomparable in general)
// or because it is a floating-point NaN value (not equal to itself in floating-point comparison),
// or because it is an array, struct, or interface containing
// such a value.
// On the other hand, pointer values are always equal to themselves,
// even if they point at or contain such problematic values,
// because they compare equal using Go's == operator, and that
// is a sufficient condition to be deeply equal, regardless of content.
// DeepEqual has been defined so that the same short-cut applies
// to slices and maps: if x and y are the same slice or the same map,
// they are deeply equal regardless of content.
//
// As DeepEqual traverses the data values it may find a cycle. The
// second and subsequent times that DeepEqual compares two pointer
// values that have been compared before, it treats the values as
// equal rather than examining the values to which they point.
// This ensures that DeepEqual terminates.
func DeepEqual(x, y any) bool {
	if x == nil || y == nil {
		return x == y
	}
	v1 := ValueOf(x)
	v2 := ValueOf(y)
	if v1.flag&flagMethod == 0 && v2.flag&flagMethod == 0 {
		if v1.typ() != v2.typ() {
			return false
		}
	} else if v1.Type() != v2.Type() {
		return false
	}
	var visited visitSet
	return deepValueEqual(v1, v2, &visited)
}
