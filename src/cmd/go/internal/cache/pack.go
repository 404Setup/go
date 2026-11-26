// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cache

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"cmd/go/internal/lockedfile"
)

const (
	idxMagic      = "GOIDX01\n"
	idxHeaderSize = 16
	idxRecSize    = 96

	packFileMagic      = "GOPACK1\n"
	packFileHeaderSize = 16
	packMagic          = "GPK1"
	packHeaderSize     = 60
	maxPackPayloadSize = 16 << 10 // 16 KiB
)

var hexByte [256]string

func init() {
	for i := range 256 {
		hexByte[i] = fmt.Sprintf("%02x", i)
	}
}

func (c *DiskCache) idxPath(b byte) string {
	return filepath.Join(c.dir, hexByte[b], "idx")
}

func (c *DiskCache) packPath(b byte) string {
	return filepath.Join(c.dir, hexByte[b], "pack")
}

func isArchivePrefix(b []byte) bool {
	return len(b) >= 8 && string(b[:8]) == "!<arch>\n"
}

func writeAllAt(f *lockedfile.File, p []byte, off int64) error {
	for len(p) > 0 {
		n, err := f.WriteAt(p, off)
		if err != nil {
			return err
		}
		p = p[n:]
		off += int64(n)
	}
	return nil
}

func (c *DiskCache) shouldMarkUsed(used time.Time) (time.Time, bool) {
	interval := mtimeInterval
	if c.trimBuildCache && isYoungGen(used) {
		interval = usedAfter30m
	}
	if now := c.now(); now.Sub(used) >= interval {
		if c.trimBuildCache {
			return now.Truncate(time.Minute).Add(-30 * time.Second), true
		}
		return now, true
	}
	return time.Time{}, false
}

func (c *DiskCache) rewriteUsedTime(prevUsed time.Time) time.Time {
	mark := c.writeTime()
	if c.trimBuildCache {
		tenured := mark.Add(-30 * time.Second)
		switch {
		case isYoungGen(prevUsed):
			if mark.Sub(prevUsed) >= usedAfter30m {
				mark = tenured
			} else if !prevUsed.After(mark) {
				mark = prevUsed
			}
		case !prevUsed.After(tenured):
			mark = tenured
		}
	}
	return mark
}

func (c *DiskCache) refreshEntryTime(used time.Time) (time.Time, bool) {
	if isYoungGen(used) {
		if now := c.now(); now.Sub(used) >= mtimeInterval {
			return now.Truncate(time.Minute), true
		}
		return time.Time{}, false
	}
	return c.shouldMarkUsed(used)
}

func encodeIdxHeader(dst []byte, gen uint64) {
	copy(dst[:8], idxMagic)
	binary.LittleEndian.PutUint64(dst[8:16], gen)
}

func encodeIdxRecord(dst []byte, id ActionID, out OutputID, size, tm, used int64, flags uint32) {
	copy(dst[0:32], id[:])
	copy(dst[32:64], out[:])
	binary.LittleEndian.PutUint64(dst[64:72], uint64(size))
	binary.LittleEndian.PutUint64(dst[72:80], uint64(tm))
	binary.LittleEndian.PutUint64(dst[80:88], uint64(used))
	binary.LittleEndian.PutUint32(dst[88:92], flags)
	binary.LittleEndian.PutUint32(dst[92:96], crc32.ChecksumIEEE(dst[:92]))
}

func decodeIdxRecord(src []byte) (id ActionID, out OutputID, size, tm, used int64, flags uint32, ok bool) {
	if len(src) != idxRecSize {
		return ActionID{}, OutputID{}, 0, 0, 0, 0, false
	}
	if crc32.ChecksumIEEE(src[:92]) != binary.LittleEndian.Uint32(src[92:96]) {
		return ActionID{}, OutputID{}, 0, 0, 0, 0, false
	}
	size = int64(binary.LittleEndian.Uint64(src[64:72]))
	tm = int64(binary.LittleEndian.Uint64(src[72:80]))
	used = int64(binary.LittleEndian.Uint64(src[80:88]))
	flags = binary.LittleEndian.Uint32(src[88:92])
	if size < 0 || tm < 0 || used < 0 {
		return ActionID{}, OutputID{}, 0, 0, 0, 0, false
	}
	copy(id[:], src[0:32])
	copy(out[:], src[32:64])
	return id, out, size, tm, used, flags, true
}

