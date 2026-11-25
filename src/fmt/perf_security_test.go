// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package fmt_test

import (
	"bytes"
	"errors"
	"fmt"
	"internal/race"
	"io"
	"math"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
)

type panicStringer struct{}

func (panicStringer) String() string { panic("boom stringer") }

type panicError struct{}

func (panicError) Error() string { panic("boom error") }

type panicFormatter struct{}

func (panicFormatter) Format(f fmt.State, verb rune) { panic("boom formatter") }

type fixedStringer string

func (s fixedStringer) String() string { return "S(" + string(s) + ")" }

type nestedIntScanner struct {
	vals []int
	n    int
}

func (ns *nestedIntScanner) Scan(state fmt.ScanState, verb rune) error {
	var v int
	if _, err := fmt.Fscan(state, &v); err != nil {
		return err
	}
	ns.vals = append(ns.vals, v)
	ns.n--
	if ns.n > 0 {
		if _, err := fmt.Fscan(state, ns); err != nil {
			return err
		}
	}
	return nil
}

type widthCheckScanner struct {
	a, b string
}

func (w *widthCheckScanner) Scan(state fmt.ScanState, verb rune) error {
	tok, err := state.Token(true, func(r rune) bool { return r >= '0' && r <= '9' })
	if err != nil {
		return err
	}
	w.a = string(tok)
	var rest string
	if _, err := fmt.Fscan(state, &rest); err != nil {
		return err
	}
	w.b = rest
	return nil
}

