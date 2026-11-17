// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package internal

import "encoding/binary"

// The algorithm uses at most SniffLen bytes to make its decision.
const SniffLen = 512

// DetectContentType implements the algorithm described
// at https://mimesniff.spec.whatwg.org/ to determine the
// Content-Type of the given data. It considers at most the
// first 512 bytes of data. DetectContentType always returns
// a valid MIME type: if it cannot determine a more specific one, it
// returns "application/octet-stream".
func DetectContentType(data []byte) string {
	if len(data) > SniffLen {
		data = data[:SniffLen]
	}

	// Index of the first non-whitespace byte in data.
	firstNonWS := 0
	if len(data) > 0 && isWS(data[0]) {
		for firstNonWS = 1; firstNonWS < len(data) && isWS(data[firstNonWS]); firstNonWS++ {
		}
	}

	// 1. HTML / XML tags (leading whitespace ignored).
	if s := data[firstNonWS:]; len(s) >= 2 && s[0] == '<' {
		if ct := matchHTMLOrXML(s); ct != "" {
			return ct
		}
	}

	// 2. Binary signatures ordered before MP4/EOT per WHATWG MIME Sniffing.
	if len(data) > 0 {
		switch data[0] {
		case '%':
			if len(data) >= 5 && data[1] == 'P' && data[2] == 'D' && data[3] == 'F' && data[4] == '-' {
				return "application/pdf"
			}
			if len(data) >= 11 && string(data[:11]) == "%!PS-Adobe-" {
				return "application/postscript"
			}
		case 0xFE:
			if len(data) >= 4 && data[1] == 0xFF {
				return "text/plain; charset=utf-16be"
			}
		case 0xFF:
			if len(data) >= 4 && data[1] == 0xFE {
				return "text/plain; charset=utf-16le"
			}
			if len(data) >= 3 && data[1] == 0xD8 && data[2] == 0xFF {
				return "image/jpeg"
			}
		case 0xEF:
			if len(data) >= 4 && data[1] == 0xBB && data[2] == 0xBF {
				return "text/plain; charset=utf-8"
			}
		case 0x00:
			if len(data) >= 4 && data[1] == 0x00 && (data[2] == 0x01 || data[2] == 0x02) && data[3] == 0x00 {
				return "image/x-icon"
			}
		case 'B':
			if len(data) >= 2 && data[1] == 'M' {
				return "image/bmp"
			}
		case 'G':
			if len(data) >= 6 && data[1] == 'I' && data[2] == 'F' && data[3] == '8' && (data[4] == '7' || data[4] == '9') && data[5] == 'a' {
				return "image/gif"
			}
		case 'R':
			if len(data) >= 12 && data[1] == 'I' && data[2] == 'F' && data[3] == 'F' {
				if len(data) >= 14 && string(data[8:14]) == "WEBPVP" {
					return "image/webp"
				}
				switch string(data[8:12]) {
				case "AVI ":
					return "video/avi"
				case "WAVE":
					return "audio/wave"
				}
			}
		case 0x89:
			if len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n" {
				return "image/png"
			}
		case 'F':
			if len(data) >= 12 && data[1] == 'O' && data[2] == 'R' && data[3] == 'M' &&
				data[8] == 'A' && data[9] == 'I' && data[10] == 'F' && data[11] == 'F' {
				return "audio/aiff"
			}
		case 'I':
			if len(data) >= 3 && data[1] == 'D' && data[2] == '3' {
				return "audio/mpeg"
			}
		case 'O':
			if len(data) >= 5 && data[1] == 'g' && data[2] == 'g' && data[3] == 'S' && data[4] == 0x00 {
				return "application/ogg"
			}
		case 'M':
			if len(data) >= 8 && string(data[:8]) == "MThd\x00\x00\x00\x06" {
				return "audio/midi"
			}
		}

		// 3. MP4 (6.2.0.2 video/mp4)
		if len(data) >= 12 && data[4] == 'f' && data[5] == 't' && data[6] == 'y' && data[7] == 'p' {
			if matchMP4(data) {
				return "video/mp4"
			}
		}

		// 4. WebM (6.2.0.3 video/webm)
		if len(data) >= 4 && data[0] == 0x1A && data[1] == 0x45 && data[2] == 0xDF && data[3] == 0xA3 {
			return "video/webm"
		}

		// 5. EOT font ("LP" at offset 34; takes precedence over post-EOT signatures).
		if len(data) >= 36 && data[34] == 'L' && data[35] == 'P' {
			return "application/vnd.ms-fontobject"
		}

		// 6. Font & archive signatures ordered after EOT.
		switch data[0] {
		case 0x00:
			if len(data) >= 4 {
				if data[1] == 0x01 && data[2] == 0x00 && data[3] == 0x00 {
					return "font/ttf"
				}
				if data[1] == 'a' && data[2] == 's' && data[3] == 'm' {
					return "application/wasm"
				}
			}
		case 'O':
			if len(data) >= 4 && data[1] == 'T' && data[2] == 'T' && data[3] == 'O' {
				return "font/otf"
			}
		case 't':
			if len(data) >= 4 && data[1] == 't' && data[2] == 'c' && data[3] == 'f' {
				return "font/collection"
			}
		case 'w':
			if len(data) >= 4 && data[1] == 'O' && data[2] == 'F' {
				if data[3] == 'F' {
					return "font/woff"
				}
				if data[3] == '2' {
					return "font/woff2"
				}
			}
		case 0x1F:
			if len(data) >= 3 && data[1] == 0x8B && data[2] == 0x08 {
				return "application/x-gzip"
			}
		case 'P':
			if len(data) >= 4 && data[1] == 'K' && data[2] == 0x03 && data[3] == 0x04 {
				return "application/zip"
			}
		case 'R':
			if len(data) >= 7 && data[1] == 'a' && data[2] == 'r' && data[3] == '!' && data[4] == 0x1A && data[5] == 0x07 {
				if data[6] == 0x00 || (len(data) >= 8 && data[6] == 0x01 && data[7] == 0x00) {
					return "application/x-rar-compressed"
				}
			}
		}
	}

	// 7. Plain text vs binary fallback (section 5, step 4).
	for _, b := range data[firstNonWS:] {
		if b <= 0x1F && (b <= 0x08 || b == 0x0B || (0x0E <= b && b <= 0x1A) || 0x1C <= b) {
			return "application/octet-stream"
		}
	}
	return "text/plain; charset=utf-8"
}

