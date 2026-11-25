// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fmt

import (
	"internal/abi"
	"internal/bytealg"
	"internal/fmtsort"
	"io"
	"os"
	"reflect"
	"strconv"
	"sync/v2"
	"unicode/utf8"
)

// Strings for use with buffer.WriteString.
// This is less overhead than using buffer.Write with byte arrays.
const (
	commaSpaceString  = ", "
	nilAngleString    = "<nil>"
	nilParenString    = "(nil)"
	nilString         = "nil"
	mapString         = "map["
	percentBangString = "%!"
	missingString     = "(MISSING)"
	badIndexString    = "(BADINDEX)"
	panicString       = "(PANIC="
	extraString       = "%!(EXTRA "
	badWidthString    = "%!(BADWIDTH)"
	badPrecString     = "%!(BADPREC)"
	noVerbString      = "%!(NOVERB)"
	invReflectString  = "<invalid reflect.Value>"
)

// State represents the printer state passed to custom formatters.
// It provides access to the [io.Writer] interface plus information about
// the flags and options for the operand's format specifier.
type State interface {
	// Write is the function to call to emit formatted output to be printed.
	Write(b []byte) (n int, err error)
	// Width returns the value of the width option and whether it has been set.
	Width() (wid int, ok bool)
	// Precision returns the value of the precision option and whether it has been set.
	Precision() (prec int, ok bool)

	// Flag reports whether the flag c, a character, has been set.
	Flag(c int) bool
}

// Formatter is implemented by any value that has a Format method.
// The implementation controls how [State] and rune are interpreted,
// and may call [Sprint] or [Fprint](f) etc. to generate its output.
type Formatter interface {
	Format(f State, verb rune)
}

// Stringer is implemented by any value that has a String method,
// which defines the “native” format for that value.
// The String method is used to print values passed as an operand
// to any format that accepts a string or to an unformatted printer
// such as [Print].
type Stringer interface {
	String() string
}

// GoStringer is implemented by any value that has a GoString method,
// which defines the Go syntax for that value.
// The GoString method is used to print values passed as an operand
// to a %#v format.
type GoStringer interface {
	GoString() string
}

// FormatString returns a string representing the fully qualified formatting
// directive captured by the [State], followed by the argument verb. ([State] does not
// itself contain the verb.) The result has a leading percent sign followed by any
// flags, the width, and the precision. Missing flags, width, and precision are
// omitted. This function allows a [Formatter] to reconstruct the original
// directive triggering the call to Format.
func FormatString(state State, verb rune) string {
	var tmp [16]byte // Use a local buffer.
	b := append(tmp[:0], '%')
	for _, c := range " +-#0" { // All known flags
		if state.Flag(int(c)) { // The argument is an int for historical reasons.
			b = append(b, byte(c))
		}
	}
	if w, ok := state.Width(); ok {
		b = strconv.AppendInt(b, int64(w), 10)
	}
	if p, ok := state.Precision(); ok {
		b = append(b, '.')
		b = strconv.AppendInt(b, int64(p), 10)
	}
	b = utf8.AppendRune(b, verb)
	return string(b)
}

// Use simple []byte instead of bytes.Buffer to avoid large dependency.
type buffer []byte

func (b *buffer) write(p []byte) {
	*b = append(*b, p...)
}

func (b *buffer) writeString(s string) {
	*b = append(*b, s...)
}

func (b *buffer) writeByte(c byte) {
	*b = append(*b, c)
}

func (b *buffer) writeRune(r rune) {
	*b = utf8.AppendRune(*b, r)
}

// pp is used to store a printer's state and is reused with sync.Pool to avoid allocations.
type pp struct {
	buf buffer

	// fmt is used to format basic items such as integers or strings.
	fmt fmt

	// reordered records whether the format string used argument reordering.
	reordered bool
	// goodArgNum records whether the most recent reordering directive was valid.
	goodArgNum bool
	// panicking is set by catchPanic to avoid infinite panic, recover, panic, ... recursion.
	panicking bool
	// erroring is set when printing an error string to guard against calling handleMethods.
	erroring bool
	// wrapErrs is set when the format string may contain a %w verb.
	wrapErrs bool
	// wrappedErrs records the targets of the %w verb.
	wrappedErrs []int
}

var ppFree = sync.Pool[*pp]{
	New: func() *pp { return new(pp) },
}

// newPrinter allocates a new pp struct or grabs a cached one.
func newPrinter() *pp {
	p := ppFree.Get()
	p.panicking = false
	p.erroring = false
	p.wrapErrs = false
	p.fmt.init(&p.buf)
	return p
}

// free saves used pp structs in ppFree; avoids an allocation per invocation.
func (p *pp) free() {
	// Proper usage of a sync.Pool requires each entry to have approximately
	// the same memory cost. To obtain this property when the stored type
	// contains a variably-sized buffer, we add a hard limit on the maximum
	// buffer to place back in the pool. If the buffer is larger than the
	// limit, we drop the buffer and recycle just the printer.
	//
	// See https://golang.org/issue/23199.
	if cap(p.buf) > 64*1024 {
		p.buf = nil
	} else {
		p.buf = p.buf[:0]
	}
	if cap(p.wrappedErrs) > 8 {
		p.wrappedErrs = nil
	}

	p.wrappedErrs = p.wrappedErrs[:0]
	ppFree.Put(p)
}

func (p *pp) Width() (wid int, ok bool) { return p.fmt.wid, p.fmt.flags&flagWidPresent != 0 }

func (p *pp) Precision() (prec int, ok bool) { return p.fmt.prec, p.fmt.flags&flagPrecPresent != 0 }

func (p *pp) Flag(b int) bool {
	switch b {
	case '-':
		return p.fmt.flags&flagMinus != 0
	case '+':
		return p.fmt.flags&(flagPlus|flagPlusV) != 0
	case '#':
		return p.fmt.flags&(flagSharp|flagSharpV) != 0
	case ' ':
		return p.fmt.flags&flagSpace != 0
	case '0':
		return p.fmt.flags&flagZero != 0
	}
	return false
}

// Write implements [io.Writer] so we can call [Fprintf] on a pp (through [State]), for
// recursive use in custom verbs.
func (p *pp) Write(b []byte) (ret int, err error) {
	p.buf.write(b)
	return len(b), nil
}

// WriteString implements [io.StringWriter] so that we can call [io.WriteString]
// on a pp (through state), for efficiency.
func (p *pp) WriteString(s string) (ret int, err error) {
	p.buf.writeString(s)
	return len(s), nil
}

// These routines end in 'f' and take a format string.

// Fprintf formats according to a format specifier and writes to w.
// It returns the number of bytes written and any write error encountered.
func Fprintf(w io.Writer, format string, a ...any) (n int, err error) {
	if len(a) == 0 && bytealg.IndexByteString(format, '%') == -1 {
		if sw, ok := w.(io.StringWriter); ok {
			return sw.WriteString(format)
		}
	}
	p := newPrinter()
	p.doPrintf(format, a)
	n, err = w.Write(p.buf)
	p.free()
	return
}

// Printf formats according to a format specifier and writes to standard output.
// It returns the number of bytes written and any write error encountered.
func Printf(format string, a ...any) (n int, err error) {
	return Fprintf(os.Stdout, format, a...)
}

// Sprintf formats according to a format specifier and returns the resulting string.
func Sprintf(format string, a ...any) string {
	if len(a) == 0 && bytealg.IndexByteString(format, '%') == -1 {
		return format
	}
	var stackBuf [256]byte
	out, nextI, nextArg, ok := appendPrintfFast(stackBuf[:0], format, a)
	if ok {
		return string(out)
	}
	p := newPrinter()
	p.buf = append(p.buf, out...)
	p.doPrintfFrom(format, a, nextI, nextArg)
	s := string(p.buf)
	p.free()
	return s
}