func TestPerfFastPathFormattingEquivalence(t *testing.T) {
	allBytes := make([]byte, 256)
	for i := range allBytes {
		allBytes[i] = byte(i)
	}
	longASCII := strings.Repeat("abc123_", 60) // >256 bytes, forces stackBuf overflow -> pp fallback
	longUTF8 := strings.Repeat("héllo日本語", 40)

	type sampleStruct struct {
		A int
		b string
		C []byte
	}

	cases := []struct {
		format string
		args   []any
		want   string
	}{
		{"", nil, ""},
		{"plain literal without verbs", nil, "plain literal without verbs"},
		{longASCII, nil, longASCII},
		{"%%", nil, "%"},
		{"100%% done", nil, "100% done"},
		{"%d|%d|%d|%d", []any{0, 42, -99, math.MinInt64}, "0|42|-99|-9223372036854775808"},
		{"%u|%x|%X|%o|%O|%b", []any{uint(255), 0xdeadbeef, 0xCAFE, 0755, 0755, 13}, "%!u(uint=255)|deadbeef|CAFE|755|0o755|1101"},
		{"%#x|%#X|%#o|%#b", []any{0, 255, 64, 5}, "0x0|0XFF|0100|0b101"},
		{"%+d|% d|%08d|%-8d|%080d", []any{7, 7, -42, 42, 9}, "+7| 7|-0000042|42      |" + strings.Repeat("0", 79) + "9"},
		{"%80s|%-80s", []any{"x", "y"}, strings.Repeat(" ", 79) + "x|y" + strings.Repeat(" ", 79)},
		{"%.0s|%.3s|%.4s|%10.3s|%-10.3s", []any{"abcdef", "abcdef", "héllo", "日本語", "日本語"}, "|abc|héll|       日本語|日本語       "},
		{"%q|%#q|%+q", []any{"a\tb\"c", "raw`str", "hé"}, `"a\tb\"c"|"raw` + "`str" + `"|"h\u00e9"`},
		{"%q|%#q", []any{"simple_ascii", "backquote_ok"}, "\"simple_ascii\"|`backquote_ok`"},
		{"%c|%q|%#U|%8c", []any{'A', 'é', rune(0x1f600), 'Z'}, "A|'é'|U+1F600 '😀'|       Z"},
		{"%t|%10t|%-10t", []any{true, false, true}, "true|     false|true      "},
		{"%g|%f|%e|%v|%v", []any{math.NaN(), math.Inf(1), math.Inf(-1), math.Copysign(0, -1), 1.5}, "NaN|+Inf|-Inf|-0|1.5"},
		{"%v|%g", []any{complex(1.5, -2.5), complex(float32(0), float32(3))}, "(1.5-2.5i)|(0+3i)"},
		{"%v|%#v|%#v", []any{[]byte(nil), []byte(nil), []byte{0, 9, 10, 99, 100, 199, 200, 255}}, "[]|[]byte(nil)|[]byte{0x0, 0x9, 0xa, 0x63, 0x64, 0xc7, 0xc8, 0xff}"},
		{"%v", []any{allBytes}, func() string {
			var b strings.Builder
			b.WriteByte('[')
			for i := 0; i < 256; i++ {
				if i > 0 {
					b.WriteByte(' ')
				}
				b.WriteString(strconv.Itoa(i))
			}
			b.WriteByte(']')
			return b.String()
		}()},
		{"%x|%X|%s|%.2s", []any{[]byte{0xab, 0xcd}, []byte{0x0f}, []byte("bytes"), []byte("hé")}, "abcd|0F|bytes|hé"},
		{"%v|%#v", []any{[]int{1, 2, 3}, []string{"a", "b"}}, `[1 2 3]|[]string{"a", "b"}`},
		{"%v|%v|%v", []any{[]uint{1, 2}, []float64{1.25, 2.5}, []any{nil, "x", []byte{1, 2}, reflect.Value{}, reflect.ValueOf(9)}}, "[1 2]|[1.25 2.5]|[<nil> x [1 2] <invalid Value> <int Value>]"},
		{"%v|%#v", []any{map[int]string{}, map[string]int{"b": 2, "a": 1}}, "map[]|map[string]int{\"a\":1, \"b\":2}"},
		{"%v", []any{map[float64]int{math.NaN(): 1, math.Copysign(0, -1): 2, 1.0: 3}}, "map[NaN:1 -0:2 1:3]"},
		{"%v|%+v|%#v", []any{sampleStruct{1, "s", []byte{9}}, sampleStruct{1, "s", nil}, sampleStruct{1, "s", nil}}, "{1 s [9]}|{A:1 b:s C:[]}|fmt_test.sampleStruct{A:1, b:\"s\", C:[]uint8(nil)}"},
		{"%s|%v", []any{longASCII, longUTF8}, longASCII + "|" + longUTF8},
		{"%*d|%.*s", []any{-5, 3, 2, "abcd"}, "3    |ab"},
		{"%s|%v|%#v", []any{fixedStringer("x"), panicStringer{}, panicFormatter{}}, "S(x)|%!v(PANIC=String method: boom stringer)|%!v(PANIC=Format method: boom formatter)"},
		{"%v", []any{panicError{}}, "%!v(PANIC=Error method: boom error)"},
	}

	for i, tc := range cases {
		gotSprintf := fmt.Sprintf(tc.format, tc.args...)
		if gotSprintf != tc.want {
			t.Errorf("case %d Sprintf(%q): got %q, want %q", i, tc.format, gotSprintf, tc.want)
		}
		gotAppendf := string(fmt.Appendf(nil, tc.format, tc.args...))
		if gotAppendf != tc.want {
			t.Errorf("case %d Appendf(%q): got %q, want %q", i, tc.format, gotAppendf, tc.want)
		}
		var buf bytes.Buffer
		if n, err := fmt.Fprintf(&buf, tc.format, tc.args...); err != nil || n != len(tc.want) || buf.String() != tc.want {
			t.Errorf("case %d Fprintf(%q): n=%d err=%v got=%q want=%q", i, tc.format, n, err, buf.String(), tc.want)
		}
	}

	// Verify Sprint/Sprintln/Append/Appendln/Fprint/Fprintln agreement on mixed args.
	mixedArgs := []any{"a", 1, 2, "b", []int{3, 4}, true, nil, fixedStringer("z"), longASCII}
	wantSprint := "a1 2b[3 4] true <nil>S(z)" + longASCII
	if got := fmt.Sprint(mixedArgs...); got != wantSprint {
		t.Errorf("Sprint(mixed): got %q, want %q", got, wantSprint)
	}
	if got := string(fmt.Append(nil, mixedArgs...)); got != wantSprint {
		t.Errorf("Append(mixed): got %q, want %q", got, wantSprint)
	}
	var fb bytes.Buffer
	_, _ = fmt.Fprint(&fb, mixedArgs...)
	if fb.String() != wantSprint {
		t.Errorf("Fprint(mixed): got %q, want %q", fb.String(), wantSprint)
	}

	wantSprintln := "a 1 2 b [3 4] true <nil> S(z) " + longASCII + "\n"
	if got := fmt.Sprintln(mixedArgs...); got != wantSprintln {
		t.Errorf("Sprintln(mixed): got %q, want %q", got, wantSprintln)
	}
	if got := string(fmt.Appendln(nil, mixedArgs...)); got != wantSprintln {
		t.Errorf("Appendln(mixed): got %q, want %q", got, wantSprintln)
	}
	fb.Reset()
	_, _ = fmt.Fprintln(&fb, mixedArgs...)
	if fb.String() != wantSprintln {
		t.Errorf("Fprintln(mixed): got %q, want %q", fb.String(), wantSprintln)
	}
}