func parseIdxHeader(data []byte) (gen uint64, validLen int, ok bool) {
	if len(data) < idxHeaderSize || string(data[:8]) != idxMagic {
		return 0, 0, false
	}
	gen = binary.LittleEndian.Uint64(data[8:16])
	validLen = idxHeaderSize + ((len(data)-idxHeaderSize)/idxRecSize)*idxRecSize
	return gen, validLen, true
}

func (c *DiskCache) getIdxEntry(id ActionID) (entry Entry, found bool, unsupported bool) {
	b := id[0]
	c.idxMu[b].RLock()
	f, err := lockedfile.Open(c.idxPath(b))
	if err != nil {
		c.idxMu[b].RUnlock()
		if errors.Is(err, errors.ErrUnsupported) {
			return Entry{}, false, true
		}
		return Entry{}, false, false
	}
	data, readErr := io.ReadAll(f)
	f.Close()
	c.idxMu[b].RUnlock()
	if readErr != nil {
		return Entry{}, false, false
	}
	_, validLen, ok := parseIdxHeader(data)
	if !ok {
		return Entry{}, false, false
	}
	for off := validLen - idxRecSize; off >= idxHeaderSize; off -= idxRecSize {
		rec := data[off : off+idxRecSize]
		if !bytes.Equal(rec[0:32], id[:]) {
			continue
		}
		_, out, size, tm, usedNano, _, recOK := decodeIdxRecord(rec)
		if !recOK {
			continue
		}
		if _, needUpdate := c.shouldMarkUsed(time.Unix(0, usedNano)); needUpdate {
			c.touchIdxTime(id, false)
		}
		c.noteUsed(id, 'a')
		c.noteUsed(out, 'd')
		return Entry{
			OutputID: out,
			Size:     size,
			Time:     time.Unix(0, tm),
		}, true, false
	}
	return Entry{}, false, false
}

func (c *DiskCache) touchIdxTime(id ActionID, keepYoung bool) {
	b := id[0]
	c.idxMu[b].Lock()
	defer c.idxMu[b].Unlock()

	f, err := lockedfile.Edit(c.idxPath(b))
	if err != nil {
		return
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return
	}
	gen, validLen, ok := parseIdxHeader(data)
	if !ok {
		return
	}
	for off := validLen - idxRecSize; off >= idxHeaderSize; off -= idxRecSize {
		rec := data[off : off+idxRecSize]
		if !bytes.Equal(rec[0:32], id[:]) {
			continue
		}
		_, out, size, tm, usedNano, flags, recOK := decodeIdxRecord(rec)
		if !recOK {
			continue
		}
		used := time.Unix(0, usedNano)
		var newUsed time.Time
		var need bool
		if keepYoung {
			newUsed, need = c.refreshEntryTime(used)
		} else {
			newUsed, need = c.shouldMarkUsed(used)
		}
		if !need {
			return
		}
		var updated [idxRecSize]byte
		encodeIdxRecord(updated[:], id, out, size, tm, newUsed.UnixNano(), flags)
		if err := writeAllAt(f, updated[:], int64(off)); err != nil {
			return
		}
		var genBuf [8]byte
		binary.LittleEndian.PutUint64(genBuf[:], gen+1)
		_ = writeAllAt(f, genBuf[:], 8)
		return
	}
}

