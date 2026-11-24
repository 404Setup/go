// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package xml_test

import (
	"bytes"
	. "encoding/xml"
	"io"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type customTextTag string

func (c customTextTag) MarshalText() ([]byte, error) {
	return []byte("[" + string(c) + "]"), nil
}

func (c *customTextTag) UnmarshalText(b []byte) error {
	s := string(b)
	if len(s) >= 2 && s[0] == '[' && s[len(s)-1] == ']' {
		s = s[1 : len(s)-1]
	}
	*c = customTextTag(s)
	return nil
}

type nonAddrPtrMarshaler struct {
	V string
}

func (n *nonAddrPtrMarshaler) MarshalXML(e *Encoder, start StartElement) error {
	return e.EncodeElement("PTR:"+n.V, start)
}

type testEmbeddedAttr struct {
	Code int `xml:"code,attr"`
}

type testRecursiveNode struct {
	Name  string               `xml:"name"`
	Child []*testRecursiveNode `xml:"child,omitempty"`
}

type testComplexDoc struct {
	XMLName  Name              `xml:"doc"`
	ID       int64             `xml:"id,attr"`
	Flag     bool              `xml:"flag,attr,omitempty"`
	Tags     []string          `xml:"tag,attr,omitempty"`
	PtrCode  *testEmbeddedAttr `xml:",omitempty"`
	Count    uint32            `xml:"count"`
	Ratio    float64           `xml:"ratio"`
	Created  time.Time         `xml:"created"`
	Custom   customTextTag     `xml:"custom"`
	Raw      []byte            `xml:"raw"`
	EmptyRaw []byte            `xml:"emptyRaw"`
	PathVal  string            `xml:"a>b>c"`
	Items    []string          `xml:"item"`
	Unknown  []string          `xml:",any"`
	Comment  string            `xml:",comment"`
	Inner    string            `xml:",innerxml"`
}

func TestFastMarshalUnmarshalEdgeCases(t *testing.T) {
	ts := time.Date(2026, 3, 15, 10, 20, 30, 0, time.UTC)
	in := testComplexDoc{
		ID:       42,
		Flag:     true,
		Tags:     []string{"alpha", "beta"},
		PtrCode:  &testEmbeddedAttr{Code: 7},
		Count:    999,
		Ratio:    3.25,
		Created:  ts,
		Custom:   "hello",
		Raw:      []byte("<escaped & \"quoted\" 'attr'>"),
		EmptyRaw: []byte{},
		PathVal:  "nested-leaf",
		Items:    []string{"first", "second"},
	}

	b, err := Marshal(in)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	var out testComplexDoc
	if err := Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal failed: %v\nxml:\n%s", err, b)
	}
	if out.ID != in.ID || out.Flag != in.Flag || !reflect.DeepEqual(out.Tags, in.Tags) ||
		out.PtrCode == nil || out.PtrCode.Code != 7 || out.Count != in.Count ||
		out.Ratio != in.Ratio || !out.Created.Equal(in.Created) || out.Custom != in.Custom ||
		!bytes.Equal(out.Raw, in.Raw) || out.EmptyRaw == nil || len(out.EmptyRaw) != 0 ||
		out.PathVal != in.PathVal || !reflect.DeepEqual(out.Items, in.Items) {
		t.Fatalf("roundtrip mismatch:\ngot  %+v\nwant %+v", out, in)
	}

	// Nil embedded pointer must stay nil if only unmatched attributes exist
	type withNilEmbed struct {
		*testEmbeddedAttr
		Name string `xml:"name"`
	}
	var ne withNilEmbed
	if err := Unmarshal([]byte(`<withNilEmbed other="9"><name>ok</name></withNilEmbed>`), &ne); err != nil {
		t.Fatalf("Unmarshal withNilEmbed failed: %v", err)
	}
	if ne.testEmbeddedAttr != nil || ne.Name != "ok" {
		t.Fatalf("unexpected embedded pointer init: %+v", ne)
	}

	// Recursive struct roundtrip
	rec := testRecursiveNode{
		Name: "root",
		Child: []*testRecursiveNode{
			{Name: "c1", Child: []*testRecursiveNode{{Name: "c1a"}}},
			{Name: "c2"},
		},
	}
	rb, err := Marshal(&rec)
	if err != nil {
		t.Fatalf("Marshal recursive failed: %v", err)
	}
	var recOut testRecursiveNode
	if err := Unmarshal(rb, &recOut); err != nil {
		t.Fatalf("Unmarshal recursive failed: %v", err)
	}
	if !reflect.DeepEqual(recOut, rec) {
		t.Fatalf("recursive mismatch:\ngot  %+v\nwant %+v", recOut, rec)
	}

	// Non-addressable value Marshal(T{}) must NOT call pointer-receiver (*T).MarshalXML
	nonAddrOut, err := Marshal(nonAddrPtrMarshaler{V: "raw"})
	if err != nil {
		t.Fatalf("Marshal non-addressable failed: %v", err)
	}
	if !strings.Contains(string(nonAddrOut), "<V>raw</V>") || strings.Contains(string(nonAddrOut), "PTR:") {
		t.Fatalf("pointer-receiver MarshalXML called on non-addressable value: %s", nonAddrOut)
	}
}