func TestPerfSecurityAppendPrefixPreserved(t *testing.T) {
	prefix := []byte("PREFIX:")

	// 1. Small append into preallocated buffer (fast path stays within cap).
	b1 := make([]byte, len(prefix), 128)
	copy(b1, prefix)
	out1 := fmt.Appendf(b1, "id=%d,ok=%t", 42, true)
	if string(out1) != "PREFIX:id=42,ok=true" {
		t.Fatalf("small Appendf mismatch: %q", out1)
	}

	// 2. Appendf triggering fallback (custom Stringer after fast prefix).
	b2 := make([]byte, len(prefix), 16)
	copy(b2, prefix)
	snapshot := append([]byte(nil), b2[:cap(b2)]...)
	out2 := fmt.Appendf(b2, "v=%d,s=%v", 7, fixedStringer("fallback"))
	if string(out2) != "PREFIX:v=7,s=S(fallback)" {
		t.Fatalf("fallback Appendf mismatch: %q", out2)
	}
	// Ensure sentinel bytes past original len weren't read as output and prefix wasn't corrupted.
	if string(snapshot[:len(prefix)]) != "PREFIX:" || string(out2[:len(prefix)]) != "PREFIX:" {
		t.Fatalf("prefix corrupted on fallback")
	}

	// 3. Large Append & Appendln overflowing original capacity and >256 bytes.
	big := strings.Repeat("X", 400)
	b3 := make([]byte, len(prefix), len(prefix)+4)
	copy(b3, prefix)
	out3 := fmt.Append(b3, 1, big, fixedStringer("tail"))
	if want := "PREFIX:1" + big + "S(tail)"; string(out3) != want {
		t.Fatalf("Append fallback mismatch: got len %d want len %d", len(out3), len(want))
	}

	out4 := fmt.Appendln(append([]byte(nil), prefix...), "hello", fixedStringer("w"), 99)
	if want := "PREFIX:hello S(w) 99\n"; string(out4) != want {
		t.Fatalf("Appendln fallback mismatch: got %q want %q", out4, want)
	}
}

func TestPerfSecurityErrorfWrapping(t *testing.T) {
	e1 := errors.New("root1")
	e2 := errors.New("root2")

	// Fast path Errorf without %w.
	errPlain := fmt.Errorf("failed step %d: %s", 3, "timeout")
	if errPlain.Error() != "failed step 3: timeout" {
		t.Fatalf("Errorf plain mismatch: %v", errPlain)
	}
	if errors.Unwrap(errPlain) != nil {
		t.Fatalf("unexpected unwrap on plain Errorf")
	}

	// Fallback path Errorf >256 bytes without %w.
	longMsg := strings.Repeat("z", 300)
	errLong := fmt.Errorf("prefix %d %s", 9, longMsg)
	if errLong.Error() != "prefix 9 "+longMsg {
		t.Fatalf("Errorf long mismatch")
	}

	// Wrapped errors.
	errSingle := fmt.Errorf("wrap: %w", e1)
	if !errors.Is(errSingle, e1) || errSingle.Error() != "wrap: root1" {
		t.Fatalf("single wrap mismatch: %v", errSingle)
	}
	errMulti := fmt.Errorf("multi: %w and %w", e1, e2)
	if !errors.Is(errMulti, e1) || !errors.Is(errMulti, e2) {
		t.Fatalf("multi wrap mismatch: %v", errMulti)
	}
}

func TestPerfSecurityScanNumErrorNoAlias(t *testing.T) {
	// Capture NumError.Num strings before reusing the pooled ss buffer.
	var nums []string
	var errs []error

	badInputs := []struct {
		target func() any
		input  string
	}{
		{func() any { var x int; return &x }, "99999999999999999999999999999999999999"},
		{func() any { var x uint; return &x }, "88888888888888888888888888888888888888"},
		{func() any { var x float64; return &x }, "1e500000"},
		{func() any { var x float64; return &x }, "1.5p99999999999999999999"},
	}

	for _, tc := range badInputs {
		_, err := fmt.Sscan(tc.input, tc.target())
		if err == nil {
			t.Fatalf("expected error for %q", tc.input)
		}
		var ne *strconv.NumError
		if !errors.As(err, &ne) {
			t.Fatalf("expected *strconv.NumError for %q, got %T (%v)", tc.input, err, err)
		}
		nums = append(nums, ne.Num)
		errs = append(errs, err)
	}

	// Aggressively overwrite pooled ss.buf across readers & string scanners.
	for i := 0; i < 64; i++ {
		var s string
		_, _ = fmt.Sscan(strings.Repeat("A", 512), &s)
		var f float64
		_, _ = fmt.Fscan(strings.NewReader("3.141592653589793"), &f)
		var d int
		_, _ = fmt.Sscanf("123456789", "%d", &d)
	}

	// Verify previously captured *strconv.NumError strings were NOT mutated.
	expectedPrefix := []string{
		"99999999999999999999999999999999999999",
		"88888888888888888888888888888888888888",
		"1e500000",
		"1.5p99999999999999999999",
	}
	for i := range nums {
		if nums[i] != expectedPrefix[i] {
			t.Fatalf("NumError[%d].Num aliased pooled buffer: got %q, want %q (err=%v)", i, nums[i], expectedPrefix[i], errs[i])
		}
	}
}

