// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cache

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestPutExecutablePathTraversal(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.NewReader([]byte("#!/bin/sh\necho ok\n"))
	badNames := []string{".", "..", "../escape", "sub/bin", `sub\bin`, "bad\x00name"}
	for _, name := range badNames {
		if _, _, err := c.PutExecutable(dummyID(1), name, payload); err == nil {
			t.Fatalf("PutExecutable(%q) unexpectedly succeeded", name)
		}
		if _, err := payload.Seek(0, 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape")); !os.IsNotExist(err) {
		t.Fatalf("traversal created file outside cache: %v", err)
	}
}

func TestGetBytesMissingZeroByte(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := ActionID(dummyID(42))
	if err := PutBytes(c, id, nil); err != nil {
		t.Fatal(err)
	}
	if data, _, err := GetBytes(c, id); err != nil || len(data) != 0 {
		t.Fatalf("initial GetBytes(empty) = %v, %v", len(data), err)
	}

	// Remove the pack file holding the 0-byte payload; GetBytes must miss, not falsely hit.
	out := sha256.Sum256(nil)
	if err := os.Remove(c.packPath(out[0])); err != nil {
		t.Fatal(err)
	}
	if _, _, err := GetBytes(c, id); err == nil {
		t.Fatal("GetBytes succeeded after deleting backing 0-byte payload")
	}
}

func TestPackRoutingAndMaterialization(t *testing.T) {
	skipIfLockUnsupported(t)

	dir := t.TempDir()
	c, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Small non-archive blob goes into pack; GetBytes/GetMmap read without materializing -d.
	idSmall := ActionID(dummyID(10))
	small := bytes.Repeat([]byte("hello-pack-"), 100)
	if err := PutBytes(c, idSmall, small); err != nil {
		t.Fatal(err)
	}
	eSmall, err := c.Get(idSmall)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.fileName(idSmall, "a")); !os.IsNotExist(err) {
		t.Fatalf("unexpected -a file created: %v", err)
	}
	if _, err := os.Stat(c.fileName(eSmall.OutputID, "d")); !os.IsNotExist(err) {
		t.Fatalf("unexpected -d file created for packed entry: %v", err)
	}
	if got, _, err := GetBytes(c, idSmall); err != nil || !bytes.Equal(got, small) {
		t.Fatalf("GetBytes mismatch: err=%v len=%d", err, len(got))
	}
	if got, _, opened, err := GetMmap(c, idSmall); err != nil || opened || !bytes.Equal(got, small) {
		t.Fatalf("GetMmap = err:%v opened:%v len:%d", err, opened, len(got))
	}

	// OutputFile lazily materializes -d and removes from pack.
	outPath := c.OutputFile(eSmall.OutputID)
	if got, err := os.ReadFile(outPath); err != nil || !bytes.Equal(got, small) {
		t.Fatalf("materialized file mismatch: err=%v", err)
	}
	if _, found, err := c.getPackedBytes(eSmall.OutputID, eSmall.Size); err != nil || found {
		t.Fatalf("expected materialized entry to be removed from pack, found=%v err=%v", found, err)
	}

	// 2. Small archive (starts with !<arch>\n) stays standalone -d.
	idAr := ActionID(dummyID(11))
	arData := append([]byte("!<arch>\n"), bytes.Repeat([]byte{'x'}, 128)...)
	if err := PutBytes(c, idAr, arData); err != nil {
		t.Fatal(err)
	}
	eAr, err := c.Get(idAr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.fileName(eAr.OutputID, "d")); err != nil {
		t.Fatalf("archive should be stored as standalone -d: %v", err)
	}

	// 3. Blob > maxPackPayloadSize stays standalone -d.
	idBig := ActionID(dummyID(12))
	bigData := bytes.Repeat([]byte("Z"), maxPackPayloadSize+1)
	if err := PutBytes(c, idBig, bigData); err != nil {
		t.Fatal(err)
	}
	eBig, err := c.Get(idBig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.fileName(eBig.OutputID, "d")); err != nil {
		t.Fatalf("large blob should be stored as standalone -d: %v", err)
	}

	// 4. Streaming *os.File (> 64KB) works in single pass.
	bigStreamPath := filepath.Join(t.TempDir(), "stream.bin")
	streamData := bytes.Repeat([]byte("stream-data-block-"), 6000) // ~108 KB
	if err := os.WriteFile(bigStreamPath, streamData, 0o644); err != nil {
		t.Fatal(err)
	}
	sf, err := os.Open(bigStreamPath)
	if err != nil {
		t.Fatal(err)
	}
	defer sf.Close()
	idStream := ActionID(dummyID(13))
	if _, _, err := c.Put(idStream, sf); err != nil {
		t.Fatal(err)
	}
	if got, _, err := GetBytes(c, idStream); err != nil || !bytes.Equal(got, streamData) {
		t.Fatalf("streamed Put/GetBytes mismatch: err=%v len=%d", err, len(got))
	}
}

