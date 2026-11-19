// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package internal

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func TestChunk(t *testing.T) {
	var b bytes.Buffer

	w := NewChunkedWriter(&b)
	const chunk1 = "hello, "
	const chunk2 = "world! 0123456789abcdef"
	w.Write([]byte(chunk1))
	w.Write([]byte(chunk2))
	w.Close()

	if g, e := b.String(), "7\r\nhello, \r\n17\r\nworld! 0123456789abcdef\r\n0\r\n"; g != e {
		t.Fatalf("chunk writer wrote %q; want %q", g, e)
	}

	r := NewChunkedReader(&b)
	data, err := io.ReadAll(r)
	if err != nil {
		t.Logf(`data: "%s"`, data)
		t.Fatalf("ReadAll from reader: %v", err)
	}
	if g, e := string(data), chunk1+chunk2; g != e {
		t.Errorf("chunk reader read %q; want %q", g, e)
	}
}

func TestChunkReadMultiple(t *testing.T) {
	// Bunch of small chunks, all read together.
	{
		var b bytes.Buffer
		w := NewChunkedWriter(&b)
		w.Write([]byte("foo"))
		w.Write([]byte("bar"))
		w.Close()

		r := NewChunkedReader(&b)
		buf := make([]byte, 10)
		n, err := r.Read(buf)
		if n != 6 || err != io.EOF {
			t.Errorf("Read = %d, %v; want 6, EOF", n, err)
		}
		buf = buf[:n]
		if string(buf) != "foobar" {
			t.Errorf("Read = %q; want %q", buf, "foobar")
		}
	}

	// One big chunk followed by a little chunk, but the small bufio.Reader size
	// should prevent the second chunk header from being read.
	{
		var b bytes.Buffer
		w := NewChunkedWriter(&b)
		// fillBufChunk is 11 bytes + 3 bytes header + 2 bytes footer = 16 bytes,
		// the same as the bufio ReaderSize below (the minimum), so even
		// though we're going to try to Read with a buffer larger enough to also
		// receive "foo", the second chunk header won't be read yet.
		const fillBufChunk = "0123456789a"
		const shortChunk = "foo"
		w.Write([]byte(fillBufChunk))
		w.Write([]byte(shortChunk))
		w.Close()

		r := NewChunkedReader(bufio.NewReaderSize(&b, 16))
		buf := make([]byte, len(fillBufChunk)+len(shortChunk))
		n, err := r.Read(buf)
		if n != len(fillBufChunk) || err != nil {
			t.Errorf("Read = %d, %v; want %d, nil", n, err, len(fillBufChunk))
		}
		buf = buf[:n]
		if string(buf) != fillBufChunk {
			t.Errorf("Read = %q; want %q", buf, fillBufChunk)
		}

		n, err = r.Read(buf)
		if n != len(shortChunk) || err != io.EOF {
			t.Errorf("Read = %d, %v; want %d, EOF", n, err, len(shortChunk))
		}
	}

	// And test that we see an EOF chunk, even though our buffer is already full:
	{
		r := NewChunkedReader(bufio.NewReader(strings.NewReader("3\r\nfoo\r\n0\r\n")))
		buf := make([]byte, 3)
		n, err := r.Read(buf)
		if n != 3 || err != io.EOF {
			t.Errorf("Read = %d, %v; want 3, EOF", n, err)
		}
		if string(buf) != "foo" {
			t.Errorf("buf = %q; want foo", buf)
		}
	}
}

func TestChunkReaderAllocs(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	var buf bytes.Buffer
	w := NewChunkedWriter(&buf)
	a, b, c := []byte("aaaaaa"), []byte("bbbbbbbbbbbb"), []byte("cccccccccccccccccccccccc")
	w.Write(a)
	w.Write(b)
	w.Write(c)
	w.Close()

	readBuf := make([]byte, len(a)+len(b)+len(c)+1)
	byter := bytes.NewReader(buf.Bytes())
	bufr := bufio.NewReader(byter)
	mallocs := testing.AllocsPerRun(100, func() {
		byter.Seek(0, io.SeekStart)
		bufr.Reset(byter)
		r := NewChunkedReader(bufr)
		n, err := io.ReadFull(r, readBuf)
		if n != len(readBuf)-1 {
			t.Fatalf("read %d bytes; want %d", n, len(readBuf)-1)
		}
		if err != io.ErrUnexpectedEOF {
			t.Fatalf("read error = %v; want ErrUnexpectedEOF", err)
		}
	})
	if mallocs > 1.5 {
		t.Errorf("mallocs = %v; want 1", mallocs)
	}
}