// Appendf formats according to a format specifier, appends the result to the byte
// slice, and returns the updated slice.
func Appendf(b []byte, format string, a ...any) []byte {
	if len(a) == 0 && bytealg.IndexByteString(format, '%') == -1 {
		return append(b, format...)
	}
	origLen := len(b)
	out, nextI, nextArg, ok := appendPrintfFast(b, format, a)
	if ok {
		return out
	}
	p := newPrinter()
	p.buf = append(p.buf, out[origLen:]...)
	p.doPrintfFrom(format, a, nextI, nextArg)
	b = append(out[:origLen], p.buf...)
	p.free()
	return b
}

// These routines do not take a format string

// Fprint formats using the default formats for its operands and writes to w.
// Spaces are added between operands when neither is a string.
// It returns the number of bytes written and any write error encountered.
func Fprint(w io.Writer, a ...any) (n int, err error) {
	p := newPrinter()
	p.doPrint(a)
	n, err = w.Write(p.buf)
	p.free()
	return
}

// Print formats using the default formats for its operands and writes to standard output.
// Spaces are added between operands when neither is a string.
// It returns the number of bytes written and any write error encountered.
func Print(a ...any) (n int, err error) {
	return Fprint(os.Stdout, a...)
}

// Sprint formats using the default formats for its operands and returns the resulting string.
// Spaces are added between operands when neither is a string.
func Sprint(a ...any) string {
	if len(a) == 0 {
		return ""
	}
	if len(a) == 1 {
		if s, ok := a[0].(string); ok {
			return s
		}
	}
	var stackBuf [256]byte
	out, nextArg, prevString, ok := appendPrintFast(stackBuf[:0], a, false)
	if ok {
		return string(out)
	}
	p := newPrinter()
	p.buf = append(p.buf, out...)
	p.doPrintFrom(a, nextArg, prevString)
	s := string(p.buf)
	p.free()
	return s
}

// Append formats using the default formats for its operands, appends the result to
// the byte slice, and returns the updated slice.
// Spaces are added between operands when neither is a string.
func Append(b []byte, a ...any) []byte {
	if len(a) == 0 {
		return b
	}
	origLen := len(b)
	out, nextArg, prevString, ok := appendPrintFast(b, a, false)
	if ok {
		return out
	}
	p := newPrinter()
	p.buf = append(p.buf, out[origLen:]...)
	p.doPrintFrom(a, nextArg, prevString)
	b = append(out[:origLen], p.buf...)
	p.free()
	return b
}

// These routines end in 'ln', do not take a format string,
// always add spaces between operands, and add a newline
// after the last operand.

// Fprintln formats using the default formats for its operands and writes to w.
// Spaces are always added between operands and a newline is appended.
// It returns the number of bytes written and any write error encountered.
func Fprintln(w io.Writer, a ...any) (n int, err error) {
	p := newPrinter()
	p.doPrintln(a)
	n, err = w.Write(p.buf)
	p.free()
	return
}

// Println formats using the default formats for its operands and writes to standard output.
// Spaces are always added between operands and a newline is appended.
// It returns the number of bytes written and any write error encountered.
func Println(a ...any) (n int, err error) {
	return Fprintln(os.Stdout, a...)
}

// Sprintln formats using the default formats for its operands and returns the resulting string.
// Spaces are always added between operands and a newline is appended.
func Sprintln(a ...any) string {
	if len(a) == 0 {
		return "\n"
	}
	var stackBuf [256]byte
	out, nextArg, _, ok := appendPrintFast(stackBuf[:0], a, true)
	if ok {
		out = append(out, '\n')
		return string(out)
	}
	p := newPrinter()
	p.buf = append(p.buf, out...)
	p.doPrintlnFrom(a, nextArg)
	s := string(p.buf)
	p.free()
	return s
}

// Appendln formats using the default formats for its operands, appends the result
// to the byte slice, and returns the updated slice. Spaces are always added
// between operands and a newline is appended.
func Appendln(b []byte, a ...any) []byte {
	if len(a) == 0 {
		return append(b, '\n')
	}
	origLen := len(b)
	out, nextArg, _, ok := appendPrintFast(b, a, true)
	if ok {
		return append(out, '\n')
	}
	p := newPrinter()
	p.buf = append(p.buf, out[origLen:]...)
	p.doPrintlnFrom(a, nextArg)
	b = append(out[:origLen], p.buf...)
	p.free()
	return b
}

// getField gets the i'th field of the struct value.
// If the field itself is a non-nil interface, return a value for
// the thing inside the interface, not the interface itself.
func getField(v reflect.Value, i int) reflect.Value {
	val := v.Field(i)
	if val.Kind() == reflect.Interface && !val.IsNil() {
		val = val.Elem()
	}
	return val
}

// tooLarge reports whether the magnitude of the integer is
// too large to be used as a formatting width or precision.
func tooLarge(x int) bool {
	const max int = 1e6
	return x > max || x < -max
}

// parsenum converts ASCII to integer.  num is 0 (and isnum is false) if no number present.
func parsenum(s string, start, end int) (num int, isnum bool, newi int) {
	if start >= end {
		return 0, false, end
	}
	for newi = start; newi < end && '0' <= s[newi] && s[newi] <= '9'; newi++ {
		if tooLarge(num) {
			return 0, false, end // Overflow; crazy long number most likely.
		}
		num = num*10 + int(s[newi]-'0')
		isnum = true
	}
	return
}

func (p *pp) unknownType(v reflect.Value) {
	if !v.IsValid() {
		p.buf.writeString(nilAngleString)
		return
	}
	p.buf.writeByte('?')
	p.buf.writeString(v.Type().String())
	p.buf.writeByte('?')
}

func (p *pp) badVerb(arg any, value reflect.Value, verb rune) {
	p.erroring = true
	p.buf.writeString(percentBangString)
	p.buf.writeRune(verb)
	p.buf.writeByte('(')
	switch {
	case arg != nil:
		p.buf.writeString(reflect.TypeOf(arg).String())
		p.buf.writeByte('=')
		p.printArg(arg, 'v')
	case value.IsValid():
		p.buf.writeString(value.Type().String())
		p.buf.writeByte('=')
		p.printValue(value, 'v', 0)
	default:
		p.buf.writeString(nilAngleString)
	}
	p.buf.writeByte(')')
	p.erroring = false
}

func (p *pp) fmtBool(arg any, value reflect.Value, v bool, verb rune) {
	switch verb {
	case 't', 'v':
		p.fmt.fmtBoolean(v)
	default:
		p.badVerb(arg, value, verb)
	}
}

// fmt0x64 formats a uint64 in hexadecimal and prefixes it with 0x or
// not, as requested, by temporarily setting the sharp flag.
func (p *pp) fmt0x64(v uint64, leading0x bool) {
	oldFlags := p.fmt.flags
	if leading0x {
		p.fmt.flags |= flagSharp
	} else {
		p.fmt.flags &^= flagSharp
	}
	p.fmt.fmtInteger(v, 16, unsigned, 'v', ldigits)
	p.fmt.flags = oldFlags
}

// fmtInteger formats a signed or unsigned integer.
func (p *pp) fmtInteger(arg any, value reflect.Value, v uint64, isSigned bool, verb rune) {
	switch verb {
	case 'v':
		if p.fmt.flags&flagSharpV != 0 && !isSigned {
			p.fmt0x64(v, true)
		} else {
			p.fmt.fmtInteger(v, 10, isSigned, verb, ldigits)
		}
	case 'd':
		p.fmt.fmtInteger(v, 10, isSigned, verb, ldigits)
	case 'b':
		p.fmt.fmtInteger(v, 2, isSigned, verb, ldigits)
	case 'o', 'O':
		p.fmt.fmtInteger(v, 8, isSigned, verb, ldigits)
	case 'x':
		p.fmt.fmtInteger(v, 16, isSigned, verb, ldigits)
	case 'X':
		p.fmt.fmtInteger(v, 16, isSigned, verb, udigits)
	case 'c':
		p.fmt.fmtC(v)
	case 'q':
		p.fmt.fmtQc(v)
	case 'U':
		p.fmt.fmtUnicode(v)
	default:
		p.badVerb(arg, value, verb)
	}
}

