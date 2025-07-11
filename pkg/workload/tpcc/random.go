// Copyright 2017 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package tpcc

import (
	"math"
	"math/rand/v2"

	"github.com/cockroachdb/apd/v3"
	"github.com/cockroachdb/cockroach/pkg/util/bufalloc"
	"github.com/cockroachdb/cockroach/pkg/workload/workloadimpl"
)

var cLastTokens = [...]string{
	"BAR", "OUGHT", "ABLE", "PRI", "PRES",
	"ESE", "ANTI", "CALLY", "ATION", "EING"}

func (w *tpcc) initNonUniformRandomConstants() {
	rng := rand.New(rand.NewPCG(RandomSeed.Seed(), 0))
	w.cLoad = rng.IntN(256)
	w.cItemID = rng.IntN(1024)
	w.cCustomerID = rng.IntN(8192)
}

const precomputedLength = 10000
const aCharsAlphabet = `abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890`
const lettersAlphabet = `ABCDEFGHIJKLMNOPQRSTUVWXYZ`
const numbersAlphabet = `1234567890`

type tpccRand struct {
	*rand.Rand
	*rand.PCG

	aChars, letters, numbers workloadimpl.PrecomputedRand
}

type aCharsOffset int
type lettersOffset int
type numbersOffset int

// Seed resets the RNG with the given seeds.
func (rng *tpccRand) Seed(seed1, seed2 uint64) {
	if rng.PCG != nil {
		// rand.Rand has no state other than the source, so we don't need to
		// recreate it.
		rng.PCG.Seed(seed1, seed2)
		return
	}
	rng.PCG = rand.NewPCG(seed1, seed2)
	rng.Rand = rand.New(rng.PCG)
}

func randStringFromAlphabet(
	rng *rand.Rand,
	a *bufalloc.ByteAllocator,
	minLen, maxLen int,
	pr workloadimpl.PrecomputedRand,
	prOffset *int,
) []byte {
	size := maxLen
	if maxLen-minLen != 0 {
		size = int(randInt(rng, minLen, maxLen))
	}
	if size == 0 {
		return nil
	}

	var b []byte
	*a, b = a.Alloc(size)
	*prOffset = pr.FillBytes(*prOffset, b)
	return b
}

// randAStringInitialDataOnly generates a random alphanumeric string of length
// between min and max inclusive. It uses a set of pregenerated random data,
// which the spec allows only for initial data. See 4.3.2.2.
//
// For speed, this is done using precomputed random data, which is explicitly
// allowed by the spec for initial data only. See 4.3.2.1.
func randAStringInitialDataOnly(
	rng *tpccRand, ao *aCharsOffset, a *bufalloc.ByteAllocator, min, max int,
) []byte {
	return randStringFromAlphabet(rng.Rand, a, min, max, rng.aChars, (*int)(ao))
}

// randNStringInitialDataOnly generates a random numeric string of length
// between min and max inclusive. See 4.3.2.2.
//
// For speed, this is done using precomputed random data, which is explicitly
// allowed by the spec for initial data only. See 4.3.2.1.
func randNStringInitialDataOnly(
	rng *tpccRand, no *numbersOffset, a *bufalloc.ByteAllocator, min, max int,
) []byte {
	return randStringFromAlphabet(rng.Rand, a, min, max, rng.numbers, (*int)(no))
}

// randStateInitialDataOnly produces a random US state. (spec just says 2
// letters)
//
// For speed, this is done using precomputed random data, which is explicitly
// allowed by the spec for initial data only. See 4.3.2.1.
func randStateInitialDataOnly(rng *tpccRand, lo *lettersOffset, a *bufalloc.ByteAllocator) []byte {
	return randStringFromAlphabet(rng.Rand, a, 2, 2, rng.letters, (*int)(lo))
}