func (c *DiskCache) putIdxEntry(id ActionID, out OutputID, size int64) (unsupported bool, err error) {
	b := id[0]
	c.idxMu[b].Lock()
	defer c.idxMu[b].Unlock()

	f, err := lockedfile.Edit(c.idxPath(b))
	if err != nil {
		if errors.Is(err, errors.ErrUnsupported) {
			return true, nil
		}
		return false, err
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return false, err
	}
	tm := time.Now().UnixNano()
	mark := c.writeTime()

	gen, validLen, ok := parseIdxHeader(data)
	if !ok {
		// New or corrupt idx file: initialize fresh header + first record.
		var buf [idxHeaderSize + idxRecSize]byte
		encodeIdxHeader(buf[:idxHeaderSize], 1)
		encodeIdxRecord(buf[idxHeaderSize:], id, out, size, tm, mark.UnixNano(), 0)
		if err := writeAllAt(f, buf[:], 0); err != nil {
			return false, err
		}
		if len(data) > len(buf) {
			if err := f.Truncate(int64(len(buf))); err != nil {
				return false, err
			}
		}
		c.addBytes(idxRecSize)
		c.noteUsed(id, 'a')
		c.noteUsed(out, 'd')
		return false, nil
	}

	// Scan existing records for an in-place update.
	for off := idxHeaderSize; off < validLen; off += idxRecSize {
		rec := data[off : off+idxRecSize]
		if !bytes.Equal(rec[0:32], id[:]) {
			continue
		}
		_, _, _, _, oldUsedNano, _, recOK := decodeIdxRecord(rec)
		if recOK && c.trimBuildCache {
			mark = c.rewriteUsedTime(time.Unix(0, oldUsedNano))
		}
		var updated [idxRecSize]byte
		encodeIdxRecord(updated[:], id, out, size, tm, mark.UnixNano(), 0)
		if err := writeAllAt(f, updated[:], int64(off)); err != nil {
			return false, err
		}
		var genBuf [8]byte
		binary.LittleEndian.PutUint64(genBuf[:], gen+1)
		_ = writeAllAt(f, genBuf[:], 8)
		if validLen != len(data) {
			_ = f.Truncate(int64(validLen))
		}
		c.noteUsed(id, 'a')
		c.noteUsed(out, 'd')
		return false, nil
	}

	// Not found: append new record at validLen.
	var rec [idxRecSize]byte
	encodeIdxRecord(rec[:], id, out, size, tm, mark.UnixNano(), 0)
	if err := writeAllAt(f, rec[:], int64(validLen)); err != nil {
		return false, err
	}
	var genBuf [8]byte
	binary.LittleEndian.PutUint64(genBuf[:], gen+1)
	if err := writeAllAt(f, genBuf[:], 8); err != nil {
		return false, err
	}
	newLen := int64(validLen + idxRecSize)
	if int64(len(data)) > newLen {
		_ = f.Truncate(newLen)
	}
	c.addBytes(idxRecSize)
	c.noteUsed(id, 'a')
	c.noteUsed(out, 'd')
	return false, nil
}

func (c *DiskCache) trimIdxBucket(b byte, classify func(mt time.Time) (remove, inBase bool), stats *trimStats) {
	idxFile := c.idxPath(b)
	if _, err := os.Stat(idxFile); err != nil {
		return
	}
	c.idxMu[b].Lock()
	defer c.idxMu[b].Unlock()

	f, err := lockedfile.Edit(idxFile)
	if err != nil {
		return
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return
	}
	gen, validLen, ok := parseIdxHeader(data)
	if !ok {
		if len(data) > 0 {
			var hdr [idxHeaderSize]byte
			encodeIdxHeader(hdr[:], 1)
			_ = writeAllAt(f, hdr[:], 0)
			_ = f.Truncate(idxHeaderSize)
		}
		return
	}

	dirty := validLen != len(data)
	kept := make([]byte, idxHeaderSize, len(data))
	for off := idxHeaderSize; off < validLen; off += idxRecSize {
		rec := data[off : off+idxRecSize]
		_, _, _, _, usedNano, _, recOK := decodeIdxRecord(rec)
		if !recOK {
			dirty = true
			continue
		}
		remove, inBase := classify(time.Unix(0, usedNano))
		if remove {
			dirty = true
			if stats != nil {
				stats.removedFiles++
				stats.removedBytes += idxRecSize
			}
			continue
		}
		kept = append(kept, rec...)
		if stats != nil {
			stats.keptFiles++
			stats.keptBytes += idxRecSize
			if inBase {
				stats.baseBytes += idxRecSize
			}
		}
	}
	if !dirty {
		return
	}
	encodeIdxHeader(kept[:idxHeaderSize], gen+1)
	if err := writeAllAt(f, kept, 0); err != nil {
		return
	}
	_ = f.Truncate(int64(len(kept)))
}

func encodePackFileHeader(dst []byte, gen uint64) {
	copy(dst[:8], packFileMagic)
	binary.LittleEndian.PutUint64(dst[8:16], gen)
}

