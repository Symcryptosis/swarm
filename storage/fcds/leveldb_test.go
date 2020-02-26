package fcds

import (
	"testing"
)

func TestShardSlotsUnsorted(t *testing.T) {
	ms, err := NewMetaStore("", true)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		shard  uint8 // which shard to manipulate on this run
		delta  int64 // shard free slot change
		expect int64 // what value should the manipulated shard have
		zero   []int // which shards should be with no free slots

	}{
		{
			// initial state, all zero
			shard:  0,
			delta:  0,
			expect: 0,
			zero:   []int{0, 1, 2, 3},
		},
		{
			// increment shard 0
			shard:  0,
			delta:  3,
			expect: 3,
			zero:   []int{1, 2, 3},
		},
		{
			// increment shard 2
			shard:  2,
			delta:  15,
			expect: 15,
			zero:   []int{1, 3},
		},
		{
			// decrement 0
			shard:  0,
			delta:  -2,
			expect: 1,
			zero:   []int{1, 3},
		},
		{
			// make shard 0 zero
			shard:  0,
			delta:  -1,
			expect: 0,
			zero:   []int{0, 1, 3},
		},
	} {
		ms.free[tc.shard] += tc.delta
		v := ms.shardSlots(false)
		if v[tc.shard].slots != tc.expect {
			t.Errorf("expected shard %d free slots counter to be %d but got %d", tc.shard, tc.expect, v[tc.shard])
		}

		for _, i := range tc.zero {
			if v[i].slots != 0 {
				t.Errorf("expected shard %d to have no free slots but got %d", tc.shard, v[i])
			}
		}
	}
}

func TestShardSlotsSorted(t *testing.T) {
	ms, err := NewMetaStore("", true)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		shard    uint8   // which shard to manipulate on this run
		delta    int64   // shard free slot change
		expect   []int64 // what free slots should the sorted sequence have
		shardSeq []uint8 // which shards should be in which index

	}{
		{
			// initial state, all zero
			shard:    0,
			delta:    0,
			expect:   []int64{0, 0, 0, 0},
			shardSeq: []uint8{0, 1, 2, 3},
		},
		{
			// inc shard 0
			shard:    0,
			delta:    10,
			expect:   []int64{10, 0, 0, 0},
			shardSeq: []uint8{0, 1, 2, 3},
		},

		{
			// inc shard 2
			shard:    2,
			delta:    11,
			expect:   []int64{11, 10, 0, 0},
			shardSeq: []uint8{2, 0, 1, 3},
		},

		{
			// dec shard 0
			shard:    0,
			delta:    -3,
			expect:   []int64{11, 7, 0, 0},
			shardSeq: []uint8{2, 0, 1, 3},
		},

		{
			// inc shard 3
			shard:    3,
			delta:    8,
			expect:   []int64{11, 8, 7, 0},
			shardSeq: []uint8{2, 3, 0, 1},
		},

		{
			// inc shard 1
			shard:    1,
			delta:    29,
			expect:   []int64{29, 11, 8, 7},
			shardSeq: []uint8{1, 2, 3, 0},
		},
	} {
		ms.free[tc.shard] += tc.delta
		v := ms.shardSlots(true)
		for i, vv := range v {
			if vv.shard != tc.shardSeq[i] {
				t.Errorf("got wrong shard number, expected shard %d but got %d", tc.shardSeq[i], vv.shard)
			}

			if vv.slots != tc.expect[i] {
				t.Errorf("free slots mismatch for shard %d: got %d want %d", vv.shard, vv.slots, tc.expect[i])
			}
		}
	}
}