package main

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
)

// ---------------------------------------------------------------------------
// QR codes for the phone pairing link: byte mode, error correction level M,
// versions 1–10 (up to 213 bytes). Follows ISO/IEC 18004; the structure is
// that of Project Nayuki's reference implementation (MIT).
// ---------------------------------------------------------------------------

type qrCode struct {
	size     int
	modules  []bool // dark
	function []bool // finder, timing, alignment, format and version areas
}

func (q *qrCode) at(x, y int) bool { return q.modules[y*q.size+x] }

func (q *qrCode) setFunc(x, y int, dark bool) {
	q.modules[y*q.size+x], q.function[y*q.size+x] = dark, true
}

// Error correction codewords per block and number of blocks, level M,
// indexed by version.
var (
	qrECCLen = [11]int{0, 10, 16, 26, 18, 24, 16, 18, 22, 22, 26}
	qrBlocks = [11]int{0, 1, 1, 1, 2, 2, 4, 4, 4, 5, 5}
)

// qrRawModules: modules left for data and error correction in a version.
func qrRawModules(ver int) int {
	r := (16*ver+128)*ver + 64
	if ver >= 2 {
		n := ver/7 + 2
		r -= (25*n-10)*n - 55
		if ver >= 7 {
			r -= 36
		}
	}
	return r
}

func qrDataCodewords(ver int) int { return qrRawModules(ver)/8 - qrECCLen[ver]*qrBlocks[ver] }

// qrEncode makes the smallest code (version 1–10, level M) holding text.
func qrEncode(text string) (*qrCode, error) {
	data := []byte(text)
	ver, countBits := 1, 8
	for ; ; ver++ {
		if ver > 10 {
			return nil, errors.New("QR kodu için metin çok uzun")
		}
		if ver >= 10 {
			countBits = 16
		}
		if 4+countBits+8*len(data) <= qrDataCodewords(ver)*8 {
			break
		}
	}

	var bits []bool
	put := func(v, n int) {
		for i := n - 1; i >= 0; i-- {
			bits = append(bits, v>>i&1 == 1)
		}
	}
	put(0b0100, 4) // byte mode
	put(len(data), countBits)
	for _, b := range data {
		put(int(b), 8)
	}
	capBits := qrDataCodewords(ver) * 8
	put(0, min(4, capBits-len(bits))) // terminator
	put(0, (8-len(bits)%8)%8)
	for pad := 0xEC; len(bits) < capBits; pad ^= 0xEC ^ 0x11 {
		put(pad, 8)
	}
	codewords := make([]byte, len(bits)/8)
	for i, b := range bits {
		if b {
			codewords[i>>3] |= 1 << (7 - i&7)
		}
	}

	q := &qrCode{size: ver*4 + 17}
	q.modules = make([]bool, q.size*q.size)
	q.function = make([]bool, q.size*q.size)
	q.drawFunctionPatterns(ver)
	q.drawCodewords(qrInterleave(ver, codewords))

	best, bestPenalty := 0, math.MaxInt
	for m := range 8 {
		q.applyMask(m)
		q.drawFormat(m)
		if p := q.penalty(); p < bestPenalty {
			best, bestPenalty = m, p
		}
		q.applyMask(m) // a mask is its own inverse
	}
	q.applyMask(best)
	q.drawFormat(best)
	return q, nil
}

// qrInterleave splits the data into blocks, adds each block's error
// correction and interleaves them in the order the symbol is read.
func qrInterleave(ver int, data []byte) []byte {
	numBlocks, eccLen := qrBlocks[ver], qrECCLen[ver]
	raw := qrRawModules(ver) / 8
	numShort, shortLen := numBlocks-raw%numBlocks, raw/numBlocks
	var blocks [][]byte
	k := 0
	for i := range numBlocks {
		n := shortLen - eccLen
		if i >= numShort {
			n++
		}
		dat := data[k : k+n]
		k += n
		b := append(slices.Clone(dat), qrReedSolomon(dat, eccLen)...)
		if i < numShort {
			b = slices.Insert(b, n, 0) // placeholder: short blocks line up with long ones
		}
		blocks = append(blocks, b)
	}
	var out []byte
	for i := range blocks[0] {
		for j, b := range blocks {
			if i != shortLen-eccLen || j >= numShort {
				out = append(out, b[i])
			}
		}
	}
	return out
}