func TestParseHexUint(t *testing.T) {
	type testCase struct {
		in      string
		want    uint64
		wantErr string
	}
	tests := []testCase{
		{"x", 0, "invalid byte in chunk length"},
		{"0000000000000000", 0, ""},
		{"0000000000000001", 1, ""},
		{"ffffffffffffffff", 1<<64 - 1, ""},
		{"000000000000bogus", 0, "invalid byte in chunk length"},
		{"00000000000000000", 0, "http chunk length too large"}, // could accept if we wanted
		{"10000000000000000", 0, "http chunk length too large"},
		{"00000000000000001", 0, "http chunk length too large"}, // could accept if we wanted
		{"", 0, "empty hex number for chunk length"},
	}
	for i := uint64(0); i <= 1234; i++ {
		tests = append(tests, testCase{in: fmt.Sprintf("%x", i), want: i})
	}
	for _, tt := range tests {
		got, err := parseHexUint([]byte(tt.in))
		if tt.wantErr != "" {
			if !strings.Contains(fmt.Sprint(err), tt.wantErr) {
				t.Errorf("parseHexUint(%q) = %v, %v; want error %q", tt.in, got, err, tt.wantErr)
			}
		} else {
			if err != nil || got != tt.want {
				t.Errorf("parseHexUint(%q) = %v, %v; want %v", tt.in, got, err, tt.want)
			}
		}
	}
}

func TestChunkReadingIgnoresExtensions(t *testing.T) {
	in := "7;ext=\"some quoted string\"\r\n" + // token=quoted string
		"hello, \r\n" +
		"17;someext\r\n" + // token without value
		"world! 0123456789abcdef\r\n" +
		"0;someextension=sometoken\r\n" // token=token
	data, err := io.ReadAll(NewChunkedReader(strings.NewReader(in)))
	if err != nil {
		t.Fatalf("ReadAll = %q, %v", data, err)
	}
	if g, e := string(data), "hello, world! 0123456789abcdef"; g != e {
		t.Errorf("read %q; want %q", g, e)
	}
}

// Issue 17355: ChunkedReader shouldn't block waiting for more data
// if it can return something.
func TestChunkReadPartial(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		pw.Write([]byte("7\r\n1234567"))
	}()
	cr := NewChunkedReader(pr)
	readBuf := make([]byte, 7)
	n, err := cr.Read(readBuf)
	if err != nil {
		t.Fatal(err)
	}
	want := "1234567"
	if n != 7 || string(readBuf) != want {
		t.Fatalf("Read: %v %q; want %d, %q", n, readBuf[:n], len(want), want)
	}
	go func() {
		pw.Write([]byte("xx"))
	}()
	_, err = cr.Read(readBuf)
	if got := fmt.Sprint(err); !strings.Contains(got, "malformed") {
		t.Fatalf("second read = %v; want malformed error", err)
	}

}

// Issue 48861: ChunkedReader should report incomplete chunks
func TestIncompleteChunk(t *testing.T) {
	const valid = "4\r\nabcd\r\n" + "5\r\nabc\r\n\r\n" + "0\r\n"

	for i := 0; i < len(valid); i++ {
		incomplete := valid[:i]
		r := NewChunkedReader(strings.NewReader(incomplete))
		if _, err := io.ReadAll(r); err != io.ErrUnexpectedEOF {
			t.Errorf("expected io.ErrUnexpectedEOF for %q, got %v", incomplete, err)
		}
	}

	r := NewChunkedReader(strings.NewReader(valid))
	if _, err := io.ReadAll(r); err != nil {
		t.Errorf("unexpected error for %q: %v", valid, err)
	}
}

func TestChunkEndReadError(t *testing.T) {
	readErr := fmt.Errorf("chunk end read error")

	r := NewChunkedReader(io.MultiReader(strings.NewReader("4\r\nabcd"), iotest.ErrReader(readErr)))
	if _, err := io.ReadAll(r); err != readErr {
		t.Errorf("expected %v, got %v", readErr, err)
	}
}