// randOriginalStringInitialDataOnly generates a random a-string[26..50] with
// 10% chance of containing the string "ORIGINAL" somewhere in the middle of the
// string. See 4.3.3.1.
//
// For speed, this is done using precomputed random data, which is explicitly
// allowed by the spec for initial data only. See 4.3.2.1.
func randOriginalStringInitialDataOnly(
	rng *tpccRand, ao *aCharsOffset, a *bufalloc.ByteAllocator,
) []byte {
	if rng.Rand.IntN(9) == 0 {
		l := int(randInt(rng.Rand, 26, 50))
		off := int(randInt(rng.Rand, 0, l-8))
		var buf []byte
		*a, buf = a.Alloc(l)
		copy(buf[:off], randAStringInitialDataOnly(rng, ao, a, off, off))
		copy(buf[off:off+8], originalString)
		copy(buf[off+8:], randAStringInitialDataOnly(rng, ao, a, l-off-8, l-off-8))
		return buf
	}
	return randAStringInitialDataOnly(rng, ao, a, 26, 50)
}

// randZip produces a random "zip code" - a 4-digit number plus the constant
// "11111". See 4.3.2.7.
//
// For speed, this is done using precomputed random data, which is explicitly
// allowed by the spec for initial data only. See 4.3.2.1.
func randZipInitialDataOnly(rng *tpccRand, no *numbersOffset, a *bufalloc.ByteAllocator) []byte {
	var buf []byte
	*a, buf = a.Alloc(9)
	copy(buf[:4], randNStringInitialDataOnly(rng, no, a, 4, 4))
	copy(buf[4:], `11111`)
	return buf
}

// randTax produces a random tax between [0.0000..0.2000]
// See 2.1.5.
func randTax(rng *rand.Rand) apd.Decimal {
	var result apd.Decimal
	result.SetFinite(randInt(rng, 0, 2000), -4)
	return result
}

// randInt returns a number within [min, max] inclusive.
// See 2.1.4.
func randInt(rng *rand.Rand, min, max int) int64 {
	return int64(rng.IntN(max-min+1) + min)
}

func makeDecimal(value float64, scale int32) apd.Decimal {
	value = math.Round(value * math.Pow10(int(scale)))
	var result apd.Decimal
	result.SetFinite(int64(value), -scale)
	return result
}

func randDecimal(rng *rand.Rand, min, max float64, scale int32) apd.Decimal {
	minInt := int64(min * math.Pow10(int(scale)))
	maxInt := int64(max * math.Pow10(int(scale)))
	value := randInt(rng, int(minInt), int(maxInt))
	var result apd.Decimal
	result.SetFinite(value, -scale)
	return result
}

// randCLastSyllables returns a customer last name string generated according to
// the table in 4.3.2.3. Given a number between 0 and 999, each of the three
// syllables is determined by the corresponding digit in the three digit
// representation of the number. For example, the number 371 generates the name
// PRICALLYOUGHT, and the number 40 generates the name BARPRESBAR.
func randCLastSyllables(n int, a *bufalloc.ByteAllocator) []byte {
	const scratchLen = 3 * 5 // 3 entries from cLastTokens * max len of an entry
	var buf []byte
	*a, buf = a.Alloc(scratchLen)
	buf = buf[:0]
	buf = append(buf, cLastTokens[n/100]...)
	n = n % 100
	buf = append(buf, cLastTokens[n/10]...)
	n = n % 10
	buf = append(buf, cLastTokens[n]...)
	return buf
}

// See 4.3.2.3.
func (w *tpcc) randCLast(rng *rand.Rand, a *bufalloc.ByteAllocator) []byte {
	return randCLastSyllables(((rng.IntN(256)|rng.IntN(1000))+w.cLoad)%1000, a)
}

// Return a non-uniform random customer ID. See 2.1.6.
func (w *tpcc) randCustomerID(rng *rand.Rand) int {
	return ((rng.IntN(1024) | (rng.IntN(3000) + 1) + w.cCustomerID) % 3000) + 1
}

// Return a non-uniform random item ID. See 2.1.6.
func (w *tpcc) randItemID(rng *rand.Rand) int {
	return ((rng.IntN(8190) | (rng.IntN(100000) + 1) + w.cItemID) % 100000) + 1
}