// qrMul multiplies in GF(2^8) modulo x^8 + x^4 + x^3 + x^2 + 1.
func qrMul(x, y byte) byte {
	z := 0
	for i := 7; i >= 0; i-- {
		z = z<<1 ^ (z>>7)*0x11D
		z ^= int(y>>i&1) * int(x)
	}
	return byte(z)
}

// qrReedSolomon returns the degree error correction codewords for data.
func qrReedSolomon(data []byte, degree int) []byte {
	div := make([]byte, degree)
	div[degree-1] = 1
	root := byte(1)
	for range degree {
		for j := range div {
			div[j] = qrMul(div[j], root)
			if j+1 < degree {
				div[j] ^= div[j+1]
			}
		}
		root = qrMul(root, 2)
	}
	res := make([]byte, degree)
	for _, b := range data {
		f := b ^ res[0]
		copy(res, res[1:])
		res[degree-1] = 0
		for i := range res {
			res[i] ^= qrMul(div[i], f)
		}
	}
	return res
}

func (q *qrCode) drawFunctionPatterns(ver int) {
	n := q.size
	for i := range n {
		q.setFunc(6, i, i%2 == 0)
		q.setFunc(i, 6, i%2 == 0)
	}
	for _, c := range [][2]int{{3, 3}, {n - 4, 3}, {3, n - 4}} {
		for dy := -4; dy <= 4; dy++ {
			for dx := -4; dx <= 4; dx++ {
				x, y, d := c[0]+dx, c[1]+dy, max(abs(dx), abs(dy))
				if x >= 0 && x < n && y >= 0 && y < n {
					q.setFunc(x, y, d != 2 && d != 4)
				}
			}
		}
	}
	al := qrAlignment(ver)
	last := len(al) - 1
	for i, ax := range al {
		for j, ay := range al {
			if (i == 0 && j == 0) || (i == 0 && j == last) || (i == last && j == 0) {
				continue // the finder corners
			}
			for dy := -2; dy <= 2; dy++ {
				for dx := -2; dx <= 2; dx++ {
					q.setFunc(ax+dx, ay+dy, max(abs(dx), abs(dy)) != 1)
				}
			}
		}
	}
	q.drawFormat(0) // reserve the area; the real bits come after masking
	if ver >= 7 {
		rem := ver
		for range 12 {
			rem = rem<<1 ^ (rem>>11)*0x1F25
		}
		bits := ver<<12 | rem
		for i := range 18 {
			a, b := n-11+i%3, i/3
			q.setFunc(a, b, bits>>i&1 == 1)
			q.setFunc(b, a, bits>>i&1 == 1)
		}
	}
}

// qrAlignment: centre coordinates of the alignment patterns.
func qrAlignment(ver int) []int {
	if ver == 1 {
		return nil
	}
	num := ver/7 + 2
	step := (ver*8 + num*3 + 5) / (num*4 - 4) * 2
	res := []int{6}
	for pos := ver*4 + 10; len(res) < num; pos -= step {
		res = slices.Insert(res, 1, pos)
	}
	return res
}

// qrFormatBits: level M (00) and the mask, BCH protected and masked.
func qrFormatBits(mask int) uint32 {
	data := uint32(mask)
	rem := data
	for range 10 {
		rem = rem<<1 ^ (rem>>9)*0x537
	}
	return (data<<10 | rem) ^ 0x5412
}