// fmtFloat formats a float. The default precision for each verb
// is specified as last argument in the call to fmt_float.
func (p *pp) fmtFloat(arg any, value reflect.Value, v float64, size int, verb rune) {
	switch verb {
	case 'v':
		p.fmt.fmtFloat(v, size, 'g', -1)
	case 'b', 'g', 'G', 'x', 'X':
		p.fmt.fmtFloat(v, size, verb, -1)
	case 'f', 'e', 'E':
		p.fmt.fmtFloat(v, size, verb, 6)
	case 'F':
		p.fmt.fmtFloat(v, size, 'f', 6)
	default:
		p.badVerb(arg, value, verb)
	}
}

// fmtComplex formats a complex number v with
// r = real(v) and j = imag(v) as (r+ji) using
// fmtFloat for r and j formatting.
func (p *pp) fmtComplex(arg any, value reflect.Value, v complex128, size int, verb rune) {
	// Make sure any unsupported verbs are found before the
	// calls to fmtFloat to not generate an incorrect error string.
	switch verb {
	case 'v', 'b', 'g', 'G', 'x', 'X', 'f', 'F', 'e', 'E':
		oldFlags := p.fmt.flags
		p.buf.writeByte('(')
		p.fmtFloat(arg, value, real(v), size/2, verb)
		// Imaginary part always has a sign.
		p.fmt.flags |= flagPlus
		p.fmtFloat(arg, value, imag(v), size/2, verb)
		p.buf.writeString("i)")
		p.fmt.flags = oldFlags
	default:
		p.badVerb(arg, value, verb)
	}
}

func (p *pp) fmtString(arg any, value reflect.Value, v string, verb rune) {
	switch verb {
	case 'v':
		if p.fmt.flags&flagSharpV != 0 {
			p.fmt.fmtQ(v)
		} else {
			p.fmt.fmtS(v)
		}
	case 's':
		p.fmt.fmtS(v)
	case 'x':
		p.fmt.fmtSx(v, ldigits)
	case 'X':
		p.fmt.fmtSx(v, udigits)
	case 'q':
		p.fmt.fmtQ(v)
	default:
		p.badVerb(arg, value, verb)
	}
}

func (p *pp) fmtBytes(v []byte, verb rune, typeString string) {
	switch verb {
	case 'v', 'd':
		if p.fmt.flags&flagSharpV != 0 {
			p.buf.writeString(typeString)
			if v == nil {
				p.buf.writeString(nilParenString)
				return
			}
			p.buf.writeByte('{')
			for i, c := range v {
				if i > 0 {
					p.buf.writeString(commaSpaceString)
				}
				p.fmt0x64(uint64(c), true)
			}
			p.buf.writeByte('}')
		} else {
			p.buf.writeByte('[')
			for i, c := range v {
				if i > 0 {
					p.buf.writeByte(' ')
				}
				p.fmt.fmtInteger(uint64(c), 10, unsigned, verb, ldigits)
			}
			p.buf.writeByte(']')
		}
	case 's':
		p.fmt.fmtBs(v)
	case 'x':
		p.fmt.fmtBx(v, ldigits)
	case 'X':
		p.fmt.fmtBx(v, udigits)
	case 'q':
		p.fmt.fmtQ(string(v))
	default:
		p.printValue(reflect.ValueOf(v), verb, 0)
	}
}

func (p *pp) fmtPointer(arg any, value reflect.Value, verb rune) {
	var u uintptr
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Map, reflect.Pointer, reflect.Slice, reflect.UnsafePointer:
		u = uintptr(value.UnsafePointer())
	default:
		p.badVerb(arg, value, verb)
		return
	}

	switch verb {
	case 'v':
		if p.fmt.flags&flagSharpV != 0 {
			p.buf.writeByte('(')
			p.buf.writeString(value.Type().String())
			p.buf.writeString(")(")
			if u == 0 {
				p.buf.writeString(nilString)
			} else {
				p.fmt0x64(uint64(u), true)
			}
			p.buf.writeByte(')')
		} else {
			if u == 0 {
				p.fmt.padString(nilAngleString)
			} else {
				p.fmt0x64(uint64(u), p.fmt.flags&flagSharp == 0)
			}
		}
	case 'p':
		p.fmt0x64(uint64(u), p.fmt.flags&flagSharp == 0)
	case 'b', 'o', 'd', 'x', 'X':
		p.fmtInteger(arg, value, uint64(u), unsigned, verb)
	default:
		p.badVerb(arg, value, verb)
	}
}

func (p *pp) catchPanic(arg any, verb rune, method string) {
	if err := recover(); err != nil {
		// If it's a nil pointer, just say "<nil>". The likeliest causes are a
		// Stringer that fails to guard against nil or a nil pointer for a
		// value receiver, and in either case, "<nil>" is a nice result.
		if v := reflect.ValueOf(arg); v.Kind() == reflect.Pointer && v.IsNil() {
			p.buf.writeString(nilAngleString)
			return
		}
		// Otherwise print a concise panic message. Most of the time the panic
		// value will print itself nicely.
		if p.panicking {
			// Nested panics; the recursion in printArg cannot succeed.
			panic(err)
		}

		oldFlags := p.fmt.flags
		// For this output we want default behavior.
		p.fmt.clearflags()

		p.buf.writeString(percentBangString)
		p.buf.writeRune(verb)
		p.buf.writeString(panicString)
		p.buf.writeString(method)
		p.buf.writeString(" method: ")
		p.panicking = true
		p.printArg(err, 'v')
		p.panicking = false
		p.buf.writeByte(')')

		p.fmt.flags = oldFlags
	}
}

func (p *pp) handleMethods(arg any, value reflect.Value, verb rune) (handled bool) {
	if p.erroring {
		return
	}
	t := abi.TypeOf(arg)
	if t == nil || t.TFlag&abi.TFlagUncommon == 0 {
		if verb == 'w' {
			p.badVerb(arg, value, verb)
			return true
		}
		return false
	}
	if verb == 'w' {
		// It is invalid to use %w other than with Errorf or with a non-error arg.
		_, ok := arg.(error)
		if !ok || !p.wrapErrs {
			p.badVerb(arg, value, verb)
			return true
		}
		// If the arg is a Formatter, pass 'v' as the verb to it.
		verb = 'v'
	}

	// Is it a Formatter?
	if formatter, ok := arg.(Formatter); ok {
		handled = true
		defer p.catchPanic(arg, verb, "Format")
		formatter.Format(p, verb)
		return
	}

	// If we're doing Go syntax and the argument knows how to supply it, take care of it now.
	if p.fmt.flags&flagSharpV != 0 {
		if stringer, ok := arg.(GoStringer); ok {
			handled = true
			defer p.catchPanic(arg, verb, "GoString")
			// Print the result of GoString unadorned.
			p.fmt.fmtS(stringer.GoString())
			return
		}
	} else {
		// If a string is acceptable according to the format, see if
		// the value satisfies one of the string-valued interfaces.
		// Println etc. set verb to %v, which is "stringable".
		switch verb {
		case 'v', 's', 'x', 'X', 'q':
			// Is it an error or Stringer?
			// The duplication in the bodies is necessary:
			// setting handled and deferring catchPanic
			// must happen before calling the method.
			switch v := arg.(type) {
			case error:
				handled = true
				defer p.catchPanic(arg, verb, "Error")
				p.fmtString(arg, value, v.Error(), verb)
				return

			case Stringer:
				handled = true
				defer p.catchPanic(arg, verb, "String")
				p.fmtString(arg, value, v.String(), verb)
				return
			}
		}
	}
	return false
}

func validIntVerb(verb rune) bool {
	switch verb {
	case 'v', 'd', 'b', 'o', 'O', 'x', 'X', 'c', 'q', 'U':
		return true
	}
	return false
}