func TestFastSecurityAndValidation(t *testing.T) {
	// Deeply nested XML must trigger depth limit cleanly without stack overflow.
	depth := 10050
	deepXML := strings.Repeat("<a>", depth) + "x" + strings.Repeat("</a>", depth)
	type deepStruct struct {
		A *deepStruct `xml:"a"`
	}
	var ds deepStruct
	if err := Unmarshal([]byte(deepXML), &ds); err == nil {
		t.Fatal("expected max depth error, got nil")
	}

	// Invalid XML character ranges & bad sequences
	badXMLs := []string{
		"<a>\x00</a>",
		"<a>\x1f</a>",
		"<a>" + string(rune(0xFFFE)) + "</a>",
		"<a>" + string(rune(0xFFFF)) + "</a>",
		"<a>\xff\xfe</a>",
		"<a>hello]]>world</a>",
		"<!-- bad -- comment --><a/>",
		`<a attr="unclosed<val"></a>`,
	}
	for _, x := range badXMLs {
		var v string
		if err := Unmarshal([]byte(x), &v); err == nil {
			t.Fatalf("expected syntax error for %q, got nil", x)
		}
	}

	// EscapeText / Marshal sanitization checks: invalid UTF-8 and control chars -> U+FFFD
	var escBuf bytes.Buffer
	rawBad := []byte("ok\x00\x07\xffok\t\n\r]]>")
	if err := EscapeText(&escBuf, rawBad); err != nil {
		t.Fatalf("EscapeText failed: %v", err)
	}
	if !strings.Contains(escBuf.String(), "\uFFFD") || !strings.Contains(escBuf.String(), "]]&gt;") {
		t.Fatalf("EscapeText did not sanitize properly: %q", escBuf.String())
	}

	// Concurrent fast plan building & reuse under -race
	type raceItem struct {
		XMLName Name      `xml:"item"`
		ID      int       `xml:"id,attr"`
		Title   string    `xml:"title"`
		Score   float64   `xml:"score"`
		At      time.Time `xml:"at"`
		Tags    []string  `xml:"tag"`
	}
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			in := raceItem{
				ID:    id,
				Title: "Entry " + strconv.Itoa(id) + " & <more>",
				Score: float64(id) * 1.5,
				At:    time.Date(2026, 1, 1, id%24, 0, 0, 0, time.UTC),
				Tags:  []string{"x", "y"},
			}
			for range 50 {
				b, err := Marshal(&in)
				if err != nil {
					t.Errorf("Marshal err: %v", err)
					return
				}
				var out raceItem
				if err := Unmarshal(b, &out); err != nil {
					t.Errorf("Unmarshal err: %v", err)
					return
				}
				if out.ID != in.ID || out.Title != in.Title || !out.At.Equal(in.At) {
					t.Errorf("race mismatch: got %+v want %+v", out, in)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}

func TestFastEncoderIndentAndTokenParity(t *testing.T) {
	type sub struct {
		Name string `xml:"name"`
		Val  int    `xml:"val,attr"`
	}
	type root struct {
		XMLName Name   `xml:"root"`
		Subs    []sub  `xml:"sub"`
		Note    string `xml:"note,omitempty"`
	}
	in := root{
		Subs: []sub{{Name: "one", Val: 1}, {Name: "two & <three>", Val: 2}},
		Note: "done",
	}

	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	enc.Indent("", "  ")
	if err := enc.Encode(&in); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Verify streaming io.Reader (non-ByteReader) produces identical output via NewDecoder
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write(buf.Bytes())
		_ = pw.Close()
	}()
	var streamed root
	if err := NewDecoder(pr).Decode(&streamed); err != nil {
		t.Fatalf("streaming Decode failed: %v", err)
	}
	want := in
	want.XMLName = Name{Local: "root"}
	if !reflect.DeepEqual(streamed, want) {
		t.Fatalf("streamed mismatch:\ngot  %+v\nwant %+v", streamed, want)
	}
}

type byteChunkReader struct {
	data  []byte
	chunk int
}

func (r *byteChunkReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := min(len(p), r.chunk, len(r.data))
	copy(p[:n], r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

type errAfterNWriter struct {
	buf   bytes.Buffer
	limit int
	err   error
}

func (w *errAfterNWriter) Write(p []byte) (int, error) {
	if w.buf.Len()+len(p) > w.limit {
		return 0, w.err
	}
	return w.buf.Write(p)
}

type namedInnerStr string
type namedInnerBytes []byte

type testTimeAndNamedDoc struct {
	XMLName   Name            `xml:"urn:test doc"`
	AttrTime  time.Time       `xml:"at,attr"`
	PtrTime   *time.Time      `xml:"pt,attr,omitempty"`
	NilTime   *time.Time      `xml:"nt,attr"`
	TimeSlice []time.Time     `xml:"ts,attr"`
	ElemTime  time.Time       `xml:"elemTime"`
	PtrElemT  *time.Time      `xml:"ptrElemTime"`
	NamedIn   namedInnerStr   `xml:",innerxml"`
	NamedRaw  namedInnerBytes `xml:",comment"`
	Nested    struct {
		XMLName Name `xml:""`
		Val     int  `xml:"val"`
	} `xml:"nested"`
}

type failingUnmarshaler struct{}

func (f *failingUnmarshaler) UnmarshalXML(d *Decoder, start StartElement) error {
	return io.ErrUnexpectedEOF
}

func TestFastDifferentialParity(t *testing.T) {
	ts := time.Date(2026, 7, 4, 15, 30, 45, 0, time.UTC)
	ts2 := ts.Add(time.Hour)
	doc := testTimeAndNamedDoc{
		AttrTime:  ts,
		PtrTime:   &ts2,
		TimeSlice: []time.Time{ts, ts2},
		ElemTime:  ts,
		PtrElemT:  &ts2,
		NamedIn:   "wrapped-not-raw",
		NamedRaw:  namedInnerBytes("comment-body-"),
	}
	doc.Nested.Val = 123

	// Compare addressable (&doc) vs non-addressable (doc) vs Encoder.Encode.
	bPtr, err := Marshal(&doc)
	if err != nil {
		t.Fatalf("Marshal(&doc) failed: %v", err)
	}
	bVal, err := Marshal(doc)
	if err != nil {
		t.Fatalf("Marshal(doc) failed: %v", err)
	}
	if !bytes.Equal(bPtr, bVal) {
		t.Fatalf("Marshal(&doc) vs Marshal(doc) mismatch:\n%s\nvs\n%s", bPtr, bVal)
	}
	var encBuf bytes.Buffer
	if err := NewEncoder(&encBuf).Encode(doc); err != nil {
		t.Fatalf("Encoder.Encode(doc) failed: %v", err)
	}
	if !bytes.Equal(bVal, encBuf.Bytes()) {
		t.Fatalf("Encoder.Encode mismatch:\n%s\nvs\n%s", encBuf.Bytes(), bVal)
	}
	if !strings.Contains(string(bVal), "<NamedIn>wrapped-not-raw</NamedIn>") {
		t.Fatalf("named string with ,innerxml must wrap as element, got: %s", bVal)
	}
	if !strings.Contains(string(bVal), "<!--comment-body- -->") {
		t.Fatalf("trailing '-' comment not padded, got: %s", bVal)
	}
	if !strings.Contains(string(bVal), `<nested xmlns="">`) {
		t.Fatalf("nested empty XMLName namespace reset missing, got: %s", bVal)
	}

	var decoded testTimeAndNamedDoc
	if err := Unmarshal(bVal, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if !decoded.AttrTime.Equal(ts) || decoded.PtrTime == nil || !decoded.PtrTime.Equal(ts2) ||
		decoded.NilTime != nil || len(decoded.TimeSlice) != 2 || !decoded.ElemTime.Equal(ts) ||
		decoded.PtrElemT == nil || !decoded.PtrElemT.Equal(ts2) ||
		!strings.Contains(string(decoded.NamedIn), "<NamedIn>wrapped-not-raw</NamedIn>") ||
		string(decoded.NamedRaw) != "comment-body- " || decoded.Nested.Val != 123 {
		t.Fatalf("roundtrip mismatch: got %+v want %+v", decoded, doc)
	}

	// Verify pooled Decoder state isolation across errors and custom Unmarshaler calls.
	for range 20 {
		var bad failingUnmarshaler
		if err := Unmarshal([]byte(`<failingUnmarshaler><x/></failingUnmarshaler>`), &bad); err == nil {
			t.Fatal("expected error from failingUnmarshaler")
		}
		var badSyntax testComplexDoc
		if err := Unmarshal([]byte(`<doc><unclosed>`), &badSyntax); err == nil {
			t.Fatal("expected syntax error")
		}
		var okDoc testTimeAndNamedDoc
		if err := Unmarshal(bVal, &okDoc); err != nil {
			t.Fatalf("pooled Unmarshal after errors failed: %v", err)
		}
		if okDoc.Nested.Val != 123 || !okDoc.AttrTime.Equal(ts) {
			t.Fatalf("pooled Unmarshal state corrupted: %+v", okDoc)
		}
	}
}

func TestFastEscapeTextComprehensive(t *testing.T) {
	inputs := [][]byte{
		{},
		[]byte("plain-ascii-7"),
		[]byte("plain-ascii-16-bytes!"),
		[]byte(strings.Repeat("0123456789abcdef", 20)),
		[]byte(`<a attr="x" alt='y'>&amp;</a>` + "\t\n\r"),
		{0x00, 0x01, 0x08, 0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x1F, 'A', '>', 0x7F},
		[]byte("valid-utf8: café 日本語 🚀"),
		{0xEF, 0xBF, 0xBE, 0xEF, 0xBF, 0xBF, 0xED, 0xA0, 0x80, 0xE0, 0x80, 0x80, 0xF4, 0x90, 0x80, 0x80, 0xFF},
		[]byte(strings.Repeat(`"a"<b&c>'d'`+"\x00\xff", 30)),
	}
	for idx, in := range inputs {
		var buf bytes.Buffer
		if err := EscapeText(&buf, in); err != nil {
			t.Fatalf("case %d bytes.Buffer EscapeText error: %v", idx, err)
		}
		var sb strings.Builder
		if err := EscapeText(&sb, in); err != nil {
			t.Fatalf("case %d strings.Builder EscapeText error: %v", idx, err)
		}
		if buf.String() != sb.String() {
			t.Fatalf("case %d Buffer vs Builder mismatch:\ngot  %q\nwant %q", idx, buf.String(), sb.String())
		}
	}

	wantErr := io.ErrShortWrite
	ew := &errAfterNWriter{limit: 5, err: wantErr}
	if err := EscapeText(ew, []byte("hello<world>")); err != wantErr {
		t.Fatalf("EscapeText IO error = %v, want %v", err, wantErr)
	}
}

func TestFastDecoderStreamingAndChunked(t *testing.T) {
	doc := makeBenchFeedDoc(12)
	raw, err := Marshal(&doc)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	for _, chunkSize := range []int{1, 3, 7, 64} {
		var out benchFeedDoc
		dec := NewDecoder(&byteChunkReader{data: raw, chunk: chunkSize})
		if err := dec.Decode(&out); err != nil {
			t.Fatalf("chunk=%d Decode failed: %v", chunkSize, err)
		}
		if out.ID != doc.ID || out.Title != doc.Title || !out.Updated.Equal(doc.Updated) || len(out.Entries) != len(doc.Entries) {
			t.Fatalf("chunk=%d decoded mismatch", chunkSize)
		}
		for i := range doc.Entries {
			if out.Entries[i].ID != doc.Entries[i].ID ||
				out.Entries[i].Author != doc.Entries[i].Author ||
				!out.Entries[i].Published.Equal(doc.Entries[i].Published) {
				t.Fatalf("chunk=%d entry %d mismatch: got %+v want %+v", chunkSize, i, out.Entries[i], doc.Entries[i])
			}
		}
	}
}

type benchFeedDoc struct {
	XMLName Name             `xml:"feed"`
	ID      string           `xml:"id"`
	Title   string           `xml:"title"`
	Updated time.Time        `xml:"updated"`
	Entries []benchFeedEntry `xml:"entry"`
}

type benchFeedEntry struct {
	ID        int64     `xml:"id,attr"`
	Category  string    `xml:"cat,attr"`
	Title     string    `xml:"title"`
	Author    string    `xml:"author>name"`
	Published time.Time `xml:"published"`
	Score     float64   `xml:"score"`
	Active    bool      `xml:"active"`
	Summary   string    `xml:"summary"`
}

func makeBenchFeedDoc(entries int) benchFeedDoc {
	ts := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	doc := benchFeedDoc{
		ID:      "urn:uuid:60a76c80-d399-11d9-b93C-0003939e0af6",
		Title:   "Engineering Systems Benchmark Feed",
		Updated: ts,
		Entries: make([]benchFeedEntry, entries),
	}
	for i := range doc.Entries {
		doc.Entries[i] = benchFeedEntry{
			ID:        int64(1000 + i),
			Category:  "perf",
			Title:     "Item title " + strconv.Itoa(i) + " with plain ASCII",
			Author:    "Alice Developer",
			Published: ts.Add(time.Duration(i) * time.Minute),
			Score:     98.5 + float64(i)*0.25,
			Active:    i%2 == 0,
			Summary:   "Summary text comparing encoding and decoding throughput across realistic payloads.",
		}
	}
	return doc
}

func BenchmarkFastSuite(b *testing.B) {
	smallDoc := makeBenchFeedDoc(10)
	smallXML, err := Marshal(&smallDoc)
	if err != nil {
		b.Fatal(err)
	}
	largeDoc := makeBenchFeedDoc(100)
	largeXML, err := Marshal(&largeDoc)
	if err != nil {
		b.Fatal(err)
	}
	plainStr := []byte(strings.Repeat("The quick brown fox jumps over the lazy dog. ", 64))
	escStr := []byte(strings.Repeat(`alpha < "beta" & 'gamma' > omega `, 64))

	b.Run("MarshalFeed10", func(b *testing.B) {
		b.SetBytes(int64(len(smallXML)))
		b.ReportAllocs()
		for b.Loop() {
			if _, err := Marshal(&smallDoc); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("MarshalIndentFeed10", func(b *testing.B) {
		b.SetBytes(int64(len(smallXML)))
		b.ReportAllocs()
		for b.Loop() {
			if _, err := MarshalIndent(&smallDoc, "", "  "); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("MarshalFeed100", func(b *testing.B) {
		b.SetBytes(int64(len(largeXML)))
		b.ReportAllocs()
		for b.Loop() {
			if _, err := Marshal(&largeDoc); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("UnmarshalFeed10", func(b *testing.B) {
		b.SetBytes(int64(len(smallXML)))
		b.ReportAllocs()
		for b.Loop() {
			var dst benchFeedDoc
			if err := Unmarshal(smallXML, &dst); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("UnmarshalFeed100", func(b *testing.B) {
		b.SetBytes(int64(len(largeXML)))
		b.ReportAllocs()
		for b.Loop() {
			var dst benchFeedDoc
			if err := Unmarshal(largeXML, &dst); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("EscapeTextPlain", func(b *testing.B) {
		var buf bytes.Buffer
		buf.Grow(len(plainStr) * 2)
		b.SetBytes(int64(len(plainStr)))
		b.ReportAllocs()
		for b.Loop() {
			buf.Reset()
			if err := EscapeText(&buf, plainStr); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("EscapeTextMixed", func(b *testing.B) {
		var buf bytes.Buffer
		buf.Grow(len(escStr) * 2)
		b.SetBytes(int64(len(escStr)))
		b.ReportAllocs()
		for b.Loop() {
			buf.Reset()
			if err := EscapeText(&buf, escStr); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func TestFastMarshalCapNoHeapLeak(t *testing.T) {
	type smallItem struct {
		ID   int    `xml:"id,attr"`
		Name string `xml:"name"`
	}
	for _, n := range []int{1, 7, 33, 129} {
		items := make([]smallItem, n)
		for i := range items {
			items[i] = smallItem{ID: i, Name: "item"}
		}
		doc := struct {
			XMLName Name        `xml:"root"`
			Items   []smallItem `xml:"item"`
		}{Items: items}

		b, err := Marshal(&doc)
		if err != nil {
			t.Fatalf("Marshal(%d) failed: %v", n, err)
		}
		if cap(b) != len(b) {
			t.Fatalf("Marshal(%d): cap(b)=%d != len(b)=%d; trailing heap capacity exposed", n, cap(b), len(b))
		}
		bi, err := MarshalIndent(&doc, "", "  ")
		if err != nil {
			t.Fatalf("MarshalIndent(%d) failed: %v", n, err)
		}
		if cap(bi) != len(bi) {
			t.Fatalf("MarshalIndent(%d): cap(bi)=%d != len(bi)=%d; trailing heap capacity exposed", n, cap(bi), len(bi))
		}
	}
}

func TestFastIntParseBoundaries(t *testing.T) {
	type intDoc struct {
		XMLName Name   `xml:"n"`
		AI8     int8   `xml:"ai8,attr"`
		AI16    int16  `xml:"ai16,attr"`
		AI32    int32  `xml:"ai32,attr"`
		AI64    int64  `xml:"ai64,attr"`
		AU8     uint8  `xml:"au8,attr"`
		AU16    uint16 `xml:"au16,attr"`
		AU32    uint32 `xml:"au32,attr"`
		AU64    uint64 `xml:"au64,attr"`
		I8      int8   `xml:"i8"`
		I64     int64  `xml:"i64"`
		U64     uint64 `xml:"u64"`
	}
	raw := `<n ai8="-128" ai16="32767" ai32="-2147483648" ai64="9223372036854775807" au8="255" au16="65535" au32="4294967295" au64="18446744073709551615"><i8>+127</i8><i64>-9223372036854775808</i64><u64>10000000000000000000</u64></n>`
	var got intDoc
	if err := Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("Unmarshal valid bounds failed: %v", err)
	}
	want := intDoc{
		XMLName: Name{Local: "n"},
		AI8:     -128,
		AI16:    32767,
		AI32:    -2147483648,
		AI64:    9223372036854775807,
		AU8:     255,
		AU16:    65535,
		AU32:    4294967295,
		AU64:    18446744073709551615,
		I8:      127,
		I64:     -9223372036854775808,
		U64:     10000000000000000000,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bounds mismatch: got %+v want %+v", got, want)
	}

	invalidCases := []string{
		`<n ai8="128"/>`,
		`<n ai8="-129"/>`,
		`<n ai64="9223372036854775808"/>`,
		`<n ai64="-9223372036854775809"/>`,
		`<n au8="256"/>`,
		`<n au8="-1"/>`,
		`<n au64="18446744073709551616"/>`,
		`<n><i8>12x</i8></n>`,
		`<n><u64>-</u64></n>`,
		`<n><i64>+</i64></n>`,
		`<n><i64>abc</i64></n>`,
	}
	for _, tc := range invalidCases {
		var bad intDoc
		if err := Unmarshal([]byte(tc), &bad); err == nil {
			t.Fatalf("expected error for %s, got %+v", tc, bad)
		}
	}
	// Empty scalar element sets zero value without error per encoding/xml semantics.
	zeroDoc := intDoc{I64: 99, U64: 88}
	if err := Unmarshal([]byte(`<n><i64></i64><u64></u64></n>`), &zeroDoc); err != nil {
		t.Fatalf("Unmarshal empty scalar element failed: %v", err)
	}
	if zeroDoc.I64 != 0 || zeroDoc.U64 != 0 {
		t.Fatalf("empty scalar elements not reset to 0: got %+v", zeroDoc)
	}
}

func TestFastAttrRawAndStreamingTokens(t *testing.T) {
	// Exercise raw attr bytes, entities, newlines inside large >32B strings, and InputPos line tracking.
	longText := strings.Repeat("a", 31) + "\t" + strings.Repeat("b", 32) + "\n" + strings.Repeat("c", 33) + "&amp;end"
	longAttr := strings.Repeat("x", 35) + "\n" + strings.Repeat("y", 35) + "&lt;1&gt;"
	docXML := `<root id="42" ok="true" f="3.25" tm="2026-07-04T15:30:45Z" big="` + longAttr + `">` +
		`<val>` + longText + `</val><empty/><nilbytes></nilbytes>` +
		`</root>`

	type streamDoc struct {
		ID       int       `xml:"id,attr"`
		OK       bool      `xml:"ok,attr"`
		F        float64   `xml:"f,attr"`
		TM       time.Time `xml:"tm,attr"`
		Big      string    `xml:"big,attr"`
		Val      string    `xml:"val"`
		Empty    []byte    `xml:"empty"`
		Missing  []byte    `xml:"missing"`
		NilBytes []byte    `xml:"nilbytes"`
	}
	var got streamDoc
	if err := Unmarshal([]byte(docXML), &got); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	wantAttr := strings.Repeat("x", 35) + "\n" + strings.Repeat("y", 35) + "<1>"
	wantText := strings.Repeat("a", 31) + "\t" + strings.Repeat("b", 32) + "\n" + strings.Repeat("c", 33) + "&end"
	if got.ID != 42 || !got.OK || got.F != 3.25 || got.TM.Year() != 2026 ||
		got.Big != wantAttr || got.Val != wantText ||
		got.Empty == nil || len(got.Empty) != 0 ||
		got.Missing != nil || got.NilBytes == nil || len(got.NilBytes) != 0 {
		t.Fatalf("decoded mismatch: %+v", got)
	}

	// Verify Token() API line tracking across >32-byte SIMD CharData and Attr cloning.
	dec := NewDecoder(strings.NewReader(docXML))
	var attrs []Attr
	var lineAfterVal int
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Token() error: %v", err)
		}
		switch tok := tok.(type) {
		case StartElement:
			if tok.Name.Local == "root" {
				attrs = tok.Copy().Attr
			}
		case EndElement:
			if tok.Name.Local == "val" {
				lineAfterVal, _ = dec.InputPos()
			}
		}
	}
	if lineAfterVal != 3 {
		t.Fatalf("InputPos line after multiline CharData/Attr = %d, want 3", lineAfterVal)
	}
	if len(attrs) != 5 || attrs[0].Value != "42" || attrs[4].Value != wantAttr {
		t.Fatalf("Token().Copy().Attr mismatch: %+v", attrs)
	}

	// Verify Strict=false with HTMLAutoClose and DefaultSpace.
	htmlInput := `<p>hello<br>world</p>`
	decHTML := NewDecoder(strings.NewReader(htmlInput))
	decHTML.Strict = false
	decHTML.AutoClose = HTMLAutoClose
	decHTML.DefaultSpace = "urn:html"
	var p struct {
		XMLName Name   `xml:"urn:html p"`
		Text    string `xml:",chardata"`
		Br      string `xml:"urn:html br"`
	}
	if err := decHTML.Decode(&p); err != nil {
		t.Fatalf("HTML AutoClose decode failed: %v", err)
	}
	if !strings.Contains(p.Text, "hello") {
		t.Fatalf("unexpected p.Text: %q", p.Text)
	}
}

type customEscapeWriter struct {
	buf bytes.Buffer
}

func (w *customEscapeWriter) Write(p []byte) (int, error) {
	return w.buf.Write(p)
}

func TestFastEscapeTextWritersAndBoundaries(t *testing.T) {
	specials := []byte{'<', '>', '&', '\'', '"', '\n', '\r', '\t', 0x00, 0x1f, 0xff}
	for size := 0; size <= 160; size++ {
		for _, sp := range specials {
			src := bytes.Repeat([]byte{'A'}, size)
			for _, pos := range []int{0, size / 2, size - 1} {
				if pos >= 0 && pos < len(src) {
					src[pos] = sp
				}
			}
			var ref customEscapeWriter
			if err := EscapeText(&ref, src); err != nil {
				t.Fatalf("EscapeText(customWriter) size=%d sp=%x error: %v", size, sp, err)
			}
			var bb bytes.Buffer
			if err := EscapeText(&bb, src); err != nil {
				t.Fatalf("EscapeText(bytes.Buffer) size=%d sp=%x error: %v", size, sp, err)
			}
			var sb strings.Builder
			if err := EscapeText(&sb, src); err != nil {
				t.Fatalf("EscapeText(strings.Builder) size=%d sp=%x error: %v", size, sp, err)
			}
			if !bytes.Equal(ref.buf.Bytes(), bb.Bytes()) || sb.String() != ref.buf.String() {
				t.Fatalf("EscapeText writer mismatch size=%d sp=%x:\nref=%q\nbb =%q\nsb =%q",
					size, sp, ref.buf.Bytes(), bb.Bytes(), sb.String())
			}
		}
	}
	// Test "]]>" spanning across 32-byte boundaries.
	cdataEnd := strings.Repeat("Z", 30) + "]]>" + strings.Repeat("Z", 35) + "]]>"
	var sb strings.Builder
	var cw customEscapeWriter
	if err := EscapeText(&sb, []byte(cdataEnd)); err != nil {
		t.Fatal(err)
	}
	if err := EscapeText(&cw, []byte(cdataEnd)); err != nil {
		t.Fatal(err)
	}
	if sb.String() != cw.buf.String() || !strings.Contains(sb.String(), "]]&gt;") {
		t.Fatalf("EscapeText ]]> mismatch: %q vs %q", sb.String(), cw.buf.String())
	}
}

func TestFastMixedPathsAndNilTextAttr(t *testing.T) {
	type mixedDoc struct {
		XMLName  Name           `xml:"root"`
		Flat     string         `xml:"flat"`
		DeepA    string         `xml:"a>b>c"`
		DeepB    int            `xml:"a>b>d"`
		Conflict string         `xml:"a>x"`
		TextAttr *customTextTag `xml:"ta,attr"`
		NilTime  *time.Time     `xml:"nt,attr"`
	}
	xmlData := `<root ta="[tag]"><flat>ok</flat><a><b><c>deep</c><d>77</d></b><x>cx</x></a></root>`
	var decoded mixedDoc
	if err := Unmarshal([]byte(xmlData), &decoded); err != nil {
		t.Fatalf("Unmarshal mixed paths failed: %v", err)
	}
	if decoded.Flat != "ok" || decoded.DeepA != "deep" || decoded.DeepB != 77 || decoded.Conflict != "cx" ||
		decoded.TextAttr == nil || *decoded.TextAttr != "tag" || decoded.NilTime != nil {
		t.Fatalf("mixed paths mismatch: %+v", decoded)
	}

	bMarshal, err := Marshal(&decoded)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	var encBuf bytes.Buffer
	if err := NewEncoder(&encBuf).Encode(&decoded); err != nil {
		t.Fatalf("Encoder.Encode failed: %v", err)
	}
	if !bytes.Equal(bMarshal, encBuf.Bytes()) {
		t.Fatalf("Marshal vs Encoder.Encode mismatch:\n%s\nvs\n%s", bMarshal, encBuf.Bytes())
	}
}

func TestFastAttrPrefixScoping(t *testing.T) {
	type ptrAttrChild struct {
		A    *Attr  `xml:",attr"`
		Any  any    `xml:",attr"`
		Val  string `xml:",chardata"`
	}
	type doc struct {
		XMLName Name           `xml:"root"`
		First   ptrAttrChild   `xml:"first"`
		Second  string         `xml:"urn:example:ns second"`
	}
	a1 := Attr{Name: Name{Space: "urn:example:ns", Local: "k"}, Value: "v1"}
	a2 := Attr{Name: Name{Space: "urn:example:other", Local: "m"}, Value: "v2"}
	d := doc{
		First:  ptrAttrChild{A: &a1, Any: a2, Val: "ok"},
		Second: "s",
	}
	out, err := Marshal(&d)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	var got doc
	if err := Unmarshal(out, &got); err != nil {
		t.Fatalf("Unmarshal failed (%s): %v", out, err)
	}
	if got.Second != "s" {
		t.Fatalf("got Second=%q, want %q; marshaled=%s", got.Second, "s", out)
	}
	// Prefix bound on <first> must not leak onto sibling <second>.
	if bytes.Contains(out, []byte(`<ns:`)) {
		t.Fatalf("namespace prefix leaked outside <first>: %s", out)
	}
}

type smallChunkReader struct {
	data []byte
	step int
}

func (r *smallChunkReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := min(len(p), r.step)
	n = min(n, len(r.data))
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

func TestFastSimpleLeafEdgeCases(t *testing.T) {
	type leafDoc struct {
		XMLName Name   `xml:"root"`
		Empty   string `xml:"a"`
		Multi   string `xml:"b"`
		Esc     string `xml:"c"`
		Comment string `xml:"d"`
		Space   string `xml:"e"`
		NS      string `xml:"f"`
	}
	input := "<root><a/><b>line1\n\tline2</b><c>x&amp;y&#34;z</c><d>pre<!--skip-->post</d><e>ok</e ><f xmlns=\"urn:u\">ns</f></root>"
	for _, step := range []int{len(input), 1, 5, 17} {
		dec := NewDecoder(&smallChunkReader{data: []byte(input), step: step})
		var got leafDoc
		if err := dec.Decode(&got); err != nil {
			t.Fatalf("step=%d Decode failed: %v", step, err)
		}
		if got.Empty != "" || got.Multi != "line1\n\tline2" || got.Esc != `x&y"z` ||
			got.Comment != "prepost" || got.Space != "ok" || got.NS != "ns" {
			t.Fatalf("step=%d decoded mismatch: %+v", step, got)
		}
		line, col := dec.InputPos()
		if line != 2 || col < 10 {
			t.Fatalf("step=%d InputPos() = (%d, %d), want line=2, col>=10", step, line, col)
		}
	}

	// Nested element inside scalar string leaf must fall back and skip child cleanly.
	var withChild leafDoc
	if err := Unmarshal([]byte(`<root><b>abc<nested/>def</b></root>`), &withChild); err != nil {
		t.Fatalf("unexpected error for child element in scalar leaf: %v", err)
	}
	if withChild.Multi != "abcdef" {
		t.Fatalf("got Multi=%q, want %q", withChild.Multi, "abcdef")
	}

	// Mismatched close tag and unescaped ]]> inside leaf must error.
	for _, bad := range []string{
		`<root><b>abc</x></root>`,
		`<root><b>abc]]>def</b></root>`,
	} {
		var got leafDoc
		if err := Unmarshal([]byte(bad), &got); err == nil {
			t.Fatalf("expected error for malformed leaf XML %q", bad)
		}
	}
}

func TestFastMarshalIndentParentPaths(t *testing.T) {
	type nested struct {
		XMLName Name   `xml:"root"`
		Leaf1   string `xml:"a>b>c"`
		Leaf2   int    `xml:"a>b>d"`
		Leaf3   string `xml:"a>e"`
	}
	in := nested{Leaf1: "hello", Leaf2: 42, Leaf3: "world"}
	got, err := MarshalIndent(&in, ">", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent failed: %v", err)
	}
	want := "><root>\n>  <a>\n>    <b>\n>      <c>hello</c>\n>      <d>42</d>\n>    </b>\n>    <e>world</e>\n>  </a>\n></root>"
	if string(got) != want {
		t.Fatalf("MarshalIndent parent paths:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestFastEscapeTextQuotesAndControl(t *testing.T) {
	in := []byte("plain\"single'amp&lt<gt>tab\tnl\ncr\rnul\x00bad\xffend")
	want := "plain&#34;single&#39;amp&amp;lt&lt;gt&gt;tab&#x9;nl&#xA;cr&#xD;nul\uFFFDbad\uFFFDend"

	var bb bytes.Buffer
	if err := EscapeText(&bb, in); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	if err := EscapeText(&sb, in); err != nil {
		t.Fatal(err)
	}
	var cw customEscapeWriter
	if err := EscapeText(&cw, in); err != nil {
		t.Fatal(err)
	}
	if bb.String() != want || sb.String() != want || cw.buf.String() != want {
		t.Fatalf("EscapeText mismatch:\nwant=%q\nbb  =%q\nsb  =%q\ncw  =%q",
			want, bb.String(), sb.String(), cw.buf.String())
	}
}