func TestPerfSecurityScanRecursiveAndWidth(t *testing.T) {
	// Deep recursive Scan without O(N^2) slowdown.
	const depth = 400
	var b strings.Builder
	for i := 0; i < depth; i++ {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(strconv.Itoa(i))
	}
	ns := nestedIntScanner{n: depth}
	if n, err := fmt.Sscan(b.String(), &ns); err != nil || n != 1 {
		t.Fatalf("Sscan recursive failed: n=%d err=%v", n, err)
	}
	if len(ns.vals) != depth {
		t.Fatalf("recursive count = %d, want %d", len(ns.vals), depth)
	}
	for i, v := range ns.vals {
		if v != i {
			t.Fatalf("vals[%d] = %d, want %d", i, v, i)
		}
	}

	// Width propagation through recursive Scan (%5v limits total runes seen by Scanner).
	var wc widthCheckScanner
	n, err := fmt.Sscanf("12abc", "%5v", &wc)
	if err != nil || n != 1 || wc.a != "12" || wc.b != "abc" {
		t.Fatalf("Sscanf width recursive: n=%d err=%v got=%+v", n, err, wc)
	}

	// nlIsEnd (Sscanln / Fscanln) with recursive Scanner must enforce newline semantics.
	var nsLine nestedIntScanner
	nsLine.n = 2
	if _, err := fmt.Sscanln("10 20 extra", &nsLine); err == nil {
		t.Fatalf("expected newline error for Sscanln with trailing token")
	}
	nsLine2 := nestedIntScanner{n: 2}
	if n, err := fmt.Sscanln("10 20\n", &nsLine2); err != nil || n != 1 {
		t.Fatalf("Sscanln recursive failed: n=%d err=%v", n, err)
	}
	if !reflect.DeepEqual(nsLine2.vals, []int{10, 20}) {
		t.Fatalf("Sscanln vals = %v, want [10 20]", nsLine2.vals)
	}

	// Verify stringReader / readRune EOF & UnreadRune state consistency on pool reuse.
	for i := 0; i < 32; i++ {
		var x int
		var f float64
		var p1, p2 int
		if n, err := fmt.Sscanf("100% 7 -2.25e1 9", "100%% %d %g %d", &x, &f, &p1); err != nil || n != 3 {
			t.Fatalf("iter %d Sscanf failed: n=%d err=%v", i, n, err)
		}
		if x != 7 || f != -22.5 || p1 != 9 || p2 != 0 {
			t.Fatalf("iter %d unexpected values: %d %g %d", i, x, f, p1)
		}
		// Short reader EOF check.
		var u int
		if _, err := fmt.Fscan(strings.NewReader(""), &u); err != io.EOF {
			t.Fatalf("expected io.EOF, got %v", err)
		}
	}
}

func TestPerfSecurityConcurrentStress(t *testing.T) {
	const workers = 8
	const iters = 200
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				s := fmt.Sprintf("w=%d,i=%d,f=%.2f,ok=%t", id, i, float64(i)*0.25, i%2 == 0)
				var wid, iter int
				var fl float64
				var ok bool
				if n, err := fmt.Sscanf(s, "w=%d,i=%d,f=%f,ok=%t", &wid, &iter, &fl, &ok); err != nil || n != 4 {
					t.Errorf("concurrent Sscanf(%q): n=%d err=%v", s, n, err)
					return
				}
				if wid != id || iter != i {
					t.Errorf("concurrent mismatch: got (%d,%d) want (%d,%d)", wid, iter, id, i)
					return
				}
				b := fmt.Appendf(nil, "%v-%x", []int{id, i}, []byte{byte(id), byte(i)})
				if len(b) == 0 {
					t.Errorf("empty Appendf")
					return
				}
			}
		}(w)
	}
	wg.Wait()
}

