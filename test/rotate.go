// $G $D/$F.go && $L $F.$A &&
// ./$A.out >tmp.go && $G tmp.go && $L -o $A.out1 tmp.$A && ./$A.out1
// rm -f tmp.go $A.out1

// Copyright 2012 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Generate test of shift and rotate by constants.
// The output is compiled and run.
//
// The output takes around a minute to compile, link, and run
// but it is only done during ./run, not in normal builds using run.go.

package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
)

func main() {
	flag.Parse()

	b := bufio.NewWriter(os.Stdout)
	defer b.Flush()

	fmt.Fprintf(b, "%s\n", prolog)

	for logBits := uint(3); logBits <= 6; logBits++ {
		for mode := 0; mode < 1<<2; mode++ {
			gentest(b, 1<<logBits, mode&1 != 0, mode&2 != 0)
		}
	}
}

const prolog = `

package main

import (
	"fmt"
	"os"
)

var (
	i8 int8 = 0x12
	i16 int16 = 0x1234
	i32 int32 = 0x12345678
	i64 int64 = 0x123456789abcdef0
	ui8 uint8 = 0x12
	ui16 uint16 = 0x1234
	ui32 uint32 = 0x12345678
	ui64 uint64 = 0x123456789abcdef0

	ni8 = ^i8
	ni16 = ^i16
	ni32 = ^i32
	ni64 = ^i64
	nui8 = ^ui8
	nui16 = ^ui16
	nui32 = ^ui32
	nui64 = ^ui64
)

var nfail = 0

func check(desc string, have, want interface{}) {
	if have != want {
		nfail++
		fmt.Printf("%s = %T(%#x), want %T(%#x)\n", desc, have, have, want, want)
		if nfail >= 100 {
			fmt.Printf("BUG: stopping after 100 failures\n")
			os.Exit(0)
		}
	}
}

func main() {
	if nfail > 0 {
		fmt.Printf("BUG\n")
	}
}

`

func gentest(b *bufio.Writer, bits uint, unsigned, inverted bool) {
	fmt.Fprintf(b, "func init() {\n")
	defer fmt.Fprintf(b, "}\n")
	n := 0

	// Generate tests for left/right and right/left.
	for l := uint(0); l <= bits; l++ {
		for r := uint(0); r <= bits; r++ {
			typ := fmt.Sprintf("int%d", bits)
			v := fmt.Sprintf("i%d", bits)
			if unsigned {
				typ = "u" + typ
				v = "u" + v
			}
			v0 := int64(0x123456789abcdef0)
			if inverted {
				v = "n" + v
				v0 = ^v0
			}
			expr1 := fmt.Sprintf("%s<<%d | %s>>%d", v, l, v, r)
			expr2 := fmt.Sprintf("%s>>%d | %s<<%d", v, r, v, l)
			
			var result string
			if unsigned {
				v := uint64(v0) >> (64 - bits)
				v = v<<l | v>>r
				v <<= 64 - bits
				v >>= 64 - bits
				result = fmt.Sprintf("%#x", v)
			} else {
				v := int64(v0) >> (64 - bits)
				v = v<<l | v>>r
				v <<= 64 - bits
				v >>= 64 - bits
				result = fmt.Sprintf("%#x", v)
			}

			fmt.Fprintf(b, "\tcheck(%q, %s, %s(%s))\n", expr1, expr1, typ, result)
			fmt.Fprintf(b, "\tcheck(%q, %s, %s(%s))\n", expr2, expr2, typ, result)

			// Chop test into multiple functions so that there's not one
			// enormous function to compile/link.
			// All the functions are named init so we don't have to do
			// anything special to call them.  ☺
			if n++; n >= 100 {
				fmt.Fprintf(b, "}\n")
				fmt.Fprintf(b, "func init() {\n")
				n = 0
			}
		}
	}
}