func (q *qrCode) drawFormat(mask int) {
	bits, n := qrFormatBits(mask), q.size
	bit := func(i int) bool { return bits>>i&1 == 1 }
	for i := range 6 {
		q.setFunc(8, i, bit(i))
	}
	q.setFunc(8, 7, bit(6))
	q.setFunc(8, 8, bit(7))
	q.setFunc(7, 8, bit(8))
	for i := 9; i < 15; i++ {
		q.setFunc(14-i, 8, bit(i))
	}
	for i := range 8 {
		q.setFunc(n-1-i, 8, bit(i))
	}
	for i := 8; i < 15; i++ {
		q.setFunc(8, n-15+i, bit(i))
	}
	q.setFunc(8, n-8, true) // the dark module
}

// drawCodewords fills the data area in the zigzag order, two columns at a
// time from the right, skipping the vertical timing column.
func (q *qrCode) drawCodewords(data []byte) {
	i, n := 0, q.size
	for right := n - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		upward := (right+1)&2 == 0
		for vert := range n {
			for j := range 2 {
				x, y := right-j, vert
				if upward {
					y = n - 1 - vert
				}
				if !q.function[y*n+x] && i < len(data)*8 {
					q.modules[y*n+x] = data[i>>3]>>(7-i&7)&1 == 1
					i++
				}
			}
		}
	}
}

func (q *qrCode) applyMask(m int) {
	for y := range q.size {
		for x := range q.size {
			var flip bool
			switch m {
			case 0:
				flip = (x+y)%2 == 0
			case 1:
				flip = y%2 == 0
			case 2:
				flip = x%3 == 0
			case 3:
				flip = (x+y)%3 == 0
			case 4:
				flip = (x/3+y/2)%2 == 0
			case 5:
				flip = x*y%2+x*y%3 == 0
			case 6:
				flip = (x*y%2+x*y%3)%2 == 0
			case 7:
				flip = ((x+y)%2+x*y%3)%2 == 0
			}
			if flip && !q.function[y*q.size+x] {
				q.modules[y*q.size+x] = !q.modules[y*q.size+x]
			}
		}
	}
}

// penalty scores how hard the symbol is to read (lower is better): long
// runs, 2×2 blocks, finder-like 1:1:3:1:1 patterns and dark/light imbalance.
func (q *qrCode) penalty() int {
	n, p := q.size, 0
	finder := []bool{true, false, true, true, true, false, true, false, false, false, false}
	for _, col := range []bool{false, true} {
		get := func(a, b int) bool {
			if col {
				return q.at(a, b)
			}
			return q.at(b, a)
		}
		for a := range n {
			run := 1
			for b := 1; b <= n; b++ {
				if b < n && get(a, b) == get(a, b-1) {
					run++
					continue
				}
				if run >= 5 {
					p += run - 2
				}
				run = 1
			}
			for b := 0; b+len(finder) <= n; b++ {
				fwd, back := true, true
				for k, f := range finder {
					fwd = fwd && get(a, b+k) == f
					back = back && get(a, b+len(finder)-1-k) == f
				}
				if fwd || back {
					p += 40
				}
			}
		}
	}
	dark := 0
	for y := range n {
		for x := range n {
			if q.at(x, y) {
				dark++
			}
			if x+1 < n && y+1 < n && q.at(x, y) == q.at(x+1, y) && q.at(x, y) == q.at(x, y+1) && q.at(x, y) == q.at(x+1, y+1) {
				p += 3
			}
		}
	}
	total := n * n
	p += ((abs(dark*20-total*10)+total-1)/total - 1) * 10
	return p
}

// svg draws the code with the 4-module quiet zone around it.
func (q *qrCode) svg() string {
	const quiet = 4
	var b strings.Builder
	w := q.size + 2*quiet
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges"><rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="`, w, w, w, w)
	for y := range q.size {
		for x := range q.size {
			if q.at(x, y) {
				fmt.Fprintf(&b, "M%d %dh1v1h-1z", x+quiet, y+quiet)
			}
		}
	}
	b.WriteString(`"/></svg>`)
	return b.String()
}