func validFloatVerb(verb rune) bool {
	switch verb {
	case 'v', 'b', 'g', 'G', 'x', 'X', 'f', 'F', 'e', 'E':
		return true
	}
	return false
}

func validStringVerb(verb rune) bool {
	switch verb {
	case 'v', 's', 'x', 'X', 'q':
		return true
	}
	return false
}

func (p *pp) printArg(arg any, verb rune) {
	if arg == nil {
		switch verb {
		case 'T', 'v':
			p.fmt.padASCIIString(nilAngleString)
		default:
			p.badVerb(arg, reflect.Value{}, verb)
		}
		return
	}

	// Special processing considerations.
	// %T (the value's type) and %p (its address) are special; we always do them first.
	switch verb {
	case 'T':
		p.fmt.fmtS(reflect.TypeOf(arg).String())
		return
	case 'p':
		p.fmtPointer(arg, reflect.ValueOf(arg), 'p')
		return
	}

	// Some types can be done without reflection.
	switch f := arg.(type) {
	case bool:
		p.fmtBool(arg, reflect.Value{}, f, verb)
	case float32:
		p.fmtFloat(arg, reflect.Value{}, float64(f), 32, verb)
	case float64:
		p.fmtFloat(arg, reflect.Value{}, f, 64, verb)
	case complex64:
		p.fmtComplex(arg, reflect.Value{}, complex128(f), 64, verb)
	case complex128:
		p.fmtComplex(arg, reflect.Value{}, f, 128, verb)
	case int:
		p.fmtInteger(arg, reflect.Value{}, uint64(f), signed, verb)
	case int8:
		p.fmtInteger(arg, reflect.Value{}, uint64(f), signed, verb)
	case int16:
		p.fmtInteger(arg, reflect.Value{}, uint64(f), signed, verb)
	case int32:
		p.fmtInteger(arg, reflect.Value{}, uint64(f), signed, verb)
	case int64:
		p.fmtInteger(arg, reflect.Value{}, uint64(f), signed, verb)
	case uint:
		p.fmtInteger(arg, reflect.Value{}, uint64(f), unsigned, verb)
	case uint8:
		p.fmtInteger(arg, reflect.Value{}, uint64(f), unsigned, verb)
	case uint16:
		p.fmtInteger(arg, reflect.Value{}, uint64(f), unsigned, verb)
	case uint32:
		p.fmtInteger(arg, reflect.Value{}, uint64(f), unsigned, verb)
	case uint64:
		p.fmtInteger(arg, reflect.Value{}, f, unsigned, verb)
	case uintptr:
		p.fmtInteger(arg, reflect.Value{}, uint64(f), unsigned, verb)
	case string:
		p.fmtString(arg, reflect.Value{}, f, verb)
	case []byte:
		p.fmtBytes(f, verb, "[]byte")
	case []int:
		if !validIntVerb(verb) {
			p.printValue(reflect.ValueOf(f), verb, 0)
			return
		}
		if p.fmt.flags&flagSharpV != 0 {
			p.buf.writeString("[]int")
			if f == nil {
				p.buf.writeString(nilParenString)
				return
			}
			p.buf.writeByte('{')
			for i, v := range f {
				if i > 0 {
					p.buf.writeString(commaSpaceString)
				}
				p.fmtInteger(nil, reflect.Value{}, uint64(v), signed, verb)
			}
			p.buf.writeByte('}')
		} else {
			p.buf.writeByte('[')
			for i, v := range f {
				if i > 0 {
					p.buf.writeByte(' ')
				}
				p.fmtInteger(nil, reflect.Value{}, uint64(v), signed, verb)
			}
			p.buf.writeByte(']')
		}
	case []int64:
		if !validIntVerb(verb) {
			p.printValue(reflect.ValueOf(f), verb, 0)
			return
		}
		if p.fmt.flags&flagSharpV != 0 {
			p.buf.writeString("[]int64")
			if f == nil {
				p.buf.writeString(nilParenString)
				return
			}
			p.buf.writeByte('{')
			for i, v := range f {
				if i > 0 {
					p.buf.writeString(commaSpaceString)
				}
				p.fmtInteger(nil, reflect.Value{}, uint64(v), signed, verb)
			}
			p.buf.writeByte('}')
		} else {
			p.buf.writeByte('[')
			for i, v := range f {
				if i > 0 {
					p.buf.writeByte(' ')
				}
				p.fmtInteger(nil, reflect.Value{}, uint64(v), signed, verb)
			}
			p.buf.writeByte(']')
		}
	case []uint:
		if !validIntVerb(verb) {
			p.printValue(reflect.ValueOf(f), verb, 0)
			return
		}
		if p.fmt.flags&flagSharpV != 0 {
			p.buf.writeString("[]uint")
			if f == nil {
				p.buf.writeString(nilParenString)
				return
			}
			p.buf.writeByte('{')
			for i, v := range f {
				if i > 0 {
					p.buf.writeString(commaSpaceString)
				}
				p.fmtInteger(nil, reflect.Value{}, uint64(v), unsigned, verb)
			}
			p.buf.writeByte('}')
		} else {
			p.buf.writeByte('[')
			for i, v := range f {
				if i > 0 {
					p.buf.writeByte(' ')
				}
				p.fmtInteger(nil, reflect.Value{}, uint64(v), unsigned, verb)
			}
			p.buf.writeByte(']')
		}
	case []uint64:
		if !validIntVerb(verb) {
			p.printValue(reflect.ValueOf(f), verb, 0)
			return
		}
		if p.fmt.flags&flagSharpV != 0 {
			p.buf.writeString("[]uint64")
			if f == nil {
				p.buf.writeString(nilParenString)
				return
			}
			p.buf.writeByte('{')
			for i, v := range f {
				if i > 0 {
					p.buf.writeString(commaSpaceString)
				}
				p.fmtInteger(nil, reflect.Value{}, v, unsigned, verb)
			}
			p.buf.writeByte('}')
		} else {
			p.buf.writeByte('[')
			for i, v := range f {
				if i > 0 {
					p.buf.writeByte(' ')
				}
				p.fmtInteger(nil, reflect.Value{}, v, unsigned, verb)
			}
			p.buf.writeByte(']')
		}
	case []string:
		if !validStringVerb(verb) {
			p.printValue(reflect.ValueOf(f), verb, 0)
			return
		}
		if p.fmt.flags&flagSharpV != 0 {
			p.buf.writeString("[]string")
			if f == nil {
				p.buf.writeString(nilParenString)
				return
			}
			p.buf.writeByte('{')
			for i, v := range f {
				if i > 0 {
					p.buf.writeString(commaSpaceString)
				}
				p.fmtString(nil, reflect.Value{}, v, verb)
			}
			p.buf.writeByte('}')
		} else {
			p.buf.writeByte('[')
			for i, v := range f {
				if i > 0 {
					p.buf.writeByte(' ')
				}
				p.fmtString(nil, reflect.Value{}, v, verb)
			}
			p.buf.writeByte(']')
		}
	case []float64:
		if !validFloatVerb(verb) {
			p.printValue(reflect.ValueOf(f), verb, 0)
			return
		}
		if p.fmt.flags&flagSharpV != 0 {
			p.buf.writeString("[]float64")
			if f == nil {
				p.buf.writeString(nilParenString)
				return
			}
			p.buf.writeByte('{')
			for i, v := range f {
				if i > 0 {
					p.buf.writeString(commaSpaceString)
				}
				p.fmtFloat(nil, reflect.Value{}, v, 64, verb)
			}
			p.buf.writeByte('}')
		} else {
			p.buf.writeByte('[')
			for i, v := range f {
				if i > 0 {
					p.buf.writeByte(' ')
				}
				p.fmtFloat(nil, reflect.Value{}, v, 64, verb)
			}
			p.buf.writeByte(']')
		}
	case []any:
		if p.fmt.flags&flagSharpV != 0 || verb == 'w' {
			p.printValue(reflect.ValueOf(f), verb, 0)
			return
		}
		p.buf.writeByte('[')
		for i, elem := range f {
			if i > 0 {
				p.buf.writeByte(' ')
			}
			if elem == nil {
				if verb == 'v' {
					p.buf.writeString(nilAngleString)
				} else {
					p.badVerb(nil, reflect.Value{}, verb)
				}
				continue
			}
			if _, isRV := elem.(reflect.Value); isRV {
				p.printValue(reflect.ValueOf(elem), verb, 1)
				continue
			}
			p.printArg(elem, verb)
		}
		p.buf.writeByte(']')
	case reflect.Value:
		// Handle extractable values with special methods
		// since printValue does not handle them at depth 0.
		if f.IsValid() && f.CanInterface() {
			arg = f.Interface() // TODO(thepudds): Currently causes f to escape.
			if p.handleMethods(arg, reflect.Value{}, verb) {
				return
			}
		}
		p.printValue(f, verb, 0)
	default:
		// If the type is not simple, it might have methods.
		if !p.handleMethods(arg, reflect.Value{}, verb) {
			// Need to use reflection, since the type had no
			// interface methods that could be used for formatting.
			p.printValue(reflect.ValueOf(f), verb, 0)
		}
	}
}

