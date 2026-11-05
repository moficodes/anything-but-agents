// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"fmt"
)

// RenderQRSVG generates an ISO/IEC 18004 Byte-mode (ECC Level L) QR code SVG
// using only the Go standard library so audience phones can scan the slide deck
// on the projector screen with zero external dependencies.
func RenderQRSVG(text string) string {
	matrix := encodeQRCodeLevelL([]byte(text))
	n := len(matrix)
	quiet := 4
	total := n + quiet*2

	var buf bytes.Buffer
	fmt.Fprintf(&buf, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges">`, total, total)
	fmt.Fprintf(&buf, `<rect width="%d" height="%d" fill="#ffffff" rx="2"/>`, total, total)
	buf.WriteString(`<path fill="#07090d" d="`)
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			if matrix[y][x] {
				fmt.Fprintf(&buf, "M%d,%dh1v1h-1z", x+quiet, y+quiet)
			}
		}
	}
	buf.WriteString(`"/></svg>`)
	return buf.String()
}

type qrVersionSpec struct {
	version    int
	size       int
	dataBytes  int
	ecBytes    int
	alignments []int
}

var qrSpecsLevelL = []qrVersionSpec{
	{version: 2, size: 25, dataBytes: 34, ecBytes: 10, alignments: []int{6, 18}},
	{version: 3, size: 29, dataBytes: 55, ecBytes: 15, alignments: []int{6, 22}},
	{version: 4, size: 33, dataBytes: 80, ecBytes: 20, alignments: []int{6, 26}},
	{version: 5, size: 37, dataBytes: 108, ecBytes: 26, alignments: []int{6, 30}},
}

func encodeQRCodeLevelL(payload []byte) [][]bool {
	spec := qrSpecsLevelL[len(qrSpecsLevelL)-1]
	for _, s := range qrSpecsLevelL {
		if len(payload)+2 <= s.dataBytes {
			spec = s
			break
		}
	}
	if len(payload)+2 > spec.dataBytes {
		payload = payload[:spec.dataBytes-2]
	}

	bits := make([]bool, 0, spec.dataBytes*8)
	appendBits := func(val, length int) {
		for i := length - 1; i >= 0; i-- {
			bits = append(bits, ((val>>i)&1) == 1)
		}
	}
	appendBits(0x4, 4)
	appendBits(len(payload), 8)
	for _, b := range payload {
		appendBits(int(b), 8)
	}
	for i := 0; i < 4 && len(bits) < spec.dataBytes*8; i++ {
		bits = append(bits, false)
	}
	for len(bits)%8 != 0 {
		bits = append(bits, false)
	}
	data := make([]byte, len(bits)/8)
	for i := range data {
		var b byte
		for j := 0; j < 8; j++ {
			b <<= 1
			if bits[i*8+j] {
				b |= 1
			}
		}
		data[i] = b
	}
	padBytes := []byte{0xEC, 0x11}
	for i := 0; len(data) < spec.dataBytes; i++ {
		data = append(data, padBytes[i%2])
	}

	ec := reedSolomonEncode(data, spec.ecBytes)
	fullCodewords := append(append([]byte{}, data...), ec...)

	n := spec.size
	modules := make([][]bool, n)
	reserved := make([][]bool, n)
	for i := range modules {
		modules[i] = make([]bool, n)
		reserved[i] = make([]bool, n)
	}

	setFunc := func(x, y int, dark bool) {
		if x >= 0 && x < n && y >= 0 && y < n {
			modules[y][x] = dark
			reserved[y][x] = true
		}
	}

	drawFinder := func(ox, oy int) {
		for dy := -1; dy <= 7; dy++ {
			for dx := -1; dx <= 7; dx++ {
				inOuter := dx >= 0 && dx <= 6 && dy >= 0 && dy <= 6
				onBorder := dx == 0 || dx == 6 || dy == 0 || dy == 6
				inCenter := dx >= 2 && dx <= 4 && dy >= 2 && dy <= 4
				setFunc(ox+dx, oy+dy, inOuter && (onBorder || inCenter))
			}
		}
	}
	drawFinder(0, 0)
	drawFinder(n-7, 0)
	drawFinder(0, n-7)

	for i := 8; i < n-8; i++ {
		setFunc(i, 6, i%2 == 0)
		setFunc(6, i, i%2 == 0)
	}

	for _, ay := range spec.alignments {
		for _, ax := range spec.alignments {
			if (ax == 6 && ay == 6) || (ax == 6 && ay == n-7) || (ax == n-7 && ay == 6) {
				continue
			}
			for dy := -2; dy <= 2; dy++ {
				for dx := -2; dx <= 2; dx++ {
					dark := dx == -2 || dx == 2 || dy == -2 || dy == 2 || (dx == 0 && dy == 0)
					setFunc(ax+dx, ay+dy, dark)
				}
			}
		}
	}

	for i := 0; i < 9; i++ {
		if i != 6 {
			reserved[8][i] = true
			reserved[i][8] = true
		}
	}
	for i := 0; i < 8; i++ {
		reserved[8][n-1-i] = true
		reserved[n-1-i][8] = true
	}
	setFunc(8, n-8, true)

	bitIdx := 0
	totalBits := len(fullCodewords) * 8
	upward := true
	for right := n - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := 0; vert < n; vert++ {
			y := vert
			if upward {
				y = n - 1 - vert
			}
			for j := 0; j < 2; j++ {
				x := right - j
				if !reserved[y][x] {
					dark := false
					if bitIdx < totalBits {
						dark = ((fullCodewords[bitIdx/8] >> (7 - (bitIdx % 8))) & 1) == 1
						bitIdx++
					}
					if (x+y)%2 == 0 {
						dark = !dark
					}
					modules[y][x] = dark
				}
			}
		}
		upward = !upward
	}

	const formatBits = 0x77C4
	formatBit := func(i int) bool {
		return ((formatBits >> i) & 1) == 1
	}
	for i := 0; i <= 5; i++ {
		modules[8][i] = formatBit(14 - i)
	}
	modules[8][7] = formatBit(8)
	modules[8][8] = formatBit(7)
	modules[7][8] = formatBit(6)
	for i := 0; i <= 5; i++ {
		modules[5-i][8] = formatBit(5 - i)
	}
	for i := 0; i < 7; i++ {
		modules[n-1-i][8] = formatBit(14 - i)
	}
	for i := 0; i < 8; i++ {
		modules[8][n-8+i] = formatBit(7 - i)
	}

	return modules
}

func reedSolomonEncode(data []byte, ecLen int) []byte {
	var exp [512]byte
	var logTbl [256]byte
	x := 1
	for i := 0; i < 255; i++ {
		exp[i] = byte(x)
		exp[i+255] = byte(x)
		logTbl[x] = byte(i)
		x <<= 1
		if x&0x100 != 0 {
			x ^= 0x11D
		}
	}
	gfMul := func(a, b byte) byte {
		if a == 0 || b == 0 {
			return 0
		}
		return exp[int(logTbl[a])+int(logTbl[b])]
	}

	gen := []byte{1}
	for i := 0; i < ecLen; i++ {
		next := make([]byte, len(gen)+1)
		for j := range gen {
			next[j] ^= gen[j]
			next[j+1] ^= gfMul(gen[j], exp[i])
		}
		gen = next
	}

	rem := make([]byte, ecLen)
	for _, b := range data {
		factor := b ^ rem[0]
		copy(rem, rem[1:])
		rem[ecLen-1] = 0
		for j := 0; j < ecLen; j++ {
			rem[j] ^= gfMul(gen[j+1], factor)
		}
	}
	return rem
}