func TestLegacyEntryFallback(t *testing.T) {
	skipIfLockUnsupported(t)

	dir := t.TempDir()
	c, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := ActionID(dummyID(99))
	payload := []byte("legacy-payload-bytes")
	out := sha256.Sum256(payload)
	entry := fmt.Sprintf("v1 %x %x %20d %20d\n", id, out, len(payload), time.Now().UnixNano())
	if err := os.WriteFile(c.fileName(id, "a"), []byte(entry), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.fileName(out, "d"), payload, 0o666); err != nil {
		t.Fatal(err)
	}
	got, _, err := GetBytes(c, id)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("legacy GetBytes = %q, %v; want %q", got, err, payload)
	}
}

func TestIdxAndPackCorruptionRecovery(t *testing.T) {
	skipIfLockUnsupported(t)

	dir := t.TempDir()
	c, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	id1 := ActionID(dummyID(1))
	data1 := []byte("valid-entry-one")
	if err := PutBytes(c, id1, data1); err != nil {
		t.Fatal(err)
	}
	e1, err := c.Get(id1)
	if err != nil {
		t.Fatal(err)
	}

	// Append torn partial garbage to idx and pack; valid entry 1 must still read,
	// and writing entry 2 must truncate the torn tail cleanly.
	idxPath := c.idxPath(id1[0])
	idxData, err := os.ReadFile(idxPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(idxPath, append(idxData, []byte("torn-tail-bytes")...), 0o666); err != nil {
		t.Fatal(err)
	}

	packPath := c.packPath(e1.OutputID[0])
	packData, err := os.ReadFile(packPath)
	if err != nil {
		t.Fatal(err)
	}
	// Append a fake header claiming PayloadSize > maxPackPayloadSize (must be ignored safely).
	var badHdr [packHeaderSize]byte
	copy(badHdr[0:4], packMagic)
	binary.LittleEndian.PutUint32(badHdr[36:40], maxPackPayloadSize+999)
	if err := os.WriteFile(packPath, append(packData, badHdr[:]...), 0o666); err != nil {
		t.Fatal(err)
	}

	if got, _, err := GetBytes(c, id1); err != nil || !bytes.Equal(got, data1) {
		t.Fatalf("GetBytes after torn tail = %q, %v", got, err)
	}

	// Write a second entry sharing the same bucket bytes (id2[0] == id1[0]).
	var id2 ActionID
	id2[0] = id1[0]
	id2[1] = 2
	data2 := []byte("valid-entry-two")
	if err := PutBytes(c, id2, data2); err != nil {
		t.Fatal(err)
	}
	if got, _, err := GetBytes(c, id2); err != nil || !bytes.Equal(got, data2) {
		t.Fatalf("GetBytes(id2) after recovery = %q, %v", got, err)
	}

	// Corrupt CRC in idx record for id1; Get must return miss.
	idxData, err = os.ReadFile(idxPath)
	if err != nil {
		t.Fatal(err)
	}
	idxData[idxHeaderSize+idxRecSize-1] ^= 0xff
	if err := os.WriteFile(idxPath, idxData, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(id1); err == nil {
		t.Fatal("Get succeeded on CRC-corrupted idx record")
	}
	if _, err := c.Get(id2); err != nil {
		t.Fatalf("Get(id2) failed after id1 corruption: %v", err)
	}
}

func TestPackConcurrency(t *testing.T) {
	skipIfLockUnsupported(t)

	dir := t.TempDir()
	c, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	const workers = 12
	const iters = 30
	var wg sync.WaitGroup
	errs := make(chan error, workers*iters)

	for w := range workers {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := range iters {
				var id ActionID
				id[0] = byte((worker + i) % 4) // force heavy bucket contention
				binary.LittleEndian.PutUint32(id[4:], uint32(worker*1000+i))
				val := bytes.Repeat([]byte{byte('a' + (worker+i)%26)}, 100+(i*13)%500)
				if err := PutBytes(c, id, val); err != nil {
					errs <- fmt.Errorf("PutBytes: %w", err)
					return
				}
				got, _, err := GetBytes(c, id)
				if err != nil || !bytes.Equal(got, val) {
					errs <- fmt.Errorf("GetBytes mismatch: err=%v", err)
					return
				}
				if i%7 == 0 {
					e, err := c.Get(id)
					if err != nil {
						errs <- err
						return
					}
					if _, err := os.ReadFile(c.OutputFile(e.OutputID)); err != nil {
						errs <- fmt.Errorf("OutputFile read: %w", err)
						return
					}
				}
				if i == iters/2 && worker == 0 {
					_ = c.Trim()
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}