// printValue is similar to printArg but starts with a reflect value, not an interface{} value.
// It does not handle 'p' and 'T' verbs because these should have been already handled by printArg.
func (p *pp) printValue(value reflect.Value, verb rune, depth int) {
	// Handle values with special methods if not already handled by printArg (depth == 0).
	// A method-less type implements none of those interfaces, so skip building arg for
	// it. An interface value gets another chance in the Interface case below. %w still
	// goes through, because handleMethods prints the %!w marker for a misused verb.
	if depth > 0 && value.IsValid() && value.CanInterface() &&
		(value.Type().NumMethod() > 0 || verb == 'w') {
		arg := value.Interface() // TODO(thepudds): Currently causes value to escape.
		if p.handleMethods(arg, value, verb) {
			return
		}
	}

	switch f := value; value.Kind() {
	case reflect.Invalid:
		if depth == 0 {
			p.buf.writeString(invReflectString)
		} else {
			switch verb {
			case 'v':
				p.buf.writeString(nilAngleString)
			default:
				p.badVerb(nil, value, verb)
			}
		}
	case reflect.Bool:
		p.fmtBool(nil, value, f.Bool(), verb)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		p.fmtInteger(nil, value, uint64(f.Int()), signed, verb)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		p.fmtInteger(nil, value, f.Uint(), unsigned, verb)
	case reflect.Float32:
		p.fmtFloat(nil, value, f.Float(), 32, verb)
	case reflect.Float64:
		p.fmtFloat(nil, value, f.Float(), 64, verb)
	case reflect.Complex64:
		p.fmtComplex(nil, value, f.Complex(), 64, verb)
	case reflect.Complex128:
		p.fmtComplex(nil, value, f.Complex(), 128, verb)
	case reflect.String:
		p.fmtString(nil, value, f.String(), verb)
	case reflect.Map:
		if p.fmt.flags&flagSharpV != 0 {
			p.buf.writeString(f.Type().String())
			if f.IsNil() {
				p.buf.writeString(nilParenString)
				return
			}
			p.buf.writeByte('{')
		} else {
			p.buf.writeString(mapString)
		}
		if f.Len() > 0 {
			sorted := fmtsort.Sort(f)
			for i, m := range sorted {
				if i > 0 {
					if p.fmt.flags&flagSharpV != 0 {
						p.buf.writeString(commaSpaceString)
					} else {
						p.buf.writeByte(' ')
					}
				}
				p.printValue(m.Key, verb, depth+1)
				p.buf.writeByte(':')
				p.printValue(m.Value, verb, depth+1)
			}
		}
		if p.fmt.flags&flagSharpV != 0 {
			p.buf.writeByte('}')
		} else {
			p.buf.writeByte(']')
		}
	case reflect.Struct:
		t := f.Type()
		if p.fmt.flags&flagSharpV != 0 {
			p.buf.writeString(t.String())
		}
		p.buf.writeByte('{')
		numField := f.NumField()
		showNames := p.fmt.flags&(flagPlusV|flagSharpV) != 0
		sharpV := p.fmt.flags&flagSharpV != 0
		for i := 0; i < numField; i++ {
			if i > 0 {
				if sharpV {
					p.buf.writeString(commaSpaceString)
				} else {
					p.buf.writeByte(' ')
				}
			}
			if showNames {
				if name := t.Field(i).Name; name != "" {
					p.buf.writeString(name)
					p.buf.writeByte(':')
				}
			}
			p.printValue(getField(f, i), verb, depth+1)
		}
		p.buf.writeByte('}')
	case reflect.Interface:
		value := f.Elem()
		if !value.IsValid() {
			if p.fmt.flags&flagSharpV != 0 {
				p.buf.writeString(f.Type().String())
				p.buf.writeString(nilParenString)
			} else {
				p.buf.writeString(nilAngleString)
			}
		} else {
			p.printValue(value, verb, depth+1)
		}
	case reflect.Array, reflect.Slice:
		t := f.Type()
		elemType := t.Elem()
		switch verb {
		case 's', 'q', 'x', 'X':
			// Handle byte and uint8 slices and arrays special for the above verbs.
			if elemType.Kind() == reflect.Uint8 {
				var bytes []byte
				if f.Kind() == reflect.Slice || f.CanAddr() {
					bytes = f.Bytes()
				} else {
					// We have an array, but we cannot Bytes() a non-addressable array,
					// so we build a slice by hand. This is a rare case but it would be nice
					// if reflection could help a little more.
					bytes = make([]byte, f.Len())
					for i := range bytes {
						bytes[i] = byte(f.Index(i).Uint())
					}
				}
				p.fmtBytes(bytes, verb, t.String())
				return
			}
		}
		n := f.Len()
		sharpV := p.fmt.flags&flagSharpV != 0
		if sharpV {
			p.buf.writeString(t.String())
			if f.Kind() == reflect.Slice && f.IsNil() {
				p.buf.writeString(nilParenString)
				return
			}
			p.buf.writeByte('{')
		} else {
			p.buf.writeByte('[')
		}
		if n > 0 && elemType.NumMethod() == 0 && elemType.Kind() != reflect.Interface && verb != 'w' {
			switch elemType.Kind() {
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				if validIntVerb(verb) {
					for i := 0; i < n; i++ {
						if i > 0 {
							if sharpV {
								p.buf.writeString(commaSpaceString)
							} else {
								p.buf.writeByte(' ')
							}
						}
						p.fmtInteger(nil, reflect.Value{}, uint64(f.Index(i).Int()), signed, verb)
					}
					goto doneSlice
				}
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
				if validIntVerb(verb) {
					for i := 0; i < n; i++ {
						if i > 0 {
							if sharpV {
								p.buf.writeString(commaSpaceString)
							} else {
								p.buf.writeByte(' ')
							}
						}
						p.fmtInteger(nil, reflect.Value{}, f.Index(i).Uint(), unsigned, verb)
					}
					goto doneSlice
				}
			case reflect.String:
				if validStringVerb(verb) {
					for i := 0; i < n; i++ {
						if i > 0 {
							if sharpV {
								p.buf.writeString(commaSpaceString)
							} else {
								p.buf.writeByte(' ')
							}
						}
						p.fmtString(nil, reflect.Value{}, f.Index(i).String(), verb)
					}
					goto doneSlice
				}
			case reflect.Float32:
				if validFloatVerb(verb) {
					for i := 0; i < n; i++ {
						if i > 0 {
							if sharpV {
								p.buf.writeString(commaSpaceString)
							} else {
								p.buf.writeByte(' ')
							}
						}
						p.fmtFloat(nil, reflect.Value{}, f.Index(i).Float(), 32, verb)
					}
					goto doneSlice
				}
			case reflect.Float64:
				if validFloatVerb(verb) {
					for i := 0; i < n; i++ {
						if i > 0 {
							if sharpV {
								p.buf.writeString(commaSpaceString)
							} else {
								p.buf.writeByte(' ')
							}
						}
						p.fmtFloat(nil, reflect.Value{}, f.Index(i).Float(), 64, verb)
					}
					goto doneSlice
				}
			case reflect.Bool:
				if verb == 't' || verb == 'v' {
					for i := 0; i < n; i++ {
						if i > 0 {
							if sharpV {
								p.buf.writeString(commaSpaceString)
							} else {
								p.buf.writeByte(' ')
							}
						}
						p.fmt.fmtBoolean(f.Index(i).Bool())
					}
					goto doneSlice
				}
			}
		}
		for i := 0; i < n; i++ {
			if i > 0 {
				if sharpV {
					p.buf.writeString(commaSpaceString)
				} else {
					p.buf.writeByte(' ')
				}
			}
			p.printValue(f.Index(i), verb, depth+1)
		}
	doneSlice:
		if sharpV {
			p.buf.writeByte('}')
		} else {
			p.buf.writeByte(']')
		}
	case reflect.Pointer:
		// pointer to array or slice or struct? ok at top level
		// but not embedded (avoid loops)
		if depth == 0 && f.UnsafePointer() != nil {
			switch a := f.Elem(); a.Kind() {
			case reflect.Array, reflect.Slice, reflect.Struct, reflect.Map:
				p.buf.writeByte('&')
				p.printValue(a, verb, depth+1)
				return
			}
		}
		fallthrough
	case reflect.Chan, reflect.Func, reflect.UnsafePointer:
		p.fmtPointer(nil, f, verb)
	default:
		p.unknownType(f)
	}
}

