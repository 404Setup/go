// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package atomic

import (
	"internal/abi"
	atomicv1 "sync/atomic"
	"unsafe"
)

// Value provides an atomic load and store of a value of type T.
// The zero Value contains the zero value of T.
//
// A Value must not be copied after first use.
type Value[T any] struct {
	// Mention *T in a field to disallow conversion between Value types.
	_ [0]*T

	// Values no larger than a machine word and containing no pointers are
	// stored in bits. Values represented by one pointer are stored directly
	// in ptr. All remaining values are stored in an immutable allocation
	// referenced by ptr.
	ptr  unsafe.Pointer
	bits uintptr
}

// Instantiate Value so its hot methods are checked by the compiler's intended
// inlining test. Keep this in sync with cmd/compile/internal/test/inl_test.go.
var _ = &Value[int]{}

// Load atomically loads and returns the value stored in v.
func (v *Value[T]) Load() (value T) {
	// Keep the type lookup open-coded. Load is a hot path, and calling
	// abi.TypeFor here would currently put it over the inlining budget.
	typePointer := any((*T)(nil))
	typ := (*abi.PtrType)(unsafe.Pointer((*abi.EmptyInterface)(unsafe.Pointer(&typePointer)).Type)).Elem
	if typ.PtrBytes == 0 && typ.Size_ <= unsafe.Sizeof(uintptr(0)) {
		bits := atomicv1.LoadUintptr(&v.bits)
		return *(*T)(unsafe.Pointer(&bits))
	}
	ptr := atomicv1.LoadPointer(&v.ptr)
	if ptr == nil {
		return value
	}
	if typ.TFlag&abi.TFlagDirectIface != 0 {
		*(*unsafe.Pointer)(unsafe.Pointer(&value)) = ptr
		return value
	}
	return *(*T)(ptr)
}

// Store atomically stores value into v.
func (v *Value[T]) Store(value T) {
	typePointer := any((*T)(nil))
	typ := (*abi.PtrType)(unsafe.Pointer((*abi.EmptyInterface)(unsafe.Pointer(&typePointer)).Type)).Elem
	if typ.PtrBytes == 0 && typ.Size_ <= unsafe.Sizeof(uintptr(0)) {
		var bits uintptr
		*(*T)(unsafe.Pointer(&bits)) = value
		atomicv1.StoreUintptr(&v.bits, bits)
		return
	}
	if typ.TFlag&abi.TFlagDirectIface != 0 {
		ptr := *(*unsafe.Pointer)(unsafe.Pointer(&value))
		atomicv1.StorePointer(&v.ptr, ptr)
		return
	}
	v.storeBoxed(typ, &value)
}

// storeBoxed is separate from Store so direct values do not escape as a
// consequence of the boxed path.
//
//go:noinline
func (v *Value[T]) storeBoxed(typ *abi.Type, value *T) {
	atomicv1.StorePointer(&v.ptr, boxedValuePointer(typ, value))
}

// Swap atomically stores new into v and returns the previous value.
func (v *Value[T]) Swap(new T) (old T) {
	typePointer := any((*T)(nil))
	typ := (*abi.PtrType)(unsafe.Pointer((*abi.EmptyInterface)(unsafe.Pointer(&typePointer)).Type)).Elem
	if typ.PtrBytes == 0 && typ.Size_ <= unsafe.Sizeof(uintptr(0)) {
		var newBits uintptr
		*(*T)(unsafe.Pointer(&newBits)) = new
		oldBits := atomicv1.SwapUintptr(&v.bits, newBits)
		return *(*T)(unsafe.Pointer(&oldBits))
	}
	if typ.TFlag&abi.TFlagDirectIface != 0 {
		newPtr := *(*unsafe.Pointer)(unsafe.Pointer(&new))
		oldPtr := atomicv1.SwapPointer(&v.ptr, newPtr)
		*(*unsafe.Pointer)(unsafe.Pointer(&old)) = oldPtr
		return old
	}
	ptr := v.swapBoxed(typ, &new)
	if ptr != nil {
		old = *(*T)(ptr)
	}
	return old
}

// swapBoxed is separate from Swap so direct values do not escape as a
// consequence of the boxed path.
//
//go:noinline
func (v *Value[T]) swapBoxed(typ *abi.Type, new *T) unsafe.Pointer {
	return atomicv1.SwapPointer(&v.ptr, boxedValuePointer(typ, new))
}

// CompareAndSwap executes the compare-and-swap operation for v.
// It panics if T is not a comparable type, or if T is an interface type and
// old or the value stored in v contains a value of an incomparable type.
func (v *Value[T]) CompareAndSwap(old, new T) bool {
	typePointer := any((*T)(nil))
	typ := (*abi.PtrType)(unsafe.Pointer((*abi.EmptyInterface)(unsafe.Pointer(&typePointer)).Type)).Elem
	if typ.Equal == nil {
		panic("sync/atomic/v2: compare and swap of uncomparable type")
	}
	if typ.PtrBytes == 0 && typ.Size_ <= unsafe.Sizeof(uintptr(0)) {
		return v.compareAndSwapBits(typ, old, new)
	}
	if typ.TFlag&abi.TFlagDirectIface != 0 {
		oldPtr := *(*unsafe.Pointer)(unsafe.Pointer(&old))
		newPtr := *(*unsafe.Pointer)(unsafe.Pointer(&new))
		return atomicv1.CompareAndSwapPointer(
			&v.ptr,
			oldPtr,
			newPtr,
		)
	}
	current := atomicv1.LoadPointer(&v.ptr)
	if !pointerValueEqual[T](typ, current, &old) {
		return false
	}
	replacement := boxedValuePointer(typ, &new)
	for {
		if atomicv1.CompareAndSwapPointer(&v.ptr, current, replacement) {
			return true
		}
		current = atomicv1.LoadPointer(&v.ptr)
		if !pointerValueEqual[T](typ, current, &old) {
			return false
		}
	}
}

func (v *Value[T]) compareAndSwapBits(typ *abi.Type, old, new T) bool {
	newBits := valueBits(new)
	for {
		currentBits := atomicv1.LoadUintptr(&v.bits)
		current := valueFromBits[T](currentBits)
		if !typ.Equal(
			abi.NoEscape(unsafe.Pointer(&current)),
			abi.NoEscape(unsafe.Pointer(&old)),
		) {
			return false
		}
		if atomicv1.CompareAndSwapUintptr(&v.bits, currentBits, newBits) {
			return true
		}
	}
}

func valueBits[T any](value T) uintptr {
	var bits uintptr
	*(*T)(unsafe.Pointer(&bits)) = value
	return bits
}

func valueFromBits[T any](bits uintptr) T {
	return *(*T)(unsafe.Pointer(&bits))
}

func boxedValuePointer[T any](typ *abi.Type, value *T) unsafe.Pointer {
	if typ.Kind_ == abi.Interface {
		boxed := new(T)
		*boxed = *value
		return unsafe.Pointer(boxed)
	}
	boxed := any(*value)
	return (*abi.EmptyInterface)(unsafe.Pointer(&boxed)).Data
}

func pointerValueEqual[T any](typ *abi.Type, ptr unsafe.Pointer, old *T) bool {
	if ptr == nil {
		var zero T
		return typ.Equal(
			abi.NoEscape(unsafe.Pointer(&zero)),
			abi.NoEscape(unsafe.Pointer(old)),
		)
	}
	return typ.Equal(ptr, abi.NoEscape(unsafe.Pointer(old)))
}