// isWS reports whether the provided byte is a whitespace byte (0xWS)
// as defined in https://mimesniff.spec.whatwg.org/#terminology.
func isWS(b byte) bool {
	return b == ' ' || (b >= '\t' && b <= '\r' && b != '\v')
}

// isTT reports whether the provided byte is a tag-terminating byte (0xTT)
// as defined in https://mimesniff.spec.whatwg.org/#terminology.
func isTT(b byte) bool {
	return b == ' ' || b == '>'
}

// matchLowerAlphaTT checks that tail starts with lower (consisting exclusively
// of lowercase ASCII letters 'a'..'z') case-insensitively, followed by a tag
// terminating byte (0xTT).
func matchLowerAlphaTT(tail []byte, lower string) bool {
	if len(tail) <= len(lower) {
		return false
	}
	for i := 0; i < len(lower); i++ {
		if tail[i]|0x20 != lower[i] {
			return false
		}
	}
	return isTT(tail[len(lower)])
}

func matchHTMLOrXML(s []byte) string {
	switch s[1] | 0x20 {
	case '!' | 0x20: // '!' (0x21 | 0x20 == 0x21)
		if s[1] == '!' {
			if len(s) >= 15 && isTT(s[14]) &&
				(binary.LittleEndian.Uint64(s[2:10])|0x0020202020202020) == 0x2065707974636f64 &&
				(binary.LittleEndian.Uint32(s[10:14])|0x20202020) == 0x6c6d7468 {
				return "text/html; charset=utf-8"
			}
			if len(s) >= 5 && s[2] == '-' && s[3] == '-' && isTT(s[4]) {
				return "text/html; charset=utf-8"
			}
		}
	case '?': // '?' (0x3F)
		if s[1] == '?' && len(s) >= 5 && s[2] == 'x' && s[3] == 'm' && s[4] == 'l' {
			return "text/xml; charset=utf-8"
		}
	case 'h':
		if matchLowerAlphaTT(s[2:], "tml") || matchLowerAlphaTT(s[2:], "ead") ||
			(len(s) >= 4 && s[2] == '1' && isTT(s[3])) {
			return "text/html; charset=utf-8"
		}
	case 's':
		if matchLowerAlphaTT(s[2:], "cript") || matchLowerAlphaTT(s[2:], "tyle") {
			return "text/html; charset=utf-8"
		}
	case 'i':
		if matchLowerAlphaTT(s[2:], "frame") {
			return "text/html; charset=utf-8"
		}
	case 'd':
		if matchLowerAlphaTT(s[2:], "iv") {
			return "text/html; charset=utf-8"
		}
	case 'f':
		if matchLowerAlphaTT(s[2:], "ont") {
			return "text/html; charset=utf-8"
		}
	case 't':
		if matchLowerAlphaTT(s[2:], "able") || matchLowerAlphaTT(s[2:], "itle") {
			return "text/html; charset=utf-8"
		}
	case 'a':
		if len(s) >= 3 && isTT(s[2]) {
			return "text/html; charset=utf-8"
		}
	case 'b':
		if (len(s) >= 3 && isTT(s[2])) || matchLowerAlphaTT(s[2:], "ody") || matchLowerAlphaTT(s[2:], "r") {
			return "text/html; charset=utf-8"
		}
	case 'p':
		if len(s) >= 3 && isTT(s[2]) {
			return "text/html; charset=utf-8"
		}
	}
	return ""
}

func matchMP4(data []byte) bool {
	// https://mimesniff.spec.whatwg.org/#signature-for-mp4
	// c.f. section 6.2.1
	boxSize := int(binary.BigEndian.Uint32(data[:4]))
	if len(data) < boxSize || boxSize%4 != 0 {
		return false
	}
	for st := 8; st < boxSize; st += 4 {
		if st == 12 {
			// Ignores the four bytes that correspond to the version number of the "major brand".
			continue
		}
		if data[st] == 'm' && data[st+1] == 'p' && data[st+2] == '4' {
			return true
		}
	}
	return false
}