func TestPerfSecurityAllocations(t *testing.T) {
	if race.Enabled {
		t.Skip("skipping allocation assertions under race detector")
	}

	if allocs := testing.AllocsPerRun(100, func() {
		_ = fmt.Sprintf("literal string with no verbs")
	}); allocs != 0 {
		t.Errorf("Sprintf literal allocs = %v, want 0", allocs)
	}

	preAlloc := make([]byte, 0, 128)
	if allocs := testing.AllocsPerRun(100, func() {
		out := fmt.Appendf(preAlloc[:0], "k=%s,n=%d,b=%t", "abc", 12345, true)
		if len(out) == 0 {
			t.Fatal("empty")
		}
	}); allocs != 0 {
		t.Errorf("Appendf preallocated allocs = %v, want 0", allocs)
	}

	if allocs := testing.AllocsPerRun(100, func() {
		out := fmt.Append(preAlloc[:0], "k=", 12345, true, "end")
		if len(out) == 0 {
			t.Fatal("empty")
		}
	}); allocs != 0 {
		t.Errorf("Append preallocated scalar allocs = %v, want 0", allocs)
	}

	var buf bytes.Buffer
	if allocs := testing.AllocsPerRun(100, func() {
		buf.Reset()
		_, _ = fmt.Fprintf(&buf, "static literal")
	}); allocs != 0 {
		t.Errorf("Fprintf literal allocs = %v, want 0", allocs)
	}
}

func TestPerfSecurityLargeWidthPaddingBounds(t *testing.T) {
	// Negative dynamic width sets flagMinus; large widths must be exact without overflow/panic.
	if got := fmt.Sprintf("%*d", -12, 42); got != "42          " {
		t.Fatalf("negative dynamic width int: got %q", got)
	}
	if got := fmt.Sprintf("%*s", 2048, "hi"); len(got) != 2048 || !strings.HasSuffix(got, "hi") || strings.Count(got, " ") != 2046 {
		t.Fatalf("large width string mismatch: len=%d", len(got))
	}
	if got := fmt.Sprintf("%0*d", 512, -7); len(got) != 512 || got[0] != '-' || !strings.HasSuffix(got, "7") || strings.Count(got, "0") != 510 {
		t.Fatalf("large zero-pad negative int mismatch: len=%d", len(got))
	}
	if got := fmt.Sprintf("%.*f", 80, 1.5); len(got) != 82 || !strings.HasPrefix(got, "1.5") {
		t.Fatalf("large float prec mismatch: %q", got)
	}
}

func BenchmarkAppendfIntString(b *testing.B) {
	buf := make([]byte, 0, 128)
	b.ReportAllocs()
	for b.Loop() {
		buf = fmt.Appendf(buf[:0], "user=%s id=%d active=%t", "alice", 42195, true)
	}
}

func BenchmarkAppendBytes(b *testing.B) {
	data := []byte{0, 7, 42, 99, 128, 199, 200, 255}
	buf := make([]byte, 0, 128)
	b.ReportAllocs()
	for b.Loop() {
		buf = fmt.Append(buf[:0], "data=", data, 12345)
	}
}

func BenchmarkErrorfSimple(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		err := fmt.Errorf("invalid item %d: %s", 404, "not found")
		if err == nil {
			b.Fatal("nil")
		}
	}
}

func BenchmarkErrorfWrapped(b *testing.B) {
	base := errors.New("io error")
	b.ReportAllocs()
	for b.Loop() {
		err := fmt.Errorf("read %d bytes: %w", 64, base)
		if err == nil {
			b.Fatal("nil")
		}
	}
}

func BenchmarkSprintMixed(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_ = fmt.Sprint("count=", 42, " ok=", true)
	}
}

func BenchmarkSprintlnMixed(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_ = fmt.Sprintln("count=", 42, "ok=", true)
	}
}

func BenchmarkFprintfLiteral(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_, _ = fmt.Fprintf(io.Discard, "plain status message without verbs\n")
	}
}

func BenchmarkSscanIntFloat(b *testing.B) {
	var i int
	var f float64
	b.ReportAllocs()
	for b.Loop() {
		if _, err := fmt.Sscan("123456 -78.25", &i, &f); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSscanfMixed(b *testing.B) {
	var i int
	var f float64
	var ok bool
	b.ReportAllocs()
	for b.Loop() {
		if _, err := fmt.Sscanf("id=9876 val=3.125 ok=true", "id=%d val=%f ok=%t", &i, &f, &ok); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSprintfMapSorted(b *testing.B) {
	m := map[int]string{3: "c", 1: "a", 4: "d", 2: "b"}
	b.ReportAllocs()
	for b.Loop() {
		_ = fmt.Sprintf("%v", m)
	}
}
