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
	XMLName   Name              `xml:"doc"`
	ID        int64             `xml:"id,attr"`
	Flag      bool              `xml:"flag,attr,omitempty"`
	Tags      []string          `xml:"tag,attr,omitempty"`
	PtrCode   *testEmbeddedAttr `xml:",omitempty"`
	Count     uint32            `xml:"count"`
	Ratio     float64           `xml:"ratio"`
	Created   time.Time         `xml:"created"`
	Custom    customTextTag     `xml:"custom"`
	Raw       []byte            `xml:"raw"`
	EmptyRaw  []byte            `xml:"emptyRaw"`
	PathVal   string            `xml:"a>b>c"`
	Items     []string          `xml:"item"`
	Unknown   []string          `xml:",any"`
	Comment   string            `xml:",comment"`
	Inner     string            `xml:",innerxml"`
}

func TestFastMarshalUnmarshalEdgeCases(t *testing.T) {
	ts := time.Date(2026, 3, 15, 10, 20, 30, 0, time.UTC)
	in := testComplexDoc{
		ID:      42,
		Flag:    true,
		Tags:    []string{"alpha", "beta"},
		PtrCode: &testEmbeddedAttr{Code: 7},
		Count:   999,
		Ratio:   3.25,
		Created: ts,
		Custom:  "hello",
		Raw:     []byte("<escaped & \"quoted\" 'attr'>"),
		EmptyRaw: []byte{},
		PathVal: "nested-leaf",
		Items:   []string{"first", "second"},
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