// intFromArg gets the argNumth element of a. On return, isInt reports whether the argument has integer type.
func intFromArg(a []any, argNum int) (num int, isInt bool, newArgNum int) {
	newArgNum = argNum
	if argNum < len(a) {
		num, isInt = a[argNum].(int) // Almost always OK.
		if !isInt {
			// Work harder.
			switch v := reflect.ValueOf(a[argNum]); v.Kind() {
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				n := v.Int()
				if int64(int(n)) == n {
					num = int(n)
					isInt = true
				}
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
				n := v.Uint()
				if int64(n) >= 0 && uint64(int(n)) == n {
					num = int(n)
					isInt = true
				}
			default:
				// Already 0, false.
			}
		}
		newArgNum = argNum + 1
		if tooLarge(num) {
			num = 0
			isInt = false
		}
	}
	return
}

// parseArgNumber returns the value of the bracketed number, minus 1
// (explicit argument numbers are one-indexed but we want zero-indexed).
// The opening bracket is known to be present at format[0].
// The returned values are the index, the number of bytes to consume
// up to the closing paren, if present, and whether the number parsed
// ok. The bytes to consume will be 1 if no closing paren is present.
func parseArgNumber(format string) (index int, wid int, ok bool) {
	// There must be at least 3 bytes: [n].
	if len(format) < 3 {
		return 0, 1, false
	}

	// Find closing bracket.
	for i := 1; i < len(format); i++ {
		if format[i] == ']' {
			width, ok, newi := parsenum(format, 1, i)
			if !ok || newi != i {
				return 0, i + 1, false
			}
			return width - 1, i + 1, true // arg numbers are one-indexed and skip paren.
		}
	}
	return 0, 1, false
}

// argNumber returns the next argument to evaluate, which is either the value of the passed-in
// argNum or the value of the bracketed integer that begins format[i:]. It also returns
// the new value of i, that is, the index of the next byte of the format to process.
func (p *pp) argNumber(argNum int, format string, i int, numArgs int) (newArgNum, newi int, found bool) {
	if len(format) <= i || format[i] != '[' {
		return argNum, i, false
	}
	p.reordered = true
	index, wid, ok := parseArgNumber(format[i:])
	if ok && 0 <= index && index < numArgs {
		return index, i + wid, true
	}
	p.goodArgNum = false
	return argNum, i + wid, ok
}

func (p *pp) badArgNum(verb rune) {
	p.buf.writeString(percentBangString)
	p.buf.writeRune(verb)
	p.buf.writeString(badIndexString)
}

func (p *pp) missingArg(verb rune) {
	p.buf.writeString(percentBangString)
	p.buf.writeRune(verb)
	p.buf.writeString(missingString)
}

func (p *pp) doPrintf(format string, a []any) {
	p.doPrintfFrom(format, a, 0, 0)
}

func (p *pp) doPrintfFrom(format string, a []any, startI, startArg int) {
	end := len(format)
	argNum := startArg  // we process one argument per non-trivial format
	afterIndex := false // previous item in format was an index like [3].
	p.reordered = false
formatLoop:
	for i := startI; i < end; {
		p.goodArgNum = true
		if format[i] != '%' {
			lasti := i
			if end-i > 8 {
				if idx := bytealg.IndexByteString(format[i:], '%'); idx >= 0 {
					i += idx
				} else {
					i = end
				}
			} else {
				i++
				for i < end && format[i] != '%' {
					i++
				}
			}
			p.buf.writeString(format[lasti:i])
			if i >= end {
				// done processing format string
				break
			}
		}

		// Process one verb
		i++

		// Do we have flags?
		p.fmt.clearflags()
		var flags fmtFlags
	simpleFormat:
		for ; i < end; i++ {
			c := format[i]
			switch c {
			case '#':
				flags |= flagSharp
			case '0':
				flags |= flagZero
			case '+':
				flags |= flagPlus
			case '-':
				flags |= flagMinus
			case ' ':
				flags |= flagSpace
			default:
				// Fast path for common case of ascii lower case simple verbs
				// without precision or width or argument indices.
				if 'a' <= c && c <= 'z' && argNum < len(a) {
					switch c {
					case 'w':
						p.wrappedErrs = append(p.wrappedErrs, argNum)
						fallthrough
					case 'v':
						// Go syntax
						if flags&flagSharp != 0 {
							flags = (flags &^ flagSharp) | flagSharpV
						}
						// Struct-field syntax
						if flags&flagPlus != 0 {
							flags = (flags &^ flagPlus) | flagPlusV
						}
					}
					if c != 'w' && c != 'p' {
						if out, ok := appendArgFast(p.buf, a[argNum], rune(c), flags, 0, 0, &p.fmt.intbuf); ok {
							p.buf = out
							argNum++
							i++
							continue formatLoop
						}
					}
					p.fmt.flags = flags
					p.printArg(a[argNum], rune(c))
					argNum++
					i++
					continue formatLoop
				}
				// Format is more complex than simple flags and a verb or is malformed.
				break simpleFormat
			}
		}

		// Do we have an explicit argument index?
		argNum, i, afterIndex = p.argNumber(argNum, format, i, len(a))

		// Do we have width?
		if i < end && format[i] == '*' {
			i++
			var widPresent bool
			p.fmt.wid, widPresent, argNum = intFromArg(a, argNum)
			if widPresent {
				flags |= flagWidPresent
			} else {
				p.buf.writeString(badWidthString)
			}

			// We have a negative width, so take its value and ensure
			// that the minus flag is set
			if p.fmt.wid < 0 {
				p.fmt.wid = -p.fmt.wid
				flags = (flags | flagMinus) &^ flagZero // Do not pad with zeros to the right.
			}
			afterIndex = false
		} else {
			var widPresent bool
			p.fmt.wid, widPresent, i = parsenum(format, i, end)
			if widPresent {
				flags |= flagWidPresent
			}
			if afterIndex && widPresent { // "%[3]2d"
				p.goodArgNum = false
			}
		}

		// Do we have precision?
		if i+1 < end && format[i] == '.' {
			i++
			if afterIndex { // "%[3].2d"
				p.goodArgNum = false
			}
			argNum, i, afterIndex = p.argNumber(argNum, format, i, len(a))
			if i < end && format[i] == '*' {
				i++
				var precPresent bool
				p.fmt.prec, precPresent, argNum = intFromArg(a, argNum)
				// Negative precision arguments don't make sense
				if p.fmt.prec < 0 {
					p.fmt.prec = 0
					precPresent = false
				}
				if precPresent {
					flags |= flagPrecPresent
				} else {
					p.buf.writeString(badPrecString)
				}
				afterIndex = false
			} else {
				var precPresent bool
				p.fmt.prec, precPresent, i = parsenum(format, i, end)
				if !precPresent {
					p.fmt.prec = 0
				}
				flags |= flagPrecPresent
			}
		}

		if !afterIndex {
			argNum, i, afterIndex = p.argNumber(argNum, format, i, len(a))
		}

		if i >= end {
			p.fmt.flags = flags
			p.buf.writeString(noVerbString)
			break
		}

		verb, size := utf8.DecodeRuneInString(format[i:])
		i += size

		switch {
		case verb == '%': // Percent does not absorb operands and ignores f.wid and f.prec.
			p.fmt.flags = flags
			p.buf.writeByte('%')
		case !p.goodArgNum:
			p.fmt.flags = flags
			p.badArgNum(verb)
		case argNum >= len(a): // No argument left over to print for the current verb.
			p.fmt.flags = flags
			p.missingArg(verb)
		case verb == 'w':
			p.wrappedErrs = append(p.wrappedErrs, argNum)
			fallthrough
		case verb == 'v':
			// Go syntax
			if flags&flagSharp != 0 {
				flags = (flags &^ flagSharp) | flagSharpV
			}
			// Struct-field syntax
			if flags&flagPlus != 0 {
				flags = (flags &^ flagPlus) | flagPlusV
			}
			fallthrough
		default:
			if verb != 'w' && verb != 'p' && verb != 'T' {
				if out, ok := appendArgFast(p.buf, a[argNum], verb, flags, p.fmt.wid, p.fmt.prec, &p.fmt.intbuf); ok {
					p.buf = out
					argNum++
					continue formatLoop
				}
			}
			p.fmt.flags = flags
			p.printArg(a[argNum], verb)
			argNum++
		}
	}

	// Check for extra arguments unless the call accessed the arguments
	// out of order, in which case it's too expensive to detect if they've all
	// been used and arguably OK if they're not.
	if !p.reordered && argNum < len(a) {
		p.fmt.clearflags()
		p.buf.writeString(extraString)
		for i, arg := range a[argNum:] {
			if i > 0 {
				p.buf.writeString(commaSpaceString)
			}
			if arg == nil {
				p.buf.writeString(nilAngleString)
			} else {
				p.buf.writeString(reflect.TypeOf(arg).String())
				p.buf.writeByte('=')
				p.printArg(arg, 'v')
			}
		}
		p.buf.writeByte(')')
	}
}