func encodePackEntryHeader(dst []byte, out OutputID, payloadSize uint32, usedNano int64, flags, payloadCRC uint32) {
	copy(dst[0:4], packMagic)
	copy(dst[4:36], out[:])
	binary.LittleEndian.PutUint32(dst[36:40], payloadSize)
	binary.LittleEndian.PutUint64(dst[40:48], uint64(usedNano))
	binary.LittleEndian.PutUint32(dst[48:52], flags)
	binary.LittleEndian.PutUint32(dst[52:56], payloadCRC)
	binary.LittleEndian.PutUint32(dst[56:60], crc32.ChecksumIEEE(dst[:56]))
}

func decodePackEntryHeader(src []byte) (out OutputID, payloadSize uint32, usedNano int64, flags, payloadCRC uint32, ok bool) {
	if len(src) != packHeaderSize || string(src[:4]) != packMagic {
		return OutputID{}, 0, 0, 0, 0, false
	}
	if crc32.ChecksumIEEE(src[:56]) != binary.LittleEndian.Uint32(src[56:60]) {
		return OutputID{}, 0, 0, 0, 0, false
	}
	payloadSize = binary.LittleEndian.Uint32(src[36:40])
	usedNano = int64(binary.LittleEndian.Uint64(src[40:48]))
	flags = binary.LittleEndian.Uint32(src[48:52])
	payloadCRC = binary.LittleEndian.Uint32(src[52:56])
	if payloadSize > maxPackPayloadSize || usedNano < 0 {
		return OutputID{}, 0, 0, 0, 0, false
	}
	copy(out[:], src[4:36])
	return out, payloadSize, usedNano, flags, payloadCRC, true
}

func parsePackFileHeader(data []byte) (gen uint64, ok bool) {
	if len(data) < packFileHeaderSize || string(data[:8]) != packFileMagic {
		return 0, false
	}
	return binary.LittleEndian.Uint64(data[8:16]), true
}

func (c *DiskCache) putPacked(out OutputID, payload []byte) (unsupported bool, err error) {
	if len(payload) > maxPackPayloadSize {
		return false, fmt.Errorf("payload size %d exceeds maxPackPayloadSize", len(payload))
	}
	b := out[0]
	c.packMu[b].Lock()
	defer c.packMu[b].Unlock()

	f, err := lockedfile.Edit(c.packPath(b))
	if err != nil {
		if errors.Is(err, errors.ErrUnsupported) {
			return true, nil
		}
		return false, err
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return false, err
	}
	payloadCRC := crc32.ChecksumIEEE(payload)
	mark := c.writeTime()

	gen, ok := parsePackFileHeader(data)
	if !ok {
		buf := make([]byte, packFileHeaderSize+packHeaderSize+len(payload))
		encodePackFileHeader(buf[:packFileHeaderSize], 1)
		encodePackEntryHeader(buf[packFileHeaderSize:packFileHeaderSize+packHeaderSize], out, uint32(len(payload)), mark.UnixNano(), 0, payloadCRC)
		copy(buf[packFileHeaderSize+packHeaderSize:], payload)
		if err := writeAllAt(f, buf, 0); err != nil {
			return false, err
		}
		if len(data) > len(buf) {
			if err := f.Truncate(int64(len(buf))); err != nil {
				return false, err
			}
		}
		c.addBytes(int64(packHeaderSize + len(payload)))
		c.noteUsed(out, 'd')
		return false, nil
	}

	off := packFileHeaderSize
	for off+packHeaderSize <= len(data) {
		hdrOut, psize, usedNano, flags, pcrc, hdrOK := decodePackEntryHeader(data[off : off+packHeaderSize])
		if !hdrOK || off+packHeaderSize+int(psize) > len(data) {
			break
		}
		pEnd := off + packHeaderSize + int(psize)
		if hdrOut == out && int(psize) == len(payload) && pcrc == payloadCRC && crc32.ChecksumIEEE(data[off+packHeaderSize:pEnd]) == pcrc {
			if c.trimBuildCache {
				newUsed := c.rewriteUsedTime(time.Unix(0, usedNano))
				var hdr [packHeaderSize]byte
				encodePackEntryHeader(hdr[:], hdrOut, psize, newUsed.UnixNano(), flags, pcrc)
				if err := writeAllAt(f, hdr[:], int64(off)); err != nil {
					return false, err
				}
				var genBuf [8]byte
				binary.LittleEndian.PutUint64(genBuf[:], gen+1)
				_ = writeAllAt(f, genBuf[:], 8)
			}
			c.noteUsed(out, 'd')
			return false, nil
		}
		off = pEnd
	}

	// Append new packed entry at valid offset off.
	entry := make([]byte, packHeaderSize+len(payload))
	encodePackEntryHeader(entry[:packHeaderSize], out, uint32(len(payload)), mark.UnixNano(), 0, payloadCRC)
	copy(entry[packHeaderSize:], payload)
	if err := writeAllAt(f, entry, int64(off)); err != nil {
		return false, err
	}
	var genBuf [8]byte
	binary.LittleEndian.PutUint64(genBuf[:], gen+1)
	if err := writeAllAt(f, genBuf[:], 8); err != nil {
		return false, err
	}
	newLen := int64(off + len(entry))
	if int64(len(data)) > newLen {
		_ = f.Truncate(newLen)
	}
	c.addBytes(int64(len(entry)))
	c.noteUsed(out, 'd')
	return false, nil
}

