package snowflake

import "time"

const seqBits int = 20
const epoch int64 = 1704085200000

type Snowflake struct {
	rep int64
}

func MakeSnowflake(rep int64) Snowflake {
	if rep < 0 {
		panic("snowflakes are unsigned 63-bit integers")
	}

	return Snowflake{rep}
}

func (s Snowflake) Rep() int64 {
	return s.rep
}

func (s Snowflake) Increment(t time.Time) Snowflake {
	u1 := s.rep >> seqBits
	u2 := t.UnixMilli() - epoch

	if u2 < 0 || u2 >= 1<<(63-seqBits) {
		panic("time is out of range")
	}

	if u2 <= u1 {
		return MakeSnowflake(s.rep + 1)
	} else {
		return MakeSnowflake(u2 << seqBits)
	}
}
