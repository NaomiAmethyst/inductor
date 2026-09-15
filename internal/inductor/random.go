// SPDX-License-Identifier: GPL-3.0-only
package inductor

import "math/bits"

// pythonRandom implements the integer-seeded MT19937 stream used by Python's
// random.Random. Comparison samples must keep their identities across the port.
type pythonRandom struct {
	state [624]uint32
	index int
}

func newPythonRandom(seed int64) *pythonRandom {
	r := &pythonRandom{index: 624}
	r.state[0] = 19650218
	for i := 1; i < 624; i++ {
		r.state[i] = 1812433253*(r.state[i-1]^(r.state[i-1]>>30)) + uint32(i)
	}
	n := uint64(seed)
	if seed < 0 {
		n = uint64(-(seed + 1)) + 1
	}
	key := []uint32{uint32(n)}
	if n>>32 != 0 {
		key = append(key, uint32(n>>32))
	}
	i, j := 1, 0
	for k := max(624, len(key)); k > 0; k-- {
		r.state[i] = (r.state[i] ^ ((r.state[i-1] ^ (r.state[i-1] >> 30)) * 1664525)) + key[j] + uint32(j)
		i++
		j++
		if i >= 624 {
			r.state[0] = r.state[623]
			i = 1
		}
		if j >= len(key) {
			j = 0
		}
	}
	for k := 623; k > 0; k-- {
		r.state[i] = (r.state[i] ^ ((r.state[i-1] ^ (r.state[i-1] >> 30)) * 1566083941)) - uint32(i)
		i++
		if i >= 624 {
			r.state[0] = r.state[623]
			i = 1
		}
	}
	r.state[0] = 0x80000000
	return r
}
func (r *pythonRandom) next() uint32 {
	if r.index >= 624 {
		for i := 0; i < 624; i++ {
			y := (r.state[i] & 0x80000000) | (r.state[(i+1)%624] & 0x7fffffff)
			r.state[i] = r.state[(i+397)%624] ^ (y >> 1)
			if y&1 != 0 {
				r.state[i] ^= 0x9908b0df
			}
		}
		r.index = 0
	}
	y := r.state[r.index]
	r.index++
	y ^= y >> 11
	y ^= (y << 7) & 0x9d2c5680
	y ^= (y << 15) & 0xefc60000
	y ^= y >> 18
	return y
}
func (r *pythonRandom) shuffle(n int, swap func(int, int)) {
	for i := n - 1; i > 0; i-- {
		k := bits.Len(uint(i + 1))
		j := int(r.next() >> uint(32-k))
		for j > i {
			j = int(r.next() >> uint(32-k))
		}
		swap(i, j)
	}
}