func (c *DiskCache) getPackedBytes(out OutputID, wantSize int64) (payload []byte, found bool, err error) {
	b := out[0]
	c.packMu[b].RLock()
	f, err := lockedfile.Open(c.packPath(b))
	if err != nil {
		c.packMu[b].RUnlock()
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, errors.ErrUnsupported) {
			return nil, false, nil
		}
		return nil, false, err
	}
	data, readErr := io.ReadAll(f)
	f.Close()
	c.packMu[b].RUnlock()
	if readErr != nil {
		return nil, false, readErr
	}
	if _, ok := parsePackFileHeader(data); !ok {
		return nil, false, nil
	}

	off := packFileHeaderSize
	for off+packHeaderSize <= len(data) {
		hdrOut, psize, usedNano, _, pcrc, hdrOK := decodePackEntryHeader(data[off : off+packHeaderSize])
		if !hdrOK || off+packHeaderSize+int(psize) > len(data) {
			break
		}
		pStart := off + packHeaderSize
		pEnd := pStart + int(psize)
		if hdrOut == out {
			if wantSize >= 0 && int64(psize) != wantSize {
				return nil, false, nil
			}
			p := data[pStart:pEnd]
			if crc32.ChecksumIEEE(p) != pcrc {
				return nil, false, nil
			}
			res := bytes.Clone(p)
			if _, needUpdate := c.shouldMarkUsed(time.Unix(0, usedNano)); needUpdate {
				c.touchPackTime(out, false)
			}
			c.noteUsed(out, 'd')
			return res, true, nil
		}
		off = pEnd
	}
	return nil, false, nil
}

func (c *DiskCache) touchPackTime(out OutputID, keepYoung bool) {
	b := out[0]
	c.packMu[b].Lock()
	defer c.packMu[b].Unlock()

	f, err := lockedfile.Edit(c.packPath(b))
	if err != nil {
		return
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return
	}
	gen, ok := parsePackFileHeader(data)
	if !ok {
		return
	}
	off := packFileHeaderSize
	for off+packHeaderSize <= len(data) {
		hdrOut, psize, usedNano, flags, pcrc, hdrOK := decodePackEntryHeader(data[off : off+packHeaderSize])
		if !hdrOK || off+packHeaderSize+int(psize) > len(data) {
			break
		}
		pEnd := off + packHeaderSize + int(psize)
		if hdrOut == out {
			used := time.Unix(0, usedNano)
			var newUsed time.Time
			var need bool
			if keepYoung {
				newUsed, need = c.refreshEntryTime(used)
			} else {
				newUsed, need = c.shouldMarkUsed(used)
			}
			if !need {
				return
			}
			var hdr [packHeaderSize]byte
			encodePackEntryHeader(hdr[:], hdrOut, psize, newUsed.UnixNano(), flags, pcrc)
			if err := writeAllAt(f, hdr[:], int64(off)); err != nil {
				return
			}
			var genBuf [8]byte
			binary.LittleEndian.PutUint64(genBuf[:], gen+1)
			_ = writeAllAt(f, genBuf[:], 8)
			return
		}
		off = pEnd
	}
}

