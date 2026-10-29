package main

import (
	"bytes"
	"strings"
	"testing"
)

// The worked example of a version 1-M "HELLO WORLD" code: its 16 data
// codewords and the 10 error correction codewords they give.
func TestQRReedSolomon(t *testing.T) {
	data := []byte{32, 91, 11, 120, 209, 114, 220, 77, 67, 64, 236, 17, 236, 17, 236, 17}
	want := []byte{196, 35, 39, 119, 235, 215, 231, 226, 93, 23}
	if got := qrReedSolomon(data, 10); !bytes.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// Format information for error correction M: the published 15-bit strings.
func TestQRFormatBits(t *testing.T) {
	for mask, want := range []uint32{
		0b101010000010010, 0b101000100100101, 0b101111001111100, 0b101101101001011,
		0b100010111111001, 0b100000011001110, 0b100111110010111, 0b100101010100000,
	} {
		if got := qrFormatBits(mask); got != want {
			t.Errorf("mask %d: %015b, want %015b", mask, got, want)
		}
	}
}

func TestQRVersionAndPatterns(t *testing.T) {
	for _, c := range []struct {
		text string
		size int
	}{
		{"hello", 21}, // version 1
		{"http://192.168.1.20:8898/pair/" + strings.Repeat("a", 32), 33}, // 62 bytes: 508 bits, version 4 holds 512
		{strings.Repeat("x", 200), 57},                                   // version 10
	} {
		q, err := qrEncode(c.text)
		if err != nil {
			t.Fatal(err)
		}
		if q.size != c.size {
			t.Errorf("%d bytes: size %d, want %d", len(c.text), q.size, c.size)
		}
		// Finder patterns: dark 7×7 ring with a 3×3 core, at three corners.
		for _, o := range [][2]int{{0, 0}, {q.size - 7, 0}, {0, q.size - 7}} {
			for y := range 7 {
				for x := range 7 {
					ring := x == 0 || x == 6 || y == 0 || y == 6
					core := x >= 2 && x <= 4 && y >= 2 && y <= 4
					if q.at(o[0]+x, o[1]+y) != (ring || core) {
						t.Fatalf("size %d: finder at %v broken at %d,%d", q.size, o, x, y)
					}
				}
			}
		}
		// The dark module next to the lower left finder.
		if !q.at(8, q.size-8) {
			t.Error("dark module missing")
		}
		svg := q.svg()
		if !strings.HasPrefix(svg, "<svg") || !strings.Contains(svg, "viewBox") {
			t.Errorf("svg: %.60s", svg)
		}
	}
	if _, err := qrEncode(strings.Repeat("x", 300)); err == nil {
		t.Error("too long for version 10 accepted")
	}
}
