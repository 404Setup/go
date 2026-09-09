// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package math

// Export internal functions for testing.
var ExpGo = exp
var Exp2Go = exp2
var HypotGo = hypot
var SqrtGo = sqrt
var TrigReduce = trigReduce

var FastExp = fastExp
var FastExp2 = fastExp2
var FastLog = fastLog
var FastLog2 = fastLog2
var FastLog10 = fastLog10
var FastPow = fastPow
var FastSin = fastSin
var FastCos = fastCos
var FastTan = fastTan

const ReduceThreshold = reduceThreshold