func TestChunkReaderTooMuchOverhead(t *testing.T) {
	// If the sender is sending 100x as many chunk header bytes as chunk data,
	// we should reject the stream at some point.
	chunk := []byte("1;")
	for i := 0; i < 100; i++ {
		chunk = append(chunk, 'a') // chunk extension
	}
	chunk = append(chunk, "\r\nX\r\n"...)
	const bodylen = 1 << 20
	r := NewChunkedReader(&funcReader{f: func(i int) ([]byte, error) {
		if i < bodylen {
			return chunk, nil
		}
		return []byte("0\r\n"), nil
	}})
	_, err := io.ReadAll(r)
	if err == nil {
		t.Fatalf("successfully read body with excessive overhead; want error")
	}
}

func TestChunkReaderByteAtATime(t *testing.T) {
	// Sending one byte per chunk should not trip the excess-overhead detection.
	const bodylen = 1 << 20
	r := NewChunkedReader(&funcReader{f: func(i int) ([]byte, error) {
		if i < bodylen {
			return []byte("1\r\nX\r\n"), nil
		}
		return []byte("0\r\n"), nil
	}})
	got, err := io.ReadAll(r)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if len(got) != bodylen {
		t.Errorf("read %v bytes, want %v", len(got), bodylen)
	}
}

func TestChunkInvalidInputs(t *testing.T) {
	for _, test := range []struct {
		name string
		b    string
	}{{
		name: "bare LF in chunk size",
		b:    "1\na\r\n0\r\n",
	}, {
		name: "extra LF in chunk size",
		b:    "1\r\r\na\r\n0\r\n",
	}, {
		name: "bare LF in chunk data",
		b:    "1\r\na\n0\r\n",
	}, {
		name: "bare LF in chunk extension",
		b:    "1;\na\r\n0\r\n",
	}} {
		t.Run(test.name, func(t *testing.T) {
			r := NewChunkedReader(strings.NewReader(test.b))
			got, err := io.ReadAll(r)
			if err == nil {
				t.Fatalf("unexpectedly parsed invalid chunked data:\n%q", got)
			}
		})
	}
}

type funcReader struct {
	f   func(iteration int) ([]byte, error)
	i   int
	b   []byte
	err error
}

func (r *funcReader) Read(p []byte) (n int, err error) {
	if len(r.b) == 0 && r.err == nil {
		r.b, r.err = r.f(r.i)
		r.i++
	}
	n = copy(p, r.b)
	r.b = r.b[n:]
	if len(r.b) > 0 {
		return n, nil
	}
	return n, r.err
}

func TestChunkWriterPaths(t *testing.T) {
	t.Run("zero-length", func(t *testing.T) {
		var dst bytes.Buffer
		bw := bufio.NewWriter(&dst)
		cw := NewChunkedWriter(bw)
		n, err := cw.Write(nil)
		if n != 0 || err != nil {
			t.Fatalf("Write(nil) = %d, %v; want 0, nil", n, err)
		}
		if err := bw.Flush(); err != nil {
			t.Fatal(err)
		}
		if dst.Len() != 0 {
			t.Fatalf("unexpected output for empty write: %q", dst.String())
		}
	})

	t.Run("bufio-coalesced-and-fallback", func(t *testing.T) {
		var dst bytes.Buffer
		bw := bufio.NewWriterSize(&dst, 32)
		cw := NewChunkedWriter(bw)
		// Fits in bw.Available()-20 (32-20 = 12 bytes).
		if n, err := cw.Write([]byte("1234567890")); n != 10 || err != nil {
			t.Fatalf("small write = %d, %v", n, err)
		}
		// Exceeds remaining bw.Available()-20, forcing fallback writeChunkSize + direct Write.
		large := strings.Repeat("a", 64)
		if n, err := cw.Write([]byte(large)); n != len(large) || err != nil {
			t.Fatalf("large write = %d, %v", n, err)
		}
		if err := cw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := bw.Flush(); err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(NewChunkedReader(&dst))
		if err != nil {
			t.Fatal(err)
		}
		want := "1234567890" + large
		if string(got) != want {
			t.Fatalf("roundtrip = %q, want %q", got, want)
		}
	})

	t.Run("flush-after-chunk", func(t *testing.T) {
		var dst bytes.Buffer
		fw := &FlushAfterChunkWriter{Writer: bufio.NewWriterSize(&dst, 64)}
		cw := NewChunkedWriter(fw)
		if _, err := cw.Write([]byte("hello")); err != nil {
			t.Fatal(err)
		}
		// FlushAfterChunkWriter must flush immediately on coalesced write.
		if got, want := dst.String(), "5\r\nhello\r\n"; got != want {
			t.Fatalf("after flushed chunk got %q, want %q", got, want)
		}
	})
}

