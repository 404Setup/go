// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package math

// These entry points are selected by cmd/compile under -fmth. They trade
// correctly rounded results for shorter polynomials, with no public API change.
// Exceptional inputs and difficult range reduction reuse the existing kernels.
// Keep calls to fallback kernels lowercase to avoid recursive substitution.

func fastExp(x float64) float64 {
	if x < -700 || x > 700 || IsNaN(x) {
		return exp(x)
	}
	k := int(x*Log2E + Copysign(0.5, x))
	r := (x - float64(k)*6.93147180369123816490e-1) - float64(k)*1.90821492927058770002e-10
	return fastExpReduced(r) * Float64frombits(uint64(k+1023)<<52)
}

func fastExp2(x float64) float64 {
	if x < -1000 || x > 1000 || IsNaN(x) {
		return exp2(x)
	}
	k := int(x + Copysign(0.5, x))
	return fastExpReduced((x-float64(k))*Ln2) * Float64frombits(uint64(k+1023)<<52)
}

// Degree 7 Taylor polynomial on [-ln(2)/2, ln(2)/2]. The relative error
// is below 8e-9 on this interval, including range-reduction rounding.
func fastExpReduced(r float64) float64 {
	return 1 + r*(1+r*(1.0/2+r*(1.0/6+r*(1.0/24+r*(1.0/120+r*(1.0/720+r*(1.0/5040)))))))
}

func fastLog(x float64) float64 {
	bits := Float64bits(x)
	exponent := bits >> 52
	if exponent == 0 || exponent >= 0x7ff {
		return log(x)
	}
	k := int(exponent) - 1023
	m := Float64frombits(bits&0xfffffffffffff | 0x3ff0000000000000)
	if m > Sqrt2 {
		m *= 0.5
		k++
	}
	// log(m) = 2*(z + z^3/3 + ...), with |z| <= 0.172.
	z := (m - 1) / (m + 1)
	z2 := z * z
	return float64(k)*Ln2 + 2*z*(1+z2*(1.0/3+z2*(1.0/5+z2*(1.0/7+z2*(1.0/9)))))
}

func fastLog2(x float64) float64  { return fastLog(x) * Log2E }
func fastLog10(x float64) float64 { return fastLog(x) * Log10E }

func fastPow(x, y float64) float64 {
	// Limit exponent amplification of the logarithm's approximation error.
	if x <= 0 || IsInf(x, 1) || IsNaN(x) || IsNaN(y) || Abs(y) > 16 {
		return pow(x, y)
	}
	if y == 0 || x == 1 {
		return 1
	}
	return fastExp(y * fastLog(x))
}

// fastSinCos uses degree 9/10 Taylor polynomials after reduction to [-pi/4,
// pi/4]. Large arguments use the existing full-precision range reduction.
func fastSinCos(x float64) (s, c float64) {
	if Abs(x) > 0x1p16 || IsNaN(x) {
		return Sincos(x)
	}
	k := int(x*(2/Pi) + Copysign(0.5, x))
	r := (x - float64(k)*1.57079632673412561417) - float64(k)*6.07710050650619224932e-11
	r2 := r * r
	s = r * (1 + r2*(-1.0/6+r2*(1.0/120+r2*(-1.0/5040+r2*(1.0/362880)))))
	c = 1 + r2*(-1.0/2+r2*(1.0/24+r2*(-1.0/720+r2*(1.0/40320-r2*(1.0/3628800)))))
	switch k & 3 {
	case 1:
		return c, -s
	case 2:
		return -s, -c
	case 3:
		return -c, s
	}
	return s, c
}

func fastSin(x float64) float64 { s, _ := fastSinCos(x); return s }
func fastCos(x float64) float64 { _, c := fastSinCos(x); return c }
func fastTan(x float64) float64 {
	if Abs(x) > 0x1p16 || IsNaN(x) {
		return tan(x)
	}
	s, c := fastSinCos(x)
	if Abs(c) < 1e-4 {
		return tan(x)
	}
	return s / c
}