func (p *pp) doPrint(a []any) {
	p.doPrintFrom(a, 0, false)
}

func (p *pp) doPrintFrom(a []any, startArg int, prevString bool) {
	for argNum := startArg; argNum < len(a); argNum++ {
		arg := a[argNum]
		isString := arg != nil && abi.TypeOf(arg).Kind() == abi.String
		// Add a space between two non-string arguments.
		if argNum > 0 && !isString && !prevString {
			p.buf.writeByte(' ')
		}
		if out, ok := appendArgFast(p.buf, arg, 'v', 0, 0, 0, &p.fmt.intbuf); ok {
			p.buf = out
		} else {
			p.printArg(arg, 'v')
		}
		prevString = isString
	}
}

// doPrintln is like doPrint but always adds a space between arguments
// and a newline after the last argument.
func (p *pp) doPrintln(a []any) {
	p.doPrintlnFrom(a, 0)
}

func (p *pp) doPrintlnFrom(a []any, startArg int) {
	for argNum := startArg; argNum < len(a); argNum++ {
		if argNum > 0 {
			p.buf.writeByte(' ')
		}
		arg := a[argNum]
		if out, ok := appendArgFast(p.buf, arg, 'v', 0, 0, 0, &p.fmt.intbuf); ok {
			p.buf = out
		} else {
			p.printArg(arg, 'v')
		}
	}
	p.buf.writeByte('\n')
}

func appendPrintfFast(dst []byte, format string, a []any) (out []byte, nextI, nextArg int, ok bool) {
	end := len(format)
	argNum := 0
	var intbuf [68]byte
fastFormatLoop:
	for i := 0; i < end; {
		if format[i] != '%' {
			lasti := i
			if end-i > 8 {
				if idx := bytealg.IndexByteString(format[i:], '%'); idx >= 0 {
					i += idx
				} else {
					i = end
				}
			} else {
				i++
				for i < end && format[i] != '%' {
					i++
				}
			}
			dst = append(dst, format[lasti:i]...)
			if i >= end {
				break
			}
		}

		verbStart := i
		i++

		var flags fmtFlags
	flagLoop:
		for ; i < end; i++ {
			c := format[i]
			switch c {
			case '#':
				flags |= flagSharp
			case '0':
				flags |= flagZero
			case '+':
				flags |= flagPlus
			case '-':
				flags |= flagMinus
			case ' ':
				flags |= flagSpace
			default:
				if 'a' <= c && c <= 'z' {
					if c == 'w' || c == 'p' || argNum >= len(a) {
						return dst, verbStart, argNum, false
					}
					if c == 'v' {
						if flags&flagSharp != 0 {
							flags = (flags &^ flagSharp) | flagSharpV
						}
						if flags&flagPlus != 0 {
							flags = (flags &^ flagPlus) | flagPlusV
						}
					}
					nextDst, handled := appendArgFast(dst, a[argNum], rune(c), flags, 0, 0, &intbuf)
					if !handled {
						return dst, verbStart, argNum, false
					}
					dst = nextDst
					argNum++
					i++
					continue fastFormatLoop
				}
				break flagLoop
			}
		}

		if i < end && (format[i] == '[' || format[i] == '*') {
			return dst, verbStart, argNum, false
		}
		var wid int
		var widPresent bool
		wid, widPresent, i = parsenum(format, i, end)
		if widPresent {
			flags |= flagWidPresent
		}

		var prec int
		if i+1 < end && format[i] == '.' {
			i++
			if format[i] == '[' || format[i] == '*' {
				return dst, verbStart, argNum, false
			}
			var precPresent bool
			prec, precPresent, i = parsenum(format, i, end)
			if !precPresent {
				prec = 0
			}
			flags |= flagPrecPresent
		}

		if i >= end || format[i] == '[' {
			return dst, verbStart, argNum, false
		}

		c := format[i]
		if c == '%' {
			dst = append(dst, '%')
			i++
			continue
		}
		if c >= utf8.RuneSelf || c == 'w' || c == 'p' || c == 'T' || argNum >= len(a) {
			return dst, verbStart, argNum, false
		}
		if c == 'v' {
			if flags&flagSharp != 0 {
				flags = (flags &^ flagSharp) | flagSharpV
			}
			if flags&flagPlus != 0 {
				flags = (flags &^ flagPlus) | flagPlusV
			}
		}
		nextDst, handled := appendArgFast(dst, a[argNum], rune(c), flags, wid, prec, &intbuf)
		if !handled {
			return dst, verbStart, argNum, false
		}
		dst = nextDst
		argNum++
		i++
	}
	if argNum < len(a) {
		return dst, end, argNum, false
	}
	return dst, end, argNum, true
}

func appendPrintFast(dst []byte, a []any, addSpaces bool) (out []byte, nextArg int, prevString bool, ok bool) {
	var intbuf [68]byte
	for argNum, arg := range a {
		_, isString := arg.(string)
		if !isString && arg != nil && abi.TypeOf(arg).Kind() == abi.String {
			// Renamed string type may implement Stringer/Formatter.
			return dst, argNum, prevString, false
		}
		needSpace := argNum > 0 && (addSpaces || (!isString && !prevString))
		startLen := len(dst)
		if needSpace {
			dst = append(dst, ' ')
		}
		nextDst, handled := appendArgFast(dst, arg, 'v', 0, 0, 0, &intbuf)
		if !handled {
			return dst[:startLen], argNum, prevString, false
		}
		dst = nextDst
		prevString = isString
	}
	return dst, len(a), prevString, true
}