// materializePacked extracts out from its pack file (if present) into targetPath
// and removes it from pack so there is no duplicate copy on disk.
func (c *DiskCache) materializePacked(out OutputID, targetPath string) bool {
	b := out[0]
	c.packMu[b].Lock()
	defer c.packMu[b].Unlock()

	packFile := c.packPath(b)
	if _, err := os.Stat(packFile); err != nil {
		return false
	}
	f, err := lockedfile.Edit(packFile)
	if err != nil {
		return false
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return false
	}
	gen, ok := parsePackFileHeader(data)
	if !ok {
		return false
	}

	var (
		found   bool
		payload []byte
		used    time.Time
		kept    []byte
	)
	off := packFileHeaderSize
	for off+packHeaderSize <= len(data) {
		hdrOut, psize, usedNano, _, pcrc, hdrOK := decodePackEntryHeader(data[off : off+packHeaderSize])
		if !hdrOK || off+packHeaderSize+int(psize) > len(data) {
			break
		}
		pStart := off + packHeaderSize
		pEnd := pStart + int(psize)
		if !found && hdrOut == out && crc32.ChecksumIEEE(data[pStart:pEnd]) == pcrc {
			found = true
			payload = bytes.Clone(data[pStart:pEnd])
			used = time.Unix(0, usedNano)
			if newUsed, need := c.shouldMarkUsed(used); need {
				used = newUsed
			}
			if kept == nil {
				kept = make([]byte, packFileHeaderSize, len(data)-(pEnd-off))
				kept = append(kept, data[packFileHeaderSize:off]...)
			}
		} else if kept != nil {
			kept = append(kept, data[off:pEnd]...)
		}
		off = pEnd
	}
	if !found {
		return false
	}

	tmp, tmpName, err := c.createTempFile(0o666)
	if err != nil {
		return false
	}
	if len(payload) > 0 {
		if _, err := tmp.Write(payload); err != nil {
			tmp.Close()
			os.Remove(tmpName)
			return false
		}
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return false
	}
	if !c.trimBuildCache && used.IsZero() {
		used = c.now()
	}
	os.Chtimes(tmpName, used, used)
	if err := os.Rename(tmpName, targetPath); err != nil {
		os.Remove(tmpName)
		return false
	}

	encodePackFileHeader(kept[:packFileHeaderSize], gen+1)
	if err := writeAllAt(f, kept, 0); err == nil {
		_ = f.Truncate(int64(len(kept)))
	}
	c.noteUsed(out, 'd')
	return true
}

func (c *DiskCache) trimPackBucket(b byte, classify func(mt time.Time) (remove, inBase bool), stats *trimStats) {
	packFile := c.packPath(b)
	if _, err := os.Stat(packFile); err != nil {
		return
	}
	c.packMu[b].Lock()
	defer c.packMu[b].Unlock()

	f, err := lockedfile.Edit(packFile)
	if err != nil {
		return
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return
	}
	gen, ok := parsePackFileHeader(data)
	if !ok {
		if len(data) > 0 {
			var hdr [packFileHeaderSize]byte
			encodePackFileHeader(hdr[:], 1)
			_ = writeAllAt(f, hdr[:], 0)
			_ = f.Truncate(packFileHeaderSize)
		}
		return
	}

	var dirty bool
	kept := make([]byte, packFileHeaderSize, len(data))
	off := packFileHeaderSize
	for off+packHeaderSize <= len(data) {
		hdrOut, psize, usedNano, _, pcrc, hdrOK := decodePackEntryHeader(data[off : off+packHeaderSize])
		if !hdrOK || off+packHeaderSize+int(psize) > len(data) {
			dirty = true
			break
		}
		pStart := off + packHeaderSize
		pEnd := pStart + int(psize)
		if crc32.ChecksumIEEE(data[pStart:pEnd]) != pcrc {
			dirty = true
			off = pEnd
			continue
		}
		_ = hdrOut
		entryBytes := int64(packHeaderSize) + int64(psize)
		remove, inBase := classify(time.Unix(0, usedNano))
		if remove {
			dirty = true
			if stats != nil {
				stats.removedFiles++
				stats.removedBytes += entryBytes
			}
		} else {
			kept = append(kept, data[off:pEnd]...)
			if stats != nil {
				stats.keptFiles++
				stats.keptBytes += entryBytes
				if inBase {
					stats.baseBytes += entryBytes
				}
			}
		}
		off = pEnd
	}
	if off != len(data) {
		dirty = true
	}
	if !dirty {
		return
	}
	encodePackFileHeader(kept[:packFileHeaderSize], gen+1)
	if err := writeAllAt(f, kept, 0); err != nil {
		return
	}
	_ = f.Truncate(int64(len(kept)))
}