func TestChunkWriterAllocs(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	payload := []byte("hello world, chunked payload!")

	bw := bufio.NewWriter(io.Discard)
	cwBufio := NewChunkedWriter(bw)
	if mallocs := testing.AllocsPerRun(100, func() {
		if _, err := cwBufio.Write(payload); err != nil {
			t.Fatal(err)
		}
	}); mallocs != 0 {
		t.Errorf("bufio Write mallocs = %v; want 0", mallocs)
	}

	cwUnbuf := NewChunkedWriter(io.Discard)
	if mallocs := testing.AllocsPerRun(100, func() {
		if _, err := cwUnbuf.Write(payload); err != nil {
			t.Fatal(err)
		}
	}); mallocs != 0 {
		t.Errorf("unbuffered Write mallocs = %v; want 0", mallocs)
	}
}

func BenchmarkChunkedWriter(b *testing.B) {
	small := []byte("hello world, chunked payload!")
	large := bytes.Repeat([]byte("x"), 16*1024)

	b.Run("BufioSmall", func(b *testing.B) {
		bw := bufio.NewWriter(io.Discard)
		cw := NewChunkedWriter(bw)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := cw.Write(small); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("BufioLarge", func(b *testing.B) {
		bw := bufio.NewWriter(io.Discard)
		cw := NewChunkedWriter(bw)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := cw.Write(large); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("FlushAfterChunk", func(b *testing.B) {
		fw := &FlushAfterChunkWriter{Writer: bufio.NewWriter(io.Discard)}
		cw := NewChunkedWriter(fw)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := cw.Write(small); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("Unbuffered", func(b *testing.B) {
		cw := NewChunkedWriter(io.Discard)
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := cw.Write(small); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func TestChunkedReaderFastPathEdgeCases(t *testing.T) {
	t.Run("ValidCases", func(t *testing.T) {
		cases := []struct {
			name string
			in   string
			want string
		}{
			{"uppercase-hex", "A\r\n0123456789\r\n0\r\n", "0123456789"},
			{"mixed-hex", "000a\r\n0123456789\r\n0f\r\nabcdefghijklmno\r\n0\r\n", "0123456789abcdefghijklmno"},
			{"ows-before-crlf", "5 \t \r\nhello\r\n0\t \r\n", "hello"},
			{"extension-after-hex", "5;foo=bar\r\nhello\r\n0;last=1\r\n", "hello"},
			{"empty-body", "0\r\n", ""},
			{"many-small-chunks", strings.Repeat("1\r\nx\r\n", 128) + "0\r\n", strings.Repeat("x", 128)},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				// Test with default bufio.Reader and tiny split reads (1-byte, 3-byte).
				for _, step := range []int{0, 1, 3, 5} {
					var src io.Reader = strings.NewReader(tc.in)
					if step > 0 {
						s := tc.in
						src = &funcReader{f: func(i int) ([]byte, error) {
							off := i * step
							if off >= len(s) {
								return nil, io.EOF
							}
							end := off + step
							if end > len(s) {
								end = len(s)
							}
							return []byte(s[off:end]), nil
						}}
					}
					got, err := io.ReadAll(NewChunkedReader(src))
					if err != nil {
						t.Fatalf("step=%d: ReadAll error: %v", step, err)
					}
					if string(got) != tc.want {
						t.Fatalf("step=%d: got %q, want %q", step, got, tc.want)
					}
				}
			})
		}
	})

	t.Run("InvalidAndSmugglingCases", func(t *testing.T) {
		cases := []struct {
			name string
			in   string
		}{
			{"empty-hex", "\r\n0\r\n"},
			{"invalid-hex-char", "1g\r\na\r\n0\r\n"},
			{"hex-overflow", "10000000000000000\r\na\r\n0\r\n"},
			{"bare-lf-size", "5\nhello\r\n0\r\n"},
			{"bare-lf-data", "5\r\nhello\n0\r\n"},
			{"bad-trailing-crlf", "5\r\nhello\rX0\r\n"},
			{"space-before-hex", " 5\r\nhello\r\n0\r\n"},
			{"cr-without-lf-in-size", "5\rhello\r\n0\r\n"},
			{"truncated-crlf", "5\r\nhello\r"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if _, err := io.ReadAll(NewChunkedReader(strings.NewReader(tc.in))); err == nil {
					t.Fatalf("expected error for %q, got nil", tc.in)
				}
			})
		}
	})
}

type plainDiscardWriter struct{}

func (plainDiscardWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestChunkedWriterNearFullAndNonStringWriterAllocs(t *testing.T) {
	// Exercise bufio.Writer with remaining capacity sweeping across the 18-byte guard.
	payload := []byte("0123456789abcdef")
	for bufSize := 16; bufSize <= 64; bufSize++ {
		var dst bytes.Buffer
		bw := bufio.NewWriterSize(&dst, bufSize)
		cw := NewChunkedWriter(bw)
		for i := 0; i < 4; i++ {
			if _, err := cw.Write(payload); err != nil {
				t.Fatalf("bufSize=%d write %d: %v", bufSize, i, err)
			}
		}
		if err := cw.Close(); err != nil {
			t.Fatalf("bufSize=%d close: %v", bufSize, err)
		}
		if err := bw.Flush(); err != nil {
			t.Fatalf("bufSize=%d flush: %v", bufSize, err)
		}
		got, err := io.ReadAll(NewChunkedReader(&dst))
		if err != nil {
			t.Fatalf("bufSize=%d readback: %v", bufSize, err)
		}
		if want := bytes.Repeat(payload, 4); !bytes.Equal(got, want) {
			t.Fatalf("bufSize=%d got %q, want %q", bufSize, got, want)
		}
	}

	if testing.Short() {
		return
	}

	// bufio.Writer near full (Available < 18) must not heap-allocate in AvailableBuffer.
	bw := bufio.NewWriterSize(plainDiscardWriter{}, 32)
	cw := NewChunkedWriter(bw)
	large := bytes.Repeat([]byte("a"), 20)
	if mallocs := testing.AllocsPerRun(100, func() {
		bw.Reset(plainDiscardWriter{})
		_, _ = bw.Write(large) // 12 bytes available (< 18)
		if _, err := cw.Write(large); err != nil {
			t.Fatal(err)
		}
		if err := cw.Close(); err != nil {
			t.Fatal(err)
		}
	}); mallocs != 0 {
		t.Errorf("near-full bufio Write+Close mallocs = %v; want 0", mallocs)
	}

	// Non-io.StringWriter must allocate 0 bytes on Write and Close.
	cwPlain := NewChunkedWriter(plainDiscardWriter{})
	if mallocs := testing.AllocsPerRun(100, func() {
		if _, err := cwPlain.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err := cwPlain.Close(); err != nil {
			t.Fatal(err)
		}
	}); mallocs != 0 {
		t.Errorf("plain io.Writer Write+Close mallocs = %v; want 0", mallocs)
	}
}

func BenchmarkChunkedReader(b *testing.B) {
	makeInput := func(chunkSize, count int) []byte {
		var buf bytes.Buffer
		cw := NewChunkedWriter(&buf)
		chunk := bytes.Repeat([]byte("a"), chunkSize)
		for i := 0; i < count; i++ {
			_, _ = cw.Write(chunk)
		}
		_ = cw.Close()
		return buf.Bytes()
	}

	for _, tc := range []struct {
		name      string
		chunkSize int
		count     int
	}{
		{"SmallChunks32Bx64", 32, 64},
		{"MediumChunks4Kx16", 4096, 16},
	} {
		b.Run(tc.name, func(b *testing.B) {
			raw := makeInput(tc.chunkSize, tc.count)
			br := bytes.NewReader(raw)
			bufr := bufio.NewReaderSize(br, 64*1024)
			dst := make([]byte, 4096)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				br.Reset(raw)
				bufr.Reset(br)
				cr := NewChunkedReader(bufr)
				for {
					_, err := cr.Read(dst)
					if err == io.EOF {
						break
					}
					if err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}
