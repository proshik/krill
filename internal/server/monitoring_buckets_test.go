package server

import (
	"testing"
	"time"
)

func TestMonBucketCount(t *testing.T) {
	const iv = 30 * time.Second
	cases := []struct {
		name string
		rng  time.Duration
		want int
	}{
		// 1h / (2*30s) = 60 buckets (60s each) -> samples land in adjacent buckets.
		{"1h", time.Hour, 60},
		// 6h and 24h exceed the 240 cap.
		{"6h", 6 * time.Hour, 240},
		{"24h", 24 * time.Hour, 240},
		// Tiny range floors at 12.
		{"5m", 5 * time.Minute, 12},
	}
	for _, c := range cases {
		if got := monBucketCount(c.rng, iv); got != c.want {
			t.Errorf("monBucketCount(%s, 30s) = %d, want %d", c.name, got, c.want)
		}
	}

	// A bucket must always span at least the sample interval, so consecutive
	// samples can never share a bucket boundary in a way that leaves gaps.
	for _, rng := range []time.Duration{time.Hour, 6 * time.Hour, 24 * time.Hour} {
		n := monBucketCount(rng, iv)
		if bucket := rng / time.Duration(n); bucket < iv {
			t.Errorf("range %s: bucket %s < interval %s (n=%d)", rng, bucket, iv, n)
		}
	}

	// Zero interval (disabled) falls back to the full resolution cap.
	if got := monBucketCount(time.Hour, 0); got != 240 {
		t.Errorf("monBucketCount(1h, 0) = %d, want 240", got)
	}
}
