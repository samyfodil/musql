// The functions glibc takes from CORE-MATH (asinh, acosh, atanh), and those it
// takes from IBM's Accurate Mathematical Library (sin, cos, tan, asin, acos,
// atan, atan2), are ported from CORE-MATH (https://core-math.gitlabpages.inria.fr/),
// src/binary64:
//
// Copyright (c) 2023-2025 Alexei Sibidanov <sibid@uvic.ca>. (asinh, atanh)
// Copyright (c) 2023 Alexei Sibidanov. (acosh)
// Copyright (c) 2026 Paul Zimmermann and Alexei Sibidanov (sin)
// Copyright (c) 2026 Paul Zimmermann (cos)
// Copyright (c) 2022-2025 Paul Zimmermann and Tom Hubrecht (tan)
// Copyright (c) 2022-2026 Alexei Sibidanov <sibid@uvic.ca>. (asin)
// Copyright (c) 2024-2025 Alexei Sibidanov. (acos)
// Copyright (c) 2023-2026 Alexei Sibidanov <sibid@uvic.ca>. (atan)
// Copyright (c) 2024-2025 Paul Zimmermann and Alexei Sibidanov (atan2)
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.
//
// CORE-MATH's results are correctly rounded, so for asinh, acosh and atanh
// they are glibc's. IBM's are not quite: glibc's sin, cos, tan, asin, acos,
// atan and atan2 differ from the correctly rounded value in the last bit for
// between 0.02% (atan2) and 0.2% (tan) of inputs, and there these answer the
// correctly rounded value. IBM's code is LGPL, which this package cannot take.
//
// Correct rounding does not depend on how the arithmetic is fused, so these
// are written as CORE-MATH's C is, without libm.go's float64() conversions.

package libm

// fastTwoSum is s = x+y rounded and the error e = x+y-s, exact when x is
// zero or |x| >= |y|.
func fastTwoSum(x, y float64) (s, e float64) {
	s = x + y
	z := s - x
	return s, y - z
}

// twoSum is s = a+b rounded and the error t = a+b-s, exact for any a and b.
func twoSum(a, b float64) (s, t float64) {
	s = a + b
	ap := s - b
	bp := s - ap
	return s, (a - ap) + (b - bp)
}