func appendArgFast(dst []byte, arg any, verb rune, flags fmtFlags, wid, prec int, intbuf *[68]byte) ([]byte, bool) {
	if arg == nil {
		if verb == 'v' {
			return appendPadASCIIString(dst, nilAngleString, flags, wid), true
		}
		return dst, false
	}
	switch f := arg.(type) {
	case string:
		switch verb {
		case 'v':
			if flags&flagSharpV != 0 {
				return appendQuoted(dst, intbuf, f, flags, wid, prec), true
			}
			fallthrough
		case 's':
			if flags&flagPrecPresent != 0 {
				f = truncateString(f, flags, prec)
			}
			return appendPadString(dst, f, flags, wid), true
		case 'x':
			return appendSbx(dst, f, nil, ldigits, flags, wid, prec), true
		case 'X':
			return appendSbx(dst, f, nil, udigits, flags, wid, prec), true
		case 'q':
			return appendQuoted(dst, intbuf, f, flags, wid, prec), true
		}
	case int:
		return appendIntVerb(dst, intbuf, uint64(f), signed, verb, flags, wid, prec)
	case int64:
		return appendIntVerb(dst, intbuf, uint64(f), signed, verb, flags, wid, prec)
	case int32:
		return appendIntVerb(dst, intbuf, uint64(f), signed, verb, flags, wid, prec)
	case int16:
		return appendIntVerb(dst, intbuf, uint64(f), signed, verb, flags, wid, prec)
	case int8:
		return appendIntVerb(dst, intbuf, uint64(f), signed, verb, flags, wid, prec)
	case uint:
		return appendIntVerb(dst, intbuf, uint64(f), unsigned, verb, flags, wid, prec)
	case uint64:
		return appendIntVerb(dst, intbuf, f, unsigned, verb, flags, wid, prec)
	case uint32:
		return appendIntVerb(dst, intbuf, uint64(f), unsigned, verb, flags, wid, prec)
	case uint16:
		return appendIntVerb(dst, intbuf, uint64(f), unsigned, verb, flags, wid, prec)
	case uint8:
		return appendIntVerb(dst, intbuf, uint64(f), unsigned, verb, flags, wid, prec)
	case uintptr:
		return appendIntVerb(dst, intbuf, uint64(f), unsigned, verb, flags, wid, prec)
	case bool:
		if verb == 't' || verb == 'v' {
			return appendBoolean(dst, f, flags, wid), true
		}
	case float64:
		return appendFloatVerb(dst, intbuf, f, 64, verb, flags, wid, prec)
	case float32:
		return appendFloatVerb(dst, intbuf, float64(f), 32, verb, flags, wid, prec)
	case complex128:
		return appendComplexVerb(dst, intbuf, f, 128, verb, flags, wid, prec)
	case complex64:
		return appendComplexVerb(dst, intbuf, complex128(f), 64, verb, flags, wid, prec)
	case []byte:
		switch verb {
		case 'v', 'd':
			if flags&flagSharpV != 0 {
				dst = append(dst, "[]byte"...)
				if f == nil {
					return append(dst, nilParenString...), true
				}
				dst = append(dst, '{')
				hexFlags := flags | flagSharp
				for i, c := range f {
					if i > 0 {
						dst = append(dst, commaSpaceString...)
					}
					dst = appendInteger(dst, intbuf, uint64(c), 16, unsigned, 'v', ldigits, hexFlags, wid, prec)
				}
				return append(dst, '}'), true
			}
			dst = append(dst, '[')
			if flags == 0 {
				for i, c := range f {
					if i > 0 {
						dst = append(dst, ' ')
					}
					if c >= 200 {
						r := uint(c-200) * 2
						dst = append(dst, '2', smalls[r], smalls[r+1])
					} else if c >= 100 {
						r := uint(c-100) * 2
						dst = append(dst, '1', smalls[r], smalls[r+1])
					} else if c >= 10 {
						r := uint(c) * 2
						dst = append(dst, smalls[r], smalls[r+1])
					} else {
						dst = append(dst, '0'+c)
					}
				}
				return append(dst, ']'), true
			}
			for i, c := range f {
				if i > 0 {
					dst = append(dst, ' ')
				}
				dst = appendInteger(dst, intbuf, uint64(c), 10, unsigned, verb, ldigits, flags, wid, prec)
			}
			return append(dst, ']'), true
		case 's':
			if flags&flagPrecPresent != 0 {
				f = truncateBytes(f, flags, prec)
			}
			return appendPad(dst, f, flags, wid), true
		case 'x':
			return appendSbx(dst, "", f, ldigits, flags, wid, prec), true
		case 'X':
			return appendSbx(dst, "", f, udigits, flags, wid, prec), true
		case 'q':
			return appendQuoted(dst, intbuf, string(f), flags, wid, prec), true
		}
	}
	return dst, false
}

func appendIntVerb(dst []byte, intbuf *[68]byte, v uint64, isSigned bool, verb rune, flags fmtFlags, wid, prec int) ([]byte, bool) {
	switch verb {
	case 'v':
		if flags&flagSharpV != 0 && !isSigned {
			return appendInteger(dst, intbuf, v, 16, unsigned, 'v', ldigits, flags|flagSharp, wid, prec), true
		}
		return appendInteger(dst, intbuf, v, 10, isSigned, verb, ldigits, flags, wid, prec), true
	case 'd':
		return appendInteger(dst, intbuf, v, 10, isSigned, verb, ldigits, flags, wid, prec), true
	case 'b':
		return appendInteger(dst, intbuf, v, 2, isSigned, verb, ldigits, flags, wid, prec), true
	case 'o', 'O':
		return appendInteger(dst, intbuf, v, 8, isSigned, verb, ldigits, flags, wid, prec), true
	case 'x':
		return appendInteger(dst, intbuf, v, 16, isSigned, verb, ldigits, flags, wid, prec), true
	case 'X':
		return appendInteger(dst, intbuf, v, 16, isSigned, verb, udigits, flags, wid, prec), true
	case 'c':
		return appendC(dst, intbuf, v, flags, wid), true
	case 'q':
		return appendQc(dst, intbuf, v, flags, wid), true
	}
	return dst, false
}

func appendFloatVerb(dst []byte, intbuf *[68]byte, v float64, size int, verb rune, flags fmtFlags, wid, prec int) ([]byte, bool) {
	p := prec
	switch verb {
	case 'v':
		verb = 'g'
		if flags&flagPrecPresent == 0 {
			p = -1
		}
	case 'b', 'g', 'G', 'x', 'X':
		if flags&flagPrecPresent == 0 {
			p = -1
		}
	case 'f', 'e', 'E':
		if flags&flagPrecPresent == 0 {
			p = 6
		}
	case 'F':
		verb = 'f'
		if flags&flagPrecPresent == 0 {
			p = 6
		}
	default:
		return dst, false
	}
	return appendFloat(dst, intbuf, v, size, verb, p, flags, wid), true
}

func appendComplexVerb(dst []byte, intbuf *[68]byte, v complex128, size int, verb rune, flags fmtFlags, wid, prec int) ([]byte, bool) {
	switch verb {
	case 'v', 'b', 'g', 'G', 'x', 'X', 'f', 'F', 'e', 'E':
		dst = append(dst, '(')
		dst, _ = appendFloatVerb(dst, intbuf, real(v), size/2, verb, flags, wid, prec)
		dst, _ = appendFloatVerb(dst, intbuf, imag(v), size/2, verb, flags|flagPlus, wid, prec)
		return append(dst, "i)"...), true
	}
	return dst, false
}
